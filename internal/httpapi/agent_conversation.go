// 本文件提供持久化 Agent 对话与确认分析接口。
package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"filesage/internal/agent"
	"filesage/internal/store"
)

type conversationRequest struct {
	Message string `json:"message"`
}

type analysisConfirmationRequest struct {
	EnableOCR bool `json:"enableOCR"`
}

type conversationMessageResponse struct {
	Role      string `json:"role"`
	Content   string `json:"content"`
	CreatedAt string `json:"createdAt"`
}

type analysisCardResponse struct {
	Action            string                    `json:"action"`
	Goal              string                    `json:"goal"`
	FilePath          string                    `json:"filePath"`
	TargetPath        string                    `json:"targetPath"`
	RequestedMode     *uint32                   `json:"requestedMode"`
	RequestedOwnerUID *uint32                   `json:"requestedOwnerUid"`
	RequestedGroupGID *uint32                   `json:"requestedGroupGid"`
	Operations        []store.AnalysisOperation `json:"operations"`
	DataTypes         []string                  `json:"dataTypes"`
	Provider          string                    `json:"provider"`
	Model             string                    `json:"model"`
	Destination       string                    `json:"destination"`
}

type conversationResponse struct {
	ID              string                        `json:"id"`
	State           store.ConversationState       `json:"state"`
	Messages        []conversationMessageResponse `json:"messages"`
	AnalysisCard    *analysisCardResponse         `json:"analysisCard,omitempty"`
	TaskID          string                        `json:"taskId,omitempty"`
	PlanID          string                        `json:"planId,omitempty"`
	DuplicateScanID string                        `json:"duplicateScanId,omitempty"`
	Error           string                        `json:"error,omitempty"`
}

type conversationSummaryResponse struct {
	ID              string                  `json:"id"`
	State           store.ConversationState `json:"state"`
	UpdatedAt       string                  `json:"updatedAt"`
	Title           string                  `json:"title"`
	TaskID          string                  `json:"taskId,omitempty"`
	PlanID          string                  `json:"planId,omitempty"`
	DuplicateScanID string                  `json:"duplicateScanId,omitempty"`
}

type conversationListResponse struct {
	Conversations []conversationSummaryResponse `json:"conversations"`
	Limit         int                           `json:"limit"`
	Offset        int                           `json:"offset"`
	HasMore       bool                          `json:"hasMore"`
}

func (api *API) agentStatus(w http.ResponseWriter, r *http.Request) {
	if api.conversations == nil {
		writeJSON(w, http.StatusOK, agent.ModelStatus{Available: false, Reason: "not_configured"})
		return
	}
	writeJSON(w, http.StatusOK, api.conversations.Status())
}

func (api *API) listAgentConversations(w http.ResponseWriter, r *http.Request) {
	if api.conversations == nil {
		writeError(w, http.StatusServiceUnavailable, "Agent conversation service is unavailable")
		return
	}
	query := r.URL.Query()
	for key, values := range query {
		if key != "limit" && key != "offset" {
			writeError(w, http.StatusBadRequest, "invalid conversation list query")
			return
		}
		if len(values) != 1 || values[0] == "" {
			writeError(w, http.StatusBadRequest, "invalid conversation list query")
			return
		}
	}
	limit, offset := 20, 0
	var err error
	if value := query.Get("limit"); value != "" {
		limit, err = strconv.Atoi(value)
		if err != nil || limit < 1 || limit > 100 {
			writeError(w, http.StatusBadRequest, "invalid conversation list limit")
			return
		}
	}
	if value := query.Get("offset"); value != "" {
		offset, err = strconv.Atoi(value)
		if err != nil || offset < 0 || offset > 100000 {
			writeError(w, http.StatusBadRequest, "invalid conversation list offset")
			return
		}
	}
	summaries, hasMore, err := api.conversations.List(r.Context(), limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Agent conversation history is unavailable")
		return
	}
	response := conversationListResponse{Conversations: make([]conversationSummaryResponse, 0, len(summaries)), Limit: limit, Offset: offset, HasMore: hasMore}
	for _, item := range summaries {
		response.Conversations = append(response.Conversations, conversationSummaryResponse{
			ID: item.ID, State: item.State, UpdatedAt: item.UpdatedAt.Format(time.RFC3339Nano), Title: item.Title, TaskID: item.TaskID, PlanID: item.PlanID, DuplicateScanID: item.DuplicateScanID,
		})
	}
	writeJSON(w, http.StatusOK, response)
}

