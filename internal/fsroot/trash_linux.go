//go:build linux

package fsroot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const trashAreaName = TrashDirectoryName

// TrashArea 返回源文件系统内受保护的回收目录及挂载边界。
func (r *Root) TrashArea(source string) (area, mountRoot string, mountID uint64, err error) {
	if err = validateMutationPath(source); err != nil {
		return "", "", 0, err
	}
	if err = r.checkVolumeAvailability(source); err != nil {
		return "", "", 0, err
	}
	if err = r.rejectSymlinkParents(source); err != nil {
		return "", "", 0, err
	}
	info, err := r.root.Lstat(source)
	if err != nil {
		return "", "", 0, err
	}
	mountID, err = r.MountID(source)
	if err != nil {
		return "", "", 0, err
	}
	mountRoot, err = r.mountRootForID(mountID)
	if err != nil {
		return "", "", 0, err
	}
	if info.IsDir() {
		if source == mountRoot {
			return "", "", 0, ErrMountRoot
		}
		if err := r.CheckTreeMounts(source, mountID); err != nil {
			return "", "", 0, err
		}
	}
	area = path.Join(mountRoot, trashAreaName)
	if mountRoot == "" {
		area = trashAreaName
	}
	if err := r.ensureTrashArea(area, mountRoot, mountID); err != nil {
		return "", "", 0, err
	}
	return area, mountRoot, mountID, nil
}

// VerifyTrashArea 校验回收目录标记、权限和挂载边界。
func (r *Root) VerifyTrashArea(area string, mountID uint64) error {
	if validateMutationPath(area) != nil || mountID == 0 {
		return ErrInvalidPath
	}
	mountRoot := path.Dir(area)
	if mountRoot == "." {
		mountRoot = ""
	}
	currentID, err := r.MountID(area)
	if err != nil || currentID != mountID {
		return ErrConflict
	}
	return r.verifyTrashArea(area, mountRoot, mountID)
}

// MountID 返回路径当前所在的 Linux 挂载实例标识。
func (r *Root) MountID(relativePath string) (uint64, error) {
	if err := validateRelativePath(relativePath); err != nil {
		return 0, err
	}
	if err := r.checkVolumeAvailability(relativePath); err != nil {
		return 0, err
	}
	if relativePath != "" {
		if err := r.rejectSymlinkParents(relativePath); err != nil {
			return 0, err
		}
	}
	if err := r.verifyRootPath(); err != nil {
		return 0, err
	}
	absolute := r.rootPath
	if relativePath != "" {
		absolute = filepath.Join(r.rootPath, filepath.FromSlash(relativePath))
	}
	var stat unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, absolute, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_MNT_ID, &stat); err != nil {
		return 0, fmt.Errorf("read mount identity: %w", err)
	}
	if stat.Mask&unix.STATX_MNT_ID == 0 || stat.Mnt_id == 0 {
		return 0, ErrIdentityUnavailable
	}
	return stat.Mnt_id, nil
}

// CheckTreeMounts 拒绝包含其他挂载实例的目录树。
func (r *Root) CheckTreeMounts(relativePath string, expectedMountID uint64) error {
	if expectedMountID == 0 || relativePath == "" {
		return ErrIdentityUnavailable
	}
	if err := validateMutationPath(relativePath); err != nil {
		return err
	}
	if err := r.rejectSymlinkParents(relativePath); err != nil {
		return err
	}
	parent, name, err := r.openParent(relativePath)
	if err != nil {
		return err
	}
	defer parent.Close()
	return checkTreeAt(int(parent.Fd()), name, expectedMountID)
}

// RemoveTreeNoFollow 核验登记身份与挂载后递归删除，不跟随软链接。
func (r *Root) RemoveTreeNoFollow(relativePath string, expectedMountID, expectedDevice, expectedInode uint64) error {
	if expectedMountID == 0 || expectedInode == 0 {
		return ErrIdentityUnavailable
	}
	if err := validateMutationPath(relativePath); err != nil {
		return err
	}
	if !hasTrashComponent(relativePath) {
		return ErrInvalidPath
	}
	if err := r.VerifyTrashArea(path.Dir(relativePath), expectedMountID); err != nil {
		return err
	}
	if err := r.rejectSymlinkParents(relativePath); err != nil {
		return err
	}
	parent, name, err := r.openParent(relativePath)
	if err != nil {
		return err
	}
	defer parent.Close()
	if err := removeTreeAt(int(parent.Fd()), name, expectedMountID, &trashIdentity{device: expectedDevice, inode: expectedInode}); err != nil {
		return fmt.Errorf("remove trash tree: %w", err)
	}
	return nil
}

