//go:build linux

package fsroot

import (
	"errors"
	"fmt"
	"io/fs"
	"path"

	"golang.org/x/sys/unix"
)

// platformUpdateMetadata 只对固定的 O_PATH 句柄执行权限或属主修改。
func platformUpdateMetadata(root *Root, rel string, expected Fingerprint, update MetadataUpdate) (Entry, error) {
	if err := root.verifyRootPath(); err != nil {
		return Entry{}, err
	}
	parentPath := path.Dir(rel)
	if parentPath == "." {
		parentPath = ""
	}
	parent, err := openRootDirectoryNoFollow(root.root, parentPath)
	if err != nil {
		return Entry{}, fmt.Errorf("open metadata parent: %w", err)
	}
	defer parent.Close()
	parentStat, err := snapshotStatxFD(int(parent.Fd()))
	if err != nil {
		return Entry{}, fmt.Errorf("inspect metadata parent: %w", err)
	}

	name := path.Base(rel)
	identity, err := openWalkIdentity(parent, name)
	if err != nil {
		return Entry{}, fmt.Errorf("open metadata target: %w", err)
	}
	defer identity.Close()
	before, err := snapshotStatxFD(int(identity.Fd()))
	if err != nil {
		return Entry{}, fmt.Errorf("inspect metadata target: %w", err)
	}
	if before.Mask&unix.STATX_UID == 0 || before.Mask&unix.STATX_GID == 0 {
		return Entry{}, ErrIdentityUnavailable
	}
	if before.Mode&unix.S_IFMT == unix.S_IFLNK {
		return Entry{}, ErrSymlinkPath
	}
	if before.Mode&unix.S_IFMT != unix.S_IFREG && before.Mode&unix.S_IFMT != unix.S_IFDIR {
		return Entry{}, ErrNotRegularFile
	}
	if !metadataMatchesFingerprint(before, expected) {
		return Entry{}, ErrConflict
	}
	pathBefore, err := snapshotStatxAt(int(parent.Fd()), name, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil || !sameSnapshotStat(before, pathBefore) {
		return Entry{}, ErrPathChanged
	}
	if err := verifyMetadataParent(root, parentPath, parentStat); err != nil {
		return Entry{}, err
	}

	if update.Mode != nil {
		err = unix.Fchmodat(int(identity.Fd()), "", uint32(*update.Mode), unix.AT_EMPTY_PATH)
		if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EINVAL) {
			return Entry{}, fmt.Errorf("chmod by pinned descriptor: %w", ErrMetadataUnsupported)
		}
	} else {
		uid, gid := -1, -1
		maxID := uint64(^uint(0) >> 1)
		if update.OwnerUID != nil {
			if uint64(*update.OwnerUID) > maxID {
				return Entry{}, fs.ErrInvalid
			}
			uid = int(*update.OwnerUID)
		}
		if update.GroupGID != nil {
			if uint64(*update.GroupGID) > maxID {
				return Entry{}, fs.ErrInvalid
			}
			gid = int(*update.GroupGID)
		}
		err = unix.Fchownat(int(identity.Fd()), "", uid, gid, unix.AT_EMPTY_PATH)
	}
	if err != nil {
		if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
			return Entry{}, fmt.Errorf("update file metadata: %w: %w", fs.ErrPermission, err)
		}
		return Entry{}, fmt.Errorf("update file metadata: %w", err)
	}

	after, err := snapshotStatxFD(int(identity.Fd()))
	if err != nil {
		return Entry{}, fmt.Errorf("%w: inspect updated metadata: %w", ErrMetadataNeedsReview, err)
	}
	if !sameSnapshotObject(before, after) {
		return Entry{}, fmt.Errorf("%w: %w", ErrMetadataNeedsReview, ErrPathChanged)
	}
	pathAfter, err := snapshotStatxAt(int(parent.Fd()), name, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil || !sameSnapshotObject(after, pathAfter) {
		return Entry{}, fmt.Errorf("%w: %w", ErrMetadataNeedsReview, ErrPathChanged)
	}
	if err := verifyMetadataParent(root, parentPath, parentStat); err != nil {
		return Entry{}, fmt.Errorf("%w: %w", ErrMetadataNeedsReview, err)
	}
	if err := root.verifyRootPath(); err != nil {
		return Entry{}, fmt.Errorf("%w: %w", ErrMetadataNeedsReview, err)
	}

	info, err := identity.Stat()
	if err != nil {
		return Entry{}, fmt.Errorf("%w: read updated file metadata: %w", ErrMetadataNeedsReview, err)
	}
	entry := entryFromInfo(name, rel, info)
	entry.ModifiedAt = unixTime(after.Mtime.Sec, after.Mtime.Nsec)
	entry.ChangedAt = unixTime(after.Ctime.Sec, after.Ctime.Nsec)
	entry.ChangeTimeAvailable = true
	entry.Device = statxDevice(after)
	entry.Inode = after.Ino
	entry.Links = uint64(after.Nlink)
	entry.IdentityAvailable = true
	entry.OwnerUID = after.Uid
	entry.GroupGID = after.Gid
	entry.OwnershipAvailable = true
	return entry, nil
}

func metadataMatchesFingerprint(current unix.Statx_t, expected Fingerprint) bool {
	kind := KindOther
	switch current.Mode & unix.S_IFMT {
	case unix.S_IFREG:
		kind = KindFile
	case unix.S_IFDIR:
		kind = KindDirectory
	}
	return expected.Kind == kind &&
		expected.Device == statxDevice(current) && expected.Inode == current.Ino &&
		expected.Links == uint64(current.Nlink) &&
		expected.Size == int64(current.Size) &&
		expected.Mode.Perm() == fs.FileMode(current.Mode&0o777) &&
		expected.ModifiedAt.Equal(unixTime(current.Mtime.Sec, current.Mtime.Nsec)) &&
		expected.ChangeTime.Equal(unixTime(current.Ctime.Sec, current.Ctime.Nsec)) &&
		expected.OwnerUID == current.Uid && expected.GroupGID == current.Gid
}

func verifyMetadataParent(root *Root, relativePath string, expected unix.Statx_t) error {
	current, err := openRootDirectoryNoFollow(root.root, relativePath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, ErrSymlinkPath) || errors.Is(err, ErrNotDirectory) {
			return ErrPathChanged
		}
		return err
	}
	defer current.Close()
	stat, err := snapshotStatxFD(int(current.Fd()))
	if err != nil || !sameSnapshotObject(expected, stat) {
		return ErrPathChanged
	}
	return nil
}
