// 本文件定义安全打包的限制和错误。
package fsroot

import (
	"errors"
)

// ArchiveFormat 是安全归档支持的输出格式。
type ArchiveFormat string

const (
	ArchiveFormatZIP   ArchiveFormat = "zip"
	ArchiveFormat7Z    ArchiveFormat = "7z"
	ArchiveFormatTAR   ArchiveFormat = "tar"
	ArchiveFormatTARGZ ArchiveFormat = "tar.gz"
)

var (
	ErrArchiveUnsupported   = errors.New("safe archive traversal unsupported")
	ErrArchiveUnsafeEntry   = errors.New("archive contains a symbolic link or special file")
	ErrArchiveLimitExceeded = errors.New("archive safety limit exceeded")
	ErrArchiveStorage       = errors.New("archive temporary storage failed")
	ErrArchiveDependency    = errors.New("7-Zip is not installed")
	ErrArchiveCommand       = errors.New("7-Zip archive creation failed")
)

// ArchiveLimits 限制打包期间的条目数和源文件总大小。
type ArchiveLimits struct {
	MaxEntries int
	MaxBytes   int64
}
