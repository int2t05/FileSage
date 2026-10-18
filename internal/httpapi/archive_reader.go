// 本文件处理压缩包只读浏览和经版本确认的安全解压。
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"filesage/internal/fsroot"
)

const (
	maxConcurrentArchiveReads          = 2
	maxArchiveExtractBodyBytes         = 4 << 20
	maxArchiveExtractSelection         = 5000
	archiveReadMaxArchiveBytes         = 20 << 30
	archiveReadMaxExternalArchiveBytes = 1 << 30
	archiveReadMaxEntries              = 50000
	archiveReadMaxEntryBytes           = 2 << 30
	archiveReadMaxTotalBytes           = 20 << 30
	archiveReadMaxPreviewBytes         = 32 << 20
	archiveReadMaxRatio                = 1000
)

type archiveReaderFileSystem interface {
	ListArchive(context.Context, string, string, fsroot.ArchiveReadLimits) ([]fsroot.ArchiveEntry, error)
	ReadArchiveEntry(context.Context, string, string, fsroot.ArchiveReadLimits) ([]byte, fsroot.ArchiveEntry, error)
	ExtractArchive(context.Context, string, string, []string, fsroot.ArchiveReadLimits) (fsroot.ArchiveExtractResult, error)
}

type archiveEntriesResponse struct {
	ArchivePath string                 `json:"archivePath"`
	Version     string                 `json:"version"`
	Directory   string                 `json:"directory"`
	Entries     []archiveEntryResponse `json:"entries"`
}

type archiveEntryResponse struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	Kind       string `json:"kind"`
	Size       int64  `json:"size"`
	ModifiedAt string `json:"modifiedAt"`
}

type archiveExtractRequest struct {
	Path        string   `json:"path"`
	Version     string   `json:"version"`
	Destination string   `json:"destination"`
	Entries     []string `json:"entries"`
}

type archiveExtractResponse struct {
	Status string                       `json:"status"`
	Paths  []string                     `json:"paths"`
	Errors []fsroot.ArchiveExtractError `json:"errors"`
}

func archiveReadLimits() fsroot.ArchiveReadLimits {
	return fsroot.ArchiveReadLimits{
		MaxArchiveBytes:         archiveReadMaxArchiveBytes,
		MaxExternalArchiveBytes: archiveReadMaxExternalArchiveBytes,
		MaxEntries:              archiveReadMaxEntries,
		MaxEntryBytes:           archiveReadMaxEntryBytes,
		MaxTotalBytes:           archiveReadMaxTotalBytes,
		MaxPreviewBytes:         archiveReadMaxPreviewBytes,
		MaxRatio:                archiveReadMaxRatio,
	}
}

func (api *API) archiveEntries(w http.ResponseWriter, r *http.Request) {
	archivePath, version, directory, ok := parseArchiveReadQuery(w, r, true)
	if !ok {
		return
	}
	if err := fsroot.ValidateArchivePath(directory, true); err != nil {
		writeError(w, http.StatusBadRequest, "归档内目录路径无效。")
		return
	}
	reader, ok := api.files.(archiveReaderFileSystem)
	if !ok {
		writeError(w, http.StatusNotImplemented, "当前服务不支持安全的压缩包浏览。")
		return
	}
	releaseSlot, ok := api.acquireArchiveReadSlot(w, r)
	if !ok {
		return
	}
	defer releaseSlot()
	release, ok := api.lockMutationPaths(w, r, archivePath)
	if !ok {
		return
	}
	defer release()
	if _, err := api.verifyArchiveVersion(archivePath, version); err != nil {
		writeArchiveReadError(w, err)
		return
	}
	entries, err := reader.ListArchive(r.Context(), archivePath, directory, archiveReadLimits())
	if err != nil {
		writeArchiveReadError(w, err)
		return
	}
	if _, err := api.verifyArchiveVersion(archivePath, version); err != nil {
		writeArchiveReadError(w, err)
		return
	}
	response := archiveEntriesResponse{ArchivePath: archivePath, Version: version, Directory: directory, Entries: make([]archiveEntryResponse, 0, len(entries))}
	for _, entry := range entries {
		response.Entries = append(response.Entries, archiveEntryResponse{
			Name: entry.Name, Path: entry.Path, Kind: entry.Kind, Size: entry.Size,
			ModifiedAt: formatArchiveTime(entry.ModifiedAt),
		})
	}
	writeJSON(w, http.StatusOK, response)
}

