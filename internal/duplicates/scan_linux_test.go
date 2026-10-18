//go:build linux

package duplicates

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"filesage/internal/fsroot"
)

// TestScanLinuxFilesystemRoot 验证真实目录中的内容校验与链接边界。
func TestScanLinuxFilesystemRoot(t *testing.T) {
	base := t.TempDir()
	managed := filepath.Join(base, "managed")
	if err := os.Mkdir(managed, 0o755); err != nil {
		t.Fatal(err)
	}
	content := []byte("identical file contents")
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(managed, name), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(managed, "c.txt"), []byte("different file contents"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(managed, "a.txt"), filepath.Join(managed, "z-alias.txt")); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside.txt")
	if err := os.WriteFile(outside, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(managed, "outside-link.txt")); err != nil {
		t.Fatal(err)
	}

	root, err := fsroot.Open(managed)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	groups, err := Scan(context.Background(), root, nil, Options{Workers: 2}, nil)
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("Scan() groups = %#v, want one group", groups)
	}
	paths := []string{groups[0].Files[0].Path, groups[0].Files[1].Path}
	if want := []string{"a.txt", "b.txt"}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("duplicate paths = %v, want %v", paths, want)
	}
}
