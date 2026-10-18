//go:build !linux

package fsroot

import (
	"context"
	"io"
)

// Walk 在未实现安全目录句柄遍历的平台上明确失败。
func (r *Root) Walk(context.Context, func(Entry) error) error {
	return ErrWalkUnsupported
}

// WalkDirectory 在未实现安全目录句柄遍历的平台上明确失败。
func (r *Root) WalkDirectory(context.Context, string, func(Entry) error) error {
	return ErrWalkUnsupported
}

// WalkExcluding 在未实现安全目录句柄遍历的平台上明确失败。
func (r *Root) WalkExcluding(context.Context, string, string, func(Entry) error) error {
	return ErrWalkUnsupported
}

// WalkDirectoryExcluding 在未实现安全目录句柄遍历的平台上明确失败。
func (r *Root) WalkDirectoryExcluding(context.Context, string, string, func(Entry) error) error {
	return ErrWalkUnsupported
}

// OpenReadNoFollow 在未实现安全目录句柄读取的平台上明确失败。
func (r *Root) OpenReadNoFollow(string) (io.ReadCloser, Entry, error) {
	return nil, Entry{}, ErrWalkUnsupported
}