func (api *API) archiveEntryContent(w http.ResponseWriter, r *http.Request) {
	archivePath, version, ok := parseArchiveContentQuery(w, r)
	if !ok {
		return
	}
	entryPath := r.URL.Query().Get("entry")
	if err := fsroot.ValidateArchivePath(entryPath, false); err != nil {
		writeError(w, http.StatusBadRequest, "归档内文件路径无效。")
		return
	}
	reader, ok := api.files.(archiveReaderFileSystem)
	if !ok {
		writeError(w, http.StatusNotImplemented, "当前服务不支持安全的压缩包阅读。")
		return
	}
	releaseSlot, ok := api.acquireArchiveReadSlot(w, r)
	if !ok {
		return
	}
	defer releaseSlot()
	release, ok := api.lockMutationPaths(w, r, archivePath)
	if !ok {
		return
	}
	defer release()
	if _, err := api.verifyArchiveVersion(archivePath, version); err != nil {
		writeArchiveReadError(w, err)
		return
	}
	content, entry, err := reader.ReadArchiveEntry(r.Context(), archivePath, entryPath, archiveReadLimits())
	if err != nil {
		writeArchiveReadError(w, err)
		return
	}
	if _, err := api.verifyArchiveVersion(archivePath, version); err != nil {
		writeArchiveReadError(w, err)
		return
	}
	contentType := mime.TypeByExtension(strings.ToLower(filepath.Ext(entry.Name)))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	contentDisposition := mime.FormatMediaType("inline", map[string]string{"filename": entry.Name})
	w.Header().Set("Content-Type", contentType)
	if contentDisposition != "" {
		w.Header().Set("Content-Disposition", contentDisposition)
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-FileSage-Version", version)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

func (api *API) extractArchive(w http.ResponseWriter, r *http.Request) {
	if !requireSameOrigin(w, r) {
		return
	}
	request, ok := parseArchiveExtractRequest(w, r)
	if !ok {
		return
	}
	if !validRelativePath(request.Path, false) || isReservedPath(request.Path) || request.Version == "" || len(request.Version) > 256 {
		writeError(w, http.StatusBadRequest, "压缩包路径或版本无效。")
		return
	}
	if !validRelativePath(request.Destination, true) || isReservedPath(request.Destination) {
		writeError(w, http.StatusBadRequest, "解压目录必须是管理根目录内的现有目录。")
		return
	}
	if len(request.Entries) > maxArchiveExtractSelection {
		writeError(w, http.StatusRequestEntityTooLarge, "选择的压缩包条目过多。")
		return
	}
	for _, entry := range request.Entries {
		if err := fsroot.ValidateArchivePath(entry, false); err != nil {
			writeError(w, http.StatusBadRequest, "压缩包条目路径无效。")
			return
		}
	}
	reader, ok := api.files.(archiveReaderFileSystem)
	if !ok {
		writeError(w, http.StatusNotImplemented, "当前服务不支持安全的压缩包解压。")
		return
	}
	releaseSlot, ok := api.acquireArchiveReadSlot(w, r)
	if !ok {
		return
	}
	defer releaseSlot()
	release, ok := api.lockMutationPaths(w, r, request.Path, request.Destination)
	if !ok {
		return
	}
	defer release()
	if _, err := api.verifyArchiveVersion(request.Path, request.Version); err != nil {
		writeArchiveReadError(w, err)
		return
	}
	if request.Destination != "" {
		destination, err := api.archiveDestination(request.Destination)
		if err != nil {
			writeArchiveReadError(w, err)
			return
		}
		if destination.Symlink || destination.Kind != fsroot.KindDirectory {
			writeError(w, http.StatusBadRequest, "解压目录必须是现有的真实目录。")
			return
		}
	} else if _, err := api.files.List(""); err != nil {
		writeArchiveReadError(w, err)
		return
	}
	auditID, ok := beginFileAudit(api, w, r, "manual_archive_extract", request.Path, request.Destination)
	if !ok {
		return
	}
	result, err := reader.ExtractArchive(r.Context(), request.Path, request.Destination, request.Entries, archiveReadLimits())
	if err != nil {
		if len(result.Paths) > 0 {
			if !finishFileAudit(api, w, r, auditID, "partial", "解压部分完成，请核查结果。") {
				return
			}
			if len(result.Errors) == 0 {
				result.Errors = append(result.Errors, fsroot.ArchiveExtractError{Entry: "", Message: "解压未能全部完成；请刷新目录核查。"})
			}
			writeJSON(w, http.StatusMultiStatus, archiveExtractResponse{Status: "partial", Paths: result.Paths, Errors: result.Errors})
			return
		}
		if !finishFileAudit(api, w, r, auditID, "failed", "解压未完成。") {
			return
		}
		writeArchiveReadError(w, err)
		return
	}
	outcome, message := "succeeded", "解压完成。"
	if len(result.Errors) > 0 {
		outcome, message = "partial", "解压部分完成，请核查结果。"
	}
	if !finishFileAudit(api, w, r, auditID, outcome, message) {
		return
	}
	writeJSON(w, http.StatusCreated, archiveExtractResponse{Status: "complete", Paths: result.Paths, Errors: []fsroot.ArchiveExtractError{}})
}

// acquireArchiveReadSlot 限制重型归档任务并响应请求取消。
func (api *API) acquireArchiveReadSlot(w http.ResponseWriter, r *http.Request) (func(), bool) {
	if api.archiveReadSlots == nil {
		writeError(w, http.StatusServiceUnavailable, "压缩包处理暂不可用，请稍后重试。")
		return func() {}, false
	}
	select {
	case api.archiveReadSlots <- struct{}{}:
		return func() { <-api.archiveReadSlots }, true
	case <-r.Context().Done():
		writeArchiveReadError(w, r.Context().Err())
		return func() {}, false
	}
}

func parseArchiveReadQuery(w http.ResponseWriter, r *http.Request, includeDirectory bool) (string, string, string, bool) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, "压缩包查询参数无效。")
		return "", "", "", false
	}
	allowed := map[string]bool{"path": true, "version": true}
	if includeDirectory {
		allowed["directory"] = true
	}
	for key, values := range values {
		if !allowed[key] || len(values) != 1 {
			writeError(w, http.StatusBadRequest, "压缩包查询参数无效或重复。")
			return "", "", "", false
		}
	}
	archivePath, version := values.Get("path"), values.Get("version")
	directory := values.Get("directory")
	if !validRelativePath(archivePath, false) || isReservedPath(archivePath) || version == "" || len(version) > 256 || includeDirectory && !hasSingleQueryValue(values, "directory") {
		writeError(w, http.StatusBadRequest, "压缩包路径、版本或目录无效。")
		return "", "", "", false
	}
	return archivePath, version, directory, true
}

