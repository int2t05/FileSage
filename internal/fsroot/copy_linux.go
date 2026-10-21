//go:build linux

// 本文件使用匿名暂存文件安全发布单文件副本。
package fsroot

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strconv"

	"golang.org/x/sys/unix"
)

// platformCopyFileNoReplace 校验来源、复制内容并以禁止覆盖方式发布。
func platformCopyFileNoReplace(ctx context.Context, root *Root, source, destination string, expected Fingerprint, expectedSHA256 string) (FileCopyResult, error) {
	sourceParent, sourceName, err := openRenameParent(root, source)
	if err != nil {
		return FileCopyResult{}, fmt.Errorf("open source parent: %w", err)
	}
	defer sourceParent.Close()
	if err := verifyOpenedDirectory(root, parentPathForOperation(source), sourceParent); err != nil {
		return FileCopyResult{}, err
	}

	sourceFD, err := unix.Openat(int(sourceParent.Fd()), sourceName, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return FileCopyResult{}, ErrNotRegularFile
		}
		return FileCopyResult{}, fmt.Errorf("open copy source: %w", err)
	}
	sourceFile := os.NewFile(uintptr(sourceFD), sourceName)
	defer sourceFile.Close()
	sourceInfo, err := sourceFile.Stat()
	if err != nil {
		return FileCopyResult{}, fmt.Errorf("inspect opened copy source: %w", err)
	}
	if !sourceInfo.Mode().IsRegular() {
		return FileCopyResult{}, ErrNotRegularFile
	}
	if !sameFingerprint(fingerprintFromInfo(sourceInfo), expected) {
		return FileCopyResult{}, conflictError("copy", source, nil)
	}

	destinationParentPath := parentPathForOperation(destination)
	destinationParent, err := openRootDirectoryNoFollow(root.root, destinationParentPath)
	if err != nil {
		return FileCopyResult{}, fmt.Errorf("open copy destination directory: %w", err)
	}
	defer destinationParent.Close()
	if err := verifyOpenedDirectory(root, destinationParentPath, destinationParent); err != nil {
		return FileCopyResult{}, err
	}
	var existing unix.Stat_t
	if err := unix.Fstatat(int(destinationParent.Fd()), path.Base(destination), &existing, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		return FileCopyResult{}, conflictError("copy", destination, fs.ErrExist)
	} else if !errors.Is(err, unix.ENOENT) {
		return FileCopyResult{}, fmt.Errorf("inspect copy destination: %w", err)
	}

	temporaryFD, err := unix.Openat(int(destinationParent.Fd()), ".", unix.O_TMPFILE|unix.O_RDWR|unix.O_CLOEXEC, 0o600)
	if err != nil {
		if isAnonymousFileUnsupported(err) {
			return FileCopyResult{}, fmt.Errorf("anonymous file staging: %w", ErrAtomicWriteUnsupported)
		}
		return FileCopyResult{}, fmt.Errorf("create anonymous copy file: %w", err)
	}
	temporary := os.NewFile(uintptr(temporaryFD), "filesage-anonymous-copy")
	defer temporary.Close()

	if err := ctx.Err(); err != nil {
		return FileCopyResult{}, err
	}
	sourceDigest := sha256.New()
	written, err := io.CopyN(io.MultiWriter(temporary, sourceDigest), contextReader{ctx: ctx, reader: sourceFile}, expected.Size)
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return FileCopyResult{}, fmt.Errorf("copy source ended early: %w", ErrSizeMismatch)
		}
		return FileCopyResult{}, fmt.Errorf("copy source data: %w", err)
	}
	if written != expected.Size {
		return FileCopyResult{}, ErrSizeMismatch
	}
	if err := requireStreamEOF(contextReader{ctx: ctx, reader: sourceFile}); err != nil {
		return FileCopyResult{}, fmt.Errorf("validate copy source length: %w", err)
	}
	if err := temporary.Chmod(expected.Mode.Perm()); err != nil {
		return FileCopyResult{}, fmt.Errorf("preserve copied file permissions: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return FileCopyResult{}, fmt.Errorf("sync copied file: %w", err)
	}

	if _, err := temporary.Seek(0, io.SeekStart); err != nil {
		return FileCopyResult{}, fmt.Errorf("rewind copied file for verification: %w", err)
	}
	copyDigest := sha256.New()
	verifiedBytes, err := io.Copy(copyDigest, contextReader{ctx: ctx, reader: temporary})
	if err != nil {
		return FileCopyResult{}, fmt.Errorf("verify copied file: %w", err)
	}
	if verifiedBytes != expected.Size || subtle.ConstantTimeCompare(sourceDigest.Sum(nil), copyDigest.Sum(nil)) != 1 {
		return FileCopyResult{}, fmt.Errorf("copied content verification failed: %w", ErrConflict)
	}
	if expectedSHA256 != "" {
		expectedDigest, err := hex.DecodeString(expectedSHA256)
		if err != nil || len(expectedDigest) != sha256.Size || subtle.ConstantTimeCompare(sourceDigest.Sum(nil), expectedDigest) != 1 {
			return FileCopyResult{}, conflictError("copy", source, nil)
		}
	}
	if err := ctx.Err(); err != nil {
		return FileCopyResult{}, err
	}
	if !sameFingerprint(fingerprintFromInfoAfterRead(sourceFile), expected) {
		return FileCopyResult{}, conflictError("copy", source, nil)
	}
	if err := root.checkRegularFileFingerprint(source, expected); err != nil {
		return FileCopyResult{}, err
	}
	if err := verifyOpenedDirectory(root, destinationParentPath, destinationParent); err != nil {
		return FileCopyResult{}, err
	}

	procPath := "/proc/self/fd/" + strconv.FormatUint(uint64(temporary.Fd()), 10)
	if err := unix.Linkat(unix.AT_FDCWD, procPath, int(destinationParent.Fd()), path.Base(destination), unix.AT_SYMLINK_FOLLOW); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return FileCopyResult{}, conflictError("copy", destination, fs.ErrExist)
		}
		if isAnonymousFileUnsupported(err) {
			return FileCopyResult{}, fmt.Errorf("publish anonymous copy file: %w", ErrAtomicWriteUnsupported)
		}
		return FileCopyResult{}, fmt.Errorf("publish copy destination: %w", err)
	}
	result := FileCopyResult{SHA256: hex.EncodeToString(sourceDigest.Sum(nil)), Published: true}
	if err := destinationParent.Sync(); err != nil {
		return result, fmt.Errorf("%w: %w", ErrPublishedNotDurable, err)
	}
	return result, nil
}

