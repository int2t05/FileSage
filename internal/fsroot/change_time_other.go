//go:build !linux

// 本文件为非 Linux 构建提供状态变更时间占位实现。
package fsroot

import (
	"io/fs"
	"time"
)

// fileChangeTime 在非 Linux 系统上报告不可用，避免误认为快照完整。
func fileChangeTime(fs.FileInfo) (time.Time, bool) {
	return time.Time{}, false
}
