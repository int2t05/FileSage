//go:build !linux

// 本文件明确拒绝无法安全实现的目录树移动。
package fsroot

import "context"

func platformMoveTreeNoReplace(context.Context, *Root, string, string, TreeSnapshot) (TreeMoveResult, error) {
	return TreeMoveResult{}, ErrWalkUnsupported
}
