//go:build linux

package fsroot

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"

	"golang.org/x/sys/unix"
)

// platformRemoveEntryIfUnchanged 在父目录句柄内复核审批状态后删除单项。
func platformRemoveEntryIfUnchanged(root *Root, expected Entry) error {
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
	if expected.Kind == KindDirectory && !expected.Symlink {
		entries, readErr := file.ReadDir(1)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return fmt.Errorf("inspect delete directory: %w", readErr)
		}
		if len(entries) != 0 {
			return conflictError("permanent delete non-empty directory", expected.Path, nil)
		}
	}
	if err := verifyRemovalEntryAt(parent, name, opened); err != nil {
		return conflictError("permanent delete", expected.Path, err)
	}
	if !expected.Symlink && expected.Kind == KindFile {
		if _, err := unix.FcntlInt(file.Fd(), unix.F_SETLEASE, unix.F_WRLCK); err != nil {
			return conflictError("permanent delete open file", expected.Path, err)
		}
		if err := requireFileWriteLease(file); err != nil {
			return conflictError("permanent delete", expected.Path, err)
		}
		if !sameFingerprint(fingerprintFromInfoAfterRead(file), expected.Fingerprint()) {
			return conflictError("permanent delete", expected.Path, nil)
		}
	}
	if err := verifyOpenedDirectory(root, parentPath, parent); err != nil {
		return err
	}
	if err := verifyRemovalEntryAt(parent, name, opened); err != nil {
		return conflictError("permanent delete", expected.Path, err)
	}
	if !expected.Symlink && expected.Kind == KindFile {
		if err := requireFileWriteLease(file); err != nil {
			return conflictError("permanent delete", expected.Path, err)
		}
	}
	removeFlags := 0
	if !expected.Symlink && expected.Kind == KindDirectory {
		removeFlags = unix.AT_REMOVEDIR
	}
	if err := unix.Unlinkat(int(parent.Fd()), name, removeFlags); err != nil {
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTEMPTY) {
			return conflictError("permanent delete", expected.Path, err)
		}
		return fmt.Errorf("permanent delete %q: %w", expected.Path, err)
	}
	if err := parent.Sync(); err != nil {
		return fmt.Errorf("%w: permanent delete %q: %w", ErrPublishedNotDurable, expected.Path, err)
	}
	return nil
}

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
