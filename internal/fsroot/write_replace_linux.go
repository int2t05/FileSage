//go:build linux

// 本文件以读租约和原子交换保护已有文件的条件替换。
package fsroot

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"

	"golang.org/x/sys/unix"
)

// platformReplaceAtomicIfUnchanged 交换后复核旧版本，冲突时尽量原子恢复。
func platformReplaceAtomicIfUnchanged(root *Root, target, temporary string, expected Fingerprint, expectedDigest, candidateDigest [32]byte, candidateSize int64, perm fs.FileMode) error {
	if root == nil || root.root == nil || parentPathForOperation(target) != parentPathForOperation(temporary) {
		return ErrInvalidPath
	}
	parent, temporaryName, err := openRenameParent(root, temporary)
	if err != nil {
		return err
	}
	defer parent.Close()
	if err := verifyOpenedDirectory(root, parentPathForOperation(target), parent); err != nil {
		return err
	}
	targetName := path.Base(target)

	candidate, err := openRegularAt(int(parent.Fd()), temporaryName)
	if err != nil {
		return recoveryWriteError(temporary, err)
	}
	defer candidate.Close()
	candidateInfo, err := candidate.Stat()
	if err != nil || candidateInfo.Size() != candidateSize || candidateInfo.Mode().Perm() != perm.Perm() {
		return recoveryWriteError(temporary, conflictError("inspect staged write", target, err))
	}
	candidateFingerprint := fingerprintFromInfo(candidateInfo)
	if digest, err := stableFileDigest(candidate, candidateFingerprint, candidateSize, false); err != nil || digest != candidateDigest {
		return recoveryWriteError(temporary, conflictError("verify staged write", target, err))
	}
	stagedPathInfo, err := root.root.Lstat(temporary)
	if err != nil || !os.SameFile(candidateInfo, stagedPathInfo) {
		return recoveryWriteError(temporary, conflictError("inspect staged write", target, err))
	}
	original, err := openRegularAt(int(parent.Fd()), targetName)
	if err != nil {
		return conflictError("replace", target, err)
	}
	defer original.Close()
	originalInfo, err := original.Stat()
	if err != nil || !sameFingerprint(fingerprintFromInfo(originalInfo), expected) {
		return conflictError("replace", target, err)
	}
	if err := verifyOpenedFileEntry(int(parent.Fd()), targetName, original); err != nil {
		return conflictError("replace", target, err)
	}
	digest, err := stableFileDigest(original, expected, expected.Size, false)
	if err != nil || digest != expectedDigest {
		return conflictError("replace", target, err)
	}
	if _, err := unix.FcntlInt(original.Fd(), unix.F_SETLEASE, unix.F_RDLCK); err != nil {
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
			return conflictError("replace", target, err)
		}
		return fmt.Errorf("protect write source %q: %w: %w", target, ErrAtomicWriteUnsupported, err)
	}
	if err := requireReplacementReadLease(original); err != nil {
		return conflictError("replace", target, err)
	}
	if err := verifyOpenedFileEntry(int(parent.Fd()), targetName, original); err != nil {
		return conflictError("replace", target, err)
	}

	if err := unix.Renameat2(int(parent.Fd()), temporaryName, int(parent.Fd()), targetName, unix.RENAME_EXCHANGE); err != nil {
		if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EOPNOTSUPP) {
			return fmt.Errorf("exchange write target: %w", ErrAtomicWriteUnsupported)
		}
		if errors.Is(err, unix.ENOENT) {
			return conflictError("replace", target, err)
		}
		return fmt.Errorf("exchange write target %q: %w", target, err)
	}

	targetInfo, targetErr := root.root.Lstat(target)
	if targetErr != nil || !os.SameFile(candidateInfo, targetInfo) {
		return recoveryWriteError(temporary, conflictError("verify published write", target, targetErr))
	}
	if err := requireReplacementReadLease(original); err != nil {
		return rollbackAtomicExchange(root, parent, target, temporary, targetName, temporaryName, candidateInfo, err)
	}
	if digest, err := stableFileDigest(candidate, candidateFingerprint, candidateSize, true); err != nil || digest != candidateDigest {
		return recoveryWriteError(temporary, conflictError("verify published write", target, err))
	}
	if err := requireReplacementReadLease(original); err != nil {
		return rollbackAtomicExchange(root, parent, target, temporary, targetName, temporaryName, candidateInfo, err)
	}
	originalInfo, err = original.Stat()
	if err != nil || !sameFingerprintAfterRename(fingerprintFromInfo(originalInfo), expected) {
		return rollbackAtomicExchange(root, parent, target, temporary, targetName, temporaryName, candidateInfo, conflictError("verify replaced source", target, err))
	}
	if digest, err := stableFileDigest(original, expected, expected.Size, true); err != nil || digest != expectedDigest {
		return rollbackAtomicExchange(root, parent, target, temporary, targetName, temporaryName, candidateInfo, conflictError("verify replaced source", target, err))
	}
	if err := verifyOpenedFileEntry(int(parent.Fd()), temporaryName, original); err != nil {
		return rollbackAtomicExchange(root, parent, target, temporary, targetName, temporaryName, candidateInfo, conflictError("verify replaced source", target, err))
	}
	if err := requireReplacementReadLease(original); err != nil {
		return rollbackAtomicExchange(root, parent, target, temporary, targetName, temporaryName, candidateInfo, err)
	}

	if err := unix.Unlinkat(int(parent.Fd()), temporaryName, 0); err != nil {
		return recoveryWriteError(temporary, fmt.Errorf("%w: remove replaced source: %w", ErrPublishedNotDurable, err))
	}
	if err := parent.Sync(); err != nil {
		return fmt.Errorf("%w: %w", ErrPublishedNotDurable, err)
	}
	return nil
}

