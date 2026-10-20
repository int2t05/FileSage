// 本文件验证 Agent 全局权限偏好和计划逐步执行状态。
package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestAgentSettingsDefaultToBypassAndPersistChanges(t *testing.T) {
	ctx := context.Background()
	metadata, _ := revisionFixture(t, 1)
	settings, err := metadata.AgentSettings(ctx)
	if err != nil || !settings.BypassPermission || settings.UpdatedAt.IsZero() {
		t.Fatalf("default Agent settings = (%#v, %v), want bypass enabled", settings, err)
	}
	at := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	if err := metadata.SetAgentBypassPermission(ctx, false, at); err != nil {
		t.Fatal(err)
	}
	settings, err = metadata.AgentSettings(ctx)
	if err != nil || settings.BypassPermission || !settings.UpdatedAt.Equal(at) {
		t.Fatalf("updated Agent settings = (%#v, %v)", settings, err)
	}
	page, err := metadata.AuditHistoryPage(ctx, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	var changes int
	for _, record := range page.Events {
		if record.EventType == "agent_permission_mode_changed" {
			changes++
			if record.Actor != "user" || record.Outcome != "manual" {
				t.Fatalf("setting audit = %#v", record)
			}
		}
	}
	if changes != 1 {
		t.Fatalf("setting audit count = %d, want 1", changes)
	}
}

func TestReviewAgentPlanCapturesModeAndPausesBetweenSteps(t *testing.T) {
	ctx := context.Background()
	metadata, plan := revisionFixture(t, 2)
	if err := metadata.SetAgentBypassPermission(ctx, false, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	mode, err := metadata.ReviewAgentPlan(ctx, plan.ID, plan.Version, "manual", at, false)
	if err != nil || mode != "manual" {
		t.Fatalf("review manual plan: %v", err)
	}
	reviewed, err := metadata.Plan(ctx, plan.ID)
	if err != nil || reviewed.Status != PlanApproved || reviewed.ExecutionMode != "manual" || reviewed.ReviewedAt == nil || !reviewed.ReviewedAt.Equal(at) {
		t.Fatalf("reviewed plan = (%#v, %v)", reviewed, err)
	}
	if reviewed.Operations[0].Approval != ApprovalPending || reviewed.Operations[1].Approval != ApprovalPending {
		t.Fatalf("manual review pre-approved steps: %#v", reviewed.Operations)
	}
	if err := metadata.StartReviewedAgentOperation(ctx, plan.ID, plan.Operations[1].ID, plan.Version, at); !errors.Is(err, ErrConflict) {
		t.Fatalf("out-of-order step start = %v, want conflict", err)
	}
	if err := metadata.StartReviewedAgentOperation(ctx, plan.ID, plan.Operations[0].ID, plan.Version, at); err != nil {
		t.Fatalf("start first reviewed step: %v", err)
	}
	if err := metadata.FinishAgentOperation(ctx, plan.ID, plan.Operations[0].ID, OperationSucceeded, "完成第一步", at); err != nil {
		t.Fatalf("finish first reviewed step: %v", err)
	}
	paused, err := metadata.Plan(ctx, plan.ID)
	if err != nil || paused.Status != PlanApproved || paused.Operations[0].Status != OperationSucceeded || paused.Operations[1].Approval != ApprovalPending {
		t.Fatalf("plan between manual steps = (%#v, %v)", paused, err)
	}
	task, err := metadata.Task(ctx, plan.TaskID)
	if err != nil || task.Status != TaskPaused {
		t.Fatalf("task between manual steps = (%#v, %v)", task, err)
	}
	if err := metadata.StartReviewedAgentOperation(ctx, plan.ID, plan.Operations[1].ID, plan.Version, at); err != nil {
		t.Fatalf("start second reviewed step: %v", err)
	}
	if err := metadata.FinishAgentOperation(ctx, plan.ID, plan.Operations[1].ID, OperationSucceeded, "完成第二步", at); err != nil {
		t.Fatalf("finish second reviewed step: %v", err)
	}
	completed, err := metadata.Plan(ctx, plan.ID)
	if err != nil || completed.Status != PlanCompleted || completed.ExecutionMode != "manual" || completed.Operations[1].Status != OperationSucceeded {
		t.Fatalf("completed manual plan = (%#v, %v)", completed, err)
	}
}

func TestBypassPlanReviewApprovesEveryOperationAtomically(t *testing.T) {
	ctx := context.Background()
	metadata, plan := revisionFixture(t, 2)
	mode, err := metadata.ReviewAgentPlan(ctx, plan.ID, plan.Version, "bypass", time.Now().UTC(), false)
	if err != nil || mode != "bypass" {
		t.Fatalf("review bypass plan: %v", err)
	}
	reviewed, err := metadata.Plan(ctx, plan.ID)
	if err != nil || reviewed.ExecutionMode != "bypass" || reviewed.ReviewedAt == nil || reviewed.Status != PlanApproved {
		t.Fatalf("reviewed bypass plan = (%#v, %v)", reviewed, err)
	}
	for _, operation := range reviewed.Operations {
		if operation.Approval != ApprovalApproved || operation.ApprovedBy != "user" {
			t.Fatalf("bypass review did not approve operation: %#v", operation)
		}
	}
	if err := metadata.ClaimAgentPlanExecution(ctx, reviewed.ID, reviewed.Version, time.Now().UTC()); err != nil {
		t.Fatalf("claim reviewed bypass plan: %v", err)
	}
}

func TestAgentPlanReviewRejectsStaleExecutionMode(t *testing.T) {
	ctx := context.Background()
	metadata, plan := revisionFixture(t, 1)
	if _, err := metadata.ReviewAgentPlan(ctx, plan.ID, plan.Version, "manual", time.Now().UTC(), false); !errors.Is(err, ErrConflict) {
		t.Fatalf("review with stale manual mode = %v, want conflict", err)
	}
	unchanged, err := metadata.Plan(ctx, plan.ID)
	if err != nil || unchanged.Status != PlanAwaitingApproval || unchanged.ReviewedAt != nil || unchanged.Operations[0].Approval != ApprovalPending {
		t.Fatalf("stale mode changed plan: (%#v, %v)", unchanged, err)
	}
}

func TestReviewAgentPlanRequiresAndAuditsPermanentDeleteConfirmation(t *testing.T) {
	ctx := context.Background()
	metadata, plan := revisionFixture(t, 1)
	if err := metadata.SetAgentBypassPermission(ctx, false, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.db.ExecContext(ctx, `UPDATE operations SET action = 'permanent_delete', source_path = 'old.txt', target_path = '' WHERE id = ?`, plan.Operations[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.ReviewAgentPlan(ctx, plan.ID, plan.Version, "manual", time.Now().UTC(), false); !errors.Is(err, ErrConflict) {
		t.Fatalf("review permanent-delete plan without confirmation = %v, want conflict", err)
	}
	unchanged, err := metadata.Plan(ctx, plan.ID)
	if err != nil || unchanged.Status != PlanAwaitingApproval || unchanged.ReviewedAt != nil {
		t.Fatalf("missing confirmation changed plan: (%#v, %v)", unchanged, err)
	}
	if _, err := metadata.ReviewAgentPlan(ctx, plan.ID, plan.Version, "manual", time.Now().UTC(), true); err != nil {
		t.Fatalf("review confirmed permanent-delete plan: %v", err)
	}
	page, err := metadata.AuditForTaskPage(ctx, plan.TaskID, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	var confirmationAudited bool
	for _, record := range page.Records {
		if record.EventType == "plan_reviewed" && strings.Contains(record.Message, "已确认计划中的永久删除不可恢复") {
			confirmationAudited = true
		}
	}
	if !confirmationAudited {
		t.Fatalf("plan audit omitted explicit irreversible-delete confirmation: %#v", page.Records)
	}

	bypassMetadata, bypassPlan := revisionFixture(t, 1)
	if _, err := bypassMetadata.db.ExecContext(ctx, `UPDATE operations SET action = 'permanent_delete', source_path = 'old.txt', target_path = '' WHERE id = ?`, bypassPlan.Operations[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := bypassMetadata.ReviewAgentPlan(ctx, bypassPlan.ID, bypassPlan.Version, "bypass", time.Now().UTC(), true); err != nil {
		t.Fatalf("review confirmed bypass permanent-delete plan: %v", err)
	}
	page, err = bypassMetadata.AuditForTaskPage(ctx, bypassPlan.TaskID, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	var operationConfirmationAudited bool
	for _, record := range page.Records {
		if record.EventType == "operation_approved" && record.OperationID == bypassPlan.Operations[0].ID && strings.Contains(record.Message, "不可恢复删除") {
			operationConfirmationAudited = true
		}
	}
	if !operationConfirmationAudited {
		t.Fatalf("bypass operation audit omitted irreversible-delete confirmation: %#v", page.Records)
	}
}
