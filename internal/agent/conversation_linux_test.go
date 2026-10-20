//go:build linux

// 本文件验证 Agent 分析确认、快照绑定和审批闭环。
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"filesage/internal/coordination"
	"filesage/internal/fsroot"
	"filesage/internal/store"
	trashsvc "filesage/internal/trash"
)

type scriptedConversationModel struct {
	mu       sync.Mutex
	status   ModelStatus
	replies  []string
	requests [][]ChatMessage
	entered  chan struct{}
	release  chan struct{}
}

func (m *scriptedConversationModel) Status() ModelStatus { return m.status }

func (m *scriptedConversationModel) complete(ctx context.Context, messages []ChatMessage) (string, error) {
	m.mu.Lock()
	call := len(m.requests)
	copyMessages := append([]ChatMessage(nil), messages...)
	m.requests = append(m.requests, copyMessages)
	if call >= len(m.replies) {
		m.mu.Unlock()
		return "", ErrModelResponse
	}
	reply := m.replies[call]
	entered, release := m.entered, m.release
	m.mu.Unlock()
	if entered != nil && call == 1 {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return "", ErrModelRequest
		}
	}
	return reply, nil
}

func (m *scriptedConversationModel) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.requests)
}

func TestConversationRetriesInvalidModelResponseOnce(t *testing.T) {
	invalid := `{"state":"ready","reply":"","goal":"建立并列目录","operations":[{"action":"create_directory","filePath":"","targetPath":"qa-a"},{"action":"create_directory","filePath":"","targetPath":"qa-b"}]}`
	valid := `{"state":"ready","reply":"我会创建两个并列文件夹，请确认。","goal":"建立并列目录","operations":[{"action":"create_directory","filePath":"","targetPath":"qa-a"},{"action":"create_directory","filePath":"","targetPath":"qa-b"}]}`
	for _, test := range []struct {
		name      string
		replies   []string
		wantState store.ConversationState
		wantOps   int
	}{
		{name: "修正后接受", replies: []string{invalid, valid}, wantState: store.ConversationAwaitingConfirmation, wantOps: 2},
		{name: "重试次数有界", replies: []string{invalid, invalid}, wantState: store.ConversationFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			rootPath := t.TempDir()
			metadata, root, plans := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
			defer root.Close()
			defer func() { _ = metadata.Close() }()
			model := &scriptedConversationModel{
				status:  ModelStatus{Available: true, Provider: "test-provider", Model: "test-model", Destination: "local"},
				replies: test.replies,
			}
			service := &ConversationService{store: metadata, root: root, plans: plans, model: model}
			conversation, err := service.Create(ctx, "在管理根目录建立两个并列目录")
			operationCount := 0
			if conversation.Analysis != nil {
				operationCount = len(conversation.Analysis.Operations)
			}
			errValid := err == nil && test.wantState == store.ConversationAwaitingConfirmation || errors.Is(err, ErrModelResponse) && test.wantState == store.ConversationFailed
			if !errValid || conversation.State != test.wantState || operationCount != test.wantOps || model.callCount() != 2 {
				t.Fatalf("Create() state=%q operations=%d calls=%d err=%v", conversation.State, operationCount, model.callCount(), err)
			}
			model.mu.Lock()
			retryMessages := model.requests[1]
			model.mu.Unlock()
			last := retryMessages[len(retryMessages)-1]
			if last.Role != "system" || !strings.Contains(last.Content, "可见工具回执") || !strings.Contains(last.Content, "不得改变用户目标") {
				t.Fatalf("retry did not include a bounded correction prompt: %#v", last)
			}
		})
	}
}

func TestConversationSummarizesCompletedToolReceiptWithoutCreatingAnotherPlan(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	metadata, root, plans := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()

	model := &scriptedConversationModel{
		status: ModelStatus{Available: true, Provider: "test-provider", Model: "test-model", Destination: "local"},
		replies: []string{
			`{"state":"ready","reply":"我会建立合同文件夹，请确认。","contextSummary":"","goal":"建立合同文件夹","operations":[{"action":"create_directory","filePath":"","targetPath":"合同"}]}`,
			`{"state":"clarifying","reply":"上一计划已完成，已创建“合同”文件夹。","contextSummary":"用户已确认创建合同文件夹，计划已完成。"}`,
		},
	}
	conversationService := &ConversationService{store: metadata, root: root, plans: plans, model: model}
	conversation, err := conversationService.Create(ctx, "创建合同文件夹")
	if err != nil || conversation.State != store.ConversationAwaitingConfirmation || conversation.Analysis == nil {
		t.Fatalf("Create() = (%#v, %v)", conversation, err)
	}
	conversation, err = conversationService.ConfirmAnalysis(ctx, conversation.ID)
	if err != nil || conversation.State != store.ConversationAwaitingApproval || conversation.PlanID == "" {
		t.Fatalf("ConfirmAnalysis() = (%#v, %v)", conversation, err)
	}
	plan, err := plans.Plan(ctx, conversation.PlanID)
	if err != nil || len(plan.Operations) != 1 {
		t.Fatalf("Plan() = (%#v, %v)", plan, err)
	}
	approved, err := plans.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve", plan.Version)
	if err != nil {
		t.Fatalf("Decide() = %v", err)
	}
	completed, err := plans.Execute(ctx, plan.ID, approved.Version)
	if err != nil || completed.Status != store.PlanCompleted || completed.Operations[0].Status != store.OperationSucceeded {
		t.Fatalf("Execute() = (%#v, %v)", completed, err)
	}

	conversation, err = conversationService.AddMessage(ctx, conversation.ID, "只总结上一轮结果，不要继续操作")
	if err != nil || conversation.State != store.ConversationClarifying || conversation.PlanID != "" || conversation.Analysis != nil {
		t.Fatalf("AddMessage() created a plan or failed to answer: (%#v, %v)", conversation, err)
	}
	model.mu.Lock()
	followupMessages := append([]ChatMessage(nil), model.requests[1]...)
	callCount := len(model.requests)
	model.mu.Unlock()
	hasReceipt, hasUserRequest := false, false
	for _, message := range followupMessages {
		if message.Role == "assistant" && strings.Contains(message.Content, "上一计划已完成") && strings.Contains(message.Content, "合同") && strings.Contains(message.Content, "目录已创建") {
			hasReceipt = true
		}
		if message.Role == "user" && message.Content == "只总结上一轮结果，不要继续操作" {
			hasUserRequest = true
		}
	}
	if !hasReceipt || !hasUserRequest {
		t.Fatalf("follow-up omitted the persisted tool receipt or user request: receipt=%t request=%t", hasReceipt, hasUserRequest)
	}
	if callCount != 2 || !strings.Contains(conversation.Messages[len(conversation.Messages)-1].Content, "已创建“合同”文件夹") {
		t.Fatalf("reply-only turn was not completed cleanly: calls=%d messages=%#v", callCount, conversation.Messages)
	}
	tasks, err := metadata.ListTasks(ctx, store.TaskListQuery{Limit: 10})
	if err != nil || len(tasks.Tasks) != 1 {
		t.Fatalf("follow-up created another task: tasks=%#v err=%v", tasks.Tasks, err)
	}
}

