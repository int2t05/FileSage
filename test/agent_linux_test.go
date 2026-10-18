// 本文件验证 Linux 上的 Agent rename 与重启恢复。
package test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"filesage/internal/agent"
	"filesage/internal/coordination"
	"filesage/internal/fsroot"
	"filesage/internal/store"
)

func TestAgentApprovedRenamePublishesOnlyAfterExplicitExecution(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	rootPath := filepath.Join(base, "managed")
	if err := os.Mkdir(rootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "before.txt"), []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := store.Open(ctx, filepath.Join(base, "state", "filesage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	defer root.Close()
	service := agent.New(metadata, root, coordination.New())

	plan, err := service.Propose(ctx, agent.Proposal{
		RequestText: "整理文件名", ScopePath: "", Action: "rename",
		SourcePath: "before.txt", TargetPath: "after.txt", Reason: "名称规范化",
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != store.PlanAwaitingApproval || len(plan.Operations) != 1 || plan.Operations[0].Status != store.OperationPending {
		t.Fatalf("proposal was not left pending: %+v", plan)
	}
	if _, err := service.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve"); err != nil {
		current, lstatErr := root.LstatEntry("before.txt")
		t.Fatalf("approve rename: %v; current snapshot=%+v lstat=%v expected=%s", err, current.Fingerprint(), lstatErr, plan.Operations[0].SourceStateJSON)
	}
	if _, err := os.Stat(filepath.Join(rootPath, "before.txt")); err != nil {
		t.Fatalf("approval executed the rename early: %v", err)
	}

	completed, err := service.Execute(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != store.PlanCompleted || completed.Operations[0].Status != store.OperationSucceeded {
		t.Fatalf("execution did not complete: %+v", completed)
	}
	content, err := os.ReadFile(filepath.Join(rootPath, "after.txt"))
	if err != nil || string(content) != "payload" {
		t.Fatalf("target content = %q, %v", content, err)
	}
	if _, err := os.Stat(filepath.Join(rootPath, "before.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source still exists: %v", err)
	}
	audit, err := metadata.AuditForTask(ctx, plan.TaskID)
	if err != nil || len(audit) != 5 {
		t.Fatalf("audit events = %d, %v; want creation, approval, claim, start, completion", len(audit), err)
	}
}

func TestAgentConcurrentExecutionCannotRenameTwice(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	rootPath := filepath.Join(base, "managed")
	if err := os.Mkdir(rootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "source.txt"), []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	metadata, err := store.Open(ctx, filepath.Join(base, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	service := agent.New(metadata, root, coordination.New())
	plan, err := service.Propose(ctx, agent.Proposal{
		RequestText: "rename", Action: "rename", SourcePath: "source.txt", TargetPath: "target.txt", Reason: "整理",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve"); err != nil {
		t.Fatal(err)
	}

	var wait sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := service.Execute(ctx, plan.ID)
			results <- err
		}()
	}
	wait.Wait()
	close(results)
	succeeded, conflicted := 0, 0
	for err := range results {
		if err == nil {
			succeeded++
		} else if errors.Is(err, agent.ErrConflict) {
			conflicted++
		} else {
			t.Fatalf("concurrent execute error = %v", err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("concurrent execute results: succeeded=%d conflicted=%d", succeeded, conflicted)
	}
	plan, err = metadata.Plan(ctx, plan.ID)
	if err != nil || plan.Status != store.PlanCompleted {
		t.Fatalf("final plan = (%+v, %v)", plan, err)
	}
}

func TestAgentInvalidatesChangedSourceAndOccupiedTarget(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	rootPath := filepath.Join(base, "managed")
	if err := os.Mkdir(rootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name, value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(rootPath, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("source.txt", "first")
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	metadata, err := store.Open(ctx, filepath.Join(base, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	service := agent.New(metadata, root, coordination.New())

	stale, err := service.Propose(ctx, agent.Proposal{RequestText: "rename", Action: "rename", SourcePath: "source.txt", TargetPath: "stale.txt", Reason: "整理"})
	if err != nil {
		t.Fatal(err)
	}
	write("source.txt", "changed after proposal")
	invalidated, err := service.Decide(ctx, stale.ID, stale.Operations[0].ID, "approve")
	if !errors.Is(err, agent.ErrConflict) {
		t.Fatalf("stale approval error = %v, want conflict", err)
	}
	if invalidated.Status != store.PlanInvalidated || invalidated.Operations[0].Approval != store.ApprovalInvalidated {
		t.Fatalf("changed source was not invalidated: %+v", invalidated)
	}

	fresh, err := service.Propose(ctx, agent.Proposal{RequestText: "rename", Action: "rename", SourcePath: "source.txt", TargetPath: "occupied.txt", Reason: "整理"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Decide(ctx, fresh.ID, fresh.Operations[0].ID, "approve"); err != nil {
		t.Fatal(err)
	}
	write("occupied.txt", "preserve")
	invalidated, err = service.Execute(ctx, fresh.ID)
	if !errors.Is(err, agent.ErrConflict) {
		t.Fatalf("occupied target execute error = %v, want conflict", err)
	}
	if invalidated.Status != store.PlanInvalidated {
		t.Fatalf("occupied target plan status = %q", invalidated.Status)
	}
	content, err := os.ReadFile(filepath.Join(rootPath, "occupied.txt"))
	if err != nil || string(content) != "preserve" {
		t.Fatalf("occupied target was overwritten: %q, %v", content, err)
	}
}

func TestAgentInvalidatesSameInodeContentChangeAfterMtimeRestore(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	rootPath := filepath.Join(base, "managed")
	if err := os.Mkdir(rootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(rootPath, "source.txt")
	if err := os.WriteFile(sourcePath, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	metadata, err := store.Open(ctx, filepath.Join(base, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	service := agent.New(metadata, root, coordination.New())
	plan, err := service.Propose(ctx, agent.Proposal{
		RequestText: "rename", Action: "rename", SourcePath: "source.txt", TargetPath: "target.txt", Reason: "内容核对",
	})
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.Stat(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := os.WriteFile(sourcePath, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(sourcePath, original.ModTime(), original.ModTime()); err != nil {
		t.Fatal(err)
	}
	updated, err := service.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve")
	if !errors.Is(err, agent.ErrConflict) {
		t.Fatalf("same-inode content change approval error = %v, want conflict", err)
	}
	if updated.Status != store.PlanInvalidated {
		t.Fatalf("same-inode content change plan status = %q, want invalidated", updated.Status)
	}
}

func TestAgentRejectsDirectoriesSymlinksAndScopeEscape(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	rootPath := filepath.Join(base, "managed")
	if err := os.MkdirAll(filepath.Join(rootPath, "scope"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "scope", "file.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(rootPath, "scope", "folder"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("file.txt", filepath.Join(rootPath, "scope", "link.txt")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	metadata, err := store.Open(ctx, filepath.Join(base, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	service := agent.New(metadata, root, coordination.New())
	baseProposal := agent.Proposal{RequestText: "rename", ScopePath: "scope", Action: "rename", TargetPath: "scope/renamed.txt", Reason: "整理"}
	for _, source := range []string{"scope/folder", "scope/link.txt", "outside.txt"} {
		proposal := baseProposal
		proposal.SourcePath = source
		if _, err := service.Propose(ctx, proposal); err == nil {
			t.Errorf("Propose accepted unsafe source %q", source)
		}
	}
}

func TestAgentRestartRecoveryNeverBlindlyReplays(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	rootPath := filepath.Join(base, "managed")
	if err := os.Mkdir(rootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "source.txt"), []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(base, "state.db")
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	service := agent.New(metadata, root, coordination.New())
	plan, err := service.Propose(ctx, agent.Proposal{RequestText: "rename", Action: "rename", SourcePath: "source.txt", TargetPath: "target.txt", Reason: "整理"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve"); err != nil {
		t.Fatal(err)
	}
	if err := metadata.StartAgentOperation(ctx, plan.ID, plan.Operations[0].ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := metadata.Close(); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}

	metadata, err = store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	root, err = fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	service = agent.New(metadata, root, coordination.New())
	if err := service.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	recovered, err := metadata.Plan(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != store.PlanApproved || recovered.Operations[0].Status != store.OperationPending {
		t.Fatalf("untouched source recovery state = %+v", recovered)
	}
	if _, err := os.Stat(filepath.Join(rootPath, "target.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery blindly executed rename: %v", err)
	}
	if err := metadata.Close(); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAgentRecoveryMarksCompletedRenameForManualReview(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	rootPath := filepath.Join(base, "managed")
	if err := os.Mkdir(rootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "source.txt"), []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(base, "state.db")
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	service := agent.New(metadata, root, coordination.New())
	plan, err := service.Propose(ctx, agent.Proposal{
		RequestText: "rename", Action: "rename", SourcePath: "source.txt", TargetPath: "target.txt", Reason: "整理",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve"); err != nil {
		t.Fatal(err)
	}
	if err := metadata.StartAgentOperation(ctx, plan.ID, plan.Operations[0].ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := root.RenameNoReplace("source.txt", "target.txt"); err != nil {
		t.Fatal(err)
	}
	if err := metadata.Close(); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}

	metadata, err = store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	root, err = fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	service = agent.New(metadata, root, coordination.New())
	if err := service.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	recovered, err := metadata.Plan(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != store.PlanFailed || recovered.Operations[0].Status != store.OperationPartial {
		t.Fatalf("recovered rename without saved digest = %+v; want manual review", recovered)
	}
}

func TestAgentRecoveryMarksUncertainRenameForManualReview(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	rootPath := filepath.Join(base, "managed")
	if err := os.Mkdir(rootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "source.txt"), []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(base, "state.db")
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	service := agent.New(metadata, root, coordination.New())
	plan, err := service.Propose(ctx, agent.Proposal{
		RequestText: "rename", Action: "rename", SourcePath: "source.txt", TargetPath: "target.txt", Reason: "整理",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve"); err != nil {
		t.Fatal(err)
	}
	if err := metadata.StartAgentOperation(ctx, plan.ID, plan.Operations[0].ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := root.RenameNoReplace("source.txt", "elsewhere.txt"); err != nil {
		t.Fatal(err)
	}
	if err := metadata.Close(); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}

	metadata, err = store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	root, err = fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	service = agent.New(metadata, root, coordination.New())
	if err := service.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	recovered, err := metadata.Plan(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != store.PlanFailed || recovered.Operations[0].Status != store.OperationPartial {
		t.Fatalf("uncertain recovery was not marked for review: %+v", recovered)
	}
}
