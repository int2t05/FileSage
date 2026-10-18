//go:build linux

// 本文件读取 Linux inode 的状态变更时间。
package fsroot

import (
	"io/fs"
	"syscall"
	"time"
)

// fileChangeTime 返回 Linux 文件状态变更时间。
func fileChangeTime(info fs.FileInfo) (time.Time, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return time.Time{}, false
	}
	return time.Unix(stat.Ctim.Sec, stat.Ctim.Nsec).UTC(), true
}