// platformMoveFileSameMountNoReplace 同卷移动后复核目录项，发现竞态则尝试恢复来源。
func platformMoveFileSameMountNoReplace(ctx context.Context, root *Root, source, destination string, expected Fingerprint, expectedSHA256 string) (FileMoveResult, error) {
	sourceParent, sourceName, err := openRenameParent(root, source)
	if err != nil {
		return FileMoveResult{}, fmt.Errorf("open move source parent: %w", err)
	}
	defer sourceParent.Close()
	destinationParent, destinationName, err := openRenameParent(root, destination)
	if err != nil {
		return FileMoveResult{}, fmt.Errorf("open move destination parent: %w", err)
	}
	defer destinationParent.Close()
	sourceParentPath := parentPathForOperation(source)
	destinationParentPath := parentPathForOperation(destination)
	if err := verifyOpenedDirectory(root, sourceParentPath, sourceParent); err != nil {
		return FileMoveResult{}, err
	}
	if err := verifyOpenedDirectory(root, destinationParentPath, destinationParent); err != nil {
		return FileMoveResult{}, err
	}

	sourceFD, err := unix.Openat(int(sourceParent.Fd()), sourceName, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return FileMoveResult{}, ErrNotRegularFile
		}
		return FileMoveResult{}, fmt.Errorf("open move source: %w", err)
	}
	sourceFile := os.NewFile(uintptr(sourceFD), sourceName)
	defer sourceFile.Close()
	sourceInfo, err := sourceFile.Stat()
	if err != nil {
		return FileMoveResult{}, fmt.Errorf("inspect move source: %w", err)
	}
	if !sourceInfo.Mode().IsRegular() || !sameFingerprint(fingerprintFromInfo(sourceInfo), expected) {
		return FileMoveResult{}, conflictError("move", source, nil)
	}
	if err := ctx.Err(); err != nil {
		return FileMoveResult{}, err
	}
	if err := verifyOpenedDirectory(root, sourceParentPath, sourceParent); err != nil {
		return FileMoveResult{}, err
	}
	if err := verifyOpenedDirectory(root, destinationParentPath, destinationParent); err != nil {
		return FileMoveResult{}, err
	}
	if err := unix.Renameat2(int(sourceParent.Fd()), sourceName, int(destinationParent.Fd()), destinationName, unix.RENAME_NOREPLACE); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return FileMoveResult{}, conflictError("move", destination, fs.ErrExist)
		}
		if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EOPNOTSUPP) {
			return FileMoveResult{}, fmt.Errorf("%w: %w", ErrNoReplaceUnsupported, err)
		}
		return FileMoveResult{}, fmt.Errorf("move file: %w", err)
	}
	result := FileMoveResult{Published: true, SourceRemoved: true}

	moveFlags := unix.O_PATH | unix.O_NOFOLLOW | unix.O_CLOEXEC
	if expectedSHA256 != "" {
		moveFlags = unix.O_RDONLY | unix.O_NONBLOCK | unix.O_NOFOLLOW | unix.O_CLOEXEC
	}
	movedFD, err := unix.Openat(int(destinationParent.Fd()), destinationName, moveFlags, 0)
	if err != nil {
		return restoreSameMountFileMove(root, source, destination, sourceParentPath, destinationParentPath, sourceParent, sourceName, destinationParent, destinationName, nil, fmt.Errorf("inspect moved destination: %w", err))
	}
	movedFile := os.NewFile(uintptr(movedFD), destinationName)
	defer movedFile.Close()
	movedInfo, err := movedFile.Stat()
	if err != nil {
		return restoreSameMountFileMove(root, source, destination, sourceParentPath, destinationParentPath, sourceParent, sourceName, destinationParent, destinationName, movedFile, fmt.Errorf("inspect moved destination: %w", err))
	}
	if !movedInfo.Mode().IsRegular() || !sameFingerprintAfterRename(fingerprintFromInfo(movedInfo), expected) {
		return restoreSameMountFileMove(root, source, destination, sourceParentPath, destinationParentPath, sourceParent, sourceName, destinationParent, destinationName, movedFile, conflictError("move", source, nil))
	}
	if expectedSHA256 != "" {
		if err := verifyMovedFileSHA256(ctx, movedFile, fingerprintFromInfo(movedInfo), expected.Size, expectedSHA256); err != nil {
			return restoreSameMountFileMove(root, source, destination, sourceParentPath, destinationParentPath, sourceParent, sourceName, destinationParent, destinationName, movedFile, conflictError("move content", source, err))
		}
	}
	if err := verifyOpenedDirectory(root, sourceParentPath, sourceParent); err != nil {
		return restoreSameMountFileMove(root, source, destination, sourceParentPath, destinationParentPath, sourceParent, sourceName, destinationParent, destinationName, movedFile, err)
	}
	if err := verifyOpenedDirectory(root, destinationParentPath, destinationParent); err != nil {
		return restoreSameMountFileMove(root, source, destination, sourceParentPath, destinationParentPath, sourceParent, sourceName, destinationParent, destinationName, movedFile, err)
	}
	if err := syncMoveParents(root, source, destination); err != nil {
		return result, fmt.Errorf("%w: %w", ErrPublishedNotDurable, err)
	}
	return result, nil
}

