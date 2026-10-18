//go:build !unix

package fsroot

import "io/fs"

func fileIdentity(fs.FileInfo) (device, inode, links uint64, available bool) {
	return 0, 0, 0, false
}

func fileOwnership(fs.FileInfo) (ownerUID, groupGID uint32, available bool) {
	return 0, 0, false
}

func nonblockingOpenFlag() int { return 0 }
