// 本文件验证 Agent 会话在计划终态后的续聊门禁。
package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestStartConversationTurnAfterTerminalPlan(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "filesage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	at := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	if err := db.CreateConversation(ctx, "conversation-followup", "重命名文件", at); err != nil {
		t.Fatal(err)
	}
	if err := db.PutTask(ctx, TaskRecord{ID: "task-followup", Kind: "agent", Status: TaskSucceeded, UpdatedAt: at}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `
		INSERT INTO plans (id, task_id, version, status, data_scope_json, created_at_ns, updated_at_ns)
		VALUES ('plan-followup', 'task-followup', 1, 'completed', '{}', 1, 1);
		INSERT INTO operations (
			id, plan_id, ordinal, action, source_path, target_path, source_state_json,
			target_conflict_policy, approval_state, approved_by, approved_at_ns,
			execution_status, result_message, created_at_ns, updated_at_ns
		) VALUES ('operation-followup', 'plan-followup', 0, 'rename', 'old.txt', 'new.txt', '{}',
			'fail', 'approved', 'user', 1, 'succeeded', '服务端确认改名完成', 1, 1);
		UPDATE agent_conversations SET state = 'awaiting_approval', task_id = 'task-followup', plan_id = 'plan-followup'
		WHERE id = 'conversation-followup';`); err != nil {
		t.Fatal(err)
	}

	if err := db.StartConversationTurn(ctx, "conversation-followup", "现在也整理另一个文件", at.Add(time.Second)); err != nil {
		t.Fatalf("StartConversationTurn() = %v", err)
	}
	conversation, err := db.Conversation(ctx, "conversation-followup")
	if err != nil {
		t.Fatal(err)
	}
	if conversation.State != ConversationProcessing || conversation.TaskID != "" || conversation.PlanID != "" {
		t.Fatalf("conversation did not start a clean follow-up: %#v", conversation)
	}
	if len(conversation.Messages) != 3 || conversation.Messages[1].Role != "assistant" || conversation.Messages[2].Role != "user" {
		t.Fatalf("follow-up transcript = %#v", conversation.Messages)
	}
	if !strings.Contains(conversation.Messages[1].Content, "共 1 项：完成 1") || !strings.Contains(conversation.Messages[1].Content, "重命名：old.txt → new.txt（完成）：服务端确认改名完成") {
		t.Fatalf("missing plan result receipt: %q", conversation.Messages[1].Content)
	}
}

