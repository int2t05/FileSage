// 本文件提供 Agent 方案的事务化状态转换。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// MaxAgentPlanOperations 限定 Agent 一版方案中的最大操作数。
const MaxAgentPlanOperations = 256

// CreateAgentTaskPlan 原子创建建议、方案和审计记录。
func (s *Store) CreateAgentTaskPlan(ctx context.Context, task TaskRecord, plan PlanRecord, audit AuditRecord) error {
	if task.ID == "" || plan.TaskID != task.ID || task.ScopePath != plan.ScopePath || audit.TaskID != task.ID || audit.PlanID != plan.ID {
		return errors.New("agent task, plan, and audit identifiers do not match")
	}
	if task.Kind != "agent" || task.Status != TaskAwaitingApproval || task.ModelProvider != "" || task.ModelName != "" {
		return errors.New("agent task must be caller-proposed and await approval")
	}
	if plan.Status != PlanAwaitingApproval || !validInitialAgentOperations(plan.Operations) {
		return errors.New("agent plan must contain supported pending operations")
	}
	if audit.OperationID != plan.Operations[0].ID || audit.EventType != "plan_created" {
		return errors.New("agent plan audit must reference its first operation")
	}
	audit.Actor = "user"
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin agent plan transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := insertTaskRecord(ctx, tx, task); err != nil {
		return err
	}
	if err := createPlanRecord(ctx, tx, plan); err != nil {
		return err
	}
	if _, err := appendAuditRecord(ctx, tx, audit); err != nil {
		return err
	}
	for _, operation := range plan.Operations[1:] {
		if _, err := appendAuditRecord(ctx, tx, operationPlanCreatedAudit(audit, operation)); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit agent plan transaction: %w", err)
	}
	return nil
}

func validInitialAgentOperations(operations []OperationRecord) bool {
	if len(operations) < 1 || len(operations) > MaxAgentPlanOperations {
		return false
	}
	hasPhotoArchive := false
	allPhotoArchive := true
	hasSingleArchive := false
	for index, operation := range operations {
		if operation.Action == "archive_photo" {
			hasPhotoArchive = true
		} else {
			allPhotoArchive = false
		}
		if operation.Action == "create_archive" || operation.Action == "extract_archive" {
			hasSingleArchive = true
		}
		if operation.Ordinal != index || !supportedAgentAction(operation.Action) ||
			operation.Approval != ApprovalPending || operation.Status != OperationPending ||
			operation.TargetConflictPolicy != "" && operation.TargetConflictPolicy != "fail" || operation.RequiresExplicitConfirmation {
			return false
		}
	}
	if hasPhotoArchive {
		return allPhotoArchive
	}
	if hasSingleArchive {
		return len(operations) == 1
	}
	return true
}

func operationPlanCreatedAudit(base AuditRecord, operation OperationRecord) AuditRecord {
	base.OperationID = operation.ID
	base.SourcePath = operation.SourcePath
	base.TargetPath = operation.TargetPath
	return base
}

func supportedAgentAction(action string) bool {
	return action == "rename" || action == "edit_text" || action == "create_file" || action == "overwrite_file" || action == "create_directory" || action == "move_file" || action == "create_symlink" || action == "copy_file" || action == "copy_directory" || action == "create_archive" || action == "extract_archive" || action == "set_permissions" || action == "set_owner" || action == "trash_duplicate" || action == "archive_photo" || action == "trash" || action == "permanent_delete"
}

func insertTaskRecord(ctx context.Context, execer storeExecer, task TaskRecord) error {
	if task.ID == "" || task.Kind == "" || !validTaskStatus(task.Status) {
		return errors.New("invalid agent task record")
	}
	_, err := execer.ExecContext(ctx, `
		INSERT INTO tasks (id, kind, status, request_text, scope_path, model_provider,
			model_name, created_at_ns, updated_at_ns)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		task.ID, task.Kind, task.Status, task.RequestText, task.ScopePath,
		task.ModelProvider, task.ModelName, timestampOrNow(task.CreatedAt), timestampOrNow(task.UpdatedAt))
	if err != nil {
		return fmt.Errorf("insert agent task: %w", err)
	}
	return nil
}

// LatestPlanForTask 返回任务的最新方案。
func (s *Store) LatestPlanForTask(ctx context.Context, taskID string) (PlanRecord, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM plans WHERE task_id = ? ORDER BY version DESC LIMIT 1`, taskID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return PlanRecord{}, ErrNotFound
	}
	if err != nil {
		return PlanRecord{}, fmt.Errorf("read latest agent plan: %w", err)
	}
	return s.Plan(ctx, id)
}

