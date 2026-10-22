// 本文件保存 Shell 隔离差异及其审核、执行状态。
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	MaxShellChanges        = 20_000
	MaxShellChangePageSize = 100
	maxShellCommandBytes   = 64 << 10
	maxShellChangeMessage  = 4096
	MaxShellOutputBytes    = 16 << 10
)

var ErrShellPagination = errors.New("invalid Shell change page")

type ShellRunStatus string

const (
	ShellRunAwaitingApproval ShellRunStatus = "awaiting_approval"
	ShellRunApproved         ShellRunStatus = "approved"
	ShellRunExecuting        ShellRunStatus = "executing"
	ShellRunCompleted        ShellRunStatus = "completed"
	ShellRunFailed           ShellRunStatus = "failed"
	ShellRunRejected         ShellRunStatus = "rejected"
	ShellRunCancelled        ShellRunStatus = "cancelled"
	ShellRunInvalidated      ShellRunStatus = "invalidated"
	ShellRunPartial          ShellRunStatus = "partial"
)

// ShellRunRecord 保存命令状态和限长输出，供审核界面及后续会话使用。
type ShellRunRecord struct {
	ID               string
	PlanID           string
	OperationID      string
	Command          string
	WorkingDirectory string
	Stdout           string
	Stderr           string
	OutputDiscarded  bool
	OutputTruncated  bool
	ExitCode         int
	StageID          string
	Status           ShellRunStatus
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// ShellChangeRecord 只保存逐路径版本摘要和审批状态。
type ShellChangeRecord struct {
	ID                string
	PlanID            string
	Ordinal           int
	Path              string
	Kind              string
	OriginalStateJSON string
	CandidateSHA256   string
	CandidateSize     int64
	CandidateMode     *uint32
	Binary            bool
	Truncated         bool
	PayloadID         string
	Approval          ApprovalState
	ApprovedBy        string
	ApprovedAt        *time.Time
	Status            OperationStatus
	ResultMessage     string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type ShellChangePage struct {
	Changes []ShellChangeRecord
	Total   int
	Limit   int
	Offset  int
	HasMore bool
}

// CreateConversationShellTaskPlan 原子保存会话、任务、单项方案、运行元数据、逐路径差异与审计。
func (s *Store) CreateConversationShellTaskPlan(ctx context.Context, conversationID string, task TaskRecord, plan PlanRecord, run ShellRunRecord, changes []ShellChangeRecord, audit AuditRecord, reply string) error {
	if !validShellID(conversationID) || task.ID == "" || plan.TaskID != task.ID || task.ScopePath != plan.ScopePath ||
		run.PlanID != plan.ID || audit.TaskID != task.ID || audit.PlanID != plan.ID || audit.OperationID != run.OperationID {
		return errors.New("Shell task, plan, run, and audit identifiers do not match")
	}
	noChanges := len(changes) == 0
	if task.Kind != "agent" || task.ModelProvider == "" || task.ModelName == "" || plan.Version < 1 || plan.ModelProvider != task.ModelProvider || plan.ModelName != task.ModelName {
		return errors.New("Shell task and plan must identify the model and await approval")
	}
	if noChanges {
		if task.Status != TaskSucceeded || plan.Status != PlanCompleted || run.Status != ShellRunCompleted || audit.Outcome != "completed" {
			return errors.New("no-change Shell run must be recorded as completed")
		}
	} else if task.Status != TaskAwaitingApproval || plan.Status != PlanAwaitingApproval || run.Status != ShellRunAwaitingApproval || len(changes) > MaxShellChanges || audit.Outcome != "awaiting_approval" {
		return errors.New("Shell changes must await approval")
	}
	if !validShellRun(run) || !validShellReply(reply) || reply != "" && (strings.Contains(reply, run.Command) || run.WorkingDirectory != "." && strings.Contains(reply, run.WorkingDirectory)) || audit.EventType != "shell_plan_created" ||
		len(audit.Message) > maxShellChangeMessage || !utf8.ValidString(audit.Message) ||
		strings.Contains(audit.Message, run.Command) || run.WorkingDirectory != "." && strings.Contains(audit.Message, run.WorkingDirectory) || audit.SourcePath != plan.ScopePath || audit.TargetPath != "" {
		return errors.New("Shell run, changes, audit, or reply is invalid")
	}
	if len(plan.Operations) != 1 {
		return errors.New("Shell plan must contain exactly one parent operation")
	}
	parent := plan.Operations[0]
	parentStateValid := parent.Approval == ApprovalPending && parent.Status == OperationPending && parent.ApprovedBy == "" && parent.ApprovedAt == nil
	parentResultValid := parent.ResultMessage == ""
	if noChanges {
		parentStateValid = (parent.Approval == ApprovalPending || parent.Approval == ApprovalApproved) && parent.Status == OperationSucceeded
		if parent.Approval == ApprovalApproved {
			parentStateValid = parent.ApprovedBy != "" && parent.ApprovedAt != nil
		} else {
			parentStateValid = parent.ApprovedBy == "" && parent.ApprovedAt == nil
		}
		parentResultValid = strings.TrimSpace(parent.ResultMessage) != "" && len(parent.ResultMessage) <= maxShellChangeMessage && utf8.ValidString(parent.ResultMessage) &&
			!strings.Contains(parent.ResultMessage, run.Command) && (run.WorkingDirectory == "." || !strings.Contains(parent.ResultMessage, run.WorkingDirectory))
	}
	if parent.ID != run.OperationID || parent.Ordinal != 0 || parent.Action != "run_shell" || parent.SourcePath != plan.ScopePath || parent.TargetPath != "" ||
		!parentStateValid || !parentResultValid || len(parent.Reason) > maxShellChangeMessage || !utf8.ValidString(parent.Reason) || strings.Contains(parent.Reason, run.Command) ||
		parent.Evidence != "" || strings.Contains(parent.Reason, run.WorkingDirectory) && run.WorkingDirectory != "." {
		return errors.New("Shell plan parent operation is invalid")
	}
	if len(changes) > 0 {
		if err := validateShellChanges(plan.ID, changes); err != nil {
			return err
		}
	}
	audit.Actor = "agent"
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin Shell plan transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var state ConversationState
	var provider, model, operationsJSON string
	var taskID, oldPlanID sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT state, provider, model, task_id, plan_id, operations_json FROM agent_conversations WHERE id = ?`, conversationID).
		Scan(&state, &provider, &model, &taskID, &oldPlanID, &operationsJSON); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrConversationNotFound
		}
		return fmt.Errorf("read conversation for Shell plan: %w", err)
	}
	if state != ConversationAnalyzing || taskID.Valid || oldPlanID.Valid || provider != task.ModelProvider || model != task.ModelName {
		return ErrConversationConflict
	}
	var confirmed []AnalysisOperation
	if json.Unmarshal([]byte(operationsJSON), &confirmed) != nil || !ValidAnalysisOperations(confirmed) || len(confirmed) != 1 ||
		confirmed[0].Action != "run_shell" || confirmed[0].ShellCommand != run.Command || confirmed[0].WorkingDir != run.WorkingDirectory ||
		confirmed[0].FilePath != plan.ScopePath {
		return ErrConversationConflict
	}
	if err := insertTaskRecord(ctx, tx, task); err != nil {
		return err
	}
	if err := createPlanRecord(ctx, tx, plan); err != nil {
		return err
	}
	if err := insertShellRun(ctx, tx, run); err != nil {
		return err
	}
	for _, change := range changes {
		if err := insertShellChange(ctx, tx, change); err != nil {
			return err
		}
		if err := appendShellAudit(ctx, tx, task.ID, plan.ID, run.OperationID, "shell_change_proposed", "awaiting_approval", change.Path, task.UpdatedAt); err != nil {
			return err
		}
	}
	if noChanges {
		if err := appendShellAudit(ctx, tx, task.ID, plan.ID, run.OperationID, "shell_run_completed", "completed", "", task.UpdatedAt); err != nil {
			return err
		}
	}
	if _, err := appendAuditRecord(ctx, tx, audit); err != nil {
		return err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_conversation_messages WHERE conversation_id = ?`, conversationID).Scan(&count); err != nil {
		return fmt.Errorf("count Shell conversation messages: %w", err)
	}
	if reply != "" && count >= maxConversationMessages {
		return ErrConversationLimit
	}
	updatedAt := timestampOrNow(task.UpdatedAt)
	nextConversationState := ConversationAwaitingApproval
	if noChanges {
		nextConversationState = ConversationAnalysisComplete
	}
	result, err := tx.ExecContext(ctx, `UPDATE agent_conversations SET state = ?, task_id = ?, plan_id = ?, error_code = '', tool_context_plan_id = '', updated_at_ns = ?
		WHERE id = ? AND state = 'analyzing' AND task_id IS NULL AND plan_id IS NULL`, nextConversationState, task.ID, plan.ID, updatedAt, conversationID)
	if err != nil {
		return fmt.Errorf("link Shell conversation to plan: %w", err)
	}
	if err := requireOneRow(result, "Shell conversation"); err != nil {
		return ErrConversationConflict
	}
	if reply != "" {
		if err := s.insertConversationMessage(ctx, tx, conversationID, count+1, "assistant", reply, task.UpdatedAt); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit Shell plan transaction: %w", err)
	}
	return nil
}

// ShellRun 返回方案对应的命令元数据和暂存状态。
func (s *Store) ShellRun(ctx context.Context, planID string) (ShellRunRecord, error) {
	if !validShellID(planID) {
		return ShellRunRecord{}, ErrNotFound
	}
	return scanShellRun(s.db.QueryRowContext(ctx, `SELECT id, plan_id, operation_id, command, working_directory, stdout, stderr, output_discarded, output_truncated, exit_code, stage_id, status, created_at_ns, updated_at_ns
		FROM shell_runs WHERE plan_id = ?`, planID))
}

// ListShellChanges 返回固定顺序的严格分页差异。
func (s *Store) ListShellChanges(ctx context.Context, planID string, limit, offset int) (ShellChangePage, error) {
	page := ShellChangePage{Limit: limit, Offset: offset, Changes: []ShellChangeRecord{}}
	if !validShellID(planID) || limit < 1 || limit > MaxShellChangePageSize || offset < 0 || offset > MaxShellChanges {
		return page, ErrShellPagination
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return page, fmt.Errorf("begin Shell change page: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM shell_runs WHERE plan_id = ?`, planID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return page, ErrNotFound
		}
		return page, fmt.Errorf("check Shell run: %w", err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM shell_changes WHERE plan_id = ?`, planID).Scan(&page.Total); err != nil {
		return page, fmt.Errorf("count Shell changes: %w", err)
	}
	if page.Total > MaxShellChanges || offset > page.Total {
		return page, ErrShellPagination
	}
	if page.Total == 0 {
		return page, nil
	}
	rows, err := tx.QueryContext(ctx, shellChangeSelect+` WHERE plan_id = ? ORDER BY ordinal LIMIT ? OFFSET ?`, planID, limit+1, offset)
	if err != nil {
		return page, fmt.Errorf("query Shell changes: %w", err)
	}
	for rows.Next() {
		change, scanErr := scanShellChange(rows)
		if scanErr != nil {
			_ = rows.Close()
			return page, scanErr
		}
		page.Changes = append(page.Changes, change)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return page, fmt.Errorf("iterate Shell changes: %w", err)
	}
	if err := rows.Close(); err != nil {
		return page, err
	}
	if len(page.Changes) > limit {
		page.Changes = page.Changes[:limit]
		page.HasMore = true
	}
	return page, nil
}

// SetShellChangeApproval 以方案版本 CAS 逐项审批，并在首次拒绝时终止整份方案。
func (s *Store) SetShellChangeApproval(ctx context.Context, planID, changeID string, decision ApprovalState, expectedVersion int, at time.Time) error {
	if decision != ApprovalApproved && decision != ApprovalRejected || !validShellID(planID) || !validShellID(changeID) || expectedVersion < 1 {
		return ErrConflict
	}
	at = conversationTime(at)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin Shell item approval: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var taskID, parentID string
	var version int
	var planStatus PlanStatus
	var operationStatus OperationStatus
	var runStatus ShellRunStatus
	if err := tx.QueryRowContext(ctx, `SELECT p.task_id, p.version, p.status, o.id, o.execution_status, r.status
		FROM plans p JOIN operations o ON o.plan_id = p.id JOIN shell_runs r ON r.plan_id = p.id
		WHERE p.id = ? AND o.action = 'run_shell'`, planID).
		Scan(&taskID, &version, &planStatus, &parentID, &operationStatus, &runStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("read Shell plan for approval: %w", err)
	}
	if version != expectedVersion || planStatus != PlanAwaitingApproval || operationStatus != OperationPending || runStatus != ShellRunAwaitingApproval {
		return ErrConflict
	}
	var pathValue string
	var approval ApprovalState
	var execution OperationStatus
	if err := tx.QueryRowContext(ctx, `SELECT path, approval_state, execution_status FROM shell_changes WHERE plan_id = ? AND id = ?`, planID, changeID).
		Scan(&pathValue, &approval, &execution); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("read Shell change for approval: %w", err)
	}
	if approval != ApprovalPending || execution != OperationPending {
		return ErrConflict
	}
	var approvedAt any
	approvedBy := ""
	if decision == ApprovalApproved {
		approvedBy, approvedAt = "user", at.UnixNano()
	}
	result, err := tx.ExecContext(ctx, `UPDATE shell_changes SET approval_state = ?, approved_by = ?, approved_at_ns = ?, updated_at_ns = ?
		WHERE plan_id = ? AND id = ? AND approval_state = 'pending' AND execution_status = 'pending'`,
		decision, approvedBy, approvedAt, at.UnixNano(), planID, changeID)
	if err != nil {
		return fmt.Errorf("update Shell change approval: %w", err)
	}
	if err := requireOneRow(result, "Shell change"); err != nil {
		return ErrConflict
	}
	event, outcome := "shell_change_approved", "approved"
	if decision == ApprovalRejected {
		event, outcome = "shell_change_rejected", "rejected"
	}
	if err := appendShellAudit(ctx, tx, taskID, planID, parentID, event, outcome, pathValue, at); err != nil {
		return err
	}
	if decision == ApprovalRejected {
		rows, err := tx.QueryContext(ctx, `SELECT id, path FROM shell_changes WHERE plan_id = ? AND id <> ? AND execution_status = 'pending' AND approval_state IN ('pending', 'approved') ORDER BY ordinal`, planID, changeID)
		if err != nil {
			return fmt.Errorf("list Shell changes to cancel: %w", err)
		}
		type cancelled struct{ id, path string }
		var others []cancelled
		for rows.Next() {
			var item cancelled
			if err := rows.Scan(&item.id, &item.path); err != nil {
				_ = rows.Close()
				return err
			}
			others = append(others, item)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, item := range others {
			result, err := tx.ExecContext(ctx, `UPDATE shell_changes SET approval_state = 'invalidated', approved_by = '', approved_at_ns = NULL,
				execution_status = 'cancelled', result_message = '整份 Shell 方案已拒绝', updated_at_ns = ?
				WHERE plan_id = ? AND id = ? AND execution_status = 'pending' AND approval_state IN ('pending', 'approved')`, at.UnixNano(), planID, item.id)
			if err != nil {
				return fmt.Errorf("cancel Shell change: %w", err)
			}
			if err := requireOneRow(result, "cancelled Shell change"); err != nil {
				return ErrConflict
			}
			if err := appendShellAudit(ctx, tx, taskID, planID, parentID, "shell_change_cancelled", "invalidated", item.path, at); err != nil {
				return err
			}
		}
		if err := execAgentOne(ctx, tx, `UPDATE operations SET approval_state = 'rejected', execution_status = 'skipped', result_message = 'Shell 方案被用户拒绝', updated_at_ns = ?
			WHERE id = ? AND plan_id = ? AND action = 'run_shell' AND approval_state = 'pending' AND execution_status = 'pending'`, at.UnixNano(), parentID, planID); err != nil {
			return ErrConflict
		}
		if err := execAgentOne(ctx, tx, `UPDATE plans SET status = 'rejected', updated_at_ns = ? WHERE id = ? AND version = ? AND status = 'awaiting_approval'`, at.UnixNano(), planID, expectedVersion); err != nil {
			return ErrConflict
		}
		if err := execAgentOne(ctx, tx, `UPDATE tasks SET status = 'cancelled', updated_at_ns = ? WHERE id = ? AND status = 'awaiting_approval'`, at.UnixNano(), taskID); err != nil {
			return ErrConflict
		}
		if err := execAgentOne(ctx, tx, `UPDATE shell_runs SET status = 'rejected', updated_at_ns = ? WHERE plan_id = ? AND status = 'awaiting_approval'`, at.UnixNano(), planID); err != nil {
			return ErrConflict
		}
		if err := appendShellAudit(ctx, tx, taskID, planID, parentID, "shell_plan_rejected", "rejected", "", at); err != nil {
			return err
		}
	} else {
		var pending int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM shell_changes WHERE plan_id = ? AND approval_state = 'pending'`, planID).Scan(&pending); err != nil {
			return err
		}
		if pending == 0 {
			if err := execAgentOne(ctx, tx, `UPDATE operations SET approval_state = 'approved', approved_by = 'user', approved_at_ns = ?, updated_at_ns = ?
				WHERE id = ? AND plan_id = ? AND action = 'run_shell' AND approval_state = 'pending' AND execution_status = 'pending'`, at.UnixNano(), at.UnixNano(), parentID, planID); err != nil {
				return ErrConflict
			}
			if err := execAgentOne(ctx, tx, `UPDATE plans SET status = 'approved', updated_at_ns = ? WHERE id = ? AND version = ? AND status = 'awaiting_approval'`, at.UnixNano(), planID, expectedVersion); err != nil {
				return ErrConflict
			}
			if err := execAgentOne(ctx, tx, `UPDATE tasks SET status = 'paused', updated_at_ns = ? WHERE id = ? AND status = 'awaiting_approval'`, at.UnixNano(), taskID); err != nil {
				return ErrConflict
			}
			if err := execAgentOne(ctx, tx, `UPDATE shell_runs SET status = 'approved', updated_at_ns = ? WHERE plan_id = ? AND status = 'awaiting_approval'`, at.UnixNano(), planID); err != nil {
				return ErrConflict
			}
			if err := appendShellAudit(ctx, tx, taskID, planID, parentID, "shell_plan_approved", "approved", "", at); err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit Shell change approval: %w", err)
	}
	return nil
}

// ReviewShellPlan 批准整个隔离执行差异集，将 Shell 作为一个受控工具步骤。
func (s *Store) ReviewShellPlan(ctx context.Context, planID string, expectedVersion int, expectedMode string, at time.Time) (string, error) {
	if !validShellID(planID) || expectedVersion < 1 || expectedMode != "bypass" && expectedMode != "manual" {
		return "", ErrConflict
	}
	at = conversationTime(at)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin Shell plan review: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var taskID, parentID string
	var version int
	var planStatus PlanStatus
	var operationApproval ApprovalState
	var operationStatus OperationStatus
	var runStatus ShellRunStatus
	if err := tx.QueryRowContext(ctx, `SELECT p.task_id, p.version, p.status, o.id, o.approval_state, o.execution_status, r.status
		FROM plans p JOIN operations o ON o.plan_id = p.id JOIN shell_runs r ON r.plan_id = p.id
		WHERE p.id = ? AND o.action = 'run_shell'`, planID).
		Scan(&taskID, &version, &planStatus, &parentID, &operationApproval, &operationStatus, &runStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("read Shell plan for review: %w", err)
	}
	if version != expectedVersion || planStatus != PlanAwaitingApproval || operationApproval != ApprovalPending ||
		operationStatus != OperationPending || runStatus != ShellRunAwaitingApproval {
		return "", ErrConflict
	}
	var bypass int
	if err := tx.QueryRowContext(ctx, `SELECT bypass_permission FROM agent_settings WHERE id = 1`).Scan(&bypass); err != nil {
		return "", fmt.Errorf("read Agent permission setting for Shell review: %w", err)
	}
	mode := "manual"
	if bypass == 1 {
		mode = "bypass"
	}
	if mode != expectedMode {
		return "", ErrConflict
	}
	var total, pending int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(CASE WHEN approval_state = 'pending' AND execution_status = 'pending' THEN 1 ELSE 0 END), 0)
		FROM shell_changes WHERE plan_id = ?`, planID).Scan(&total, &pending); err != nil {
		return "", err
	}
	if total < 1 || total > MaxShellChanges || pending != total {
		return "", ErrConflict
	}
	rows, err := tx.QueryContext(ctx, `SELECT path FROM shell_changes WHERE plan_id = ? ORDER BY ordinal`, planID)
	if err != nil {
		return "", err
	}
	paths := make([]string, 0, total)
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			_ = rows.Close()
			return "", err
		}
		paths = append(paths, value)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return "", err
	}
	if err := rows.Close(); err != nil {
		return "", err
	}
	result, err := tx.ExecContext(ctx, `UPDATE shell_changes SET approval_state = 'approved', approved_by = 'user', approved_at_ns = ?, updated_at_ns = ?
		WHERE plan_id = ? AND approval_state = 'pending' AND execution_status = 'pending'`, at.UnixNano(), at.UnixNano(), planID)
	if err != nil {
		return "", fmt.Errorf("approve reviewed Shell changes: %w", err)
	}
	if count, countErr := result.RowsAffected(); countErr != nil || count != int64(total) {
		return "", ErrConflict
	}
	if err := execAgentOne(ctx, tx, `UPDATE operations SET approval_state = 'approved', approved_by = 'user', approved_at_ns = ?, updated_at_ns = ?
		WHERE id = ? AND plan_id = ? AND action = 'run_shell' AND approval_state = 'pending' AND execution_status = 'pending'`,
		at.UnixNano(), at.UnixNano(), parentID, planID); err != nil {
		return "", ErrConflict
	}
	if err := execAgentOne(ctx, tx, `UPDATE plans SET status = 'approved', execution_mode = ?, reviewed_at_ns = ?, updated_at_ns = ?
		WHERE id = ? AND version = ? AND status = 'awaiting_approval' AND reviewed_at_ns IS NULL`,
		mode, at.UnixNano(), at.UnixNano(), planID, expectedVersion); err != nil {
		return "", ErrConflict
	}
	if err := execAgentOne(ctx, tx, `UPDATE tasks SET status = 'paused', updated_at_ns = ? WHERE id = ? AND status = 'awaiting_approval'`, at.UnixNano(), taskID); err != nil {
		return "", ErrConflict
	}
	if err := execAgentOne(ctx, tx, `UPDATE shell_runs SET status = 'approved', updated_at_ns = ? WHERE plan_id = ? AND status = 'awaiting_approval'`, at.UnixNano(), planID); err != nil {
		return "", ErrConflict
	}
	for _, value := range paths {
		if err := appendShellAudit(ctx, tx, taskID, planID, parentID, "shell_change_approved", "approved", value, at); err != nil {
			return "", err
		}
	}
	outcome, message := "manual", "用户审核 Shell 计划；手动模式需确认 Shell 步骤后提交隔离差异"
	if mode == "bypass" {
		outcome, message = "bypass", "用户审核 Shell 计划；隔离执行变化将作为一个工具步骤自动提交"
	}
	if _, err := appendAuditRecord(ctx, tx, AuditRecord{
		TaskID: taskID, PlanID: planID, OperationID: parentID,
		EventType: "plan_reviewed", Actor: "user", Outcome: outcome, Message: message, CreatedAt: at,
	}); err != nil {
		return "", err
	}
	if err := appendShellAudit(ctx, tx, taskID, planID, parentID, "shell_plan_approved", "approved", "", at); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit Shell plan review: %w", err)
	}
	return mode, nil
}

// StartShellExecution 原子启动已批准的 Shell 父操作与暂存运行。
func (s *Store) StartShellExecution(ctx context.Context, planID string, expectedVersion int, at time.Time) error {
	if !validShellID(planID) || expectedVersion < 1 {
		return ErrConflict
	}
	at = conversationTime(at)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin Shell execution: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var taskID, parentID string
	var version, stopRequested int
	var planStatus PlanStatus
	var operationApproval ApprovalState
	var operationStatus OperationStatus
	var runStatus ShellRunStatus
	if err := tx.QueryRowContext(ctx, `SELECT p.task_id, p.version, p.status, p.stop_requested, o.id, o.approval_state, o.execution_status, r.status
		FROM plans p JOIN operations o ON o.plan_id = p.id JOIN shell_runs r ON r.plan_id = p.id
		WHERE p.id = ? AND o.action = 'run_shell'`, planID).
		Scan(&taskID, &version, &planStatus, &stopRequested, &parentID, &operationApproval, &operationStatus, &runStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("read Shell execution state: %w", err)
	}
	if version != expectedVersion || planStatus != PlanApproved || stopRequested != 0 || operationApproval != ApprovalApproved ||
		operationStatus != OperationPending || runStatus != ShellRunApproved {
		return ErrConflict
	}
	var total, approved, pending int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(CASE WHEN approval_state = 'approved' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN execution_status = 'pending' THEN 1 ELSE 0 END), 0) FROM shell_changes WHERE plan_id = ?`, planID).
		Scan(&total, &approved, &pending); err != nil {
		return err
	}
	if total < 1 || total > MaxShellChanges || approved != total || pending != total {
		return ErrConflict
	}
	if err := execAgentOne(ctx, tx, `UPDATE operations SET execution_status = 'running', updated_at_ns = ? WHERE id = ? AND plan_id = ? AND action = 'run_shell' AND approval_state = 'approved' AND execution_status = 'pending'`, at.UnixNano(), parentID, planID); err != nil {
		return ErrConflict
	}
	if err := execAgentOne(ctx, tx, `UPDATE plans SET status = 'executing', updated_at_ns = ? WHERE id = ? AND version = ? AND status = 'approved'`, at.UnixNano(), planID, expectedVersion); err != nil {
		return ErrConflict
	}
	if err := execAgentOne(ctx, tx, `UPDATE tasks SET status = 'running', updated_at_ns = ? WHERE id = ? AND status = 'paused'`, at.UnixNano(), taskID); err != nil {
		return ErrConflict
	}
	if err := execAgentOne(ctx, tx, `UPDATE shell_runs SET status = 'executing', updated_at_ns = ? WHERE plan_id = ? AND status = 'approved'`, at.UnixNano(), planID); err != nil {
		return ErrConflict
	}
	if err := appendAgentAudit(ctx, tx, taskID, planID, parentID, "operation_started", "started", "", at); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit Shell execution start: %w", err)
	}
	return nil
}

