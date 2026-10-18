// 本文件提供受版本保护的 UTF-8 文本保存接口。
package httpapi

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"filesage/internal/fsroot"
)

const maxEditableTextSize = 2 << 20

var editableTextExtensions = map[string]struct{}{
	".bash": {}, ".c": {}, ".cc": {}, ".cfg": {}, ".conf": {}, ".cpp": {}, ".css": {},
	".csv": {}, ".go": {}, ".h": {}, ".hpp": {}, ".htm": {}, ".html": {}, ".ini": {},
	".java": {}, ".js": {}, ".json": {}, ".jsx": {}, ".log": {}, ".md": {}, ".markdown": {},
	".mdown": {}, ".ps1": {}, ".py": {}, ".rb": {}, ".rs": {}, ".scss": {}, ".sh": {},
	".sql": {}, ".sum": {}, ".svg": {}, ".toml": {}, ".ts": {}, ".tsx": {}, ".txt": {},
	".xml": {}, ".yaml": {}, ".yml": {},
}

type atomicTextWriter interface {
	WriteAtomicIfUnchanged(string, fsroot.Fingerprint, []byte, fs.FileMode) error
}

type textWriteResponse struct {
	Path    string `json:"path"`
	Version string `json:"version"`
}

// writeTextFile 按版本令牌替换文本，或以新路径安全创建草稿副本。
func (api *API) writeTextFile(w http.ResponseWriter, r *http.Request) {
	if !requireSameOrigin(w, r) {
		return
	}
	relativePath, ok := requestPath(w, r)
	if !ok {
		return
	}
	if relativePath == "" || isReservedPath(relativePath) {
		writeError(w, http.StatusBadRequest, "path must identify a file within the managed root")
		return
	}
	if !validEntryName(path.Base(relativePath)) {
		writeError(w, http.StatusBadRequest, "path must end in a valid file name")
		return
	}
	if !editableTextPath(relativePath) {
		writeError(w, http.StatusUnsupportedMediaType, "此文件格式不支持网页内编辑。")
		return
	}
	if !textRequestContentType(r) {
		writeError(w, http.StatusUnsupportedMediaType, "只接受 UTF-8 纯文本内容。")
		return
	}

	versionValues := r.Header.Values("X-FileSage-Version")
	createValues := r.Header.Values("If-None-Match")
	create := len(createValues) == 1 && createValues[0] == "*" && len(versionValues) == 0
	if (!create && (len(versionValues) != 1 || versionValues[0] == "" || len(createValues) != 0)) || (len(createValues) > 0 && !create) {
		writeError(w, http.StatusBadRequest, "保存需要有效的文件版本或新文件条件。")
		return
	}

	content, err := readEditableText(w, r)
	if err != nil {
		if errors.Is(err, errTextTooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "文本文件超过 2 MiB 保存上限。")
			return
		}
		if errors.Is(err, errInvalidText) {
			writeError(w, http.StatusBadRequest, "文本必须是有效的 UTF-8，且不能包含空字符。")
			return
		}
		writeError(w, http.StatusBadRequest, "无法读取文本内容。")
		return
	}

	writer, supported := api.files.(atomicTextWriter)
	if !supported {
		writeError(w, http.StatusNotImplemented, "当前文件系统不支持安全的原子写入。")
		return
	}
	release, ok := api.lockMutationPaths(w, r, relativePath)
	if !ok {
		return
	}
	defer release()

	expected := fsroot.Fingerprint{}
	permission := fs.FileMode(0o644)
	if !create {
		entry, err := api.verifyEntryVersion(relativePath, versionValues[0])
		if err != nil {
			writeFileError(w, err)
			return
		}
		if entry.Kind != fsroot.KindFile || entry.Symlink {
			writeError(w, http.StatusBadRequest, "仅允许编辑普通文本文件。")
			return
		}
		expected = entry.Fingerprint()
		permission = entry.Mode.Perm()
	}
	sourcePath := relativePath
	if create {
		sourcePath = ""
	}
	auditID, ok := beginFileAudit(api, w, r, "manual_text_save", sourcePath, relativePath)
	if !ok {
		return
	}
	if err := writer.WriteAtomicIfUnchanged(relativePath, expected, content, permission); err != nil {
		if !finishFileAudit(api, w, r, auditID, "failed", "文本保存未完成。") {
			return
		}
		writeFileError(w, err)
		return
	}
	if !finishFileAudit(api, w, r, auditID, "succeeded", "文本保存完成。") {
		return
	}

	entry, err := api.fileEntry(relativePath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "文件已保存，但无法读取新版本信息；请刷新后继续。")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, textWriteResponse{Path: relativePath, Version: versionForEntry(entry)})
}

var (
	errTextTooLarge = errors.New("editable text exceeds limit")
	errInvalidText  = errors.New("editable text is not valid UTF-8")
)

// readEditableText 限制正文大小并拒绝非 UTF-8 或二进制空字符。
func readEditableText(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxEditableTextSize+1)
	content, err := io.ReadAll(r.Body)
	if err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			return nil, errTextTooLarge
		}
		return nil, err
	}
	if len(content) > maxEditableTextSize {
		return nil, errTextTooLarge
	}
	if !utf8.Valid(content) || bytes.IndexByte(content, 0) >= 0 {
		return nil, errInvalidText
	}
	return content, nil
}

// textRequestContentType 仅接受 UTF-8 纯文本媒体类型。
func textRequestContentType(r *http.Request) bool {
	value := r.Header.Get("Content-Type")
	mediaType, parameters, err := mime.ParseMediaType(value)
	if err != nil || mediaType != "text/plain" {
		return false
	}
	charset := strings.ToLower(parameters["charset"])
	return charset == "" || charset == "utf-8"
}

// editableTextPath 限定可由网页编辑的文件名后缀。
func editableTextPath(relativePath string) bool {
	name := strings.ToLower(path.Base(relativePath))
	if name == "dockerfile" || name == "makefile" || name == ".gitignore" || name == ".editorconfig" {
		return true
	}
	_, ok := editableTextExtensions[strings.ToLower(filepath.Ext(name))]
	return ok
}

// fileEntry 从真实文件系统读取写入后的条目状态。
func (api *API) fileEntry(relativePath string) (fsroot.Entry, error) {
	if filesystem, ok := api.files.(indexedFileSystem); ok {
		return filesystem.LstatEntry(relativePath)
	}
	entries, err := api.files.List(parentPath(relativePath))
	if err != nil {
		return fsroot.Entry{}, err
	}
	for _, entry := range entries {
		if entry.Path == relativePath {
			return entry, nil
		}
	}
	return fsroot.Entry{}, fs.ErrNotExist
}
