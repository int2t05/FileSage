//go:build linux

// Package trash 验证回收流程的冲突边界。
package trash

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filesage/internal/fsroot"
	"filesage/internal/store"
)

type mutateAfterTrashDestination struct {
	*store.Store
	mutate func() error
}

func (m *mutateAfterTrashDestination) SetTrashDestinationForAgent(ctx context.Context, id int64, destination, taskID, planID, operationID string) error {
	if err := m.Store.SetTrashDestinationForAgent(ctx, id, destination, taskID, planID, operationID); err != nil {
		return err
	}
	return m.mutate()
}

func TestAgentTrashRechecksDirectoryTreeImmediatelyBeforeMove(t *testing.T) {
	ctx := context.Background()
	rootPath, databasePath := t.TempDir(), filepath.Join(t.TempDir(), "filesage.db")
	directory := filepath.Join(rootPath, "folder")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	childPath := filepath.Join(directory, "child.txt")
	if err := os.WriteFile(childPath, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	metadata, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	now := time.Now().UTC()
	operation := store.OperationRecord{
		ID: "operation", Ordinal: 0, Action: "trash", SourcePath: "folder", Reason: "用户已确认移入回收站",
		SourceStateJSON: `{}`, TargetStateJSON: `{}`, TargetConflictPolicy: "fail",
		Approval: store.ApprovalPending, Status: store.OperationPending, CreatedAt: now, UpdatedAt: now,
	}
	if err := metadata.CreateAgentTaskPlan(ctx,
		store.TaskRecord{ID: "task", Kind: "agent", Status: store.TaskAwaitingApproval, RequestText: "移入回收站", CreatedAt: now, UpdatedAt: now},
		store.PlanRecord{ID: "plan", TaskID: "task", Version: 1, Status: store.PlanAwaitingApproval, DataScopeJSON: `{}`, CreatedAt: now, UpdatedAt: now, Operations: []store.OperationRecord{operation}},
		store.AuditRecord{TaskID: "task", PlanID: "plan", OperationID: "operation", EventType: "plan_created", Actor: "agent", Outcome: "awaiting_approval"},
	); err != nil {
		t.Fatal(err)
	}
	approvedTree, err := root.SnapshotTree(ctx, "folder")
	if err != nil {
		t.Fatal(err)
	}
	data := &mutateAfterTrashDestination{Store: metadata, mutate: func() error {
		return os.WriteFile(childPath, []byte("after!"), 0o600)
	}}
	manager, err := Open(ctx, root, data)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := root.LstatEntry("folder")
	if err != nil {
		t.Fatal(err)
	}
	verificationCalls := 0
	_, err = manager.MoveToTrashForAgent(ctx, entry, "task", "plan", "operation", func() error {
		verificationCalls++
		current, snapshotErr := root.SnapshotTree(ctx, "folder")
		if snapshotErr != nil {
			return snapshotErr
		}
		if current.Digest != approvedTree.Digest {
			return fsroot.ErrConflict
		}
		return nil
	})
	if !errors.Is(err, fsroot.ErrConflict) || verificationCalls != 1 {
		t.Fatalf("MoveToTrashForAgent() = (%v, %d verification calls), want conflict after one final verification", err, verificationCalls)
	}
	if _, err := os.Stat(directory); err != nil {
		t.Fatalf("changed directory was moved: %v", err)
	}
	if body, err := os.ReadFile(childPath); err != nil || string(body) != "after!" {
		t.Fatalf("changed child was not preserved: body=%q err=%v", body, err)
	}
	page, err := manager.List(ctx, 100, "")
	if err != nil || len(page.Items) != 1 || page.Items[0].Status != string(store.TrashNeedsAttention) {
		t.Fatalf("changed trash intent = (%#v, %v), want manual attention", page.Items, err)
	}
	audit, err := metadata.AuditForTaskPage(ctx, "task", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	linkedEvents := 0
	for _, event := range audit.Records {
		if !strings.HasPrefix(event.EventType, "trash_") {
			continue
		}
		linkedEvents++
		if event.Actor != "agent" || event.TaskID != "task" || event.PlanID != "plan" || event.OperationID != "operation" {
			t.Fatalf("Agent trash conflict audit is not linked: %#v", event)
		}
	}
	if linkedEvents != 3 {
		t.Fatalf("Agent trash conflict audit has %d lifecycle events, want three", linkedEvents)
	}
}
