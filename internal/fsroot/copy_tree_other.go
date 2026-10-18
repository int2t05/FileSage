//go:build !linux

// 本文件明确声明非 Linux 平台不提供安全目录树复制。
package fsroot

import "context"

func platformCopyTreeNoReplace(context.Context, *Root, string, string, TreeSnapshot) (TreeCopyResult, error) {
	return TreeCopyResult{}, ErrWalkUnsupported
}

func platformCleanupTreeCopyStaging(*Root, string) error {
	return ErrWalkUnsupported
}
