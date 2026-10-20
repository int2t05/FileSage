// 本文件提供 Agent 权限设置和计划逐步执行接口。
package httpapi

import (
	"net/http"
	"time"

	"filesage/internal/store"
)

type agentSettingsRequest struct {
	BypassPermission *bool `json:"bypassPermission"`
}

type agentPlanStartRequest struct {
	ExpectedPlanVersion    int    `json:"expectedPlanVersion"`
	ExpectedExecutionMode  string `json:"expectedExecutionMode"`
	ConfirmPermanentDelete *bool  `json:"confirmPermanentDelete"`
}

type agentPlanStepRequest struct {
	ExpectedPlanVersion int `json:"expectedPlanVersion"`
}

type agentPlanRejectRequest struct {
	ExpectedPlanVersion int `json:"expectedPlanVersion"`
}

type agentSettingsResponse struct {
	BypassPermission bool   `json:"bypassPermission"`
	UpdatedAt        string `json:"updatedAt"`
}

func (api *API) getAgentSettings(w http.ResponseWriter, r *http.Request) {
	if api.agentSettings == nil {
		writeError(w, http.StatusServiceUnavailable, "Agent settings are not configured")
		return
	}
	settings, err := api.agentSettings.AgentSettings(r.Context())
	if err != nil {
		writeAgentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, agentSettingsResponse{
		BypassPermission: settings.BypassPermission,
		UpdatedAt:        settings.UpdatedAt.Format(time.RFC3339Nano),
	})
}

func (api *API) updateAgentSettings(w http.ResponseWriter, r *http.Request) {
	if !requireSameOrigin(w, r) {
		return
	}
	if api.agentSettings == nil {
		writeError(w, http.StatusServiceUnavailable, "Agent settings are not configured")
		return
	}
	var request agentSettingsRequest
	if !decodeRequestJSON(w, r, &request) {
		return
	}
	if request.BypassPermission == nil {
		writeError(w, http.StatusBadRequest, "bypassPermission is required")
		return
	}
	if err := api.agentSettings.SetAgentBypassPermission(r.Context(), *request.BypassPermission, time.Now().UTC()); err != nil {
		writeAgentError(w, err)
		return
	}
	settings, err := api.agentSettings.AgentSettings(r.Context())
	if err != nil {
		writeAgentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, agentSettingsResponse{
		BypassPermission: settings.BypassPermission,
		UpdatedAt:        settings.UpdatedAt.Format(time.RFC3339Nano),
	})
}

func (api *API) rejectAgentPlan(w http.ResponseWriter, r *http.Request) {
	if !requireSameOrigin(w, r) {
		return
	}
	workflow, ok := api.agent.(agentPlanRejectWorkflow)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "Agent plan rejection is not configured")
		return
	}
	var request agentPlanRejectRequest
	if !decodeRequestJSON(w, r, &request) {
		return
	}
	planID := r.PathValue("planId")
	if !validTaskID(planID) || request.ExpectedPlanVersion < 1 {
		writeError(w, http.StatusBadRequest, "invalid plan identifier or expected plan version")
		return
	}
	plan, err := workflow.RejectPlan(r.Context(), planID, request.ExpectedPlanVersion)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	api.writeAgentPlanResponse(w, r, plan)
}

func (api *API) startAgentPlan(w http.ResponseWriter, r *http.Request) {
	if !requireSameOrigin(w, r) {
		return
	}
	workflow, ok := api.agent.(agentExecutionWorkflow)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "Agent execution workflow is not configured")
		return
	}
	var request agentPlanStartRequest
	if !decodeRequestJSON(w, r, &request) {
		return
	}
	planID := r.PathValue("planId")
	if !validTaskID(planID) || request.ExpectedPlanVersion < 1 || request.ExpectedExecutionMode != "bypass" && request.ExpectedExecutionMode != "manual" || request.ConfirmPermanentDelete == nil {
		writeError(w, http.StatusBadRequest, "invalid plan identifier, expected plan version, execution mode, or permanent-delete confirmation")
		return
	}
	plan, err := workflow.StartPlan(r.Context(), planID, request.ExpectedPlanVersion, request.ExpectedExecutionMode, *request.ConfirmPermanentDelete)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	api.writeAgentPlanResponse(w, r, plan)
}

func (api *API) executeAgentStep(w http.ResponseWriter, r *http.Request) {
	if !requireSameOrigin(w, r) {
		return
	}
	workflow, ok := api.agent.(agentExecutionWorkflow)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "Agent execution workflow is not configured")
		return
	}
	var request agentPlanStepRequest
	if !decodeRequestJSON(w, r, &request) {
		return
	}
	planID, operationID := r.PathValue("planId"), r.PathValue("operationId")
	if !validTaskID(planID) || !validTaskID(operationID) || request.ExpectedPlanVersion < 1 {
		writeError(w, http.StatusBadRequest, "invalid plan, operation, or expected plan version")
		return
	}
	plan, err := workflow.ExecuteStep(r.Context(), planID, operationID, request.ExpectedPlanVersion)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	api.writeAgentPlanResponse(w, r, plan)
}

var _ agentSettingsRepository = (*store.Store)(nil)
