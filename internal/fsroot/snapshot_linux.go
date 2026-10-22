//go:build linux

// 本文件通过目录句柄建立不跟随软链接的元数据快照。
package fsroot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"

	"golang.org/x/sys/unix"
)

const maxSnapshotLinkTarget = 1 << 20

// snapshotNode 保留扫描期间用于复核的条目身份。
type snapshotNode struct {
	entry    Entry
	stat     unix.Statx_t
	identity *os.File
}

// platformSnapshotSupported 确认当前实现提供 Linux 安全扫描。
func platformSnapshotSupported() error { return nil }

// platformSnapshotTree 只通过已打开的目录句柄递归扫描所选子树。
func platformSnapshotTree(ctx context.Context, root *Root, relativePath string, limits *TreeSnapshotLimits) (TreeSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return TreeSnapshot{}, err
	}
	if err := root.verifyRootPath(); err != nil {
		return TreeSnapshot{}, fmt.Errorf("verify snapshot root: %w", err)
	}
	snapshot := TreeSnapshot{RootPath: relativePath, Entries: make([]TreeSnapshotEntry, 0)}
	budget := snapshotBudget{entries: &snapshot.Entries, limits: limits}
	if relativePath == "" {
		directory, err := root.root.Open(".")
		if err != nil {
			return TreeSnapshot{}, fmt.Errorf("open snapshot root: %w", err)
		}
		defer directory.Close()
		stat, err := snapshotStatxFD(int(directory.Fd()))
		if err != nil {
			return TreeSnapshot{}, fmt.Errorf("inspect snapshot root: %w", err)
		}
		info, err := directory.Stat()
		if err != nil {
			return TreeSnapshot{}, fmt.Errorf("stat snapshot root: %w", err)
		}
		entry, err := snapshotEntry(".", "", info, stat)
		if err != nil {
			return TreeSnapshot{}, err
		}
		root.applyReadOnly(&entry, "")
		rootNode := snapshotNode{entry: entry, stat: stat}
		if err := budget.append(snapshotItem(entry, stat.Mnt_id)); err != nil {
			return TreeSnapshot{}, err
		}
		if err := snapshotDirectory(ctx, root, directory, &rootNode, &budget); err != nil {
			return TreeSnapshot{}, err
		}
		finalStat, err := snapshotStatxFD(int(directory.Fd()))
		if err != nil || !sameSnapshotStat(stat, finalStat) {
			return TreeSnapshot{}, ErrPathChanged
		}
	} else {
		parentPath := path.Dir(relativePath)
		if parentPath == "." {
			parentPath = ""
		}
		parent, err := openRootDirectoryNoFollow(root.root, parentPath)
		if err != nil {
			return TreeSnapshot{}, fmt.Errorf("open snapshot parent: %w", err)
		}
		defer parent.Close()
		name := path.Base(relativePath)
		node, err := snapshotNodeAt(ctx, parent, name, relativePath)
		if err != nil {
			return TreeSnapshot{}, err
		}
		root.applyReadOnly(&node.entry, relativePath)
		defer node.identity.Close()
		if err := budget.append(snapshotItem(node.entry, node.stat.Mnt_id)); err != nil {
			return TreeSnapshot{}, err
		}
		if node.entry.Kind == KindDirectory {
			directory, err := openWalkDirectory(parent, name)
			if err != nil {
				return TreeSnapshot{}, fmt.Errorf("open snapshot directory %q: %w", relativePath, err)
			}
			openedStat, statErr := snapshotStatxFD(int(directory.Fd()))
			if statErr != nil || !sameSnapshotObject(node.stat, openedStat) {
				_ = directory.Close()
				return TreeSnapshot{}, ErrPathChanged
			}
			walkErr := snapshotDirectory(ctx, root, directory, &node, &budget)
			closeErr := directory.Close()
			if walkErr != nil {
				return TreeSnapshot{}, walkErr
			}
			if closeErr != nil {
				return TreeSnapshot{}, fmt.Errorf("close snapshot directory %q: %w", relativePath, closeErr)
			}
		}
		if err := verifySnapshotNode(parent, name, relativePath, &node); err != nil {
			return TreeSnapshot{}, err
		}
		if err := verifySnapshotParent(root, parentPath, parent); err != nil {
			return TreeSnapshot{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return TreeSnapshot{}, err
	}
	if err := root.verifyRootPath(); err != nil {
		return TreeSnapshot{}, fmt.Errorf("verify snapshot root after scan: %w", err)
	}
	sort.Slice(snapshot.Entries, func(i, j int) bool { return snapshot.Entries[i].Entry.Path < snapshot.Entries[j].Entry.Path })
	snapshot.Digest = digestTreeSnapshot(snapshot.RootPath, snapshot.Entries)
	return snapshot, nil
}

// verifySnapshotParent 重开根相对父目录并确认句柄仍指向同一对象。
func verifySnapshotParent(root *Root, relativePath string, opened *os.File) error {
	current, err := openRootDirectoryNoFollow(root.root, relativePath)
	if err != nil {
		if errors.Is(err, ErrSymlinkPath) || errors.Is(err, ErrNotDirectory) || errors.Is(err, fs.ErrNotExist) {
			return ErrPathChanged
		}
		return err
	}
	defer current.Close()
	openedStat, err := snapshotStatxFD(int(opened.Fd()))
	if err != nil {
		return err
	}
	currentStat, err := snapshotStatxFD(int(current.Fd()))
	if err != nil {
		return err
	}
	if !sameSnapshotObject(openedStat, currentStat) {
		return ErrPathChanged
	}
	return nil
}

// snapshotDirectory 复核目录身份、元数据和完整目录项清单。
type snapshotBudget struct {
	entries   *[]TreeSnapshotEntry
	limits    *TreeSnapshotLimits
	totalSize int64
	totalPath int
}

// append 累计资源用量后再保存快照项。
func (b *snapshotBudget) append(item TreeSnapshotEntry) error {
	if b.limits != nil {
		if len(*b.entries) >= b.limits.MaxEntries || len(item.Entry.Path) > b.limits.MaxPathBytes-b.totalPath {
			return ErrSnapshotLimitExceeded
		}
		if item.Entry.Kind == KindFile {
			if item.Entry.Size < 0 || item.Entry.Size > b.limits.MaxBytes-b.totalSize {
				return ErrSnapshotLimitExceeded
			}
			b.totalSize += item.Entry.Size
		}
		b.totalPath += len(item.Entry.Path)
	}
	*b.entries = append(*b.entries, item)
	return nil
}

// remainingEntries 返回目录清单可继续读取的数量；负数表示不限制。
func (b *snapshotBudget) remainingEntries() int {
	if b.limits == nil {
		return -1
	}
	return b.limits.MaxEntries - len(*b.entries)
}

func snapshotDirectory(ctx context.Context, root *Root, directory *os.File, node *snapshotNode, budget *snapshotBudget) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := root.verifyWalkVolume(directory, node.entry.Path); err != nil {
		return err
	}
	initial, err := snapshotStatxFD(int(directory.Fd()))
	if err != nil {
		return fmt.Errorf("inspect directory %q: %w", node.entry.Path, err)
	}
	if initial.Mode&unix.S_IFMT != unix.S_IFDIR || !sameSnapshotObject(node.stat, initial) {
		return fmt.Errorf("directory %q: %w", node.entry.Path, ErrPathChanged)
	}
	initialNames, err := snapshotDirectoryNames(directory, budget.remainingEntries())
	if err != nil {
		return fmt.Errorf("read directory %q: %w", node.entry.Path, err)
	}
	for _, name := range initialNames {
		if err := ctx.Err(); err != nil {
			return err
		}
		childPath := path.Join(node.entry.Path, name)
		volume, isMountRoot := root.registeredMountAt(childPath)
		unavailable := false
		if isMountRoot {
			if err := root.checkRegisteredMount(volume); errors.Is(err, ErrVolumeUnavailable) {
				unavailable = true
			} else if err != nil {
				return fmt.Errorf("inspect mounted volume %q: %w", childPath, err)
			}
		}
		child, err := snapshotNodeAt(ctx, directory, name, childPath)
		if err != nil {
			if isMountRoot && unavailable && errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return err
		}
		root.applyReadOnly(&child.entry, childPath)
		if isMountRoot && (unavailable || child.stat.Mnt_id != volume.id) {
			markUnavailableVolume(&child.entry)
			unavailable = true
		}
		if err := budget.append(snapshotItem(child.entry, child.stat.Mnt_id)); err != nil {
			_ = child.identity.Close()
			return err
		}
		if child.entry.Kind == KindDirectory && !unavailable {
			childDirectory, err := openWalkDirectory(directory, name)
			if err != nil {
				_ = child.identity.Close()
				return fmt.Errorf("open snapshot directory %q: %w", childPath, err)
			}
			openedStat, statErr := snapshotStatxFD(int(childDirectory.Fd()))
			if statErr != nil || !sameSnapshotObject(child.stat, openedStat) {
				_ = childDirectory.Close()
				_ = child.identity.Close()
				return fmt.Errorf("directory %q: %w", childPath, ErrPathChanged)
			}
			walkErr := snapshotDirectory(ctx, root, childDirectory, &child, budget)
			closeErr := childDirectory.Close()
			if walkErr != nil {
				_ = child.identity.Close()
				return walkErr
			}
			if closeErr != nil {
				_ = child.identity.Close()
				return fmt.Errorf("close snapshot directory %q: %w", childPath, closeErr)
			}
		}
		if err := verifySnapshotNode(directory, name, childPath, &child); err != nil {
			_ = child.identity.Close()
			return err
		}
		if err := child.identity.Close(); err != nil {
			return fmt.Errorf("close snapshot entry %q: %w", childPath, err)
		}
	}
	finalLimit := budget.remainingEntries()
	if budget.limits != nil {
		finalLimit = budget.remainingEntries() + len(initialNames)
	}
	finalNames, err := snapshotDirectoryNames(directory, finalLimit)
	if err != nil {
		return fmt.Errorf("recheck directory entries %q: %w", node.entry.Path, err)
	}
	if !sameNames(initialNames, finalNames) {
		return fmt.Errorf("directory entries %q: %w", node.entry.Path, ErrPathChanged)
	}
	final, err := snapshotStatxFD(int(directory.Fd()))
	if err != nil {
		return fmt.Errorf("recheck directory %q: %w", node.entry.Path, err)
	}
	if !sameSnapshotStat(initial, final) {
		return fmt.Errorf("directory %q: %w", node.entry.Path, ErrPathChanged)
	}
	return ctx.Err()
}

