//go:build linux

package fsroot

import (
	"errors"
	"io/fs"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// platformMkdirAllNoFollow 使用目录句柄防止创建期间路径被软链接替换。
func platformMkdirAllNoFollow(root *Root, rel string, perm fs.FileMode) ([]string, error) {
	current, err := root.root.Open(".")
	if err != nil {
		return nil, err
	}
	created := make([]string, 0, 2)
	currentPath := ""
	for _, component := range strings.Split(rel, "/") {
		var before unix.Stat_t
		err := unix.Fstatat(int(current.Fd()), component, &before, unix.AT_SYMLINK_NOFOLLOW)
		if errors.Is(err, unix.ENOENT) {
			createdHere := false
			if createErr := unix.Mkdirat(int(current.Fd()), component, uint32(perm.Perm())); createErr == nil {
				createdHere = true
			} else if !errors.Is(createErr, unix.EEXIST) {
				_ = current.Close()
				return created, createErr
			}
			if err := unix.Fstatat(int(current.Fd()), component, &before, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				_ = current.Close()
				return created, err
			}
			if createdHere && before.Mode&unix.S_IFMT == unix.S_IFDIR {
				created = append(created, joinRootPath(currentPath, component))
			}
		} else if err != nil {
			_ = current.Close()
			return created, err
		}
		if before.Mode&unix.S_IFMT == unix.S_IFLNK {
			_ = current.Close()
			return created, ErrSymlinkPath
		}
		if before.Mode&unix.S_IFMT != unix.S_IFDIR {
			_ = current.Close()
			return created, ErrNotDirectory
		}
		nextFD, err := unix.Openat(int(current.Fd()), component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			_ = current.Close()
			return created, ErrPathChanged
		}
		var after unix.Stat_t
		if err := unix.Fstat(nextFD, &after); err != nil || before.Dev != after.Dev || before.Ino != after.Ino {
			_ = unix.Close(nextFD)
			_ = current.Close()
			return created, ErrPathChanged
		}
		next := os.NewFile(uintptr(nextFD), component)
		_ = current.Close()
		current = next
		currentPath = joinRootPath(currentPath, component)
	}
	_ = current.Close()
	return created, nil
}

func joinRootPath(parent, name string) string {
	if parent == "" {
		return name
	}
	return parent + "/" + name
}