// ReviewAgentPlan 记录人工计划审核，并按审核时固定的模式准备执行。
func (s *Store) ReviewAgentPlan(ctx context.Context, planID string, expectedVersion int, expectedMode string, at time.Time, confirmPermanentDelete bool) (string, error) {
	if planID == "" || expectedVersion < 1 || expectedMode != "bypass" && expectedMode != "manual" {
		return "", ErrConflict
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	at = at.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin Agent plan review: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var taskID string
	var version int
	var status PlanStatus
	if err := tx.QueryRowContext(ctx, `SELECT task_id, version, status FROM plans WHERE id = ?`, planID).Scan(&taskID, &version, &status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("read Agent plan for review: %w", err)
	}
	if version != expectedVersion || status != PlanAwaitingApproval {
		return "", ErrConflict
	}
	var bypass int
	if err := tx.QueryRowContext(ctx, `SELECT bypass_permission FROM agent_settings WHERE id = 1`).Scan(&bypass); err != nil {
		return "", fmt.Errorf("read Agent permission setting for review: %w", err)
	}
	mode := "manual"
	if bypass == 1 {
		mode = "bypass"
	}
	if mode != expectedMode {
		return "", ErrConflict
	}
	var total, pending, permanentDeletes int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN approval_state = 'pending' AND execution_status = 'pending' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN action = 'permanent_delete' THEN 1 ELSE 0 END), 0)
		FROM operations WHERE plan_id = ? AND action <> 'run_shell'`, planID).Scan(&total, &pending, &permanentDeletes); err != nil {
		return "", err
	}
	if total < 1 || total != pending || (permanentDeletes > 0) != confirmPermanentDelete {
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
	if mode == "bypass" {
		rows, err := tx.QueryContext(ctx, `SELECT id, action, source_path, target_path FROM operations WHERE plan_id = ? AND execution_status = 'pending' ORDER BY ordinal`, planID)
		if err != nil {
			return "", err
		}
		type operationAudit struct{ id, action, source, target string }
		operations := make([]operationAudit, 0, total)
		for rows.Next() {
			var operation operationAudit
			if err := rows.Scan(&operation.id, &operation.action, &operation.source, &operation.target); err != nil {
				_ = rows.Close()
				return "", err
			}
			operations = append(operations, operation)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return "", err
		}
		if err := rows.Close(); err != nil {
			return "", err
		}
		result, err := tx.ExecContext(ctx, `UPDATE operations SET approval_state = 'approved', approved_by = 'user', approved_at_ns = ?, updated_at_ns = ?
			WHERE plan_id = ? AND approval_state = 'pending' AND execution_status = 'pending'`, at.UnixNano(), at.UnixNano(), planID)
		if err != nil {
			return "", fmt.Errorf("approve reviewed Agent steps: %w", err)
		}
		if count, countErr := result.RowsAffected(); countErr != nil || count != int64(total) {
			return "", ErrConflict
		}
		for _, operation := range operations {
			message := "用户审核整份方案并允许自动执行"
			if operation.action == "permanent_delete" {
				message = "用户确认不可恢复删除并允许自动执行"
			}
			if _, err := appendAuditRecord(ctx, tx, AuditRecord{
				TaskID: taskID, PlanID: planID, OperationID: operation.id,
				EventType: "operation_approved", Actor: "user", Outcome: "approved",
				Message: message, SourcePath: operation.source, TargetPath: operation.target, CreatedAt: at,
			}); err != nil {
				return "", err
			}
		}
	}
	outcome, message := "manual", "用户审核计划；后续每一步需单独确认"
	if mode == "bypass" {
		outcome, message = "bypass", "用户审核计划；后续步骤按 bypass 模式顺序运行"
	}
	if confirmPermanentDelete {
		message += "；已确认计划中的永久删除不可恢复"
	}
	if _, err := appendAuditRecord(ctx, tx, AuditRecord{
		TaskID: taskID, PlanID: planID, EventType: "plan_reviewed", Actor: "user", Outcome: outcome,
		Message: message, CreatedAt: at,
	}); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit Agent plan review: %w", err)
	}
	return mode, nil
}

// ReviseAgentPlan 保留旧方案并原子创建一份全新待审批版本。
func (s *Store) ReviseAgentPlan(ctx context.Context, previousID string, expectedVersion int, plan PlanRecord, at time.Time) error {
	if expectedVersion < 1 || previousID == "" || plan.ID == "" || plan.ID == previousID ||
		plan.Version != expectedVersion+1 || plan.Status != PlanAwaitingApproval ||
		!validInitialAgentOperations(plan.Operations) {
		return ErrConflict
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	at = at.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin agent plan revision: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var taskID, scopePath, provider, model string
	var version int
	var status PlanStatus
	if err := tx.QueryRowContext(ctx, `SELECT task_id, version, status, scope_path, model_provider, model_name FROM plans WHERE id = ?`, previousID).
		Scan(&taskID, &version, &status, &scopePath, &provider, &model); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("read plan before revision: %w", err)
	}
	if version != expectedVersion || status != PlanAwaitingApproval || plan.TaskID != taskID ||
		plan.ScopePath != scopePath || plan.ModelProvider != provider || plan.ModelName != model {
		return ErrConflict
	}
	var taskStatus TaskStatus
	if err := tx.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id = ?`, taskID).Scan(&taskStatus); err != nil {
		return fmt.Errorf("read task before revision: %w", err)
	}
	if taskStatus != TaskAwaitingApproval {
		return ErrConflict
	}

	// 先 CAS 旧方案以取得写事务，再检查逐项审批状态，避免与审批并发通过。
	result, err := tx.ExecContext(ctx, `UPDATE plans SET status = 'invalidated', updated_at_ns = ? WHERE id = ? AND version = ? AND status = 'awaiting_approval'`,
		at.UnixNano(), previousID, expectedVersion)
	if err != nil {
		return fmt.Errorf("claim previous plan for revision: %w", err)
	}
	if err := requireOneRow(result, "previous agent plan"); err != nil {
		return ErrConflict
	}
	var total, pending int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(CASE WHEN approval_state = 'pending' AND execution_status = 'pending' THEN 1 ELSE 0 END), 0) FROM operations WHERE plan_id = ?`, previousID).
		Scan(&total, &pending); err != nil {
		return fmt.Errorf("check operations before revision: %w", err)
	}
	if total == 0 || pending != total {
		return ErrConflict
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, source_path, target_path FROM operations WHERE plan_id = ? ORDER BY ordinal`, previousID)
	if err != nil {
		return fmt.Errorf("read operations before revision: %w", err)
	}
	type previousOperation struct{ id, source, target string }
	previousOperations := make([]previousOperation, 0, total)
	for rows.Next() {
		var operation previousOperation
		if err := rows.Scan(&operation.id, &operation.source, &operation.target); err != nil {
			_ = rows.Close()
			return err
		}
		previousOperations = append(previousOperations, operation)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	result, err = tx.ExecContext(ctx, `UPDATE operations SET approval_state = 'invalidated', execution_status = 'cancelled', result_message = ?, updated_at_ns = ? WHERE plan_id = ? AND approval_state = 'pending' AND execution_status = 'pending'`,
		"操作清单已修改，旧项审批已失效", at.UnixNano(), previousID)
	if err != nil {
		return fmt.Errorf("invalidate previous plan operations: %w", err)
	}
	if count, countErr := result.RowsAffected(); countErr != nil || count != int64(total) {
		return ErrConflict
	}
	if err := createPlanRecord(ctx, tx, plan); err != nil {
		return err
	}
	if err := execAgentOne(ctx, tx, `UPDATE tasks SET updated_at_ns = ? WHERE id = ? AND status = 'awaiting_approval'`, at.UnixNano(), taskID); err != nil {
		return fmt.Errorf("update task after plan revision: %w", err)
	}
	if plan.ModelProvider != "" || plan.ModelName != "" {
		result, err := tx.ExecContext(ctx, `UPDATE agent_conversations SET plan_id = ?, updated_at_ns = ? WHERE plan_id = ? AND task_id = ? AND state = 'awaiting_approval'`,
			plan.ID, at.UnixNano(), previousID, taskID)
		if err != nil {
			return fmt.Errorf("link revised plan to conversation: %w", err)
		}
		if err := requireOneRow(result, "Agent conversation"); err != nil {
			return ErrConflict
		}
	}
	if _, err := appendAuditRecord(ctx, tx, AuditRecord{
		TaskID: taskID, PlanID: previousID, EventType: "plan_edited", Actor: "user", Outcome: "revised",
		Message: "用户修改操作清单；旧审批已失效，已创建新的待审核版本", CreatedAt: at,
	}); err != nil {
		return err
	}
	for _, operation := range previousOperations {
		if _, err := appendAuditRecord(ctx, tx, AuditRecord{
			TaskID: taskID, PlanID: previousID, OperationID: operation.id,
			EventType: "operation_superseded", Actor: "user", Outcome: "invalidated",
			Message: "用户修改操作清单，此项审批已失效", SourcePath: operation.source, TargetPath: operation.target, CreatedAt: at,
		}); err != nil {
			return err
		}
	}
	for _, operation := range plan.Operations {
		if _, err := appendAuditRecord(ctx, tx, operationPlanCreatedAudit(AuditRecord{
			TaskID: taskID, PlanID: plan.ID, EventType: "plan_created", Actor: "user", Outcome: "awaiting_approval",
			Message: "用户编辑后的操作清单已保存，尚未执行", CreatedAt: at,
		}, operation)); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit agent plan revision: %w", err)
	}
	return nil
}

