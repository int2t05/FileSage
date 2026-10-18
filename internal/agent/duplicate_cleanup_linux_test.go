// 本文件验证重复文件清理的审批与回收站闭环。
package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"filesage/internal/config"
	"filesage/internal/coordination"
	"filesage/internal/fsroot"
	"filesage/internal/store"
	trashsvc "filesage/internal/trash"
)

func TestDuplicateCleanupRequiresSelectionAndMovesOnlyApprovedCopyToTrash(t *testing.T) {
	fixture := newDuplicateCleanupFixture(t)
	plan, err := fixture.conversations.CreateDuplicateCleanupPlan(fixture.ctx, fixture.scanID, DuplicateCleanupRequest{
		GroupID: 1, KeeperPath: "docs/keep.md", TrashPaths: []string{"docs/copy-a.md", "docs/copy-b.md"},
	})
	if err != nil || plan.Status != store.PlanAwaitingApproval || len(plan.Operations) != 2 || plan.Operations[0].Action != "trash_duplicate" {
		t.Fatalf("cleanup plan = (%#v, %v)", plan, err)
	}
	if _, err := fixture.plans.Propose(fixture.ctx, Proposal{
		RequestText: "清理重复文件", ScopePath: "docs", Action: "trash_duplicate",
		SourcePath: "docs/copy-a.md", TargetPath: "docs/keep.md", Reason: "手动提交",
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unbound cleanup proposal error = %v, want invalid", err)
	}

	approved := plan
	for _, operation := range plan.Operations {
		approved, err = fixture.plans.Decide(fixture.ctx, plan.ID, operation.ID, "approve", approved.Version)
		if err != nil {
			t.Fatalf("approve operation %s: %v", operation.ID, err)
		}
	}
	if approved.Status != store.PlanApproved {
		t.Fatalf("approved plan = %#v", approved)
	}
	completed, err := fixture.plans.Execute(fixture.ctx, plan.ID, approved.Version)
	if err != nil || completed.Status != store.PlanCompleted || completed.Operations[0].Status != store.OperationSucceeded {
		t.Fatalf("completed plan = (%#v, %v)", completed, err)
	}
	for _, path := range []string{"docs/copy-a.md", "docs/copy-b.md"} {
		if _, err := fixture.root.LstatEntry(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("selected duplicate %s remains at source: %v", path, err)
		}
	}
	if _, err := fixture.root.LstatEntry("docs/copy-c.md"); err != nil {
		t.Fatalf("unselected duplicate changed: %v", err)
	}
	if _, err := fixture.root.LstatEntry("docs/keep.md"); err != nil {
		t.Fatalf("keeper changed: %v", err)
	}
	page, err := fixture.trash.List(fixture.ctx, 100, "")
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("trash result = (%#v, %v)", page.Items, err)
	}
	paths := map[string]bool{}
	for _, item := range page.Items {
		paths[item.OriginalPath] = item.Status == string(store.TrashTrashed)
	}
	if !paths["docs/copy-a.md"] || !paths["docs/copy-b.md"] {
		t.Fatalf("selected trash paths = %#v", paths)
	}
}