// InvalidateShellPlan 原子使 Shell 方案及全部逐项审批因文件快照冲突失效。
func (s *Store) InvalidateShellPlan(ctx context.Context, planID, changeID, message string, at time.Time) error {
	if !validShellID(planID) || !validShellID(changeID) || len(message) == 0 || len(message) > maxShellChangeMessage || !utf8.ValidString(message) {
		return ErrConflict
	}
	at = conversationTime(at)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin Shell plan invalidation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var taskID, parentID string
	var version, completed int
	var planStatus PlanStatus
	var operationStatus OperationStatus
	var runStatus ShellRunStatus
	if err := tx.QueryRowContext(ctx, `SELECT p.task_id, p.version, p.status, o.id, o.execution_status, r.status,
		(SELECT COUNT(*) FROM shell_changes c WHERE c.plan_id = p.id AND c.execution_status <> 'pending')
		FROM plans p JOIN operations o ON o.plan_id = p.id JOIN shell_runs r ON r.plan_id = p.id
		WHERE p.id = ? AND o.action = 'run_shell'`, planID).
		Scan(&taskID, &version, &planStatus, &parentID, &operationStatus, &runStatus, &completed); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("read Shell plan for invalidation: %w", err)
	}
	if completed != 0 || planStatus != PlanAwaitingApproval && planStatus != PlanApproved && planStatus != PlanExecuting ||
		runStatus != ShellRunAwaitingApproval && runStatus != ShellRunApproved && runStatus != ShellRunExecuting ||
		operationStatus != OperationPending && operationStatus != OperationRunning {
		return ErrConflict
	}
	var conflictPath string
	if err := tx.QueryRowContext(ctx, `SELECT path FROM shell_changes WHERE plan_id = ? AND id = ? AND execution_status = 'pending'`, planID, changeID).Scan(&conflictPath); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, path FROM shell_changes WHERE plan_id = ? AND execution_status = 'pending' ORDER BY ordinal`, planID)
	if err != nil {
		return err
	}
	type staleChange struct{ id, path string }
	var changes []staleChange
	for rows.Next() {
		var item staleChange
		if err := rows.Scan(&item.id, &item.path); err != nil {
			_ = rows.Close()
			return err
		}
		changes = append(changes, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range changes {
		changeStatus, resultMessage, outcome := "cancelled", "Shell 方案因文件状态变化而失效", "invalidated"
		if item.id == changeID {
			changeStatus, resultMessage, outcome = "skipped", message, "stale"
		}
		result, err := tx.ExecContext(ctx, `UPDATE shell_changes SET approval_state = 'invalidated', approved_by = '', approved_at_ns = NULL,
			execution_status = ?, result_message = ?, updated_at_ns = ? WHERE plan_id = ? AND id = ? AND execution_status = 'pending'`,
			changeStatus, resultMessage, at.UnixNano(), planID, item.id)
		if err != nil {
			return err
		}
		if err := requireOneRow(result, "invalidated Shell change"); err != nil {
			return ErrConflict
		}
		if err := appendShellAudit(ctx, tx, taskID, planID, parentID, "shell_change_invalidated", outcome, item.path, at); err != nil {
			return err
		}
	}
	if err := execAgentOne(ctx, tx, `UPDATE operations SET approval_state = 'invalidated', approved_by = '', approved_at_ns = NULL,
		execution_status = 'skipped', result_message = ?, updated_at_ns = ? WHERE id = ? AND plan_id = ? AND action = 'run_shell' AND execution_status IN ('pending', 'running') AND approval_state IN ('pending', 'approved')`,
		message, at.UnixNano(), parentID, planID); err != nil {
		return ErrConflict
	}
	if err := execAgentOne(ctx, tx, `UPDATE plans SET status = 'invalidated', updated_at_ns = ? WHERE id = ? AND version = ? AND status IN ('awaiting_approval', 'approved', 'executing')`, at.UnixNano(), planID, version); err != nil {
		return ErrConflict
	}
	if err := execAgentOne(ctx, tx, `UPDATE tasks SET status = 'failed', updated_at_ns = ? WHERE id = ? AND status IN ('awaiting_approval', 'paused', 'running')`, at.UnixNano(), taskID); err != nil {
		return ErrConflict
	}
	if err := execAgentOne(ctx, tx, `UPDATE shell_runs SET status = 'invalidated', updated_at_ns = ? WHERE plan_id = ? AND status IN ('awaiting_approval', 'approved', 'executing')`, at.UnixNano(), planID); err != nil {
		return ErrConflict
	}
	if _, err := tx.ExecContext(ctx, `UPDATE agent_conversations SET state = 'failed', error_code = 'file_conflict', updated_at_ns = ? WHERE task_id = ? AND plan_id = ? AND state = 'awaiting_approval'`, at.UnixNano(), taskID, planID); err != nil {
		return err
	}
	if err := appendShellAudit(ctx, tx, taskID, planID, parentID, "shell_plan_invalidated", "stale", conflictPath, at); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit Shell plan invalidation: %w", err)
	}
	return nil
}

// FinishShellExecution 原子结束父操作、任务、方案和运行，并取消未提交差异。
func (s *Store) FinishShellExecution(ctx context.Context, planID string, status ShellRunStatus, message string, expectedVersion int, at time.Time) error {
	if !validShellID(planID) || expectedVersion < 1 || status != ShellRunCompleted && status != ShellRunFailed && status != ShellRunPartial && status != ShellRunCancelled ||
		len(message) > maxShellChangeMessage || !utf8.ValidString(message) {
		return errors.New("invalid Shell execution result")
	}
	operationResult := OperationSucceeded
	planResult, taskResult := PlanCompleted, TaskSucceeded
	switch status {
	case ShellRunFailed:
		operationResult, planResult, taskResult = OperationFailed, PlanFailed, TaskFailed
	case ShellRunPartial:
		operationResult, planResult, taskResult = OperationPartial, PlanFailed, TaskPartial
	case ShellRunCancelled:
		operationResult, planResult, taskResult = OperationCancelled, PlanCancelled, TaskCancelled
	}
	at = conversationTime(at)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin Shell execution finish: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var taskID, parentID string
	var version int
	var planStatus PlanStatus
	var runStatus ShellRunStatus
	var operationStatus OperationStatus
	if err := tx.QueryRowContext(ctx, `SELECT p.task_id, p.version, p.status, o.id, o.execution_status, r.status
		FROM plans p JOIN operations o ON o.plan_id = p.id JOIN shell_runs r ON r.plan_id = p.id
		WHERE p.id = ? AND o.action = 'run_shell'`, planID).
		Scan(&taskID, &version, &planStatus, &parentID, &operationStatus, &runStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("read Shell execution result state: %w", err)
	}
	if version != expectedVersion || planStatus != PlanExecuting || operationStatus != OperationRunning || runStatus != ShellRunExecuting {
		return ErrConflict
	}
	var total, succeeded, failed, running int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(CASE WHEN execution_status = 'succeeded' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN execution_status = 'failed' OR execution_status = 'partial' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN execution_status = 'running' THEN 1 ELSE 0 END), 0) FROM shell_changes WHERE plan_id = ?`, planID).
		Scan(&total, &succeeded, &failed, &running); err != nil {
		return err
	}
	if total < 1 || total > MaxShellChanges || running != 0 {
		return ErrConflict
	}
	if status == ShellRunCompleted && succeeded != total || status == ShellRunFailed && (failed == 0 || succeeded != 0) ||
		status == ShellRunPartial && (succeeded == 0 || failed == 0) {
		return ErrConflict
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, path FROM shell_changes WHERE plan_id = ? AND execution_status = 'pending' ORDER BY ordinal`, planID)
	if err != nil {
		return err
	}
	type cancelledChange struct{ id, path string }
	var pending []cancelledChange
	for rows.Next() {
		var item cancelledChange
		if err := rows.Scan(&item.id, &item.path); err != nil {
			_ = rows.Close()
			return err
		}
		pending = append(pending, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range pending {
		result, err := tx.ExecContext(ctx, `UPDATE shell_changes SET approval_state = 'invalidated', approved_by = '', approved_at_ns = NULL,
			execution_status = 'cancelled', result_message = ?, updated_at_ns = ? WHERE plan_id = ? AND id = ? AND execution_status = 'pending'`,
			"Shell 执行结束，未提交此项", at.UnixNano(), planID, item.id)
		if err != nil {
			return err
		}
		if err := requireOneRow(result, "cancelled Shell change"); err != nil {
			return ErrConflict
		}
		if err := appendShellAudit(ctx, tx, taskID, planID, parentID, "shell_change_execution_cancelled", "cancelled", item.path, at); err != nil {
			return err
		}
	}
	if err := execAgentOne(ctx, tx, `UPDATE operations SET execution_status = ?, result_message = ?, updated_at_ns = ? WHERE id = ? AND plan_id = ? AND execution_status = 'running'`, operationResult, message, at.UnixNano(), parentID, planID); err != nil {
		return ErrConflict
	}
	if err := execAgentOne(ctx, tx, `UPDATE shell_runs SET status = ?, updated_at_ns = ? WHERE plan_id = ? AND status = 'executing'`, status, at.UnixNano(), planID); err != nil {
		return ErrConflict
	}
	if err := execAgentOne(ctx, tx, `UPDATE plans SET status = ?, updated_at_ns = ? WHERE id = ? AND version = ? AND status = 'executing'`, planResult, at.UnixNano(), planID, expectedVersion); err != nil {
		return ErrConflict
	}
	if err := execAgentOne(ctx, tx, `UPDATE tasks SET status = ?, updated_at_ns = ? WHERE id = ? AND status = 'running'`, taskResult, at.UnixNano(), taskID); err != nil {
		return ErrConflict
	}
	if err := appendAgentAudit(ctx, tx, taskID, planID, parentID, "operation_finished", string(operationResult), message, at); err != nil {
		return err
	}
	if err := appendAgentAudit(ctx, tx, taskID, planID, parentID, "shell_run_finished", string(status), message, at); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit Shell execution finish: %w", err)
	}
	return nil
}

// StartShellChange 标记一项已批准差异开始执行并记录其路径审计。
func (s *Store) StartShellChange(ctx context.Context, planID, changeID string, expectedVersion int, at time.Time) error {
	return s.transitionShellChange(ctx, planID, changeID, expectedVersion, at, func(tx *sql.Tx, taskID, parentID, pathValue string) error {
		var planStatus PlanStatus
		var operationStatus OperationStatus
		var runStatus ShellRunStatus
		if err := tx.QueryRowContext(ctx, `SELECT p.status, o.execution_status, r.status FROM plans p JOIN operations o ON o.plan_id = p.id JOIN shell_runs r ON r.plan_id = p.id WHERE p.id = ?`, planID).
			Scan(&planStatus, &operationStatus, &runStatus); err != nil {
			return err
		}
		if planStatus != PlanExecuting || operationStatus != OperationRunning || runStatus != ShellRunExecuting {
			return ErrConflict
		}
		result, err := tx.ExecContext(ctx, `UPDATE shell_changes SET execution_status = 'running', updated_at_ns = ? WHERE plan_id = ? AND id = ? AND approval_state = 'approved' AND execution_status = 'pending'`, timestampOrNow(at), planID, changeID)
		if err != nil {
			return err
		}
		if err := requireOneRow(result, "Shell change"); err != nil {
			return ErrConflict
		}
		return appendShellAudit(ctx, tx, taskID, planID, parentID, "shell_change_started", "running", pathValue, at)
	})
}

// FinishShellChange 保存一项差异的执行结果和路径审计。
func (s *Store) FinishShellChange(ctx context.Context, planID, changeID string, status OperationStatus, message string, expectedVersion int, at time.Time) error {
	if status != OperationSucceeded && status != OperationFailed && status != OperationPartial || len(message) > maxShellChangeMessage || !utf8.ValidString(message) {
		return errors.New("invalid Shell change result")
	}
	return s.transitionShellChange(ctx, planID, changeID, expectedVersion, at, func(tx *sql.Tx, taskID, parentID, pathValue string) error {
		var planStatus PlanStatus
		var operationStatus OperationStatus
		var runStatus ShellRunStatus
		if err := tx.QueryRowContext(ctx, `SELECT p.status, o.execution_status, r.status FROM plans p JOIN operations o ON o.plan_id = p.id JOIN shell_runs r ON r.plan_id = p.id WHERE p.id = ?`, planID).
			Scan(&planStatus, &operationStatus, &runStatus); err != nil {
			return err
		}
		if planStatus != PlanExecuting || operationStatus != OperationRunning || runStatus != ShellRunExecuting {
			return ErrConflict
		}
		result, err := tx.ExecContext(ctx, `UPDATE shell_changes SET execution_status = ?, result_message = ?, updated_at_ns = ?
			WHERE plan_id = ? AND id = ? AND approval_state = 'approved' AND execution_status = 'running'`, status, message, timestampOrNow(at), planID, changeID)
		if err != nil {
			return err
		}
		if err := requireOneRow(result, "Shell change"); err != nil {
			return ErrConflict
		}
		return appendShellAudit(ctx, tx, taskID, planID, parentID, "shell_change_finished", string(status), pathValue, at)
	})
}

// ActiveShellPayloadIDs 返回仍处于审批或执行流程的候选载荷 ID。
func (s *Store) ActiveShellPayloadIDs(ctx context.Context) (map[string]struct{}, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT c.payload_id FROM shell_changes c JOIN shell_runs r ON r.plan_id = c.plan_id
		WHERE c.payload_id <> '' AND c.execution_status IN ('pending', 'running')
		AND r.status IN ('awaiting_approval', 'approved', 'executing')`)
	if err != nil {
		return nil, fmt.Errorf("list active Shell payloads: %w", err)
	}
	defer rows.Close()
	ids := make(map[string]struct{})
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}

