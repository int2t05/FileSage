//go:build !linux

// 本文件为其他平台声明明确的不支持语义。
package fsroot

import (
	"context"
)

// platformSnapshotSupported 提示调用方当前平台无法安全扫描。
func platformSnapshotSupported() error { return ErrWalkUnsupported }

func platformSnapshotTree(context.Context, *Root, string, *TreeSnapshotLimits) (TreeSnapshot, error) {
	return TreeSnapshot{}, ErrWalkUnsupported
}