// SetAgentApproval 原子记录单项审批及审计，审批人固定为本地用户。
func (s *Store) SetAgentApproval(ctx context.Context, planID, operationID string, decision ApprovalState, at time.Time, expectedVersion ...int) error {
	if decision != ApprovalApproved && decision != ApprovalRejected {
		return errors.New("agent decision must approve or reject")
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin agent approval transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var taskID string
	var version int
	var planStatus PlanStatus
	var operationStatus OperationStatus
	var approval ApprovalState
	if err := tx.QueryRowContext(ctx, `
		SELECT p.task_id, p.version, p.status, o.execution_status, o.approval_state
		FROM plans p JOIN operations o ON o.plan_id = p.id
		WHERE p.id = ? AND o.id = ?`, planID, operationID).Scan(&taskID, &version, &planStatus, &operationStatus, &approval); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("read agent operation for approval: %w", err)
	}
	if len(expectedVersion) > 0 && (expectedVersion[0] < 1 || version != expectedVersion[0]) {
		return ErrConflict
	}
	if planStatus != PlanAwaitingApproval || operationStatus != OperationPending || approval != ApprovalPending {
		return ErrConflict
	}
	var approvedAt any
	if decision == ApprovalApproved {
		approvedAt = at.UTC().UnixNano()
	}
	approvedBy := ""
	if decision == ApprovalApproved {
		approvedBy = "user"
	}
	nextOperationStatus := OperationPending
	if decision == ApprovalRejected {
		nextOperationStatus = OperationSkipped
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE operations SET approval_state = ?, approved_by = ?, approved_at_ns = ?, execution_status = ?, updated_at_ns = ?
		WHERE id = ? AND plan_id = ? AND approval_state = 'pending' AND execution_status = 'pending'`,
		decision, approvedBy, approvedAt, nextOperationStatus,
		at.UTC().UnixNano(), operationID, planID)
	if err != nil {
		return fmt.Errorf("update agent approval: %w", err)
	}
	if err := requireOneRow(result, "agent operation"); err != nil {
		return ErrConflict
	}
	event, outcome := "operation_approved", "approved"
	if decision == ApprovalRejected {
		event, outcome = "operation_rejected", "rejected"
		if err := cancelAgentOperations(ctx, tx, planID, operationID, "方案中的其他操作因本项被拒绝而取消", at); err != nil {
			return err
		}
		if err := execAgentOne(ctx, tx, `UPDATE plans SET status = ?, updated_at_ns = ? WHERE id = ? AND status = ?`, PlanRejected, at.UTC().UnixNano(), planID, PlanAwaitingApproval); err != nil {
			return fmt.Errorf("reject agent plan: %w", err)
		}
		if err := execAgentOne(ctx, tx, `UPDATE tasks SET status = ?, updated_at_ns = ? WHERE id = ?`, TaskCancelled, at.UTC().UnixNano(), taskID); err != nil {
			return fmt.Errorf("cancel rejected agent task: %w", err)
		}
	} else {
		var pending int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM operations WHERE plan_id = ? AND approval_state = 'pending'`, planID).Scan(&pending); err != nil {
			return err
		}
		if pending == 0 {
			if err := execAgentOne(ctx, tx, `UPDATE plans SET status = ?, updated_at_ns = ? WHERE id = ? AND status = ?`, PlanApproved, at.UTC().UnixNano(), planID, PlanAwaitingApproval); err != nil {
				return fmt.Errorf("approve agent plan: %w", err)
			}
			if err := execAgentOne(ctx, tx, `UPDATE tasks SET status = ?, updated_at_ns = ? WHERE id = ?`, TaskPaused, at.UTC().UnixNano(), taskID); err != nil {
				return fmt.Errorf("pause approved agent task: %w", err)
			}
		}
	}
	if _, err := appendAuditRecord(ctx, tx, AuditRecord{
		TaskID: taskID, PlanID: planID, OperationID: operationID, EventType: event,
		Actor: "user", Outcome: outcome, CreatedAt: at,
	}); err != nil {
		return err
	}
	if decision == ApprovalRejected {
		if _, err := appendAuditRecord(ctx, tx, AuditRecord{
			TaskID: taskID, PlanID: planID, EventType: "plan_rejected", Actor: "user",
			Outcome: "rejected", Message: "至少一项操作被拒绝，整版方案已终止", CreatedAt: at,
		}); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit agent approval: %w", err)
	}
	return nil
}

