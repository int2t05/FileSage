//go:build !linux

package fsroot

import "io/fs"

func platformRenameNoReplace(_ *Root, _, _ string) error {
	return ErrNoReplaceUnsupported
}

func atomicWriteSupported() bool { return false }

func platformReplaceAtomicIfUnchanged(_ *Root, _, _ string, _ Fingerprint, _, _ [32]byte, _ int64, _ fs.FileMode) error {
	return ErrAtomicWriteUnsupported
}
