// 本文件验证 Shell 计划审核、暂存差异与执行状态。
package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type shellFixture struct {
	db           *Store
	conversation string
	task         TaskRecord
	plan         PlanRecord
	run          ShellRunRecord
	changes      []ShellChangeRecord
	audit        AuditRecord
	reply        string
}

func prepareShellFixture(t *testing.T, count int, noChanges bool) shellFixture {
	t.Helper()
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "filesage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	conversationID := "shell-conversation"
	if err := db.CreateConversation(ctx, conversationID, "整理选择范围中的文件", now); err != nil {
		t.Fatal(err)
	}
	card := AnalysisCard{
		Goal: "在隔离环境分析文件变化", Provider: "local", Model: "test-model", Destination: "local",
		Operations: []AnalysisOperation{{Action: "run_shell", FilePath: "docs", ShellCommand: "find . -type f", WorkingDir: "."}},
	}
	if err := db.FinishConversationTurn(ctx, conversationID, ConversationAwaitingConfirmation, "请确认命令和范围。", card, "", now); err != nil {
		t.Fatal(err)
	}
	if claimed, err := db.BeginConversationAnalysis(ctx, conversationID, now); err != nil || !claimed {
		t.Fatalf("BeginConversationAnalysis() = (%t, %v)", claimed, err)
	}
	taskStatus, planStatus, runStatus := TaskAwaitingApproval, PlanAwaitingApproval, ShellRunAwaitingApproval
	operationStatus, auditOutcome, reply := OperationPending, "awaiting_approval", "已生成文件差异，请审核。"
	if noChanges {
		taskStatus, planStatus, runStatus = TaskSucceeded, PlanCompleted, ShellRunCompleted
		operationStatus, auditOutcome, reply = OperationSucceeded, "completed", "命令已完成，没有发现文件变化。"
	}
	task := TaskRecord{
		ID: "shell-task", Kind: "agent", Status: taskStatus, RequestText: "整理选择范围中的文件", ScopePath: "docs",
		ModelProvider: "local", ModelName: "test-model", CreatedAt: now, UpdatedAt: now,
	}
	operation := OperationRecord{
		ID: "shell-parent", PlanID: "shell-plan", Ordinal: 0, Action: "run_shell", SourcePath: "docs",
		Approval: ApprovalPending, Status: operationStatus, CreatedAt: now, UpdatedAt: now,
	}
	if noChanges {
		operation.ResultMessage = "沙箱任务已完成，没有发现文件变化。"
	}
	plan := PlanRecord{
		ID: "shell-plan", TaskID: task.ID, Version: 1, Status: planStatus, ScopePath: task.ScopePath,
		ModelProvider: task.ModelProvider, ModelName: task.ModelName, DataScopeJSON: `{"source":"conversation"}`,
		CreatedAt: now, UpdatedAt: now, Operations: []OperationRecord{operation},
	}
	run := ShellRunRecord{
		ID: "shell-run", PlanID: plan.ID, OperationID: operation.ID, Command: "find . -type f", WorkingDirectory: ".",
		Stdout: "file listing", Stderr: "warning", OutputTruncated: true, ExitCode: 0,
		StageID: "stage-payload", Status: runStatus, CreatedAt: now, UpdatedAt: now,
	}
	changes := make([]ShellChangeRecord, count)
	for index := range changes {
		mode := uint32(0o644)
		changes[index] = ShellChangeRecord{
			ID: fmt.Sprintf("change-%05d", index), PlanID: plan.ID, Ordinal: index,
			Path: fmt.Sprintf("result/%05d.txt", index), Kind: "added",
			OriginalStateJSON: `{"fingerprint":{"exists":false},"sha256":"","mode":0,"isDirectory":false,"size":0}`,
			CandidateSHA256:   "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			CandidateSize:     5, CandidateMode: &mode, PayloadID: fmt.Sprintf("payload-%05d", index),
			Approval: ApprovalPending, Status: OperationPending, CreatedAt: now, UpdatedAt: now,
		}
	}
	return shellFixture{
		db: db, conversation: conversationID, task: task, plan: plan, run: run, changes: changes,
		audit: AuditRecord{TaskID: task.ID, PlanID: plan.ID, OperationID: operation.ID, EventType: "shell_plan_created", Actor: "agent", Outcome: auditOutcome, SourcePath: task.ScopePath, CreatedAt: now},
		reply: reply,
	}
}

