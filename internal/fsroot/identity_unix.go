//go:build unix

package fsroot

import (
	"io/fs"
	"syscall"
)

func fileIdentity(info fs.FileInfo) (device, inode, links uint64, available bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return 0, 0, 0, false
	}
	return uint64(stat.Dev), uint64(stat.Ino), uint64(stat.Nlink), true
}

func fileOwnership(info fs.FileInfo) (ownerUID, groupGID uint32, available bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return 0, 0, false
	}
	return stat.Uid, stat.Gid, true
}

func nonblockingOpenFlag() int { return syscall.O_NONBLOCK }