func TestConfirmedAnalysisPersistsBoundPlanAndReusesApprovalFlow(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	body := "# Private sentinel 7412\nMeeting record for quarterly review.\n"
	if err := os.WriteFile(filepath.Join(rootPath, "notes.md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(t.TempDir(), "filesage.sqlite")
	metadata, root, plans := openAgentTestServices(t, databasePath, rootPath)
	defer root.Close()
	defer func() {
		if metadata != nil {
			_ = metadata.Close()
		}
	}()
	model := &scriptedConversationModel{
		status: ModelStatus{Available: true, Provider: "test-provider", Model: "test-model", Destination: "local"},
		replies: []string{
			`{"state":"ready","reply":"我将读取 notes.md 全文并发送给 test-model，请确认。","goal":"根据正文标题生成文件名","action":"rename","filePath":"notes.md","targetPath":""}`,
			`{"supported":true,"targetName":"季度会议纪要.md","reasonCode":"topic"}`,
		},
	}
	conversationService := &ConversationService{store: metadata, root: root, plans: plans, model: model}
	conversation, err := conversationService.Create(ctx, "请依据 notes.md 正文重命名")
	if err != nil || conversation.State != store.ConversationAwaitingConfirmation {
		t.Fatalf("Create() = (%#v, %v)", conversation, err)
	}
	if len(conversation.Messages) != 2 || strings.Contains(conversation.Messages[0].Content, "Private sentinel") || conversation.Analysis == nil {
		t.Fatalf("unexpected pre-confirmation conversation: %#v", conversation)
	}
	model.mu.Lock()
	preconfirmRequest := model.requests[0]
	model.mu.Unlock()
	if strings.Contains(marshalMessages(t, preconfirmRequest), "Private sentinel") {
		t.Fatal("file body reached model before confirmation")
	}
	before, err := root.LstatEntry("notes.md")
	if err != nil {
		t.Fatal(err)
	}
	conversation, err = conversationService.ConfirmAnalysis(ctx, conversation.ID)
	if err != nil || conversation.State != store.ConversationAwaitingApproval || conversation.PlanID == "" {
		t.Fatalf("ConfirmAnalysis() = (%#v, %v)", conversation, err)
	}
	model.mu.Lock()
	analysisRequest := model.requests[1]
	model.mu.Unlock()
	if !strings.Contains(marshalMessages(t, analysisRequest), "Private sentinel") {
		t.Fatal("confirmed file body did not reach model")
	}
	plan, err := plans.Plan(ctx, conversation.PlanID)
	if err != nil || len(plan.Operations) != 1 {
		t.Fatalf("Plan() = (%#v, %v)", plan, err)
	}
	if operation := plan.Operations[0]; operation.Reason != "根据正文主题生成建议名称" || operation.Evidence != "" {
		t.Fatalf("plan retained free-form model text: reason=%q evidence=%q", operation.Reason, operation.Evidence)
	}
	audit, err := metadata.AuditForTaskPage(ctx, plan.TaskID, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	storedRecords, err := json.Marshal(struct {
		Conversation store.AgentConversation
		Plan         store.PlanRecord
		Audit        store.TaskAuditPage
	}{conversation, plan, audit})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(storedRecords), "Private sentinel 7412") || strings.Contains(string(storedRecords), "Meeting record for quarterly review") {
		t.Fatal("file body appeared in the persisted conversation, plan, or audit")
	}
	var savedFingerprint fsroot.Fingerprint
	if err := json.Unmarshal([]byte(plan.Operations[0].SourceStateJSON), &savedFingerprint); err != nil {
		t.Fatal(err)
	}
	if !sameFingerprint(savedFingerprint, before.Fingerprint()) {
		t.Fatalf("plan did not retain the pre-analysis fingerprint: %#v != %#v", savedFingerprint, before.Fingerprint())
	}
	callCount := model.callCount()
	repeated, err := conversationService.ConfirmAnalysis(ctx, conversation.ID)
	if err != nil || repeated.PlanID != conversation.PlanID || model.callCount() != callCount {
		t.Fatalf("repeated confirmation = (%#v, %v), model calls=%d", repeated, err, model.callCount())
	}
	tasks, err := metadata.ListTasks(ctx, store.TaskListQuery{Limit: 10})
	if err != nil || len(tasks.Tasks) != 1 {
		t.Fatalf("ListTasks() = (%#v, %v), want exactly one task", tasks, err)
	}
	if _, err := plans.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve"); err != nil {
		t.Fatal(err)
	}
	if _, err := plans.Execute(ctx, plan.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(rootPath, "notes.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source still exists after approved execution: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rootPath, "季度会议纪要.md")); err != nil {
		t.Fatalf("renamed file missing: %v", err)
	}
	if err := metadata.Close(); err != nil {
		t.Fatal(err)
	}
	metadata = nil
	databaseBytes, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(databaseBytes), "Private sentinel 7412") || strings.Contains(string(databaseBytes), "Meeting record for quarterly review") {
		t.Fatal("file body appeared in SQLite after closing the store")
	}
	reopened, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restored, err := reopened.Conversation(ctx, conversation.ID)
	if err != nil || restored.State != store.ConversationAwaitingApproval || restored.PlanID != conversation.PlanID || len(restored.Messages) != 3 {
		t.Fatalf("restored conversation = (%#v, %v)", restored, err)
	}
}

func TestConversationSupportsOrderedOperationsWithoutReadingContent(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "parent"), 0o755); err != nil {
		t.Fatal(err)
	}
	metadata, root, plans := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()
	model := &scriptedConversationModel{
		status: ModelStatus{Available: true, Provider: "test-provider", Model: "test-model", Destination: "local"},
		replies: []string{`{"state":"ready","reply":"请确认按顺序建立两个目录。","goal":"整理目录结构","operations":[
			{"action":"create_directory","targetPath":"parent/one"},
			{"action":"create_directory","targetPath":"parent/two"}]}`},
	}
	service := &ConversationService{store: metadata, root: root, plans: plans, model: model}
	conversation, err := service.Create(ctx, "先建立 one，再建立 two")
	if err != nil || conversation.State != store.ConversationAwaitingConfirmation || conversation.Analysis == nil || len(conversation.Analysis.Operations) != 2 {
		t.Fatalf("Create() = (%#v, %v)", conversation, err)
	}
	conversation, err = service.ConfirmAnalysis(ctx, conversation.ID)
	if err != nil || conversation.State != store.ConversationAwaitingApproval || conversation.PlanID == "" {
		t.Fatalf("ConfirmAnalysis() = (%#v, %v)", conversation, err)
	}
	plan, err := plans.Plan(ctx, conversation.PlanID)
	if err != nil || len(plan.Operations) != 2 || plan.Operations[0].Ordinal != 0 || plan.Operations[1].Ordinal != 1 {
		t.Fatalf("created plan = (%#v, %v)", plan, err)
	}
	if model.callCount() != 1 {
		t.Fatalf("non-content operations called model %d times; want only the clarification turn", model.callCount())
	}
}

