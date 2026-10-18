// 本文件验证软链接创建的路径约束和不覆盖行为。
package fsroot

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSymlinkNoReplaceCreatesFileAndDirectoryLinks(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootPath, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rootPath, "sorted", "refs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "assets", "manual.pdf"), []byte("manual"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	requireSymlinkCreation(t, root)

	for _, test := range []struct {
		name       string
		target     string
		link       string
		wantTarget string
		wantKind   Kind
	}{
		{name: "root file", target: "assets/manual.pdf", link: "manual-link.pdf", wantTarget: "assets/manual.pdf", wantKind: KindFile},
		{name: "nested file", target: "assets/manual.pdf", link: "sorted/refs/manual-link.pdf", wantTarget: "../../assets/manual.pdf", wantKind: KindFile},
		{name: "directory", target: "assets", link: "sorted/assets-link", wantTarget: "../assets", wantKind: KindDirectory},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := root.SymlinkNoReplace(test.target, test.link); err != nil {
				if runtime.GOOS == "windows" && errors.Is(err, os.ErrPermission) {
					t.Skipf("symbolic links are unavailable: %v", err)
				}
				t.Fatal(err)
			}
			gotTarget, err := os.Readlink(filepath.Join(rootPath, filepath.FromSlash(test.link)))
			if err != nil {
				t.Fatal(err)
			}
			if gotTarget != test.wantTarget {
				t.Fatalf("link target = %q, want %q", gotTarget, test.wantTarget)
			}
			entry, err := root.LstatEntry(test.link)
			if err != nil {
				t.Fatal(err)
			}
			if !entry.Symlink || entry.Kind != test.wantKind || entry.LinkStatus != LinkStatusInternal {
				t.Fatalf("link entry = %+v", entry)
			}
		})
	}
}

// requireSymlinkCreation 在当前平台确认软链接创建权限。
func requireSymlinkCreation(t *testing.T, root *Root) {
	t.Helper()
	err := root.SymlinkNoReplace("assets/manual.pdf", "symlink-support-check")
	if err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symbolic links are unavailable: %v", err)
		}
		t.Fatal(err)
	}
	if err := root.Remove("symlink-support-check"); err != nil {
		t.Fatal(err)
	}
}

func TestSymlinkNoReplaceNeverOverwrites(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "source"), []byte("source"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "existing"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	err = root.SymlinkNoReplace("source", "existing")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("SymlinkNoReplace() error = %v, want ErrConflict", err)
	}
	contents, err := os.ReadFile(filepath.Join(rootPath, "existing"))
	if err != nil || string(contents) != "keep" {
		t.Fatalf("existing file changed: contents=%q err=%v", contents, err)
	}
}

func TestSymlinkNoReplaceRejectsInvalidTargets(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "inside"), []byte("safe"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	for _, target := range []string{"", ".", "../outside", "/etc/passwd", filepath.Join(rootPath, "inside"), `folder\\inside`} {
		if err := root.SymlinkNoReplace(target, "link"); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("target %q: error = %v, want ErrInvalidPath", target, err)
		}
	}
	if err := root.SymlinkNoReplace("missing", "link"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing target error = %v, want fs.ErrNotExist", err)
	}
}

func TestSymlinkNoReplaceRejectsDanglingCyclicAndEscapingTargets(t *testing.T) {
	rootPath := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	links := []struct{ name, target string }{
		{"dangling", "missing"},
		{"cycle-a", "cycle-b"},
		{"cycle-b", "cycle-a"},
		{"outside", outside},
	}
	for _, link := range links {
		if err := os.Symlink(link.target, filepath.Join(rootPath, link.name)); err != nil {
			if runtime.GOOS == "windows" {
				t.Skipf("symbolic links are unavailable: %v", err)
			}
			t.Fatal(err)
		}
	}
	root, err := Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	for _, test := range []struct {
		target string
		want   error
	}{
		{target: "dangling", want: ErrDanglingLink},
		{target: "cycle-a", want: ErrLinkCycle},
		{target: "outside", want: ErrOutsideRoot},
	} {
		if err := root.SymlinkNoReplace(test.target, "new-link"); !errors.Is(err, test.want) {
			t.Errorf("target %q: error = %v, want %v", test.target, err, test.want)
		}
	}
}

func TestSymlinkNoReplaceRejectsSymlinkedParent(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "target"), []byte("safe"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(rootPath, "alias")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symbolic links are unavailable: %v", err)
		}
		t.Fatal(err)
	}
	root, err := Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	if err := root.SymlinkNoReplace("target", "alias/new-link"); !errors.Is(err, ErrSymlinkPath) {
		t.Fatalf("symlink parent error = %v, want ErrSymlinkPath", err)
	}
	if _, err := os.Lstat(filepath.Join(rootPath, "real", "new-link")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("link was created through symlinked parent: %v", err)
	}
}