// snapshotNodeAt 通过父目录句柄读取单项元数据，并保持身份句柄打开。
func snapshotNodeAt(ctx context.Context, parent *os.File, name, relativePath string) (snapshotNode, error) {
	if err := ctx.Err(); err != nil {
		return snapshotNode{}, err
	}
	identity, err := openWalkIdentity(parent, name)
	if err != nil {
		return snapshotNode{}, fmt.Errorf("open entry %q: %w", relativePath, err)
	}
	stat, err := snapshotStatxAt(int(parent.Fd()), name, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		_ = identity.Close()
		return snapshotNode{}, fmt.Errorf("stat entry %q: %w", relativePath, err)
	}
	fdStat, err := snapshotStatxFD(int(identity.Fd()))
	if err != nil {
		_ = identity.Close()
		return snapshotNode{}, fmt.Errorf("inspect entry %q: %w", relativePath, err)
	}
	if !sameSnapshotStat(stat, fdStat) {
		_ = identity.Close()
		return snapshotNode{}, fmt.Errorf("entry %q: %w", relativePath, ErrPathChanged)
	}
	info, err := identity.Stat()
	if err != nil {
		_ = identity.Close()
		return snapshotNode{}, fmt.Errorf("stat entry %q: %w", relativePath, err)
	}
	entry, err := snapshotEntry(name, relativePath, info, stat)
	if err != nil {
		_ = identity.Close()
		return snapshotNode{}, err
	}
	if entry.Symlink {
		target, err := readlinkAt(int(parent.Fd()), name)
		if err != nil {
			_ = identity.Close()
			return snapshotNode{}, fmt.Errorf("read symbolic link %q: %w", relativePath, err)
		}
		entry.LinkTarget = target
	}
	return snapshotNode{entry: entry, stat: stat, identity: identity}, nil
}