// verifyMovedFileSHA256 核对同卷移动项的稳定状态和内容摘要。
func verifyMovedFileSHA256(ctx context.Context, file *os.File, expectedState Fingerprint, size int64, expectedSHA256 string) error {
	expectedDigest, err := hex.DecodeString(expectedSHA256)
	if err != nil || len(expectedDigest) != sha256.Size {
		return fs.ErrInvalid
	}
	before, err := file.Stat()
	if err != nil {
		return err
	}
	beforeState := fingerprintFromInfo(before)
	if !sameFingerprint(beforeState, expectedState) || size < 0 || before.Size() != size {
		return ErrConflict
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	digest := sha256.New()
	count, err := io.CopyN(digest, contextReader{ctx: ctx, reader: file}, size)
	if err != nil {
		return err
	}
	if count != size {
		return ErrSizeMismatch
	}
	after, err := file.Stat()
	if err != nil {
		return err
	}
	if !sameFingerprint(fingerprintFromInfo(after), beforeState) || subtle.ConstantTimeCompare(digest.Sum(nil), expectedDigest) != 1 {
		return ErrConflict
	}
	return nil
}

// restoreSameMountFileMove 在提交版本不符时无覆盖地恢复移动项。
func restoreSameMountFileMove(root *Root, source, destination, sourceParentPath, destinationParentPath string, sourceParent *os.File, sourceName string, destinationParent *os.File, destinationName string, movedFile *os.File, cause error) (FileMoveResult, error) {
	retained := func(err error) (FileMoveResult, error) {
		return FileMoveResult{Published: true, SourceRemoved: true, SourceRetained: destination}, errors.Join(cause, fmt.Errorf("restore moved source: %w", err))
	}
	if movedFile == nil {
		return retained(ErrPathChanged)
	}
	if err := verifyOpenedDirectory(root, sourceParentPath, sourceParent); err != nil {
		return retained(err)
	}
	if err := verifyOpenedDirectory(root, destinationParentPath, destinationParent); err != nil {
		return retained(err)
	}
	currentFD, err := unix.Openat(int(destinationParent.Fd()), destinationName, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return retained(err)
	}
	current := os.NewFile(uintptr(currentFD), destinationName)
	defer current.Close()
	movedInfo, err := movedFile.Stat()
	if err != nil {
		return retained(err)
	}
	currentInfo, err := current.Stat()
	if err != nil {
		return retained(err)
	}
	if !os.SameFile(movedInfo, currentInfo) {
		return retained(ErrPathChanged)
	}
	if err := unix.Renameat2(int(destinationParent.Fd()), destinationName, int(sourceParent.Fd()), sourceName, unix.RENAME_NOREPLACE); err != nil {
		return retained(err)
	}
	if err := syncMoveParents(root, source, destination); err != nil {
		return FileMoveResult{}, errors.Join(cause, fmt.Errorf("%w: restore source: %w", ErrPublishedNotDurable, err))
	}
	return FileMoveResult{}, cause
}

// platformRemoveFileIfUnchanged 隔离来源后复核身份与内容，再删除副本。
func platformRemoveFileIfUnchanged(ctx context.Context, root *Root, relativePath, destination string, expected Fingerprint, expectedSHA256 string) (bool, string, error) {
	parent, name, err := openRenameParent(root, relativePath)
	if err != nil {
		return false, "", fmt.Errorf("open source parent before removal: %w", err)
	}
	defer parent.Close()
	parentPath := parentPathForOperation(relativePath)
	if err := verifyOpenedDirectory(root, parentPath, parent); err != nil {
		return false, "", err
	}
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return false, "", ErrNotRegularFile
		}
		return false, "", fmt.Errorf("inspect source before removal: %w", err)
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return false, "", fmt.Errorf("stat source before removal: %w", err)
	}
	if !info.Mode().IsRegular() {
		return false, "", ErrNotRegularFile
	}
	if !sameFingerprint(fingerprintFromInfo(info), expected) {
		return false, "", conflictError("move", relativePath, nil)
	}
	destinationEntry, err := root.LstatEntry(destination)
	if err != nil {
		return false, "", fmt.Errorf("inspect published move target: %w", err)
	}
	if destinationEntry.Kind != KindFile || destinationEntry.Symlink {
		return false, "", conflictError("move", destination, nil)
	}
	if !destinationEntry.IdentityAvailable {
		return false, "", ErrIdentityUnavailable
	}
	if err := ctx.Err(); err != nil {
		return false, "", err
	}
	quarantinePath, quarantine, err := createFileMoveQuarantine(parent, relativePath)
	if err != nil {
		return false, "", fmt.Errorf("create source quarantine: %w", err)
	}
	defer quarantine.Close()
	quarantineName := path.Base(quarantinePath)
	if err := ctx.Err(); err != nil {
		return false, "", errors.Join(err, removeEmptyFileMoveQuarantine(parent, quarantineName, quarantine))
	}
	quarantineStat, err := statxFD(int(quarantine.Fd()))
	if err != nil {
		return false, "", errors.Join(err, removeEmptyFileMoveQuarantine(parent, quarantineName, quarantine))
	}
	record := moveRecoveryRecord{
		Version: 1, ID: moveRecoveryID(quarantinePath), Kind: moveRecoveryFile,
		SourcePath: relativePath, QuarantinePath: quarantinePath,
		DestinationPath:        destination,
		SourceIdentity:         moveObjectIdentity{Device: expected.Device, Inode: expected.Inode},
		QuarantineIdentity:     moveObjectIdentity{Device: statxDevice(quarantineStat), Inode: quarantineStat.Ino},
		DestinationIdentity:    moveObjectIdentity{Device: destinationEntry.Device, Inode: destinationEntry.Inode},
		DestinationFingerprint: destinationEntry.Fingerprint(), DestinationDigest: expectedSHA256,
	}
	if err := root.writeMoveRecoveryRecord(record); err != nil {
		cleanupErr := removeEmptyFileMoveQuarantine(parent, quarantineName, quarantine)
		return false, "", errors.Join(fmt.Errorf("record interrupted file move: %w", err), cleanupErr)
	}
	if err := ctx.Err(); err != nil {
		cleanupErr := removeEmptyFileMoveQuarantine(parent, quarantineName, quarantine)
		journalErr := root.removeMoveRecoveryRecord(record.ID)
		return false, "", errors.Join(err, cleanupErr, journalErr)
	}
	if err := unix.Renameat2(int(parent.Fd()), name, int(quarantine.Fd()), "source", unix.RENAME_NOREPLACE); err != nil {
		cleanupErr := removeEmptyFileMoveQuarantine(parent, quarantineName, quarantine)
		journalErr := root.removeMoveRecoveryRecord(record.ID)
		if errors.Is(err, unix.EEXIST) {
			err = conflictError("move", relativePath, fs.ErrExist)
		}
		return false, "", errors.Join(fmt.Errorf("isolate moved source: %w", err), cleanupErr, journalErr)
	}
	if err := quarantine.Sync(); err != nil {
		return restoreFileMoveQuarantine(root, parent, name, quarantineName, quarantine, quarantinePath, record.ID, fmt.Errorf("sync isolated source: %w", err))
	}
	if err := parent.Sync(); err != nil {
		return restoreFileMoveQuarantine(root, parent, name, quarantineName, quarantine, quarantinePath, record.ID, fmt.Errorf("sync source parent after isolation: %w", err))
	}
	if err := verifyOpenedDirectory(root, parentPath, parent); err != nil {
		return restoreFileMoveQuarantine(root, parent, name, quarantineName, quarantine, quarantinePath, record.ID, err)
	}
	if err := ctx.Err(); err != nil {
		return restoreFileMoveQuarantine(root, parent, name, quarantineName, quarantine, quarantinePath, record.ID, err)
	}
	if err := verifyMoveQuarantineDirectory(parent, quarantineName, quarantine); err != nil {
		return restoreFileMoveQuarantine(root, parent, name, quarantineName, quarantine, quarantinePath, record.ID, err)
	}
	if err := verifyQuarantinedMoveFile(ctx, quarantine, quarantinePath, expected, expectedSHA256); err != nil {
		return restoreFileMoveQuarantine(root, parent, name, quarantineName, quarantine, quarantinePath, record.ID, err)
	}
	if err := ctx.Err(); err != nil {
		return restoreFileMoveQuarantine(root, parent, name, quarantineName, quarantine, quarantinePath, record.ID, err)
	}
	if err := verifyOpenedDirectory(root, parentPath, parent); err != nil {
		return restoreFileMoveQuarantine(root, parent, name, quarantineName, quarantine, quarantinePath, record.ID, err)
	}
	if err := verifyMoveQuarantineDirectory(parent, quarantineName, quarantine); err != nil {
		return restoreFileMoveQuarantine(root, parent, name, quarantineName, quarantine, quarantinePath, record.ID, err)
	}
	if err := unix.Unlinkat(int(quarantine.Fd()), "source", 0); err != nil {
		return restoreFileMoveQuarantine(root, parent, name, quarantineName, quarantine, quarantinePath, record.ID, fmt.Errorf("remove isolated source: %w", err))
	}
	var durabilityErr error
	if err := quarantine.Sync(); err != nil {
		durabilityErr = errors.Join(durabilityErr, err)
	}
	if err := removeEmptyFileMoveQuarantine(parent, quarantineName, quarantine); err != nil {
		durabilityErr = errors.Join(durabilityErr, err)
	}
	if err := parent.Sync(); err != nil {
		durabilityErr = errors.Join(durabilityErr, err)
	}
	if durabilityErr != nil {
		return true, "", fmt.Errorf("%w: source was removed but quarantine cleanup was not confirmed: %w", ErrPublishedNotDurable, durabilityErr)
	}
	if err := root.removeMoveRecoveryRecord(record.ID); err != nil {
		return true, "", fmt.Errorf("%w: clear completed file move record: %w", ErrPublishedNotDurable, err)
	}
	return true, "", nil
}

