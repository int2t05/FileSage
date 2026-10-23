//go:build linux

// 本文件通过同卷隔离和恢复日志安全执行永久删除。

package fsroot

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"

	"golang.org/x/sys/unix"
)

// platformRemoveEntryIfUnchanged 隔离并复核目录项后再永久删除。
func platformRemoveEntryIfUnchanged(root *Root, expected Entry) error {
	return removeEntryIfUnchanged(root, expected, unix.Renameat2)
}

// removeEntryIfUnchanged 隔离、复核并删除审批指定的目录项。
func removeEntryIfUnchanged(root *Root, expected Entry, renameat2 func(int, string, int, string, uint) error) error {
	if err := platformRequireMoveRecovery(root); err != nil {
		return err
	}
	if renameat2 == nil {
		return ErrNoReplaceUnsupported
	}
	parentPath, name := parentPathForOperation(expected.Path), path.Base(expected.Path)
	parent, err := openRootDirectoryNoFollow(root.root, parentPath)
	if err != nil {
		return fmt.Errorf("open delete parent: %w", err)
	}
	defer parent.Close()
	if err := verifyOpenedDirectory(root, parentPath, parent); err != nil {
		return err
	}

	flags := unix.O_RDONLY | unix.O_NONBLOCK | unix.O_NOFOLLOW | unix.O_CLOEXEC
	if expected.Symlink {
		flags = unix.O_PATH | unix.O_NOFOLLOW | unix.O_CLOEXEC
	} else if expected.Kind == KindDirectory {
		flags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	}
	fd, err := unix.Openat(int(parent.Fd()), name, flags, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			return conflictError("permanent delete", expected.Path, err)
		}
		return fmt.Errorf("open delete target: %w", err)
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect delete target: %w", err)
	}
	opened, err := statxFD(fd)
	if err != nil {
		return fmt.Errorf("inspect delete identity: %w", err)
	}
	if !matchesRemovalEntry(expected, info, opened) {
		return conflictError("permanent delete", expected.Path, nil)
	}
	if !expected.Symlink && expected.Kind == KindFile {
		if _, err := unix.FcntlInt(file.Fd(), unix.F_SETLEASE, unix.F_WRLCK); err != nil {
			return conflictError("permanent delete open file", expected.Path, err)
		}
		if err := requireFileWriteLease(file); err != nil {
			return conflictError("permanent delete", expected.Path, err)
		}
		current, err := file.Stat()
		if err != nil || !sameFingerprint(fingerprintFromInfo(current), expected.Fingerprint()) {
			return conflictError("permanent delete", expected.Path, nil)
		}
	}

	quarantinePath, quarantine, err := createFileMoveQuarantine(parent, expected.Path)
	if err != nil {
		return fmt.Errorf("create delete quarantine: %w", err)
	}
	defer quarantine.Close()
	quarantineName := path.Base(quarantinePath)
	if err := quarantine.Sync(); err != nil {
		return cleanupDeletePreparation(root, parent, quarantineName, quarantine, "", quarantinePath, fmt.Errorf("sync delete quarantine: %w", err))
	}
	if err := parent.Sync(); err != nil {
		return cleanupDeletePreparation(root, parent, quarantineName, quarantine, "", quarantinePath, fmt.Errorf("sync delete quarantine parent: %w", err))
	}
	quarantineStat, err := statxFD(int(quarantine.Fd()))
	if err != nil {
		return cleanupDeletePreparation(root, parent, quarantineName, quarantine, "", quarantinePath, err)
	}
	record := moveRecoveryRecord{
		Version: 1, ID: moveRecoveryID(quarantinePath), Kind: moveRecoveryDelete, Phase: moveDeletePrepared,
		SourcePath: expected.Path, QuarantinePath: quarantinePath,
		SourceIdentity:     moveObjectIdentity{Device: statxDevice(opened), Inode: opened.Ino},
		QuarantineIdentity: moveObjectIdentity{Device: statxDevice(quarantineStat), Inode: quarantineStat.Ino},
	}
	if err := root.writeMoveRecoveryRecord(record); err != nil {
		return cleanupDeletePreparation(root, parent, quarantineName, quarantine, "", quarantinePath, fmt.Errorf("record interrupted delete: %w", err))
	}

	if err := verifyOpenedDirectory(root, parentPath, parent); err != nil {
		return cleanupDeletePreparation(root, parent, quarantineName, quarantine, record.ID, quarantinePath, err)
	}
	if err := verifyRemovalEntryAt(parent, name, opened); err != nil {
		return cleanupDeletePreparation(root, parent, quarantineName, quarantine, record.ID, quarantinePath, conflictError("permanent delete", expected.Path, err))
	}
	if !expected.Symlink && expected.Kind == KindFile {
		if err := requireFileWriteLease(file); err != nil {
			return cleanupDeletePreparation(root, parent, quarantineName, quarantine, record.ID, quarantinePath, conflictError("permanent delete", expected.Path, err))
		}
	}
	if err := renameat2(int(parent.Fd()), name, int(quarantine.Fd()), "source", unix.RENAME_NOREPLACE); err != nil {
		if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EOPNOTSUPP) {
			err = fmt.Errorf("%w: %w", ErrNoReplaceUnsupported, err)
		} else if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.EEXIST) {
			err = conflictError("permanent delete", expected.Path, err)
		}
		return cleanupDeletePreparation(root, parent, quarantineName, quarantine, record.ID, quarantinePath, fmt.Errorf("isolate delete target: %w", err))
	}

	if err := quarantine.Sync(); err != nil {
		return restoreDeleteQuarantine(root, parent, name, quarantineName, quarantine, quarantinePath, record.ID, expected.Path, fmt.Errorf("sync isolated delete target: %w", err))
	}
	if err := parent.Sync(); err != nil {
		return restoreDeleteQuarantine(root, parent, name, quarantineName, quarantine, quarantinePath, record.ID, expected.Path, fmt.Errorf("sync delete source parent: %w", err))
	}
	record.Phase = moveDeleteIsolated
	if err := root.writeMoveRecoveryRecord(record); err != nil {
		return restoreDeleteQuarantine(root, parent, name, quarantineName, quarantine, quarantinePath, record.ID, expected.Path, fmt.Errorf("record isolated delete target: %w", err))
	}
	if err := verifyOpenedDirectory(root, parentPath, parent); err != nil {
		return restoreDeleteQuarantine(root, parent, name, quarantineName, quarantine, quarantinePath, record.ID, expected.Path, err)
	}
	if err := verifyMoveQuarantineDirectory(parent, quarantineName, quarantine); err != nil {
		return restoreDeleteQuarantine(root, parent, name, quarantineName, quarantine, quarantinePath, record.ID, expected.Path, err)
	}
	if err := verifyQuarantinedRemovalAt(quarantine, "source", opened); err != nil {
		return restoreDeleteQuarantine(root, parent, name, quarantineName, quarantine, quarantinePath, record.ID, expected.Path, conflictError("permanent delete", expected.Path, err))
	}
	if expected.Symlink {
		if !matchesRemovalLink(expected, opened, quarantine) {
			return restoreDeleteQuarantine(root, parent, name, quarantineName, quarantine, quarantinePath, record.ID, expected.Path, conflictError("permanent delete", expected.Path, nil))
		}
	} else {
		current, err := file.Stat()
		if err != nil || !sameFingerprintAfterRename(fingerprintFromInfo(current), expected.Fingerprint()) {
			return restoreDeleteQuarantine(root, parent, name, quarantineName, quarantine, quarantinePath, record.ID, expected.Path, conflictError("permanent delete", expected.Path, err))
		}
	}
	if !expected.Symlink && expected.Kind == KindFile {
		if err := requireFileWriteLease(file); err != nil {
			return restoreDeleteQuarantine(root, parent, name, quarantineName, quarantine, quarantinePath, record.ID, expected.Path, conflictError("permanent delete", expected.Path, err))
		}
	}
	if expected.Kind == KindDirectory && !expected.Symlink {
		entries, readErr := file.ReadDir(1)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return restoreDeleteQuarantine(root, parent, name, quarantineName, quarantine, quarantinePath, record.ID, expected.Path, fmt.Errorf("inspect isolated delete directory: %w", readErr))
		}
		if len(entries) != 0 {
			return restoreDeleteQuarantine(root, parent, name, quarantineName, quarantine, quarantinePath, record.ID, expected.Path, conflictError("permanent delete non-empty directory", expected.Path, nil))
		}
	}

	removeFlags := 0
	if !expected.Symlink && expected.Kind == KindDirectory {
		removeFlags = unix.AT_REMOVEDIR
	}
	if err := unix.Unlinkat(int(quarantine.Fd()), "source", removeFlags); err != nil {
		if errors.Is(err, unix.ENOTEMPTY) {
			err = conflictError("permanent delete non-empty directory", expected.Path, err)
		}
		return restoreDeleteQuarantine(root, parent, name, quarantineName, quarantine, quarantinePath, record.ID, expected.Path, fmt.Errorf("remove isolated delete target: %w", err))
	}
	if err := quarantine.Sync(); err != nil {
		return fmt.Errorf("%w: delete completed but quarantine sync failed: %w", ErrPublishedNotDurable, err)
	}
	record.Phase = moveDeleteCommitted
	if err := root.writeMoveRecoveryRecord(record); err != nil {
		return fmt.Errorf("%w: delete completed but recovery phase was not recorded: %w", ErrPublishedNotDurable, err)
	}
	if err := removeEmptyFileMoveQuarantine(parent, quarantineName, quarantine); err != nil {
		return fmt.Errorf("%w: delete completed but quarantine cleanup failed: %w", ErrPublishedNotDurable, err)
	}
	if err := parent.Sync(); err != nil {
		return fmt.Errorf("%w: delete completed but source directory sync failed: %w", ErrPublishedNotDurable, err)
	}
	if err := root.removeMoveRecoveryRecord(record.ID); err != nil {
		return fmt.Errorf("%w: delete completed but recovery record cleanup failed: %w", ErrPublishedNotDurable, err)
	}
	return nil
}

