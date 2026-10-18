//go:build !linux

// 本文件在无法保证禁止覆盖时拒绝流式发布。
package fsroot

import (
	"io"
	"io/fs"
)

func platformWriteNewAtomicFrom(_ *Root, _ string, _ io.Reader, _ int64, _ fs.FileMode) (string, error) {
	return "", ErrNoReplaceUnsupported
}
