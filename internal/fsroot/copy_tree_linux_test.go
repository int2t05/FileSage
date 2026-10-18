//go:build linux

// Linux 目录树复制与并发冲突测试。
package fsroot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// TestCopyTreeNoReplaceCopiesAndVerifiesTree 验证目录内容和空目录完整复制。
func TestCopyTreeNoReplaceCopiesAndVerifiesTree(t *testing.T) {
	base := t.TempDir()
	sourcePath := filepath.Join(base, "source")
	if err := os.MkdirAll(filepath.Join(sourcePath, "nested", "empty"), 0o750); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"root.txt":          []byte("root contents"),
		"nested/data.bin":   []byte(strings.Repeat("abc123", 4096)),
		"nested/empty-file": {},
	}
	for relative, data := range files {
		if err := os.WriteFile(filepath.Join(sourcePath, filepath.FromSlash(relative)), data, 0o640); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(base, "copies"), 0o750); err != nil {
		t.Fatal(err)
	}
	root, err := Open(base)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	snapshot, err := root.SnapshotTree(context.Background(), "source")
	if err != nil {
		t.Fatal(err)
	}

	result, err := root.CopyTreeNoReplace(context.Background(), "source", "copies/clone", snapshot)
	if err != nil {
		t.Fatalf("CopyTreeNoReplace() error = %v", err)
	}
	if !result.Published || result.FilesCopied != len(files) {
		t.Fatalf("CopyTreeNoReplace() result = %+v", result)
	}
	var expectedBytes int64
	for relative, expected := range files {
		actual, err := os.ReadFile(filepath.Join(base, "copies", "clone", filepath.FromSlash(relative)))
		if err != nil {
			t.Fatalf("read copied %q: %v", relative, err)
		}
		if string(actual) != string(expected) {
			t.Errorf("copied %q differs from source", relative)
		}
		expectedBytes += int64(len(expected))
	}
	if result.BytesCopied != expectedBytes {
		t.Errorf("BytesCopied = %d, want %d", result.BytesCopied, expectedBytes)
	}
	if len(result.FileDigests) != len(files) {
		t.Errorf("FileDigests has %d entries, want %d", len(result.FileDigests), len(files))
	}
	if info, err := os.Stat(filepath.Join(base, "copies", "clone", "nested", "empty")); err != nil || !info.IsDir() {
		t.Errorf("empty directory was not copied: info=%v err=%v", info, err)
	}
	if err := root.VerifyTreeSnapshot(context.Background(), snapshot); err != nil {
		t.Errorf("copy changed source tree: %v", err)
	}
}