// createFileMoveQuarantine 建立只允许服务用户访问的同卷临时目录。
func createFileMoveQuarantine(parent *os.File, source string) (string, *os.File, error) {
	for range 8 {
		quarantinePath, err := createMoveQuarantinePath(source)
		if err != nil {
			return "", nil, err
		}
		name := path.Base(quarantinePath)
		if err := unix.Mkdirat(int(parent.Fd()), name, 0o700); err != nil {
			if errors.Is(err, unix.EEXIST) {
				continue
			}
			return "", nil, err
		}
		fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			_ = unix.Unlinkat(int(parent.Fd()), name, unix.AT_REMOVEDIR)
			return "", nil, err
		}
		directory := os.NewFile(uintptr(fd), name)
		if err := unix.Fchmod(fd, 0o700); err != nil {
			_ = unix.Unlinkat(int(parent.Fd()), name, unix.AT_REMOVEDIR)
			_ = directory.Close()
			return "", nil, err
		}
		if err := verifyMoveQuarantineDirectory(parent, name, directory); err != nil {
			_ = unix.Unlinkat(int(parent.Fd()), name, unix.AT_REMOVEDIR)
			_ = directory.Close()
			return "", nil, err
		}
		return quarantinePath, directory, nil
	}
	return "", nil, fs.ErrExist
}