// matchesRemovalLink 确认隔离目录中的软链接仍是原对象。
func matchesRemovalLink(expected Entry, opened unix.Statx_t, quarantine *os.File) bool {
	current, err := statxAt(int(quarantine.Fd()), "source", unix.AT_SYMLINK_NOFOLLOW)
	return err == nil && opened.Mode&unix.S_IFMT == unix.S_IFLNK &&
		expected.LinkIdentityAvailable && expected.LinkDevice == statxDevice(current) &&
		expected.LinkInode == current.Ino && expected.LinkLinks == uint64(current.Nlink)
}

// verifyQuarantinedRemovalAt 比较隔离项与已打开对象。
func verifyQuarantinedRemovalAt(quarantine *os.File, name string, opened unix.Statx_t) error {
	current, err := statxAt(int(quarantine.Fd()), name, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		return err
	}
	if !sameSnapshotObject(opened, current) {
		return ErrPathChanged
	}
	return nil
}

// cleanupDeletePreparation 清理尚未隔离成功的目录和日志。
func cleanupDeletePreparation(root *Root, parent *os.File, quarantineName string, quarantine *os.File, recoveryID, quarantinePath string, cause error) error {
	if err := removeEmptyFileMoveQuarantine(parent, quarantineName, quarantine); err != nil {
		return &EntryRemovalRecoveryError{Path: quarantinePath, Cause: errors.Join(cause, err)}
	}
	if err := parent.Sync(); err != nil {
		return &EntryRemovalRecoveryError{Path: quarantinePath, Cause: errors.Join(cause, err)}
	}
	if recoveryID != "" {
		if err := root.removeMoveRecoveryRecord(recoveryID); err != nil {
			return &EntryRemovalRecoveryError{Path: quarantinePath, Cause: errors.Join(cause, err)}
		}
	}
	return cause
}

