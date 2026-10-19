// 本文件验证多个 Agent 会话隔离并可在服务重启后恢复。
package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"filesage/internal/agent"
	"filesage/internal/config"
	"filesage/internal/coordination"
	"filesage/internal/fsroot"
	"filesage/internal/store"
)

func TestAgentConversationSessionsPersistIndependentlyAcrossRestart(t *testing.T) {
	var modelMu sync.Mutex
	var modelUserMessages [][]string
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			t.Errorf("model request = %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		var request struct {
			Messages []agent.ChatMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode model request: %v", err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		var userMessages []string
		for _, message := range request.Messages {
			if message.Role == "user" {
				userMessages = append(userMessages, message.Content)
			}
		}
		modelMu.Lock()
		modelUserMessages = append(modelUserMessages, userMessages)
		modelMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"state\":\"clarifying\",\"reply\":\"请补充具体范围。\"}"}}]}`)
	}))
	defer model.Close()

	ctx := context.Background()
	rootPath := t.TempDir()
	databasePath := filepath.Join(t.TempDir(), "filesage.sqlite")
	modelConfig := config.AgentModelConfig{BaseURL: model.URL + "/v1", Model: "session-test", Provider: "test"}
	type runtime struct {
		metadata      *store.Store
		root          *fsroot.Root
		conversations *agent.ConversationService
		server        *httptest.Server
	}
	startRuntime := func() *runtime {
		metadata, err := store.Open(ctx, databasePath)
		if err != nil {
			t.Fatalf("open metadata store: %v", err)
		}
		root, err := fsroot.Open(rootPath)
		if err != nil {
			_ = metadata.Close()
			t.Fatalf("open filesystem root: %v", err)
		}
		conversations := agent.NewConversationService(metadata, root, nil, modelConfig)
		handler := NewWithAgentAndIndexAndConversation(root, metadata, nil, nil, nil, conversations, nil, coordination.New())
		return &runtime{metadata: metadata, root: root, conversations: conversations, server: httptest.NewServer(handler)}
	}
	closeRuntime := func(current *runtime) {
		current.server.Close()
		current.conversations.Close()
		_ = current.root.Close()
		_ = current.metadata.Close()
	}
	current := startRuntime()
	t.Cleanup(func() { closeRuntime(current) })

	request := func(method, path, body string) (int, []byte) {
		t.Helper()
		var reader io.Reader
		if body != "" {
			reader = strings.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, current.server.URL+path, reader)
		if err != nil {
			t.Fatalf("create request: %v", err)
		}
		req.Header.Set("Origin", current.server.URL)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("send request: %v", err)
		}
		defer response.Body.Close()
		responseBody, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		return response.StatusCode, responseBody
	}
	decodeConversation := func(status int, body []byte, wantStatus int) conversationResponse {
		t.Helper()
		var conversation conversationResponse
		if status != wantStatus || json.Unmarshal(body, &conversation) != nil {
			t.Fatalf("conversation response = %d %s", status, body)
		}
		return conversation
	}

	status, body := request(http.MethodPost, "/api/v1/agent/conversations", `{"message":"甲会话的整理需求"}`)
	first := decodeConversation(status, body, http.StatusCreated)
	status, body = request(http.MethodPost, "/api/v1/agent/conversations", `{"message":"乙会话的归档需求"}`)
	second := decodeConversation(status, body, http.StatusCreated)
	if first.ID == "" || second.ID == "" || first.ID == second.ID || first.State != store.ConversationClarifying || second.State != store.ConversationClarifying {
		t.Fatalf("created sessions = (%q, %q), states (%q, %q)", first.ID, second.ID, first.State, second.State)
	}

	status, body = request(http.MethodPost, "/api/v1/agent/conversations/"+first.ID+"/messages", `{"message":"甲会话的补充说明"}`)
	first = decodeConversation(status, body, http.StatusOK)
	if len(first.Messages) != 4 || first.Messages[2].Content != "甲会话的补充说明" {
		t.Fatalf("continued first session messages = %#v", first.Messages)
	}
	modelMu.Lock()
	if len(modelUserMessages) != 3 || strings.Join(modelUserMessages[0], "|") != "甲会话的整理需求" ||
		strings.Join(modelUserMessages[1], "|") != "乙会话的归档需求" ||
		strings.Join(modelUserMessages[2], "|") != "甲会话的整理需求|甲会话的补充说明" {
		modelMu.Unlock()
		t.Fatalf("model received mixed session context: %#v", modelUserMessages)
	}
	modelMu.Unlock()

	closeRuntime(current)
	current = startRuntime()
	status, body = request(http.MethodGet, "/api/v1/agent/conversations?limit=20", "")
	var history conversationListResponse
	if status != http.StatusOK || json.Unmarshal(body, &history) != nil || len(history.Conversations) != 2 {
		t.Fatalf("conversation history after restart = %d %s", status, body)
	}
	for _, expected := range []struct {
		id       string
		messages []string
	}{{first.ID, []string{"甲会话的整理需求", "请补充具体范围。", "甲会话的补充说明", "请补充具体范围。"}}, {second.ID, []string{"乙会话的归档需求", "请补充具体范围。"}}} {
		status, body = request(http.MethodGet, "/api/v1/agent/conversations/"+expected.id, "")
		restored := decodeConversation(status, body, http.StatusOK)
		if restored.ID != expected.id || restored.State != store.ConversationClarifying || len(restored.Messages) != len(expected.messages) {
			t.Fatalf("restored conversation %q = %#v", expected.id, restored)
		}
		for index, content := range expected.messages {
			if restored.Messages[index].Content != content {
				t.Fatalf("restored conversation %q message %d = %q, want %q", expected.id, index, restored.Messages[index].Content, content)
			}
		}
	}
}
