//go:build linux

// 本文件验证 Linux 特殊文件不能作为软链接目标。
package fsroot

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestSymlinkNoReplaceRejectsSpecialFileTarget(t *testing.T) {
	rootPath := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(rootPath, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	if err := root.SymlinkNoReplace("pipe", "pipe-link"); !errors.Is(err, ErrNotRegularFile) {
		t.Fatalf("SymlinkNoReplace() error = %v, want ErrNotRegularFile", err)
	}
	if _, err := os.Lstat(filepath.Join(rootPath, "pipe-link")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("special file link was created: %v", err)
	}
}