// restoreDeleteQuarantine 在原路径空闲时恢复隔离项。
func restoreDeleteQuarantine(root *Root, parent *os.File, sourceName, quarantineName string, quarantine *os.File, quarantinePath, recoveryID, sourcePath string, cause error) error {
	if err := unix.Renameat2(int(quarantine.Fd()), "source", int(parent.Fd()), sourceName, unix.RENAME_NOREPLACE); err != nil {
		return &EntryRemovalRecoveryError{Path: path.Join(quarantinePath, "source"), Cause: errors.Join(cause, fmt.Errorf("restore isolated delete target: %w", err))}
	}
	var restoreErr error
	if err := quarantine.Sync(); err != nil {
		restoreErr = errors.Join(restoreErr, err)
	}
	if err := parent.Sync(); err != nil {
		restoreErr = errors.Join(restoreErr, err)
	}
	if err := removeEmptyFileMoveQuarantine(parent, quarantineName, quarantine); err != nil {
		restoreErr = errors.Join(restoreErr, err)
	}
	if err := parent.Sync(); err != nil {
		restoreErr = errors.Join(restoreErr, err)
	}
	if restoreErr == nil {
		if err := root.removeMoveRecoveryRecord(recoveryID); err != nil {
			restoreErr = errors.Join(restoreErr, err)
		}
	}
	if restoreErr != nil {
		return &EntryRemovalRecoveryError{Path: sourcePath, Cause: errors.Join(cause, restoreErr)}
	}
	return cause
}

// matchesRemovalEntry 比较打开对象与审批快照。
func matchesRemovalEntry(expected Entry, info os.FileInfo, opened unix.Statx_t) bool {
	if expected.Symlink {
		return opened.Mode&unix.S_IFMT == unix.S_IFLNK &&
			expected.LinkIdentityAvailable &&
			expected.LinkDevice == statxDevice(opened) &&
			expected.LinkInode == opened.Ino &&
			expected.LinkLinks == uint64(opened.Nlink)
	}
	if expected.Kind == KindDirectory && opened.Mode&unix.S_IFMT != unix.S_IFDIR {
		return false
	}
	if expected.Kind == KindFile && opened.Mode&unix.S_IFMT != unix.S_IFREG {
		return false
	}
	return sameFingerprint(fingerprintFromInfo(info), expected.Fingerprint())
}

func verifyRemovalEntryAt(parent *os.File, name string, opened unix.Statx_t) error {
	current, err := statxAt(int(parent.Fd()), name, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		return err
	}
	if !sameSnapshotStat(opened, current) {
		return ErrPathChanged
	}
	return nil
}
