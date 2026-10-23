// 本文件验证整份方案拒绝的状态兼容性。
package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"filesage/internal/coordination"
	"filesage/internal/fsroot"
	"filesage/internal/store"
)

// TestRejectPlanHandlesPreviouslyApprovedSteps 验证旧式部分审批方案仍可整份拒绝。
func TestRejectPlanHandlesPreviouslyApprovedSteps(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	metadata, err := store.Open(ctx, filepath.Join(t.TempDir(), "filesage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.ConfigureMoveRecoveryDirectory(filepath.Join(t.TempDir(), "move-recovery")); err != nil {
		t.Fatal(err)
	}
	service := New(metadata, root, coordination.New())
	plan, err := service.ProposeMany(ctx, []Proposal{
		{RequestText: "建立两个目录", Action: "create_directory", TargetPath: "one", Reason: "按用户确认新建目录"},
		{RequestText: "建立两个目录", Action: "create_directory", TargetPath: "two", Reason: "按用户确认新建目录"},
	})
	if err != nil || len(plan.Operations) != 2 {
		t.Fatalf("ProposeMany() = (%#v, %v)", plan, err)
	}
	partiallyApproved, err := service.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve", plan.Version)
	if err != nil || partiallyApproved.Status != store.PlanAwaitingApproval {
		t.Fatalf("partial approval = (%#v, %v)", partiallyApproved, err)
	}

	rejected, err := service.RejectPlan(ctx, plan.ID, plan.Version)
	if err != nil || rejected.Status != store.PlanRejected {
		t.Fatalf("RejectPlan() = (%#v, %v)", rejected, err)
	}
	if rejected.Operations[0].Approval != store.ApprovalInvalidated || rejected.Operations[0].Status != store.OperationCancelled ||
		rejected.Operations[1].Approval != store.ApprovalRejected || rejected.Operations[1].Status != store.OperationSkipped {
		t.Fatalf("rejected operations = %#v", rejected.Operations)
	}
	for _, name := range []string{"one", "two"} {
		if _, err := os.Stat(filepath.Join(rootPath, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("rejection created %q: %v", name, err)
		}
	}
}