func (api *API) createAgentConversation(w http.ResponseWriter, r *http.Request) {
	if !requireSameOrigin(w, r) {
		return
	}
	if api.conversations == nil {
		writeError(w, http.StatusServiceUnavailable, "Agent conversation service is unavailable")
		return
	}
	var request conversationRequest
	if !decodeRequestJSON(w, r, &request) {
		return
	}
	conversation, err := api.conversations.Create(r.Context(), request.Message)
	if conversation.ID != "" {
		writeJSON(w, http.StatusCreated, toConversationResponse(conversation))
		return
	}
	writeConversationError(w, err)
}

func (api *API) getAgentConversation(w http.ResponseWriter, r *http.Request) {
	if api.conversations == nil {
		writeError(w, http.StatusServiceUnavailable, "Agent conversation service is unavailable")
		return
	}
	conversation, err := api.conversations.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeConversationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toConversationResponse(conversation))
}

func (api *API) addAgentConversationMessage(w http.ResponseWriter, r *http.Request) {
	if !requireSameOrigin(w, r) {
		return
	}
	if api.conversations == nil {
		writeError(w, http.StatusServiceUnavailable, "Agent conversation service is unavailable")
		return
	}
	var request conversationRequest
	if !decodeRequestJSON(w, r, &request) {
		return
	}
	conversation, err := api.conversations.AddMessage(r.Context(), r.PathValue("id"), request.Message)
	if conversation.ID != "" {
		writeJSON(w, http.StatusOK, toConversationResponse(conversation))
		return
	}
	writeConversationError(w, err)
}

func (api *API) confirmAgentConversationAnalysis(w http.ResponseWriter, r *http.Request) {
	if !requireSameOrigin(w, r) {
		return
	}
	var request analysisConfirmationRequest
	if !decodeRequestJSON(w, r, &request) {
		return
	}
	if api.conversations == nil {
		writeError(w, http.StatusServiceUnavailable, "Agent conversation service is unavailable")
		return
	}
	conversation, err := api.conversations.ConfirmAnalysis(r.Context(), r.PathValue("id"), request.EnableOCR)
	if err != nil {
		if conversation.ID != "" {
			if conversation.State == store.ConversationFailed {
				writeJSON(w, http.StatusOK, toConversationResponse(conversation))
				return
			}
			writeConversationError(w, err)
			return
		}
		writeConversationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toConversationResponse(conversation))
}

func toConversationResponse(conversation store.AgentConversation) conversationResponse {
	response := conversationResponse{
		ID: conversation.ID, State: conversation.State, Messages: make([]conversationMessageResponse, 0, len(conversation.Messages)),
		TaskID: conversation.TaskID, PlanID: conversation.PlanID, DuplicateScanID: conversation.DuplicateScanID, Error: conversation.Error,
	}
	for _, message := range conversation.Messages {
		response.Messages = append(response.Messages, conversationMessageResponse{
			Role: message.Role, Content: message.Content, CreatedAt: message.CreatedAt.Format(time.RFC3339Nano),
		})
	}
	if conversation.Analysis != nil {
		response.AnalysisCard = &analysisCardResponse{
			Action: conversation.Analysis.Action, Goal: conversation.Analysis.Goal,
			FilePath: conversation.Analysis.FilePath, TargetPath: conversation.Analysis.TargetPath,
			RequestedMode:     conversation.Analysis.RequestedMode,
			RequestedOwnerUID: conversation.Analysis.RequestedOwnerUID, RequestedGroupGID: conversation.Analysis.RequestedGroupGID,
			Operations: conversation.Analysis.Operations,
			DataTypes:  conversation.Analysis.DataTypes, Provider: conversation.Analysis.Provider,
			Model: conversation.Analysis.Model, Destination: conversation.Analysis.Destination,
		}
	}
	return response
}

func writeConversationError(w http.ResponseWriter, err error) {
	message := "Agent conversation request failed"
	switch {
	case errors.Is(err, store.ErrConversationNotFound), errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "Agent conversation not found")
	case errors.Is(err, agent.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid Agent conversation request")
	case errors.Is(err, store.ErrConversationConflict), errors.Is(err, store.ErrConflict), errors.Is(err, agent.ErrConflict):
		writeError(w, http.StatusConflict, "Agent conversation state or file version changed")
	case errors.Is(err, store.ErrConversationLimit):
		writeError(w, http.StatusRequestEntityTooLarge, "Agent conversation limit reached")
	case errors.Is(err, agent.ErrMessageTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "Agent conversation message is too large")
	case errors.Is(err, agent.ErrUnsupported):
		writeError(w, http.StatusUnprocessableEntity, "Agent operation is not supported")
	case errors.Is(err, agent.ErrModelUnavailable):
		writeError(w, http.StatusServiceUnavailable, "Agent model is not configured")
	case errors.Is(err, agent.ErrModelRequest), errors.Is(err, agent.ErrModelResponse):
		writeError(w, http.StatusBadGateway, "Agent model request failed")
	default:
		writeError(w, http.StatusInternalServerError, message)
	}
}
