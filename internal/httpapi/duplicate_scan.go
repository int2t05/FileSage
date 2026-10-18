// 本文件提供只读重复扫描进度与结果接口。
package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"filesage/internal/agent"
	"filesage/internal/store"
)

type duplicateScanWorkflow interface {
	GetDuplicateScan(context.Context, string) (store.DuplicateScan, error)
	CancelDuplicateScan(context.Context, string) (store.DuplicateScan, error)
	ListDuplicateGroups(context.Context, string, int, int) (store.DuplicateScanGroupPage, error)
	ListDuplicateFiles(context.Context, string, int64, int, int) (store.DuplicateScanFilePage, error)
	CreateDuplicateCleanupPlan(context.Context, string, agent.DuplicateCleanupRequest) (store.PlanRecord, error)
}

type duplicateCleanupPlanRequest struct {
	GroupID    string   `json:"groupId"`
	KeeperPath string   `json:"keeperPath"`
	TrashPaths []string `json:"trashPaths"`
}

type duplicateCleanupPlanResponse struct {
	TaskID string `json:"taskId"`
}

type duplicateScanResponse struct {
	ID                 string                    `json:"id"`
	Status             store.DuplicateScanStatus `json:"status"`
	Phase              string                    `json:"phase"`
	ScopePath          string                    `json:"scopePath"`
	EntriesVisited     int                       `json:"entriesVisited"`
	CandidateFiles     int                       `json:"candidateFiles"`
	FilesHashed        int                       `json:"filesHashed"`
	BytesHashed        int64                     `json:"bytesHashed"`
	TotalBytes         int64                     `json:"totalBytes"`
	GroupCount         int                       `json:"groupCount"`
	DuplicateFileCount int                       `json:"duplicateFileCount"`
	ReclaimableBytes   int64                     `json:"reclaimableBytes"`
	ErrorCode          string                    `json:"errorCode"`
	UpdatedAt          string                    `json:"updatedAt"`
}

type duplicateGroupResponse struct {
	ID             string `json:"id"`
	SizeBytes      int64  `json:"size"`
	DuplicateCount int    `json:"duplicateCount"`
}

type duplicateGroupsResponse struct {
	Groups  []duplicateGroupResponse `json:"groups"`
	Limit   int                      `json:"limit"`
	Offset  int                      `json:"offset"`
	HasMore bool                     `json:"hasMore"`
}

type duplicateFileResponse struct {
	Path       string `json:"path"`
	SizeBytes  int64  `json:"size"`
	ModifiedAt string `json:"modifiedAt"`
}

type duplicateFilesResponse struct {
	Files   []duplicateFileResponse `json:"files"`
	Limit   int                     `json:"limit"`
	Offset  int                     `json:"offset"`
	HasMore bool                    `json:"hasMore"`
}

func (api *API) getDuplicateScan(w http.ResponseWriter, r *http.Request) {
	workflow, ok := api.duplicateScanService(w)
	if !ok {
		return
	}
	scan, err := workflow.GetDuplicateScan(r.Context(), r.PathValue("id"))
	if err != nil {
		writeDuplicateScanError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toDuplicateScanResponse(scan))
}

func (api *API) cancelDuplicateScan(w http.ResponseWriter, r *http.Request) {
	if !requireSameOrigin(w, r) || !requireEmptyRequestBody(w, r) {
		return
	}
	workflow, ok := api.duplicateScanService(w)
	if !ok {
		return
	}
	scan, err := workflow.CancelDuplicateScan(r.Context(), r.PathValue("id"))
	if err != nil {
		writeDuplicateScanError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toDuplicateScanResponse(scan))
}

func (api *API) listDuplicateGroups(w http.ResponseWriter, r *http.Request) {
	workflow, ok := api.duplicateScanService(w)
	if !ok {
		return
	}
	limit, offset, ok := parseDuplicatePage(w, r, 20)
	if !ok {
		return
	}
	page, err := workflow.ListDuplicateGroups(r.Context(), r.PathValue("id"), limit, offset)
	if err != nil {
		writeDuplicateScanError(w, err)
		return
	}
	response := duplicateGroupsResponse{Groups: make([]duplicateGroupResponse, 0, len(page.Groups)), Limit: page.Limit, Offset: page.Offset, HasMore: page.HasMore}
	for _, group := range page.Groups {
		response.Groups = append(response.Groups, duplicateGroupResponse{ID: strconv.FormatInt(group.ID, 10), SizeBytes: group.SizeBytes, DuplicateCount: group.DuplicateCount})
	}
	writeJSON(w, http.StatusOK, response)
}