type shellChangeTransaction func(*sql.Tx, string, string, string) error

func (s *Store) transitionShellChange(ctx context.Context, planID, changeID string, expectedVersion int, at time.Time, apply shellChangeTransaction) error {
	if !validShellID(planID) || !validShellID(changeID) || expectedVersion < 1 {
		return ErrConflict
	}
	at = conversationTime(at)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin Shell change transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var taskID, parentID, pathValue string
	var version int
	if err := tx.QueryRowContext(ctx, `SELECT p.task_id, p.version, o.id, c.path FROM shell_changes c JOIN plans p ON p.id = c.plan_id
		JOIN operations o ON o.plan_id = p.id WHERE c.plan_id = ? AND c.id = ? AND o.action = 'run_shell'`, planID, changeID).
		Scan(&taskID, &version, &parentID, &pathValue); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("read Shell change: %w", err)
	}
	if version != expectedVersion {
		return ErrConflict
	}
	if err := apply(tx, taskID, parentID, pathValue); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit Shell change transaction: %w", err)
	}
	return nil
}

const shellChangeSelect = `SELECT id, plan_id, ordinal, path, kind, original_state_json,
	candidate_sha256, candidate_size, candidate_mode, binary, truncated, payload_id,
	approval_state, approved_by, approved_at_ns, execution_status, result_message, created_at_ns, updated_at_ns FROM shell_changes`

