//go:build linux

// 本文件验证 Linux 快照的安全边界与冲突语义。
package fsroot

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestSnapshotTreeEmptyAndStableDigest(t *testing.T) {
	root := openWalkTestRoot(t, t.TempDir())
	first, err := root.SnapshotTree(context.Background(), "")
	if err != nil {
		t.Fatalf("SnapshotTree() error = %v", err)
	}
	second, err := root.SnapshotTree(context.Background(), "")
	if err != nil {
		t.Fatalf("second SnapshotTree() error = %v", err)
	}
	if len(first.Entries) != 1 || first.Entries[0].Entry.Path != "" || first.Entries[0].Entry.Kind != KindDirectory {
		t.Fatalf("empty root entries = %+v, want root directory only", first.Entries)
	}
	if first.Digest == "" || first.Digest != second.Digest {
		t.Fatalf("snapshot digests = %q and %q, want stable non-empty digest", first.Digest, second.Digest)
	}
	if err := root.VerifyTreeSnapshot(context.Background(), first); err != nil {
		t.Fatalf("VerifyTreeSnapshot() on unchanged tree error = %v", err)
	}
	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	var restored TreeSnapshot
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}
	if err := root.VerifyTreeSnapshot(context.Background(), restored); err != nil {
		t.Fatalf("VerifyTreeSnapshot() after JSON round trip error = %v", err)
	}
}

