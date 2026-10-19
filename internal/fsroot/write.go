// 本文件实现根目录内的受控创建、移动、删除与内容替换。
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

// Mkdir 创建一个目录，不递归创建父目录，也不覆盖已有项。
func (r *Root) Mkdir(rel string, perm fs.FileMode) error {
	if err := validateMutationPath(rel); err != nil {
		return err
	}
	if hasTrashComponent(rel) {
		return ErrInvalidPath
	}
	if err := validatePermissionMode(perm); err != nil {
		return err
	}
	if err := r.rejectSymlinkParents(rel); err != nil {
		return err
	}
	if err := r.root.Mkdir(rel, perm); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return conflictError("mkdir", rel, err)
		}
		return fmt.Errorf("mkdir %q: %w", rel, err)
	}
	return nil
}

// MkdirAllNoFollow 逐级创建真实目录，不跟随软链接且不覆盖非目录项。
func (r *Root) MkdirAllNoFollow(rel string, perm fs.FileMode) ([]string, error) {
	if err := validateMutationPath(rel); err != nil || hasTrashComponent(rel) {
		return nil, ErrInvalidPath
	}
	if err := validatePermissionMode(perm); err != nil {
		return nil, err
	}
	if err := r.checkVolumeAvailability(rel); err != nil {
		return nil, err
	}
	return platformMkdirAllNoFollow(r, rel, perm)
}

// RenameNoReplace 原子地重命名单个目录项；目标存在时返回冲突。
func (r *Root) RenameNoReplace(oldRel, newRel string) error {
	if err := validateMutationPath(oldRel); err != nil {
		return err
	}
	if err := validateMutationPath(newRel); err != nil {
		return err
	}
	if hasTrashComponent(oldRel) || hasTrashComponent(newRel) {
		return ErrInvalidPath
	}
	if oldRel == newRel {
		return conflictError("rename", newRel, fs.ErrExist)
	}
	if err := r.rejectSymlinkParents(oldRel); err != nil {
		return err
	}
	if err := r.rejectSymlinkParents(newRel); err != nil {
		return err
	}
	if _, err := r.root.Lstat(oldRel); err != nil {
		return fmt.Errorf("inspect rename source %q: %w", oldRel, err)
	}
	if err := platformRenameNoReplace(r, oldRel, newRel); err != nil {
		return fmt.Errorf("rename %q to %q: %w", oldRel, newRel, err)
	}
	return nil
}

// MoveToTrash 仅允许移动到已校验的内部回收目录。
func (r *Root) MoveToTrash(oldRel, newRel string, mountID uint64) error {
	if err := validateMutationPath(oldRel); err != nil || hasTrashComponent(oldRel) {
		return ErrInvalidPath
	}
	if err := validateMutationPath(newRel); err != nil || !hasTrashComponent(newRel) {
		return ErrInvalidPath
	}
	area := path.Dir(newRel)
	if err := r.VerifyTrashArea(area, mountID); err != nil {
		return err
	}
	oldMountID, err := r.MountID(oldRel)
	if err != nil || oldMountID != mountID {
		return ErrConflict
	}
	info, err := r.root.Lstat(oldRel)
	if err != nil {
		return err
	}
	if info.IsDir() {
		if err := r.CheckTreeMounts(oldRel, mountID); err != nil {
			return err
		}
	}
	if err := r.rejectSymlinkParents(oldRel); err != nil {
		return err
	}
	if err := r.rejectSymlinkParents(newRel); err != nil {
		return err
	}
	if err := platformRenameNoReplace(r, oldRel, newRel); err != nil {
		return fmt.Errorf("move %q to trash: %w", oldRel, err)
	}
	return nil
}

// RestoreFromTrash 仅允许从已校验的内部回收目录移出且不覆盖。
func (r *Root) RestoreFromTrash(oldRel, newRel string, mountID uint64) error {
	if err := validateMutationPath(oldRel); err != nil || !hasTrashComponent(oldRel) {
		return ErrInvalidPath
	}
	if err := validateMutationPath(newRel); err != nil || hasTrashComponent(newRel) {
		return ErrInvalidPath
	}
	area := path.Dir(oldRel)
	if err := r.VerifyTrashArea(area, mountID); err != nil {
		return err
	}
	oldMountID, err := r.MountID(oldRel)
	if err != nil || oldMountID != mountID {
		return ErrConflict
	}
	if err := r.rejectSymlinkParents(newRel); err != nil {
		return err
	}
	newParent := path.Dir(newRel)
	if newParent == "." {
		newParent = ""
	}
	newMountID, err := r.MountID(newParent)
	if err != nil {
		return err
	}
	if newMountID != mountID {
		return ErrConflict
	}
	if err := platformRenameNoReplace(r, oldRel, newRel); err != nil {
		return fmt.Errorf("restore %q: %w", oldRel, err)
	}
	return nil
}

