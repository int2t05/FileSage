//go:build !linux

package fsroot

func directoryOpenFlag() int { return 0 }
