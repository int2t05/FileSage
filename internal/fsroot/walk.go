package fsroot

import "errors"

// ErrWalkUnsupported 表示当前平台没有安全遍历实现。
var ErrWalkUnsupported = errors.New("safe filesystem walk unsupported")