// verifyMoveQuarantineDirectory 确认临时目录仍是本服务独占的真实目录。
func verifyMoveQuarantineDirectory(parent *os.File, name string, directory *os.File) error {
	opened, err := statxFD(int(directory.Fd()))
	if err != nil {
		return err
	}
	current, err := statxAt(int(parent.Fd()), name, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		return err
	}
	if opened.Mode&unix.S_IFMT != unix.S_IFDIR || opened.Mode&0o777 != 0o700 || opened.Uid != uint32(unix.Geteuid()) || !sameSnapshotObject(opened, current) {
		return ErrPathChanged
	}
	return nil
}

// verifyQuarantinedMoveFile 核验隔离文件身份、大小、元数据和复制摘要。
func verifyQuarantinedMoveFile(ctx context.Context, quarantine *os.File, quarantinePath string, expected Fingerprint, expectedSHA256 string) error {
	fd, err := unix.Openat(int(quarantine.Fd()), "source", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return ErrNotRegularFile
		}
		return err
	}
	file := os.NewFile(uintptr(fd), "source")
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || !sameFingerprintAfterRename(fingerprintFromInfo(info), expected) {
		return conflictError("move", quarantinePath, nil)
	}
	digest, err := hex.DecodeString(expectedSHA256)
	if err != nil || len(digest) != sha256.Size {
		return ErrConflict
	}
	actualDigest := sha256.New()
	count, err := io.Copy(actualDigest, contextReader{ctx: ctx, reader: file})
	if err != nil {
		return err
	}
	if count != expected.Size || !sameFingerprintAfterRename(fingerprintFromInfoAfterRead(file), expected) || subtle.ConstantTimeCompare(actualDigest.Sum(nil), digest) != 1 {
		return conflictError("move", quarantinePath, nil)
	}
	return nil
}

