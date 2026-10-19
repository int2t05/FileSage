// 本文件处理经过整批预检的无覆盖批量重命名。
package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"strings"

	"filesage/internal/fsroot"
)

const (
	maxBatchRenameItems      = 256
	maxBatchRenameBodyBytes  = 4 << 20
	maxBatchRenameVersionLen = 256
)

type batchRenameRequest struct {
	Items []batchRenameInput `json:"items"`
}

type batchRenameInput struct {
	Path    string `json:"path"`
	Version string `json:"version"`
	Name    string `json:"name"`
}

type batchRenameItem struct {
	Path       string `json:"path"`
	TargetPath string `json:"targetPath"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Message    string `json:"message,omitempty"`
}

type batchRenameResponse struct {
	Status string            `json:"status"`
	Items  []batchRenameItem `json:"items"`
}

type batchRenameTarget struct {
	request    batchRenameInput
	targetPath string
}

// batchRename 在持锁状态下校验整批，再按请求顺序逐项提交。
func (api *API) batchRename(w http.ResponseWriter, r *http.Request) {
	if !requireSameOrigin(w, r) {
		return
	}
	request, ok := decodeBatchRenameRequest(w, r)
	if !ok {
		return
	}
	targets, paths, ok := validateBatchRenameItems(w, request.Items)
	if !ok {
		return
	}
	if api.files == nil {
		writeError(w, http.StatusServiceUnavailable, "文件服务暂不可用。")
		return
	}
	inspector, ok := api.files.(indexedFileSystem)
	if !ok {
		writeError(w, http.StatusNotImplemented, "文件系统不支持安全批量重命名。")
		return
	}

	release, ok := api.lockMutationPaths(w, r, paths...)
	if !ok {
		return
	}
	defer release()

	if err := validateBatchRenameParents(inspector, targets); err != nil {
		writeBatchRenamePreflightError(w, err)
		return
	}
	if err := api.preflightBatchRename(targets); err != nil {
		writeBatchRenamePreflightError(w, err)
		return
	}

	response := batchRenameResponse{Status: "completed", Items: make([]batchRenameItem, len(targets))}
	for index, target := range targets {
		item := batchRenameItem{
			Path: target.request.Path, TargetPath: target.targetPath,
			Name: target.request.Name, Status: "not_started",
		}
		response.Items[index] = item
	}
	for index, target := range targets {
		if _, err := api.verifyEntryVersion(target.request.Path, target.request.Version); err != nil {
			response.Status = "partial"
			response.Items[index].Status = "failed"
			response.Items[index].Message = batchRenameFailureMessage(err)
			writeJSON(w, http.StatusMultiStatus, response)
			return
		}
		auditID, err := api.beginManualAudit(r.Context(), "manual_batch_rename", target.request.Path, target.targetPath)
		if err != nil {
			if index == 0 {
				writeError(w, http.StatusServiceUnavailable, "操作记录暂不可用，未执行文件操作。")
				return
			}
			response.Status = "partial"
			response.Items[index].Status = "failed"
			response.Items[index].Message = "操作记录暂不可用，未执行此项。"
			writeJSON(w, http.StatusMultiStatus, response)
			return
		}
		if err := api.files.RenameNoReplace(target.request.Path, target.targetPath); err != nil {
			if !finishFileAudit(api, w, r, auditID, "failed", "重命名未完成。") {
				return
			}
			response.Status = "partial"
			response.Items[index].Status = "failed"
			response.Items[index].Message = batchRenameFailureMessage(err)
			writeJSON(w, http.StatusMultiStatus, response)
			return
		}
		if !finishFileAudit(api, w, r, auditID, "succeeded", "重命名完成。") {
			return
		}
		response.Items[index].Status = "renamed"
	}
	writeJSON(w, http.StatusOK, response)
}

// decodeBatchRenameRequest 限制请求体并拒绝重复、未知字段及尾随 JSON。
func decodeBatchRenameRequest(w http.ResponseWriter, r *http.Request) (batchRenameRequest, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBatchRenameBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeBatchRenameDecodeError(w, err)
		return batchRenameRequest{}, false
	}
	var request batchRenameRequest
	if err := json.Unmarshal(body, &request); err != nil {
		writeBatchRenameDecodeError(w, err)
		return batchRenameRequest{}, false
	}
	return request, true
}

func (request *batchRenameRequest) UnmarshalJSON(data []byte) error {
	fields, err := strictObjectFields(data, "items")
	if err != nil {
		return err
	}
	items, ok := fields["items"]
	if !ok {
		return errors.New("items is required")
	}
	return json.Unmarshal(items, &request.Items)
}

func (item *batchRenameInput) UnmarshalJSON(data []byte) error {
	fields, err := strictObjectFields(data, "path", "version", "name")
	if err != nil {
		return err
	}
	for _, field := range []string{"path", "version", "name"} {
		if _, ok := fields[field]; !ok {
			return errors.New("required item field is missing")
		}
	}
	if err := json.Unmarshal(fields["path"], &item.Path); err != nil {
		return err
	}
	if err := json.Unmarshal(fields["version"], &item.Version); err != nil {
		return err
	}
	return json.Unmarshal(fields["name"], &item.Name)
}

func strictObjectFields(data []byte, allowed ...string) (map[string]json.RawMessage, error) {
	allowedFields := make(map[string]struct{}, len(allowed))
	for _, field := range allowed {
		allowedFields[field] = struct{}{}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := token.(json.Delim)
	if !ok || delim != '{' {
		return nil, errors.New("expected object")
	}
	fields := make(map[string]json.RawMessage, len(allowedFields))
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		field, ok := token.(string)
		if !ok {
			return nil, errors.New("expected object field")
		}
		if _, ok := allowedFields[field]; !ok {
			return nil, errors.New("unknown object field")
		}
		if _, duplicate := fields[field]; duplicate {
			return nil, errors.New("duplicate object field")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		fields[field] = value
	}
	token, err = decoder.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := token.(json.Delim); !ok || delim != '}' {
		return nil, errors.New("unterminated object")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("trailing JSON value")
		}
		return nil, err
	}
	return fields, nil
}

func writeBatchRenameDecodeError(w http.ResponseWriter, err error) {
	var sizeErr *http.MaxBytesError
	if errors.As(err, &sizeErr) {
		writeError(w, http.StatusRequestEntityTooLarge, "批量请求内容过大。")
		return
	}
	writeError(w, http.StatusBadRequest, "批量重命名请求格式无效。")
}

func validateBatchRenameItems(w http.ResponseWriter, items []batchRenameInput) ([]batchRenameTarget, []string, bool) {
	if len(items) == 0 || len(items) > maxBatchRenameItems {
		writeError(w, http.StatusBadRequest, "所选项目数量无效。")
		return nil, nil, false
	}
	targets := make([]batchRenameTarget, 0, len(items))
	paths := make([]string, 0, len(items)*2)
	seenSources := make(map[string]struct{}, len(items))
	seenTargets := make(map[string]struct{}, len(items))
	for _, item := range items {
		if !validRelativePath(item.Path, false) || isReservedPath(item.Path) || item.Version == "" || len(item.Version) > maxBatchRenameVersionLen || !validEntryName(item.Name) || item.Name == fsroot.TrashDirectoryName {
			writeError(w, http.StatusBadRequest, "所选路径、版本或名称无效。")
			return nil, nil, false
		}
		if _, exists := seenSources[item.Path]; exists {
			writeError(w, http.StatusBadRequest, "源路径不能重复。")
			return nil, nil, false
		}
		seenSources[item.Path] = struct{}{}
		targetPath := joinPath(parentPath(item.Path), item.Name)
		if _, exists := seenTargets[targetPath]; exists {
			writeError(w, http.StatusBadRequest, "目标路径不能重复。")
			return nil, nil, false
		}
		seenTargets[targetPath] = struct{}{}
		targets = append(targets, batchRenameTarget{request: item, targetPath: targetPath})
		paths = append(paths, item.Path, targetPath)
	}
	for source := range seenSources {
		for other := range seenSources {
			if source != other && strings.HasPrefix(other, source+"/") {
				writeError(w, http.StatusBadRequest, "父目录和其子项不能同时批量重命名。")
				return nil, nil, false
			}
		}
	}
	return targets, paths, true
}

func validateBatchRenameParents(inspector indexedFileSystem, targets []batchRenameTarget) error {
	checked := make(map[string]struct{})
	for _, target := range targets {
		parent := parentPath(target.request.Path)
		components := strings.Split(parent, "/")
		current := ""
		for _, component := range components {
			if component == "" {
				continue
			}
			current = joinPath(current, component)
			if _, ok := checked[current]; ok {
				continue
			}
			entry, err := inspector.LstatEntry(current)
			if err != nil {
				return err
			}
			if entry.Symlink {
				return fsroot.ErrSymlinkPath
			}
			if entry.Kind != fsroot.KindDirectory {
				return fsroot.ErrNotDirectory
			}
			checked[current] = struct{}{}
		}
	}
	return nil
}

func (api *API) preflightBatchRename(targets []batchRenameTarget) error {
	listings := make(map[string]map[string]fsroot.Entry)
	for index := range targets {
		target := &targets[index]
		parent := parentPath(target.request.Path)
		entries, ok := listings[parent]
		if !ok {
			listed, err := api.files.List(parent)
			if err != nil {
				return err
			}
			entries = make(map[string]fsroot.Entry, len(listed))
			for _, entry := range listed {
				entries[entry.Path] = entry
			}
			listings[parent] = entries
		}
		entry, exists := entries[target.request.Path]
		if !exists {
			return fsroot.ErrConflict
		}
		if entry.Symlink {
			if !entry.LinkIdentityAvailable {
				return fsroot.ErrIdentityUnavailable
			}
		} else if !entry.IdentityAvailable {
			return fsroot.ErrIdentityUnavailable
		}
		if versionForEntry(entry) != target.request.Version {
			return fsroot.ErrConflict
		}
		if _, occupied := entries[target.targetPath]; occupied {
			return fsroot.ErrConflict
		}
	}
	return nil
}

func writeBatchRenamePreflightError(w http.ResponseWriter, err error) {
	if errors.Is(err, fs.ErrPermission) || errors.Is(err, fsroot.ErrSymlinkPath) || errors.Is(err, fsroot.ErrNotDirectory) || errors.Is(err, fsroot.ErrInvalidPath) || errors.Is(err, fsroot.ErrOutsideRoot) || errors.Is(err, fsroot.ErrVolumeUnavailable) {
		writeFileError(w, err)
		return
	}
	writeError(w, http.StatusConflict, "所选项目状态已变化或目标名称已被占用，请刷新后重试。")
}

func batchRenameFailureMessage(err error) string {
	switch {
	case errors.Is(err, fsroot.ErrVolumeUnavailable):
		return "管理卷暂不可用，请确认磁盘已挂载后重试。"
	case errors.Is(err, fs.ErrPermission):
		return "没有权限完成此项重命名。"
	case errors.Is(err, fs.ErrExist), errors.Is(err, fsroot.ErrConflict):
		return "目标路径已被占用或文件状态已变化。"
	case errors.Is(err, fs.ErrNotExist):
		return "源路径已变化，未完成重命名。"
	case errors.Is(err, fsroot.ErrSymlinkPath), errors.Is(err, fsroot.ErrNotDirectory), errors.Is(err, fsroot.ErrInvalidPath):
		return "路径边界已变化，未完成重命名。"
	case errors.Is(err, fsroot.ErrNoReplaceUnsupported):
		return "当前文件系统不支持安全的无覆盖重命名。"
	default:
		return "重命名失败，请刷新目录并核对结果。"
	}
}