type shellRowScanner interface{ Scan(...any) error }

func scanShellChange(row shellRowScanner) (ShellChangeRecord, error) {
	var change ShellChangeRecord
	var sha sql.NullString
	var size, mode, approvedAt sql.NullInt64
	var binaryValue, truncated int
	var createdAt, updatedAt int64
	if err := row.Scan(&change.ID, &change.PlanID, &change.Ordinal, &change.Path, &change.Kind, &change.OriginalStateJSON,
		&sha, &size, &mode, &binaryValue, &truncated, &change.PayloadID, &change.Approval, &change.ApprovedBy,
		&approvedAt, &change.Status, &change.ResultMessage, &createdAt, &updatedAt); err != nil {
		return ShellChangeRecord{}, fmt.Errorf("scan Shell change: %w", err)
	}
	if sha.Valid {
		change.CandidateSHA256 = sha.String
	}
	if size.Valid {
		change.CandidateSize = size.Int64
	}
	if mode.Valid {
		if mode.Int64 < 0 || mode.Int64 > 0o777 {
			return ShellChangeRecord{}, errors.New("stored Shell candidate mode is invalid")
		}
		value := uint32(mode.Int64)
		change.CandidateMode = &value
	}
	change.Binary = binaryValue != 0
	change.Truncated = truncated != 0
	if approvedAt.Valid {
		value := time.Unix(0, approvedAt.Int64).UTC()
		change.ApprovedAt = &value
	}
	change.CreatedAt = time.Unix(0, createdAt).UTC()
	change.UpdatedAt = time.Unix(0, updatedAt).UTC()
	return change, nil
}

