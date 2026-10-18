//go:build linux

package fsroot

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"

	"golang.org/x/sys/unix"
)

func platformRenameNoReplace(root *Root, oldRel, newRel string) error {
	oldParent, oldName, err := openRenameParent(root, oldRel)
	if err != nil {
		return fmt.Errorf("open rename source parent: %w", err)
	}
	defer oldParent.Close()

	newParent, newName, err := openRenameParent(root, newRel)
	if err != nil {
		return fmt.Errorf("open rename destination parent: %w", err)
	}
	defer newParent.Close()

	if err := unix.Renameat2(int(oldParent.Fd()), oldName, int(newParent.Fd()), newName, unix.RENAME_NOREPLACE); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("%w: %w", ErrConflict, fs.ErrExist)
		}
		if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EOPNOTSUPP) {
			return fmt.Errorf("%w: %w", ErrNoReplaceUnsupported, err)
		}
		return err
	}
	return nil
}

func openRenameParent(root *Root, rel string) (*os.File, string, error) {
	parent := path.Dir(rel)
	if parent == "." {
		parent = ""
	}
	file, err := openRootDirectoryNoFollow(root.root, parent)
	if err != nil {
		return nil, "", err
	}
	return file, path.Base(rel), nil
}

func atomicWriteSupported() bool { return true }