func TestConversationTrashRequiresApprovalAndMovesOnlySelectedEntriesToRecoverableTrash(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	docsPath := filepath.Join(rootPath, "docs")
	if err := os.Mkdir(docsPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(docsPath, "folder"), 0o700); err != nil {
		t.Fatal(err)
	}
	const fileBody = "private trash sentinel 8821"
	if err := os.WriteFile(filepath.Join(docsPath, "remove.md"), []byte(fileBody), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(docsPath, "folder", "child.txt"), []byte("folder contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing.txt", filepath.Join(docsPath, "broken-link")); err != nil {
		t.Fatal(err)
	}
	metadata, root, plans := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()
	trash, err := trashsvc.Open(ctx, root, metadata)
	if err != nil {
		t.Fatal(err)
	}
	plans.trash = trash
	model := &scriptedConversationModel{
		status: ModelStatus{Available: true, Provider: "test-provider", Model: "test-model", Destination: "local"},
		replies: []string{`{"state":"ready","reply":"我会把选中的文件、文件夹和软链接移入可恢复回收站，请确认。","goal":"清理指定项目","operations":[
			{"action":"trash","filePath":"docs/remove.md"},
			{"action":"trash","filePath":"docs/folder"},
			{"action":"trash","filePath":"docs/broken-link"}]}`},
	}
	conversationService := &ConversationService{store: metadata, root: root, plans: plans, model: model}
	conversation, err := conversationService.Create(ctx, "将 docs/remove.md、docs/folder 和 docs/broken-link 移入回收站")
	if err != nil || conversation.State != store.ConversationAwaitingConfirmation || conversation.Analysis == nil || len(conversation.Analysis.Operations) != 3 {
		t.Fatalf("Create() = (%#v, %v)", conversation, err)
	}
	if strings.Contains(marshalMessages(t, model.requests[0]), fileBody) {
		t.Fatal("file body reached the model for a path-only trash operation")
	}
	for _, relativePath := range []string{"docs/remove.md", "docs/folder", "docs/broken-link"} {
		if _, err := root.LstatEntry(relativePath); err != nil {
			t.Fatalf("entry changed before analysis confirmation: %s: %v", relativePath, err)
		}
	}
	conversation, err = conversationService.ConfirmAnalysis(ctx, conversation.ID)
	if err != nil || conversation.State != store.ConversationAwaitingApproval || conversation.PlanID == "" {
		t.Fatalf("ConfirmAnalysis() = (%#v, %v)", conversation, err)
	}
	plan, err := plans.Plan(ctx, conversation.PlanID)
	if err != nil || len(plan.Operations) != 3 {
		t.Fatalf("Plan() = (%#v, %v)", plan, err)
	}
	for _, operation := range plan.Operations {
		if operation.Action != "trash" || operation.TargetPath != "" {
			t.Fatalf("unexpected trash operation: %#v", operation)
		}
		plan, err = plans.Decide(ctx, plan.ID, operation.ID, "approve", plan.Version)
		if err != nil {
			t.Fatalf("approve %s: %v", operation.SourcePath, err)
		}
	}
	for _, relativePath := range []string{"docs/remove.md", "docs/folder", "docs/broken-link"} {
		if _, err := root.LstatEntry(relativePath); err != nil {
			t.Fatalf("approval moved entry before execution: %s: %v", relativePath, err)
		}
	}
	completed, err := plans.Execute(ctx, plan.ID, plan.Version)
	if err != nil || completed.Status != store.PlanCompleted {
		t.Fatalf("Execute() = (%#v, %v)", completed, err)
	}
	for _, operation := range completed.Operations {
		if operation.Status != store.OperationSucceeded {
			t.Fatalf("trash operation did not succeed: %#v", operation)
		}
		if _, err := root.LstatEntry(operation.SourcePath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("source %s remains after execution: %v", operation.SourcePath, err)
		}
	}
	page, err := trash.List(ctx, 100, "")
	if err != nil || len(page.Items) != 3 {
		t.Fatalf("trash page = (%#v, %v), want three entries", page.Items, err)
	}
	trashed := make(map[string]bool, len(page.Items))
	for _, item := range page.Items {
		trashed[item.OriginalPath] = item.Status == string(store.TrashTrashed)
	}
	for _, relativePath := range []string{"docs/remove.md", "docs/folder", "docs/broken-link"} {
		if !trashed[relativePath] {
			t.Fatalf("selected entry %s was not recoverably trashed: %#v", relativePath, trashed)
		}
	}
	audit, err := metadata.AuditForTaskPage(ctx, completed.TaskID, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	finished := 0
	linkedTrashEvents := 0
	for _, record := range audit.Records {
		if record.EventType == "operation_finished" && record.Actor == "user" && record.Outcome == "succeeded" {
			finished++
		}
		if strings.HasPrefix(record.EventType, "trash_") {
			if record.Actor != "agent" || record.TaskID != completed.TaskID || record.PlanID != completed.ID || record.OperationID == "" {
				t.Fatalf("trash audit event is not linked to the approved Agent operation: %#v", record)
			}
			linkedTrashEvents++
		}
	}
	if finished != 3 {
		t.Fatalf("task audit has %d successful Agent outcomes, want three: %#v", finished, audit.Records)
	}
	if linkedTrashEvents != 9 {
		t.Fatalf("task audit has %d linked trash lifecycle events, want nine: %#v", linkedTrashEvents, audit.Records)
	}
}

// TestConversationPermanentDeleteRequiresApprovalAndNoFollowPurge 验证不可恢复删除的审批、清理和审计。
func TestConversationPermanentDeleteRequiresApprovalAndNoFollowPurge(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	docsPath := filepath.Join(rootPath, "docs")
	if err := os.Mkdir(docsPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(docsPath, "folder"), 0o700); err != nil {
		t.Fatal(err)
	}
	const fileBody = "private permanent delete sentinel 6942"
	if err := os.WriteFile(filepath.Join(docsPath, "remove.txt"), []byte(fileBody), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(docsPath, "folder", "child.txt"), []byte("folder contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	externalFile := filepath.Join(t.TempDir(), "external.txt")
	if err := os.WriteFile(externalFile, []byte("must survive symlink deletion"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(externalFile, filepath.Join(docsPath, "external-link")); err != nil {
		t.Fatal(err)
	}
	metadata, root, plans := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()
	trash, err := trashsvc.Open(ctx, root, metadata)
	if err != nil {
		t.Fatal(err)
	}
	plans.trash = trash
	if err := metadata.SetAgentBypassPermission(ctx, false, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	model := &scriptedConversationModel{
		status: ModelStatus{Available: true, Provider: "test-provider", Model: "test-model", Destination: "local"},
		replies: []string{`{"state":"ready","reply":"这些项目将被永久删除且无法恢复，请确认生成方案。","goal":"永久清理指定项目","operations":[
			{"action":"permanent_delete","filePath":"docs/remove.txt"},
			{"action":"permanent_delete","filePath":"docs/folder"},
			{"action":"permanent_delete","filePath":"docs/external-link"}]}`},
	}
	conversationService := &ConversationService{store: metadata, root: root, plans: plans, model: model}
	conversation, err := conversationService.Create(ctx, "明确永久删除 docs/remove.txt、docs/folder 和 docs/external-link，不需要恢复")
	if err != nil || conversation.State != store.ConversationAwaitingConfirmation || conversation.Analysis == nil || len(conversation.Analysis.Operations) != 3 {
		t.Fatalf("Create() = (%#v, %v)", conversation, err)
	}
	if strings.Contains(marshalMessages(t, model.requests[0]), fileBody) {
		t.Fatal("file body reached the model for a path-only permanent-delete operation")
	}
	paths := []string{"docs/remove.txt", "docs/folder", "docs/external-link"}
	for _, relativePath := range paths {
		if _, err := root.LstatEntry(relativePath); err != nil {
			t.Fatalf("entry changed before analysis confirmation: %s: %v", relativePath, err)
		}
	}
	conversation, err = conversationService.ConfirmAnalysis(ctx, conversation.ID)
	if err != nil || conversation.State != store.ConversationAwaitingApproval || conversation.PlanID == "" {
		t.Fatalf("ConfirmAnalysis() = (%#v, %v)", conversation, err)
	}
	plan, err := plans.Plan(ctx, conversation.PlanID)
	if err != nil || len(plan.Operations) != len(paths) {
		t.Fatalf("Plan() = (%#v, %v)", plan, err)
	}
	operationIDs := make([]string, 0, len(plan.Operations))
	for _, operation := range plan.Operations {
		if operation.Action != "permanent_delete" || operation.Approval != store.ApprovalPending {
			t.Fatalf("unexpected permanent-delete operation: %#v", operation)
		}
		operationIDs = append(operationIDs, operation.ID)
	}
	if _, err := plans.StartPlan(ctx, plan.ID, plan.Version, "manual", false); !errors.Is(err, ErrConflict) {
		t.Fatalf("start permanent-delete plan without confirmation = %v, want conflict", err)
	}
	for _, relativePath := range paths {
		if _, err := root.LstatEntry(relativePath); err != nil {
			t.Fatalf("missing confirmation changed entry: %s: %v", relativePath, err)
		}
	}
	plan, err = plans.StartPlan(ctx, plan.ID, plan.Version, "manual", true)
	if err != nil || plan.Status != store.PlanApproved || plan.ExecutionMode != "manual" {
		t.Fatalf("confirmed permanent-delete review = (%#v, %v)", plan, err)
	}
	for _, relativePath := range paths {
		if _, err := root.LstatEntry(relativePath); err != nil {
			t.Fatalf("approval removed entry before execution: %s: %v", relativePath, err)
		}
	}
	var completed store.PlanRecord
	for _, operationID := range operationIDs {
		completed, err = plans.ExecuteStep(ctx, plan.ID, operationID, plan.Version)
		if err != nil {
			t.Fatalf("execute confirmed permanent-delete step %s: %v", operationID, err)
		}
		plan = completed
	}
	if completed.Status != store.PlanCompleted {
		t.Fatalf("manual permanent-delete plan status = %q, want completed", completed.Status)
	}
	for _, operation := range completed.Operations {
		if operation.Status != store.OperationSucceeded || !strings.Contains(operation.ResultMessage, "无法恢复") {
			t.Fatalf("permanent-delete operation did not report irreversible success: %#v", operation)
		}
		if _, err := root.LstatEntry(operation.SourcePath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("source %s remains after deletion: %v", operation.SourcePath, err)
		}
	}
	if content, err := os.ReadFile(externalFile); err != nil || string(content) != "must survive symlink deletion" {
		t.Fatalf("permanent deletion followed a symlink target: content=%q err=%v", content, err)
	}
	audit, err := metadata.AuditForTaskPage(ctx, completed.TaskID, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	eventsByOperation := make(map[string]map[string]bool)
	finished := 0
	for _, record := range audit.Records {
		if record.EventType == "operation_finished" && record.Actor == "user" && record.Outcome == "succeeded" {
			finished++
		}
		if strings.HasPrefix(record.EventType, "trash_") {
			if record.Actor != "agent" || record.TaskID != completed.TaskID || record.PlanID != completed.ID || record.OperationID == "" {
				t.Fatalf("permanent-delete lifecycle event is not linked to its operation: %#v", record)
			}
			if eventsByOperation[record.OperationID] == nil {
				eventsByOperation[record.OperationID] = make(map[string]bool)
			}
			eventsByOperation[record.OperationID][record.EventType] = true
		}
	}
	if finished != len(paths) || len(eventsByOperation) != len(paths) {
		t.Fatalf("audit has %d finished operations and %d lifecycle groups, want %d each", finished, len(eventsByOperation), len(paths))
	}
	for operationID, events := range eventsByOperation {
		for _, event := range []string{"trash_moving", "trash_destination_ready", "trash_trashed", "trash_purge_started", "trash_purged"} {
			if !events[event] {
				t.Fatalf("operation %s is missing audit event %s: %#v", operationID, event, events)
			}
		}
	}
}

// TestConversationRequiresExplicitPermanentDeleteRequest 验证模型不能把普通删除升级为永久删除。
func TestConversationRequiresExplicitPermanentDeleteRequest(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "old.txt"), []byte("keep until approval"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata, root, plans := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()
	trash, err := trashsvc.Open(ctx, root, metadata)
	if err != nil {
		t.Fatal(err)
	}
	plans.trash = trash
	permanentResponse := `{"state":"ready","reply":"我会永久删除该文件，请确认。","contextSummary":"用户已同意永久删除 old.txt。","goal":"删除旧文件","operations":[{"action":"permanent_delete","filePath":"old.txt"}]}`
	model := &scriptedConversationModel{
		status:  ModelStatus{Available: true, Provider: "test-provider", Model: "test-model", Destination: "local"},
		replies: []string{permanentResponse, permanentResponse, permanentResponse, permanentResponse},
	}
	service := &ConversationService{store: metadata, root: root, plans: plans, model: model}
	conversation, err := service.Create(ctx, "请删除 old.txt")
	if err != nil || conversation.State != store.ConversationClarifying || conversation.Analysis != nil || conversation.ContextSummary != "用户已同意永久删除 old.txt。" || len(conversation.Messages) == 0 || !strings.Contains(conversation.Messages[len(conversation.Messages)-1].Content, "明确说明") {
		t.Fatalf("ambiguous delete result = (%#v, %v), want explicit clarification", conversation, err)
	}
	if _, err := root.LstatEntry("old.txt"); err != nil {
		t.Fatalf("ambiguous request changed the file: %v", err)
	}
	conversation, err = service.AddMessage(ctx, conversation.ID, "我没有授权永久删除 old.txt")
	if err != nil || conversation.State != store.ConversationClarifying || conversation.Analysis != nil {
		t.Fatalf("unauthorized permanent-delete mention = (%#v, %v), want clarification", conversation, err)
	}
	if _, err := root.LstatEntry("old.txt"); err != nil {
		t.Fatalf("unauthorized mention changed the file: %v", err)
	}
	conversation, err = service.AddMessage(ctx, conversation.ID, "不允许永久删除 old.txt，请将它放进回收站")
	if err != nil || conversation.State != store.ConversationClarifying || conversation.Analysis != nil {
		t.Fatalf("negative permanent-delete request = (%#v, %v), want clarification", conversation, err)
	}
	if _, err := root.LstatEntry("old.txt"); err != nil {
		t.Fatalf("negative request changed the file: %v", err)
	}
	conversation, err = service.AddMessage(ctx, conversation.ID, "请永久删除 old.txt，不需要恢复")
	if err != nil || conversation.State != store.ConversationAwaitingConfirmation || conversation.Analysis == nil || conversation.Analysis.Action != "permanent_delete" {
		t.Fatalf("explicit permanent-delete request = (%#v, %v)", conversation, err)
	}
	if _, err := root.LstatEntry("old.txt"); err != nil {
		t.Fatalf("explicit request changed the file before plan approval: %v", err)
	}
}

func TestTrashApprovalInvalidatesWhenSourceChanges(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "source.txt"), []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata, root, plans := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()
	trash, err := trashsvc.Open(ctx, root, metadata)
	if err != nil {
		t.Fatal(err)
	}
	plans.trash = trash
	plan, err := plans.Propose(ctx, Proposal{
		RequestText: "将文件移入回收站", Action: "trash", SourcePath: "source.txt", Reason: "按用户确认移入回收站",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "source.txt"), []byte("changed after proposal"), 0o600); err != nil {
		t.Fatal(err)
	}
	updated, err := plans.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve", plan.Version)
	if !errors.Is(err, ErrConflict) || updated.Status != store.PlanInvalidated || updated.Operations[0].Approval != store.ApprovalInvalidated {
		t.Fatalf("stale trash approval = (%#v, %v), want invalidated", updated, err)
	}
	if data, err := os.ReadFile(filepath.Join(rootPath, "source.txt")); err != nil || string(data) != "changed after proposal" {
		t.Fatalf("stale entry was changed: data=%q err=%v", data, err)
	}
	page, err := trash.List(ctx, 100, "")
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("stale item entered trash: %#v, %v", page.Items, err)
	}
}

func TestTrashApprovalInvalidatesWhenDirectoryContentsChange(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	directory := filepath.Join(rootPath, "folder")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "existing.txt"), []byte("approved content"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata, root, plans := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()
	trash, err := trashsvc.Open(ctx, root, metadata)
	if err != nil {
		t.Fatal(err)
	}
	plans.trash = trash
	plan, err := plans.Propose(ctx, Proposal{
		RequestText: "将文件夹移入回收站", Action: "trash", SourcePath: "folder", Reason: "按用户确认移入回收站",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "new.txt"), []byte("added after approval"), 0o600); err != nil {
		t.Fatal(err)
	}
	updated, err := plans.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve", plan.Version)
	if !errors.Is(err, ErrConflict) || updated.Status != store.PlanInvalidated || updated.Operations[0].Approval != store.ApprovalInvalidated {
		t.Fatalf("stale directory approval = (%#v, %v), want invalidated", updated, err)
	}
	if _, err := os.Stat(filepath.Join(directory, "new.txt")); err != nil {
		t.Fatalf("changed directory was moved: %v", err)
	}
	page, err := trash.List(ctx, 100, "")
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("changed directory entered trash: %#v, %v", page.Items, err)
	}
}

func TestConversationMultiOperationPreflightCreatesNoPartialPlan(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "parent"), 0o755); err != nil {
		t.Fatal(err)
	}
	metadata, root, plans := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()
	model := &scriptedConversationModel{
		status: ModelStatus{Available: true, Provider: "test-provider", Model: "test-model", Destination: "local"},
		replies: []string{`{"state":"ready","reply":"请确认两个目录。","goal":"整理目录结构","operations":[
			{"action":"create_directory","targetPath":"parent/one"},
			{"action":"create_directory","targetPath":"parent/two"}]}`},
	}
	service := &ConversationService{store: metadata, root: root, plans: plans, model: model}
	conversation, err := service.Create(ctx, "建立两个目录")
	if err != nil || conversation.State != store.ConversationAwaitingConfirmation {
		t.Fatalf("Create() = (%#v, %v)", conversation, err)
	}
	if err := os.Mkdir(filepath.Join(rootPath, "parent", "two"), 0o700); err != nil {
		t.Fatal(err)
	}
	failed, err := service.ConfirmAnalysis(ctx, conversation.ID)
	if err == nil || failed.State != store.ConversationFailed || failed.PlanID != "" {
		t.Fatalf("ConfirmAnalysis() = (%#v, %v), want a failed confirmation without a plan", failed, err)
	}
	page, err := metadata.ListTasks(ctx, store.TaskListQuery{Kind: "agent", Limit: 20})
	if err != nil || len(page.Tasks) != 0 {
		t.Fatalf("partial plan persisted: tasks=%#v err=%v", page.Tasks, err)
	}
	if _, err := os.Stat(filepath.Join(rootPath, "parent", "one")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preflight created the valid first target: %v", err)
	}
}

func TestConversationAcceptsModelSizedIndependentPlan(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "parent"), 0o755); err != nil {
		t.Fatal(err)
	}
	metadata, root, plans := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()

	operations := make([]store.AnalysisOperation, 21)
	for index := range operations {
		operations[index] = store.AnalysisOperation{Action: "create_directory", TargetPath: fmt.Sprintf("parent/category-%02d", index)}
	}
	response, err := json.Marshal(struct {
		State      string                    `json:"state"`
		Reply      string                    `json:"reply"`
		Goal       string                    `json:"goal"`
		Operations []store.AnalysisOperation `json:"operations"`
	}{State: "ready", Reply: "将按计划整理类别，请确认。", Goal: "整理类别目录", Operations: operations})
	if err != nil {
		t.Fatal(err)
	}
	model := &scriptedConversationModel{
		status:  ModelStatus{Available: true, Provider: "test-provider", Model: "test-model", Destination: "local"},
		replies: []string{string(response)},
	}
	service := &ConversationService{store: metadata, root: root, plans: plans, model: model}
	conversation, err := service.Create(ctx, "建立 21 个类别文件夹")
	if err != nil || conversation.State != store.ConversationAwaitingConfirmation || conversation.Analysis == nil || len(conversation.Analysis.Operations) != len(operations) {
		var operationCount int
		if conversation.Analysis != nil {
			operationCount = len(conversation.Analysis.Operations)
		}
		t.Fatalf("Create() = (state %q, operations %d, error %v)", conversation.State, operationCount, err)
	}
	conversation, err = service.ConfirmAnalysis(ctx, conversation.ID)
	if err != nil || conversation.State != store.ConversationAwaitingApproval || conversation.PlanID == "" {
		t.Fatalf("ConfirmAnalysis() = (state %q, plan %q, error %v)", conversation.State, conversation.PlanID, err)
	}
	plan, err := plans.Plan(ctx, conversation.PlanID)
	if err != nil || len(plan.Operations) != len(operations) {
		t.Fatalf("Plan() = (%d operations, %v), want %d", len(plan.Operations), err, len(operations))
	}
}

func TestConfirmedDirectoryConversationRequiresApprovalAndExplicitExecution(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "photos"), 0o755); err != nil {
		t.Fatal(err)
	}
	metadata, root, plans := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()
	model := &scriptedConversationModel{
		status:  ModelStatus{Available: true, Provider: "test-provider", Model: "test-model", Destination: "local"},
		replies: []string{`{"state":"ready","reply":"我将仅使用任务描述和目标路径生成新建文件夹方案，请确认。","goal":"按年份整理照片","action":"create_directory","filePath":"","targetPath":"photos/2025"}`},
	}
	service := &ConversationService{store: metadata, root: root, plans: plans, model: model}
	conversation, err := service.Create(ctx, "在 photos 下新建 2025 文件夹")
	if err != nil || conversation.State != store.ConversationAwaitingConfirmation || conversation.Analysis == nil {
		t.Fatalf("Create() = (%#v, %v)", conversation, err)
	}
	if card := conversation.Analysis; card.Action != "create_directory" || card.TargetPath != "photos/2025" || card.FilePath != "" || len(card.DataTypes) != 1 || card.DataTypes[0] != "任务描述与目标路径" {
		t.Fatalf("directory confirmation card = %#v", card)
	}
	if model.callCount() != 1 {
		t.Fatalf("clarification model calls = %d, want 1", model.callCount())
	}
	target := filepath.Join(rootPath, "photos", "2025")
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("conversation created directory before confirmation: %v", err)
	}
	conversation, err = service.ConfirmAnalysis(ctx, conversation.ID)
	if err != nil || conversation.State != store.ConversationAwaitingApproval || conversation.PlanID == "" {
		t.Fatalf("ConfirmAnalysis() = (%#v, %v)", conversation, err)
	}
	if model.callCount() != 1 {
		t.Fatalf("directory confirmation made an unnecessary model call: %d", model.callCount())
	}
	plan, err := plans.Plan(ctx, conversation.PlanID)
	if err != nil || len(plan.Operations) != 1 {
		t.Fatalf("Plan() = (%#v, %v)", plan, err)
	}
	operation := plan.Operations[0]
	if operation.Action != "create_directory" || operation.SourcePath != "photos" || operation.TargetPath != "photos/2025" {
		t.Fatalf("directory operation = %#v", operation)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plan confirmation created directory before approval: %v", err)
	}
	if _, err := plans.Decide(ctx, plan.ID, operation.ID, "approve"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("approval executed directory creation: %v", err)
	}
	completed, err := plans.Execute(ctx, plan.ID)
	if err != nil || completed.Status != store.PlanCompleted || completed.Operations[0].Status != store.OperationSucceeded {
		t.Fatalf("Execute() = (%#v, %v)", completed, err)
	}
	if info, err := os.Stat(target); err != nil || !info.IsDir() {
		t.Fatalf("executed directory = (%v, %v)", info, err)
	}
}

