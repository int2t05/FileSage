// 本文件验证统一操作历史的分页、脱敏和错误边界。
package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"filesage/internal/fsroot"
	"filesage/internal/store"
)

func TestAuditHistoryReturnsSanitizedKeysetPages(t *testing.T) {
	ctx := context.Background()
	metadata, err := store.Open(ctx, filepath.Join(t.TempDir(), "filesage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()

	var ids []int64
	for _, name := range []string{"one.txt", "two.txt", "three.txt"} {
		id, err := metadata.BeginManualAudit(ctx, store.AuditRecord{
			EventType: "manual_rename", SourcePath: "inbox/" + name,
			TargetPath: "inbox/renamed-" + name, Message: "private Bearer abc.def",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := metadata.FinishManualAudit(ctx, id, "succeeded", "private Bearer abc.def"); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}

	handler := NewWithTaskStore(nil, metadata, nil, nil)
	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/api/v1/audit?limit=2", nil))
	if first.Code != http.StatusOK {
		t.Fatalf("first page status = %d, body = %s", first.Code, first.Body.String())
	}
	var page auditHistoryResponse
	if err := json.Unmarshal(first.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 2 || !page.HasMore || page.NextCursor != ids[1] {
		t.Fatalf("unexpected first page: %+v", page)
	}
	if page.Events[0].ID != ids[2] || page.Events[1].ID != ids[1] {
		t.Fatalf("events are not newest first: %+v", page.Events)
	}
	var raw map[string]any
	if err := json.Unmarshal(first.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	firstEvent := raw["events"].([]any)[0].(map[string]any)
	if _, ok := firstEvent["message"]; ok {
		t.Fatal("response exposed an audit message")
	}
	if firstEvent["actor"] != "user" || firstEvent["outcome"] != "succeeded" {
		t.Fatalf("unexpected public labels: %+v", firstEvent)
	}

	second := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/audit?limit=2&before="+strconv.FormatInt(page.NextCursor, 10), nil)
	handler.ServeHTTP(second, request)
	if second.Code != http.StatusOK {
		t.Fatalf("second page status = %d, body = %s", second.Code, second.Body.String())
	}
	var next auditHistoryResponse
	if err := json.Unmarshal(second.Body.Bytes(), &next); err != nil {
		t.Fatal(err)
	}
	if len(next.Events) != 1 || next.HasMore || next.Events[0].ID != ids[0] {
		t.Fatalf("unexpected second page: %+v", next)
	}
}

func TestAuditHistoryRejectsInvalidQueries(t *testing.T) {
	metadata, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "filesage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	handler := NewWithTaskStore(nil, metadata, nil, nil)

	for _, query := range []string{
		"?limit=0", "?limit=101", "?before=0", "?before=-2",
		"?before=1&before=2", "?unknown=1",
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/audit"+query, nil))
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("query %q status = %d, want %d", query, recorder.Code, http.StatusBadRequest)
		}
	}
}

func TestAuditHistoryRequiresRepository(t *testing.T) {
	handler := NewWithLocker(nil, nil, nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/audit", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
}

func TestManualDirectoryAndRenameOperationsAppearInHistory(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "Photos"), 0o755); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "linux" {
		if err := os.WriteFile(filepath.Join(rootPath, "draft.txt"), []byte("draft"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	metadata, err := store.Open(ctx, filepath.Join(t.TempDir(), "filesage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	handler := NewWithTaskStore(root, metadata, nil, nil)

	created := performJSONRequest(handler, http.MethodPost, "/api/v1/files/directories", `{"parent":"","name":"Photos"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create directory status = %d: %s", created.Code, created.Body.String())
	}
	if runtime.GOOS == "linux" {
		version := versionFromListing(t, handler, "draft.txt")
		renamed := performJSONRequest(handler, http.MethodPost, "/api/v1/files/rename", `{"path":"draft.txt","name":"final.txt","version":"`+version+`"}`)
		if renamed.Code != http.StatusOK {
			t.Fatalf("rename status = %d: %s", renamed.Code, renamed.Body.String())
		}
	}

	history := httptest.NewRecorder()
	handler.ServeHTTP(history, httptest.NewRequest(http.MethodGet, "/api/v1/audit", nil))
	if history.Code != http.StatusOK {
		t.Fatalf("history status = %d: %s", history.Code, history.Body.String())
	}
	var page auditHistoryResponse
	if err := json.Unmarshal(history.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	wantEvents := 1
	if runtime.GOOS == "linux" {
		wantEvents = 2
	}
	if len(page.Events) != wantEvents {
		t.Fatalf("history has %d events, want %d: %+v", len(page.Events), wantEvents, page.Events)
	}
	if runtime.GOOS == "linux" {
		if got := page.Events[0]; got.EventType != "manual_rename" || got.SourcePath != "draft.txt" || got.TargetPath != "final.txt" || got.Outcome != "succeeded" {
			t.Fatalf("unexpected rename audit: %+v", got)
		}
	}
	createIndex := 0
	if runtime.GOOS == "linux" {
		createIndex = 1
	}
	if got := page.Events[createIndex]; got.EventType != "manual_create_directory" || got.TargetPath != "Photos (2)" || got.Outcome != "succeeded" {
		t.Fatalf("unexpected directory audit: %+v", got)
	}
}
