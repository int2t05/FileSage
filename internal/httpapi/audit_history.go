// 本文件提供不含正文的统一操作历史接口。
package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"filesage/internal/store"
)

type auditHistoryResponse struct {
	Events     []auditHistoryEvent `json:"events"`
	Limit      int                 `json:"limit"`
	NextCursor int64               `json:"nextCursor"`
	HasMore    bool                `json:"hasMore"`
}

type auditHistoryEvent struct {
	ID         int64  `json:"id"`
	TaskID     string `json:"taskId,omitempty"`
	EventType  string `json:"eventType"`
	Actor      string `json:"actor"`
	Outcome    string `json:"outcome"`
	SourcePath string `json:"sourcePath,omitempty"`
	TargetPath string `json:"targetPath,omitempty"`
	CreatedAt  string `json:"createdAt"`
}

// auditHistory 返回不含正文和结果说明的统一操作记录。
func (api *API) auditHistory(w http.ResponseWriter, r *http.Request) {
	if api.audits == nil {
		writeError(w, http.StatusServiceUnavailable, "operation history is not configured")
		return
	}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid audit query")
		return
	}
	limit, before, ok := parseAuditHistoryQuery(w, values)
	if !ok {
		return
	}
	page, err := api.audits.AuditHistoryPage(r.Context(), before, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "operation history is unavailable")
		return
	}
	response := auditHistoryResponse{
		Events: make([]auditHistoryEvent, 0, len(page.Events)), Limit: page.Limit,
		NextCursor: page.NextCursor, HasMore: page.HasMore,
	}
	for _, record := range page.Events {
		response.Events = append(response.Events, auditHistoryEvent{
			ID: record.ID, TaskID: safeTaskID(record.TaskID),
			EventType: safeAuditLabel(record.EventType, ""),
			Actor:     safeAuditLabel(record.Actor, ""), Outcome: safeAuditLabel(record.Outcome, ""),
			SourcePath: safeTaskPath(record.SourcePath), TargetPath: safeTaskPath(record.TargetPath),
			CreatedAt: formatAPITime(record.CreatedAt),
		})
	}
	writeJSON(w, http.StatusOK, response)
}

func parseAuditHistoryQuery(w http.ResponseWriter, values url.Values) (int, int64, bool) {
	for key, entries := range values {
		if (key != "limit" && key != "before") || len(entries) != 1 {
			writeError(w, http.StatusBadRequest, "invalid or repeated audit query parameter")
			return 0, 0, false
		}
	}
	limit := 50
	if raw, ok := values["limit"]; ok {
		parsed, err := strconv.Atoi(raw[0])
		if err != nil || parsed < 1 || parsed > 100 {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and 100")
			return 0, 0, false
		}
		limit = parsed
	}
	var before int64
	if raw, ok := values["before"]; ok {
		parsed, err := strconv.ParseInt(raw[0], 10, 64)
		if err != nil || parsed < 1 {
			writeError(w, http.StatusBadRequest, "before must be a positive audit ID")
			return 0, 0, false
		}
		before = parsed
	}
	return limit, before, true
}

func safeTaskID(value string) string {
	if value == "" || !validTaskID(value) {
		return ""
	}
	return value
}

// beginManualAudit 在人工文件操作前登记待完成审计。
func (api *API) beginManualAudit(ctx context.Context, eventType, sourcePath, targetPath string) (int64, error) {
	if api.audits == nil {
		return 0, nil
	}
	return api.audits.BeginManualAudit(ctx, store.AuditRecord{
		EventType: eventType, Actor: "user", Outcome: "pending",
		SourcePath: sourcePath, TargetPath: targetPath,
	})
}

// finishManualAudit 保存人工文件操作的最终结果。
func (api *API) finishManualAudit(ctx context.Context, id int64, outcome, message string) error {
	if id == 0 {
		return nil
	}
	if api.audits == nil {
		return errors.New("operation audit repository is unavailable")
	}
	return api.audits.FinishManualAudit(ctx, id, outcome, message)
}

var _ auditRepository = (*store.Store)(nil)