// TestConfirmedMoveConversationRequiresApprovalAndExplicitExecution 验证对话至单文件移动的审批闭环。
func TestConfirmedMoveConversationRequiresApprovalAndExplicitExecution(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	sourcePath := filepath.Join(rootPath, "待整理", "合同.pdf")
	targetDirectory := filepath.Join(rootPath, "合同")
	if err := os.MkdirAll(filepath.Dir(sourcePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(targetDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte("%PDF-private-document-payload")
	if err := os.WriteFile(sourcePath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	metadata, root, plans := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()
	model := &scriptedConversationModel{
		status:  ModelStatus{Available: true, Provider: "test-provider", Model: "test-model", Destination: "local"},
		replies: []string{`{"state":"ready","reply":"只使用任务描述、源路径与目标路径生成移动方案，请确认。","goal":"将合同归档到合同文件夹","action":"move_file","filePath":"待整理/合同.pdf","targetPath":"合同/合同.pdf"}`},
	}
	service := &ConversationService{store: metadata, root: root, plans: plans, model: model}
	conversation, err := service.Create(ctx, "把待整理/合同.pdf 移到合同文件夹")
	if err != nil || conversation.State != store.ConversationAwaitingConfirmation || conversation.Analysis == nil {
		t.Fatalf("Create() = (%#v, %v)", conversation, err)
	}
	card := conversation.Analysis
	if card.Action != "move_file" || card.FilePath != "待整理/合同.pdf" || card.TargetPath != "合同/合同.pdf" || len(card.DataTypes) != 1 || card.DataTypes[0] != "任务描述、源路径与目标路径" {
		t.Fatalf("move confirmation card = %#v", card)
	}
	model.mu.Lock()
	firstRequest := marshalMessages(t, model.requests[0])
	model.mu.Unlock()
	if strings.Contains(firstRequest, string(body)) || strings.Contains(firstRequest, "PDF-private-document-payload") {
		t.Fatal("file content reached the clarification model")
	}

	conversation, err = service.ConfirmAnalysis(ctx, conversation.ID)
	if err != nil || conversation.State != store.ConversationAwaitingApproval || conversation.PlanID == "" {
		t.Fatalf("ConfirmAnalysis() = (%#v, %v)", conversation, err)
	}
	if model.callCount() != 1 {
		t.Fatalf("move confirmation made an unnecessary model call: %d", model.callCount())
	}
	plan, err := plans.Plan(ctx, conversation.PlanID)
	if err != nil || len(plan.Operations) != 1 {
		t.Fatalf("Plan() = (%#v, %v)", plan, err)
	}
	operation := plan.Operations[0]
	if operation.Action != "move_file" || operation.SourcePath != "待整理/合同.pdf" || operation.TargetPath != "合同/合同.pdf" {
		t.Fatalf("move operation = %#v", operation)
	}
	var savedSource, savedTargetParent fsroot.Fingerprint
	if err := json.Unmarshal([]byte(operation.SourceStateJSON), &savedSource); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(operation.TargetStateJSON), &savedTargetParent); err != nil {
		t.Fatal(err)
	}
	source, err := root.LstatEntry("待整理/合同.pdf")
	if err != nil || !sameFingerprint(savedSource, source.Fingerprint()) {
		t.Fatalf("move plan source snapshot = (%#v, %v)", savedSource, err)
	}
	parent, err := root.LstatEntry("合同")
	if err != nil || !sameFingerprint(savedTargetParent, parent.Fingerprint()) {
		t.Fatalf("move plan target parent snapshot = (%#v, %v)", savedTargetParent, err)
	}
	targetPath := filepath.Join(targetDirectory, "合同.pdf")
	if _, err := os.Stat(sourcePath); err != nil {
		t.Fatalf("conversation moved source before approval: %v", err)
	}
	if _, err := os.Stat(targetPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("conversation created target before approval: %v", err)
	}
	if _, err := plans.Decide(ctx, plan.ID, operation.ID, "approve"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sourcePath); err != nil {
		t.Fatalf("approval moved source before explicit execution: %v", err)
	}
	if _, err := os.Stat(targetPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("approval created target before explicit execution: %v", err)
	}
	completed, err := plans.Execute(ctx, plan.ID)
	if err != nil || completed.Status != store.PlanCompleted || completed.Operations[0].Status != store.OperationSucceeded {
		t.Fatalf("Execute() = (%#v, %v)", completed, err)
	}
	if _, err := os.Stat(sourcePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source still exists after approved execution: %v", err)
	}
	if got, err := os.ReadFile(targetPath); err != nil || string(got) != string(body) {
		t.Fatalf("moved target contents = (%q, %v)", got, err)
	}
}

func TestAnalysisResponseRejectsFreeTextEvidence(t *testing.T) {
	for _, response := range []string{
		`{"supported":true,"targetName":"safe.md","reasonCode":"topic","reason":"Private sentinel 7412"}`,
		`{"supported":true,"targetName":"safe.md","reasonCode":"topic","evidence":"Private sentinel 7412"}`,
		`{"supported":true,"targetName":"safe.md","reasonCode":"topic","reply":"Private sentinel 7412"}`,
	} {
		var result analysisResult
		if err := decodeStrictModelJSON(response, &result); !errors.Is(err, ErrModelResponse) {
			t.Fatalf("decodeStrictModelJSON(%s) error = %v, want ErrModelResponse", response, err)
		}
	}
}

func TestAnalysisRejectsFileChangedWhileModelIsRunning(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	filePath := filepath.Join(rootPath, "notes.md")
	if err := os.WriteFile(filePath, []byte("# Before analysis\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata, root, plans := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer func() {
		if metadata != nil {
			_ = metadata.Close()
		}
	}()
	model := &scriptedConversationModel{
		status: ModelStatus{Available: true, Provider: "test-provider", Model: "test-model", Destination: "local"},
		replies: []string{
			`{"state":"ready","reply":"请确认分析。","goal":"依据正文命名","action":"rename","filePath":"notes.md","targetPath":""}`,
			`{"supported":true,"targetName":"new.md","reasonCode":"title"}`,
		},
		entered: make(chan struct{}), release: make(chan struct{}),
	}
	service := &ConversationService{store: metadata, root: root, plans: plans, model: model}
	conversation, err := service.Create(ctx, "重命名 notes.md")
	if err != nil || conversation.State != store.ConversationAwaitingConfirmation {
		t.Fatalf("Create() = (%#v, %v)", conversation, err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := service.ConfirmAnalysis(ctx, conversation.ID)
		result <- err
	}()
	select {
	case <-model.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("analysis model call did not start")
	}
	if err := os.WriteFile(filePath, []byte("# Changed while model was running\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	close(model.release)
	if err := <-result; !errors.Is(err, ErrConflict) {
		t.Fatalf("ConfirmAnalysis() error = %v, want conflict", err)
	}
	conversation, err = service.Get(ctx, conversation.ID)
	if err != nil || conversation.State != store.ConversationFailed || conversation.Error != "file_changed" {
		t.Fatalf("conversation after race = (%#v, %v)", conversation, err)
	}
	tasks, err := metadata.ListTasks(ctx, store.TaskListQuery{Limit: 10})
	if err != nil || len(tasks.Tasks) != 0 {
		t.Fatalf("changed file created a plan/task: %#v (%v)", tasks, err)
	}
}

func TestConcurrentConfirmationCannotStartDuplicateAnalysis(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "notes.md"), []byte("# Content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata, root, plans := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer func() {
		if metadata != nil {
			_ = metadata.Close()
		}
	}()
	model := &scriptedConversationModel{
		status: ModelStatus{Available: true, Provider: "test-provider", Model: "test-model", Destination: "local"},
		replies: []string{
			`{"state":"ready","reply":"请确认。","goal":"根据正文重命名","action":"rename","filePath":"notes.md","targetPath":""}`,
			`{"supported":true,"targetName":"content.md","reasonCode":"semantic"}`,
		},
		entered: make(chan struct{}), release: make(chan struct{}),
	}
	service := &ConversationService{store: metadata, root: root, plans: plans, model: model}
	conversation, err := service.Create(ctx, "重命名 notes.md")
	if err != nil || conversation.State != store.ConversationAwaitingConfirmation {
		t.Fatalf("Create() = (%#v, %v)", conversation, err)
	}
	firstResult := make(chan error, 1)
	go func() {
		_, err := service.ConfirmAnalysis(ctx, conversation.ID)
		firstResult <- err
	}()
	select {
	case <-model.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("analysis model call did not start")
	}
	if _, err := service.ConfirmAnalysis(ctx, conversation.ID); !errors.Is(err, store.ErrConversationConflict) {
		t.Fatalf("second in-flight confirmation error = %v", err)
	}
	close(model.release)
	if err := <-firstResult; err != nil {
		t.Fatalf("first confirmation failed: %v", err)
	}
	if got := model.callCount(); got != 2 {
		t.Fatalf("provider call count = %d, want one clarification + one analysis", got)
	}
}

func TestUnsupportedFileNeverGetsConfirmationCard(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "report.pdf"), []byte("%PDF-private"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata, root, plans := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()
	model := &scriptedConversationModel{
		status:  ModelStatus{Available: true, Provider: "test-provider", Model: "test-model", Destination: "local"},
		replies: []string{`{"state":"ready","reply":"请确认分析。","goal":"依据正文命名","action":"rename","filePath":"report.pdf","targetPath":""}`},
	}
	service := &ConversationService{store: metadata, root: root, plans: plans, model: model}
	conversation, err := service.Create(ctx, "整理 report.pdf")
	if err != nil || conversation.State != store.ConversationUnsupported || conversation.Analysis != nil || model.callCount() != 1 {
		t.Fatalf("unsupported file conversation = (%#v, %v), calls=%d", conversation, err, model.callCount())
	}
	if strings.Contains(marshalMessages(t, model.requests[0]), "PDF-private") {
		t.Fatal("unsupported file body reached the clarification model")
	}
}

func TestTargetCollisionDoesNotCreatePlanOrOverwriteFile(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "notes.md"), []byte("# source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "occupied.md"), []byte("keep target"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata, root, plans := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()
	model := &scriptedConversationModel{
		status: ModelStatus{Available: true, Provider: "test-provider", Model: "test-model", Destination: "local"},
		replies: []string{
			`{"state":"ready","reply":"请确认分析。","goal":"依据正文命名","action":"rename","filePath":"notes.md","targetPath":""}`,
			`{"supported":true,"targetName":"occupied.md","reasonCode":"title"}`,
		},
	}
	service := &ConversationService{store: metadata, root: root, plans: plans, model: model}
	conversation, err := service.Create(ctx, "重命名 notes.md")
	if err != nil || conversation.State != store.ConversationAwaitingConfirmation {
		t.Fatalf("Create() = (%#v, %v)", conversation, err)
	}
	conversation, err = service.ConfirmAnalysis(ctx, conversation.ID)
	if !errors.Is(err, ErrConflict) || conversation.State != store.ConversationFailed || conversation.Error != "plan_conflict" {
		t.Fatalf("collision result = (%#v, %v)", conversation, err)
	}
	target, err := os.ReadFile(filepath.Join(rootPath, "occupied.md"))
	if err != nil || string(target) != "keep target" {
		t.Fatalf("target changed after collision: %q (%v)", target, err)
	}
	tasks, err := metadata.ListTasks(ctx, store.TaskListQuery{Limit: 10})
	if err != nil || len(tasks.Tasks) != 0 {
		t.Fatalf("target collision persisted a task: %#v (%v)", tasks, err)
	}
}

func TestConfirmedMetadataConversationStoresTargetWithoutReadingContent(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	const privateContent = "private-metadata-file-content-3184"
	if err := os.WriteFile(filepath.Join(rootPath, "memo.txt"), []byte(privateContent), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata, root, plans := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()
	model := &scriptedConversationModel{
		status:  ModelStatus{Available: true, Provider: "test-provider", Model: "test-model", Destination: "local"},
		replies: []string{`{"state":"ready","reply":"我只会使用任务描述、路径和目标权限，请确认。","goal":"限制文件读取权限","action":"set_permissions","filePath":"memo.txt","targetPath":"","requestedMode":256}`},
	}
	service := &ConversationService{store: metadata, root: root, plans: plans, model: model}
	conversation, err := service.Create(ctx, "将 memo.txt 权限改为 0400")
	if err != nil || conversation.State != store.ConversationAwaitingConfirmation || conversation.Analysis == nil {
		t.Fatalf("Create() = (%#v, %v)", conversation, err)
	}
	card := conversation.Analysis
	if card.Action != "set_permissions" || card.FilePath != "memo.txt" || card.RequestedMode == nil || *card.RequestedMode != 0o400 || card.RequestedOwnerUID != nil || card.RequestedGroupGID != nil {
		t.Fatalf("metadata confirmation card = %#v", card)
	}
	if strings.Contains(marshalMessages(t, model.requests[0]), privateContent) {
		t.Fatal("file content reached the metadata clarification model")
	}
	conversation, err = service.ConfirmAnalysis(ctx, conversation.ID)
	if err != nil || conversation.State != store.ConversationAwaitingApproval || conversation.PlanID == "" {
		t.Fatalf("ConfirmAnalysis() = (%#v, %v)", conversation, err)
	}
	if model.callCount() != 1 {
		t.Fatalf("metadata confirmation made a model call with file content: %d", model.callCount())
	}
	plan, err := plans.Plan(ctx, conversation.PlanID)
	if err != nil || len(plan.Operations) != 1 || plan.Operations[0].Mode == nil || *plan.Operations[0].Mode != 0o400 {
		t.Fatalf("metadata plan = (%#v, %v)", plan, err)
	}
	if got := fileMode(t, filepath.Join(rootPath, "memo.txt")); got != 0o600 {
		t.Fatalf("confirmation changed file mode to %#o before approval", got)
	}
	if _, err := plans.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve"); err != nil {
		t.Fatal(err)
	}
	completed, err := plans.Execute(ctx, plan.ID)
	if err != nil || completed.Status != store.PlanCompleted || fileMode(t, filepath.Join(rootPath, "memo.txt")) != 0o400 {
		t.Fatalf("metadata execution = (%#v, %v)", completed, err)
	}
}

func TestDuplicateScanNeedsConfirmationAndPersistsOnlyReadOnlyResults(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	scope := filepath.Join(rootPath, "docs")
	if err := os.Mkdir(scope, 0o700); err != nil {
		t.Fatal(err)
	}
	const privateContent = "duplicate-scan-private-body-6204"
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(scope, name), []byte(privateContent), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	metadata, root, plans := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()
	service := &ConversationService{
		store: metadata, root: root, plans: plans,
		model: &scriptedConversationModel{
			status: ModelStatus{Available: true, Provider: "test-provider", Model: "test-model", Destination: "local"},
			replies: []string{
				`{"state":"ready","reply":"将在 docs 范围内比对完整文件内容，只生成只读报告，请确认。","goal":"找出重复文件","action":"find_duplicates","filePath":"docs","targetPath":""}`,
				`{"state":"ready","reply":"重新扫描 docs 范围，请确认。","goal":"再次核查重复文件","action":"find_duplicates","filePath":"docs","targetPath":""}`,
			},
		},
		duplicateScan: newDuplicateScanManager(metadata, root),
	}
	defer service.Close()

	conversation, err := service.Create(ctx, "找出 docs 中的重复文件")
	if err != nil || conversation.State != store.ConversationAwaitingConfirmation || conversation.Analysis == nil || conversation.Analysis.Action != "find_duplicates" {
		t.Fatalf("duplicate confirmation = (%#v, %v)", conversation, err)
	}
	model := service.model.(*scriptedConversationModel)
	if strings.Contains(marshalMessages(t, model.requests[0]), privateContent) {
		t.Fatal("duplicate file body reached the conversation model")
	}
	conversation, err = service.ConfirmAnalysis(ctx, conversation.ID)
	if err != nil || conversation.State != store.ConversationAnalyzing || conversation.DuplicateScanID == "" {
		t.Fatalf("confirmed duplicate scan = (%#v, %v)", conversation, err)
	}
	firstScanID := conversation.DuplicateScanID
	deadline := time.Now().Add(5 * time.Second)
	var scan store.DuplicateScan
	for time.Now().Before(deadline) {
		scan, err = service.GetDuplicateScan(ctx, firstScanID)
		if err != nil {
			t.Fatal(err)
		}
		if scan.Status == store.DuplicateScanSucceeded || scan.Status == store.DuplicateScanFailed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if scan.Status != store.DuplicateScanSucceeded || scan.GroupCount != 1 || scan.DuplicateFileCount != 2 {
		t.Fatalf("duplicate scan result = %#v", scan)
	}
	conversation, err = service.Get(ctx, conversation.ID)
	if err != nil || conversation.State != store.ConversationAnalysisComplete || conversation.DuplicateScanID != firstScanID {
		t.Fatalf("completed duplicate conversation = (%#v, %v)", conversation, err)
	}
	groups, err := service.ListDuplicateGroups(ctx, firstScanID, 20, 0)
	if err != nil || len(groups.Groups) != 1 {
		t.Fatalf("duplicate groups = (%#v, %v)", groups, err)
	}
	files, err := service.ListDuplicateFiles(ctx, firstScanID, groups.Groups[0].ID, 20, 0)
	if err != nil || len(files.Files) != 2 || files.Files[0].Path != "docs/a.txt" || files.Files[1].Path != "docs/b.txt" {
		t.Fatalf("duplicate files = (%#v, %v)", files, err)
	}
	if strings.Contains(marshalMessages(t, model.requests[len(model.requests)-1]), privateContent) {
		t.Fatal("duplicate file body appeared in a later model request")
	}

	conversation, err = service.AddMessage(ctx, conversation.ID, "再扫描一次")
	if err != nil || conversation.State != store.ConversationAwaitingConfirmation {
		t.Fatalf("follow-up duplicate confirmation = (%#v, %v)", conversation, err)
	}
	conversation, err = service.ConfirmAnalysis(ctx, conversation.ID)
	if err != nil || conversation.DuplicateScanID == "" || conversation.DuplicateScanID == firstScanID {
		t.Fatalf("second duplicate scan = (%#v, %v)", conversation, err)
	}
	oldScan, err := service.GetDuplicateScan(ctx, firstScanID)
	if err != nil || oldScan.Status != store.DuplicateScanSucceeded {
		t.Fatalf("prior duplicate scan history = (%#v, %v)", oldScan, err)
	}
}

func TestMetadataConversationResponseRequiresTypedTargetValues(t *testing.T) {
	decimal755 := uint32(755)
	for _, response := range []conversationTurnResult{
		{State: "ready", Reply: "请确认", Goal: "权限", Action: "set_permissions", FilePath: "notes.txt"},
		{State: "ready", Reply: "请确认", Goal: "属主", Action: "set_owner", FilePath: "notes.txt"},
		{State: "ready", Reply: "请确认", Goal: "混合", Action: "set_permissions", FilePath: "notes.txt", RequestedOwnerUID: uint32Pointer(1)},
		{State: "ready", Reply: "请确认", Goal: "八进制权限误传", Action: "set_permissions", FilePath: "notes.txt", RequestedMode: &decimal755},
	} {
		if err := validateTurnResult(&response); !errors.Is(err, ErrModelResponse) {
			t.Fatalf("validateTurnResult(%#v) error = %v, want invalid model response", response, err)
		}
	}
	mode := uint32(0o640)
	valid := conversationTurnResult{State: "ready", Reply: "请确认", Goal: "权限", Action: "set_permissions", FilePath: "notes.txt", RequestedMode: &mode}
	if err := validateTurnResult(&valid); err != nil {
		t.Fatalf("validateTurnResult(valid) = %v", err)
	}
	if !strings.Contains(conversationSystemPrompt, "0755 应输出十进制 493") || !strings.Contains(conversationSystemPrompt, "不得从用户名、组名或描述推断数字 ID") {
		t.Fatal("metadata prompt lost numeric mode or owner-ID requirements")
	}
}

func TestConversationAcceptsLineBreaksInModelClarification(t *testing.T) {
	valid := conversationTurnResult{State: "clarifying", Reply: "请补充整理范围：\n1. 目标目录\n2. 期望操作"}
	if err := validateTurnResult(&valid); err != nil {
		t.Fatalf("validateTurnResult(multiline clarification) = %v", err)
	}
	if !strings.Contains(conversationSystemPrompt, "每种状态都必须包含非空 reply") {
		t.Fatal("conversation prompt must require a visible reply for every state")
	}

	invalid := conversationTurnResult{State: "clarifying", Reply: "请补充范围\x00"}
	if err := validateTurnResult(&invalid); !errors.Is(err, ErrModelResponse) {
		t.Fatalf("validateTurnResult(control character) = %v, want invalid model response", err)
	}
}

func TestConversationPromptKeepsUserFacingRepliesBrief(t *testing.T) {
	if !strings.Contains(conversationSystemPrompt, "简洁自然") || !strings.Contains(conversationSystemPrompt, "不要复述卡片已显示") {
		t.Fatal("conversation prompt must keep user-facing replies concise")
	}
	if !strings.Contains(conversationSystemPrompt, "goal 不超过 60 字") || !strings.Contains(conversationSystemPrompt, "reply 不超过 120 字") {
		t.Fatal("conversation prompt must constrain goal and reply length")
	}
	if !strings.Contains(shellIntentPrompt, "不复述卡片中的命令、范围或审批流程") {
		t.Fatal("Shell replies must not repeat details already shown in the confirmation card")
	}
}

func uint32Pointer(value uint32) *uint32 {
	return &value
}

func openAgentTestServices(t *testing.T, databasePath, rootPath string) (*store.Store, *fsroot.Root, *Service) {
	t.Helper()
	metadata, err := store.Open(context.Background(), databasePath)
	if err != nil {
		t.Fatal(err)
	}
	root, err := fsroot.Open(rootPath)
	if err != nil {
		_ = metadata.Close()
		t.Fatal(err)
	}
	if err := root.ConfigureMoveRecoveryDirectory(filepath.Join(filepath.Dir(databasePath), "move-recovery")); err != nil {
		_ = root.Close()
		_ = metadata.Close()
		t.Fatal(err)
	}
	return metadata, root, New(metadata, root, coordination.New())
}

func marshalMessages(t *testing.T, messages []ChatMessage) string {
	t.Helper()
	data, err := json.Marshal(messages)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestRenameRecordsPartialWhenTargetContentDiffers(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	parent := filepath.Join(rootPath, "parent")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(parent, "source.txt")
	target := filepath.Join(parent, "target.txt")
	if err := os.WriteFile(source, []byte("approved"), 0o600); err != nil {
		t.Fatal(err)
	}
	originalInfo, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()
	plan, err := service.ProposeMany(ctx, []Proposal{{
		RequestText: "重命名文件", ScopePath: "parent", Action: "rename",
		SourcePath: "parent/source.txt", TargetPath: "parent/target.txt", Reason: "按用户确认的名称修改",
	}})
	if err != nil || len(plan.Operations) != 1 {
		t.Fatalf("ProposeMany() = (%#v, %v)", plan, err)
	}
	op := plan.Operations[0]
	if _, err := service.Decide(ctx, plan.ID, op.ID, "approve", plan.Version); err != nil {
		t.Fatal(err)
	}
	if err := metadata.ClaimAgentPlanExecution(ctx, plan.ID, plan.Version, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := metadata.StartAgentOperation(ctx, plan.ID, op.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	digest, err := service.hashApprovedFile(op)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.applyOperation(op); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(target, originalInfo.ModTime(), originalInfo.ModTime()); err != nil {
		t.Fatal(err)
	}
	entry, err := root.LstatEntry(op.TargetPath)
	if err != nil || !matchesRenameResult(entry.Fingerprint(), op.SourceStateJSON) {
		t.Fatalf("test setup did not retain the approved file fingerprint: (%#v, %v)", entry, err)
	}
	updated, err := service.recordRenameOutcome(ctx, plan.ID, op, digest, nil)
	if !errors.Is(err, ErrConflict) || updated.Status != store.PlanFailed || updated.Operations[0].Status != store.OperationPartial {
		t.Fatalf("tampered rename result = (%#v, %v), want manual review", updated, err)
	}
}

func TestRenameRecoveryRequiresManualReviewWithoutPersistedDigest(t *testing.T) {
	for _, test := range []struct {
		name       string
		changeFile bool
	}{
		{name: "normal rename target"},
		{name: "same inode content changed", changeFile: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			rootPath := t.TempDir()
			parent := filepath.Join(rootPath, "parent")
			if err := os.Mkdir(parent, 0o755); err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(parent, "source.txt")
			target := filepath.Join(parent, "target.txt")
			if err := os.WriteFile(source, []byte("approved"), 0o640); err != nil {
				t.Fatal(err)
			}
			originalInfo, err := os.Stat(source)
			if err != nil {
				t.Fatal(err)
			}
			metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
			defer root.Close()
			defer metadata.Close()
			plan, err := service.ProposeMany(ctx, []Proposal{{
				RequestText: "重命名文件", ScopePath: "parent", Action: "rename",
				SourcePath: "parent/source.txt", TargetPath: "parent/target.txt", Reason: "按用户确认的名称修改",
			}})
			if err != nil || len(plan.Operations) != 1 {
				t.Fatalf("ProposeMany() = (%#v, %v)", plan, err)
			}
			op := plan.Operations[0]
			if _, err := service.Decide(ctx, plan.ID, op.ID, "approve", plan.Version); err != nil {
				t.Fatal(err)
			}
			if err := metadata.ClaimAgentPlanExecution(ctx, plan.ID, plan.Version, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			if err := metadata.StartAgentOperation(ctx, plan.ID, op.ID, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			if err := root.RenameNoReplace(op.SourcePath, op.TargetPath); err != nil {
				t.Fatal(err)
			}
			if test.changeFile {
				if err := os.WriteFile(target, []byte("tampered"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(target, originalInfo.Mode().Perm()); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(target, originalInfo.ModTime(), originalInfo.ModTime()); err != nil {
					t.Fatal(err)
				}
				entry, err := root.LstatEntry(op.TargetPath)
				if err != nil || !matchesRenameResult(entry.Fingerprint(), op.SourceStateJSON) {
					t.Fatalf("target no longer resembles approved snapshot: (%#v, %v)", entry, err)
				}
			}
			if err := service.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			recovered, err := service.Plan(ctx, plan.ID)
			if err != nil || recovered.Status != store.PlanFailed || recovered.Operations[0].Status != store.OperationPartial {
				t.Fatalf("recovered rename = (%#v, %v), want manual review", recovered, err)
			}
			tasks, err := metadata.ListTasks(ctx, store.TaskListQuery{Kind: "agent", Limit: 10})
			if err != nil || len(tasks.Tasks) != 1 || tasks.Tasks[0].Status != store.TaskPartial {
				t.Fatalf("recovered task state = (%#v, %v)", tasks.Tasks, err)
			}
		})
	}
}
