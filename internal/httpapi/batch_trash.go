// 本文件处理经过版本校验的批量回收站操作。
package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path"

	"filesage/internal/fsroot"
)

const (
	maxBatchTrashItems      = 256
	maxBatchTrashBodyBytes  = 4 << 20
	maxBatchTrashVersionLen = 256
)

type batchTrashRequest struct {
	Items []batchTrashInput `json:"items"`
}

type batchTrashInput struct {
	Path    string `json:"path"`
	Version string `json:"version"`
}

type batchTrashItem struct {
	Path       string `json:"path"`
	Status     string `json:"status"`
	IncludedIn string `json:"includedIn,omitempty"`
	Error      string `json:"error,omitempty"`
}

type batchTrashResponse struct {
	Items     []batchTrashItem `json:"items"`
	Succeeded int              `json:"succeeded"`
	Failed    int              `json:"failed"`
}

// batchTrash 先整体校验版本，再按请求顺序移动未被祖先目录覆盖的条目。
func (api *API) batchTrash(w http.ResponseWriter, r *http.Request) {
	if !requireSameOrigin(w, r) {
		return
	}
	request, ok := decodeBatchTrashRequest(w, r)
	if !ok {
		return
	}
	paths, ok := validateBatchTrashItems(w, request.Items)
	if !ok {
		return
	}
	if api.trash == nil || api.files == nil {
		writeError(w, http.StatusServiceUnavailable, "回收站服务暂不可用。")
		return
	}

	release, ok := api.lockMutationPaths(w, r, paths...)
	if !ok {
		return
	}
	defer release()

	entries := make(map[string]fsroot.Entry, len(request.Items))
	preflightOK := true
	for _, item := range request.Items {
		entry, err := api.verifyEntryVersion(item.Path, item.Version)
		if err != nil {
			preflightOK = false
			continue
		}
		entries[item.Path] = entry
	}
	if !preflightOK {
		writeError(w, http.StatusConflict, "所选项目状态已变化，请刷新后重试。")
		return
	}

	response := batchTrashResponse{Items: make([]batchTrashItem, len(request.Items))}
	coveredBy := make([]string, len(request.Items))
	for index, item := range request.Items {
		response.Items[index].Path = item.Path
		coveredBy[index] = batchTrashAncestor(item.Path, entries)
	}

	moveSucceeded := make(map[string]bool, len(request.Items))
	auditUnavailable := false
	operationAttempted := false
	for index, item := range request.Items {
		if coveredBy[index] != "" {
			continue
		}
		if auditUnavailable {
			response.Items[index] = failedBatchTrashItem(item.Path, "操作记录暂不可用，未执行此项。")
			moveSucceeded[item.Path] = false
			continue
		}
		entry := entries[item.Path]
		if entry.Kind == fsroot.KindOther && !entry.Symlink {
			response.Items[index] = failedBatchTrashItem(item.Path, "特殊文件不能移入回收站。")
			moveSucceeded[item.Path] = false
			continue
		}
		auditID, err := api.beginManualAudit(r.Context(), "manual_batch_trash", item.Path, "")
		if err != nil {
			response.Items[index] = failedBatchTrashItem(item.Path, "操作记录暂不可用，未执行此项。")
			moveSucceeded[item.Path] = false
			auditUnavailable = true
			continue
		}
		operationAttempted = true
		if _, err := api.trash.MoveToTrash(r.Context(), entry); err != nil {
			if !finishFileAudit(api, w, r, auditID, "needs_review", "移入回收站结果待核查。") {
				return
			}
			response.Items[index] = failedBatchTrashItem(item.Path, "移入回收站失败。")
			moveSucceeded[item.Path] = false
			continue
		}
		if !finishFileAudit(api, w, r, auditID, "succeeded", "已移入回收站。") {
			return
		}
		response.Items[index] = batchTrashItem{Path: item.Path, Status: "trashed"}
		moveSucceeded[item.Path] = true
	}

	for index, item := range request.Items {
		ancestor := coveredBy[index]
		if ancestor == "" {
			continue
		}
		if moveSucceeded[ancestor] {
			response.Items[index] = batchTrashItem{Path: item.Path, Status: "included", IncludedIn: ancestor}
			continue
		}
		response.Items[index] = failedBatchTrashItem(item.Path, "所选祖先目录未能移入回收站，子项未处理。")
	}

	for _, item := range response.Items {
		if item.Status == "failed" {
			response.Failed++
		} else {
			response.Succeeded++
		}
	}
	status := http.StatusOK
	if response.Failed != 0 {
		status = http.StatusMultiStatus
	}
	if auditUnavailable && !operationAttempted {
		writeError(w, http.StatusServiceUnavailable, "操作记录暂不可用，未执行文件操作。")
		return
	}
	writeJSON(w, status, response)
}

// decodeBatchTrashRequest 限制请求体大小并拒绝多余字段或尾随 JSON。
func decodeBatchTrashRequest(w http.ResponseWriter, r *http.Request) (batchTrashRequest, bool) {
	var request batchTrashRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxBatchTrashBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeBatchTrashDecodeError(w, err)
		return batchTrashRequest{}, false
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		writeBatchTrashDecodeError(w, err)
		return batchTrashRequest{}, false
	}
	return request, true
}

// writeBatchTrashDecodeError 仅返回稳定的公开错误信息。
func writeBatchTrashDecodeError(w http.ResponseWriter, err error) {
	var sizeErr *http.MaxBytesError
	if errors.As(err, &sizeErr) {
		writeError(w, http.StatusRequestEntityTooLarge, "批量请求内容过大。")
		return
	}
	writeError(w, http.StatusBadRequest, "批量请求格式无效。")
}

// validateBatchTrashItems 在任何锁或文件操作前检查全部选择。
func validateBatchTrashItems(w http.ResponseWriter, items []batchTrashInput) ([]string, bool) {
	if len(items) == 0 || len(items) > maxBatchTrashItems {
		writeError(w, http.StatusBadRequest, "所选项目数量无效。")
		return nil, false
	}
	paths := make([]string, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		if !validRelativePath(item.Path, false) || isReservedPath(item.Path) || item.Version == "" || len(item.Version) > maxBatchTrashVersionLen {
			writeError(w, http.StatusBadRequest, "所选路径或版本无效。")
			return nil, false
		}
		if _, exists := seen[item.Path]; exists {
			writeError(w, http.StatusBadRequest, "所选项目不能重复。")
			return nil, false
		}
		seen[item.Path] = struct{}{}
		paths = append(paths, item.Path)
	}
	return paths, true
}

// batchTrashAncestor 返回覆盖当前条目的最外层真实目录。
func batchTrashAncestor(relativePath string, entries map[string]fsroot.Entry) string {
	ancestor := ""
	for parent := path.Dir(relativePath); parent != "."; parent = path.Dir(parent) {
		entry, selected := entries[parent]
		if selected && entry.Kind == fsroot.KindDirectory && !entry.Symlink {
			ancestor = parent
		}
	}
	return ancestor
}

// failedBatchTrashItem 构造不泄露底层错误的条目结果。
func failedBatchTrashItem(relativePath, message string) batchTrashItem {
	return batchTrashItem{Path: relativePath, Status: "failed", Error: message}
}
