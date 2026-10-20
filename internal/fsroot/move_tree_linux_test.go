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

// TestMoveTreeNoReplaceDetectsMutationDuringCrossMountCopy 验证跨挂载复制期间的来源变化不会发布目标。
func TestMoveTreeNoReplaceDetectsMutationDuringCrossMountCopy(t *testing.T) {
	root, sourceBase, destinationBase := openCrossMountMoveRoots(t)
	defer root.Close()
	sourcePath := filepath.Join(sourceBase, "source")
	if err := os.Mkdir(sourcePath, 0o700); err != nil {
		t.Fatal(err)
	}
	sourceFile := filepath.Join(sourcePath, "file")
	if err := os.WriteFile(sourceFile, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := strings.TrimPrefix(filepath.ToSlash(sourcePath), "/")
	destination := strings.TrimPrefix(filepath.ToSlash(filepath.Join(destinationBase, "moved")), "/")
	snapshot, err := root.SnapshotTree(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	ctx := &treeCopyHookContext{Context: context.Background(), parent: destinationBase, triggerAt: 4, hook: func() error {
		return os.WriteFile(sourceFile, []byte("mutated!"), 0o600)
	}}
	result, err := root.MoveTreeNoReplace(ctx, source, destination, snapshot)
	if !ctx.triggered || !errors.Is(err, ErrConflict) {
		t.Fatalf("cross-mount move after source mutation = (%+v, %v), want triggered conflict", result, err)
	}
	if result.Published || result.SourceRemoved {
		t.Fatalf("conflicted move result = %+v, want no published target or source removal", result)
	}
	assertMoveFileContents(t, sourceFile, "mutated!")
	if _, err := os.Lstat(filepath.Join(destinationBase, "moved")); !os.IsNotExist(err) {
		t.Fatalf("conflicted move published a target: %v", err)
	}
	assertNoTreeCopyTemps(t, destinationBase)
}

// TestMoveTreeNoReplacePreservesMutationAfterCopy 验证发布副本后来源变化时保留两份文件。
func TestMoveTreeNoReplacePreservesMutationAfterCopy(t *testing.T) {
	root, sourceBase, destinationBase := openCrossMountMoveRoots(t)
	defer root.Close()
	sourcePath := filepath.Join(sourceBase, "source")
	if err := os.Mkdir(sourcePath, 0o700); err != nil {
		t.Fatal(err)
	}
	sourceFile := filepath.Join(sourcePath, "file")
	if err := os.WriteFile(sourceFile, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := strings.TrimPrefix(filepath.ToSlash(sourcePath), "/")
	destinationPath := filepath.Join(destinationBase, "moved")
	destination := strings.TrimPrefix(filepath.ToSlash(destinationPath), "/")
	snapshot, err := root.SnapshotTree(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	ctx := &moveTreePublicationHookContext{Context: context.Background(), destination: destinationPath, hook: func() error {
		return os.WriteFile(sourceFile, []byte("mutated!"), 0o600)
	}}
	result, err := root.MoveTreeNoReplace(ctx, source, destination, snapshot)
	if !ctx.triggered || !errors.Is(err, ErrConflict) {
		t.Fatalf("cross-mount move after publication = (%+v, %v), want triggered conflict", result, err)
	}
	if !result.Published || result.SourceRemoved {
		t.Fatalf("conflicted move result = %+v, want published copy and retained source", result)
	}
	assertMoveFileContents(t, sourceFile, "mutated!")
	assertMoveFileContents(t, filepath.Join(destinationPath, "file"), "original")
}

// TestMoveTreeNoReplaceRestoresMutationDuringQuarantineCheck 验证隔离复核发现来源变化后恢复来源路径。
func TestMoveTreeNoReplaceRestoresMutationDuringQuarantineCheck(t *testing.T) {
	root, sourceBase, destinationBase := openCrossMountMoveRoots(t)
	defer root.Close()
	sourcePath := filepath.Join(sourceBase, "source")
	if err := os.Mkdir(sourcePath, 0o700); err != nil {
		t.Fatal(err)
	}
	sourceFile := filepath.Join(sourcePath, "file")
	if err := os.WriteFile(sourceFile, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := strings.TrimPrefix(filepath.ToSlash(sourcePath), "/")
	destinationPath := filepath.Join(destinationBase, "moved")
	destination := strings.TrimPrefix(filepath.ToSlash(destinationPath), "/")
	snapshot, err := root.SnapshotTree(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	ctx := &moveTreeQuarantineHookContext{Context: context.Background(), source: sourcePath, parent: sourceBase, hook: func(quarantine string) error {
		return os.WriteFile(filepath.Join(quarantine, "file"), []byte("mutated!"), 0o600)
	}}
	result, err := root.MoveTreeNoReplace(ctx, source, destination, snapshot)
	if !ctx.triggered || !errors.Is(err, ErrConflict) {
		t.Fatalf("cross-mount move after quarantine mutation = (%+v, %v), want triggered conflict", result, err)
	}
	if !result.Published || result.SourceRemoved || result.SourceRetained != "" {
		t.Fatalf("conflicted move result = %+v, want published copy and restored source", result)
	}
	assertMoveFileContents(t, sourceFile, "mutated!")
	assertMoveFileContents(t, filepath.Join(destinationPath, "file"), "original")
	entries, err := os.ReadDir(sourceBase)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".filesage-move-quarantine-") {
			t.Fatalf("quarantine path remains after conflict recovery: %q", entry.Name())
		}
	}
}

// openCrossMountMoveRoots 在 /tmp 与 /dev/shm 创建真实跨挂载测试目录。
func openCrossMountMoveRoots(t *testing.T) (*Root, string, string) {
	t.Helper()
	root, err := Open("/")
	if err != nil {
		t.Skipf("open filesystem root: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })
	sourceBase, err := os.MkdirTemp("/tmp", "filesage-tree-move-source-")
	if err != nil {
		t.Skipf("create source fixture: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sourceBase) })
	destinationBase, err := os.MkdirTemp("/dev/shm", "filesage-tree-move-target-")
	if err != nil {
		t.Skipf("create destination fixture: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(destinationBase) })
	sourceMountID, err := root.MountID(strings.TrimPrefix(filepath.ToSlash(sourceBase), "/"))
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
	return root, sourceBase, destinationBase
}

// moveTreePublicationHookContext 在跨挂载副本发布后触发来源变化。
type moveTreePublicationHookContext struct {
	context.Context
	destination string
	hook        func() error
	triggered   bool
}

// Err 在公开目标出现后执行竞态钩子。
func (ctx *moveTreePublicationHookContext) Err() error {
	if !ctx.triggered {
		if _, err := os.Lstat(ctx.destination); err == nil {
			ctx.triggered = true
			if err := ctx.hook(); err != nil {
				return err
			}
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	return ctx.Context.Err()
}

// moveTreeQuarantineHookContext 在来源被隔离后触发内容变化。
type moveTreeQuarantineHookContext struct {
	context.Context
	source    string
	parent    string
	hook      func(string) error
	triggered bool
}

// Err 在隔离目录出现后执行竞态钩子。
func (ctx *moveTreeQuarantineHookContext) Err() error {
	if !ctx.triggered {
		if _, err := os.Lstat(ctx.source); os.IsNotExist(err) {
			entries, readErr := os.ReadDir(ctx.parent)
			if readErr != nil {
				return readErr
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".filesage-move-quarantine-") {
					ctx.triggered = true
					if err := ctx.hook(filepath.Join(ctx.parent, entry.Name())); err != nil {
						return err
					}
					break
				}
			}
		} else if err != nil {
			return err
		}
	}
	return ctx.Context.Err()
}

// assertMoveFileContents 核对移动后文件的实际内容。
func assertMoveFileContents(t *testing.T, name, want string) {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil || string(data) != want {
		t.Fatalf("file contents = (%q, %v), want %q", data, err, want)
	}
}
