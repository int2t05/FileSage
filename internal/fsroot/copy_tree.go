// 本文件定义目录树安全复制接口与快照预检。
package fsroot

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
)

var (
	// ErrTreeCopyUnsafeEntry 表示目录树含软链接或特殊文件。
	ErrTreeCopyUnsafeEntry = errors.New("tree copy rejects symbolic links and special files")
	// ErrTreeCopyIntoSource 表示目标位于来源目录树内部。
	ErrTreeCopyIntoSource = errors.New("tree copy destination is inside source")
	// ErrTreeCopyInProgress 表示目标父目录仍有进程执行目录复制。
	ErrTreeCopyInProgress = errors.New("tree copy is still in progress")
)

// TreeCopyResult 描述已复制内容、逐文件摘要与原子发布状态。
type TreeCopyResult struct {
	FilesCopied int
	BytesCopied int64
	Published   bool
	FileDigests map[string]string
}

// CopyTreeNoReplace 依据已审批快照复制目录树，目标存在时不会覆盖。
func (r *Root) CopyTreeNoReplace(ctx context.Context, source, destination string, expected TreeSnapshot) (TreeCopyResult, error) {
	if ctx == nil {
		return TreeCopyResult{}, fmt.Errorf("copy tree requires a context: %w", fs.ErrInvalid)
	}
	if r == nil || r.root == nil {
		return TreeCopyResult{}, fmt.Errorf("copy tree requires an open root: %w", fs.ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return TreeCopyResult{}, err
	}
	expected, err := prepareTreeSnapshot(expected)
	if err != nil {
		return TreeCopyResult{}, err
	}
	if err := validateTreeCopy(source, destination, expected); err != nil {
		return TreeCopyResult{}, err
	}
	if err := r.checkVolumeAvailability(source); err != nil {
		return TreeCopyResult{}, err
	}
	if err := r.checkVolumeAvailability(destination); err != nil {
		return TreeCopyResult{}, err
	}
	return platformCopyTreeNoReplace(ctx, r, source, destination, expected)
}

// CleanupTreeCopyStaging 清理目标父目录内已中断且可验证的复制暂存项。
func (r *Root) CleanupTreeCopyStaging(relativeParent string) error {
	if r == nil || r.root == nil {
		return fmt.Errorf("tree copy cleanup requires an open root: %w", fs.ErrInvalid)
	}
	if err := validateRelativePath(relativeParent); err != nil || hasTrashComponent(relativeParent) {
		return ErrInvalidPath
	}
	if err := r.checkVolumeAvailability(relativeParent); err != nil {
		return err
	}
	return platformCleanupTreeCopyStaging(r, relativeParent)
}

// validateTreeCopy 检查目录复制范围和快照结构。
func validateTreeCopy(source, destination string, snapshot TreeSnapshot) error {
	if source != "" && (validateMutationPath(source) != nil || hasTrashComponent(source)) {
		return ErrInvalidPath
	}
	if validateMutationPath(destination) != nil || hasTrashComponent(destination) {
		return ErrInvalidPath
	}
	if snapshot.RootPath != source {
		return ErrInvalidPath
	}
	if err := validateTreeSnapshot(snapshot); err != nil {
		return err
	}
	if treeDestinationInsideSource(source, destination) {
		return ErrTreeCopyIntoSource
	}
	if len(snapshot.Entries) == 0 {
		return ErrInvalidPath
	}
	rootEntry := snapshot.Entries[0].Entry
	if rootEntry.Symlink || rootEntry.Kind == KindOther {
		return fmt.Errorf("%q: %w", rootEntry.Path, ErrTreeCopyUnsafeEntry)
	}
	if rootEntry.Kind != KindDirectory {
		return ErrNotDirectory
	}

	entries := make(map[string]Entry, len(snapshot.Entries))
	for index, item := range snapshot.Entries {
		entry := item.Entry
		if entry.MountUnavailable || entry.ReadOnly {
			return ErrVolumeUnavailable
		}
		if entry.Symlink || entry.LinkIdentityAvailable || entry.LinkTarget != "" {
			return fmt.Errorf("%q: %w", entry.Path, ErrTreeCopyUnsafeEntry)
		}
		switch entry.Kind {
		case KindDirectory:
			if !entry.Mode.IsDir() {
				return ErrInvalidPath
			}
		case KindFile:
			if !entry.Mode.IsRegular() {
				return ErrInvalidPath
			}
		case KindOther:
			return fmt.Errorf("%q: %w", entry.Path, ErrTreeCopyUnsafeEntry)
		default:
			return ErrInvalidPath
		}
		if entry.Name != path.Base(entry.Path) {
			if entry.Path != "" || entry.Name != "." {
				return ErrInvalidPath
			}
		}
		if index == 0 && entry.Path != source {
			return ErrInvalidPath
		}
		entries[entry.Path] = entry
	}
	for _, item := range snapshot.Entries[1:] {
		parent := path.Dir(item.Entry.Path)
		if parent == "." {
			parent = ""
		}
		parentEntry, ok := entries[parent]
		if !ok || parentEntry.Kind != KindDirectory {
			return ErrInvalidPath
		}
	}
	return nil
}

// treeDestinationInsideSource 使用根相对路径判断目标是否嵌套于来源。
func treeDestinationInsideSource(source, destination string) bool {
	if source == "" {
		return true
	}
	return destination == source || strings.HasPrefix(destination, source+"/")
}
