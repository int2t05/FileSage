// 本文件提供人工操作的统一审计记录与游标分页。
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// AuditHistoryPage 保存按审计 ID 倒序排列的统一历史页。
type AuditHistoryPage struct {
	Events     []AuditRecord
	Limit      int
	NextCursor int64
	HasMore    bool
}

// BeginManualAudit 先登记人工操作，固定操作者和待完成状态。
func (s *Store) BeginManualAudit(ctx context.Context, record AuditRecord) (int64, error) {
	if !strings.HasPrefix(record.EventType, "manual_") {
		return 0, errors.New("manual audit event type must start with manual_")
	}
	if record.TaskID != "" || record.PlanID != "" || record.OperationID != "" {
		return 0, errors.New("manual audit cannot reference a task, plan, or operation")
	}
	record.Actor = "user"
	record.Outcome = "pending"
	return appendAuditRecord(ctx, s.db, record)
}

// FinishManualAudit 仅允许一个调用者完成仍待处理的人工记录。
func (s *Store) FinishManualAudit(ctx context.Context, id int64, outcome, message string) error {
	if id < 1 || !validManualAuditOutcome(outcome) {
		return errors.New("invalid manual audit completion")
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE audit_events SET outcome = ?, message = ?
		WHERE id = ? AND actor = 'user' AND task_id IS NULL
			AND plan_id IS NULL AND operation_id IS NULL
			AND substr(event_type, 1, 7) = 'manual_' AND outcome = 'pending'`,
		outcome, message, id,
	)
	if err != nil {
		return fmt.Errorf("finish manual audit: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count finished manual audits: %w", err)
	}
	if rows != 1 {
		return fmt.Errorf("manual audit is missing or already finished: %w", ErrConflict)
	}
	return nil
}

// AuditHistoryPage 使用 ID 游标分页，后续新增记录不会移动已有页边界。
func (s *Store) AuditHistoryPage(ctx context.Context, beforeID int64, limit int) (AuditHistoryPage, error) {
	if beforeID < 0 || limit < 1 || limit > 100 {
		return AuditHistoryPage{}, errors.New("invalid audit history page")
	}
	query := `SELECT id, COALESCE(task_id, ''), COALESCE(plan_id, ''),
		COALESCE(operation_id, ''), event_type, actor, outcome, message,
		source_path, target_path, created_at_ns FROM audit_events`
	args := make([]any, 0, 2)
	if beforeID > 0 {
		query += ` WHERE id < ?`
		args = append(args, beforeID)
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return AuditHistoryPage{}, fmt.Errorf("read audit history page: %w", err)
	}
	defer rows.Close()

	page := AuditHistoryPage{Events: make([]AuditRecord, 0, limit), Limit: limit}
	for rows.Next() {
		var record AuditRecord
		var createdAt int64
		if err := rows.Scan(
			&record.ID, &record.TaskID, &record.PlanID, &record.OperationID,
			&record.EventType, &record.Actor, &record.Outcome, &record.Message,
			&record.SourcePath, &record.TargetPath, &createdAt,
		); err != nil {
			return AuditHistoryPage{}, fmt.Errorf("scan audit history record: %w", err)
		}
		record.CreatedAt = time.Unix(0, createdAt).UTC()
		page.Events = append(page.Events, record)
	}
	if err := rows.Err(); err != nil {
		return AuditHistoryPage{}, fmt.Errorf("iterate audit history page: %w", err)
	}
	if len(page.Events) > limit {
		page.Events = page.Events[:limit]
		page.HasMore = true
		page.NextCursor = page.Events[len(page.Events)-1].ID
	}
	return page, nil
}

func validManualAuditOutcome(outcome string) bool {
	switch outcome {
	case "succeeded", "failed", "partial", "needs_review":
		return true
	default:
		return false
	}
}
