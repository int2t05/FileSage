//go:build !linux

// 本文件明确拒绝当前未实现的安全单文件复制。
package fsroot

import (
	"context"
)

// platformCopyFileNoReplace 在未实现安全发布的平台上明确失败。
func platformCopyFileNoReplace(context.Context, *Root, string, string, Fingerprint) (FileCopyResult, error) {
	return FileCopyResult{}, ErrAtomicWriteUnsupported
}

// platformRemoveFileIfUnchanged 在未实现安全删除的平台上明确失败。
func platformRemoveFileIfUnchanged(*Root, string, Fingerprint) (bool, error) {
	return false, ErrAtomicWriteUnsupported
}