// verifySnapshotNode 确认路径仍指向初次扫描的对象且元数据未变。
func verifySnapshotNode(parent *os.File, name, relativePath string, node *snapshotNode) error {
	fdStat, err := snapshotStatxFD(int(node.identity.Fd()))
	if err != nil {
		return fmt.Errorf("recheck entry %q: %w", relativePath, err)
	}
	pathStat, err := snapshotStatxAt(int(parent.Fd()), name, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		return fmt.Errorf("recheck entry %q: %w", relativePath, ErrPathChanged)
	}
	if !sameSnapshotStat(node.stat, fdStat) || !sameSnapshotStat(node.stat, pathStat) {
		return fmt.Errorf("recheck entry %q: %w", relativePath, ErrPathChanged)
	}
	if node.entry.Symlink {
		target, err := readlinkAt(int(parent.Fd()), name)
		if err != nil || target != node.entry.LinkTarget {
			return fmt.Errorf("recheck symbolic link %q: %w", relativePath, ErrPathChanged)
		}
	}
	return nil
}

// snapshotEntry 将 statx 身份与文件句柄元数据合并为快照条目。
func snapshotEntry(name, relativePath string, info os.FileInfo, stat unix.Statx_t) (Entry, error) {
	entry := entryFromInfo(name, relativePath, info)
	if !entry.IdentityAvailable || stat.Mask&unix.STATX_CTIME == 0 || stat.Mask&unix.STATX_MNT_ID == 0 || stat.Mnt_id == 0 || stat.Mask&unix.STATX_INO == 0 || stat.Ino == 0 {
		return Entry{}, ErrIdentityUnavailable
	}
	entry.ModifiedAt = unixTime(stat.Mtime.Sec, stat.Mtime.Nsec)
	entry.ChangedAt = unixTime(stat.Ctime.Sec, stat.Ctime.Nsec)
	entry.ChangeTimeAvailable = true
	entry.Device = statxDevice(stat)
	entry.Inode = stat.Ino
	entry.Links = uint64(stat.Nlink)
	entry.IdentityAvailable = true
	entry.OwnerUID = stat.Uid
	entry.GroupGID = stat.Gid
	entry.OwnershipAvailable = stat.Mask&unix.STATX_UID != 0 && stat.Mask&unix.STATX_GID != 0
	if info.Mode()&os.ModeSymlink != 0 {
		entry.Symlink = true
		entry.LinkDevice = entry.Device
		entry.LinkInode = entry.Inode
		entry.LinkLinks = entry.Links
		entry.LinkIdentityAvailable = true
	}
	return entry, nil
}