func createShellFixture(t *testing.T, count int, noChanges bool) shellFixture {
	t.Helper()
	fixture := prepareShellFixture(t, count, noChanges)
	if err := fixture.db.CreateConversationShellTaskPlan(context.Background(), fixture.conversation, fixture.task, fixture.plan, fixture.run, fixture.changes, fixture.audit, fixture.reply); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func TestCreateConversationShellTaskPlanPersistsAndPagesChanges(t *testing.T) {
	fixture := createShellFixture(t, 105, false)
	ctx := context.Background()
	run, err := fixture.db.ShellRun(ctx, fixture.plan.ID)
	if err != nil || run.Command != fixture.run.Command || run.WorkingDirectory != "." || run.StageID != fixture.run.StageID ||
		run.Stdout != fixture.run.Stdout || run.Stderr != fixture.run.Stderr || run.OutputTruncated != fixture.run.OutputTruncated {
		t.Fatalf("ShellRun() = (%#v, %v)", run, err)
	}
	first, err := fixture.db.ListShellChanges(ctx, fixture.plan.ID, 100, 0)
	if err != nil || first.Total != 105 || len(first.Changes) != 100 || !first.HasMore || first.Changes[0].Ordinal != 0 || first.Changes[99].Ordinal != 99 {
		t.Fatalf("first page = (%#v, %v)", first, err)
	}
	second, err := fixture.db.ListShellChanges(ctx, fixture.plan.ID, 100, 100)
	if err != nil || second.Total != 105 || len(second.Changes) != 5 || second.HasMore || second.Changes[0].Ordinal != 100 {
		t.Fatalf("second page = (%#v, %v)", second, err)
	}
	if _, err := fixture.db.ListShellChanges(ctx, fixture.plan.ID, 101, 0); !errors.Is(err, ErrShellPagination) {
		t.Fatalf("oversized page error = %v", err)
	}
	if _, err := fixture.db.ListShellChanges(ctx, fixture.plan.ID, 10, 106); !errors.Is(err, ErrShellPagination) {
		t.Fatalf("past-end page error = %v", err)
	}
	var proposalAudits int
	if err := fixture.db.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_events WHERE plan_id = ? AND event_type = 'shell_change_proposed' AND operation_id = ?`, fixture.plan.ID, fixture.run.OperationID).Scan(&proposalAudits); err != nil || proposalAudits != 105 {
		t.Fatalf("proposal audit count = (%d, %v)", proposalAudits, err)
	}
}

func TestCreateConversationShellTaskPlanOmitsRedundantAssistantReply(t *testing.T) {
	for _, noChanges := range []bool{false, true} {
		t.Run(map[bool]string{false: "with changes", true: "without changes"}[noChanges], func(t *testing.T) {
			count := 1
			if noChanges {
				count = 0
			}
			fixture := prepareShellFixture(t, count, noChanges)
			if err := fixture.db.CreateConversationShellTaskPlan(context.Background(), fixture.conversation, fixture.task, fixture.plan, fixture.run, fixture.changes, fixture.audit, ""); err != nil {
				t.Fatal(err)
			}
			conversation, err := fixture.db.Conversation(context.Background(), fixture.conversation)
			if err != nil {
				t.Fatal(err)
			}
			if len(conversation.Messages) != 2 || conversation.PlanID != fixture.plan.ID {
				t.Fatalf("conversation after Shell plan = (%#v), want only the existing exchange and linked plan", conversation)
			}
		})
	}
}

func TestCreateConversationShellTaskPlanRollsBackEveryRecord(t *testing.T) {
	fixture := prepareShellFixture(t, 2, false)
	if _, err := fixture.db.db.ExecContext(context.Background(), `CREATE TRIGGER reject_shell_reply BEFORE INSERT ON agent_conversation_messages
		WHEN NEW.conversation_id = 'shell-conversation' AND NEW.role = 'assistant' AND NEW.sequence = 3
		BEGIN SELECT RAISE(ABORT, 'injected reply failure'); END`); err != nil {
		t.Fatal(err)
	}
	err := fixture.db.CreateConversationShellTaskPlan(context.Background(), fixture.conversation, fixture.task, fixture.plan, fixture.run, fixture.changes, fixture.audit, fixture.reply)
	if err == nil {
		t.Fatal("transaction succeeded despite reply insert failure")
	}
	for _, table := range []string{"tasks", "plans", "operations", "shell_runs", "shell_changes", "audit_events"} {
		var count int
		query := ""
		switch table {
		case "tasks":
			query = "SELECT COUNT(*) FROM tasks WHERE id = 'shell-task'"
		case "plans":
			query = "SELECT COUNT(*) FROM plans WHERE id = 'shell-plan'"
		case "operations":
			query = "SELECT COUNT(*) FROM operations WHERE plan_id = 'shell-plan'"
		case "shell_runs":
			query = "SELECT COUNT(*) FROM shell_runs WHERE plan_id = 'shell-plan'"
		case "shell_changes":
			query = "SELECT COUNT(*) FROM shell_changes WHERE plan_id = 'shell-plan'"
		case "audit_events":
			query = "SELECT COUNT(*) FROM audit_events WHERE task_id = 'shell-task'"
		}
		if err := fixture.db.db.QueryRowContext(context.Background(), query).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s rows after rollback = (%d, %v)", table, count, err)
		}
	}
	conversation, err := fixture.db.Conversation(context.Background(), fixture.conversation)
	if err != nil || conversation.State != ConversationAnalyzing || conversation.TaskID != "" || conversation.PlanID != "" {
		t.Fatalf("conversation after rollback = (%#v, %v)", conversation, err)
	}
}

func TestRejectShellPlanChecksVersionAndRejectsWholePlan(t *testing.T) {
	fixture := createShellFixture(t, 3, false)
	ctx := context.Background()
	if err := fixture.db.RejectShellPlan(ctx, fixture.plan.ID, fixture.plan.Version+1, time.Now()); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale plan version error = %v", err)
	}
	if _, err := fixture.db.db.ExecContext(ctx, `UPDATE shell_changes SET approval_state = 'approved', approved_by = 'user', approved_at_ns = ? WHERE plan_id = ? AND id = ?`,
		time.Now().UnixNano(), fixture.plan.ID, fixture.changes[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.RejectShellPlan(ctx, fixture.plan.ID, fixture.plan.Version, time.Now()); err != nil {
		t.Fatal(err)
	}
	plan, err := fixture.db.Plan(ctx, fixture.plan.ID)
	if err != nil || plan.Status != PlanRejected || plan.Operations[0].Approval != ApprovalRejected || plan.Operations[0].Status != OperationSkipped {
		t.Fatalf("rejected Shell plan = (%#v, %v)", plan, err)
	}
	var approved, rejected, invalidated, cancelled int
	if err := fixture.db.db.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(CASE WHEN approval_state = 'approved' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN approval_state = 'rejected' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN approval_state = 'invalidated' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN execution_status = 'cancelled' THEN 1 ELSE 0 END), 0)
		FROM shell_changes WHERE plan_id = ?`, fixture.plan.ID).Scan(&approved, &rejected, &invalidated, &cancelled); err != nil {
		t.Fatal(err)
	}
	if approved != 0 || rejected != 3 || invalidated != 0 || cancelled != 3 {
		t.Fatalf("change terminal states = approved:%d rejected:%d invalidated:%d cancelled:%d", approved, rejected, invalidated, cancelled)
	}
	var staleApprovalMetadata int
	if err := fixture.db.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM shell_changes WHERE plan_id = ? AND (approved_by <> '' OR approved_at_ns IS NOT NULL)`, fixture.plan.ID).Scan(&staleApprovalMetadata); err != nil || staleApprovalMetadata != 0 {
		t.Fatalf("rejected change approval metadata = (%d, %v)", staleApprovalMetadata, err)
	}
	var pathAudits, wrongOperation int
	if err := fixture.db.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(CASE WHEN operation_id <> ? THEN 1 ELSE 0 END), 0)
		FROM audit_events WHERE plan_id = ? AND event_type IN ('shell_change_proposed', 'shell_change_rejected') AND source_path <> ''`, fixture.run.OperationID, fixture.plan.ID).Scan(&pathAudits, &wrongOperation); err != nil || pathAudits != 6 || wrongOperation != 0 {
		t.Fatalf("path audit = (%d paths, %d wrong parents, %v)", pathAudits, wrongOperation, err)
	}
}

