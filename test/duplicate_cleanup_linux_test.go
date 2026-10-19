// 本文件从 HTTP 接口验证重复清理、审批、回收与恢复闭环。
package test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"filesage/internal/agent"
	"filesage/internal/config"
	"filesage/internal/coordination"
	"filesage/internal/fsroot"
	"filesage/internal/httpapi"
	"filesage/internal/store"
	trashsvc "filesage/internal/trash"
)

func TestDuplicateCleanupHTTPFlowRequiresReviewAndCanRestore(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	rootPath := filepath.Join(base, "managed")
	if err := os.MkdirAll(filepath.Join(rootPath, "docs"), 0o700); err != nil {
		t.Fatal(err)
	}
	content := []byte("identical duplicate document")
	for _, name := range []string{"keep.md", "copy-a.md", "copy-b.md", "copy-c.md"} {
		if err := os.WriteFile(filepath.Join(rootPath, "docs", name), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	metadata, err := store.Open(ctx, filepath.Join(base, "state", "filesage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	trashManager, err := trashsvc.Open(ctx, root, metadata)
	if err != nil {
		t.Fatal(err)
	}
	locks := coordination.New()
	plans := agent.New(metadata, root, locks, trashManager)
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		content := `{"state":"ready","reply":"我会检查 docs 中的重复文件。","goal":"查找 docs 中的重复文件","action":"find_duplicates","filePath":"docs"}`
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": content}}}})
	}))
	defer modelServer.Close()
	conversations := agent.NewConversationService(metadata, root, plans, config.AgentModelConfig{
		BaseURL: modelServer.URL, Model: "test", Provider: "http-e2e",
	})
	defer conversations.Close()
	handler := httpapi.NewWithAgentAndIndexAndConversation(root, metadata, trashManager, plans, nil, conversations, nil, locks)
	server := httptest.NewServer(handler)
	defer server.Close()

	var created struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/v1/agent/conversations", server.URL, map[string]string{"message": "查找 docs 中的重复文件"}, http.StatusCreated, &created)
	if created.State != string(store.ConversationAwaitingConfirmation) || created.ID == "" {
		t.Fatalf("conversation = (%s, %s), want confirmation", created.ID, created.State)
	}

	var conversation struct {
		DuplicateScanID string `json:"duplicateScanId"`
	}
	requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/v1/agent/conversations/"+created.ID+"/confirm-analysis", server.URL, map[string]bool{"enableOCR": false}, http.StatusOK, &conversation)
	if conversation.DuplicateScanID == "" {
		t.Fatal("confirmed scan did not return an identifier")
	}

	var scan struct {
		Status string `json:"status"`
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		requestJSON(t, server.Client(), http.MethodGet, server.URL+"/api/v1/agent/duplicate-scans/"+conversation.DuplicateScanID, "", nil, http.StatusOK, &scan)
		if scan.Status == string(store.DuplicateScanSucceeded) {
			break
		}
		if scan.Status == string(store.DuplicateScanFailed) || time.Now().After(deadline) {
			t.Fatalf("duplicate scan status = %q", scan.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}

	var createdTask struct {
		TaskID string `json:"taskId"`
	}
	requestJSON(t, server.Client(), http.MethodPost,
		server.URL+"/api/v1/agent/duplicate-scans/"+conversation.DuplicateScanID+"/cleanup-plan", server.URL,
		map[string]any{"groupId": "1", "keeperPath": "docs/keep.md", "trashPaths": []string{"docs/copy-a.md", "docs/copy-b.md"}},
		http.StatusCreated, &createdTask)

	type planResponse struct {
		TaskID      string `json:"taskId"`
		PlanID      string `json:"planId"`
		PlanVersion int    `json:"planVersion"`
		Status      string `json:"status"`
		Operations  []struct {
			ID         string `json:"id"`
			Action     string `json:"action"`
			SourcePath string `json:"sourcePath"`
			TargetPath string `json:"targetPath"`
			Approval   string `json:"approval"`
			ExecStatus string `json:"executionStatus"`
		} `json:"operations"`
	}
	var plan planResponse
	requestJSON(t, server.Client(), http.MethodGet, server.URL+"/api/v1/agent/tasks/"+createdTask.TaskID, "", nil, http.StatusOK, &plan)
	if plan.Status != string(store.PlanAwaitingApproval) || len(plan.Operations) != 2 {
		t.Fatalf("cleanup plan = (%s, %d operations)", plan.Status, len(plan.Operations))
	}
	for _, operation := range plan.Operations {
		if operation.Action != "trash_duplicate" || operation.TargetPath != "docs/keep.md" || operation.Approval != string(store.ApprovalPending) {
			t.Fatalf("unclear cleanup operation: %+v", operation)
		}
	}
	if _, err := os.Stat(filepath.Join(rootPath, "docs", "copy-a.md")); err != nil {
		t.Fatalf("approval moved a file before execution: %v", err)
	}
	requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/v1/agent/plans/"+plan.PlanID+"/start", server.URL,
		map[string]any{"expectedPlanVersion": plan.PlanVersion, "expectedExecutionMode": "bypass"}, http.StatusOK, &plan)
	if plan.Status != string(store.PlanCompleted) {
		t.Fatalf("review and execution status = %q", plan.Status)
	}
	for _, operation := range plan.Operations {
		if operation.Approval != string(store.ApprovalApproved) || operation.ExecStatus != string(store.OperationSucceeded) {
			t.Fatalf("reviewed execution result = %+v", operation)
		}
	}
	for _, name := range []string{"copy-a.md", "copy-b.md"} {
		if _, err := os.Stat(filepath.Join(rootPath, "docs", name)); !os.IsNotExist(err) {
			t.Fatalf("selected copy %s remains, err=%v", name, err)
		}
	}
	for _, name := range []string{"keep.md", "copy-c.md"} {
		if _, err := os.Stat(filepath.Join(rootPath, "docs", name)); err != nil {
			t.Fatalf("keeper or unselected copy %s changed: %v", name, err)
		}
	}

	var trashPage struct {
		Items []struct {
			ID           string `json:"id"`
			OriginalPath string `json:"originalPath"`
			Status       string `json:"status"`
		} `json:"items"`
	}
	requestJSON(t, server.Client(), http.MethodGet, server.URL+"/api/v1/trash?limit=100", "", nil, http.StatusOK, &trashPage)
	if len(trashPage.Items) != 2 {
		t.Fatalf("trash contains %d items, want 2", len(trashPage.Items))
	}
	for _, item := range trashPage.Items {
		if item.Status != string(store.TrashTrashed) || item.OriginalPath != "docs/copy-a.md" && item.OriginalPath != "docs/copy-b.md" {
			t.Fatalf("unexpected trash item: %+v", item)
		}
	}
	var restored struct {
		Path string `json:"path"`
	}
	requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/v1/trash/"+trashPage.Items[0].ID+"/restore", server.URL, nil, http.StatusOK, &restored)
	if _, err := os.Stat(filepath.Join(rootPath, filepath.FromSlash(restored.Path))); err != nil {
		t.Fatalf("restored copy did not return: %v", err)
	}

	var audit struct {
		Events []struct {
			EventType string `json:"eventType"`
		} `json:"events"`
	}
	requestJSON(t, server.Client(), http.MethodGet, server.URL+"/api/v1/tasks/"+createdTask.TaskID+"/audit?limit=100", "", nil, http.StatusOK, &audit)
	if len(audit.Events) < 7 {
		t.Fatalf("task audit has %d events, want plan, approvals, and execution", len(audit.Events))
	}
}

func requestJSON(t *testing.T, client *http.Client, method, url, origin string, body any, status int, result any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != status {
		payload, _ := io.ReadAll(response.Body)
		t.Fatalf("%s %s returned %d, want %d: %s", method, url, response.StatusCode, status, payload)
	}
	if result != nil {
		if err := json.NewDecoder(response.Body).Decode(result); err != nil {
			t.Fatalf("decode %s response: %v", url, err)
		}
	}
}
