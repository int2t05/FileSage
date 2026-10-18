//go:build !linux

package fsroot

import "context"

// Watch 在未实现安全目录监听的平台上明确失败。
func (r *Root) Watch(context.Context, func()) error {
	return ErrWatchUnsupported
}