func TestReviewedShellPlanApprovesDiffAsOneToolStep(t *testing.T) {
	fixture := createShellFixture(t, 2, false)
	ctx := context.Background()
	if err := fixture.db.SetAgentBypassPermission(ctx, false, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	mode, err := fixture.db.ReviewShellPlan(ctx, fixture.plan.ID, fixture.plan.Version, "manual", time.Now().UTC())
	if err != nil || mode != "manual" {
		t.Fatalf("review Shell plan: %v", err)
	}
	plan, err := fixture.db.Plan(ctx, fixture.plan.ID)
	if err != nil || plan.Status != PlanApproved || plan.ExecutionMode != "manual" || plan.ReviewedAt == nil || plan.Operations[0].Approval != ApprovalApproved {
		t.Fatalf("reviewed Shell plan = (%#v, %v)", plan, err)
	}
	for _, change := range fixture.changes {
		if change.Approval != ApprovalPending || change.Status != OperationPending {
			t.Fatalf("fixture unexpectedly mutated: %#v", change)
		}
	}
	page, err := fixture.db.ListShellChanges(ctx, fixture.plan.ID, 10, 0)
	if err != nil || len(page.Changes) != 2 {
		t.Fatalf("reviewed Shell changes = (%#v, %v)", page, err)
	}
	for _, change := range page.Changes {
		if change.Approval != ApprovalApproved || change.ApprovedBy != "user" {
			t.Fatalf("Shell diff was not included in plan review: %#v", change)
		}
	}
	run, err := fixture.db.ShellRun(ctx, plan.ID)
	if err != nil || run.Status != ShellRunApproved || run.ExitCode != 0 || run.OutputDiscarded != fixture.run.OutputDiscarded ||
		run.Stdout != fixture.run.Stdout || run.Stderr != fixture.run.Stderr || run.OutputTruncated != fixture.run.OutputTruncated {
		t.Fatalf("reviewed Shell run = (%#v, %v)", run, err)
	}
	if err := fixture.db.StartShellExecution(ctx, plan.ID, plan.Version, time.Now().UTC()); err != nil {
		t.Fatalf("start reviewed Shell tool step: %v", err)
	}
	if _, err := fixture.db.AuditForTask(ctx, plan.TaskID); err != nil {
		t.Fatal(err)
	}
}

func TestRejectShellPlanRaceHasOneWinner(t *testing.T) {
	fixture := createShellFixture(t, 2, false)
	ctx := context.Background()
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- fixture.db.RejectShellPlan(ctx, fixture.plan.ID, fixture.plan.Version, time.Now())
		}()
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("plan rejection race succeeded %d times", successes)
	}
	var eventCount int
	if err := fixture.db.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_events WHERE plan_id = ? AND operation_id = ? AND event_type = 'shell_plan_rejected'`, fixture.plan.ID, fixture.run.OperationID).Scan(&eventCount); err != nil || eventCount != 1 {
		t.Fatalf("plan rejection audit count = (%d, %v)", eventCount, err)
	}
}

func TestShellPlanInvalidationAndExecutionAreAtomic(t *testing.T) {
	ctx := context.Background()
	t.Run("invalidate before writing", func(t *testing.T) {
		fixture := createShellFixture(t, 3, false)
		if err := fixture.db.SetAgentBypassPermission(ctx, false, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.db.ReviewShellPlan(ctx, fixture.plan.ID, fixture.plan.Version, "manual", time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		if err := fixture.db.InvalidateShellPlan(ctx, fixture.plan.ID, fixture.changes[1].ID, "文件状态已变化", time.Now()); err != nil {
			t.Fatal(err)
		}
		plan, err := fixture.db.Plan(ctx, fixture.plan.ID)
		if err != nil || plan.Status != PlanInvalidated || plan.Operations[0].Approval != ApprovalInvalidated || plan.Operations[0].Status != OperationSkipped {
			t.Fatalf("invalidated plan = (%#v, %v)", plan, err)
		}
		run, err := fixture.db.ShellRun(ctx, fixture.plan.ID)
		if err != nil || run.Status != ShellRunInvalidated {
			t.Fatalf("invalidated run = (%#v, %v)", run, err)
		}
		conversation, err := fixture.db.Conversation(ctx, fixture.conversation)
		if err != nil || conversation.State != ConversationFailed || conversation.Error != "file_conflict" {
			t.Fatalf("invalidated conversation = (%#v, %v)", conversation, err)
		}
		var stalePaths, wrongParent int
		if err := fixture.db.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(CASE WHEN operation_id <> ? THEN 1 ELSE 0 END), 0)
			FROM audit_events WHERE plan_id = ? AND event_type = 'shell_change_invalidated'`, fixture.run.OperationID, fixture.plan.ID).Scan(&stalePaths, &wrongParent); err != nil || stalePaths != 3 || wrongParent != 0 {
			t.Fatalf("invalidation audits = (%d, %d, %v)", stalePaths, wrongParent, err)
		}
	})
	t.Run("execution success", func(t *testing.T) {
		fixture := createShellFixture(t, 2, false)
		if err := fixture.db.SetAgentBypassPermission(ctx, false, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.db.ReviewShellPlan(ctx, fixture.plan.ID, fixture.plan.Version, "manual", time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		if err := fixture.db.StartShellExecution(ctx, fixture.plan.ID, fixture.plan.Version, time.Now()); err != nil {
			t.Fatal(err)
		}
		for _, change := range fixture.changes {
			if err := fixture.db.StartShellChange(ctx, fixture.plan.ID, change.ID, fixture.plan.Version, time.Now()); err != nil {
				t.Fatal(err)
			}
			if err := fixture.db.FinishShellChange(ctx, fixture.plan.ID, change.ID, OperationSucceeded, "已提交", fixture.plan.Version, time.Now()); err != nil {
				t.Fatal(err)
			}
		}
		if err := fixture.db.FinishShellExecution(ctx, fixture.plan.ID, ShellRunCompleted, "已完成", fixture.plan.Version, time.Now()); err != nil {
			t.Fatal(err)
		}
		plan, err := fixture.db.Plan(ctx, fixture.plan.ID)
		if err != nil || plan.Status != PlanCompleted || plan.Operations[0].Status != OperationSucceeded {
			t.Fatalf("completed Shell plan = (%#v, %v)", plan, err)
		}
		run, err := fixture.db.ShellRun(ctx, fixture.plan.ID)
		if err != nil || run.Status != ShellRunCompleted {
			t.Fatalf("completed Shell run = (%#v, %v)", run, err)
		}
	})
}

func TestShellNoChangeRunCompletesWithoutApproval(t *testing.T) {
	fixture := createShellFixture(t, 0, true)
	ctx := context.Background()
	plan, err := fixture.db.Plan(ctx, fixture.plan.ID)
	if err != nil || plan.Status != PlanCompleted || plan.Operations[0].Status != OperationSucceeded {
		t.Fatalf("no-change plan = (%#v, %v)", plan, err)
	}
	conversation, err := fixture.db.Conversation(ctx, fixture.conversation)
	if err != nil || conversation.State != ConversationAnalysisComplete || conversation.TaskID != fixture.task.ID || conversation.PlanID != fixture.plan.ID {
		t.Fatalf("no-change conversation = (%#v, %v)", conversation, err)
	}
	page, err := fixture.db.ListShellChanges(ctx, fixture.plan.ID, 10, 0)
	if err != nil || page.Total != 0 || len(page.Changes) != 0 || page.HasMore {
		t.Fatalf("no-change page = (%#v, %v)", page, err)
	}
	var taskStatus TaskStatus
	if err := fixture.db.db.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id = ?`, fixture.task.ID).Scan(&taskStatus); err != nil || taskStatus != TaskSucceeded {
		t.Fatalf("no-change task status = (%s, %v)", taskStatus, err)
	}
}

func TestCreateNoChangeShellPlanRequiresSafeParentResult(t *testing.T) {
	for _, result := range []string{"", "执行命令 find . -type f", strings.Repeat("a", maxShellChangeMessage+1)} {
		t.Run(fmt.Sprintf("result_%d", len(result)), func(t *testing.T) {
			fixture := prepareShellFixture(t, 0, true)
			fixture.plan.Operations[0].ResultMessage = result
			if err := fixture.db.CreateConversationShellTaskPlan(context.Background(), fixture.conversation, fixture.task, fixture.plan, fixture.run, fixture.changes, fixture.audit, fixture.reply); err == nil {
				t.Fatal("invalid no-change parent result was accepted")
			}
			var count int
			if err := fixture.db.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM tasks WHERE id = ?`, fixture.task.ID).Scan(&count); err != nil || count != 0 {
				t.Fatalf("task count after invalid result = (%d, %v)", count, err)
			}
		})
	}
}

