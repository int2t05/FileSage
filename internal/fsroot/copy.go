// 本文件提供经过版本校验的单文件复制与移动入口。
package fsroot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// FileMoveResult 记录移动结果及冲突时保留的来源路径。
type FileMoveResult struct {
	Published      bool
	SourceRemoved  bool
	SourceRetained string
	SHA256         string
}

// MoveRecoverySummary 汇总启动时恢复、清理与待人工核查的操作。
type MoveRecoverySummary struct {
	Restored     int
	Cleaned      int
	Pending      int
	PendingPaths []string
}

// CopyFileNoReplace 只复制状态未变化的普通文件，目标始终禁止覆盖。
func (r *Root) CopyFileNoReplace(ctx context.Context, source, destination string, expected Fingerprint) (FileCopyResult, error) {
	return r.copyFileNoReplace(ctx, source, destination, expected, "")
}

// copyFileNoReplace 按可选摘要校验后原子发布副本。
func (r *Root) copyFileNoReplace(ctx context.Context, source, destination string, expected Fingerprint, expectedSHA256 string) (FileCopyResult, error) {
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
	return platformCopyFileNoReplace(ctx, r, source, destination, expected, expectedSHA256)
}

// MoveFileNoReplace 同卷原子移动并复核来源；跨卷复制后复核再删除源文件。
func (r *Root) MoveFileNoReplace(ctx context.Context, source, destination string, expected Fingerprint) (FileMoveResult, error) {
	return r.moveFileNoReplace(ctx, source, destination, expected, "")
}

// MoveFileNoReplaceWithSHA256 仅在内容与调用方提供的摘要一致时提交来源。
func (r *Root) MoveFileNoReplaceWithSHA256(ctx context.Context, source, destination string, expected Fingerprint, expectedSHA256 string) (FileMoveResult, error) {
	digest, err := hex.DecodeString(expectedSHA256)
	if err != nil || len(digest) != sha256.Size {
		return FileMoveResult{}, fmt.Errorf("move content digest: %w", fs.ErrInvalid)
	}
	return r.moveFileNoReplace(ctx, source, destination, expected, expectedSHA256)
}

// moveFileNoReplace 按可选摘要校验后移动文件。
func (r *Root) moveFileNoReplace(ctx context.Context, source, destination string, expected Fingerprint, expectedSHA256 string) (FileMoveResult, error) {
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
		return platformMoveFileSameMountNoReplace(ctx, r, source, destination, expected, expectedSHA256)
	}
	if err := platformRequireMoveRecovery(r); err != nil {
		return FileMoveResult{}, err
	}

	copyResult, err := r.copyFileNoReplace(ctx, source, destination, expected, expectedSHA256)
	outcome := FileMoveResult{Published: copyResult.Published, SHA256: copyResult.SHA256}
	if err != nil {
		return outcome, err
	}
	if err := ctx.Err(); err != nil {
		return outcome, err
	}
	removed, retained, err := platformRemoveFileIfUnchanged(ctx, r, source, destination, expected, copyResult.SHA256)
	outcome.SourceRemoved = removed
	outcome.SourceRetained = retained
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