// restoreFileMoveQuarantine 恢复来源；无法恢复时返回实际保留路径。
func restoreFileMoveQuarantine(root *Root, parent *os.File, sourceName, quarantineName string, quarantine *os.File, quarantinePath, recoveryID string, cause error) (bool, string, error) {
	if err := unix.Renameat2(int(quarantine.Fd()), "source", int(parent.Fd()), sourceName, unix.RENAME_NOREPLACE); err != nil {
		return false, path.Join(quarantinePath, "source"), errors.Join(cause, fmt.Errorf("restore isolated source: %w", err))
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
	if restoreErr == nil {
		if err := root.removeMoveRecoveryRecord(recoveryID); err != nil {
			restoreErr = errors.Join(restoreErr, err)
		}
	}
	return false, "", errors.Join(cause, restoreErr)
}

// removeEmptyFileMoveQuarantine 只移除身份仍匹配的空临时目录。
func removeEmptyFileMoveQuarantine(parent *os.File, name string, directory *os.File) error {
	if err := verifyMoveQuarantineDirectory(parent, name, directory); err != nil {
		return err
	}
	return unix.Unlinkat(int(parent.Fd()), name, unix.AT_REMOVEDIR)
}

// verifyOpenedDirectory 确认已打开目录仍对应管理根内的原路径。
func verifyOpenedDirectory(root *Root, relativePath string, opened *os.File) error {
	openedInfo, err := opened.Stat()
	if err != nil {
		return fmt.Errorf("inspect opened directory: %w", err)
	}
	if !openedInfo.IsDir() {
		return ErrNotDirectory
	}
	currentPath := relativePath
	if currentPath == "" {
		currentPath = "."
	}
	currentInfo, err := root.root.Stat(currentPath)
	if err != nil || !os.SameFile(openedInfo, currentInfo) {
		return ErrPathChanged
	}
	return nil
}

// parentPathForOperation 将根目录规范化为空相对路径。
func parentPathForOperation(relativePath string) string {
	parent := path.Dir(relativePath)
	if parent == "." {
		return ""
	}
	return parent
}

// fingerprintFromInfoAfterRead 读取文件描述符的最终状态。
func fingerprintFromInfoAfterRead(file *os.File) Fingerprint {
	info, err := file.Stat()
	if err != nil {
		return Fingerprint{}
	}
	return fingerprintFromInfo(info)
}

// isAnonymousFileUnsupported 判断文件系统是否缺少匿名暂存能力。
func isAnonymousFileUnsupported(err error) bool {
	return errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.ENOENT)
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

// Read 在每次读取前响应请求取消。
func (reader contextReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(buffer)
}