func (r *Root) ensureTrashArea(area, mountRoot string, mountID uint64) error {
	if err := r.validateTrashAreaLocation(area, mountRoot, mountID); err != nil {
		return err
	}
	_, err := r.root.Lstat(area)
	if errors.Is(err, fs.ErrNotExist) {
		if err := r.createTrashAreaDirectory(area, mountRoot, mountID); err != nil {
			return fmt.Errorf("create trash area: %w", err)
		}
		if err := r.createTrashMarker(area); err != nil {
			return fmt.Errorf("create trash marker: %w", err)
		}
		return r.verifyTrashArea(area, mountRoot, mountID)
	}
	if err != nil {
		return fmt.Errorf("inspect trash area: %w", err)
	}
	return r.verifyTrashArea(area, mountRoot, mountID)
}

func (r *Root) verifyTrashArea(area, mountRoot string, mountID uint64) error {
	if err := r.validateTrashAreaLocation(area, mountRoot, mountID); err != nil {
		return err
	}
	info, err := r.root.Lstat(area)
	if err != nil {
		return fmt.Errorf("inspect trash area: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return errors.New("trash area is not a private real directory")
	}
	areaMountID, err := r.MountID(area)
	if err != nil || areaMountID != mountID {
		return errors.New("trash area is not on the source mount")
	}
	if err := r.CheckTreeMounts(area, mountID); err != nil {
		return err
	}
	marker := path.Join(area, ".filesage-marker")
	markerInfo, err := r.root.Lstat(marker)
	if errors.Is(err, fs.ErrNotExist) {
		return errors.New("existing directory is not a FileSage trash area")
	}
	if err != nil {
		return fmt.Errorf("inspect trash marker: %w", err)
	}
	if !markerInfo.Mode().IsRegular() || markerInfo.Mode()&os.ModeSymlink != 0 || markerInfo.Mode().Perm()&0o077 != 0 {
		return errors.New("trash marker is not a private regular file")
	}
	file, err := r.root.OpenFile(marker, os.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("open trash marker: %w", err)
	}
	openedInfo, statErr := file.Stat()
	if statErr != nil || !os.SameFile(markerInfo, openedInfo) {
		_ = file.Close()
		return ErrPathChanged
	}
	data, readErr := io.ReadAll(io.LimitReader(file, 64))
	_ = file.Close()
	if readErr != nil || string(data) != "filesage-trash-v1\n" {
		return errors.New("trash marker is invalid")
	}
	return nil
}

func (r *Root) validateTrashAreaLocation(area, mountRoot string, mountID uint64) error {
	if mountID == 0 || !validStoredTrashArea(area, mountRoot) {
		return ErrInvalidPath
	}
	parent := mountRoot
	parentID, err := r.MountID(parent)
	if err != nil || parentID != mountID {
		return ErrConflict
	}
	if err := r.rejectSymlinkParents(area); err != nil {
		return err
	}
	return nil
}

func validStoredTrashArea(area, mountRoot string) bool {
	expected := trashAreaName
	if mountRoot != "" {
		if validateMutationPath(mountRoot) != nil {
			return false
		}
		expected = path.Join(mountRoot, trashAreaName)
	}
	return area == expected && validateMutationPath(area) == nil
}

// createTrashAreaDirectory 通过已核验的父目录句柄独占创建回收目录。
func (r *Root) createTrashAreaDirectory(area, mountRoot string, mountID uint64) error {
	if err := r.validateTrashAreaLocation(area, mountRoot, mountID); err != nil {
		return err
	}
	parentID, err := r.MountID(mountRoot)
	if err != nil || parentID != mountID {
		return ErrConflict
	}
	parentPath := mountRoot
	if parentPath == "" {
		parentPath = "."
	}
	parent, err := r.root.Open(parentPath)
	if err != nil {
		return err
	}
	defer parent.Close()
	info, err := parent.Stat()
	if err != nil || !info.IsDir() {
		if err != nil {
			return err
		}
		return ErrNotDirectory
	}
	fdStat, err := statxFD(int(parent.Fd()))
	if err != nil || fdStat.Mnt_id != mountID {
		return ErrConflict
	}
	currentInfo, err := r.root.Stat(parentPath)
	if err != nil || !os.SameFile(info, currentInfo) {
		return ErrPathChanged
	}
	if err := r.validateTrashAreaLocation(area, mountRoot, mountID); err != nil {
		return err
	}
	if err := unix.Mkdirat(int(parent.Fd()), trashAreaName, 0o700); err != nil {
		return err
	}
	return nil
}

func (r *Root) createTrashMarker(area string) error {
	marker := path.Join(area, ".filesage-marker")
	file, err := r.root.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(file, "filesage-trash-v1\n"); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func (r *Root) verifyRootPath() error {
	opened, err := r.root.Stat(".")
	if err != nil {
		return err
	}
	current, err := os.Stat(r.rootPath)
	if err != nil || !os.SameFile(opened, current) {
		return ErrPathChanged
	}
	return nil
}

func (r *Root) mountRootForID(mountID uint64) (string, error) {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return "", fmt.Errorf("read mount table: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		var id uint64
		if _, err := fmt.Sscan(fields[0], &id); err != nil || id != mountID {
			continue
		}
		mountPath := filepath.Clean(decodeMountField(fields[4]))
		relative, err := filepath.Rel(r.rootPath, mountPath)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return "", nil
		}
		if relative == "." {
			return "", nil
		}
		return filepath.ToSlash(relative), nil
	}
	return "", ErrIdentityUnavailable
}

func decodeMountField(value string) string {
	var decoded strings.Builder
	for index := 0; index < len(value); index++ {
		if value[index] == '\\' && index+3 < len(value) {
			if value[index+1] >= '0' && value[index+1] <= '7' && value[index+2] >= '0' && value[index+2] <= '7' && value[index+3] >= '0' && value[index+3] <= '7' {
				decoded.WriteByte((value[index+1]-'0')*64 + (value[index+2]-'0')*8 + (value[index+3] - '0'))
				index += 3
				continue
			}
		}
		decoded.WriteByte(value[index])
	}
	return decoded.String()
}

func (r *Root) openParent(relativePath string) (*os.File, string, error) {
	parent := path.Dir(relativePath)
	if parent == "." {
		parent = "."
	}
	file, err := r.root.Open(parent)
	if err != nil {
		return nil, "", err
	}
	info, err := file.Stat()
	if err != nil || !info.IsDir() {
		_ = file.Close()
		if err != nil {
			return nil, "", err
		}
		return nil, "", ErrNotDirectory
	}
	return file, path.Base(relativePath), nil
}

func checkTreeAt(parentFD int, name string, expected uint64) error {
	stat, err := statxAt(parentFD, name, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		return err
	}
	if stat.Mnt_id != expected {
		return ErrNestedMount
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return nil
	}
	fd, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	opened, err := statxFD(fd)
	if err != nil || opened.Mnt_id != expected {
		if err != nil {
			return err
		}
		return ErrNestedMount
	}
	file := os.NewFile(uintptr(fd), name)
	children, err := file.ReadDir(-1)
	if err != nil {
		_ = file.Close()
		return err
	}
	for _, child := range children {
		if err := checkTreeAt(fd, child.Name(), expected); err != nil {
			_ = file.Close()
			return err
		}
	}
	_ = file.Close()
	return nil
}

type trashIdentity struct {
	device uint64
	inode  uint64
}

func removeTreeAt(parentFD int, name string, expectedMountID uint64, expectedRoot *trashIdentity) error {
	return removeTreeAtWithMovePolicy(parentFD, name, expectedMountID, expectedRoot, nil, "")
}

type moveTreeRemovalPolicy struct {
	quarantine string
	entries    map[string]Entry
	ctx        context.Context
	preflight  bool
	visited    int
}

// removeMoveTreeAt 先核对整棵树，再逐项取得租约并移除来源。
func removeMoveTreeAt(ctx context.Context, parentFD int, name string, expectedMountID uint64, source, quarantine string, snapshot TreeSnapshot) error {
	if len(snapshot.Entries) == 0 || snapshot.RootPath != source || snapshot.Entries[0].Entry.Path != source {
		return ErrInvalidPath
	}
	policy := &moveTreeRemovalPolicy{
		quarantine: quarantine,
		entries:    make(map[string]Entry, len(snapshot.Entries)),
		ctx:        ctx,
		preflight:  true,
	}
	for _, item := range snapshot.Entries {
		entryPath := item.Entry.Path
		if entryPath != source && !strings.HasPrefix(entryPath, source+"/") {
			return ErrInvalidPath
		}
		movePath := quarantine + strings.TrimPrefix(entryPath, source)
		if _, exists := policy.entries[movePath]; exists {
			return ErrInvalidPath
		}
		policy.entries[movePath] = item.Entry
	}
	root := snapshot.Entries[0].Entry
	expectedRoot := &trashIdentity{device: root.Device, inode: root.Inode}
	if err := removeTreeAtWithMovePolicy(parentFD, name, expectedMountID, expectedRoot, policy, quarantine); err != nil {
		return err
	}
	if policy.visited != len(policy.entries) {
		return ErrConflict
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	policy.visited = 0
	policy.preflight = false
	if err := removeTreeAtWithMovePolicy(parentFD, name, expectedMountID, expectedRoot, policy, quarantine); err != nil {
		return err
	}
	if policy.visited != len(policy.entries) {
		return ErrConflict
	}
	return nil
}

// removeTreeAtWithMovePolicy 共用安全遍历，并在移动删除前复核每项状态。
func removeTreeAtWithMovePolicy(parentFD int, name string, expectedMountID uint64, expectedRoot *trashIdentity, policy *moveTreeRemovalPolicy, relativePath string) error {
	if policy != nil && policy.preflight {
		if err := policy.ctx.Err(); err != nil {
			return err
		}
	}
	stat, err := statxAt(parentFD, name, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		return err
	}
	if stat.Mnt_id != expectedMountID {
		return ErrNestedMount
	}
	if expectedRoot != nil && !matchesTrashIdentity(stat, *expectedRoot) {
		return ErrPathChanged
	}
	var expected Entry
	if policy != nil {
		var ok bool
		expected, ok = policy.entries[relativePath]
		if !ok {
			return ErrConflict
		}
		policy.visited++
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		if policy != nil && stat.Mode&unix.S_IFMT == unix.S_IFREG {
			return removeMoveFileEntry(parentFD, name, expectedMountID, expected.Fingerprint(), policy.preflight)
		}
		fd, err := unix.Openat(parentFD, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		defer unix.Close(fd)
		opened, err := statxFD(fd)
		if err != nil {
			return err
		}
		if opened.Mnt_id != expectedMountID || !sameTrashObject(stat, opened) {
			return ErrPathChanged
		}
		if expectedRoot != nil && !matchesTrashIdentity(opened, *expectedRoot) {
			return ErrPathChanged
		}
		if policy != nil && (opened.Ino != expected.Inode || statxDevice(opened) != expected.Device) {
			return ErrConflict
		}
		current, err := statxAt(parentFD, name, unix.AT_SYMLINK_NOFOLLOW)
		if err != nil {
			return err
		}
		if current.Mnt_id != expectedMountID || !sameTrashObject(opened, current) {
			return ErrPathChanged
		}
		if policy != nil && policy.preflight {
			return nil
		}
		return unix.Unlinkat(parentFD, name, 0)
	}
	fd, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	opened, err := statxFD(fd)
	if err != nil || opened.Mnt_id != expectedMountID || !sameTrashObject(stat, opened) {
		_ = unix.Close(fd)
		if err != nil {
			return err
		}
		if opened.Mnt_id != expectedMountID {
			return ErrNestedMount
		}
		return ErrPathChanged
	}
	if expectedRoot != nil && !matchesTrashIdentity(opened, *expectedRoot) {
		_ = unix.Close(fd)
		return ErrPathChanged
	}
	file := os.NewFile(uintptr(fd), name)
	if policy != nil {
		info, statErr := file.Stat()
		if statErr != nil {
			_ = file.Close()
			return statErr
		}
		current := fingerprintFromInfo(info)
		matches := sameFingerprint(current, expected.Fingerprint())
		if relativePath == policy.quarantine {
			matches = sameFingerprintAfterRename(current, expected.Fingerprint())
		}
		if !matches {
			_ = file.Close()
			return ErrConflict
		}
	}
	children, err := file.ReadDir(-1)
	if err != nil {
		_ = file.Close()
		return err
	}
	for _, child := range children {
		childPath := path.Join(relativePath, child.Name())
		if err := removeTreeAtWithMovePolicy(fd, child.Name(), expectedMountID, nil, policy, childPath); err != nil {
			_ = file.Close()
			return err
		}
	}
	opened, err = statxFD(fd)
	if err != nil {
		_ = file.Close()
		return err
	}
	if opened.Mnt_id != expectedMountID || !sameTrashObject(stat, opened) {
		_ = file.Close()
		return ErrPathChanged
	}
	_ = file.Close()
	current, err := statxAt(parentFD, name, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		return err
	}
	if current.Mnt_id != expectedMountID || !sameTrashObject(opened, current) || current.Mode&unix.S_IFMT != unix.S_IFDIR {
		return ErrPathChanged
	}
	if policy != nil && policy.preflight {
		return nil
	}
	return unix.Unlinkat(parentFD, name, unix.AT_REMOVEDIR)
}

// removeMoveFileEntry 在快照复核及写租约保护下删除普通文件。
func removeMoveFileEntry(parentFD int, name string, expectedMountID uint64, expected Fingerprint, preflight bool) error {
	fd, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	opened, err := statxFD(fd)
	if err != nil {
		return err
	}
	current, err := statxAt(parentFD, name, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		return err
	}
	if opened.Mnt_id != expectedMountID || opened.Mode&unix.S_IFMT != unix.S_IFREG || !sameTrashObject(opened, current) || !sameFingerprint(fingerprintFromInfo(info), expected) {
		return ErrConflict
	}
	if _, err := unix.FcntlInt(file.Fd(), unix.F_SETLEASE, unix.F_WRLCK); err != nil {
		return fmt.Errorf("%w: acquire source write lease: %w", ErrConflict, err)
	}
	if err := requireFileWriteLease(file); err != nil {
		return fmt.Errorf("%w: source write lease was broken: %w", ErrConflict, err)
	}
	current, err = statxAt(parentFD, name, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		return err
	}
	if current.Mnt_id != expectedMountID || current.Mode&unix.S_IFMT != unix.S_IFREG || !sameTrashObject(opened, current) || !sameFingerprint(fingerprintFromInfoAfterRead(file), expected) {
		return ErrConflict
	}
	if err := requireFileWriteLease(file); err != nil {
		return fmt.Errorf("%w: source write lease was broken: %w", ErrConflict, err)
	}
	if preflight {
		return nil
	}
	return unix.Unlinkat(parentFD, name, 0)
}

func sameTrashObject(first, second unix.Statx_t) bool {
	return first.Ino == second.Ino && first.Dev_major == second.Dev_major && first.Dev_minor == second.Dev_minor
}

func matchesTrashIdentity(stat unix.Statx_t, expected trashIdentity) bool {
	return stat.Ino == expected.inode && statxDevice(stat) == expected.device
}

func statxDevice(stat unix.Statx_t) uint64 {
	major, minor := uint64(stat.Dev_major), uint64(stat.Dev_minor)
	return ((major & 0xfff) << 8) | (minor & 0xff) | ((minor &^ 0xff) << 12) | ((major &^ 0xfff) << 32)
}

func statxAt(dirfd int, name string, flags int) (unix.Statx_t, error) {
	var stat unix.Statx_t
	if err := unix.Statx(dirfd, name, flags, unix.STATX_BASIC_STATS|unix.STATX_MNT_ID, &stat); err != nil {
		return stat, err
	}
	if stat.Mask&unix.STATX_MNT_ID == 0 || stat.Mask&unix.STATX_INO == 0 || stat.Mnt_id == 0 {
		return stat, ErrIdentityUnavailable
	}
	return stat, nil
}

func statxFD(fd int) (unix.Statx_t, error) {
	return statxAt(fd, "", unix.AT_EMPTY_PATH)
}