// snapshotDirectoryNames 用新句柄读取并排序直接子项。
func snapshotDirectoryNames(directory *os.File, maxNames int) ([]string, error) {
	fd, err := unix.Openat(int(directory.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	listing := os.NewFile(uintptr(fd), "snapshot-directory")
	names := make([]string, 0)
	for {
		batchSize := 128
		if maxNames >= 0 && maxNames-len(names) < batchSize {
			batchSize = maxNames - len(names) + 1
			if batchSize < 1 {
				batchSize = 1
			}
		}
		children, readErr := listing.ReadDir(batchSize)
		for _, child := range children {
			if child.Name() == TrashDirectoryName {
				continue
			}
			names = append(names, child.Name())
			if maxNames >= 0 && len(names) > maxNames {
				_ = listing.Close()
				return nil, ErrSnapshotLimitExceeded
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				_ = listing.Close()
				return nil, readErr
			}
			break
		}
		if len(children) == 0 {
			break
		}
	}
	closeErr := listing.Close()
	if closeErr != nil {
		return nil, closeErr
	}
	sort.Strings(names)
	return names, nil
}

// sameNames 比较目录前后两次读取到的名称清单。
func sameNames(first, second []string) bool {
	if len(first) != len(second) {
		return false
	}
	for index := range first {
		if first[index] != second[index] {
			return false
		}
	}
	return true
}

// readlinkAt 从父目录句柄读取软链接原始目标，不解析目标路径。
func readlinkAt(dirfd int, name string) (string, error) {
	for size := 256; size <= maxSnapshotLinkTarget; size *= 2 {
		buffer := make([]byte, size)
		count, err := unix.Readlinkat(dirfd, name, buffer)
		if err != nil {
			return "", err
		}
		if count < len(buffer) {
			return string(buffer[:count]), nil
		}
	}
	return "", fmt.Errorf("symbolic link target exceeds snapshot limit")
}

// snapshotStatxAt 要求路径项提供身份、ctime、mtime 和挂载 ID。
func snapshotStatxAt(dirfd int, name string, flags int) (unix.Statx_t, error) {
	stat, err := statxAt(dirfd, name, flags)
	if err != nil {
		return stat, err
	}
	if stat.Mask&unix.STATX_CTIME == 0 || stat.Mask&unix.STATX_MTIME == 0 || stat.Ino == 0 {
		return stat, ErrIdentityUnavailable
	}
	return stat, nil
}

// snapshotStatxFD 从已打开对象句柄读取相同的安全元数据。
func snapshotStatxFD(fd int) (unix.Statx_t, error) {
	return snapshotStatxAt(fd, "", unix.AT_EMPTY_PATH)
}

// sameSnapshotObject 比较对象身份、类型和挂载位置。
func sameSnapshotObject(first, second unix.Statx_t) bool {
	return first.Mnt_id == second.Mnt_id && first.Ino == second.Ino && first.Dev_major == second.Dev_major && first.Dev_minor == second.Dev_minor && first.Mode&unix.S_IFMT == second.Mode&unix.S_IFMT
}

// sameSnapshotStat 比较快照绑定的身份与状态字段。
func sameSnapshotStat(first, second unix.Statx_t) bool {
	return sameSnapshotObject(first, second) && first.Mode == second.Mode && first.Size == second.Size && first.Nlink == second.Nlink && first.Uid == second.Uid && first.Gid == second.Gid && first.Mtime.Sec == second.Mtime.Sec && first.Mtime.Nsec == second.Mtime.Nsec && first.Ctime.Sec == second.Ctime.Sec && first.Ctime.Nsec == second.Ctime.Nsec
}
