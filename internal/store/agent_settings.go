// 本文件保存全局 Agent 权限设置，并记录设置变更审计。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// AgentSettings 保存单用户实例的全局 Agent 权限偏好。
type AgentSettings struct {
	BypassPermission bool      `json:"bypassPermission"`
	UpdatedAt        time.Time `json:"updatedAt"`
}

// AgentSettings 返回持久化的 Agent 权限设置。
func (s *Store) AgentSettings(ctx context.Context) (AgentSettings, error) {
	var bypass int
	var updatedAt int64
	err := s.db.QueryRowContext(ctx, `SELECT bypass_permission, updated_at_ns FROM agent_settings WHERE id = 1`).Scan(&bypass, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentSettings{}, errors.New("Agent settings are not initialized")
	}
	if err != nil {
		return AgentSettings{}, fmt.Errorf("read Agent settings: %w", err)
	}
	return AgentSettings{BypassPermission: bypass == 1, UpdatedAt: time.Unix(0, updatedAt).UTC()}, nil
}

// SetAgentBypassPermission 更新全局偏好并写入审计记录。
func (s *Store) SetAgentBypassPermission(ctx context.Context, bypass bool, at time.Time) error {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	at = at.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin Agent settings update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var current int
	if err := tx.QueryRowContext(ctx, `SELECT bypass_permission FROM agent_settings WHERE id = 1`).Scan(&current); err != nil {
		return fmt.Errorf("read Agent permission setting: %w", err)
	}
	next := boolInt(bypass)
	if current == next {
		return tx.Commit()
	}
	if err := execAgentOne(ctx, tx, `UPDATE agent_settings SET bypass_permission = ?, updated_at_ns = ? WHERE id = 1`, next, at.UnixNano()); err != nil {
		return fmt.Errorf("update Agent permission setting: %w", err)
	}
	mode, outcome := "逐步确认", "manual"
	if bypass {
		mode, outcome = "自动逐步执行", "bypass"
	}
	if _, err := appendAuditRecord(ctx, tx, AuditRecord{
		EventType: "agent_permission_mode_changed", Actor: "user", Outcome: outcome,
		Message: "Agent 执行模式已设为：" + mode, CreatedAt: at,
	}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit Agent settings update: %w", err)
	}
	return nil
}
