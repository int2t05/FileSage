//go:build linux

package fsroot

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// 验证重命名打开父目录时不跟随软链接。
func TestOpenRenameParentRejectsSymlink(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(rootPath, "alias")); err != nil {
		t.Fatal(err)
	}
	root, err := Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })

	parent, _, err := openRenameParent(root, "alias/source.txt")
	if parent != nil {
		_ = parent.Close()
		t.Fatal("symlink parent was opened")
	}
	if !errors.Is(err, ErrSymlinkPath) {
		t.Fatalf("openRenameParent() error = %v, want ErrSymlinkPath", err)
	}
}
