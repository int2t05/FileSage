// 本文件验证 Agent 方案修订的原子状态转换。
package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func revisionFixture(t *testing.T, operationCount int) (*Store, PlanRecord) {
	t.Helper()
	ctx := context.Background()
	metadata, err := Open(ctx, filepath.Join(t.TempDir(), "filesage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = metadata.Close() })
	now := time.Now().UTC()
	task := TaskRecord{ID: "revision-task", Kind: "agent", Status: TaskAwaitingApproval, RequestText: "整理文件", CreatedAt: now, UpdatedAt: now}
	operations := make([]OperationRecord, operationCount)
	for index := range operations {
		operations[index] = OperationRecord{
			ID: fmt.Sprintf("old-operation-%02d", index), Ordinal: index, Action: "create_directory",
			SourcePath: "", TargetPath: fmt.Sprintf("old-%02d", index), Reason: "原方案",
			Approval: ApprovalPending, Status: OperationPending, CreatedAt: now, UpdatedAt: now,
		}
	}
	plan := PlanRecord{
		ID: "old-plan", TaskID: task.ID, Version: 1, Status: PlanAwaitingApproval,
		DataScopeJSON: `{"proposalSource":"caller"}`, CreatedAt: now, UpdatedAt: now, Operations: operations,
	}
	if err := metadata.CreateAgentTaskPlan(ctx, task, plan, AuditRecord{
		TaskID: task.ID, PlanID: plan.ID, OperationID: operations[0].ID,
		EventType: "plan_created", Actor: "user", Outcome: "awaiting_approval", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	return metadata, plan
}

func revisedFixturePlan(previous PlanRecord, operationCount int) PlanRecord {
	now := time.Now().UTC()
	operations := make([]OperationRecord, operationCount)
	for index := range operations {
		operations[index] = OperationRecord{
			ID: fmt.Sprintf("new-operation-%02d", index), PlanID: "new-plan", Ordinal: index,
			Action: "create_directory", TargetPath: fmt.Sprintf("new-%02d", index), Reason: "修改后的方案",
			Approval: ApprovalPending, Status: OperationPending, CreatedAt: now, UpdatedAt: now,
		}
	}
	return PlanRecord{
		ID: "new-plan", TaskID: previous.TaskID, Version: previous.Version + 1, Status: PlanAwaitingApproval,
		DataScopeJSON: `{"proposalSource":"user-edit"}`, CreatedAt: now, UpdatedAt: now, Operations: operations,
	}
}

func TestReviseAgentPlanPreservesHistoryAndAppendsAudit(t *testing.T) {
	ctx := context.Background()
	metadata, previous := revisionFixture(t, 2)
	revised := revisedFixturePlan(previous, 1)
	if err := metadata.ReviseAgentPlan(ctx, previous.ID, previous.Version, revised, time.Now().UTC()); err != nil {
		t.Fatalf("ReviseAgentPlan() error = %v", err)
	}
	old, err := metadata.Plan(ctx, previous.ID)
	if err != nil || old.Status != PlanInvalidated || len(old.Operations) != 2 {
		t.Fatalf("old plan = (%#v, %v)", old, err)
	}
	for _, operation := range old.Operations {
		if operation.Approval != ApprovalInvalidated || operation.Status != OperationCancelled {
			t.Fatalf("old operation is still actionable: %#v", operation)
		}
	}
	latest, err := metadata.LatestPlanForTask(ctx, previous.TaskID)
	if err != nil || latest.ID != revised.ID || latest.Version != 2 || len(latest.Operations) != 1 || latest.Operations[0].Approval != ApprovalPending {
		t.Fatalf("latest plan = (%#v, %v)", latest, err)
	}
	audit, err := metadata.AuditForTaskPage(ctx, previous.TaskID, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	var edited, superseded, created int
	for _, event := range audit.Records {
		if event.EventType == "plan_edited" && event.PlanID == previous.ID && event.Actor == "user" {
			edited++
		}
		if event.EventType == "operation_superseded" && event.PlanID == previous.ID {
			superseded++
		}
		if event.EventType == "plan_created" && event.PlanID == revised.ID {
			created++
		}
	}
	if edited != 1 || superseded != 2 || created != 1 {
		t.Fatalf("audit counts: edited=%d superseded=%d created=%d records=%#v", edited, superseded, created, audit.Records)
	}
}

func TestReviseAgentPlanRejectsStaleApprovedAndInvalidReplacement(t *testing.T) {
	ctx := context.Background()
	t.Run("stale version", func(t *testing.T) {
		metadata, previous := revisionFixture(t, 1)
		defer metadata.Close()
		if err := metadata.ReviseAgentPlan(ctx, previous.ID, 2, revisedFixturePlan(previous, 1), time.Now()); !errors.Is(err, ErrConflict) {
			t.Fatalf("stale revision = %v, want conflict", err)
		}
	})
	t.Run("approval began", func(t *testing.T) {
		metadata, previous := revisionFixture(t, 1)
		defer metadata.Close()
		if err := metadata.SetAgentApproval(ctx, previous.ID, previous.Operations[0].ID, ApprovalApproved, time.Now(), previous.Version); err != nil {
			t.Fatal(err)
		}
		if err := metadata.ReviseAgentPlan(ctx, previous.ID, previous.Version, revisedFixturePlan(previous, 1), time.Now()); !errors.Is(err, ErrConflict) {
			t.Fatalf("revision after approval = %v, want conflict", err)
		}
	})
	t.Run("operation limit", func(t *testing.T) {
		metadata, previous := revisionFixture(t, 1)
		defer metadata.Close()
		if err := metadata.ReviseAgentPlan(ctx, previous.ID, previous.Version, revisedFixturePlan(previous, MaxAgentPlanOperations+1), time.Now()); err == nil {
			t.Fatalf("%d-operation plan revision succeeded", MaxAgentPlanOperations+1)
		}
		old, err := metadata.Plan(ctx, previous.ID)
		if err != nil || old.Status != PlanAwaitingApproval {
			t.Fatalf("invalid replacement changed old plan: (%#v, %v)", old, err)
		}
	})
}

func TestReviseAgentPlanMovesConversationToNewVersionAtomically(t *testing.T) {
	ctx := context.Background()
	metadata, err := Open(ctx, filepath.Join(t.TempDir(), "filesage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	now := time.Now().UTC()
	if err := metadata.CreateConversation(ctx, "revision-conversation", "创建归档目录", now); err != nil {
		t.Fatal(err)
	}
	if err := metadata.FinishConversationTurn(ctx, "revision-conversation", ConversationAwaitingConfirmation, "请确认目录。", AnalysisCard{
		Goal: "整理归档目录", Operations: []AnalysisOperation{{Action: "create_directory", TargetPath: "archive"}},
		Provider: "test", Model: "model", Destination: "local",
	}, "", now); err != nil {
		t.Fatal(err)
	}
	if claimed, err := metadata.BeginConversationAnalysis(ctx, "revision-conversation", now); err != nil || !claimed {
		t.Fatalf("BeginConversationAnalysis() = (%t, %v)", claimed, err)
	}
	operation := OperationRecord{ID: "conversation-operation", Ordinal: 0, Action: "create_directory", TargetPath: "archive", Approval: ApprovalPending, Status: OperationPending, CreatedAt: now, UpdatedAt: now}
	task := TaskRecord{ID: "conversation-task", Kind: "agent", Status: TaskAwaitingApproval, RequestText: "创建归档目录", ModelProvider: "test", ModelName: "model", CreatedAt: now, UpdatedAt: now}
	plan := PlanRecord{ID: "conversation-plan", TaskID: task.ID, Version: 1, Status: PlanAwaitingApproval, ModelProvider: "test", ModelName: "model", DataScopeJSON: `{"proposalSource":"conversation"}`, CreatedAt: now, UpdatedAt: now, Operations: []OperationRecord{operation}}
	if err := metadata.CreateConversationTaskPlan(ctx, "revision-conversation", task, plan, AuditRecord{
		TaskID: task.ID, PlanID: plan.ID, OperationID: operation.ID, EventType: "plan_created", Actor: "agent", Outcome: "awaiting_approval", CreatedAt: now,
	}, "已生成执行计划，请审阅。"); err != nil {
		t.Fatal(err)
	}
	revised := revisedFixturePlan(plan, 1)
	revised.ID = "conversation-plan-v2"
	revised.ModelProvider = plan.ModelProvider
	revised.ModelName = plan.ModelName
	revised.Operations[0].PlanID = revised.ID
	if err := metadata.ReviseAgentPlan(ctx, plan.ID, plan.Version, revised, now.Add(time.Second)); err != nil {
		t.Fatalf("ReviseAgentPlan() error = %v", err)
	}
	conversation, err := metadata.Conversation(ctx, "revision-conversation")
	if err != nil || conversation.State != ConversationAwaitingApproval || conversation.PlanID != revised.ID {
		t.Fatalf("conversation after edit = (%#v, %v)", conversation, err)
	}
}
