//go:build !linux

// runner_other.go 在非 Linux 平台明确禁用 Shell 执行。
package shellstage

import (
	"context"
	"fmt"
)

// platformCheckAvailability 在非 Linux 平台明确关闭 Shell 沙箱。
func platformCheckAvailability(context.Context) error {
	return fmt.Errorf("%w: Bubblewrap runner is Linux-only", ErrUnavailable)
}

// platformExecute 在非 Linux 平台拒绝执行，不会回退到宿主 Shell。
func platformExecute(context.Context, Request) (Result, error) {
	return Result{}, fmt.Errorf("%w: Bubblewrap runner is Linux-only", ErrUnavailable)
}