func TestShellResultIsAvailableOnlyToTheNextConversationTurn(t *testing.T) {
	fixture := createShellFixture(t, 0, true)
	ctx := context.Background()
	if err := fixture.db.StartConversationTurn(ctx, fixture.conversation, "根据刚才的命令结果继续", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	conversation, err := fixture.db.Conversation(ctx, fixture.conversation)
	if err != nil || conversation.ToolContextPlanID != fixture.plan.ID {
		t.Fatalf("Shell tool context = (%q, %v), want %q", conversation.ToolContextPlanID, err, fixture.plan.ID)
	}
	if err := fixture.db.FinishConversationTurn(ctx, fixture.conversation, ConversationClarifying, "请说明下一步。", AnalysisCard{}, "", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	conversation, err = fixture.db.Conversation(ctx, fixture.conversation)
	if err != nil || conversation.ToolContextPlanID != "" {
		t.Fatalf("consumed Shell tool context = (%q, %v), want empty", conversation.ToolContextPlanID, err)
	}
}

func TestBoundedToolResultKeepsValidUTF8AndOneLine(t *testing.T) {
	got := boundedToolResult("  操作\n已完成  ", 32)
	if got != "操作 已完成" {
		t.Fatalf("boundedToolResult() = %q", got)
	}
	got = boundedToolResult("中文结果较长", 5)
	if !strings.HasSuffix(got, "…") || !utf8.ValidString(got) {
		t.Fatalf("boundedToolResult() = %q, want valid truncated text", got)
	}
}

func TestConversationPlanReceiptReportsOmittedLongPath(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "filesage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	at := time.Now().UTC()
	if err := db.PutTask(ctx, TaskRecord{ID: "task-long-path", Kind: "agent", Status: TaskSucceeded, UpdatedAt: at}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `
		INSERT INTO plans (id, task_id, version, status, data_scope_json, created_at_ns, updated_at_ns)
		VALUES ('plan-long-path', 'task-long-path', 1, 'completed', '{}', 1, 1);
		INSERT INTO operations (
			id, plan_id, ordinal, action, source_path, target_path, source_state_json,
			target_conflict_policy, approval_state, approved_by, approved_at_ns,
			execution_status, created_at_ns, updated_at_ns
		) VALUES ('operation-long-path', 'plan-long-path', 0, 'rename', ?, 'target.txt', '{}',
			'fail', 'approved', 'user', 1, 'succeeded', 1, 1);`, strings.Repeat("a", 3600)); err != nil {
		t.Fatal(err)
	}
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := conversationPlanReceipt(ctx, tx, "plan-long-path", PlanCompleted)
	_ = tx.Rollback()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(receipt, "共 1 项：完成 1") || !strings.Contains(receipt, "1 项未附详细回执，详情见任务记录") || strings.Contains(receipt, "另有 1 项") {
		t.Fatalf("receipt should identify all omitted operations: %q", receipt)
	}
}

func TestConversationActionLabelsCoverPersistedToolActions(t *testing.T) {
	for action, want := range map[string]string{
		"archive_photo":   "归档照片",
		"trash_duplicate": "清理重复文件",
	} {
		if got := conversationActionLabel(action); got != want {
			t.Errorf("conversationActionLabel(%q) = %q, want %q", action, got, want)
		}
	}
}

func TestStartConversationTurnRejectsActivePlan(t *testing.T) {
	for _, status := range []PlanStatus{PlanDraft, PlanAwaitingApproval, PlanApproved, PlanExecuting} {
		t.Run(string(status), func(t *testing.T) {
			ctx := context.Background()
			db, err := Open(ctx, filepath.Join(t.TempDir(), "filesage.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := db.CreateConversation(ctx, "conversation-active", "整理文件", time.Now()); err != nil {
				t.Fatal(err)
			}
			if err := db.PutTask(ctx, TaskRecord{ID: "task-active", Kind: "agent", Status: TaskAwaitingApproval}); err != nil {
				t.Fatal(err)
			}
			if _, err := db.db.ExecContext(ctx, `
				INSERT INTO plans (id, task_id, version, status, data_scope_json, created_at_ns, updated_at_ns)
				VALUES ('plan-active', 'task-active', 1, ?, '{}', 1, 1);
				UPDATE agent_conversations SET state = 'awaiting_approval', task_id = 'task-active', plan_id = 'plan-active'
				WHERE id = 'conversation-active';`, status); err != nil {
				t.Fatal(err)
			}
			if err := db.StartConversationTurn(ctx, "conversation-active", "继续", time.Now()); !errors.Is(err, ErrConversationConflict) {
				t.Fatalf("StartConversationTurn() = %v, want conflict", err)
			}
		})
	}
}

func TestStartConversationTurnKeepsLimitFailureRecoverable(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "filesage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	at := time.Now().UTC()
	if err := db.CreateConversation(ctx, "conversation-limit", "初始消息", at); err != nil {
		t.Fatal(err)
	}
	for sequence := 2; sequence <= maxConversationMessages-1; sequence++ {
		if err := db.insertConversationMessage(ctx, db.db, "conversation-limit", sequence, "assistant", "历史回复", at); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.db.ExecContext(ctx, `UPDATE agent_conversations SET state = 'clarifying' WHERE id = ?`, "conversation-limit"); err != nil {
		t.Fatal(err)
	}
	if err := db.StartConversationTurn(ctx, "conversation-limit", "继续", at); !errors.Is(err, ErrConversationLimit) {
		t.Fatalf("StartConversationTurn() = %v, want message limit", err)
	}
	conversation, err := db.Conversation(ctx, "conversation-limit")
	if err != nil || conversation.State != ConversationClarifying || len(conversation.Messages) != maxConversationMessages-1 {
		t.Fatalf("limit failure mutated conversation: state=%s messages=%d err=%v", conversation.State, len(conversation.Messages), err)
	}
}

func TestStartConversationTurnReservesCompletePlanTurn(t *testing.T) {
	for _, test := range []struct {
		name         string
		messageCount int
		linkedPlan   bool
		wantAllowed  bool
	}{
		{name: "conversation without plan at boundary", messageCount: 97, wantAllowed: true},
		{name: "conversation without plan over boundary", messageCount: 98},
		{name: "terminal plan at boundary", messageCount: 96, linkedPlan: true, wantAllowed: true},
		{name: "terminal plan over boundary", messageCount: 97, linkedPlan: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			db, err := Open(ctx, filepath.Join(t.TempDir(), "filesage.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			at := time.Now().UTC()
			if err := db.CreateConversation(ctx, "conversation-budget", "初始消息", at); err != nil {
				t.Fatal(err)
			}
			for sequence := 2; sequence <= test.messageCount; sequence++ {
				if err := db.insertConversationMessage(ctx, db.db, "conversation-budget", sequence, "assistant", "历史回复", at); err != nil {
					t.Fatal(err)
				}
			}
			state := ConversationClarifying
			if test.linkedPlan {
				state = ConversationAwaitingApproval
				if err := db.PutTask(ctx, TaskRecord{ID: "task-budget", Kind: "agent", Status: TaskSucceeded, UpdatedAt: at}); err != nil {
					t.Fatal(err)
				}
				if _, err := db.db.ExecContext(ctx, `
					INSERT INTO plans (id, task_id, version, status, data_scope_json, created_at_ns, updated_at_ns)
					VALUES ('plan-budget', 'task-budget', 1, 'completed', '{}', 1, 1);
					UPDATE agent_conversations SET state = ?, task_id = 'task-budget', plan_id = 'plan-budget'
					WHERE id = 'conversation-budget';`, state); err != nil {
					t.Fatal(err)
				}
			} else if _, err := db.db.ExecContext(ctx, `UPDATE agent_conversations SET state = ? WHERE id = ?`, state, "conversation-budget"); err != nil {
				t.Fatal(err)
			}

			err = db.StartConversationTurn(ctx, "conversation-budget", "继续", at.Add(time.Second))
			if test.wantAllowed {
				if err != nil {
					t.Fatalf("StartConversationTurn() = %v", err)
				}
				if err := db.FinishConversationTurn(ctx, "conversation-budget", ConversationAwaitingConfirmation, "请确认新建文件夹。", AnalysisCard{
					Goal: "新建归档文件夹", Operations: []AnalysisOperation{{Action: "create_directory", TargetPath: "new-folder"}},
					Provider: "test", Model: "model", Destination: "local",
				}, "", at.Add(2*time.Second)); err != nil {
					t.Fatalf("FinishConversationTurn() = %v", err)
				}
				if claimed, err := db.BeginConversationAnalysis(ctx, "conversation-budget", at.Add(3*time.Second)); err != nil || !claimed {
					t.Fatalf("BeginConversationAnalysis() = (%t, %v)", claimed, err)
				}
				operation := OperationRecord{ID: "operation-new", Ordinal: 0, Action: "create_directory", TargetPath: "new-folder", Approval: ApprovalPending, Status: OperationPending, CreatedAt: at, UpdatedAt: at}
				task := TaskRecord{ID: "task-new", Kind: "agent", Status: TaskAwaitingApproval, RequestText: "新建归档文件夹", ModelProvider: "test", ModelName: "model", CreatedAt: at, UpdatedAt: at}
				plan := PlanRecord{ID: "plan-new", TaskID: task.ID, Version: 1, Status: PlanAwaitingApproval, ModelProvider: "test", ModelName: "model", DataScopeJSON: `{"proposalSource":"conversation"}`, CreatedAt: at, UpdatedAt: at, Operations: []OperationRecord{operation}}
				if err := db.CreateConversationTaskPlan(ctx, "conversation-budget", task, plan, AuditRecord{
					TaskID: task.ID, PlanID: plan.ID, OperationID: operation.ID, EventType: "plan_created", Outcome: "awaiting_approval", CreatedAt: at,
				}, "已生成执行计划，请审阅。"); err != nil {
					t.Fatalf("CreateConversationTaskPlan() = %v", err)
				}
				conversation, err := db.Conversation(ctx, "conversation-budget")
				if err != nil || len(conversation.Messages) != maxConversationMessages || conversation.State != ConversationAwaitingApproval || conversation.PlanID != plan.ID {
					t.Fatalf("completed turn messages=%d err=%v", len(conversation.Messages), err)
				}
				return
			}
			if !errors.Is(err, ErrConversationLimit) {
				t.Fatalf("StartConversationTurn() = %v, want message limit", err)
			}
			conversation, err := db.Conversation(ctx, "conversation-budget")
			if err != nil || len(conversation.Messages) != test.messageCount || conversation.State != state {
				t.Fatalf("limit failure mutated conversation: state=%s messages=%d err=%v", conversation.State, len(conversation.Messages), err)
			}
		})
	}
}
