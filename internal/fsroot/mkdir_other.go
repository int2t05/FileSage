//go:build !linux

package fsroot

import "io/fs"

func platformMkdirAllNoFollow(_ *Root, _ string, _ fs.FileMode) ([]string, error) {
	return nil, ErrNoReplaceUnsupported
}