func cancelAgentOperations(ctx context.Context, tx *sql.Tx, planID, exceptID, message string, at time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, source_path, target_path FROM operations
		WHERE plan_id = ? AND id <> ? AND execution_status = 'pending' AND approval_state IN ('pending', 'approved') ORDER BY ordinal`, planID, exceptID)
	if err != nil {
		return err
	}
	type cancelledOperation struct{ id, source, target string }
	var operations []cancelledOperation
	for rows.Next() {
		var operation cancelledOperation
		if err := rows.Scan(&operation.id, &operation.source, &operation.target); err != nil {
			_ = rows.Close()
			return err
		}
		operations = append(operations, operation)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, operation := range operations {
		result, err := tx.ExecContext(ctx, `UPDATE operations SET approval_state = 'invalidated', approved_by = '', approved_at_ns = NULL,
			execution_status = 'cancelled', result_message = ?, updated_at_ns = ?
			WHERE id = ? AND plan_id = ? AND execution_status = 'pending' AND approval_state IN ('pending', 'approved')`,
			message, at.UTC().UnixNano(), operation.id, planID)
		if err != nil {
			return err
		}
		if err := requireOneRow(result, "cancelled agent operation"); err != nil {
			return ErrConflict
		}
		var taskID string
		if err := tx.QueryRowContext(ctx, `SELECT task_id FROM plans WHERE id = ?`, planID).Scan(&taskID); err != nil {
			return err
		}
		if _, err := appendAuditRecord(ctx, tx, AuditRecord{
			TaskID: taskID, PlanID: planID, OperationID: operation.id,
			EventType: "operation_cancelled", Actor: "system", Outcome: "cancelled",
			Message: message, SourcePath: operation.source, TargetPath: operation.target, CreatedAt: at,
		}); err != nil {
			return err
		}
	}
	return nil
}

// StartAgentOperation 原子占用已批准操作并写入审计。
func (s *Store) StartAgentOperation(ctx context.Context, planID, operationID string, at time.Time) error {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	return s.transitionAgentOperation(ctx, planID, operationID, at, func(tx *sql.Tx, taskID string) error {
		var planStatus PlanStatus
		var stopRequested int
		var ordinal int
		if err := tx.QueryRowContext(ctx, `SELECT p.status, p.stop_requested, o.ordinal FROM plans p JOIN operations o ON o.plan_id = p.id WHERE p.id = ? AND o.id = ?`, planID, operationID).Scan(&planStatus, &stopRequested, &ordinal); err != nil {
			return err
		}
		if (planStatus != PlanApproved && planStatus != PlanExecuting) || stopRequested != 0 {
			return ErrConflict
		}
		var unfinishedBefore int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM operations WHERE plan_id = ? AND ordinal < ? AND execution_status <> 'succeeded'`, planID, ordinal).Scan(&unfinishedBefore); err != nil {
			return err
		}
		if unfinishedBefore != 0 {
			return ErrConflict
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE operations SET execution_status = 'running', updated_at_ns = ?
			WHERE id = ? AND plan_id = ? AND approval_state = 'approved' AND execution_status = 'pending'`,
			at.UTC().UnixNano(), operationID, planID)
		if err != nil {
			return err
		}
		if err := requireOneRow(result, "agent operation"); err != nil {
			return ErrConflict
		}
		if planStatus == PlanApproved {
			if err := execAgentOne(ctx, tx, `UPDATE plans SET status = 'executing', updated_at_ns = ? WHERE id = ? AND status = 'approved'`, at.UTC().UnixNano(), planID); err != nil {
				return err
			}
		}
		if err := execAgentOne(ctx, tx, `UPDATE tasks SET status = 'running', updated_at_ns = ? WHERE id = ? AND status IN ('paused', 'running')`, at.UTC().UnixNano(), taskID); err != nil {
			return err
		}
		return appendAgentAudit(ctx, tx, taskID, planID, operationID, "operation_started", "started", "", at)
	})
}

// StartReviewedAgentOperation 原子批准并占用已审核方案的下一步。
func (s *Store) StartReviewedAgentOperation(ctx context.Context, planID, operationID string, expectedVersion int, at time.Time) error {
	if planID == "" || operationID == "" || expectedVersion < 1 {
		return ErrConflict
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	at = at.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin reviewed Agent step: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var taskID string
	var version int
	var status PlanStatus
	var mode string
	var reviewedAt sql.NullInt64
	var stopRequested int
	if err := tx.QueryRowContext(ctx, `SELECT task_id, version, status, execution_mode, reviewed_at_ns, stop_requested
		FROM plans WHERE id = ?`, planID).
		Scan(&taskID, &version, &status, &mode, &reviewedAt, &stopRequested); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if version != expectedVersion || status != PlanApproved || mode != "manual" || !reviewedAt.Valid || stopRequested != 0 {
		return ErrConflict
	}
	var ordinal int
	var approval ApprovalState
	var operationStatus OperationStatus
	if err := tx.QueryRowContext(ctx, `SELECT ordinal, approval_state, execution_status FROM operations WHERE plan_id = ? AND id = ?`, planID, operationID).
		Scan(&ordinal, &approval, &operationStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if operationStatus != OperationPending || approval != ApprovalPending && approval != ApprovalApproved {
		return ErrConflict
	}
	var unfinishedBefore int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM operations WHERE plan_id = ? AND ordinal < ? AND execution_status <> 'succeeded'`, planID, ordinal).Scan(&unfinishedBefore); err != nil {
		return err
	}
	var running int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM operations WHERE plan_id = ? AND execution_status = 'running'`, planID).Scan(&running); err != nil {
		return err
	}
	if unfinishedBefore != 0 || running != 0 {
		return ErrConflict
	}
	if approval == ApprovalPending {
		if err := execAgentOne(ctx, tx, `UPDATE operations SET approval_state = 'approved', approved_by = 'user', approved_at_ns = ?, updated_at_ns = ?
			WHERE id = ? AND plan_id = ? AND approval_state = 'pending' AND execution_status = 'pending'`, at.UnixNano(), at.UnixNano(), operationID, planID); err != nil {
			return ErrConflict
		}
		var source, target string
		if err := tx.QueryRowContext(ctx, `SELECT source_path, target_path FROM operations WHERE id = ? AND plan_id = ?`, operationID, planID).Scan(&source, &target); err != nil {
			return err
		}
		if _, err := appendAuditRecord(ctx, tx, AuditRecord{
			TaskID: taskID, PlanID: planID, OperationID: operationID,
			EventType: "operation_approved", Actor: "user", Outcome: "approved",
			Message: "用户确认执行当前步骤", SourcePath: source, TargetPath: target, CreatedAt: at,
		}); err != nil {
			return err
		}
	}
	if err := execAgentOne(ctx, tx, `UPDATE operations SET execution_status = 'running', updated_at_ns = ?
		WHERE id = ? AND plan_id = ? AND approval_state = 'approved' AND execution_status = 'pending'`, at.UnixNano(), operationID, planID); err != nil {
		return ErrConflict
	}
	if err := execAgentOne(ctx, tx, `UPDATE plans SET status = 'executing', updated_at_ns = ? WHERE id = ? AND version = ? AND status = 'approved'`, at.UnixNano(), planID, expectedVersion); err != nil {
		return ErrConflict
	}
	if err := execAgentOne(ctx, tx, `UPDATE tasks SET status = 'running', updated_at_ns = ? WHERE id = ? AND status = 'paused'`, at.UnixNano(), taskID); err != nil {
		return ErrConflict
	}
	if err := appendAgentAudit(ctx, tx, taskID, planID, operationID, "operation_started", "started", "", at); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit reviewed Agent step: %w", err)
	}
	return nil
}

// ClaimAgentPlanExecution 记录用户对整份已审批方案的最终执行确认。
func (s *Store) ClaimAgentPlanExecution(ctx context.Context, planID string, expectedVersion int, at time.Time) error {
	if expectedVersion < 1 {
		return ErrConflict
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin agent plan execution claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var taskID string
	var version int
	var status PlanStatus
	if err := tx.QueryRowContext(ctx, `SELECT task_id, version, status FROM plans WHERE id = ?`, planID).Scan(&taskID, &version, &status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if version != expectedVersion || status != PlanApproved {
		return ErrConflict
	}
	var operations, approved, pending int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(CASE WHEN approval_state = 'approved' THEN 1 ELSE 0 END), 0), COALESCE(SUM(CASE WHEN execution_status = 'pending' THEN 1 ELSE 0 END), 0) FROM operations WHERE plan_id = ?`, planID).Scan(&operations, &approved, &pending); err != nil {
		return err
	}
	if operations < 1 || approved != operations || pending < 1 {
		return ErrConflict
	}
	if err := execAgentOne(ctx, tx, `UPDATE plans SET status = 'executing', updated_at_ns = ? WHERE id = ? AND version = ? AND status = 'approved'`, at.UTC().UnixNano(), planID, expectedVersion); err != nil {
		return err
	}
	if err := execAgentOne(ctx, tx, `UPDATE tasks SET status = 'running', updated_at_ns = ? WHERE id = ? AND status = 'paused'`, at.UTC().UnixNano(), taskID); err != nil {
		return err
	}
	if _, err := appendAuditRecord(ctx, tx, AuditRecord{
		TaskID: taskID, PlanID: planID, EventType: "plan_execution_confirmed", Actor: "user",
		Outcome: "started", Message: "用户确认执行已审批的方案", CreatedAt: at,
	}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit agent plan execution claim: %w", err)
	}
	return nil
}

