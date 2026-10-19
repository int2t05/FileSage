// 本文件定义目录树移动结果与快照校验。
package fsroot

import (
	"context"
	"fmt"
	"io/fs"
)

// TreeMoveResult 描述目标发布、源目录移除及失败时的隔离路径。
type TreeMoveResult struct {
	Published      bool
	SourceRemoved  bool
	Atomic         bool
	SourceRetained string
}

// MoveTreeNoReplace 原子移动同挂载目录，跨挂载时复制校验后再移除源目录。
func (r *Root) MoveTreeNoReplace(ctx context.Context, source, destination string, expected TreeSnapshot) (TreeMoveResult, error) {
	if ctx == nil {
		return TreeMoveResult{}, fmt.Errorf("move tree requires a context: %w", fs.ErrInvalid)
	}
	if r == nil || r.root == nil {
		return TreeMoveResult{}, fmt.Errorf("move tree requires an open root: %w", fs.ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return TreeMoveResult{}, err
	}
	expected, err := prepareTreeSnapshot(expected)
	if err != nil {
		return TreeMoveResult{}, err
	}
	if err := validateTreeCopy(source, destination, expected); err != nil {
		return TreeMoveResult{}, err
	}
	if source == "" || source == destination {
		return TreeMoveResult{}, ErrInvalidPath
	}
	if err := r.checkVolumeAvailability(source); err != nil {
		return TreeMoveResult{}, err
	}
	if err := r.checkVolumeAvailability(destination); err != nil {
		return TreeMoveResult{}, err
	}
	return platformMoveTreeNoReplace(ctx, r, source, destination, expected)
}

// validateTreeMoveSnapshot 仅允许由普通目录和普通文件组成的树。
func validateTreeMoveSnapshot(snapshot TreeSnapshot) error {
	if snapshot.RootPath == "" {
		return ErrInvalidPath
	}
	if err := validateTreeSnapshot(snapshot); err != nil {
		return err
	}
	for _, item := range snapshot.Entries {
		if item.Entry.Kind != KindDirectory && item.Entry.Kind != KindFile {
			return fmt.Errorf("%q: %w", item.Entry.Path, ErrTreeCopyUnsafeEntry)
		}
	}
	return nil
}
