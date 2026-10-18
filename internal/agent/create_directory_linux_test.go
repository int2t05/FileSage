//go:build linux

// 本文件验证 Agent 新建目录的审批、并发复核与恢复语义。
package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"filesage/internal/store"
)

func TestCreateDirectoryRequiresApprovalThenExplicitExecution(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "documents"), 0o755); err != nil {
		t.Fatal(err)
	}
	metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()

	plan := proposeDirectory(t, ctx, service, "documents", "documents/contracts", "documents")
	if len(plan.Operations) != 1 || plan.Operations[0].Action != "create_directory" || plan.Operations[0].SourcePath != "documents" || plan.Operations[0].TargetPath != "documents/contracts" {
		t.Fatalf("directory proposal = %#v", plan.Operations)
	}
	target := filepath.Join(rootPath, "documents", "contracts")
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("proposal created its target: %v", err)
	}
	if _, err := service.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("approval executed the operation: %v", err)
	}
	completed, err := service.Execute(ctx, plan.ID)
	if err != nil || completed.Status != store.PlanCompleted || completed.Operations[0].Status != store.OperationSucceeded {
		t.Fatalf("Execute() = (%#v, %v)", completed, err)
	}
	info, err := os.Stat(target)
	if err != nil || !info.IsDir() {
		t.Fatalf("created target = (%v, %v)", info, err)
	}
}

func TestCreateDirectorySupportsManagedRootAsParent(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()

	plan := proposeDirectory(t, ctx, service, "", "root-level", "")
	if _, err := service.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve"); err != nil {
		t.Fatal(err)
	}
	completed, err := service.Execute(ctx, plan.ID)
	if err != nil || completed.Status != store.PlanCompleted {
		t.Fatalf("Execute() = (%#v, %v)", completed, err)
	}
	if info, err := os.Stat(filepath.Join(rootPath, "root-level")); err != nil || !info.IsDir() {
		t.Fatalf("root-level directory = (%v, %v)", info, err)
	}
}

func TestCreateDirectoryRejectsParentAndTargetConflicts(t *testing.T) {
	ctx := context.Background()
	t.Run("parent identity changed before approval", func(t *testing.T) {
		rootPath := t.TempDir()
		if err := os.Mkdir(filepath.Join(rootPath, "parent"), 0o755); err != nil {
			t.Fatal(err)
		}
		metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
		defer root.Close()
		defer metadata.Close()
		plan := proposeDirectory(t, ctx, service, "parent", "parent/new", "")
		if err := os.Rename(filepath.Join(rootPath, "parent"), filepath.Join(rootPath, "old-parent")); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(rootPath, "parent"), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := service.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve"); !errors.Is(err, ErrConflict) {
			t.Fatalf("Decide() error = %v, want ErrConflict", err)
		}
		if _, err := os.Stat(filepath.Join(rootPath, "parent", "new")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("conflict created target: %v", err)
		}
	})

	t.Run("target appears before approval", func(t *testing.T) {
		rootPath := t.TempDir()
		if err := os.Mkdir(filepath.Join(rootPath, "parent"), 0o755); err != nil {
			t.Fatal(err)
		}
		metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
		defer root.Close()
		defer metadata.Close()
		plan := proposeDirectory(t, ctx, service, "parent", "parent/new", "")
		target := filepath.Join(rootPath, "parent", "new")
		if err := os.WriteFile(target, []byte("preserve"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := service.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve"); !errors.Is(err, ErrConflict) {
			t.Fatalf("Decide() error = %v, want ErrConflict", err)
		}
		content, err := os.ReadFile(target)
		if err != nil || string(content) != "preserve" {
			t.Fatalf("conflicting target changed: %q (%v)", content, err)
		}
	})

	t.Run("target appears after approval", func(t *testing.T) {
		rootPath := t.TempDir()
		if err := os.Mkdir(filepath.Join(rootPath, "parent"), 0o755); err != nil {
			t.Fatal(err)
		}
		metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
		defer root.Close()
		defer metadata.Close()
		plan := proposeDirectory(t, ctx, service, "parent", "parent/new", "")
		if _, err := service.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve"); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(rootPath, "parent", "new")
		if err := os.WriteFile(target, []byte("preserve"), 0o600); err != nil {
			t.Fatal(err)
		}
		updated, err := service.Execute(ctx, plan.ID)
		if !errors.Is(err, ErrConflict) || updated.Status != store.PlanInvalidated {
			t.Fatalf("Execute() = (%#v, %v), want invalidated conflict", updated, err)
		}
		content, readErr := os.ReadFile(target)
		if readErr != nil || string(content) != "preserve" {
			t.Fatalf("conflicting target changed: %q (%v)", content, readErr)
		}
	})
}

