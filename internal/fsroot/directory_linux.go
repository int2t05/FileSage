//go:build linux

package fsroot

import "syscall"

func directoryOpenFlag() int { return syscall.O_DIRECTORY }
