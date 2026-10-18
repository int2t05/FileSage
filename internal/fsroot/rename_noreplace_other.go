//go:build !linux

package fsroot

func platformRenameNoReplace(_ *Root, _, _ string) error {
	return ErrNoReplaceUnsupported
}

func atomicWriteSupported() bool { return false }
