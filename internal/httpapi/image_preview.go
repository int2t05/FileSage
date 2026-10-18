// 本文件提供 HEIF 与相机 RAW 图片的受限 JPEG 预览接口。
package httpapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	"filesage/internal/fsroot"
	"filesage/internal/imagepreview"
)

type imagePreviewFileSystem interface {
	OpenRead(string) (io.ReadCloser, fsroot.Entry, error)
}

type imagePreviewGenerator interface {
	Generate(context.Context, string, io.Reader, imagepreview.Size) ([]byte, error)
}

// NewImagePreviewHandler 创建有版本校验的 HEIF 与 RAW JPEG 预览接口。
func NewImagePreviewHandler(files imagePreviewFileSystem, generator imagePreviewGenerator) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeError(w, http.StatusMethodNotAllowed, "仅支持 GET 请求。")
			return
		}
		if files == nil {
			writeError(w, http.StatusServiceUnavailable, "图片预览服务未启用。")
			return
		}
		if r.URL.Path != "/api/v1/files/image-preview" {
			http.NotFound(w, r)
			return
		}
		relativePath, version, size, ok := imagePreviewQuery(w, r)
		if !ok {
			return
		}
		if !isImagePreviewPath(relativePath) {
			writeError(w, http.StatusUnsupportedMediaType, "仅支持 HEIC、HEIF、DNG、CR2、CR3、NEF 和 ARW 图片。")
			return
		}

		file, entry, err := files.OpenRead(relativePath)
		if err != nil {
			writeFileError(w, err)
			return
		}
		if entry.Kind != fsroot.KindFile {
			_ = file.Close()
			writeError(w, http.StatusBadRequest, "仅支持普通图片文件。")
			return
		}
		if versionForEntry(entry) != version {
			_ = file.Close()
			writeError(w, http.StatusConflict, "文件版本已变化，请刷新后重试。")
			return
		}
		etag := `"` + version + "-" + string(size) + `"`
		if versionedImageNotModified(r, etag, entry.ModifiedAt) {
			if err := file.Close(); err != nil {
				writeError(w, http.StatusInternalServerError, "无法关闭图片文件。")
				return
			}
			setVersionedJPEGHeaders(w, version, etag, entry.ModifiedAt)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if generator == nil {
			_ = file.Close()
			writeError(w, http.StatusServiceUnavailable, "图片预览服务未启用。")
			return
		}

		preview, generateErr := generator.Generate(r.Context(), relativePath, file, size)
		closeErr := file.Close()
		if r.Context().Err() != nil {
			return
		}
		if generateErr != nil {
			writeImagePreviewError(w, generateErr)
			return
		}
		if closeErr != nil {
			writeError(w, http.StatusInternalServerError, "无法安全关闭图片文件。")
			return
		}
		current, currentEntry, err := files.OpenRead(relativePath)
		if err != nil {
			writeError(w, http.StatusConflict, "图片在生成预览期间发生变化，请刷新后重试。")
			return
		}
		currentVersion := versionForEntry(currentEntry)
		currentCloseErr := current.Close()
		if currentVersion != version {
			writeError(w, http.StatusConflict, "图片在生成预览期间发生变化，请刷新后重试。")
			return
		}
		if currentCloseErr != nil {
			writeError(w, http.StatusInternalServerError, "无法关闭图片文件。")
			return
		}
		setVersionedJPEGHeaders(w, version, etag, entry.ModifiedAt)
		http.ServeContent(w, r, filepath.Base(relativePath)+".jpg", entry.ModifiedAt, bytes.NewReader(preview))
	})
}

// imagePreviewQuery 验证相对路径、文件版本与预览尺寸。
func imagePreviewQuery(w http.ResponseWriter, r *http.Request) (string, string, imagepreview.Size, bool) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, "图片预览查询参数无效。")
		return "", "", "", false
	}
	for key, value := range values {
		if (key != "path" && key != "version" && key != "size") || len(value) != 1 {
			writeError(w, http.StatusBadRequest, "仅接受一个 path、version 和 size。")
			return "", "", "", false
		}
	}
	paths, versions, sizes := values["path"], values["version"], values["size"]
	if len(paths) != 1 || !validRelativePath(paths[0], false) || isReservedPath(paths[0]) {
		writeError(w, http.StatusBadRequest, "path 必须是管理根目录内的相对路径。")
		return "", "", "", false
	}
	if len(versions) != 1 || versions[0] == "" || len(versions[0]) > 256 {
		writeError(w, http.StatusBadRequest, "version 必须是有效的文件版本。")
		return "", "", "", false
	}
	if len(sizes) != 1 {
		writeError(w, http.StatusBadRequest, "size 必须为 thumb 或 view。")
		return "", "", "", false
	}
	size := imagepreview.Size(sizes[0])
	if size != imagepreview.Thumbnail && size != imagepreview.Viewer {
		writeError(w, http.StatusBadRequest, "size 必须为 thumb 或 view。")
		return "", "", "", false
	}
	return paths[0], versions[0], size, true
}

// isImagePreviewPath 限定由外部解码器处理的图像格式。
func isImagePreviewPath(relativePath string) bool {
	switch strings.ToLower(filepath.Ext(relativePath)) {
	case ".heic", ".heif", ".dng", ".cr2", ".cr3", ".nef", ".arw":
		return true
	default:
		return false
	}
}

// writeImagePreviewError 将解码失败映射为稳定的 HTTP 状态。
func writeImagePreviewError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, imagepreview.ErrDisabled):
		writeError(w, http.StatusServiceUnavailable, "HEIF／RAW 预览未启用，请安装对应的系统解码工具。")
	case errors.Is(err, imagepreview.ErrBusy):
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, "图片预览任务较多，请稍后重试。")
	case errors.Is(err, imagepreview.ErrTimeout), errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "图片预览生成超时。")
	case errors.Is(err, imagepreview.ErrInputTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "图片文件超过预览大小上限。")
	case errors.Is(err, imagepreview.ErrOutputTooLarge), errors.Is(err, imagepreview.ErrInvalidImage):
		writeError(w, http.StatusUnprocessableEntity, "图片格式无法生成有效预览。")
	case errors.Is(err, imagepreview.ErrUnsupported):
		writeError(w, http.StatusUnsupportedMediaType, "图片预览规格不受支持。")
	case errors.Is(err, context.Canceled):
		writeError(w, http.StatusRequestTimeout, "图片预览已取消。")
	default:
		writeError(w, http.StatusUnprocessableEntity, "图片格式无法解码或文件内容无效。")
	}
}