// RequestAgentPlanStop 停止当前操作结束后的所有后续操作。
func (s *Store) RequestAgentPlanStop(ctx context.Context, planID string, expectedVersion int, at time.Time) error {
	if expectedVersion < 1 {
		return ErrConflict
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin agent stop request: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var taskID string
	var version int
	var status PlanStatus
	var stopRequested int
	if err := tx.QueryRowContext(ctx, `SELECT task_id, version, status, stop_requested FROM plans WHERE id = ?`, planID).Scan(&taskID, &version, &status, &stopRequested); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if version != expectedVersion || status != PlanExecuting {
		return ErrConflict
	}
	if stopRequested != 0 {
		return nil
	}
	var running, pending int
	if err := tx.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(CASE WHEN execution_status = 'running' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN execution_status = 'pending' THEN 1 ELSE 0 END), 0)
		FROM operations WHERE plan_id = ?`, planID).Scan(&running, &pending); err != nil {
		return err
	}
	if running > 1 || running == 0 && pending == 0 {
		return ErrConflict
	}
	if err := execAgentOne(ctx, tx, `UPDATE plans SET stop_requested = 1, updated_at_ns = ? WHERE id = ? AND status = 'executing' AND version = ? AND stop_requested = 0`, at.UTC().UnixNano(), planID, expectedVersion); err != nil {
		return err
	}
	if _, err := appendAuditRecord(ctx, tx, AuditRecord{
		TaskID: taskID, PlanID: planID, EventType: "plan_stop_requested", Actor: "user",
		Outcome: "requested", Message: "用户请求当前操作结束后停止后续操作", CreatedAt: at,
	}); err != nil {
		return err
	}
	if running == 0 {
		if err := finishAgentPlanStop(ctx, tx, taskID, planID, at); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit agent stop request: %w", err)
	}
	return nil
}

// FinishAgentOperation 保存执行结果与对应审计。
func (s *Store) FinishAgentOperation(ctx context.Context, planID, operationID string, status OperationStatus, message string, at time.Time) error {
	if status != OperationSucceeded && status != OperationFailed && status != OperationPartial {
		return errors.New("invalid final agent operation status")
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	return s.transitionAgentOperation(ctx, planID, operationID, at, func(tx *sql.Tx, taskID string) error {
		result, err := tx.ExecContext(ctx, `
			UPDATE operations SET execution_status = ?, result_message = ?, updated_at_ns = ?
			WHERE id = ? AND plan_id = ? AND execution_status = 'running'`,
			status, message, at.UTC().UnixNano(), operationID, planID)
		if err != nil {
			return err
		}
		if err := requireOneRow(result, "agent operation"); err != nil {
			return ErrConflict
		}
		if err := appendAgentAudit(ctx, tx, taskID, planID, operationID, "operation_finished", string(status), message, at); err != nil {
			return err
		}
		var unfinished int
		var executionMode string
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), (SELECT execution_mode FROM plans WHERE id = ?)
			FROM operations WHERE plan_id = ? AND execution_status IN ('pending', 'running')`, planID, planID).Scan(&unfinished, &executionMode); err != nil {
			return err
		}
		if status == OperationFailed || status == OperationPartial {
			if err := cancelAgentOperations(ctx, tx, planID, operationID, "前序操作失败，后续操作已取消", at); err != nil {
				return err
			}
			if err := finalizeAgentPlan(ctx, tx, taskID, planID, at); err != nil {
				return err
			}
		} else if unfinished > 0 {
			var stopRequested int
			if err := tx.QueryRowContext(ctx, `SELECT stop_requested FROM plans WHERE id = ? AND status = 'executing'`, planID).Scan(&stopRequested); err != nil {
				return err
			}
			if stopRequested != 0 {
				if err := finishAgentPlanStop(ctx, tx, taskID, planID, at); err != nil {
					return err
				}
			} else if executionMode == "manual" {
				if err := execAgentOne(ctx, tx, `UPDATE plans SET status = 'approved', updated_at_ns = ? WHERE id = ? AND status = 'executing'`, at.UTC().UnixNano(), planID); err != nil {
					return err
				}
				if err := execAgentOne(ctx, tx, `UPDATE tasks SET status = 'paused', updated_at_ns = ? WHERE id = ? AND status = 'running'`, at.UTC().UnixNano(), taskID); err != nil {
					return err
				}
				if err := appendAgentAudit(ctx, tx, taskID, planID, operationID, "plan_step_paused", "awaiting_confirmation", "当前步骤完成，等待用户确认下一步", at); err != nil {
					return err
				}
			} else if err := execAgentOne(ctx, tx, `UPDATE tasks SET status = 'running', updated_at_ns = ? WHERE id = ? AND status = 'running'`, at.UTC().UnixNano(), taskID); err != nil {
				return err
			}
		} else if unfinished == 0 {
			if err := finalizeAgentPlan(ctx, tx, taskID, planID, at); err != nil {
				return err
			}
		}
		return nil
	})
}

