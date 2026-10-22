// 本文件提供流式新建入口，并按平台选择安全实现。
package fsroot

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
)

// WriteNewAtomicFrom 流式新建文件，目标已存在时不覆盖。
func (r *Root) WriteNewAtomicFrom(rel string, source io.Reader, expectedSize int64, perm fs.FileMode) (string, error) {
	if err := validateMutationPath(rel); err != nil {
		return "", err
	}
	if hasTrashComponent(rel) {
		return "", ErrInvalidPath
	}
	if source == nil {
		return "", fmt.Errorf("write %q: nil source: %w", rel, fs.ErrInvalid)
	}
	if expectedSize < 0 {
		return "", fmt.Errorf("write %q: negative expected size: %w", rel, fs.ErrInvalid)
	}
	if err := validatePermissionMode(perm); err != nil {
		return "", err
	}
	if err := r.rejectSymlinkParents(rel); err != nil {
		return "", err
	}
	if _, err := r.root.Lstat(rel); err == nil {
		return "", conflictError("create", rel, fs.ErrExist)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("inspect new file %q: %w", rel, err)
	}
	return platformWriteNewAtomicFrom(r, rel, source, expectedSize, perm)
}

// WriteAtomicFrom 校验旧版本后流式原子写入文件，避免将大候选载入内存。
func (r *Root) WriteAtomicFrom(rel string, expected Fingerprint, source io.Reader, expectedSize int64, perm fs.FileMode) (string, error) {
	if !expected.Exists {
		return r.WriteNewAtomicFrom(rel, source, expectedSize, perm)
	}
	if err := validateMutationPath(rel); err != nil {
		return "", err
	}
	if hasTrashComponent(rel) || source == nil || expectedSize < 0 {
		return "", ErrInvalidPath
	}
	if err := validatePermissionMode(perm); err != nil {
		return "", err
	}
	if err := r.checkVolumeAvailability(rel); err != nil {
		return "", err
	}
	if !atomicWriteSupported() {
		return "", fmt.Errorf("write %q: %w", rel, ErrAtomicWriteUnsupported)
	}
	if expected.Links > 1 {
		return "", fmt.Errorf("write %q: %w", rel, ErrHardLinked)
	}
	if err := r.checkFingerprint(rel, expected); err != nil {
		return "", err
	}
	parentRel := path.Dir(rel)
	if parentRel == "." {
		parentRel = ""
	}
	var tempRel string
	var temp *os.File
	for range 4 {
		suffix := make([]byte, 12)
		if _, err := rand.Read(suffix); err != nil {
			return "", err
		}
		tempRel = path.Join(parentRel, ".filesage-write-"+hex.EncodeToString(suffix))
		file, err := r.root.OpenFile(tempRel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm.Perm())
		if err == nil {
			temp = file
			break
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", err
		}
	}
	if temp == nil {
		return "", fs.ErrExist
	}
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = r.root.Remove(tempRel)
		}
	}()
	digest := sha256.New()
	written, err := io.CopyN(io.MultiWriter(temp, digest), source, expectedSize)
	if err != nil || written != expectedSize {
		_ = temp.Close()
		return "", errors.Join(err, ErrSizeMismatch)
	}
	if err := streamReaderEOF(source); err != nil {
		_ = temp.Close()
		return "", err
	}
	if err := temp.Chmod(perm.Perm()); err != nil {
		_ = temp.Close()
		return "", err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return "", err
	}
	if err := temp.Close(); err != nil {
		return "", err
	}
	if err := r.checkFingerprint(rel, expected); err != nil {
		return "", err
	}
	expectedDigest, err := r.digestIfUnchanged(rel, expected)
	if err != nil {
		return "", err
	}
	var candidateDigest [sha256.Size]byte
	copy(candidateDigest[:], digest.Sum(nil))
	// 流式写回与普通编辑共用租约、原子交换及冲突恢复边界。
	if err := platformReplaceAtomicIfUnchanged(r, rel, tempRel, expected, expectedDigest, candidateDigest, expectedSize, perm.Perm()); err != nil {
		var recovery *WriteRecoveryError
		if errors.As(err, &recovery) {
			removeTemp = false
		}
		return "", err
	}
	removeTemp = false
	return hex.EncodeToString(candidateDigest[:]), nil
}

// streamReaderEOF 拒绝候选在声明长度之外还包含数据。
func streamReaderEOF(source io.Reader) error {
	var extra [1]byte
	for emptyReads := 0; emptyReads < 100; emptyReads++ {
		n, err := source.Read(extra[:])
		if n > 0 {
			return ErrSizeMismatch
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	return io.ErrNoProgress
}
