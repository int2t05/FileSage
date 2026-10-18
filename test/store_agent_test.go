// 本文件验证 Agent 任务状态与审计的事务边界。
package test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"filesage/internal/store"
)

func TestAgentPlanApprovalExecutionAndAuditAreAtomic(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Now().UTC()
	task := store.TaskRecord{ID: "task-agent", Kind: "agent", Status: store.TaskAwaitingApproval, ScopePath: "photos", CreatedAt: now, UpdatedAt: now}
	plan := store.PlanRecord{
		ID: "plan-agent", TaskID: task.ID, Version: 1, Status: store.PlanAwaitingApproval,
		ScopePath: "photos", DataScopeJSON: `{"proposalSource":"caller"}`, CreatedAt: now, UpdatedAt: now,
		Operations: []store.OperationRecord{{
			ID: "op-agent", Ordinal: 0, Action: "rename", SourcePath: "photos/a.jpg", TargetPath: "photos/b.jpg",
			Reason: "统一名称", SourceStateJSON: `{"exists":true}`, TargetStateJSON: `{"exists":false}`,
			TargetConflictPolicy: "fail", Approval: store.ApprovalPending, Status: store.OperationPending,
			CreatedAt: now, UpdatedAt: now,
		}},
	}
	badAudit := store.AuditRecord{TaskID: task.ID, PlanID: plan.ID, OperationID: "op-agent", Actor: "user", Outcome: "accepted"}
	if err := db.CreateAgentTaskPlan(ctx, task, plan, badAudit); err == nil {
		t.Fatal("CreateAgentTaskPlan accepted audit without an event type")
	}
	if _, err := db.Task(ctx, task.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("failed transaction left task: %v", err)
	}
	if _, err := db.Plan(ctx, plan.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("failed transaction left plan: %v", err)
	}

	badAudit.EventType = "plan_created"
	if err := db.CreateAgentTaskPlan(ctx, task, plan, badAudit); err != nil {
		t.Fatalf("CreateAgentTaskPlan() error = %v", err)
	}
	latest, err := db.LatestPlanForTask(ctx, task.ID)
	if err != nil || latest.ID != plan.ID || len(latest.Operations) != 1 {
		t.Fatalf("LatestPlanForTask() = (%+v, %v)", latest, err)
	}

	if err := db.SetAgentApproval(ctx, plan.ID, "op-agent", store.ApprovalApproved, now.Add(time.Minute)); err != nil {
		t.Fatalf("SetAgentApproval() error = %v", err)
	}
	if err := db.SetAgentApproval(ctx, plan.ID, "op-agent", store.ApprovalRejected, now.Add(2*time.Minute)); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate decision error = %v, want conflict", err)
	}
	approved, err := db.Plan(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if approved.Status != store.PlanApproved || approved.Operations[0].Approval != store.ApprovalApproved || approved.Operations[0].ApprovedBy != "user" {
		t.Fatalf("approval was not stored with fixed actor: %+v", approved)
	}
	taskRecord, err := db.Task(ctx, task.ID)
	if err != nil || taskRecord.Status != store.TaskPaused {
		t.Fatalf("approved task status = (%q, %v), want paused", taskRecord.Status, err)
	}
	if err := db.StartAgentOperation(ctx, plan.ID, "op-agent", now.Add(2*time.Minute)); err != nil {
		t.Fatalf("StartAgentOperation() error = %v", err)
	}
	if err := db.StartAgentOperation(ctx, plan.ID, "op-agent", now.Add(3*time.Minute)); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate execution claim error = %v, want conflict", err)
	}
	if err := db.FinishAgentOperation(ctx, plan.ID, "op-agent", store.OperationSucceeded, "完成", now.Add(4*time.Minute)); err != nil {
		t.Fatalf("FinishAgentOperation() error = %v", err)
	}
	completed, err := db.Plan(ctx, plan.ID)
	if err != nil || completed.Status != store.PlanCompleted || completed.Operations[0].Status != store.OperationSucceeded {
		t.Fatalf("completed plan = (%+v, %v)", completed, err)
	}
	audit, err := db.AuditForTask(ctx, task.ID)
	if err != nil || len(audit) != 4 {
		t.Fatalf("audit count = (%d, %v), want 4 atomic events", len(audit), err)
	}
	for _, event := range audit {
		if event.Actor != "user" {
			t.Fatalf("audit actor = %q, want server actor user", event.Actor)
		}
	}
}

func TestAgentRecoveryOnlyReturnsKnownUntouchedOperationToApproved(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "agent-recovery.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC()
	task := store.TaskRecord{ID: "task-recovery", Kind: "agent", Status: store.TaskAwaitingApproval, CreatedAt: now}
	plan := store.PlanRecord{
		ID: "plan-recovery", TaskID: task.ID, Version: 1, Status: store.PlanAwaitingApproval,
		Operations: []store.OperationRecord{{
			ID: "op-recovery", Ordinal: 0, Action: "rename", SourcePath: "a", TargetPath: "b",
			Approval: store.ApprovalPending, Status: store.OperationPending,
		}},
	}
	if err := db.CreateAgentTaskPlan(ctx, task, plan, store.AuditRecord{TaskID: task.ID, PlanID: plan.ID, OperationID: "op-recovery", EventType: "plan_created", Actor: "user", Outcome: "pending"}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetAgentApproval(ctx, plan.ID, "op-recovery", store.ApprovalApproved, now); err != nil {
		t.Fatal(err)
	}
	if err := db.StartAgentOperation(ctx, plan.ID, "op-recovery", now); err != nil {
		t.Fatal(err)
	}
	if err := db.RecoverAgentOperation(ctx, plan.ID, "op-recovery", "retry", "source unchanged", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	recovered, err := db.Plan(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != store.PlanApproved || recovered.Operations[0].Status != store.OperationPending || recovered.Operations[0].Approval != store.ApprovalApproved {
		t.Fatalf("recovered operation is not safe for explicit retry: %+v", recovered)
	}
	if err := db.StartAgentOperation(ctx, plan.ID, "op-recovery", now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := db.InvalidateAgentOperation(ctx, plan.ID, "op-recovery", "执行前状态改变", now.Add(3*time.Minute)); err != nil {
		t.Fatalf("invalidate claimed operation: %v", err)
	}
	invalidated, err := db.Plan(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if invalidated.Status != store.PlanInvalidated || invalidated.Operations[0].Status != store.OperationSkipped || invalidated.Operations[0].Approval != store.ApprovalInvalidated {
		t.Fatalf("running stale operation was not atomically invalidated: %+v", invalidated)
	}
}