// finishAgentPlanStop 取消未开始项并以停止状态原子结束方案。
func finishAgentPlanStop(ctx context.Context, tx *sql.Tx, taskID, planID string, at time.Time) error {
	var running int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM operations WHERE plan_id = ? AND execution_status = 'running'`, planID).Scan(&running); err != nil {
		return err
	}
	if running != 0 {
		return ErrConflict
	}
	if err := cancelAgentOperations(ctx, tx, planID, "", "用户请求停止，后续操作未执行", at); err != nil {
		return err
	}
	if err := execAgentOne(ctx, tx, `UPDATE plans SET status = 'cancelled', updated_at_ns = ? WHERE id = ? AND status = 'executing' AND stop_requested = 1`, at.UTC().UnixNano(), planID); err != nil {
		return err
	}
	if err := execAgentOne(ctx, tx, `UPDATE tasks SET status = 'cancelled', updated_at_ns = ? WHERE id = ? AND status IN ('running', 'paused')`, at.UTC().UnixNano(), taskID); err != nil {
		return err
	}
	return appendAgentAudit(ctx, tx, taskID, planID, "", "plan_execution_stopped", "stopped", "当前操作已安全结束，后续操作已停止", at)
}

func finalizeAgentPlan(ctx context.Context, tx *sql.Tx, taskID, planID string, at time.Time) error {
	var total, succeeded, failed, partial, unsettled int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN execution_status = 'succeeded' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN execution_status = 'failed' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN execution_status = 'partial' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN execution_status IN ('pending', 'running') THEN 1 ELSE 0 END), 0)
		FROM operations WHERE plan_id = ?`, planID).Scan(&total, &succeeded, &failed, &partial, &unsettled); err != nil {
		return err
	}
	if total == 0 || unsettled != 0 {
		return ErrConflict
	}
	planStatus, taskStatus := PlanCompleted, TaskSucceeded
	if failed > 0 || partial > 0 {
		planStatus = PlanFailed
		if succeeded > 0 || partial > 0 {
			taskStatus = TaskPartial
		} else {
			taskStatus = TaskFailed
		}
	}
	if err := execAgentOne(ctx, tx, `UPDATE plans SET status = ?, updated_at_ns = ? WHERE id = ? AND status = 'executing'`, planStatus, at.UTC().UnixNano(), planID); err != nil {
		return err
	}
	return execAgentOne(ctx, tx, `UPDATE tasks SET status = ?, updated_at_ns = ? WHERE id = ? AND status IN ('running', 'paused')`, taskStatus, at.UTC().UnixNano(), taskID)
}

// CancelUnstartedAgentOperations 在首项失败后取消所有尚未开始的操作。
func (s *Store) CancelUnstartedAgentOperations(ctx context.Context, planID, exceptID, message string, at time.Time) error {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	return s.transitionAgentOperation(ctx, planID, exceptID, at, func(tx *sql.Tx, _ string) error {
		return cancelAgentOperations(ctx, tx, planID, exceptID, message, at)
	})
}

// FinalizeAgentPlan 根据各项结果汇总整份方案状态。
func (s *Store) FinalizeAgentPlan(ctx context.Context, planID string, at time.Time) error {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	return s.transitionAgentOperation(ctx, planID, "", at, func(tx *sql.Tx, taskID string) error {
		return finalizeAgentPlan(ctx, tx, taskID, planID, at)
	})
}