// TestCopyTreeNoReplacePreservesReadOnlyDirectoryModes 验证只读目录权限不会被放宽。
func TestCopyTreeNoReplacePreservesReadOnlyDirectoryModes(t *testing.T) {
	base := t.TempDir()
	sourcePath := filepath.Join(base, "source")
	nestedPath := filepath.Join(sourcePath, "nested")
	if err := os.MkdirAll(nestedPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nestedPath, "file"), []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(nestedPath, 0o555); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sourcePath, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(sourcePath, 0o700)
	defer os.Chmod(nestedPath, 0o700)
	if err := os.Mkdir(filepath.Join(base, "copies"), 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := Open(base)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	snapshot, err := root.SnapshotTree(context.Background(), "source")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := root.CopyTreeNoReplace(context.Background(), "source", "copies/clone", snapshot); err != nil {
		t.Fatalf("CopyTreeNoReplace() error = %v", err)
	}
	for _, relative := range []string{"copies/clone", "copies/clone/nested"} {
		info, err := os.Stat(filepath.Join(base, filepath.FromSlash(relative)))
		if err != nil || info.Mode().Perm() != 0o555 {
			t.Errorf("directory %q mode = (%v, %v), want 0555", relative, info, err)
		}
	}
	defer os.Chmod(filepath.Join(base, "copies", "clone"), 0o700)
	defer os.Chmod(filepath.Join(base, "copies", "clone", "nested"), 0o700)
}

// TestCleanupTreeCopyStagingRemovesVerifiedOrphans 只回收带身份标记的中断暂存项。
func TestCleanupTreeCopyStagingRemovesVerifiedOrphans(t *testing.T) {
	base := t.TempDir()
	root, err := Open(base)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	nonce := strings.Repeat("a", 24)
	name := treeCopyTempPrefix + nonce
	stagePath := filepath.Join(base, name)
	if err := os.Mkdir(stagePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stagePath, "partial"), []byte("interrupted"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(stagePath, "sealed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stagePath, "sealed", "partial"), []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	stageFD, err := unix.Open(stagePath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	stageStat, err := snapshotStatxFD(stageFD)
	_ = unix.Close(stageFD)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(stagePath, "sealed"), 0); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stagePath, 0); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, name+".lock"), []byte(treeCopyActiveMarker(nonce, stageStat)), 0o600); err != nil {
		t.Fatal(err)
	}
	unmarkedName := treeCopyTempPrefix + strings.Repeat("b", 24)
	if err := os.Mkdir(filepath.Join(base, unmarkedName), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, unmarkedName, "keep"), []byte("user data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := root.CleanupTreeCopyStaging(""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(stagePath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("verified orphan remains: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(base, name+".lock")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("orphan marker remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, unmarkedName, "keep")); err != nil {
		t.Errorf("unmarked user data was removed: %v", err)
	}
}

