//go:build linux

// 本文件验证 Shell 对话、逐项审批与安全写回闭环。
package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
		replies: []string{`{"state":"ready","reply":"确认后会在隔离副本运行命令，并逐项展示文件变化。","goal":"创建只读归档目录并调整文档目录权限","operations":[{"action":"run_shell","filePath":"","targetPath":"","shellCommand":"mkdir -p sealed/nested && printf approved > sealed/nested/note.txt && chmod 0555 sealed/nested && chmod 0555 sealed && chmod 0700 docs","workingDir":"."}]}`},
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
	run, err := service.ShellRun(ctx, plan.ID)
	if err != nil || run.Status != store.ShellRunAwaitingApproval || run.Command == "" {
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