// verifyOpenedFileEntry 确认打开文件仍是目录中的目标项。
func verifyOpenedFileEntry(parentFD int, name string, file *os.File) error {
	opened, err := statxFD(int(file.Fd()))
	if err != nil {
		return err
	}
	current, err := statxAt(parentFD, name, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		return err
	}
	if !sameSnapshotObject(opened, current) {
		return ErrConflict
	}
	return nil
}

// requireReplacementReadLease 确保外部进程未打开旧文件写入。
func requireReplacementReadLease(file *os.File) error {
	lease, err := unix.FcntlInt(file.Fd(), unix.F_GETLEASE, 0)
	if err != nil {
		return err
	}
	if lease != unix.F_RDLCK {
		return ErrConflict
	}
	return nil
}

func rollbackAtomicExchange(root *Root, parent *os.File, target, temporary, targetName, temporaryName string, candidate fs.FileInfo, cause error) error {
	currentTarget, err := root.root.Lstat(target)
	if err != nil || !os.SameFile(candidate, currentTarget) {
		if err == nil {
			err = ErrConflict
		}
		return recoveryWriteError(temporary, errors.Join(ErrConflict, cause, fmt.Errorf("target changed before rollback: %w", err)))
	}
	if err := unix.Renameat2(int(parent.Fd()), temporaryName, int(parent.Fd()), targetName, unix.RENAME_EXCHANGE); err != nil {
		return recoveryWriteError(temporary, errors.Join(ErrConflict, cause, fmt.Errorf("rollback exchange: %w", err)))
	}
	if err := parent.Sync(); err != nil {
		return recoveryWriteError(temporary, errors.Join(ErrConflict, cause, fmt.Errorf("sync rollback: %w", err)))
	}
	return conflictError("replace", target, cause)
}

func recoveryWriteError(path string, cause error) error {
	return &WriteRecoveryError{Path: path, Cause: errors.Join(ErrConflict, cause)}
}

func openRegularAt(parentFD int, name string) (*os.File, error) {
	fd, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, ErrNotRegularFile
	}
	return file, nil
}

func stableFileDigest(file *os.File, expected Fingerprint, size int64, ignoreChangeTime bool) ([32]byte, error) {
	before, err := file.Stat()
	if err != nil {
		return [32]byte{}, err
	}
	beforeFingerprint := fingerprintFromInfo(before)
	if ignoreChangeTime {
		if !sameFingerprintAfterRename(beforeFingerprint, expected) {
			return [32]byte{}, ErrConflict
		}
	} else if !sameFingerprint(beforeFingerprint, expected) {
		return [32]byte{}, ErrConflict
	}
	if size < 0 || before.Size() != size {
		return [32]byte{}, ErrSizeMismatch
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return [32]byte{}, err
	}
	hash := sha256.New()
	if _, err := io.CopyN(hash, file, size); err != nil {
		return [32]byte{}, err
	}
	after, err := file.Stat()
	if err != nil {
		return [32]byte{}, err
	}
	afterFingerprint := fingerprintFromInfo(after)
	if ignoreChangeTime {
		if !sameFingerprintAfterRename(afterFingerprint, expected) {
			return [32]byte{}, ErrConflict
		}
	} else if !sameFingerprint(afterFingerprint, expected) {
		return [32]byte{}, ErrConflict
	}
	var result [32]byte
	copy(result[:], hash.Sum(nil))
	return result, nil
}