// TestCleanupTreeCopyStagingRejectsActiveCopies 验证活动复制会阻止清理和恢复。
func TestCleanupTreeCopyStagingRejectsActiveCopies(t *testing.T) {
	base := t.TempDir()
	root, err := Open(base)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	nonce := strings.Repeat("c", 24)
	name := treeCopyTempPrefix + nonce
	stagePath := filepath.Join(base, name)
	if err := os.Mkdir(stagePath, 0o700); err != nil {
		t.Fatal(err)
	}
	stageFD, err := unix.Open(stagePath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	stageStat, err := snapshotStatxFD(stageFD)
	_ = unix.Close(stageFD)
	if err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(base, name+".lock")
	if err := os.WriteFile(markerPath, []byte(treeCopyActiveMarker(nonce, stageStat)), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(markerPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	if err := root.CleanupTreeCopyStaging(""); !errors.Is(err, ErrTreeCopyInProgress) {
		t.Fatalf("CleanupTreeCopyStaging() error = %v, want active-copy error", err)
	}
	if _, err := os.Stat(stagePath); err != nil {
		t.Errorf("active staging directory was removed: %v", err)
	}
}

// TestCopyTreeNoReplaceRejectsUnsafeEntries 验证链接和特殊文件会阻止复制。
func TestCopyTreeNoReplaceRejectsUnsafeEntries(t *testing.T) {
	for _, test := range []struct {
		name string
		add  func(string) error
	}{
		{name: "symbolic link", add: func(path string) error { return os.Symlink("file.txt", path) }},
		{name: "fifo", add: func(path string) error { return unix.Mkfifo(path, 0o600) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := t.TempDir()
			sourcePath := filepath.Join(base, "source")
			if err := os.Mkdir(sourcePath, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := test.add(filepath.Join(sourcePath, "unsafe")); err != nil {
				t.Fatal(err)
			}
			root, err := Open(base)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			snapshot, err := root.SnapshotTree(context.Background(), "source")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := root.CopyTreeNoReplace(context.Background(), "source", "copy", snapshot); !errors.Is(err, ErrTreeCopyUnsafeEntry) {
				t.Fatalf("CopyTreeNoReplace() error = %v, want ErrTreeCopyUnsafeEntry", err)
			}
			if _, err := os.Lstat(filepath.Join(base, "copy")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("unsafe copy published a target: %v", err)
			}
			assertNoTreeCopyTemps(t, base)
		})
	}
}

// TestCopyTreeNoReplaceRejectsSymbolicLinkRoot 验证来源根不能是软链接。
func TestCopyTreeNoReplaceRejectsSymbolicLinkRoot(t *testing.T) {
	base := t.TempDir()
	if err := os.Mkdir(filepath.Join(base, "source"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("source", filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}
	root, err := Open(base)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	snapshot, err := root.SnapshotTree(context.Background(), "link")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := root.CopyTreeNoReplace(context.Background(), "link", "copy", snapshot); !errors.Is(err, ErrTreeCopyUnsafeEntry) {
		t.Fatalf("symbolic link root error = %v, want ErrTreeCopyUnsafeEntry", err)
	}
}

// TestCopyTreeNoReplaceRejectsExistingAndNestedTargets 验证不覆盖目标且不复制进自身。
func TestCopyTreeNoReplaceRejectsExistingAndNestedTargets(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "source", "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "source", "child", "file"), []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(base, "existing"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "existing", "marker"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := Open(base)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	snapshot, err := root.SnapshotTree(context.Background(), "source")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := root.CopyTreeNoReplace(context.Background(), "source", "existing", snapshot); !errors.Is(err, ErrConflict) {
		t.Fatalf("existing target error = %v, want ErrConflict", err)
	}
	marker, err := os.ReadFile(filepath.Join(base, "existing", "marker"))
	if err != nil || string(marker) != "keep" {
		t.Fatalf("existing target was changed: data=%q err=%v", marker, err)
	}
	if _, err := root.CopyTreeNoReplace(context.Background(), "source", "source/copy", snapshot); !errors.Is(err, ErrTreeCopyIntoSource) {
		t.Fatalf("nested target error = %v, want ErrTreeCopyIntoSource", err)
	}
	assertNoTreeCopyTemps(t, base)
}

// TestCopyTreeNoReplaceDetectsSourceConflictAndCleansStaging 验证过期快照不会发布副本。
func TestCopyTreeNoReplaceDetectsSourceConflictAndCleansStaging(t *testing.T) {
	base := t.TempDir()
	if err := os.Mkdir(filepath.Join(base, "source"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "source", "file"), []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := Open(base)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	snapshot, err := root.SnapshotTree(context.Background(), "source")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "source", "file"), []byte("after!"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := root.CopyTreeNoReplace(context.Background(), "source", "copy", snapshot); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale snapshot error = %v, want ErrConflict", err)
	}
	if _, err := os.Lstat(filepath.Join(base, "copy")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stale copy published a target: %v", err)
	}
	assertNoTreeCopyTemps(t, base)
}

// TestCopyTreeNoReplaceDetectsMutationDuringCopy 验证复制期间的源修改会使操作冲突。
func TestCopyTreeNoReplaceDetectsMutationDuringCopy(t *testing.T) {
	base := t.TempDir()
	if err := os.Mkdir(filepath.Join(base, "source"), 0o700); err != nil {
		t.Fatal(err)
	}
	filePath := filepath.Join(base, "source", "file")
	if err := os.WriteFile(filePath, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := Open(base)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	snapshot, err := root.SnapshotTree(context.Background(), "source")
	if err != nil {
		t.Fatal(err)
	}
	ctx := &treeCopyHookContext{Context: context.Background(), parent: base, triggerAt: 4, hook: func() error {
		return os.WriteFile(filePath, []byte("mutated!"), 0o600)
	}}
	if _, err := root.CopyTreeNoReplace(ctx, "source", "copy", snapshot); !errors.Is(err, ErrConflict) {
		t.Fatalf("copy after source mutation error = %v, want ErrConflict", err)
	}
	if _, err := os.Lstat(filepath.Join(base, "copy")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("conflicted copy published a target: %v", err)
	}
	assertNoTreeCopyTemps(t, base)
}

// TestCopyTreeNoReplaceDoesNotReplaceRacingTarget 验证并发创建的目标不会被替换。
func TestCopyTreeNoReplaceDoesNotReplaceRacingTarget(t *testing.T) {
	base := t.TempDir()
	if err := os.Mkdir(filepath.Join(base, "source"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "source", "file"), []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := Open(base)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	snapshot, err := root.SnapshotTree(context.Background(), "source")
	if err != nil {
		t.Fatal(err)
	}
	ctx := &treeCopyHookContext{Context: context.Background(), parent: base, triggerAt: 4, hook: func() error {
		target := filepath.Join(base, "copy")
		if err := os.Mkdir(target, 0o700); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(target, "marker"), []byte("keep"), 0o600)
	}}
	if _, err := root.CopyTreeNoReplace(ctx, "source", "copy", snapshot); !errors.Is(err, ErrConflict) {
		t.Fatalf("racing target error = %v, want ErrConflict", err)
	}
	marker, err := os.ReadFile(filepath.Join(base, "copy", "marker"))
	if err != nil || string(marker) != "keep" {
		t.Fatalf("racing target was changed: data=%q err=%v", marker, err)
	}
	assertNoTreeCopyTemps(t, base)
}

// TestCopyTreeNoReplaceCancellationCleansStaging 验证取消后清理未发布暂存目录。
func TestCopyTreeNoReplaceCancellationCleansStaging(t *testing.T) {
	base := t.TempDir()
	if err := os.Mkdir(filepath.Join(base, "source"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "source", "file"), []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := Open(base)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	snapshot, err := root.SnapshotTree(context.Background(), "source")
	if err != nil {
		t.Fatal(err)
	}
	baseCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &treeCopyHookContext{Context: baseCtx, parent: base, triggerAt: 4, hook: func() error {
		cancel()
		return context.Canceled
	}}
	if _, err := root.CopyTreeNoReplace(ctx, "source", "copy", snapshot); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled copy error = %v, want context.Canceled", err)
	}
	if _, err := os.Lstat(filepath.Join(base, "copy")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("cancelled copy published a target: %v", err)
	}
	assertNoTreeCopyTemps(t, base)
}

// treeCopyHookContext 在暂存目录出现后触发竞态或取消。
type treeCopyHookContext struct {
	context.Context
	parent    string
	triggerAt int
	calls     int
	triggered bool
	hook      func() error
}

// Err 在测试指定的检查点执行竞态钩子。
func (ctx *treeCopyHookContext) Err() error {
	if !ctx.triggered && treeCopyTempExists(ctx.parent) {
		ctx.calls++
		if ctx.calls == ctx.triggerAt {
			ctx.triggered = true
			if err := ctx.hook(); err != nil {
				return err
			}
		}
	}
	return ctx.Context.Err()
}

// treeCopyTempExists 检查测试目录中是否存在复制暂存目录。
func treeCopyTempExists(parent string) bool {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".filesage-tree-copy-") {
			return true
		}
	}
	return false
}

// assertNoTreeCopyTemps 验证失败路径清除了本次复制的暂存目录。
func assertNoTreeCopyTemps(t *testing.T, parent string) {
	t.Helper()
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".filesage-tree-copy-") {
			t.Errorf("staging directory was not cleaned: %s", entry.Name())
		}
	}
}

// TestCopyTreeNoReplaceRejectsNilAndCancelledContext 验证无效或已取消上下文会被拒绝。
func TestCopyTreeNoReplaceRejectsNilAndCancelledContext(t *testing.T) {
	base := t.TempDir()
	if err := os.Mkdir(filepath.Join(base, "source"), 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := Open(base)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	snapshot, err := root.SnapshotTree(context.Background(), "source")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := root.CopyTreeNoReplace(nil, "source", "copy", snapshot); !errors.Is(err, os.ErrInvalid) {
		t.Fatalf("nil context error = %v, want fs.ErrInvalid", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := root.CopyTreeNoReplace(cancelled, "source", "copy", snapshot); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled context error = %v, want context.Canceled", err)
	}
}
