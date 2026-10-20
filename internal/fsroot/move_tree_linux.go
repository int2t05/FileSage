//go:build linux

// 本文件实现 Linux 目录树移动及跨挂载安全回退。
package fsroot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"

	"golang.org/x/sys/unix"
)

// platformMoveTreeNoReplace 在同挂载时原子移动，跨挂载时复制后隔离并移除源。
func platformMoveTreeNoReplace(ctx context.Context, root *Root, source, destination string, expected TreeSnapshot) (TreeMoveResult, error) {
	if err := validateTreeMoveSnapshot(expected); err != nil {
		return TreeMoveResult{}, err
	}
	if source == "" || source != expected.RootPath {
		return TreeMoveResult{}, ErrInvalidPath
	}
	if err := root.VerifyTreeSnapshot(ctx, expected); err != nil {
		return TreeMoveResult{}, err
	}
	sourceMountID := expected.Entries[0].MountID
	if err := root.CheckTreeMounts(source, sourceMountID); err != nil {
		return TreeMoveResult{}, err
	}
	for _, item := range expected.Entries {
		if item.MountID != sourceMountID {
			return TreeMoveResult{}, ErrNestedMount
		}
	}
	sourceParentPath := parentPathForOperation(source)
	sourceParentMountID, err := root.MountID(sourceParentPath)
	if err != nil {
		return TreeMoveResult{}, err
	}
	if sourceParentMountID != sourceMountID {
		return TreeMoveResult{}, ErrMountRoot
	}
	destinationParentPath := parentPathForOperation(destination)
	destinationMountID, err := root.MountID(destinationParentPath)
	if err != nil {
		return TreeMoveResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return TreeMoveResult{}, err
	}

	if destinationMountID == sourceMountID {
		if err := root.VerifyTreeSnapshot(ctx, expected); err != nil {
			return TreeMoveResult{}, err
		}
		if err := root.RenameNoReplace(source, destination); err == nil {
			result := TreeMoveResult{Published: true, SourceRemoved: true, Atomic: true}
			actual, verifyErr := root.SnapshotTree(ctx, destination)
			if verifyErr != nil || !sameRelocatedTreeSnapshot(expected, actual, source, destination) {
				if verifyErr == nil {
					verifyErr = ErrConflict
				}
				return restoreSameMountTree(root, source, destination, expected, result, verifyErr)
			}
			if err := syncMoveParents(root, source, destination); err != nil {
				return result, fmt.Errorf("%w: %w", ErrPublishedNotDurable, err)
			}
			return result, nil
		} else if !errors.Is(err, unix.EXDEV) && !errors.Is(err, ErrNoReplaceUnsupported) {
			return TreeMoveResult{}, err
		}
	}
	if err := platformRequireMoveRecovery(root); err != nil {
		return TreeMoveResult{}, err
	}
	return copyThenRemoveTree(ctx, root, source, destination, expected, sourceMountID)
}

// restoreSameMountTree 在同卷改名后的快照不符时恢复来源。
func restoreSameMountTree(root *Root, source, destination string, expected TreeSnapshot, result TreeMoveResult, cause error) (TreeMoveResult, error) {
	current, err := root.LstatEntry(destination)
	if err != nil || current.Kind != KindDirectory || !current.IdentityAvailable || current.Device != expected.Entries[0].Entry.Device || current.Inode != expected.Entries[0].Entry.Inode {
		result.SourceRetained = destination
		return result, errors.Join(cause, fmt.Errorf("restore moved directory: %w", errors.Join(ErrPathChanged, err)))
	}
	if err := root.RenameNoReplace(destination, source); err != nil {
		result.SourceRetained = destination
		return result, errors.Join(cause, fmt.Errorf("restore moved directory: %w", err))
	}
	result.Published = false
	result.SourceRemoved = false
	result.SourceRetained = ""
	if err := syncMoveParents(root, source, destination); err != nil {
		return result, errors.Join(cause, fmt.Errorf("%w: restore directory source: %w", ErrPublishedNotDurable, err))
	}
	return result, cause
}

