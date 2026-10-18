package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"

	"filesage/internal/fsroot"
	"filesage/internal/store"
)

func TestTaskListIsSortedPaginatedFilteredAndOmitsRequestText(t *testing.T) {
	metadata, handler := newTaskHandler(t)
	secret := "PRIVATE_REQUEST_TEXT_AND_API_KEY=secret-value"
	for _, task := range []store.TaskRecord{
		{ID: "task-a", Kind: "upload", Status: store.TaskRunning, RequestText: secret, ScopePath: "incoming", CreatedAt: testTime(1), UpdatedAt: testTime(10)},
		{ID: "task-b", Kind: "agent", Status: store.TaskSucceeded, RequestText: secret, ScopePath: "photos", CreatedAt: testTime(2), UpdatedAt: testTime(20)},
		{ID: "task-c", Kind: "upload", Status: store.TaskFailed, RequestText: secret, ScopePath: "documents", CreatedAt: testTime(3), UpdatedAt: testTime(20)},
	} {
		if err := metadata.PutTask(context.Background(), task); err != nil {
			t.Fatal(err)
		}
	}

	response := performRequest(handler, http.MethodGet, "/api/v1/tasks?limit=2&offset=0", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("first page status = %d: %s", response.Code, response.Body.String())
	}
	responseBody := response.Body.String()
	var page taskListResponse
	if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	if len(page.Tasks) != 2 || page.Tasks[0].ID != "task-c" || page.Tasks[1].ID != "task-b" || !page.HasMore {
		t.Fatalf("first page = %#v", page)
	}
	if strings.Contains(responseBody, secret) || strings.Contains(responseBody, "requestText") || strings.Contains(responseBody, "modelProvider") {
		t.Fatalf("task response leaked private task fields: %s", responseBody)
	}

	response = performRequest(handler, http.MethodGet, "/api/v1/tasks?limit=2&offset=2", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("second page status = %d: %s", response.Code, response.Body.String())
	}
	if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	if len(page.Tasks) != 1 || page.Tasks[0].ID != "task-a" || page.HasMore {
		t.Fatalf("second page = %#v", page)
	}

	response = performRequest(handler, http.MethodGet, "/api/v1/tasks?kind=upload&status=failed", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("filtered page status = %d: %s", response.Code, response.Body.String())
	}
	if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	if len(page.Tasks) != 1 || page.Tasks[0].ID != "task-c" {
		t.Fatalf("filtered page = %#v", page)
	}
}

func TestTaskAuditIsPaginatedNewestFirstAndRedactsSensitiveText(t *testing.T) {
	metadata, handler := newTaskHandler(t)
	if err := metadata.PutTask(context.Background(), store.TaskRecord{
		ID: "audit-task", Kind: "agent", Status: store.TaskSucceeded, RequestText: "PRIVATE_PROMPT",
		CreatedAt: testTime(1), UpdatedAt: testTime(4),
	}); err != nil {
		t.Fatal(err)
	}
	for index, event := range []store.AuditRecord{
		{TaskID: "audit-task", EventType: "created", Actor: "user", Outcome: "accepted", Message: "PRIVATE_AUDIT_MESSAGE", CreatedAt: testTime(1)},
		{TaskID: "audit-task", EventType: "executed", Actor: "agent", Outcome: "succeeded", Message: "finished PRIVATE_PROMPT; api_key=supersecret; Bearer abcdefgh; sk-1234567890abcdefghijkl", SourcePath: "photos/a.jpg", TargetPath: "photos/2026/a.jpg", CreatedAt: testTime(2)},
	} {
		if _, err := metadata.AppendAudit(context.Background(), event); err != nil {
			t.Fatalf("append audit %d: %v", index, err)
		}
	}

	response := performRequest(handler, http.MethodGet, "/api/v1/tasks/audit-task/audit?limit=1", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("audit page status = %d: %s", response.Code, response.Body.String())
	}
	responseBody := response.Body.String()
	var page taskAuditResponse
	if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.Events[0].EventType != "executed" || !page.HasMore {
		t.Fatalf("first audit page = %#v", page)
	}
	if page.Events[0].SourcePath != "photos/a.jpg" || page.Events[0].TargetPath != "photos/2026/a.jpg" {
		t.Fatalf("audit path details = %#v", page.Events[0])
	}
	if !strings.Contains(responseBody, `"message":"finished [任务请求正文已隐藏]`) {
		t.Fatalf("audit result message is missing or request text was not redacted: %s", responseBody)
	}
	for _, secret := range []string{"PRIVATE_PROMPT", "supersecret", "abcdefgh", "sk-1234567890abcdefghijkl", "PRIVATE_AUDIT_MESSAGE"} {
		if strings.Contains(responseBody, secret) {
			t.Fatalf("audit response leaked %q: %s", secret, responseBody)
		}
	}

	response = performRequest(handler, http.MethodGet, "/api/v1/tasks/audit-task/audit?limit=1&offset=1", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("second audit page status = %d: %s", response.Code, response.Body.String())
	}
	if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.Events[0].EventType != "created" || page.HasMore {
		t.Fatalf("second audit page = %#v", page)
	}

	response = performRequest(handler, http.MethodGet, "/api/v1/tasks/missing/audit", nil)
	if response.Code != http.StatusNotFound {
		t.Fatalf("missing task audit status = %d, want 404: %s", response.Code, response.Body.String())
	}
}

