//go:build !linux

// 非 Linux 平台目录树复制边界测试。
package fsroot

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCopyTreeNoReplaceUnsupportedPlatform 验证非 Linux 平台明确拒绝复制。
func TestCopyTreeNoReplaceUnsupportedPlatform(t *testing.T) {
	rootPath := t.TempDir()
	root, err := Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	changedAt := time.Unix(1_800_000_000, 0)
	entry := Entry{
		Name:                "source",
		Path:                "source",
		Kind:                KindDirectory,
		Mode:                fs.ModeDir | 0o700,
		ModifiedAt:          changedAt,
		ChangedAt:           changedAt,
		ChangeTimeAvailable: true,
		Device:              1,
		Inode:               2,
		IdentityAvailable:   true,
	}
	item := TreeSnapshotEntry{Entry: entry, MountID: 1, ChangedAt: changedAt, ChangeTimeAvailable: true}
	snapshot := TreeSnapshot{RootPath: "source", Entries: []TreeSnapshotEntry{item}}
	snapshot.Digest = digestTreeSnapshot(snapshot.RootPath, snapshot.Entries)
	if _, err := root.CopyTreeNoReplace(context.Background(), "source", "copy", snapshot); !errors.Is(err, ErrWalkUnsupported) {
		t.Fatalf("CopyTreeNoReplace() error = %v, want ErrWalkUnsupported", err)
	}
	if _, err := os.Lstat(filepath.Join(rootPath, "copy")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("unsupported copy created a target: %v", err)
	}
}