func (api *API) listDuplicateFiles(w http.ResponseWriter, r *http.Request) {
	workflow, ok := api.duplicateScanService(w)
	if !ok {
		return
	}
	groupID, err := strconv.ParseInt(r.PathValue("groupId"), 10, 64)
	if err != nil || groupID < 1 {
		writeError(w, http.StatusBadRequest, "invalid duplicate group")
		return
	}
	limit, offset, ok := parseDuplicatePage(w, r, 100)
	if !ok {
		return
	}
	page, err := workflow.ListDuplicateFiles(r.Context(), r.PathValue("id"), groupID, limit, offset)
	if err != nil {
		writeDuplicateScanError(w, err)
		return
	}
	response := duplicateFilesResponse{Files: make([]duplicateFileResponse, 0, len(page.Files)), Limit: page.Limit, Offset: page.Offset, HasMore: page.HasMore}
	for _, file := range page.Files {
		response.Files = append(response.Files, duplicateFileResponse{Path: file.Path, SizeBytes: file.SizeBytes, ModifiedAt: file.ModifiedAt.Format(time.RFC3339Nano)})
	}
	writeJSON(w, http.StatusOK, response)
}

func (api *API) createDuplicateCleanupPlan(w http.ResponseWriter, r *http.Request) {
	if !requireSameOrigin(w, r) {
		return
	}
	workflow, ok := api.duplicateScanService(w)
	if !ok {
		return
	}
	var request duplicateCleanupPlanRequest
	if !decodeRequestJSON(w, r, &request) {
		return
	}
	groupID, err := strconv.ParseInt(request.GroupID, 10, 64)
	if err != nil || groupID < 1 {
		writeError(w, http.StatusBadRequest, "invalid duplicate group")
		return
	}
	plan, err := workflow.CreateDuplicateCleanupPlan(r.Context(), r.PathValue("id"), agent.DuplicateCleanupRequest{
		GroupID: groupID, KeeperPath: request.KeeperPath, TrashPaths: request.TrashPaths,
	})
	if err != nil {
		writeDuplicateScanError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, duplicateCleanupPlanResponse{TaskID: plan.TaskID})
}

func (api *API) duplicateScanService(w http.ResponseWriter) (duplicateScanWorkflow, bool) {
	if api.conversations == nil {
		writeError(w, http.StatusServiceUnavailable, "duplicate scan service is unavailable")
		return nil, false
	}
	workflow, ok := api.conversations.(duplicateScanWorkflow)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "duplicate scan service is unavailable")
		return nil, false
	}
	return workflow, true
}

func parseDuplicatePage(w http.ResponseWriter, r *http.Request, defaultLimit int) (int, int, bool) {
	query := r.URL.Query()
	for key, values := range query {
		if key != "limit" && key != "offset" || len(values) != 1 || values[0] == "" {
			writeError(w, http.StatusBadRequest, "invalid duplicate scan page")
			return 0, 0, false
		}
	}
	limit, offset := defaultLimit, 0
	var err error
	if value := query.Get("limit"); value != "" {
		limit, err = strconv.Atoi(value)
		if err != nil || limit < 1 || limit > 100 {
			writeError(w, http.StatusBadRequest, "invalid duplicate scan limit")
			return 0, 0, false
		}
	}
	if value := query.Get("offset"); value != "" {
		offset, err = strconv.Atoi(value)
		if err != nil || offset < 0 || offset > 100_000 {
			writeError(w, http.StatusBadRequest, "invalid duplicate scan offset")
			return 0, 0, false
		}
	}
	return limit, offset, true
}

func toDuplicateScanResponse(scan store.DuplicateScan) duplicateScanResponse {
	return duplicateScanResponse{
		ID: scan.ID, Status: scan.Status, Phase: scan.Phase, ScopePath: scan.ScopePath,
		EntriesVisited: scan.EntriesVisited, CandidateFiles: scan.CandidateFiles,
		FilesHashed: scan.FilesHashed, BytesHashed: scan.BytesHashed, TotalBytes: scan.TotalBytes,
		GroupCount: scan.GroupCount, DuplicateFileCount: scan.DuplicateFileCount,
		ReclaimableBytes: scan.ReclaimableBytes, ErrorCode: scan.ErrorCode,
		UpdatedAt: scan.UpdatedAt.Format(time.RFC3339Nano),
	}
}

func writeDuplicateScanError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "duplicate scan not found")
	case errors.Is(err, store.ErrDuplicateScanUnavailable):
		writeError(w, http.StatusConflict, "duplicate scan results are not available")
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "duplicate scan state changed")
	case errors.Is(err, agent.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid duplicate cleanup selection")
	case errors.Is(err, agent.ErrConflict):
		writeError(w, http.StatusConflict, "duplicate cleanup selection changed")
	case errors.Is(err, agent.ErrUnsupported):
		writeError(w, http.StatusServiceUnavailable, "duplicate cleanup is unavailable")
	default:
		writeError(w, http.StatusInternalServerError, "duplicate scan request failed")
	}
}
