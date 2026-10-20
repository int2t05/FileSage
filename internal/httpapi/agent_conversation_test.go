// 本文件验证 Agent 对话接口的形状、分页和安全响应。
package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filesage/internal/agent"
	"filesage/internal/config"
	"filesage/internal/coordination"
	"filesage/internal/fsroot"
	"filesage/internal/store"
)

type conversationServiceStub struct {
	status       agent.ModelStatus
	summaries    []store.ConversationSummary
	listedLimit  int
	listedOffset int
	createCalls  int
	addCalls     int
	confirmCalls int
	confirmOCR   bool
	conversation store.AgentConversation
	err          error
}

type duplicateScanServiceStub struct {
	conversationServiceStub
	scan           store.DuplicateScan
	groups         store.DuplicateScanGroupPage
	files          store.DuplicateScanFilePage
	cancelCall     int
	cleanupPlan    store.PlanRecord
	cleanupRequest agent.DuplicateCleanupRequest
	cleanupCalls   int
}

func (stub *duplicateScanServiceStub) GetDuplicateScan(context.Context, string) (store.DuplicateScan, error) {
	return stub.scan, stub.err
}
func (stub *duplicateScanServiceStub) CancelDuplicateScan(context.Context, string) (store.DuplicateScan, error) {
	stub.cancelCall++
	return stub.scan, stub.err
}
func (stub *duplicateScanServiceStub) ListDuplicateGroups(context.Context, string, int, int) (store.DuplicateScanGroupPage, error) {
	return stub.groups, stub.err
}
func (stub *duplicateScanServiceStub) ListDuplicateFiles(context.Context, string, int64, int, int) (store.DuplicateScanFilePage, error) {
	return stub.files, stub.err
}
func (stub *duplicateScanServiceStub) CreateDuplicateCleanupPlan(_ context.Context, _ string, request agent.DuplicateCleanupRequest) (store.PlanRecord, error) {
	stub.cleanupCalls++
	stub.cleanupRequest = request
	return stub.cleanupPlan, stub.err
}

func (stub *conversationServiceStub) Status() agent.ModelStatus { return stub.status }
func (stub *conversationServiceStub) List(_ context.Context, limit, offset int) ([]store.ConversationSummary, bool, error) {
	stub.listedLimit, stub.listedOffset = limit, offset
	return stub.summaries, false, stub.err
}
func (stub *conversationServiceStub) Create(context.Context, string) (store.AgentConversation, error) {
	stub.createCalls++
	return stub.conversation, stub.err
}
func (stub *conversationServiceStub) Get(context.Context, string) (store.AgentConversation, error) {
	return stub.conversation, stub.err
}
func (stub *conversationServiceStub) AddMessage(context.Context, string, string) (store.AgentConversation, error) {
	stub.addCalls++
	return stub.conversation, stub.err
}
func (stub *conversationServiceStub) ConfirmAnalysis(_ context.Context, _ string, options ...bool) (store.AgentConversation, error) {
	stub.confirmCalls++
	stub.confirmOCR = len(options) == 1 && options[0]
	return stub.conversation, stub.err
}

func TestAgentConversationListAndStatusDoNotExposeMessagesOrCredentials(t *testing.T) {
	stub := &conversationServiceStub{
		status: agent.ModelStatus{Available: true, Provider: "ollama", Model: "qwen-local", Destination: "local"},
		summaries: []store.ConversationSummary{{
			ID: "conversation-1", State: store.ConversationAwaitingApproval,
			Title: "根据正文标题整理名称", TaskID: "task-1", PlanID: "plan-1", TaskStatus: store.TaskSucceeded, UpdatedAt: time.Date(2026, 10, 15, 8, 30, 0, 0, time.UTC),
		}},
	}
	handler := NewWithAgentAndIndexAndConversation(nil, nil, nil, nil, nil, stub, nil, nil)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/agent/conversations?limit=100&offset=20", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || stub.listedLimit != 100 || stub.listedOffset != 20 {
		t.Fatalf("list response=%d limit=%d offset=%d body=%s", response.Code, stub.listedLimit, stub.listedOffset, response.Body.String())
	}
	var list map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(list)
	if strings.Contains(string(encoded), "messages") || strings.Contains(string(encoded), "文件正文") || !strings.Contains(string(encoded), "taskId") || !strings.Contains(string(encoded), "planId") || !strings.Contains(string(encoded), `"taskStatus":"succeeded"`) {
		t.Fatalf("conversation summary has wrong shape: %s", encoded)
	}
	request = httptest.NewRequest(http.MethodGet, "/api/v1/agent/status", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "apiKey") || strings.Contains(response.Body.String(), "secret") {
		t.Fatalf("model status response = %d %s", response.Code, response.Body.String())
	}
}

