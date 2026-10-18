// 本文件提供经过版本校验的单文件复制与移动入口。
package fsroot

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
)

// FileCopyResult 描述复制内容是否已原子发布。
type FileCopyResult struct {
	SHA256    string
	Published bool
}

// FileMoveResult 记录目标发布、源删除和跨卷复制摘要。
type FileMoveResult struct {
	Published     bool
	SourceRemoved bool
	SHA256        string
}

// CopyFileNoReplace 只复制状态未变化的普通文件，目标始终禁止覆盖。
func (r *Root) CopyFileNoReplace(ctx context.Context, source, destination string, expected Fingerprint) (FileCopyResult, error) {
	if err := validateFileOperationPaths(source, destination, expected); err != nil {
		return FileCopyResult{}, err
	}
	if ctx == nil {
		return FileCopyResult{}, fmt.Errorf("copy %q: nil context: %w", source, fs.ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return FileCopyResult{}, err
	}
	if err := r.checkRegularFileFingerprint(source, expected); err != nil {
		return FileCopyResult{}, err
	}
	if err := r.rejectSymlinkParents(destination); err != nil {
		return FileCopyResult{}, err
	}
	if _, err := r.root.Lstat(destination); err == nil {
		return FileCopyResult{}, conflictError("copy", destination, fs.ErrExist)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return FileCopyResult{}, fmt.Errorf("inspect copy target %q: %w", destination, err)
	}
	return platformCopyFileNoReplace(ctx, r, source, destination, expected)
}

// MoveFileNoReplace 同卷原子移动；跨卷先复制并核验，再删除未变化的源文件。
func (r *Root) MoveFileNoReplace(ctx context.Context, source, destination string, expected Fingerprint) (FileMoveResult, error) {
	if err := validateFileOperationPaths(source, destination, expected); err != nil {
		return FileMoveResult{}, err
	}
	if ctx == nil {
		return FileMoveResult{}, fmt.Errorf("move %q: nil context: %w", source, fs.ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return FileMoveResult{}, err
	}
	if err := r.checkRegularFileFingerprint(source, expected); err != nil {
		return FileMoveResult{}, err
	}
	destinationParent := path.Dir(destination)
	if destinationParent == "." {
		destinationParent = ""
	}
	sourceMount, err := r.MountID(source)
	if err != nil {
		return FileMoveResult{}, err
	}
	destinationMount, err := r.MountID(destinationParent)
	if err != nil {
		return FileMoveResult{}, err
	}
	if sourceMount == destinationMount {
		if err := ctx.Err(); err != nil {
			return FileMoveResult{}, err
		}
		if err := r.RenameNoReplace(source, destination); err != nil {
			return FileMoveResult{}, err
		}
		return FileMoveResult{Published: true, SourceRemoved: true}, nil
	}

	copyResult, err := r.CopyFileNoReplace(ctx, source, destination, expected)
	outcome := FileMoveResult{Published: copyResult.Published, SHA256: copyResult.SHA256}
	if err != nil {
		return outcome, err
	}
	if err := ctx.Err(); err != nil {
		return outcome, err
	}
	removed, err := platformRemoveFileIfUnchanged(r, source, expected)
	outcome.SourceRemoved = removed
	return outcome, err
}

// validateFileOperationPaths 校验根目录内的文件路径和来源快照。
func validateFileOperationPaths(source, destination string, expected Fingerprint) error {
	if err := validateMutationPath(source); err != nil || hasTrashComponent(source) {
		return ErrInvalidPath
	}
	if err := validateMutationPath(destination); err != nil || hasTrashComponent(destination) {
		return ErrInvalidPath
	}
	if source == destination {
		return conflictError("file operation", destination, fs.ErrExist)
	}
	if !expected.Exists || expected.Kind != KindFile {
		return ErrNotRegularFile
	}
	if !expected.IdentityAvailable {
		return fmt.Errorf("file operation source: %w", ErrIdentityUnavailable)
	}
	return nil
}

// checkRegularFileFingerprint 拒绝链接和状态已变化的来源文件。
func (r *Root) checkRegularFileFingerprint(relativePath string, expected Fingerprint) error {
	if err := r.rejectSymlinkParents(relativePath); err != nil {
		return err
	}
	info, err := r.root.Lstat(relativePath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return conflictError("file operation", relativePath, err)
		}
		return err
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("file operation source %q: %w", relativePath, ErrNotRegularFile)
	}
	current := fingerprintFromInfo(info)
	if !current.IdentityAvailable || !sameFingerprint(current, expected) {
		return conflictError("file operation", relativePath, nil)
	}
	return nil
}