func parseArchiveContentQuery(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(values) != 3 || len(values["path"]) != 1 || len(values["version"]) != 1 || len(values["entry"]) != 1 {
		writeError(w, http.StatusBadRequest, "压缩包内容查询参数无效。")
		return "", "", false
	}
	for key := range values {
		if key != "path" && key != "version" && key != "entry" {
			writeError(w, http.StatusBadRequest, "压缩包内容查询参数无效。")
			return "", "", false
		}
	}
	archivePath, version := values.Get("path"), values.Get("version")
	if !validRelativePath(archivePath, false) || isReservedPath(archivePath) || version == "" || len(version) > 256 {
		writeError(w, http.StatusBadRequest, "压缩包路径或版本无效。")
		return "", "", false
	}
	return archivePath, version, true
}

func hasSingleQueryValue(values url.Values, key string) bool {
	value, exists := values[key]
	return exists && len(value) == 1
}

func parseArchiveExtractRequest(w http.ResponseWriter, r *http.Request) (archiveExtractRequest, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxArchiveExtractBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var request archiveExtractRequest
	if err := decoder.Decode(&request); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "解压请求内容过大。")
			return archiveExtractRequest{}, false
		}
		writeError(w, http.StatusBadRequest, "解压请求 JSON 无效。")
		return archiveExtractRequest{}, false
	}
	var trailing any
	if !errors.Is(decoder.Decode(&trailing), io.EOF) {
		writeError(w, http.StatusBadRequest, "解压请求必须只有一个 JSON 对象。")
		return archiveExtractRequest{}, false
	}
	return request, true
}