func TestDuplicateCleanupApprovalInvalidatesChangedFiles(t *testing.T) {
	fixture := newDuplicateCleanupFixture(t)
	plan, err := fixture.conversations.CreateDuplicateCleanupPlan(fixture.ctx, fixture.scanID, DuplicateCleanupRequest{
		GroupID: 1, KeeperPath: "docs/keep.md", TrashPaths: []string{"docs/copy-a.md"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.rootDir, "docs", "copy-a.md"), []byte("changed bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	updated, err := fixture.plans.Decide(fixture.ctx, plan.ID, plan.Operations[0].ID, "approve", plan.Version)
	if !errors.Is(err, ErrConflict) || updated.Status != store.PlanInvalidated || updated.Operations[0].Approval != store.ApprovalInvalidated {
		t.Fatalf("changed cleanup approval = (%#v, %v), want invalidated", updated, err)
	}
}

type duplicateCleanupFixture struct {
	ctx           context.Context
	rootDir       string
	scanID        string
	root          *fsroot.Root
	trash         *trashsvc.Manager
	plans         *Service
	conversations *ConversationService
}

func newDuplicateCleanupFixture(t *testing.T) duplicateCleanupFixture {
	t.Helper()
	ctx := context.Background()
	rootDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootDir, "docs"), 0o700); err != nil {
		t.Fatal(err)
	}
	content := []byte("identical file contents")
	for _, name := range []string{"keep.md", "copy-a.md", "copy-b.md", "copy-c.md"} {
		if err := os.WriteFile(filepath.Join(rootDir, "docs", name), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := fsroot.Open(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	metadata, err := store.Open(ctx, filepath.Join(t.TempDir(), "filesage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = metadata.Close() })
	trashManager, err := trashsvc.Open(ctx, root, metadata)
	if err != nil {
		t.Fatal(err)
	}
	plans := New(metadata, root, coordination.New(), trashManager)
	conversations := NewConversationService(metadata, root, plans, config.AgentModelConfig{})
	t.Cleanup(conversations.Close)
	scanID := seedSuccessfulDuplicateCleanupScan(t, metadata, root, []string{"docs/keep.md", "docs/copy-a.md", "docs/copy-b.md", "docs/copy-c.md"})
	return duplicateCleanupFixture{ctx: ctx, rootDir: rootDir, scanID: scanID, root: root, trash: trashManager, plans: plans, conversations: conversations}
}

func seedSuccessfulDuplicateCleanupScan(t *testing.T, metadata *store.Store, root *fsroot.Root, paths []string) string {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	conversationID, scanID := "conversation-cleanup-e2e", "scan-cleanup-e2e"
	if err := metadata.CreateConversation(ctx, conversationID, "查找重复文件", now); err != nil {
		t.Fatal(err)
	}
	card := store.AnalysisCard{
		Operations: []store.AnalysisOperation{{Action: "find_duplicates"}},
		Goal:       "查找重复文件", DataTypes: []string{"任务描述与扫描范围"},
		Provider: "local", Model: "test", Destination: "local",
	}
	if err := metadata.FinishConversationTurn(ctx, conversationID, store.ConversationAwaitingConfirmation, "确认扫描", card, "", now); err != nil {
		t.Fatal(err)
	}
	if claimed, err := metadata.BeginConversationAnalysis(ctx, conversationID, now); err != nil || !claimed {
		t.Fatalf("begin analysis = (%t, %v)", claimed, err)
	}
	scan := store.DuplicateScan{
		ID: scanID, TaskID: scanID, ConversationID: conversationID, ScopePath: "docs",
		Status: store.DuplicateScanQueued, Phase: "enumerating", CreatedAt: now, UpdatedAt: now,
	}
	task := store.TaskRecord{
		ID: scanID, Kind: "duplicate_scan", Status: store.TaskQueued,
		RequestText: "查找重复文件", ScopePath: "docs", CreatedAt: now, UpdatedAt: now,
	}
	if err := metadata.CreateDuplicateScan(ctx, scan, task); err != nil {
		t.Fatal(err)
	}
	if err := metadata.StartDuplicateScan(ctx, scanID, now); err != nil {
		t.Fatal(err)
	}
	files := make([]store.DuplicateScanResultFile, 0, len(paths))
	for _, relativePath := range paths {
		entry, err := root.LstatEntry(relativePath)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, store.DuplicateScanResultFile{Path: relativePath, SizeBytes: entry.Size, ModifiedAt: entry.ModifiedAt})
	}
	groups := []store.DuplicateScanResultGroup{{SizeBytes: files[0].SizeBytes, Files: files}}
	progress := store.DuplicateScanProgress{Phase: "complete", EntriesVisited: len(paths), CandidateFiles: len(paths), FilesHashed: len(paths), BytesHashed: files[0].SizeBytes * int64(len(paths)), TotalBytes: files[0].SizeBytes * int64(len(paths))}
	if err := metadata.FinishDuplicateScan(ctx, scanID, string(store.DuplicateScanSucceeded), "", progress, groups, "扫描完成", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	return scanID
}