// InvalidateAgentOperation 记录执行前快照失效。
func (s *Store) InvalidateAgentOperation(ctx context.Context, planID, operationID, message string, at time.Time) error {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	return s.transitionAgentOperation(ctx, planID, operationID, at, func(tx *sql.Tx, taskID string) error {
		result, err := tx.ExecContext(ctx, `
			UPDATE operations SET approval_state = 'invalidated', approved_by = '', approved_at_ns = NULL,
				execution_status = 'skipped', result_message = ?, updated_at_ns = ?
			WHERE id = ? AND plan_id = ? AND execution_status IN ('pending', 'running') AND approval_state IN ('pending', 'approved')`,
			message, at.UTC().UnixNano(), operationID, planID)
		if err != nil {
			return err
		}
		if err := requireOneRow(result, "agent operation"); err != nil {
			return ErrConflict
		}
		if err := cancelAgentOperations(ctx, tx, planID, operationID, "方案因文件状态变化而失效，后续操作已取消", at); err != nil {
			return err
		}
		if err := execAgentOne(ctx, tx, `UPDATE plans SET status = 'invalidated', updated_at_ns = ? WHERE id = ? AND status IN ('awaiting_approval', 'approved', 'executing')`, at.UTC().UnixNano(), planID); err != nil {
			return err
		}
		var succeeded int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM operations WHERE plan_id = ? AND execution_status = 'succeeded'`, planID).Scan(&succeeded); err != nil {
			return err
		}
		taskStatus := TaskFailed
		if succeeded > 0 {
			taskStatus = TaskPartial
		}
		if err := execAgentOne(ctx, tx, `UPDATE tasks SET status = ?, updated_at_ns = ? WHERE id = ?`, taskStatus, at.UTC().UnixNano(), taskID); err != nil {
			return err
		}
		return appendAgentAudit(ctx, tx, taskID, planID, operationID, "operation_invalidated", "stale", message, at)
	})
}

func (s *Store) transitionAgentOperation(ctx context.Context, planID, operationID string, at time.Time, apply func(*sql.Tx, string) error) error {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin agent operation transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var taskID string
	if err := tx.QueryRowContext(ctx, `SELECT task_id FROM plans WHERE id = ?`, planID).Scan(&taskID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("read agent task: %w", err)
	}
	if err := apply(tx, taskID); err != nil {
		return fmt.Errorf("change agent operation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit agent operation: %w", err)
	}
	return nil
}

func appendAgentAudit(ctx context.Context, tx *sql.Tx, taskID, planID, operationID, event, outcome, message string, at time.Time) error {
	var sourcePath, targetPath string
	if operationID != "" {
		if err := tx.QueryRowContext(ctx, `SELECT source_path, target_path FROM operations WHERE id = ?`, operationID).Scan(&sourcePath, &targetPath); err != nil {
			return err
		}
	}
	_, err := appendAuditRecord(ctx, tx, AuditRecord{
		TaskID: taskID, PlanID: planID, OperationID: operationID,
		EventType: event, Actor: "user", Outcome: outcome, Message: message,
		SourcePath: sourcePath, TargetPath: targetPath, CreatedAt: at,
	})
	return err
}

func execAgentOne(ctx context.Context, tx *sql.Tx, statement string, args ...any) error {
	result, err := tx.ExecContext(ctx, statement, args...)
	if err != nil {
		return err
	}
	if err := requireOneRow(result, "agent state"); err != nil {
		return ErrConflict
	}
	return nil
}

// RunningAgentPlans 返回进程中断时仍在执行的方案。
func (s *Store) RunningAgentPlans(ctx context.Context) ([]PlanRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.id FROM plans p JOIN tasks t ON t.id = p.task_id
		WHERE p.status = 'executing' AND t.kind = 'agent'
		ORDER BY p.created_at_ns, p.id`)
	if err != nil {
		return nil, fmt.Errorf("list running agent plans: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	plans := make([]PlanRecord, 0, len(ids))
	for _, id := range ids {
		plan, err := s.Plan(ctx, id)
		if err != nil {
			return nil, err
		}
		plans = append(plans, plan)
	}
	return plans, nil
}

// ActiveAgentTextPayloadIDs 返回仍需保留候选正文的操作 ID。
func (s *Store) ActiveAgentTextPayloadIDs(ctx context.Context) (map[string]struct{}, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT o.id FROM operations o JOIN plans p ON p.id = o.plan_id
		WHERE o.action IN ('edit_text', 'create_file', 'overwrite_file') AND o.execution_status IN ('pending', 'running')
			AND p.status IN ('awaiting_approval', 'approved', 'executing')`)
	if err != nil {
		return nil, fmt.Errorf("list active Agent text candidates: %w", err)
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

// ActiveAgentTextPlans 返回仍待审批或执行且引用文本候选的方案。
func (s *Store) ActiveAgentTextPlans(ctx context.Context) ([]PlanRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT p.id FROM plans p JOIN operations o ON o.plan_id = p.id
		JOIN tasks t ON t.id = p.task_id
		WHERE t.kind = 'agent' AND o.action IN ('edit_text', 'create_file', 'overwrite_file')
			AND o.execution_status = 'pending' AND p.status IN ('awaiting_approval', 'approved', 'executing')
		ORDER BY p.created_at_ns, p.id`)
	if err != nil {
		return nil, fmt.Errorf("list active Agent text plans: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	plans := make([]PlanRecord, 0, len(ids))
	for _, id := range ids {
		plan, err := s.Plan(ctx, id)
		if err != nil {
			return nil, err
		}
		plans = append(plans, plan)
	}
	return plans, nil
}

// RecoverAgentOperation 处理重启后的在途操作，绝不自动重放文件写入。
func (s *Store) RecoverAgentOperation(ctx context.Context, planID, operationID, resolution, message string, at time.Time) error {
	if resolution != "retry" && resolution != "succeeded" && resolution != "partial" {
		return errors.New("invalid agent recovery resolution")
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	return s.transitionAgentOperation(ctx, planID, operationID, at, func(tx *sql.Tx, taskID string) error {
		var approval ApprovalState
		var stopRequested int
		if err := tx.QueryRowContext(ctx, `
			SELECT o.approval_state, p.stop_requested FROM operations o JOIN plans p ON p.id = o.plan_id
			WHERE o.id = ? AND o.plan_id = ? AND o.execution_status = 'running'`, operationID, planID).Scan(&approval, &stopRequested); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrConflict
			}
			return err
		}
		if approval != ApprovalApproved {
			return ErrConflict
		}
		if resolution == "retry" {
			if stopRequested != 0 {
				result, err := tx.ExecContext(ctx, `UPDATE operations SET approval_state = 'invalidated', approved_by = '', approved_at_ns = NULL,
					execution_status = 'cancelled', result_message = ?, updated_at_ns = ?
					WHERE id = ? AND plan_id = ? AND execution_status = 'running'`, message, at.UTC().UnixNano(), operationID, planID)
				if err != nil {
					return err
				}
				if err := requireOneRow(result, "stopped recovered operation"); err != nil {
					return ErrConflict
				}
				if err := appendAgentAudit(ctx, tx, taskID, planID, operationID, "operation_recovered", "stopped", message, at); err != nil {
					return err
				}
				return finishAgentPlanStop(ctx, tx, taskID, planID, at)
			}
			result, err := tx.ExecContext(ctx, `
				UPDATE operations SET execution_status = 'pending', result_message = ?, updated_at_ns = ?
				WHERE id = ? AND plan_id = ? AND execution_status = 'running'`,
				message, at.UTC().UnixNano(), operationID, planID)
			if err != nil {
				return err
			}
			if err := requireOneRow(result, "agent operation"); err != nil {
				return ErrConflict
			}
			if err := execAgentOne(ctx, tx, `UPDATE plans SET status = 'approved', version = version + 1, updated_at_ns = ? WHERE id = ? AND status = 'executing'`, at.UTC().UnixNano(), planID); err != nil {
				return err
			}
			if err := execAgentOne(ctx, tx, `UPDATE tasks SET status = 'paused', updated_at_ns = ? WHERE id = ?`, at.UTC().UnixNano(), taskID); err != nil {
				return err
			}
			return appendAgentAudit(ctx, tx, taskID, planID, operationID, "operation_recovered", "retry_ready", message, at)
		}
		status, outcome := OperationPartial, "manual_review"
		if resolution == "succeeded" {
			status, outcome = OperationSucceeded, "recovered_success"
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE operations SET execution_status = ?, result_message = ?, updated_at_ns = ?
			WHERE id = ? AND plan_id = ? AND execution_status = 'running'`,
			status, message, at.UTC().UnixNano(), operationID, planID)
		if err != nil {
			return err
		}
		if err := requireOneRow(result, "agent operation"); err != nil {
			return ErrConflict
		}
		if resolution == "partial" {
			if err := cancelAgentOperations(ctx, tx, planID, operationID, "重启后操作结果不确定，后续操作已取消", at); err != nil {
				return err
			}
			if err := execAgentOne(ctx, tx, `UPDATE plans SET status = 'failed', updated_at_ns = ? WHERE id = ? AND status = 'executing'`, at.UTC().UnixNano(), planID); err != nil {
				return err
			}
			if err := execAgentOne(ctx, tx, `UPDATE tasks SET status = 'partial', updated_at_ns = ? WHERE id = ?`, at.UTC().UnixNano(), taskID); err != nil {
				return err
			}
		} else if stopRequested != 0 {
			var pending int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM operations WHERE plan_id = ? AND execution_status = 'pending'`, planID).Scan(&pending); err != nil {
				return err
			}
			if pending > 0 {
				if err := appendAgentAudit(ctx, tx, taskID, planID, operationID, "operation_recovered", outcome, message, at); err != nil {
					return err
				}
				return finishAgentPlanStop(ctx, tx, taskID, planID, at)
			}
			if err := finalizeAgentPlan(ctx, tx, taskID, planID, at); err != nil {
				return err
			}
		} else {
			var pending int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM operations WHERE plan_id = ? AND execution_status = 'pending'`, planID).Scan(&pending); err != nil {
				return err
			}
			if pending > 0 {
				if err := execAgentOne(ctx, tx, `UPDATE plans SET status = 'approved', version = version + 1, updated_at_ns = ? WHERE id = ? AND status = 'executing'`, at.UTC().UnixNano(), planID); err != nil {
					return err
				}
				if err := execAgentOne(ctx, tx, `UPDATE tasks SET status = 'paused', updated_at_ns = ? WHERE id = ?`, at.UTC().UnixNano(), taskID); err != nil {
					return err
				}
			} else if err := finalizeAgentPlan(ctx, tx, taskID, planID, at); err != nil {
				return err
			}
		}
		return appendAgentAudit(ctx, tx, taskID, planID, operationID, "operation_recovered", outcome, message, at)
	})
}

// RecoverIdleAgentPlan 在操作间重启后暂停方案并要求用户再次确认。
func (s *Store) RecoverIdleAgentPlan(ctx context.Context, planID string, at time.Time) error {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	return s.transitionAgentOperation(ctx, planID, "", at, func(tx *sql.Tx, taskID string) error {
		var running, pending, stopRequested int
		if err := tx.QueryRowContext(ctx, `SELECT stop_requested FROM plans WHERE id = ?`, planID).Scan(&stopRequested); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT
			COALESCE(SUM(CASE WHEN execution_status = 'running' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN execution_status = 'pending' THEN 1 ELSE 0 END), 0)
			FROM operations WHERE plan_id = ?`, planID).Scan(&running, &pending); err != nil {
			return err
		}
		if running != 0 {
			return ErrConflict
		}
		if stopRequested != 0 {
			if pending > 0 {
				return finishAgentPlanStop(ctx, tx, taskID, planID, at)
			}
			if err := finalizeAgentPlan(ctx, tx, taskID, planID, at); err != nil {
				return err
			}
			return appendAgentAudit(ctx, tx, taskID, planID, "", "plan_recovered", "completed", "服务重启后确认所有操作均已完成", at)
		}
		if pending > 0 {
			if err := execAgentOne(ctx, tx, `UPDATE plans SET status = 'approved', version = version + 1, updated_at_ns = ? WHERE id = ? AND status = 'executing'`, at.UTC().UnixNano(), planID); err != nil {
				return err
			}
			if err := execAgentOne(ctx, tx, `UPDATE tasks SET status = 'paused', updated_at_ns = ? WHERE id = ? AND status = 'running'`, at.UTC().UnixNano(), taskID); err != nil {
				return err
			}
			return appendAgentAudit(ctx, tx, taskID, planID, "", "plan_recovered", "confirmation_required", "服务重启后未自动继续，需用户再次确认执行", at)
		}
		if err := finalizeAgentPlan(ctx, tx, taskID, planID, at); err != nil {
			return err
		}
		return appendAgentAudit(ctx, tx, taskID, planID, "", "plan_recovered", "completed", "服务重启后确认所有操作均已完成", at)
	})
}