func (api *API) verifyArchiveVersion(relativePath, version string) (fsroot.Entry, error) {
	entry, err := api.verifyEntryVersion(relativePath, version)
	if err != nil {
		return fsroot.Entry{}, err
	}
	if entry.Symlink || entry.Kind != fsroot.KindFile {
		return fsroot.Entry{}, fsroot.ErrNotRegularFile
	}
	return entry, nil
}

func (api *API) archiveDestination(relativePath string) (fsroot.Entry, error) {
	entries, err := api.files.List(parentPath(relativePath))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fsroot.Entry{}, fsroot.ErrConflict
		}
		return fsroot.Entry{}, err
	}
	for _, entry := range entries {
		if entry.Path != relativePath {
			continue
		}
		if entry.Symlink || entry.Kind != fsroot.KindDirectory {
			return fsroot.Entry{}, fsroot.ErrNotDirectory
		}
		if !entry.IdentityAvailable {
			return fsroot.Entry{}, fsroot.ErrIdentityUnavailable
		}
		return entry, nil
	}
	return fsroot.Entry{}, fsroot.ErrConflict
}

func writeArchiveReadError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, fsroot.ErrArchiveEncrypted):
		writeError(w, http.StatusUnprocessableEntity, "压缩包已加密，当前不支持解密。")
	case errors.Is(err, fsroot.ErrArchiveUnsafeEntry):
		writeError(w, http.StatusUnprocessableEntity, "压缩包包含越界路径、链接或特殊文件，已拒绝操作。")
	case errors.Is(err, fsroot.ErrArchiveLimitExceeded):
		writeError(w, http.StatusRequestEntityTooLarge, "压缩包超过安全处理上限。")
	case errors.Is(err, fsroot.ErrArchiveCorrupt):
		writeError(w, http.StatusUnprocessableEntity, "压缩包损坏或格式无效。")
	case errors.Is(err, fsroot.ErrArchiveUnsupportedFile), errors.Is(err, fsroot.ErrArchiveUnsupported):
		writeError(w, http.StatusNotImplemented, "此压缩格式、条目或压缩算法当前不受支持。")
	case errors.Is(err, fsroot.ErrArchiveEntryNotFound), errors.Is(err, fsroot.ErrNotRegularFile):
		writeError(w, http.StatusNotFound, "压缩包内未找到可读取的普通文件。")
	case errors.Is(err, fsroot.ErrArchiveStorage):
		writeError(w, http.StatusInsufficientStorage, "服务器暂存空间不足，操作未完成。")
	case errors.Is(err, fsroot.ErrConflict), errors.Is(err, fsroot.ErrPathChanged):
		writeError(w, http.StatusConflict, "压缩包或目标目录状态已变化，请刷新后重试。")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusRequestTimeout, "压缩包操作已取消。")
	case errors.Is(err, fsroot.ErrInvalidPath), errors.Is(err, fsroot.ErrOutsideRoot), errors.Is(err, fsroot.ErrNotDirectory), errors.Is(err, fsroot.ErrDanglingLink), errors.Is(err, fsroot.ErrLinkCycle):
		writeError(w, http.StatusBadRequest, "归档或目标路径无效。")
	case errors.Is(err, fsroot.ErrNoReplaceUnsupported), errors.Is(err, fsroot.ErrAtomicWriteUnsupported):
		writeError(w, http.StatusNotImplemented, "当前文件系统不支持安全的无覆盖发布。")
	default:
		writeError(w, http.StatusInternalServerError, "压缩包操作失败，请刷新目录核查。")
	}
}

func formatArchiveTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}
