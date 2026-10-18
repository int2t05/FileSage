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
func platformCopyFileNoReplace(ctx context.Context, root *Root, source, destination string, expected Fingerprint) (FileCopyResult, error) {
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

// platformRemoveFileIfUnchanged 仅在来源快照仍匹配时删除目录项。
func platformRemoveFileIfUnchanged(root *Root, relativePath string, expected Fingerprint) (bool, error) {
	parent, name, err := openRenameParent(root, relativePath)
	if err != nil {
		return false, fmt.Errorf("open source parent before removal: %w", err)
	}
	defer parent.Close()
	if err := verifyOpenedDirectory(root, parentPathForOperation(relativePath), parent); err != nil {
		return false, err
	}
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return false, ErrNotRegularFile
		}
		return false, fmt.Errorf("inspect source before removal: %w", err)
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return false, fmt.Errorf("stat source before removal: %w", err)
	}
	if !info.Mode().IsRegular() {
		return false, ErrNotRegularFile
	}
	if !sameFingerprint(fingerprintFromInfo(info), expected) {
		return false, conflictError("move", relativePath, nil)
	}
	if err := unix.Unlinkat(int(parent.Fd()), name, 0); err != nil {
		return false, fmt.Errorf("remove moved source: %w", err)
	}
	if err := parent.Sync(); err != nil {
		return true, fmt.Errorf("source was removed but directory sync failed: %w", err)
	}
	return true, nil
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
