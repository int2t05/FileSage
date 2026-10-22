package httpapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	"filesage/internal/fsroot"
	"filesage/internal/office"
)

var officePreviewSlots = make(chan struct{}, office.MaxConcurrentPreviews)

// officePreview 返回当前文件版本生成的只读 PDF 派生预览。
func (api *API) officePreview(w http.ResponseWriter, r *http.Request) {
	relativePath, version, ok := officePreviewQuery(w, r)
	if !ok {
		return
	}

	extension := strings.ToLower(filepath.Ext(relativePath))
	if extension != ".docx" && extension != ".pptx" {
		writeOfficePreviewError(w, office.ErrUnsupportedFormat)
		return
	}
	if version == "" {
		writeError(w, http.StatusBadRequest, "当前文件版本不能为空。")
		return
	}
	releaseSlot, ok := tryAcquireOfficePreviewSlot()
	if !ok {
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, "Office 预览繁忙，请稍后重试。")
		return
	}
	defer releaseSlot()

	release, ok := api.lockMutationPaths(w, r, relativePath)
	if !ok {
		return
	}
	entry, err := api.verifyEntryVersion(relativePath, version)
	if err != nil {
		release()
		writeFileError(w, err)
		return
	}
	if entry.Kind != fsroot.KindFile {
		release()
		writeError(w, http.StatusBadRequest, "仅支持预览普通 DOCX 或 PPTX 文件。")
		return
	}
	if entry.Size > office.MaxInputBytes {
		release()
		writeOfficePreviewError(w, office.ErrInputTooLarge)
		return
	}

	file, openedEntry, err := api.files.OpenRead(relativePath)
	if err != nil {
		release()
		writeFileError(w, err)
		return
	}
	if openedEntry.Kind != fsroot.KindFile || versionForEntry(openedEntry) != version {
		_ = file.Close()
		release()
		writeError(w, http.StatusConflict, "文件版本已变化，请刷新后重试。")
		return
	}
	if openedEntry.Size > office.MaxInputBytes {
		_ = file.Close()
		release()
		writeOfficePreviewError(w, office.ErrInputTooLarge)
		return
	}
	data, readErr := io.ReadAll(io.LimitReader(file, office.MaxInputBytes+1))
	closeErr := file.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		release()
		writeError(w, http.StatusInternalServerError, "无法安全读取 Office 文件。")
		return
	}
	if int64(len(data)) > office.MaxInputBytes {
		release()
		writeOfficePreviewError(w, office.ErrInputTooLarge)
		return
	}
	if int64(len(data)) != openedEntry.Size {
		release()
		writeError(w, http.StatusConflict, "文件内容在读取期间发生变化，请刷新后重试。")
		return
	}
	if _, err := api.verifyEntryVersion(relativePath, version); err != nil {
		release()
		writeFileError(w, err)
		return
	}
	release()

	pdf, err := office.Convert(r.Context(), data, extension)
	if err != nil {
		writeOfficePreviewError(w, err)
		return
	}
	if _, err := api.verifyEntryVersion(relativePath, version); err != nil {
		writeFileError(w, err)
		return
	}

	name := strings.TrimSuffix(filepath.Base(relativePath), filepath.Ext(relativePath)) + ".pdf"
	contentDisposition := mime.FormatMediaType("inline", map[string]string{"filename": name})
	if contentDisposition == "" {
		contentDisposition = "inline"
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", contentDisposition)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-FileSage-Version", version)
	http.ServeContent(w, r, name, entry.ModifiedAt, bytes.NewReader(pdf))
}

// tryAcquireOfficePreviewSlot 在读取源文件前限制并发请求。
func tryAcquireOfficePreviewSlot() (func(), bool) {
	select {
	case officePreviewSlots <- struct{}{}:
		return func() { <-officePreviewSlots }, true
	default:
		return nil, false
	}
}

func officePreviewQuery(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Office 预览查询参数无效。")
		return "", "", false
	}
	for key, value := range values {
		if (key != "path" && key != "version") || len(value) != 1 {
			writeError(w, http.StatusBadRequest, "Office 预览只接受一个 path 和一个 version。")
			return "", "", false
		}
	}
	pathValues, versionValues := values["path"], values["version"]
	if len(pathValues) != 1 || !validRelativePath(pathValues[0], false) || isReservedPath(pathValues[0]) {
		writeError(w, http.StatusBadRequest, "path 必须是管理根目录内的干净相对路径。")
		return "", "", false
	}
	if len(versionValues) > 1 || len(versionValues) == 1 && len(versionValues[0]) > 256 {
		writeError(w, http.StatusBadRequest, "Office 预览文件版本无效。")
		return "", "", false
	}
	version := ""
	if len(versionValues) == 1 {
		version = versionValues[0]
	}
	return pathValues[0], version, true
}

func writeOfficePreviewError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, office.ErrUnsupportedFormat):
		writeError(w, http.StatusUnsupportedMediaType, "仅支持 DOCX 和 PPTX 文件的 PDF 预览。")
	case errors.Is(err, office.ErrUnavailable):
		writeError(w, http.StatusServiceUnavailable, "Office PDF 预览未启用；请安装 LibreOffice、Bubblewrap、prlimit 和 util-linux 后重启服务。")
	case errors.Is(err, office.ErrInputTooLarge), errors.Is(err, office.ErrExpandedTooLarge), errors.Is(err, office.ErrOutputTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "Office 文件或派生预览超过安全处理上限。")
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "Office 文件转换超时。")
	case errors.Is(err, context.Canceled):
		writeError(w, http.StatusRequestTimeout, "Office 文件转换已取消。")
	case errors.Is(err, office.ErrInvalidDocument):
		writeError(w, http.StatusUnprocessableEntity, "Office 文件损坏、加密或格式不受支持。")
	case errors.Is(err, office.ErrConversionFailed):
		writeError(w, http.StatusUnprocessableEntity, "无法将此 Office 文件转换为 PDF。")
	default:
		writeError(w, http.StatusInternalServerError, "Office 预览生成失败。")
	}
}