func TestTaskAuditLabelsAreRedactedBoundedAndControlFree(t *testing.T) {
	metadata, handler := newTaskHandler(t)
	requestText := "PRIVATE_PROMPT\nBODY"
	if err := metadata.PutTask(context.Background(), store.TaskRecord{
		ID: "audit-label-task", Kind: "agent", Status: store.TaskFailed, RequestText: requestText,
		CreatedAt: testTime(1), UpdatedAt: testTime(2),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.AppendAudit(context.Background(), store.AuditRecord{
		TaskID:     "audit-label-task",
		EventType:  "upload.publication\n" + requestText + strings.Repeat("e", maxAuditLabelRunes),
		Actor:      "system\r" + requestText + strings.Repeat("a", maxAuditLabelRunes),
		Outcome:    "failed\t" + requestText + strings.Repeat("o", maxAuditLabelRunes),
		Message:    strings.Repeat("m", maxAuditMessageRunes+50) + "\x00",
		SourcePath: "unsafe\nname",
		TargetPath: strings.Repeat("a", 256),
		CreatedAt:  testTime(2),
	}); err != nil {
		t.Fatal(err)
	}

	response := performRequest(handler, http.MethodGet, "/api/v1/tasks/audit-label-task/audit", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("audit status = %d: %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if strings.Contains(body, requestText) || strings.Contains(body, "requestText") || strings.Contains(body, "modelProvider") || strings.Contains(body, "modelName") {
		t.Fatalf("audit response exposed task-private data: %s", body)
	}
	var page taskAuditResponse
	if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(page.Events))
	}
	event := page.Events[0]
	for label, value := range map[string]string{
		"eventType": event.EventType,
		"actor":     event.Actor,
		"outcome":   event.Outcome,
	} {
		if len([]rune(value)) > maxAuditLabelRunes {
			t.Errorf("%s length = %d, want at most %d", label, len([]rune(value)), maxAuditLabelRunes)
		}
		for _, character := range value {
			if unicode.IsControl(character) {
				t.Errorf("%s contains control rune %U", label, character)
			}
		}
		if !strings.Contains(value, "[任务请求正文已隐藏]") {
			t.Errorf("%s did not redact request text: %q", label, value)
		}
	}
	if len([]rune(event.Message)) > maxAuditMessageRunes {
		t.Errorf("message length = %d, want at most %d", len([]rune(event.Message)), maxAuditMessageRunes)
	}
	for _, character := range event.Message {
		if unicode.IsControl(character) {
			t.Errorf("message contains control rune %U", character)
		}
	}
	if event.SourcePath != "" || event.TargetPath != "" {
		t.Fatalf("unsafe audit paths were returned: source=%q target=%q", event.SourcePath, event.TargetPath)
	}
}

func TestTaskListRejectsInvalidPaginationAndFilters(t *testing.T) {
	_, handler := newTaskHandler(t)
	for _, target := range []string{
		"/api/v1/tasks?limit=101",
		"/api/v1/tasks?offset=-1",
		"/api/v1/tasks?limit=1&limit=2",
		"/api/v1/tasks?status=unknown",
		"/api/v1/tasks?kind=",
		"/api/v1/tasks?unexpected=value",
		"/api/v1/tasks/invalid%0Aid/audit",
		"/api/v1/tasks/missing/audit?limit=0",
	} {
		response := performRequest(handler, http.MethodGet, target, nil)
		if response.Code != http.StatusBadRequest {
			t.Errorf("%s status = %d, want 400: %s", target, response.Code, response.Body.String())
		}
	}
}

func TestPutTaskAndAuditRollsBackBothRecordsOnAuditFailure(t *testing.T) {
	metadata, _ := newTaskHandler(t)
	task := store.TaskRecord{
		ID: "atomic-task", Kind: "agent", Status: store.TaskQueued,
		CreatedAt: testTime(1), UpdatedAt: testTime(1),
	}
	audit := store.AuditRecord{
		TaskID: task.ID, PlanID: "missing-plan", EventType: "queued",
		Actor: "system", Outcome: "accepted", CreatedAt: testTime(1),
	}
	if err := metadata.PutTaskAndAudit(context.Background(), task, audit); err == nil {
		t.Fatal("expected audit foreign-key failure")
	}
	if _, err := metadata.Task(context.Background(), task.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("task remained after its audit insert failed: %v", err)
	}
}

func TestTaskStoreWithoutInjectionReturnsUnavailable(t *testing.T) {
	root, err := fsroot.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	handler := New(root, nil)
	for _, target := range []string{"/api/v1/tasks", "/api/v1/tasks/any/audit"} {
		response := performRequest(handler, http.MethodGet, target, nil)
		if response.Code != http.StatusServiceUnavailable {
			t.Errorf("%s status = %d, want 503", target, response.Code)
		}
	}
}

func newTaskHandler(t *testing.T) (*store.Store, http.Handler) {
	t.Helper()
	metadata, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "tasks.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = metadata.Close() })
	root, err := fsroot.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return metadata, NewWithTaskStore(root, metadata, nil, nil)
}

func testTime(second int) time.Time {
	return time.Date(2026, time.October, 15, 0, 0, second, 0, time.UTC)
}