func scanShellRun(row shellRowScanner) (ShellRunRecord, error) {
	var run ShellRunRecord
	var createdAt, updatedAt int64
	var outputDiscarded, outputTruncated int
	err := row.Scan(&run.ID, &run.PlanID, &run.OperationID, &run.Command, &run.WorkingDirectory, &run.Stdout, &run.Stderr,
		&outputDiscarded, &outputTruncated, &run.ExitCode, &run.StageID, &run.Status, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ShellRunRecord{}, ErrNotFound
	}
	if err != nil {
		return ShellRunRecord{}, fmt.Errorf("read Shell run: %w", err)
	}
	run.OutputDiscarded = outputDiscarded == 1
	run.OutputTruncated = outputTruncated == 1
	run.CreatedAt = time.Unix(0, createdAt).UTC()
	run.UpdatedAt = time.Unix(0, updatedAt).UTC()
	return run, nil
}

func insertShellRun(ctx context.Context, tx *sql.Tx, run ShellRunRecord) error {
	if !validShellRun(run) || run.Status != ShellRunAwaitingApproval && run.Status != ShellRunCompleted {
		return errors.New("invalid initial Shell run")
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO shell_runs (id, plan_id, operation_id, command, working_directory, stdout, stderr, output_discarded, output_truncated, exit_code, stage_id, status, created_at_ns, updated_at_ns)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, run.ID, run.PlanID, run.OperationID, run.Command, run.WorkingDirectory,
		run.Stdout, run.Stderr, boolInt(run.OutputDiscarded), boolInt(run.OutputTruncated), run.ExitCode, run.StageID, run.Status,
		timestampOrNow(run.CreatedAt), timestampOrNow(run.UpdatedAt))
	if err != nil {
		return fmt.Errorf("insert Shell run: %w", err)
	}
	return nil
}

