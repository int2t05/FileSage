// 本文件提供 Agent Shell 变化查询与差异预览接口。
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"filesage/internal/agent"
	"filesage/internal/store"
)

type agentShellHTTPWorkflow interface {
	ShellChanges(context.Context, string, int, int) (store.ShellChangePage, error)
	PreviewShellChange(context.Context, string, string) (agent.ShellChangePreview, error)
}

type agentShellChangeStateResponse struct {
	SHA256      string `json:"sha256"`
	Mode        uint32 `json:"mode"`
	IsDirectory bool   `json:"isDirectory"`
	Size        int64  `json:"size"`
}

type agentShellChangeResponse struct {
	ID            string                         `json:"id"`
	Path          string                         `json:"path"`
	Kind          string                         `json:"kind"`
	Approval      store.ApprovalState            `json:"approval"`
	Status        store.OperationStatus          `json:"status"`
	Binary        bool                           `json:"binary"`
	DiffTruncated bool                           `json:"diffTruncated"`
	Before        *agentShellChangeStateResponse `json:"before"`
	After         *agentShellChangeStateResponse `json:"after"`
}

type agentShellChangesResponse struct {
	Changes []agentShellChangeResponse `json:"changes"`
	Offset  int                        `json:"offset"`
	Limit   int                        `json:"limit"`
	Total   int                        `json:"total"`
	HasMore bool                       `json:"hasMore"`
}

type agentShellChangePreviewResponse struct {
	Change   agentShellChangeResponse `json:"change"`
	TextDiff string                   `json:"textDiff"`
}

// listAgentShellChanges 返回无缓存的变更分页。
func (api *API) listAgentShellChanges(w http.ResponseWriter, r *http.Request) {
	setNoStore(w)
	workflow, ok := api.agent.(agentShellHTTPWorkflow)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "Agent Shell review is not configured")
		return
	}
	planID := r.PathValue("planId")
	if !validTaskID(planID) {
		writeError(w, http.StatusBadRequest, "invalid plan identifier")
		return
	}
	limit, offset, ok := shellPageQuery(w, r)
	if !ok {
		return
	}
	page, err := workflow.ShellChanges(r.Context(), planID, limit, offset)
	if err != nil {
		if errors.Is(err, store.ErrShellPagination) {
			writeError(w, http.StatusBadRequest, "invalid Shell change page")
			return
		}
		writeAgentError(w, err)
		return
	}
	response := agentShellChangesResponse{
		Changes: make([]agentShellChangeResponse, 0, len(page.Changes)),
		Offset:  page.Offset, Limit: page.Limit, Total: page.Total, HasMore: page.HasMore,
	}
	for _, change := range page.Changes {
		item, err := toAgentShellChangeResponse(change)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "Agent Shell change is unavailable")
			return
		}
		response.Changes = append(response.Changes, item)
	}
	writeJSON(w, http.StatusOK, response)
}

// previewAgentShellChange 按需读取单项文本差异，不缓存响应。
func (api *API) previewAgentShellChange(w http.ResponseWriter, r *http.Request) {
	setNoStore(w)
	workflow, ok := api.agent.(agentShellHTTPWorkflow)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "Agent Shell review is not configured")
		return
	}
	planID, changeID := r.PathValue("planId"), r.PathValue("changeId")
	if !validTaskID(planID) || !validTaskID(changeID) {
		writeError(w, http.StatusBadRequest, "invalid plan or change identifier")
		return
	}
	preview, err := workflow.PreviewShellChange(r.Context(), planID, changeID)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	change, err := toAgentShellChangeResponse(preview.Change)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Agent Shell change is unavailable")
		return
	}
	writeJSON(w, http.StatusOK, agentShellChangePreviewResponse{Change: change, TextDiff: preview.Diff})
}

func shellPageQuery(w http.ResponseWriter, r *http.Request) (int, int, bool) {
	query := r.URL.Query()
	for key, values := range query {
		if key != "limit" && key != "offset" || len(values) != 1 || values[0] == "" {
			writeError(w, http.StatusBadRequest, "invalid Shell change list query")
			return 0, 0, false
		}
	}
	limit, offset := 50, 0
	var err error
	if value := query.Get("limit"); value != "" {
		limit, err = strconv.Atoi(value)
		if err != nil || limit < 1 || limit > store.MaxShellChangePageSize {
			writeError(w, http.StatusBadRequest, "invalid Shell change page limit")
			return 0, 0, false
		}
	}
	if value := query.Get("offset"); value != "" {
		offset, err = strconv.Atoi(value)
		if err != nil || offset < 0 || offset > store.MaxShellChanges {
			writeError(w, http.StatusBadRequest, "invalid Shell change page offset")
			return 0, 0, false
		}
	}
	return limit, offset, true
}

func toAgentShellChangeResponse(change store.ShellChangeRecord) (agentShellChangeResponse, error) {
	var before *agentShellChangeStateResponse
	var original struct {
		Fingerprint struct {
			Exists *bool `json:"exists"`
		} `json:"fingerprint"`
		SHA256      string `json:"sha256"`
		Mode        uint32 `json:"mode"`
		IsDirectory bool   `json:"isDirectory"`
		Size        int64  `json:"size"`
	}
	if err := json.Unmarshal([]byte(change.OriginalStateJSON), &original); err != nil || original.Fingerprint.Exists == nil || original.Size < 0 {
		return agentShellChangeResponse{}, errors.New("invalid Shell original state")
	}
	if *original.Fingerprint.Exists {
		before = &agentShellChangeStateResponse{
			SHA256: original.SHA256, Mode: original.Mode,
			IsDirectory: original.IsDirectory, Size: original.Size,
		}
	}
	var after *agentShellChangeStateResponse
	if change.Kind != "deleted" {
		if change.CandidateMode == nil || change.CandidateSize < 0 {
			return agentShellChangeResponse{}, errors.New("invalid Shell candidate state")
		}
		after = &agentShellChangeStateResponse{
			SHA256: change.CandidateSHA256, Mode: *change.CandidateMode,
			IsDirectory: change.CandidateSHA256 == "", Size: change.CandidateSize,
		}
	}
	return agentShellChangeResponse{
		ID: change.ID, Path: change.Path, Kind: change.Kind,
		Approval: change.Approval, Status: change.Status,
		Binary: change.Binary, DiffTruncated: change.Truncated,
		Before: before, After: after,
	}, nil
}
