//go:build !linux

// 本文件明确拒绝当前未实现的安全单文件复制。
package fsroot

import (
	"context"
)

// platformCopyFileNoReplace 在未实现安全发布的平台上明确失败。
func platformCopyFileNoReplace(context.Context, *Root, string, string, Fingerprint, string) (FileCopyResult, error) {
	return FileCopyResult{}, ErrAtomicWriteUnsupported
}

// platformMoveFileSameMountNoReplace 在不支持安全无覆盖改名的平台上明确失败。
func platformMoveFileSameMountNoReplace(context.Context, *Root, string, string, Fingerprint, string) (FileMoveResult, error) {
	return FileMoveResult{}, ErrNoReplaceUnsupported
}

// platformRemoveFileIfUnchanged 在未实现安全删除的平台上明确失败。
func platformRemoveFileIfUnchanged(context.Context, *Root, string, string, Fingerprint, string) (bool, string, error) {
	return false, "", ErrAtomicWriteUnsupported
}