func insertShellChange(ctx context.Context, tx *sql.Tx, change ShellChangeRecord) error {
	if !validShellChange(change) {
		return fmt.Errorf("invalid Shell change %q", change.ID)
	}
	var sha, size, mode, approvedAt any
	if change.CandidateSHA256 != "" {
		sha, size = change.CandidateSHA256, change.CandidateSize
	}
	if change.CandidateMode != nil {
		mode = int64(*change.CandidateMode)
	}
	if change.ApprovedAt != nil {
		approvedAt = change.ApprovedAt.UTC().UnixNano()
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO shell_changes (
		id, plan_id, ordinal, path, kind, original_state_json, candidate_sha256, candidate_size, candidate_mode,
		binary, truncated, payload_id, approval_state, approved_by, approved_at_ns, execution_status, result_message, created_at_ns, updated_at_ns
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		change.ID, change.PlanID, change.Ordinal, change.Path, change.Kind, change.OriginalStateJSON, sha, size, mode,
		boolInt(change.Binary), boolInt(change.Truncated), change.PayloadID, change.Approval, change.ApprovedBy, approvedAt,
		change.Status, change.ResultMessage, timestampOrNow(change.CreatedAt), timestampOrNow(change.UpdatedAt))
	if err != nil {
		return fmt.Errorf("insert Shell change %q: %w", change.ID, err)
	}
	return nil
}

func validateShellChanges(planID string, changes []ShellChangeRecord) error {
	ids, paths := make(map[string]struct{}, len(changes)), make(map[string]struct{}, len(changes))
	for index, change := range changes {
		if change.PlanID != planID || change.Ordinal != index || !validShellChange(change) {
			return fmt.Errorf("invalid Shell change at ordinal %d", index)
		}
		if _, exists := ids[change.ID]; exists {
			return errors.New("Shell change identifiers must be unique")
		}
		if _, exists := paths[change.Path]; exists {
			return errors.New("Shell change paths must be unique")
		}
		ids[change.ID], paths[change.Path] = struct{}{}, struct{}{}
	}
	return nil
}

func validShellChange(change ShellChangeRecord) bool {
	if !validShellID(change.ID) || change.Ordinal < 0 || change.Ordinal >= MaxShellChanges || !validShellRelativePath(change.Path) ||
		!validShellOriginalState(change.OriginalStateJSON, change.Kind) || change.Approval != ApprovalPending || change.Status != OperationPending ||
		change.ApprovedBy != "" || change.ApprovedAt != nil || len(change.ResultMessage) > maxShellChangeMessage || !utf8.ValidString(change.ResultMessage) {
		return false
	}
	if change.Kind == "deleted" {
		return change.CandidateSHA256 == "" && change.CandidateSize == 0 && change.CandidateMode == nil && !change.Binary && !change.Truncated && change.PayloadID == ""
	}
	if change.CandidateMode == nil || *change.CandidateMode > 0o777 || change.CandidateSize < 0 {
		return false
	}
	if change.CandidateSHA256 == "" {
		return change.CandidateSize == 0 && change.PayloadID == "" && !change.Binary && !change.Truncated
	}
	return isSHA256(change.CandidateSHA256) && validShellID(change.PayloadID)
}

func validShellOriginalState(value, changeKind string) bool {
	if len(value) > 4096 {
		return false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(value), &fields) != nil || fields == nil || len(fields) != 5 {
		return false
	}
	for _, field := range []string{"fingerprint", "sha256", "mode", "isDirectory", "size"} {
		if _, exists := fields[field]; !exists {
			return false
		}
	}
	var fingerprintFields map[string]json.RawMessage
	if json.Unmarshal(fields["fingerprint"], &fingerprintFields) != nil || fingerprintFields == nil {
		return false
	}
	var exists bool
	if raw, ok := fingerprintFields["exists"]; !ok || json.Unmarshal(raw, &exists) != nil {
		return false
	}
	for field := range fingerprintFields {
		switch field {
		case "exists", "kind", "size", "modifiedAt", "mode", "device", "inode", "links", "identityAvailable", "ownerUid", "groupGid", "ownershipAvailable", "changeTime", "changeTimeAvailable":
		default:
			return false
		}
	}
	var state struct {
		Fingerprint struct {
			Exists *bool `json:"exists"`
		} `json:"fingerprint"`
		SHA256      string `json:"sha256"`
		Mode        uint32 `json:"mode"`
		IsDirectory bool   `json:"isDirectory"`
		Size        int64  `json:"size"`
	}
	if json.Unmarshal([]byte(value), &state) != nil || state.Fingerprint.Exists == nil || state.Size < 0 || state.Mode > 0o777 ||
		state.SHA256 != "" && !isSHA256(state.SHA256) || *state.Fingerprint.Exists && state.IsDirectory == (state.SHA256 != "") ||
		!*state.Fingerprint.Exists && (state.SHA256 != "" || state.Size != 0 || state.Mode != 0 || state.IsDirectory) {
		return false
	}
	if changeKind == "added" {
		return !*state.Fingerprint.Exists
	}
	return (changeKind == "modified" || changeKind == "deleted") && *state.Fingerprint.Exists
}

func validShellRun(run ShellRunRecord) bool {
	return validShellID(run.ID) && validShellID(run.PlanID) && validShellID(run.OperationID) && validShellID(run.StageID) &&
		strings.TrimSpace(run.Command) != "" && len(run.Command) <= maxShellCommandBytes && utf8.ValidString(run.Command) &&
		(run.WorkingDirectory == "." || validShellRelativePath(run.WorkingDirectory)) &&
		len(run.Stdout) <= MaxShellOutputBytes && len(run.Stderr) <= MaxShellOutputBytes &&
		utf8.ValidString(run.Stdout) && utf8.ValidString(run.Stderr) &&
		(!run.OutputDiscarded || run.Stdout == "" && run.Stderr == "" && !run.OutputTruncated) &&
		run.ExitCode >= 0 && run.ExitCode <= 255 && validShellRunStatus(run.Status)
}

func validShellRunStatus(status ShellRunStatus) bool {
	switch status {
	case ShellRunAwaitingApproval, ShellRunApproved, ShellRunExecuting, ShellRunCompleted, ShellRunFailed, ShellRunRejected, ShellRunCancelled, ShellRunInvalidated, ShellRunPartial:
		return true
	default:
		return false
	}
}

func validShellID(value string) bool {
	if value == "" || len(value) > 256 || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func validShellReply(value string) bool {
	return value == "" || len(value) <= maxShellChangeMessage && utf8.ValidString(value)
}

func appendShellAudit(ctx context.Context, tx *sql.Tx, taskID, planID, parentID, event, outcome, pathValue string, at time.Time) error {
	actor := "agent"
	switch event {
	case "shell_change_approved", "shell_change_rejected", "shell_change_cancelled", "shell_plan_approved", "shell_plan_rejected":
		actor = "user"
	}
	_, err := appendAuditRecord(ctx, tx, AuditRecord{
		TaskID: taskID, PlanID: planID, OperationID: parentID, EventType: event, Actor: actor, Outcome: outcome,
		SourcePath: pathValue, CreatedAt: at,
	})
	return err
}