// copyThenRemoveTree 先验证并发布副本，再隔离源目录、复核快照并安全删除。
func copyThenRemoveTree(ctx context.Context, root *Root, source, destination string, expected TreeSnapshot, sourceMountID uint64) (TreeMoveResult, error) {
	copyResult, err := root.CopyTreeNoReplace(ctx, source, destination, expected)
	result := TreeMoveResult{Published: copyResult.Published}
	if err != nil {
		return result, err
	}
	if !copyResult.Published {
		return result, errors.New("directory copy did not confirm publication")
	}
	destinationSnapshot, err := root.SnapshotTree(ctx, destination)
	if err != nil {
		return result, err
	}
	if len(destinationSnapshot.Entries) == 0 {
		return result, ErrConflict
	}
	destinationRoot := destinationSnapshot.Entries[0].Entry
	if destinationRoot.Kind != KindDirectory || !destinationRoot.IdentityAvailable {
		return result, ErrIdentityUnavailable
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := root.VerifyTreeSnapshot(ctx, expected); err != nil {
		return result, err
	}
	var quarantine string
	var recoveryID string
	for range 8 {
		quarantine, err = createMoveQuarantinePath(source)
		if err != nil {
			return result, fmt.Errorf("create source quarantine path: %w", err)
		}
		record := moveRecoveryRecord{
			Version: 1, ID: moveRecoveryID(quarantine), Kind: moveRecoveryTree,
			SourcePath: source, QuarantinePath: quarantine,
			DestinationPath: destination,
			SourceIdentity: moveObjectIdentity{
				Device: expected.Entries[0].Entry.Device,
				Inode:  expected.Entries[0].Entry.Inode,
			},
			QuarantineIdentity: moveObjectIdentity{
				Device: expected.Entries[0].Entry.Device,
				Inode:  expected.Entries[0].Entry.Inode,
			},
			DestinationIdentity:    moveObjectIdentity{Device: destinationRoot.Device, Inode: destinationRoot.Inode},
			DestinationFingerprint: destinationRoot.Fingerprint(),
			DestinationDigest:      destinationSnapshot.Digest,
		}
		if err := root.writeMoveRecoveryRecord(record); err != nil {
			return result, fmt.Errorf("record interrupted directory move: %w", err)
		}
		recoveryID = record.ID
		err = root.RenameNoReplace(source, quarantine)
		if err == nil {
			break
		}
		if cleanupErr := root.removeMoveRecoveryRecord(recoveryID); cleanupErr != nil {
			return result, errors.Join(fmt.Errorf("isolate move source before removal: %w", err), cleanupErr)
		}
		if !errors.Is(err, fs.ErrExist) {
			return result, fmt.Errorf("isolate move source before removal: %w", err)
		}
		if verifyErr := root.VerifyTreeSnapshot(ctx, expected); verifyErr != nil {
			return result, verifyErr
		}
	}
	if err != nil {
		return result, fmt.Errorf("reserve source quarantine path: %w", err)
	}
	result.SourceRetained = quarantine
	if err := syncMoveParents(root, source, quarantine); err != nil {
		return restoreMoveQuarantine(root, source, quarantine, result, fmt.Errorf("sync isolated source: %w", err))
	}
	if err := root.CheckTreeMounts(quarantine, sourceMountID); err != nil {
		return restoreMoveQuarantine(root, source, quarantine, result, err)
	}
	actual, err := root.SnapshotTree(ctx, quarantine)
	if err != nil {
		return restoreMoveQuarantine(root, source, quarantine, result, err)
	}
	if !sameRelocatedTreeSnapshot(expected, actual, source, quarantine) {
		return restoreMoveQuarantine(root, source, quarantine, result, ErrConflict)
	}
	if err := removeMoveQuarantine(root, quarantine, sourceMountID, expected.Entries[0].Entry); err != nil {
		return result, fmt.Errorf("remove isolated move source %q: %w", quarantine, err)
	}
	result.SourceRemoved = true
	result.SourceRetained = ""
	if err := syncMoveParent(root, source); err != nil {
		return result, fmt.Errorf("%w: %w", ErrPublishedNotDurable, err)
	}
	if err := root.removeMoveRecoveryRecord(recoveryID); err != nil {
		return result, fmt.Errorf("%w: clear completed directory move record: %w", ErrPublishedNotDurable, err)
	}
	return result, nil
}

// createMoveQuarantinePath 为源目录生成同级且不可覆盖的临时名称。
func createMoveQuarantinePath(source string) (string, error) {
	parent := parentPathForOperation(source)
	suffix := make([]byte, 12)
	if _, err := rand.Read(suffix); err != nil {
		return "", err
	}
	name := ".filesage-move-quarantine-" + hex.EncodeToString(suffix)
	if parent == "" {
		return name, nil
	}
	return path.Join(parent, name), nil
}

// sameRelocatedTreeSnapshot 忽略隔离重命名引起的根名称与 ctime 变化。
func sameRelocatedTreeSnapshot(expected, actual TreeSnapshot, source, quarantine string) bool {
	if len(expected.Entries) != len(actual.Entries) || len(actual.Entries) == 0 {
		return false
	}
	entries := append([]TreeSnapshotEntry(nil), actual.Entries...)
	for index := range entries {
		entry := &entries[index]
		switch {
		case entry.Entry.Path == quarantine:
			entry.Entry.Path = source
			entry.Entry.Name = expected.Entries[0].Entry.Name
			entry.Entry.ChangedAt = expected.Entries[0].Entry.ChangedAt
			entry.ChangedAt = expected.Entries[0].ChangedAt
		case strings.HasPrefix(entry.Entry.Path, quarantine+"/"):
			entry.Entry.Path = source + strings.TrimPrefix(entry.Entry.Path, quarantine)
		default:
			return false
		}
	}
	return digestTreeSnapshot(source, entries) == expected.Digest
}

// restoreMoveQuarantine 在发现源变化时尽量原样放回来源路径。
func restoreMoveQuarantine(root *Root, source, quarantine string, result TreeMoveResult, cause error) (TreeMoveResult, error) {
	if err := root.RenameNoReplace(quarantine, source); err != nil {
		return result, errors.Join(cause, fmt.Errorf("restore isolated source %q: %w", quarantine, err))
	}
	result.SourceRetained = ""
	if err := syncMoveParent(root, source); err != nil {
		return result, errors.Join(cause, fmt.Errorf("sync restored source: %w", err))
	}
	if err := root.removeMoveRecoveryRecord(moveRecoveryID(quarantine)); err != nil {
		return result, errors.Join(cause, fmt.Errorf("clear restored directory move record: %w", err))
	}
	return result, cause
}

// removeMoveQuarantine 仅删除本次命名的隔离目录并核验根身份与挂载边界。
func removeMoveQuarantine(root *Root, quarantine string, mountID uint64, expected Entry) error {
	if validateMutationPath(quarantine) != nil || hasTrashComponent(quarantine) || !strings.HasPrefix(path.Base(quarantine), ".filesage-move-quarantine-") {
		return ErrInvalidPath
	}
	if err := root.rejectSymlinkParents(quarantine); err != nil {
		return err
	}
	if err := root.CheckTreeMounts(quarantine, mountID); err != nil {
		return err
	}
	parent, name, err := root.openParent(quarantine)
	if err != nil {
		return err
	}
	defer parent.Close()
	return removeTreeAt(int(parent.Fd()), name, mountID, &trashIdentity{device: expected.Device, inode: expected.Inode})
}

// syncMoveParents 持久化移动前后所在目录。
func syncMoveParents(root *Root, source, destination string) error {
	if err := syncMoveParent(root, source); err != nil {
		return err
	}
	if parentPathForOperation(source) == parentPathForOperation(destination) {
		return nil
	}
	return syncMoveParent(root, destination)
}

// syncMoveParent 同步移动项的父目录。
func syncMoveParent(root *Root, relativePath string) error {
	parent, err := openRootDirectoryNoFollow(root.root, parentPathForOperation(relativePath))
	if err != nil {
		return err
	}
	defer parent.Close()
	return parent.Sync()
}