func TestCreateDirectoryRejectsOutOfScopeAndUnsafeParents(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "scope"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(rootPath, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(rootPath, "real"), filepath.Join(rootPath, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "file"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()

	for _, proposal := range []Proposal{
		{ScopePath: "scope", SourcePath: "", TargetPath: "outside", Action: "create_directory", Reason: "用户指定目录"},
		{ScopePath: "scope", SourcePath: "scope", TargetPath: "elsewhere/new", Action: "create_directory", Reason: "用户指定目录"},
		{ScopePath: "", SourcePath: "link", TargetPath: "link/new", Action: "create_directory", Reason: "用户指定目录"},
		{ScopePath: "", SourcePath: "file", TargetPath: "file/new", Action: "create_directory", Reason: "用户指定目录"},
		{ScopePath: "", SourcePath: "", TargetPath: ".filesage-trash/new", Action: "create_directory", Reason: "用户指定目录"},
	} {
		proposal.RequestText = "新建目录"
		if _, err := service.Propose(ctx, proposal); err == nil {
			t.Fatalf("Propose(%#v) succeeded, want rejection", proposal)
		}
	}
}

func TestCreateDirectoryCannotExecuteWithoutApproval(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()

	plan := proposeDirectory(t, ctx, service, "", "new-directory", "")
	if _, err := service.Execute(ctx, plan.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("unapproved Execute() error = %v, want ErrConflict", err)
	}
	if _, err := service.Decide(ctx, plan.ID, plan.Operations[0].ID, "reject"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Execute(ctx, plan.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("rejected Execute() error = %v, want ErrConflict", err)
	}
	if _, err := os.Stat(filepath.Join(rootPath, "new-directory")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unapproved operation created target: %v", err)
	}
}

func TestCreateDirectoryRecoveryNeverReplaysAndMarksUncertainResultPartial(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name       string
		create     bool
		wantStatus store.OperationStatus
	}{
		{name: "target absent", wantStatus: store.OperationPending},
		{name: "target present is uncertain", create: true, wantStatus: store.OperationPartial},
	} {
		t.Run(test.name, func(t *testing.T) {
			rootPath := t.TempDir()
			metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
			defer root.Close()
			defer metadata.Close()
			plan := proposeDirectory(t, ctx, service, "", "interrupted", "")
			if _, err := service.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve"); err != nil {
				t.Fatal(err)
			}
			if err := metadata.StartAgentOperation(ctx, plan.ID, plan.Operations[0].ID, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(rootPath, "interrupted")
			if test.create {
				if err := os.Mkdir(target, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := service.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			recovered, err := service.Plan(ctx, plan.ID)
			if err != nil || recovered.Operations[0].Status != test.wantStatus {
				t.Fatalf("recovered plan = (%#v, %v), want operation %q", recovered, err, test.wantStatus)
			}
			if test.create {
				if _, err := os.Stat(target); err != nil {
					t.Fatalf("recovery removed uncertain target: %v", err)
				}
			} else if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("recovery replayed directory creation: %v", err)
			}
		})
	}
}

// proposeDirectory 创建由服务端快照绑定的手动目录建议。
func proposeDirectory(t *testing.T, ctx context.Context, service *Service, parent, target, scope string) store.PlanRecord {
	t.Helper()
	plan, err := service.Propose(ctx, Proposal{
		RequestText: "新建分类目录", ScopePath: scope, Action: "create_directory",
		SourcePath: parent, TargetPath: target, Reason: "按用户确认的分类新建目录",
	})
	if err != nil {
		t.Fatalf("Propose() error = %v", err)
	}
	return plan
}