// Remove 删除单个文件、软链接或空目录，不递归删除目录内容。
func (r *Root) Remove(rel string) error {
	if err := validateMutationPath(rel); err != nil {
		return err
	}
	if hasTrashComponent(rel) {
		return ErrInvalidPath
	}
	if err := r.rejectSymlinkParents(rel); err != nil {
		return err
	}
	if err := r.root.Remove(rel); err != nil {
		return fmt.Errorf("remove %q: %w", rel, err)
	}
	return nil
}

// WriteAtomicIfUnchanged 仅在状态与快照相符时提交完整内容。
// 零值快照只创建新文件；已存在文件必须提供可用身份快照。
func (r *Root) WriteAtomicIfUnchanged(rel string, expected Fingerprint, data []byte, perm fs.FileMode) error {
	if err := validateMutationPath(rel); err != nil {
		return err
	}
	if hasTrashComponent(rel) {
		return ErrInvalidPath
	}
	if err := validatePermissionMode(perm); err != nil {
		return err
	}
	if err := r.checkVolumeAvailability(rel); err != nil {
		return err
	}
	if !atomicWriteSupported() {
		return fmt.Errorf("write %q: %w", rel, ErrAtomicWriteUnsupported)
	}
	if err := r.rejectSymlinkParents(rel); err != nil {
		return err
	}
	if expected.Exists && !expected.IdentityAvailable {
		return fmt.Errorf("write %q: %w", rel, ErrIdentityUnavailable)
	}
	if expected.Exists && expected.Links > 1 {
		return fmt.Errorf("write %q: %w", rel, ErrHardLinked)
	}
	if err := r.checkFingerprint(rel, expected); err != nil {
		return err
	}

	parent := path.Dir(rel)
	if parent == "." {
		parent = ""
	}
	var tempRel string
	var tempFile *os.File
	for range 4 {
		suffix := make([]byte, 12)
		if _, err := rand.Read(suffix); err != nil {
			return fmt.Errorf("create write temporary name: %w", err)
		}
		name := ".filesage-write-" + hex.EncodeToString(suffix)
		tempRel = path.Join(parent, name)
		file, openErr := r.root.OpenFile(tempRel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm.Perm())
		if openErr == nil {
			tempFile = file
			break
		}
		if !errors.Is(openErr, fs.ErrExist) {
			return fmt.Errorf("create write temporary file in %q: %w", parent, openErr)
		}
	}
	if tempFile == nil {
		return fmt.Errorf("create write temporary file in %q: %w", parent, fs.ErrExist)
	}
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = r.root.Remove(tempRel)
		}
	}()

	if err := writeAll(tempFile, data); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("write temporary file for %q: %w", rel, err)
	}
	if err := tempFile.Chmod(perm.Perm()); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("set temporary file mode for %q: %w", rel, err)
	}
	if err := tempFile.Sync(); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("sync temporary file for %q: %w", rel, err)
	}
	if err := tempFile.Close(); err != nil {
		return fmt.Errorf("close temporary file for %q: %w", rel, err)
	}

	if err := r.checkFingerprint(rel, expected); err != nil {
		return err
	}
	if !expected.Exists {
		if err := platformRenameNoReplace(r, tempRel, rel); err != nil {
			return fmt.Errorf("publish new file %q: %w", rel, err)
		}
	} else {
		expectedDigest, err := r.digestIfUnchanged(rel, expected)
		if err != nil {
			return err
		}
		candidateDigest := sha256.Sum256(data)
		if err := platformReplaceAtomicIfUnchanged(r, rel, tempRel, expected, expectedDigest, candidateDigest, int64(len(data)), perm.Perm()); err != nil {
			var recovery *WriteRecoveryError
			if errors.As(err, &recovery) {
				removeTemp = false
			}
			return err
		}
	}
	removeTemp = false
	return nil
}

// WriteRecoveryError 表示冲突文件已保留在管理根目录内的临时路径。
type WriteRecoveryError struct {
	Path  string
	Cause error
}

func (e *WriteRecoveryError) Error() string {
	return fmt.Sprintf("write needs manual recovery at %q: %v", e.Path, e.Cause)
}

func (e *WriteRecoveryError) Unwrap() error { return e.Cause }