func TestShellChangesRejectContentInOriginalVersionAndEnforceLimit(t *testing.T) {
	validFile := `{"fingerprint":{"exists":true,"kind":"file","size":3,"mode":420},"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","mode":420,"isDirectory":false,"size":3}`
	for _, test := range []struct {
		name string
		json string
		kind string
		want bool
	}{
		{name: "modified file", json: validFile, kind: "modified", want: true},
		{name: "deleted file", json: validFile, kind: "deleted", want: true},
		{name: "added file", json: `{"fingerprint":{"exists":false},"sha256":"","mode":0,"isDirectory":false,"size":0}`, kind: "added", want: true},
		{name: "content field", json: `{"fingerprint":{"exists":true},"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","mode":420,"isDirectory":false,"size":3,"content":"secret"}`, kind: "modified"},
		{name: "missing exists marker", json: `{"fingerprint":{},"sha256":"","mode":0,"isDirectory":false,"size":0}`, kind: "added"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := validShellOriginalState(test.json, test.kind); got != test.want {
				t.Fatalf("validShellOriginalState() = %t, want %t", got, test.want)
			}
		})
	}

	fixture := prepareShellFixture(t, 0, false)
	tooMany := make([]ShellChangeRecord, MaxShellChanges+1)
	if err := fixture.db.CreateConversationShellTaskPlan(context.Background(), fixture.conversation, fixture.task, fixture.plan, fixture.run, tooMany, fixture.audit, fixture.reply); err == nil {
		t.Fatal("oversized Shell change list was accepted")
	}
	var taskCount int
	if err := fixture.db.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM tasks WHERE id = ?`, fixture.task.ID).Scan(&taskCount); err != nil || taskCount != 0 {
		t.Fatalf("task count after oversized request = (%d, %v)", taskCount, err)
	}
}
