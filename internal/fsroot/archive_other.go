//go:build !linux

package fsroot

import (
	"context"
	"io"
)

// WriteArchive 在未实现安全目录句柄遍历的平台上明确失败。
func (r *Root) WriteArchive(context.Context, []string, io.Writer, ArchiveLimits) error {
	return ErrArchiveUnsupported
}

// WriteArchiveFormat 在未实现安全目录句柄遍历的平台上明确失败。
func (r *Root) WriteArchiveFormat(context.Context, []string, io.Writer, ArchiveLimits, ArchiveFormat) error {
	return ErrArchiveUnsupported
}

// ValidateArchiveFormat 在未实现安全遍历的平台明确拒绝归档创建。
func ValidateArchiveFormat(ArchiveFormat) error { return ErrArchiveUnsupported }