// digestIfUnchanged 在内容摘要前后复核文件身份与路径状态。
func (r *Root) digestIfUnchanged(rel string, expected Fingerprint) ([32]byte, error) {
	if err := r.checkFingerprint(rel, expected); err != nil {
		return [32]byte{}, err
	}
	file, err := r.root.Open(rel)
	if err != nil {
		return [32]byte{}, fmt.Errorf("open write source %q: %w", rel, err)
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil || !sameFingerprint(fingerprintFromInfo(before), expected) {
		return [32]byte{}, conflictError("read write source", rel, err)
	}
	digest := sha256.New()
	if _, err := io.CopyN(digest, file, expected.Size); err != nil {
		return [32]byte{}, conflictError("read write source", rel, err)
	}
	after, err := file.Stat()
	if err != nil || !sameFingerprint(fingerprintFromInfo(after), expected) {
		return [32]byte{}, conflictError("read write source", rel, err)
	}
	if err := r.checkFingerprint(rel, expected); err != nil {
		return [32]byte{}, err
	}
	var result [32]byte
	copy(result[:], digest.Sum(nil))
	return result, nil
}

func sameFingerprintAfterRename(current, expected Fingerprint) bool {
	current.ChangeTime = expected.ChangeTime
	return sameFingerprint(current, expected)
}

func validateMutationPath(rel string) error {
	if rel == "" {
		return fmt.Errorf("%w: root cannot be mutated as an entry", ErrInvalidPath)
	}
	return validateRelativePath(rel)
}

func validatePermissionMode(perm fs.FileMode) error {
	if perm&^fs.ModePerm != 0 {
		return fmt.Errorf("unsupported permission bits %s: %w", perm, fs.ErrInvalid)
	}
	return nil
}

func (r *Root) rejectSymlinkParents(rel string) error {
	if err := r.checkVolumeAvailability(rel); err != nil {
		return err
	}
	parent := path.Dir(rel)
	if parent == "." {
		return nil
	}
	resolved, err := r.resolvePath(parent)
	if err != nil {
		return fmt.Errorf("resolve parent of %q: %w", rel, err)
	}
	if resolved.hadLink {
		return fmt.Errorf("parent of %q: %w", rel, ErrSymlinkPath)
	}
	if !resolved.info.IsDir() {
		return fmt.Errorf("parent of %q: %w", rel, ErrNotDirectory)
	}
	return nil
}

func (r *Root) checkFingerprint(rel string, expected Fingerprint) error {
	if err := r.rejectSymlinkParents(rel); err != nil {
		return err
	}
	info, err := r.root.Lstat(rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) && !expected.Exists {
			return nil
		}
		if errors.Is(err, fs.ErrNotExist) && expected.Exists {
			return conflictError("write", rel, err)
		}
		return fmt.Errorf("inspect write target %q: %w", rel, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("write target %q: %w", rel, ErrSymlinkPath)
	}
	if !expected.Exists {
		return conflictError("create", rel, fs.ErrExist)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("write target %q: %w", rel, ErrNotRegularFile)
	}
	current := fingerprintFromInfo(info)
	if !current.IdentityAvailable || !expected.IdentityAvailable {
		return fmt.Errorf("write %q: %w", rel, ErrIdentityUnavailable)
	}
	if current.Links > 1 || expected.Links > 1 {
		return fmt.Errorf("write %q: %w", rel, ErrHardLinked)
	}
	if !sameFingerprint(current, expected) {
		return conflictError("write", rel, nil)
	}
	return nil
}

func fingerprintFromInfo(info fs.FileInfo) Fingerprint {
	device, inode, links, available := fileIdentity(info)
	ownerUID, groupGID, ownershipAvailable := fileOwnership(info)
	changeTime, changeTimeAvailable := fileChangeTime(info)
	return Fingerprint{
		Exists:              true,
		Kind:                kindFromInfo(info),
		Size:                info.Size(),
		ModifiedAt:          info.ModTime(),
		Mode:                info.Mode(),
		Device:              device,
		Inode:               inode,
		Links:               links,
		IdentityAvailable:   available,
		OwnerUID:            ownerUID,
		GroupGID:            groupGID,
		OwnershipAvailable:  ownershipAvailable,
		ChangeTime:          changeTime,
		ChangeTimeAvailable: changeTimeAvailable,
	}
}

func sameFingerprint(current, expected Fingerprint) bool {
	return current.Exists == expected.Exists &&
		current.Kind == expected.Kind &&
		current.Size == expected.Size &&
		current.ModifiedAt.Equal(expected.ModifiedAt) &&
		current.Mode == expected.Mode &&
		current.Device == expected.Device &&
		current.Inode == expected.Inode &&
		current.Links == expected.Links &&
		current.IdentityAvailable == expected.IdentityAvailable &&
		current.OwnerUID == expected.OwnerUID &&
		current.GroupGID == expected.GroupGID &&
		current.OwnershipAvailable == expected.OwnershipAvailable &&
		current.ChangeTimeAvailable == expected.ChangeTimeAvailable &&
		current.ChangeTime.Equal(expected.ChangeTime)
}

func conflictError(operation, rel string, cause error) error {
	if cause == nil {
		return fmt.Errorf("%s %q: %w", operation, rel, ErrConflict)
	}
	return fmt.Errorf("%s %q: %w: %w", operation, rel, ErrConflict, cause)
}

func writeAll(file *os.File, data []byte) error {
	for len(data) > 0 {
		n, err := file.Write(data)
		data = data[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
