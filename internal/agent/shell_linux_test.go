//go:build linux

// 本文件验证 Shell 对话、逐项审批与安全写回闭环。
package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filesage/internal/coordination"
	"filesage/internal/shellstage"
	"filesage/internal/store"
	trashsvc "filesage/internal/trash"
)

func TestShellConversationReviewsChangesBeforeWritingAndAppliesDirectoryModesLast(t *testing.T) {
	ctx := context.Background()
	if err := shellstage.CheckAvailability(ctx); err != nil {
		t.Skipf("Shell 沙箱不可用: %v", err)
	}
	rootPath := t.TempDir()
	defer func() {
		_ = os.Chmod(filepath.Join(rootPath, "sealed", "nested"), 0o700)
		_ = os.Chmod(filepath.Join(rootPath, "sealed"), 0o700)
	}()
	docsPath := filepath.Join(rootPath, "docs")
	if err := os.Mkdir(docsPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(docsPath, "existing.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata, root, _ := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer metadata.Close()
	defer root.Close()
	trash, err := trashsvc.Open(ctx, root, metadata)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewWithStaging(metadata, root, coordination.New(), filepath.Join(t.TempDir(), "text"), filepath.Join(t.TempDir(), "shell"), trash)
	if err != nil {
		t.Fatal(err)
	}
	model := &scriptedConversationModel{
		status:  ModelStatus{Available: true, Provider: "test-provider", Model: "test-model", Destination: "local"},
		replies: []string{`{"state":"ready","reply":"确认后会在隔离副本运行命令，并逐项展示文件变化。","goal":"创建只读归档目录并调整文档目录权限","operations":[{"action":"run_shell","filePath":"","targetPath":"","shellCommand":"cat docs/existing.txt && mkdir -p sealed/nested && printf approved > sealed/nested/note.txt && chmod 0555 sealed/nested && chmod 0555 sealed && chmod 0700 docs","workingDir":"."}]}`},
	}
	conversationService := &ConversationService{store: metadata, root: root, plans: service, model: model}
	conversation, err := conversationService.Create(ctx, "在隔离环境创建只读归档目录，并把 docs 设为仅所有者可访问")
	if err != nil || conversation.State != store.ConversationAwaitingConfirmation {
		t.Fatalf("Create() = (%#v, %v)", conversation, err)
	}
	if _, err := os.Stat(filepath.Join(rootPath, "sealed")); !os.IsNotExist(err) {
		t.Fatalf("未确认分析就修改了真实目录: %v", err)
	}
	conversation, err = conversationService.ConfirmAnalysis(ctx, conversation.ID)
	if err != nil || conversation.State != store.ConversationAwaitingApproval || conversation.PlanID == "" {
		t.Fatalf("ConfirmAnalysis() = (%#v, %v)", conversation, err)
	}
	plan, err := metadata.Plan(ctx, conversation.PlanID)
	if err != nil || plan.Status != store.PlanAwaitingApproval {
		t.Fatalf("Plan() = (%#v, %v)", plan, err)
	}
	if len(plan.Operations) != 1 || plan.Operations[0].Reason != "创建只读归档目录并调整文档目录权限" {
		t.Fatalf("Shell 步骤用途 = %#v", plan.Operations)
	}
	run, err := service.ShellRun(ctx, plan.ID)
	if err != nil || run.Status != store.ShellRunAwaitingApproval || run.Command == "" || !run.OutputDiscarded {
		t.Fatalf("ShellRun() = (%#v, %v)", run, err)
	}
	page, err := service.ShellChanges(ctx, plan.ID, store.MaxShellChangePageSize, 0)
	if err != nil || page.Total != 4 {
		t.Fatalf("ShellChanges() = (%#v, %v), want four reviewed changes", page, err)
	}
	paths := make(map[string]store.ShellChangeRecord, len(page.Changes))
	for _, change := range page.Changes {
		paths[change.Path] = change
	}
	for _, relative := range []string{"sealed", "sealed/nested", "sealed/nested/note.txt", "docs"} {
		if _, exists := paths[relative]; !exists {
			t.Fatalf("review plan omitted %q: %#v", relative, paths)
		}
	}
	preview, err := service.PreviewShellChange(ctx, plan.ID, paths["sealed/nested/note.txt"].ID)
	if err != nil || !strings.Contains(preview.Diff, "approved") {
		t.Fatalf("PreviewShellChange() = (%#v, %v)", preview, err)
	}
	if _, err := os.Stat(filepath.Join(rootPath, "sealed")); !os.IsNotExist(err) {
		t.Fatalf("逐项审核前已写入真实目录: %v", err)
	}
	for _, change := range page.Changes {
		plan, err = service.DecideShellChange(ctx, plan.ID, change.ID, "approve", plan.Version)
		if err != nil {
			t.Fatalf("approve %q: %v", change.Path, err)
		}
	}
	if plan.Status != store.PlanApproved {
		t.Fatalf("plan after per-item approval = %q", plan.Status)
	}
	if _, err := os.Stat(filepath.Join(rootPath, "sealed")); !os.IsNotExist(err) {
		t.Fatalf("批准清单后再次确认执行前已写入真实目录: %v", err)
	}
	plan, err = service.Execute(ctx, plan.ID, plan.Version)
	if err != nil || plan.Status != store.PlanCompleted {
		t.Fatalf("Execute() = (%#v, %v)", plan, err)
	}
	for _, check := range []struct {
		path string
		mode os.FileMode
	}{
		{path: "sealed", mode: 0o555},
		{path: filepath.Join("sealed", "nested"), mode: 0o555},
		{path: "docs", mode: 0o700},
	} {
		info, err := os.Stat(filepath.Join(rootPath, check.path))
		if err != nil || info.Mode().Perm() != check.mode {
			t.Fatalf("directory %q mode = (%v, %v), want %04o", check.path, info, err, check.mode)
		}
	}
	if content, err := os.ReadFile(filepath.Join(rootPath, "sealed", "nested", "note.txt")); err != nil || string(content) != "approved" {
		t.Fatalf("approved content = (%q, %v)", content, err)
	}
	if content, err := os.ReadFile(filepath.Join(docsPath, "existing.txt")); err != nil || string(content) != "keep" {
		t.Fatalf("unmodified content = (%q, %v)", content, err)
	}
}

func TestBypassReviewsAndExecutesShellThroughOneToolStep(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := shellstage.CheckAvailability(ctx); err != nil {
		t.Skipf("Shell 沙箱不可用: %v", err)
	}
	rootPath := t.TempDir()
	metadata, root, _ := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer metadata.Close()
	defer root.Close()
	trash, err := trashsvc.Open(ctx, root, metadata)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewWithStaging(metadata, root, coordination.New(), filepath.Join(t.TempDir(), "text"), filepath.Join(t.TempDir(), "shell"), trash)
	if err != nil {
		t.Fatal(err)
	}
	model := &scriptedConversationModel{
		status:  ModelStatus{Available: true, Provider: "test-provider", Model: "test-model", Destination: "local"},
		replies: []string{`{"state":"ready","reply":"将在隔离目录创建测试文件。","goal":"创建 Shell 工具验收文件","operations":[{"action":"run_shell","filePath":"","targetPath":"","shellCommand":"printf 'shell-bypass-ok\\n' > shell-bypass.txt","workingDir":"."}]}`},
	}
	conversationService := &ConversationService{store: metadata, root: root, plans: service, model: model}
	conversation, err := conversationService.Create(ctx, "创建一个 Shell 工具验收文件")
	if err != nil || conversation.State != store.ConversationAwaitingConfirmation {
		t.Fatalf("Create() = (%#v, %v)", conversation, err)
	}
	conversation, err = conversationService.ConfirmAnalysis(ctx, conversation.ID)
	if err != nil || conversation.State != store.ConversationAwaitingApproval || conversation.PlanID == "" {
		t.Fatalf("ConfirmAnalysis() = (%#v, %v)", conversation, err)
	}
	if _, err := os.Stat(filepath.Join(rootPath, "shell-bypass.txt")); !os.IsNotExist(err) {
		t.Fatalf("计划审核前已修改管理目录: %v", err)
	}
	plan, err := metadata.Plan(ctx, conversation.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	completed, err := service.StartPlan(ctx, plan.ID, plan.Version, "bypass", false)
	if err != nil || completed.Status != store.PlanCompleted || len(completed.Operations) != 1 || completed.Operations[0].Action != "run_shell" || completed.Operations[0].Status != store.OperationSucceeded {
		t.Fatalf("StartPlan() = (%#v, %v)", completed, err)
	}
	if content, err := os.ReadFile(filepath.Join(rootPath, "shell-bypass.txt")); err != nil || string(content) != "shell-bypass-ok\n" {
		t.Fatalf("bypass tool result = (%q, %v)", content, err)
	}
	changes, err := service.ShellChanges(ctx, plan.ID, 10, 0)
	if err != nil || len(changes.Changes) != 1 {
		t.Fatalf("ShellChanges() = (%#v, %v)", changes, err)
	}
	preview, err := service.PreviewShellChange(ctx, plan.ID, changes.Changes[0].ID)
	if err != nil || !strings.Contains(preview.Diff, "+shell-bypass-ok") {
		t.Fatalf("completed addition diff = (%#v, %v)", preview, err)
	}
}

func TestShellExecutionInvalidatesPlanAfterExternalFileChange(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := shellstage.CheckAvailability(ctx); err != nil {
		t.Skipf("Shell 沙箱不可用: %v", err)
	}
	rootPath := t.TempDir()
	target := filepath.Join(rootPath, "note.txt")
	if err := os.WriteFile(target, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata, root, _ := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer metadata.Close()
	defer root.Close()
	trash, err := trashsvc.Open(ctx, root, metadata)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewWithStaging(metadata, root, coordination.New(), filepath.Join(t.TempDir(), "text"), filepath.Join(t.TempDir(), "shell"), trash)
	if err != nil {
		t.Fatal(err)
	}
	conversationID := "0123456789abcdef0123456789abcdef"
	if err := metadata.CreateConversation(ctx, conversationID, "更新测试说明文件", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := metadata.FinishConversationTurn(ctx, conversationID, store.ConversationAwaitingConfirmation, "请确认隔离命令。", store.AnalysisCard{
		Goal: "更新测试说明文件", Provider: "test-provider", Model: "test-model", Destination: "local",
		Operations: []store.AnalysisOperation{{Action: "run_shell", ShellCommand: "printf 'approved' > note.txt", WorkingDir: "."}},
	}, "", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if claimed, err := metadata.BeginConversationAnalysis(ctx, conversationID, time.Now().UTC(), false); err != nil || !claimed {
		t.Fatalf("BeginConversationAnalysis() = (%t, %v)", claimed, err)
	}

	plan, err := service.ProposeShell(ctx, ShellProposal{
		RequestText: "更新测试说明文件", ConversationID: conversationID, Command: "printf 'approved' > note.txt", WorkingDirectory: ".",
		ModelProvider: "test-provider", ModelName: "test-model",
	})
	if err != nil || plan.Status != store.PlanAwaitingApproval {
		t.Fatalf("ProposeShell() = (%#v, %v)", plan, err)
	}
	page, err := service.ShellChanges(ctx, plan.ID, 10, 0)
	if err != nil || len(page.Changes) != 1 {
		t.Fatalf("ShellChanges() = (%#v, %v)", page, err)
	}
	plan, err = service.DecideShellChange(ctx, plan.ID, page.Changes[0].ID, "approve", plan.Version)
	if err != nil || plan.Status != store.PlanApproved {
		t.Fatalf("DecideShellChange() = (%#v, %v)", plan, err)
	}

	original, err := root.LstatEntry("note.txt")
	if err != nil || !original.ChangeTimeAvailable {
		t.Fatalf("original file change time = (%#v, %v), want available", original, err)
	}
	if err := os.WriteFile(target, []byte("update"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(target, original.ModifiedAt, original.ModifiedAt); err != nil {
		t.Fatal(err)
	}

	updated, err := service.Execute(ctx, plan.ID, plan.Version)
	if !errors.Is(err, ErrConflict) || updated.Status != store.PlanInvalidated {
		t.Fatalf("Execute() = (%#v, %v), want invalidated plan and conflict", updated, err)
	}
	if content, err := os.ReadFile(target); err != nil || string(content) != "update" {
		t.Fatalf("external content changed to (%q, %v)", content, err)
	}
	run, err := service.ShellRun(ctx, plan.ID)
	if err != nil || run.Status != store.ShellRunInvalidated {
		t.Fatalf("ShellRun() = (%#v, %v), want invalidated", run, err)
	}
	changes, err := service.ShellChanges(ctx, plan.ID, 10, 0)
	if err != nil || len(changes.Changes) != 1 || changes.Changes[0].Approval != store.ApprovalInvalidated {
		t.Fatalf("invalidated Shell changes = (%#v, %v)", changes, err)
	}
}
