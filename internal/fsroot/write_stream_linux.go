//go:build linux

// 本文件实现 Linux 下禁止覆盖的原子流式发布。
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
	"strings"

	"golang.org/x/sys/unix"
)

func platformWriteNewAtomicFrom(root *Root, rel string, source io.Reader, expectedSize int64, perm fs.FileMode) (hashHex string, err error) {
	parentRel := path.Dir(rel)
	if parentRel == "." {
		parentRel = ""
	}
	parent, err := openRootDirectoryNoFollow(root.root, parentRel)
	if err != nil {
		return "", fmt.Errorf("open destination directory: %w", err)
	}
	defer parent.Close()

	name := path.Base(rel)
	var targetStat unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &targetStat, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		return "", conflictError("create", rel, fs.ErrExist)
	} else if !errors.Is(err, unix.ENOENT) {
		return "", fmt.Errorf("inspect new file %q: %w", rel, err)
	}

	tempName, tempFile, err := createWriteTemp(parent, perm)
	if err != nil {
		return "", fmt.Errorf("create temporary file for %q: %w", rel, err)
	}
	tempExists := true
	defer func() {
		if tempFile != nil {
			if closeErr := tempFile.Close(); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("close temporary file: %w", closeErr))
			}
		}
		if tempExists {
			if removeErr := unix.Unlinkat(int(parent.Fd()), tempName, 0); removeErr != nil && !errors.Is(removeErr, unix.ENOENT) {
				err = errors.Join(err, fmt.Errorf("remove temporary file: %w", removeErr))
			}
		}
	}()

	digest := sha256.New()
	written, err := io.CopyN(io.MultiWriter(tempFile, digest), source, expectedSize)
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return "", fmt.Errorf("read %d of %d expected bytes: %w", written, expectedSize, errors.Join(ErrSizeMismatch, err))
		}
		return "", fmt.Errorf("stream data for %q: %w", rel, err)
	}
	if written != expectedSize {
		return "", fmt.Errorf("read %d of %d expected bytes: %w", written, expectedSize, ErrSizeMismatch)
	}
	if err := requireStreamEOF(source); err != nil {
		return "", fmt.Errorf("validate stream length for %q: %w", rel, err)
	}
	if err := tempFile.Chmod(perm.Perm()); err != nil {
		return "", fmt.Errorf("set temporary file mode for %q: %w", rel, err)
	}
	if err := tempFile.Sync(); err != nil {
		return "", fmt.Errorf("sync temporary file for %q: %w", rel, err)
	}
	if err := tempFile.Close(); err != nil {
		tempFile = nil
		return "", fmt.Errorf("close temporary file for %q: %w", rel, err)
	}
	tempFile = nil

	if err := unix.Renameat2(int(parent.Fd()), tempName, int(parent.Fd()), name, unix.RENAME_NOREPLACE); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return "", conflictError("create", rel, fs.ErrExist)
		}
		if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EOPNOTSUPP) {
			return "", fmt.Errorf("publish %q: %w: %w", rel, ErrNoReplaceUnsupported, err)
		}
		return "", fmt.Errorf("publish %q: %w", rel, err)
	}
	tempExists = false
	hashHex = hex.EncodeToString(digest.Sum(nil))
	if err := parent.Sync(); err != nil {
		return hashHex, fmt.Errorf("%w: %w", ErrPublishedNotDurable, err)
	}
	return hashHex, nil
}

func openRootDirectoryNoFollow(root *os.Root, rel string) (*os.File, error) {
	current, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	if rel == "" {
		return current, nil
	}
	for _, component := range strings.Split(rel, "/") {
		var before unix.Stat_t
		if err := unix.Fstatat(int(current.Fd()), component, &before, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			_ = current.Close()
			return nil, err
		}
		if before.Mode&unix.S_IFMT == unix.S_IFLNK {
			_ = current.Close()
			return nil, ErrSymlinkPath
		}
		if before.Mode&unix.S_IFMT != unix.S_IFDIR {
			_ = current.Close()
			return nil, ErrNotDirectory
		}

		nextFD, err := unix.Openat(int(current.Fd()), component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			_ = current.Close()
			if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
				return nil, ErrPathChanged
			}
			return nil, err
		}
		var after unix.Stat_t
		if err := unix.Fstat(nextFD, &after); err != nil {
			_ = unix.Close(nextFD)
			_ = current.Close()
			return nil, err
		}
		if before.Dev != after.Dev || before.Ino != after.Ino {
			_ = unix.Close(nextFD)
			_ = current.Close()
			return nil, ErrPathChanged
		}
		next := os.NewFile(uintptr(nextFD), component)
		_ = current.Close()
		current = next
	}
	return current, nil
}

func createWriteTemp(parent *os.File, perm fs.FileMode) (string, *os.File, error) {
	for range 4 {
		suffix := make([]byte, 12)
		if _, err := rand.Read(suffix); err != nil {
			return "", nil, err
		}
		name := ".filesage-write-" + hex.EncodeToString(suffix)
		fd, err := unix.Openat(int(parent.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(perm.Perm()))
		if err == nil {
			return name, os.NewFile(uintptr(fd), name), nil
		}
		if !errors.Is(err, unix.EEXIST) {
			return "", nil, err
		}
	}
	return "", nil, fs.ErrExist
}

func requireStreamEOF(source io.Reader) error {
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
