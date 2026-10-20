//go:build !linux

package fsroot

import "context"

// ConfigureMoveRecoveryDirectory 在不支持跨卷安全移动的平台上无需设置日志。
func (r *Root) ConfigureMoveRecoveryDirectory(string) error { return nil }

// RecoverInterruptedMoves 当前平台不执行跨卷移动，无需恢复日志。
func (r *Root) RecoverInterruptedMoves(context.Context) (MoveRecoverySummary, error) {
	return MoveRecoverySummary{}, nil
}

func platformRequireMoveRecovery(*Root) error { return nil }
