//go:build linux

package fsroot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestWalkSkipsTrashAndDoesNotFollowLinks(t *testing.T) {
	base := t.TempDir()
	rootPath := filepath.Join(base, "managed")
	outside := filepath.Join(base, "outside")
	for _, directory := range []string{
		filepath.Join(rootPath, "nested", "deep"),
		filepath.Join(rootPath, TrashDirectoryName),
		filepath.Join(rootPath, "nested", TrashDirectoryName),
		outside,
	} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, contents := range map[string]string{
		filepath.Join(rootPath, "root.txt"):                                 "root",
		filepath.Join(rootPath, "nested", "deep", "inside.txt"):             "inside",
		filepath.Join(rootPath, TrashDirectoryName, "hidden.txt"):           "hidden",
		filepath.Join(rootPath, "nested", TrashDirectoryName, "hidden.txt"): "hidden",
		filepath.Join(outside, "secret.txt"):                                "secret",
	} {
		if err := os.WriteFile(name, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for name, target := range map[string]string{
		"internal-file": "root.txt",
		"internal-dir":  "nested",
		"external-dir":  outside,
		"dangling":      "missing.txt",
		"cycle-a":       "cycle-b",
		"cycle-b":       "cycle-a",
	} {
		if err := os.Symlink(target, filepath.Join(rootPath, name)); err != nil {
			t.Skipf("symlink creation unavailable: %v", err)
		}
	}

	root := openWalkTestRoot(t, rootPath)
	entries := make(map[string]Entry)
	if err := root.Walk(context.Background(), func(entry Entry) error {
		entries[entry.Path] = entry
		return nil
	}); err != nil {
		t.Fatalf("Walk() error = %v", err)
	}

	for _, included := range []string{"root.txt", "nested", "nested/deep", "nested/deep/inside.txt"} {
		if _, ok := entries[included]; !ok {
			t.Errorf("Walk() omitted %q", included)
		}
	}
	for _, omitted := range []string{
		TrashDirectoryName,
		TrashDirectoryName + "/hidden.txt",
		"nested/" + TrashDirectoryName,
		"nested/" + TrashDirectoryName + "/hidden.txt",
		"internal-dir/deep",
		"internal-dir/deep/inside.txt",
		"external-dir/secret.txt",
		"cycle-a/anything",
	} {
		if _, ok := entries[omitted]; ok {
			t.Errorf("Walk() followed or included excluded path %q", omitted)
		}
	}
	for _, link := range []string{"internal-file", "internal-dir", "external-dir", "dangling", "cycle-a", "cycle-b"} {
		entry, ok := entries[link]
		if !ok || !entry.Symlink || entry.Kind != KindOther || !entry.LinkIdentityAvailable {
			t.Errorf("Walk() link %q = %+v, present=%t; want link metadata only", link, entry, ok)
		}
	}
}

func TestWalkExcludingSkipsCategorySubtreeAndKeepsOtherRootFiles(t *testing.T) {
	rootPath := t.TempDir()
	for _, directory := range []string{"source/nested", "categories/Work"} {
		if err := os.MkdirAll(filepath.Join(rootPath, directory), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range map[string]string{
		"source/nested/a.md":    "source",
		"categories/root.md":    "category root",
		"categories/Work/b.txt": "category child",
	} {
		if err := os.WriteFile(filepath.Join(rootPath, filepath.FromSlash(name)), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root := openWalkTestRoot(t, rootPath)
	var walked []string
	if err := root.WalkExcluding(context.Background(), "", "categories", func(entry Entry) error {
		walked = append(walked, entry.Path)
		return nil
	}); err != nil {
		t.Fatalf("WalkExcluding() error = %v", err)
	}
	for _, want := range []string{"source", "source/nested", "source/nested/a.md"} {
		if !containsPath(walked, want) {
			t.Errorf("WalkExcluding() omitted %q from %v", want, walked)
		}
	}
	for _, excluded := range []string{"categories", "categories/root.md", "categories/Work", "categories/Work/b.txt"} {
		if containsPath(walked, excluded) {
			t.Errorf("WalkExcluding() included excluded path %q", excluded)
		}
	}
}

func containsPath(paths []string, want string) bool {
	for _, path := range paths {
		if path == want {
			return true
		}
	}
	return false
}

func TestWalkDirectoryRejectsSymlinkScopeAndListLimitedRejectsPartialResult(t *testing.T) {
	rootPath := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(rootPath, "linked")); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "a.txt"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "b.txt"), []byte("b"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := openWalkTestRoot(t, rootPath)
	if err := root.WalkDirectory(context.Background(), "linked", func(Entry) error { return nil }); err == nil {
		t.Fatal("WalkDirectory() followed a symlink scope")
	}
	if entries, err := root.ListLimited("", 1); !errors.Is(err, ErrEntryLimit) || entries != nil {
		t.Fatalf("ListLimited() = (%v, %v), want an all-or-nothing entry limit", entries, err)
	}
}

func TestOpenReadNoFollowRejectsFinalAndParentLinks(t *testing.T) {
	rootPath := t.TempDir()
	outside := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "linked-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "real.txt"), []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.txt", filepath.Join(rootPath, "linked.txt")); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(rootPath, "linked-dir", "nested")); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	root := openWalkTestRoot(t, rootPath)
	for _, name := range []string{"linked.txt", "linked-dir/nested/secret.txt"} {
		reader, _, err := root.OpenReadNoFollow(name)
		if reader != nil {
			_ = reader.Close()
		}
		if err == nil {
			t.Errorf("OpenReadNoFollow(%q) followed a symbolic link", name)
		}
	}
}

func TestWalkCancellationStopsTraversal(t *testing.T) {
	rootPath := t.TempDir()
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		if err := os.WriteFile(filepath.Join(rootPath, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root := openWalkTestRoot(t, rootPath)
	ctx, cancel := context.WithCancel(context.Background())
	visited := 0
	err := root.Walk(ctx, func(Entry) error {
		visited++
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Walk() error = %v, want context.Canceled", err)
	}
	if visited != 1 {
		t.Fatalf("Walk() visited %d entries after cancellation, want 1", visited)
	}
}

func TestWalkRejectsDirectoryReplacementDuringVisit(t *testing.T) {
	rootPath := t.TempDir()
	directoryPath := filepath.Join(rootPath, "replace")
	outsidePath := filepath.Join(t.TempDir(), "outside")
	if err := os.Mkdir(directoryPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directoryPath, "inside.txt"), []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(outsidePath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outsidePath, "secret.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := openWalkTestRoot(t, rootPath)
	var visited []string
	err := root.Walk(context.Background(), func(entry Entry) error {
		visited = append(visited, entry.Path)
		if entry.Path == "replace" {
			if err := os.Rename(directoryPath, filepath.Join(rootPath, "moved")); err != nil {
				return err
			}
			if err := os.Symlink(outsidePath, directoryPath); err != nil {
				return err
			}
		}
		return nil
	})
	if !errors.Is(err, ErrPathChanged) {
		t.Fatalf("Walk() error = %v, want ErrPathChanged", err)
	}
	for _, name := range visited {
		if name == "replace/secret.txt" {
			t.Fatalf("Walk() followed a replacement symlink: %v", visited)
		}
	}
}

func TestWalkRejectsDirectoryMutationDuringScan(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "first.txt"), []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := openWalkTestRoot(t, rootPath)
	mutated := false
	err := root.Walk(context.Background(), func(entry Entry) error {
		if !mutated && entry.Path == "first.txt" {
			mutated = true
			if err := os.WriteFile(filepath.Join(rootPath, "added.txt"), []byte("added"), 0o600); err != nil {
				return err
			}
			changedTime := time.Unix(1, 0)
			return os.Chtimes(rootPath, changedTime, changedTime)
		}
		return nil
	})
	if !errors.Is(err, ErrPathChanged) {
		t.Fatalf("Walk() error = %v, want ErrPathChanged", err)
	}
}

func TestWalkCrossesNestedMountWhenAvailable(t *testing.T) {
	rootPath := t.TempDir()
	mountPath := filepath.Join(rootPath, "mounted")
	if err := os.Mkdir(mountPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mount("tmpfs", mountPath, "tmpfs", 0, "size=1m"); err != nil {
		t.Skipf("nested tmpfs mount unavailable: %v", err)
	}
	t.Cleanup(func() {
		if err := unix.Unmount(mountPath, 0); err != nil {
			t.Errorf("unmount test tmpfs: %v", err)
		}
	})
	if err := os.WriteFile(filepath.Join(mountPath, "mounted.txt"), []byte("mounted"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := openWalkTestRoot(t, rootPath)
	found := false
	if err := root.Walk(context.Background(), func(entry Entry) error {
		if entry.Path == "mounted/mounted.txt" {
			found = true
		}
		return nil
	}); err != nil {
		t.Fatalf("Walk() error = %v", err)
	}
	if !found {
		t.Fatal("Walk() did not cross a nested mount")
	}
}

func openWalkTestRoot(t *testing.T, rootPath string) *Root {
	t.Helper()
	root, err := Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}
