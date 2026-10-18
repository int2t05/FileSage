//go:build linux

// 本文件验证目录树移动、跨挂载回退及来源冲突保护。
package fsroot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMoveTreeNoReplaceUsesAtomicRenameOnSameMount 验证同挂载目录使用原子无覆盖移动。
func TestMoveTreeNoReplaceUsesAtomicRenameOnSameMount(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootPath, "source", "nested", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(rootPath, "destination"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "source", "nested", "note.txt"), []byte("move tree"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	snapshot, err := root.SnapshotTree(context.Background(), "source")
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(filepath.Join(rootPath, "source"))
	if err != nil {
		t.Fatal(err)
	}

	result, err := root.MoveTreeNoReplace(context.Background(), "source", "destination/source", snapshot)
	if err != nil {
		t.Fatalf("MoveTreeNoReplace() error = %v", err)
	}
	if !result.Published || !result.SourceRemoved || !result.Atomic {
		t.Fatalf("MoveTreeNoReplace() result = %+v, want atomic completed move", result)
	}
	if _, err := os.Lstat(filepath.Join(rootPath, "source")); !os.IsNotExist(err) {
		t.Fatalf("source remains after move: %v", err)
	}
	after, err := os.Stat(filepath.Join(rootPath, "destination", "source"))
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("moved directory identity = (%v, %v), want original directory", after, err)
	}
	if _, err := os.Stat(filepath.Join(rootPath, "destination", "source", "nested", "empty")); err != nil {
		t.Fatalf("empty directory was not moved: %v", err)
	}
	assertMoveFileContents(t, filepath.Join(rootPath, "destination", "source", "nested", "note.txt"), "move tree")
}

// TestMoveTreeNoReplaceRejectsStaleSnapshotBeforeChangingPaths 验证过期快照不会移动或删除来源。
func TestMoveTreeNoReplaceRejectsStaleSnapshotBeforeChangingPaths(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootPath, "source", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(rootPath, "destination"), 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	snapshot, err := root.SnapshotTree(context.Background(), "source")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "source", "raced.txt"), []byte("new content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := root.MoveTreeNoReplace(context.Background(), "source", "destination/source", snapshot); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale move error = %v, want ErrConflict", err)
	}
	assertMoveFileContents(t, filepath.Join(rootPath, "source", "raced.txt"), "new content")
	if _, err := os.Lstat(filepath.Join(rootPath, "destination", "source")); !os.IsNotExist(err) {
		t.Fatalf("stale move created a destination: %v", err)
	}
}

// TestMoveTreeNoReplaceCopiesThenRemovesAcrossMounts 验证真实跨挂载移动先复制再移除来源。
func TestMoveTreeNoReplaceCopiesThenRemovesAcrossMounts(t *testing.T) {
	root, err := Open("/")
	if err != nil {
		t.Skipf("open filesystem root: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })
	sourceBase, err := os.MkdirTemp("/tmp", "filesage-tree-move-source-")
	if err != nil {
		t.Skipf("create source fixture: %v", err)
	}
	destinationBase, err := os.MkdirTemp("/dev/shm", "filesage-tree-move-target-")
	if err != nil {
		_ = os.RemoveAll(sourceBase)
		t.Skipf("create destination fixture: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(sourceBase)
		_ = os.RemoveAll(destinationBase)
	})
	sourcePath := filepath.Join(sourceBase, "source")
	if err := os.MkdirAll(filepath.Join(sourcePath, "nested", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourcePath, "nested", "note.txt"), []byte("cross mount"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := strings.TrimPrefix(filepath.ToSlash(sourcePath), "/")
	destination := strings.TrimPrefix(filepath.ToSlash(filepath.Join(destinationBase, "moved")), "/")
	sourceMountID, err := root.MountID(source)
	if err != nil {
		t.Skipf("read source mount identity: %v", err)
	}
	destinationMountID, err := root.MountID(strings.TrimPrefix(filepath.ToSlash(destinationBase), "/"))
	if err != nil {
		t.Skipf("read destination mount identity: %v", err)
	}
	if sourceMountID == destinationMountID {
		t.Skip("/tmp and /dev/shm are on the same mount")
	}
	snapshot, err := root.SnapshotTree(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}

	result, err := root.MoveTreeNoReplace(context.Background(), source, destination, snapshot)
	if err != nil {
		t.Fatalf("cross-mount MoveTreeNoReplace() error = %v", err)
	}
	if !result.Published || !result.SourceRemoved || result.Atomic || result.SourceRetained != "" {
		t.Fatalf("cross-mount result = %+v, want verified copy and source removal", result)
	}
	if _, err := os.Lstat(sourcePath); !os.IsNotExist(err) {
		t.Fatalf("cross-mount source remains: %v", err)
	}
	assertMoveFileContents(t, filepath.Join(destinationBase, "moved", "nested", "note.txt"), "cross mount")
	if _, err := os.Stat(filepath.Join(destinationBase, "moved", "nested", "empty")); err != nil {
		t.Fatalf("cross-mount empty directory is missing: %v", err)
	}
	remaining, err := os.ReadDir(sourceBase)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range remaining {
		if strings.HasPrefix(entry.Name(), ".filesage-move-quarantine-") {
			t.Fatalf("quarantine path remains after successful move: %q", entry.Name())
		}
	}
}

// assertMoveFileContents 核对移动后文件的实际内容。
func assertMoveFileContents(t *testing.T, name, want string) {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil || string(data) != want {
		t.Fatalf("file contents = (%q, %v), want %q", data, err, want)
	}
}