func TestPersistedConversationFailuresRemainSuccessfulResponses(t *testing.T) {
	stub := &conversationServiceStub{
		status:       agent.ModelStatus{Reason: "not_configured"},
		conversation: store.AgentConversation{ID: "0123456789abcdef0123456789abcdef", State: store.ConversationFailed, Error: "provider_error"},
		err:          agent.ErrModelRequest,
	}
	handler := NewWithAgentAndIndexAndConversation(nil, nil, nil, nil, nil, stub, nil, nil)
	request := httptest.NewRequest(http.MethodPost, "http://filesage.local/api/v1/agent/conversations", strings.NewReader(`{"message":"rename report.md"}`))
	request.Header.Set("Origin", "http://filesage.local")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || !strings.Contains(response.Body.String(), `"state":"failed"`) || !strings.Contains(response.Body.String(), `"error":"provider_error"`) {
		t.Fatalf("create response = %d %s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodPost, "http://filesage.local/api/v1/agent/conversations/0123456789abcdef0123456789abcdef/messages", strings.NewReader(`{"message":"try again"}`))
	request.Header.Set("Origin", "http://filesage.local")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || stub.addCalls != 1 || !strings.Contains(response.Body.String(), `"state":"failed"`) {
		t.Fatalf("message response = %d calls=%d %s", response.Code, stub.addCalls, response.Body.String())
	}
}

func TestAgentConversationResponseKeepsContextSummaryPrivate(t *testing.T) {
	stub := &conversationServiceStub{conversation: store.AgentConversation{
		ID: "0123456789abcdef0123456789abcdef", State: store.ConversationClarifying,
		ContextSummary: "private-context-summary-sentinel",
		Messages:       []store.ConversationMessage{{Role: "assistant", Content: "请补充整理范围。"}},
	}}
	handler := NewWithAgentAndIndexAndConversation(nil, nil, nil, nil, nil, stub, nil, nil)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/agent/conversations/0123456789abcdef0123456789abcdef", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "private-context-summary-sentinel") || strings.Contains(response.Body.String(), "contextSummary") {
		t.Fatalf("conversation response exposed internal summary: %d %s", response.Code, response.Body.String())
	}
}

func TestCreateDirectoryConfirmationCardKeepsActionAndTargetPath(t *testing.T) {
	stub := &conversationServiceStub{
		status: agent.ModelStatus{Available: true, Provider: "ollama", Model: "qwen-local", Destination: "local"},
		conversation: store.AgentConversation{
			ID: "0123456789abcdef0123456789abcdef", State: store.ConversationAwaitingConfirmation,
			Messages: []store.ConversationMessage{{Role: "assistant", Content: "请确认新建目录。"}},
			Analysis: &store.AnalysisCard{
				Action: "create_directory", Goal: "按年份整理照片", TargetPath: "照片/2025",
				DataTypes: []string{"任务描述与目标路径"}, Provider: "ollama", Model: "qwen-local", Destination: "local",
			},
		},
	}
	handler := NewWithAgentAndIndexAndConversation(nil, nil, nil, nil, nil, stub, nil, nil)
	request := httptest.NewRequest(http.MethodPost, "http://filesage.local/api/v1/agent/conversations", strings.NewReader(`{"message":"创建照片归档目录"}`))
	request.Header.Set("Origin", "http://filesage.local")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var result conversationResponse
	if response.Code != http.StatusCreated || json.Unmarshal(response.Body.Bytes(), &result) != nil {
		t.Fatalf("create conversation response = %d %s", response.Code, response.Body.String())
	}
	if result.AnalysisCard == nil || result.AnalysisCard.Action != "create_directory" || result.AnalysisCard.TargetPath != "照片/2025" || result.AnalysisCard.FilePath != "" {
		t.Fatalf("analysis card response = %#v", result.AnalysisCard)
	}
}

func TestProviderErrorsAndSecretsAreNotReturnedToClient(t *testing.T) {
	const secret = "api-key-never-returned"
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+secret {
			t.Errorf("authorization header = %q", got)
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("provider-raw-secret: " + secret))
	}))
	defer provider.Close()

	ctx := context.Background()
	root, err := fsroot.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	dataDir := t.TempDir()
	metadata, err := store.Open(ctx, filepath.Join(dataDir, "filesage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	plans := agent.New(metadata, root, coordination.New())
	conversationService := agent.NewConversationService(metadata, root, plans, config.AgentModelConfig{
		BaseURL: provider.URL, Model: "test-model", Provider: "test-provider", APIKey: secret,
	})
	handler := NewWithAgentAndIndexAndConversation(root, metadata, nil, plans, nil, conversationService, nil, coordination.New())
	request := httptest.NewRequest(http.MethodPost, "http://filesage.local/api/v1/agent/conversations", strings.NewReader(`{"message":"请帮我整理 notes.md"}`))
	request.Header.Set("Origin", "http://filesage.local")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || !strings.Contains(response.Body.String(), `"state":"failed"`) {
		t.Fatalf("conversation response = %d %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), secret) || strings.Contains(response.Body.String(), "provider-raw-secret") {
		t.Fatalf("provider response or key leaked: %s", response.Body.String())
	}
	request = httptest.NewRequest(http.MethodGet, "http://filesage.local/api/v1/agent/status", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if strings.Contains(response.Body.String(), secret) || strings.Contains(response.Body.String(), provider.URL) {
		t.Fatalf("status exposed model configuration: %s", response.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dataDir, "filesage.sqlite")); err != nil {
		t.Fatalf("conversation persistence was not created: %v", err)
	}
}

func TestConversationWritesRequireSameOriginAndEmptyConfirmBody(t *testing.T) {
	stub := &conversationServiceStub{
		conversation: store.AgentConversation{ID: "0123456789abcdef0123456789abcdef", State: store.ConversationAwaitingApproval},
	}
	handler := NewWithAgentAndIndexAndConversation(nil, nil, nil, nil, nil, stub, nil, nil)
	request := httptest.NewRequest(http.MethodPost, "http://filesage.local/api/v1/agent/conversations", strings.NewReader(`{"message":"rename"}`))
	request.Header.Set("Origin", "http://evil.local")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || stub.createCalls != 0 {
		t.Fatalf("cross-origin create = status %d, calls %d", response.Code, stub.createCalls)
	}
	request = httptest.NewRequest(http.MethodPost, "http://filesage.local/api/v1/agent/conversations/0123456789abcdef0123456789abcdef/confirm-analysis", strings.NewReader(`{}`))
	request.Header.Set("Origin", "http://filesage.local")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || stub.confirmCalls != 1 || stub.confirmOCR {
		t.Fatalf("confirm without OCR = status %d, calls %d, OCR %t", response.Code, stub.confirmCalls, stub.confirmOCR)
	}
	request = httptest.NewRequest(http.MethodPost, "http://filesage.local/api/v1/agent/conversations/0123456789abcdef0123456789abcdef/confirm-analysis", strings.NewReader(`{"enableOCR":true}`))
	request.Header.Set("Origin", "http://filesage.local")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || stub.confirmCalls != 2 || !stub.confirmOCR {
		t.Fatalf("confirm with OCR = status %d, calls %d, OCR %t", response.Code, stub.confirmCalls, stub.confirmOCR)
	}
}

func TestDuplicateScanEndpointsHideDigestsAndValidateCancellation(t *testing.T) {
	stub := &duplicateScanServiceStub{
		scan: store.DuplicateScan{
			ID: "0123456789abcdef0123456789abcdef", Status: store.DuplicateScanSucceeded,
			Phase: "complete", ScopePath: "docs", GroupCount: 1, DuplicateFileCount: 2,
			ReclaimableBytes: 7, UpdatedAt: time.Date(2026, 10, 16, 5, 0, 0, 0, time.UTC),
		},
		groups:      store.DuplicateScanGroupPage{Groups: []store.DuplicateScanGroup{{ID: 1, SizeBytes: 7, DuplicateCount: 2}}, Limit: 20},
		files:       store.DuplicateScanFilePage{Files: []store.DuplicateScanFile{{Path: "docs/a.txt", SizeBytes: 7, ModifiedAt: time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)}}, Limit: 100},
		cleanupPlan: store.PlanRecord{TaskID: "task-cleanup"},
	}
	handler := NewWithAgentAndIndexAndConversation(nil, nil, nil, nil, nil, stub, nil, nil)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/agent/duplicate-scans/0123456789abcdef0123456789abcdef", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"errorCode":""`) || strings.Contains(response.Body.String(), "sha256") {
		t.Fatalf("duplicate scan status = %d %s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodGet, "/api/v1/agent/duplicate-scans/0123456789abcdef0123456789abcdef/groups?limit=1", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"id":"1"`) || strings.Contains(response.Body.String(), "SHA") {
		t.Fatalf("duplicate groups = %d %s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodGet, "/api/v1/agent/duplicate-scans/0123456789abcdef0123456789abcdef/groups/1/files", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"path":"docs/a.txt"`) || strings.Contains(response.Body.String(), "digest") {
		t.Fatalf("duplicate files = %d %s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodPost, "http://filesage.local/api/v1/agent/duplicate-scans/0123456789abcdef0123456789abcdef/cancel", nil)
	request.Header.Set("Origin", "http://evil.local")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || stub.cancelCall != 0 {
		t.Fatalf("cross-origin cancel = %d, calls=%d", response.Code, stub.cancelCall)
	}
	request = httptest.NewRequest(http.MethodPost, "http://filesage.local/api/v1/agent/duplicate-scans/0123456789abcdef0123456789abcdef/cancel", strings.NewReader(`{}`))
	request.Header.Set("Origin", "http://filesage.local")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || stub.cancelCall != 0 {
		t.Fatalf("cancel with body = %d, calls=%d", response.Code, stub.cancelCall)
	}
	request = httptest.NewRequest(http.MethodPost, "http://filesage.local/api/v1/agent/duplicate-scans/0123456789abcdef0123456789abcdef/cancel", nil)
	request.Header.Set("Origin", "http://filesage.local")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || stub.cancelCall != 1 {
		t.Fatalf("valid cancel = %d, calls=%d, body=%s", response.Code, stub.cancelCall, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodPost, "http://filesage.local/api/v1/agent/duplicate-scans/0123456789abcdef0123456789abcdef/cleanup-plan", strings.NewReader(`{"groupId":"1","keeperPath":"docs/a.txt","trashPaths":["docs/b.txt"]}`))
	request.Header.Set("Origin", "http://filesage.local")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || !strings.Contains(response.Body.String(), `"taskId":"task-cleanup"`) ||
		stub.cleanupCalls != 1 || stub.cleanupRequest.KeeperPath != "docs/a.txt" || len(stub.cleanupRequest.TrashPaths) != 1 {
		t.Fatalf("create cleanup plan = %d, calls=%d, body=%s", response.Code, stub.cleanupCalls, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodPost, "http://filesage.local/api/v1/agent/duplicate-scans/0123456789abcdef0123456789abcdef/cleanup-plan", strings.NewReader(`{"groupId":"1","keeperPath":"docs/a.txt","trashPaths":["docs/b.txt"]}`))
	request.Header.Set("Origin", "http://evil.local")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || stub.cleanupCalls != 1 {
		t.Fatalf("cross-origin cleanup = %d, calls=%d", response.Code, stub.cleanupCalls)
	}
}
