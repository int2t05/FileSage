// 本文件验证 Agent 对话迁移、恢复和摘要边界。
package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestAnalysisFileSizeLimitMatchesSupportedFormats(t *testing.T) {
	for _, file := range []struct {
		path  string
		limit int64
	}{
		{path: "notes.md", limit: MaxAnalysisTextFileBytes},
		{path: "report.pdf", limit: MaxAnalysisDocumentFileBytes},
		{path: "contract.docx", limit: MaxAnalysisDocumentFileBytes},
		{path: "slides.pptx", limit: MaxAnalysisDocumentFileBytes},
	} {
		if limit := AnalysisFileSizeLimit(file.path); limit != file.limit {
			t.Errorf("AnalysisFileSizeLimit(%q) = %d, want %d", file.path, limit, file.limit)
		}
	}
	for _, file := range []string{"sheet.xlsx", "image.png", "notes.txt.exe"} {
		if limit := AnalysisFileSizeLimit(file); limit != 0 {
			t.Errorf("AnalysisFileSizeLimit(%q) = %d, want 0", file, limit)
		}
	}
}

func TestConversationPersistenceRecoveryAndListSummary(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "filesage.sqlite")
	db, err := Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	createdAt := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	if err := db.CreateConversation(ctx, "conversation-1", "请整理目录里的 notes.md", createdAt); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishConversationTurn(ctx, "conversation-1", ConversationAwaitingConfirmation, "将读取单个文件全文，请确认。", AnalysisCard{
		Goal: "依据正文标题重命名", FilePath: "notes.md", Provider: "test", Model: "model", Destination: "local",
	}, "", createdAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	conversation, err := db.Conversation(ctx, "conversation-1")
	if err != nil || conversation.State != ConversationAwaitingConfirmation || conversation.Analysis == nil || len(conversation.Messages) != 2 {
		t.Fatalf("Conversation() = (%#v, %v)", conversation, err)
	}
	list, hasMore, err := db.ListConversations(ctx, 20, 0)
	if err != nil || hasMore || len(list) != 1 {
		t.Fatalf("ListConversations() = (%#v, %t, %v)", list, hasMore, err)
	}
	if list[0].Title != "依据正文标题重命名" || list[0].UpdatedAt.IsZero() {
		t.Fatalf("conversation summary = %#v", list[0])
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conversation, err = db.Conversation(ctx, "conversation-1")
	if err != nil || conversation.State != ConversationAwaitingConfirmation || len(conversation.Messages) != 2 {
		t.Fatalf("reloaded Conversation() = (%#v, %v)", conversation, err)
	}
	if claimed, err := db.BeginConversationAnalysis(ctx, "conversation-1", time.Now()); err != nil || !claimed {
		t.Fatalf("BeginConversationAnalysis() = (%t, %v)", claimed, err)
	}
	conversation, err = db.Conversation(ctx, "conversation-1")
	if err != nil || conversation.State != ConversationAnalyzing || conversation.Analysis == nil {
		t.Fatalf("analysis claim lost its card: (%#v, %v)", conversation, err)
	}
	if claimed, err := db.BeginConversationAnalysis(ctx, "conversation-1", time.Now()); err != nil || claimed {
		t.Fatalf("duplicate analysis claim = (%t, %v)", claimed, err)
	}
	if err := db.RecoverConversations(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	conversation, err = db.Conversation(ctx, "conversation-1")
	if err != nil || conversation.State != ConversationFailed || conversation.Error != "service_restarted" || conversation.Analysis == nil {
		t.Fatalf("recovered Conversation() = (%#v, %v)", conversation, err)
	}
}

// TestConversationSummaryIncludesCurrentTaskStatus 验证摘要读取关联任务的最新状态。
func TestConversationSummaryIncludesCurrentTaskStatus(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "filesage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := db.CreateConversation(ctx, "conversation-task", "整理文件", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := db.PutTask(ctx, TaskRecord{ID: "task-current", Kind: "agent", Status: TaskSucceeded}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `INSERT INTO plans (id, task_id, version, status, data_scope_json, created_at_ns, updated_at_ns) VALUES (?, ?, 1, 'completed', '{}', 1, 1)`, "plan-current", "task-current"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `UPDATE agent_conversations SET state = ?, task_id = ?, plan_id = ? WHERE id = ?`, ConversationAwaitingApproval, "task-current", "plan-current", "conversation-task"); err != nil {
		t.Fatal(err)
	}

	summaries, _, err := db.ListConversations(ctx, 20, 0)
	if err != nil || len(summaries) != 1 || summaries[0].TaskStatus != TaskSucceeded {
		t.Fatalf("ListConversations() = (%#v, %v)", summaries, err)
	}
}

func TestClarifyingConversationCanFinishWithoutAnalysisCard(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "filesage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.CreateConversation(ctx, "conversation-2", "整理文件", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishConversationTurn(ctx, "conversation-2", ConversationClarifying, "请提供文件相对路径。", AnalysisCard{}, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	conversation, err := db.Conversation(ctx, "conversation-2")
	if err != nil || conversation.State != ConversationClarifying || conversation.Analysis != nil || len(conversation.Messages) != 2 {
		t.Fatalf("clarifying conversation = (%#v, %v)", conversation, err)
	}
}

func TestDirectoryAnalysisCardPersistsWithoutFileContent(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "filesage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.CreateConversation(ctx, "conversation-directory", "创建照片归档目录", time.Now()); err != nil {
		t.Fatal(err)
	}
	card := AnalysisCard{
		Action: "create_directory", Goal: "按年份归档照片", TargetPath: "照片/2025",
		DataTypes: []string{"任务描述与目标路径"}, Provider: "local", Model: "test", Destination: "local",
	}
	if err := db.FinishConversationTurn(ctx, "conversation-directory", ConversationAwaitingConfirmation, "请确认创建路径。", card, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	conversation, err := db.Conversation(ctx, "conversation-directory")
	if err != nil || conversation.Analysis == nil {
		t.Fatalf("Conversation() = (%#v, %v)", conversation, err)
	}
	if got := conversation.Analysis; got.Action != "create_directory" || got.TargetPath != "照片/2025" || got.FilePath != "" || len(got.DataTypes) != 1 || got.DataTypes[0] != "任务描述与目标路径" {
		t.Fatalf("directory analysis card = %#v", got)
	}
}

func TestMetadataConfirmationCardPersistsTypedTargets(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "filesage.sqlite")
	db, err := Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	uid, gid := uint32(0), uint32(0)
	if err := db.CreateConversation(ctx, "conversation-owner", "设置 notes.txt 的 UID", time.Now()); err != nil {
		t.Fatal(err)
	}
	card := AnalysisCard{
		Action: "set_owner", Goal: "设置文件属主", FilePath: "notes.txt", RequestedOwnerUID: &uid, RequestedGroupGID: &gid,
		DataTypes: []string{"任务描述、文件路径与目标属主"}, Provider: "local", Model: "test", Destination: "local",
	}
	if err := db.FinishConversationTurn(ctx, "conversation-owner", ConversationAwaitingConfirmation, "请确认 UID。", card, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conversation, err := db.Conversation(ctx, "conversation-owner")
	if err != nil || conversation.Analysis == nil || conversation.Analysis.RequestedOwnerUID == nil || *conversation.Analysis.RequestedOwnerUID != 0 || conversation.Analysis.RequestedGroupGID == nil || *conversation.Analysis.RequestedGroupGID != 0 {
		t.Fatalf("restored metadata confirmation card = (%#v, %v)", conversation.Analysis, err)
	}
}

func TestConversationPlanMustContainEveryConfirmedOperation(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "filesage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC()
	if err := db.CreateConversation(ctx, "conversation-multi", "建立两个目录", now); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishConversationTurn(ctx, "conversation-multi", ConversationAwaitingConfirmation, "请确认两个目录。", AnalysisCard{
		Goal: "整理目录", Operations: []AnalysisOperation{
			{Action: "create_directory", TargetPath: "parent/one"},
			{Action: "create_directory", TargetPath: "parent/two"},
		}, Provider: "test", Model: "model", Destination: "local",
	}, "", now); err != nil {
		t.Fatal(err)
	}
	if claimed, err := db.BeginConversationAnalysis(ctx, "conversation-multi", now); err != nil || !claimed {
		t.Fatalf("BeginConversationAnalysis() = (%t, %v)", claimed, err)
	}
	operation := OperationRecord{
		ID: "operation-one", Ordinal: 0, Action: "create_directory", SourcePath: "parent", TargetPath: "parent/one",
		Approval: ApprovalPending, Status: OperationPending, CreatedAt: now, UpdatedAt: now,
	}
	task := TaskRecord{
		ID: "task-truncated", Kind: "agent", Status: TaskAwaitingApproval, RequestText: "建立两个目录",
		ModelProvider: "test", ModelName: "model", CreatedAt: now, UpdatedAt: now,
	}
	plan := PlanRecord{
		ID: "plan-truncated", TaskID: task.ID, Version: 1, Status: PlanAwaitingApproval,
		ModelProvider: "test", ModelName: "model", CreatedAt: now, UpdatedAt: now, Operations: []OperationRecord{operation},
	}
	audit := AuditRecord{
		TaskID: task.ID, PlanID: plan.ID, OperationID: operation.ID, EventType: "plan_created",
		Actor: "agent", Outcome: "awaiting_approval", CreatedAt: now,
	}
	if err := db.CreateConversationTaskPlan(ctx, "conversation-multi", task, plan, audit, "已生成待审批方案。"); err != ErrConversationConflict {
		t.Fatalf("truncated plan error = %v, want ErrConversationConflict", err)
	}
	if _, err := db.Task(ctx, task.ID); err != ErrNotFound {
		t.Fatalf("truncated plan task was partially inserted: error=%v", err)
	}
}

func TestCreateSymlinkIsLimitedToOneAnalysisOperation(t *testing.T) {
	symlink := AnalysisOperation{Action: "create_symlink", FilePath: "source.txt", TargetPath: "link"}
	if !ValidAnalysisOperations([]AnalysisOperation{symlink}) {
		t.Fatal("single create_symlink operation should be valid")
	}
	if ValidAnalysisOperations([]AnalysisOperation{symlink, {Action: "create_directory", TargetPath: "folder"}}) {
		t.Fatal("mixed operations containing create_symlink should be rejected")
	}
	if ValidAnalysisOperations([]AnalysisOperation{symlink, {Action: "create_symlink", FilePath: "source.txt", TargetPath: "other-link"}}) {
		t.Fatal("multiple create_symlink operations should be rejected")
	}
}

func TestPermanentDeleteAnalysisRequiresSingleSafeSourcePath(t *testing.T) {
	operation := AnalysisOperation{Action: "permanent_delete", FilePath: "资料/旧文件.pdf"}
	if !ValidAnalysisOperations([]AnalysisOperation{operation}) {
		t.Fatal("a single permanent-delete operation with a safe source path should be valid")
	}
	for _, invalid := range []AnalysisOperation{
		{Action: "permanent_delete", FilePath: "../外部/旧文件.pdf"},
		{Action: "permanent_delete", FilePath: "资料/旧文件.pdf", TargetPath: "恢复/旧文件.pdf"},
		{Action: "permanent_delete", FilePath: "资料/旧文件.pdf", RequestedMode: new(uint32)},
	} {
		if ValidAnalysisOperations([]AnalysisOperation{invalid}) {
			t.Fatalf("invalid permanent-delete operation was accepted: %#v", invalid)
		}
	}
}
