//go:build linux

// 本文件记录并复核 Linux 子卷挂载实例。

package fsroot

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// platformRegisterMounts 在启动时记录管理根目录内可见的子卷实例。
func platformRegisterMounts(rootPath string) ([]registeredMount, error) {
	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, fmt.Errorf("read mount table: %w", err)
	}
	defer file.Close()

	paths := make(map[string]map[uint64]struct{})
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 6 {
			continue
		}
		id, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil || id == 0 {
			continue
		}
		mountPath := filepath.Clean(decodeMountField(fields[4]))
		relative, err := filepath.Rel(rootPath, mountPath)
		if err != nil || relative == "." || relative == ".." || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			continue
		}
		relative = filepath.ToSlash(relative)
		if paths[relative] == nil {
			paths[relative] = make(map[uint64]struct{})
		}
		paths[relative][id] = struct{}{}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read mount table: %w", err)
	}

	relatives := make([]string, 0, len(paths))
	for relative := range paths {
		relatives = append(relatives, relative)
	}
	sort.Strings(relatives)
	mounts := make([]registeredMount, 0, len(relatives))
	for _, relative := range relatives {
		currentID, err := platformMountIDAt(rootPath, relative)
		if err != nil {
			return nil, fmt.Errorf("inspect mount %q: %w", relative, err)
		}
		if _, visible := paths[relative][currentID]; visible {
			mounts = append(mounts, registeredMount{path: relative, id: currentID})
		}
	}
	return mounts, nil
}

// platformMountIDAt 返回路径当前所在的挂载实例。
func platformMountIDAt(rootPath, relativePath string) (uint64, error) {
	absolute := rootPath
	if relativePath != "" {
		absolute = filepath.Join(rootPath, filepath.FromSlash(relativePath))
	}
	var stat unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, absolute, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_MNT_ID, &stat); err != nil {
		return 0, err
	}
	if stat.Mask&unix.STATX_MNT_ID == 0 || stat.Mnt_id == 0 {
		return 0, ErrIdentityUnavailable
	}
	return stat.Mnt_id, nil
}

// platformMountIDForFD 返回已打开对象所属的挂载实例。
func platformMountIDForFD(fd uintptr) (uint64, error) {
	var stat unix.Statx_t
	if err := unix.Statx(int(fd), "", unix.AT_EMPTY_PATH, unix.STATX_MNT_ID, &stat); err != nil {
		return 0, err
	}
	if stat.Mask&unix.STATX_MNT_ID == 0 || stat.Mnt_id == 0 {
		return 0, ErrIdentityUnavailable
	}
	return stat.Mnt_id, nil
}