func TestSnapshotTreeSortedAndSubtreeUsesManagedPaths(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootPath, "selected", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"z.txt", "a.txt", "selected/deep/item.txt"} {
		if err := os.WriteFile(filepath.Join(rootPath, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root := openWalkTestRoot(t, rootPath)
	snapshot, err := root.SnapshotTree(context.Background(), "selected")
	if err != nil {
		t.Fatalf("SnapshotTree() error = %v", err)
	}
	want := []string{"selected", "selected/deep", "selected/deep/item.txt"}
	if len(snapshot.Entries) != len(want) {
		t.Fatalf("entries = %+v, want paths %v", snapshot.Entries, want)
	}
	for index, entry := range snapshot.Entries {
		if entry.Entry.Path != want[index] {
			t.Fatalf("entry paths = %+v, want sorted paths %v", snapshot.Entries, want)
		}
	}
}

func TestSnapshotTreeWithLimitsStopsAndDiscardsPartialResult(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "selected"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		if err := os.WriteFile(filepath.Join(rootPath, "selected", name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root := openWalkTestRoot(t, rootPath)
	snapshot, err := root.SnapshotTreeWithLimits(context.Background(), "selected", TreeSnapshotLimits{
		MaxEntries: 2, MaxBytes: 1024, MaxPathBytes: 1024,
	})
	if !errors.Is(err, ErrSnapshotLimitExceeded) {
		t.Fatalf("SnapshotTreeWithLimits() error = %v, want limit error", err)
	}
	if len(snapshot.Entries) != 0 {
		t.Fatalf("limited snapshot returned partial entries: %+v", snapshot.Entries)
	}
}

func TestVerifyTreeSnapshotDetectsDeepChanges(t *testing.T) {
	rootPath := t.TempDir()
	deep := filepath.Join(rootPath, "one", "two", "deep.txt")
	if err := os.MkdirAll(filepath.Dir(deep), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(deep, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := openWalkTestRoot(t, rootPath)
	snapshot, err := root.SnapshotTree(context.Background(), "one")
	if err != nil {
		t.Fatalf("SnapshotTree() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "one", "two", "added.txt"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := root.VerifyTreeSnapshot(context.Background(), snapshot); !errors.Is(err, ErrConflict) {
		t.Fatalf("VerifyTreeSnapshot() error = %v, want ErrConflict", err)
	}
}

func TestVerifyTreeSnapshotDetectsDeepDeletion(t *testing.T) {
	rootPath := t.TempDir()
	deep := filepath.Join(rootPath, "one", "two", "deep.txt")
	if err := os.MkdirAll(filepath.Dir(deep), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(deep, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := openWalkTestRoot(t, rootPath)
	snapshot, err := root.SnapshotTree(context.Background(), "one")
	if err != nil {
		t.Fatalf("SnapshotTree() error = %v", err)
	}
	if err := os.Remove(deep); err != nil {
		t.Fatal(err)
	}
	if err := root.VerifyTreeSnapshot(context.Background(), snapshot); !errors.Is(err, ErrConflict) {
		t.Fatalf("VerifyTreeSnapshot() after deep deletion error = %v, want ErrConflict", err)
	}
}

type mutateOnCheck struct {
	context.Context
	checks  int
	trigger int
	mutate  func() error
	err     error
}

// Err 在选定检查点模拟父目录被移出管理根。
func (c *mutateOnCheck) Err() error {
	c.checks++
	if c.checks == c.trigger {
		c.err = c.mutate()
	}
	return c.Context.Err()
}

func TestSnapshotTreeRejectsDetachedParentDirectory(t *testing.T) {
	rootPath := t.TempDir()
	parentPath := filepath.Join(rootPath, "parent")
	if err := os.Mkdir(parentPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parentPath, "item"), []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := openWalkTestRoot(t, rootPath)
	outsidePath := filepath.Join(t.TempDir(), "moved-parent")
	ctx := &mutateOnCheck{
		Context: context.Background(),
		trigger: 3,
		mutate: func() error {
			if err := os.Rename(parentPath, outsidePath); err != nil {
				return err
			}
			if err := os.Mkdir(parentPath, 0o755); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(parentPath, "item"), []byte("replacement"), 0o600)
		},
	}
	if _, err := root.SnapshotTree(ctx, "parent/item"); !errors.Is(err, ErrPathChanged) {
		t.Fatalf("SnapshotTree() error = %v, want ErrPathChanged", err)
	}
	if ctx.err != nil {
		t.Fatalf("mutate parent: %v", ctx.err)
	}
}

func TestVerifyTreeSnapshotDetectsDeepReplacementAndMetadataChange(t *testing.T) {
	rootPath := t.TempDir()
	deep := filepath.Join(rootPath, "dir", "deep.txt")
	if err := os.Mkdir(filepath.Dir(deep), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(deep, []byte("same"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := openWalkTestRoot(t, rootPath)
	snapshot, err := root.SnapshotTree(context.Background(), "dir")
	if err != nil {
		t.Fatalf("SnapshotTree() error = %v", err)
	}
	if err := os.Chmod(deep, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := root.VerifyTreeSnapshot(context.Background(), snapshot); !errors.Is(err, ErrConflict) {
		t.Fatalf("VerifyTreeSnapshot() after chmod error = %v, want ErrConflict", err)
	}

	snapshot, err = root.SnapshotTree(context.Background(), "dir")
	if err != nil {
		t.Fatalf("second SnapshotTree() error = %v", err)
	}
	replacementDir := t.TempDir()
	replacement := filepath.Join(replacementDir, "replacement.txt")
	if err := os.WriteFile(replacement, []byte("same"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, deep); err != nil {
		t.Fatal(err)
	}
	if err := root.VerifyTreeSnapshot(context.Background(), snapshot); !errors.Is(err, ErrConflict) {
		t.Fatalf("VerifyTreeSnapshot() after replacement error = %v, want ErrConflict", err)
	}
}

func TestSnapshotTreeRecordsSymlinkIdentityAndRawTarget(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "target"), []byte("target"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target", filepath.Join(rootPath, "link")); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	root := openWalkTestRoot(t, rootPath)
	snapshot, err := root.SnapshotTree(context.Background(), "link")
	if err != nil {
		t.Fatalf("SnapshotTree() error = %v", err)
	}
	if len(snapshot.Entries) != 1 {
		t.Fatalf("symlink snapshot entries = %+v, want link only", snapshot.Entries)
	}
	entry := snapshot.Entries[0]
	if !entry.Entry.Symlink || entry.Entry.LinkTarget != "target" || !entry.Entry.IdentityAvailable || !entry.Entry.LinkIdentityAvailable || entry.MountID == 0 {
		t.Fatalf("symlink snapshot = %+v, want its own identity, raw target and mount id", entry)
	}
	if err := os.Remove(filepath.Join(rootPath, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing", filepath.Join(rootPath, "link")); err != nil {
		t.Fatal(err)
	}
	if err := root.VerifyTreeSnapshot(context.Background(), snapshot); !errors.Is(err, ErrConflict) {
		t.Fatalf("VerifyTreeSnapshot() after link replacement error = %v, want ErrConflict", err)
	}
}

type cancelAfterChecks struct {
	context.Context
	checks atomic.Int32
	limit  int32
}

// Err 在指定检查次数后模拟扫描期间取消。
func (c *cancelAfterChecks) Err() error {
	if c.checks.Add(1) >= c.limit {
		return context.Canceled
	}
	return c.Context.Err()
}

func TestSnapshotTreeHonorsCancellationDuringScan(t *testing.T) {
	rootPath := t.TempDir()
	for index := range 100 {
		name := filepath.Join(rootPath, "file-"+string(rune('a'+index%26))+"-"+string(rune('A'+index/26)))
		if err := os.WriteFile(name, []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root := openWalkTestRoot(t, rootPath)
	ctx := &cancelAfterChecks{Context: context.Background(), limit: 6}
	if _, err := root.SnapshotTree(ctx, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("SnapshotTree() error = %v, want context.Canceled", err)
	}
}
