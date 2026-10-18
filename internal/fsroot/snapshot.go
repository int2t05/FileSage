// 本文件定义目录树快照、稳定摘要与复核入口。
package fsroot

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io/fs"
	"sort"
	"time"
)

// ErrSnapshotLimitExceeded 表示目录快照触发了资源上限。
var ErrSnapshotLimitExceeded = errors.New("tree snapshot limit exceeded")

// TreeSnapshot 描述目录树扫描结果，不包含文件内容。
type TreeSnapshot struct {
	RootPath string              `json:"rootPath"`
	Entries  []TreeSnapshotEntry `json:"entries"`
	Digest   string              `json:"digest"`
}

// TreeSnapshotEntry 保存一项的无跟随元数据与所在挂载标识。
type TreeSnapshotEntry struct {
	Entry               Entry     `json:"entry"`
	MountID             uint64    `json:"mountId"`
	ChangedAt           time.Time `json:"changedAt"`
	ChangeTimeAvailable bool      `json:"changeTimeAvailable"`
}

// TreeSnapshotLimits 限制快照条目、文件字节数和路径总长度。
type TreeSnapshotLimits struct {
	MaxEntries   int   // 最大条目数
	MaxBytes     int64 // 普通文件总字节数
	MaxPathBytes int   // 路径总字节数
}

// SnapshotTree 生成管理根相对路径的稳定元数据快照。
func (r *Root) SnapshotTree(ctx context.Context, relativePath string) (TreeSnapshot, error) {
	return r.snapshotTree(ctx, relativePath, nil)
}

// SnapshotTreeWithLimits 在扫描期间执行资源上限检查。
func (r *Root) SnapshotTreeWithLimits(ctx context.Context, relativePath string, limits TreeSnapshotLimits) (TreeSnapshot, error) {
	if limits.MaxEntries < 1 || limits.MaxBytes < 1 || limits.MaxPathBytes < 1 {
		return TreeSnapshot{}, ErrSnapshotLimitExceeded
	}
	return r.snapshotTree(ctx, relativePath, &limits)
}

func (r *Root) snapshotTree(ctx context.Context, relativePath string, limits *TreeSnapshotLimits) (TreeSnapshot, error) {
	if ctx == nil {
		return TreeSnapshot{}, fmt.Errorf("snapshot requires a context")
	}
	if r == nil || r.root == nil {
		return TreeSnapshot{}, fmt.Errorf("snapshot requires an open root")
	}
	if err := platformSnapshotSupported(); err != nil {
		return TreeSnapshot{}, err
	}
	if err := validateRelativePath(relativePath); err != nil {
		return TreeSnapshot{}, err
	}
	if hasTrashComponent(relativePath) {
		return TreeSnapshot{}, ErrInvalidPath
	}
	if err := ctx.Err(); err != nil {
		return TreeSnapshot{}, err
	}
	return platformSnapshotTree(ctx, r, relativePath, limits)
}

