// 本文件提供持久化 Agent 对话与确认分析接口。
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"filesage/internal/agent"
	"filesage/internal/store"
)

type conversationRequest struct {
	Message string `json:"message"`
}

type agentConversationStreamer interface {
	CreateStream(context.Context, string, func(agent.ConversationStreamUpdate)) (store.AgentConversation, error)
	AddMessageStream(context.Context, string, string, func(agent.ConversationStreamUpdate)) (store.AgentConversation, error)
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
	TaskStatus      store.TaskStatus        `json:"taskStatus,omitempty"`
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
			ID: item.ID, State: item.State, UpdatedAt: item.UpdatedAt.Format(time.RFC3339Nano), Title: item.Title, TaskID: item.TaskID, TaskStatus: item.TaskStatus, PlanID: item.PlanID, DuplicateScanID: item.DuplicateScanID,
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
	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		if streamer, ok := api.conversations.(agentConversationStreamer); ok && streamConversation(w, r, func(onUpdate func(agent.ConversationStreamUpdate)) (store.AgentConversation, error) {
			return streamer.CreateStream(r.Context(), request.Message, onUpdate)
		}) {
			return
		}
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

func (api *API) deleteAgentConversation(w http.ResponseWriter, r *http.Request) {
	if !requireSameOrigin(w, r) {
		return
	}
	if api.conversations == nil {
		writeError(w, http.StatusServiceUnavailable, "Agent conversation service is unavailable")
		return
	}
	if err := api.conversations.Delete(r.Context(), r.PathValue("id")); err != nil {
		writeConversationError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		if streamer, ok := api.conversations.(agentConversationStreamer); ok && streamConversation(w, r, func(onUpdate func(agent.ConversationStreamUpdate)) (store.AgentConversation, error) {
			return streamer.AddMessageStream(r.Context(), r.PathValue("id"), request.Message, onUpdate)
		}) {
			return
		}
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

func streamConversation(w http.ResponseWriter, r *http.Request, run func(func(agent.ConversationStreamUpdate)) (store.AgentConversation, error)) bool {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	if err := controller.Flush(); errors.Is(err, http.ErrNotSupported) {
		return false
	} else if err != nil {
		return true
	}
	conversation, err := run(func(update agent.ConversationStreamUpdate) {
		writeConversationEvent(w, controller, "delta", update)
	})
	if conversation.ID != "" {
		writeConversationEvent(w, controller, "done", toConversationResponse(conversation))
		return true
	}
	status, message := conversationErrorDetails(err)
	writeConversationEvent(w, controller, "error", map[string]any{"status": status, "message": message})
	return true
}

func writeConversationEvent(w http.ResponseWriter, controller *http.ResponseController, name string, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		return
	}
	_, _ = w.Write([]byte("event: " + name + "\ndata: " + string(data) + "\n\n"))
	_ = controller.Flush()
}

func writeConversationError(w http.ResponseWriter, err error) {
	status, message := conversationErrorDetails(err)
	writeError(w, status, message)
}

func conversationErrorDetails(err error) (int, string) {
	switch {
	case errors.Is(err, store.ErrConversationNotFound), errors.Is(err, store.ErrNotFound):
		return http.StatusNotFound, "Agent conversation not found"
	case errors.Is(err, agent.ErrInvalid):
		return http.StatusBadRequest, "invalid Agent conversation request"
	case errors.Is(err, store.ErrConversationConflict), errors.Is(err, store.ErrConflict), errors.Is(err, agent.ErrConflict):
		return http.StatusConflict, "Agent conversation state or file version changed"
	case errors.Is(err, store.ErrConversationLimit):
		return http.StatusRequestEntityTooLarge, "Agent conversation limit reached"
	case errors.Is(err, agent.ErrMessageTooLarge):
		return http.StatusRequestEntityTooLarge, "Agent conversation message is too large"
	case errors.Is(err, agent.ErrUnsupported):
		return http.StatusUnprocessableEntity, "Agent operation is not supported"
	case errors.Is(err, agent.ErrModelUnavailable):
		return http.StatusServiceUnavailable, "Agent model is not configured"
	case errors.Is(err, agent.ErrModelRequest), errors.Is(err, agent.ErrModelResponse):
		return http.StatusBadGateway, "Agent model request failed"
	default:
		return http.StatusInternalServerError, "Agent conversation request failed"
	}
}