// VerifyTreeSnapshot 重新扫描并比对元数据快照；不一致时返回冲突。
func (r *Root) VerifyTreeSnapshot(ctx context.Context, snapshot TreeSnapshot) error {
	if ctx == nil {
		return fmt.Errorf("snapshot verification requires a context")
	}
	if r == nil || r.root == nil {
		return fmt.Errorf("snapshot verification requires an open root")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := platformSnapshotSupported(); err != nil {
		return err
	}
	var err error
	snapshot, err = prepareTreeSnapshot(snapshot)
	if err != nil {
		return err
	}
	if err := validateTreeSnapshot(snapshot); err != nil {
		return err
	}
	current, err := platformSnapshotTree(ctx, r, snapshot.RootPath, nil)
	if err != nil {
		if treeSnapshotChanged(err) {
			return ErrConflict
		}
		return err
	}
	if current.Digest != snapshot.Digest {
		return ErrConflict
	}
	return nil
}

// validateTreeSnapshot 拒绝被篡改、缺项或路径越界的快照。
func validateTreeSnapshot(snapshot TreeSnapshot) error {
	if err := validateRelativePath(snapshot.RootPath); err != nil || hasTrashComponent(snapshot.RootPath) {
		return ErrInvalidPath
	}
	if len(snapshot.Entries) == 0 {
		return ErrInvalidPath
	}
	for index, item := range snapshot.Entries {
		entry := item.Entry
		if item.MountID == 0 || !entry.IdentityAvailable || !entry.ChangeTimeAvailable || !item.ChangeTimeAvailable || !entry.ChangedAt.Equal(item.ChangedAt) || entry.Path == "." {
			return ErrInvalidPath
		}
		if validateRelativePath(entry.Path) != nil && entry.Path != "" {
			return ErrInvalidPath
		}
		if hasTrashComponent(entry.Path) || !pathWithinSnapshot(snapshot.RootPath, entry.Path) {
			return ErrInvalidPath
		}
		if entry.Symlink && (!entry.LinkIdentityAvailable || entry.LinkTarget == "") {
			return ErrInvalidPath
		}
		if index > 0 && snapshot.Entries[index-1].Entry.Path >= entry.Path {
			return ErrInvalidPath
		}
	}
	if snapshot.Entries[0].Entry.Path != snapshot.RootPath {
		return ErrInvalidPath
	}
	if digestTreeSnapshot(snapshot.RootPath, snapshot.Entries) != snapshot.Digest {
		return ErrInvalidPath
	}
	return nil
}

// prepareTreeSnapshot 恢复 JSON 不包含的服务端 ctime 字段。
func prepareTreeSnapshot(snapshot TreeSnapshot) (TreeSnapshot, error) {
	snapshot.Entries = append([]TreeSnapshotEntry(nil), snapshot.Entries...)
	for index := range snapshot.Entries {
		item := &snapshot.Entries[index]
		switch {
		case item.ChangeTimeAvailable:
			if item.Entry.ChangeTimeAvailable && !item.Entry.ChangedAt.Equal(item.ChangedAt) {
				return TreeSnapshot{}, ErrInvalidPath
			}
			item.Entry.ChangedAt = item.ChangedAt
			item.Entry.ChangeTimeAvailable = true
		case item.Entry.ChangeTimeAvailable:
			item.ChangedAt = item.Entry.ChangedAt
			item.ChangeTimeAvailable = true
		default:
			return TreeSnapshot{}, ErrInvalidPath
		}
	}
	return snapshot, nil
}

// snapshotItem 保留快照序列化所需的 ctime 副本。
func snapshotItem(entry Entry, mountID uint64) TreeSnapshotEntry {
	return TreeSnapshotEntry{
		Entry:               entry,
		MountID:             mountID,
		ChangedAt:           entry.ChangedAt,
		ChangeTimeAvailable: entry.ChangeTimeAvailable,
	}
}

// pathWithinSnapshot 检查条目是否位于所选快照根内。
func pathWithinSnapshot(rootPath, entryPath string) bool {
	if rootPath == "" {
		return entryPath == "" || validateRelativePath(entryPath) == nil
	}
	return entryPath == rootPath || len(entryPath) > len(rootPath) && entryPath[:len(rootPath)] == rootPath && entryPath[len(rootPath)] == '/'
}

// treeSnapshotChanged 判断复核扫描中的路径竞态或路径失效。
func treeSnapshotChanged(err error) bool {
	return errors.Is(err, ErrPathChanged) || errors.Is(err, ErrSymlinkPath) || errors.Is(err, ErrNotDirectory) || errors.Is(err, fs.ErrNotExist)
}

// digestTreeSnapshot 按固定字段顺序计算快照摘要，不读取文件内容。
func digestTreeSnapshot(rootPath string, entries []TreeSnapshotEntry) string {
	sorted := append([]TreeSnapshotEntry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Entry.Path < sorted[j].Entry.Path })
	digest := sha256.New()
	_, _ = digest.Write([]byte("filesage-tree-snapshot\x00v1"))
	writeSnapshotString(digest, rootPath)
	writeSnapshotUint64(digest, uint64(len(sorted)))
	for _, item := range sorted {
		entry := item.Entry
		writeSnapshotString(digest, entry.Path)
		writeSnapshotString(digest, entry.Name)
		writeSnapshotString(digest, string(entry.Kind))
		writeSnapshotString(digest, string(entry.LinkStatus))
		writeSnapshotInt64(digest, entry.Size)
		writeSnapshotUint64(digest, uint64(entry.Mode))
		writeSnapshotUint64(digest, uint64(entry.OwnerUID))
		writeSnapshotUint64(digest, uint64(entry.GroupGID))
		writeSnapshotBool(digest, entry.OwnershipAvailable)
		writeSnapshotTime(digest, entry.ModifiedAt)
		writeSnapshotTime(digest, entry.ChangedAt)
		writeSnapshotBool(digest, entry.ChangeTimeAvailable)
		writeSnapshotUint64(digest, entry.Device)
		writeSnapshotUint64(digest, entry.Inode)
		writeSnapshotUint64(digest, entry.Links)
		writeSnapshotBool(digest, entry.IdentityAvailable)
		writeSnapshotBool(digest, entry.Symlink)
		writeSnapshotString(digest, entry.LinkTarget)
		writeSnapshotUint64(digest, entry.LinkDevice)
		writeSnapshotUint64(digest, entry.LinkInode)
		writeSnapshotUint64(digest, entry.LinkLinks)
		writeSnapshotBool(digest, entry.LinkIdentityAvailable)
		writeSnapshotUint64(digest, item.MountID)
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// writeSnapshotString 以长度前缀保留文件名的原始字节。
func writeSnapshotString(destination hash.Hash, value string) {
	writeSnapshotUint64(destination, uint64(len(value)))
	_, _ = destination.Write([]byte(value))
}

// writeSnapshotUint64 写入大端无符号整数。
func writeSnapshotUint64(destination hash.Hash, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, _ = destination.Write(encoded[:])
}

// writeSnapshotInt64 按相同字节序写入有符号整数。
func writeSnapshotInt64(destination hash.Hash, value int64) {
	writeSnapshotUint64(destination, uint64(value))
}

// writeSnapshotBool 写入固定宽度布尔值。
func writeSnapshotBool(destination hash.Hash, value bool) {
	if value {
		_, _ = destination.Write([]byte{1})
		return
	}
	_, _ = destination.Write([]byte{0})
}

// writeSnapshotTime 写入 Unix 秒与纳秒。
func writeSnapshotTime(destination hash.Hash, value time.Time) {
	writeSnapshotInt64(destination, value.Unix())
	writeSnapshotUint64(destination, uint64(value.Nanosecond()))
}

// unixTime 将 statx 时间戳转换为 Go 时间。
func unixTime(seconds int64, nanoseconds uint32) time.Time {
	return time.Unix(seconds, int64(nanoseconds))
}
