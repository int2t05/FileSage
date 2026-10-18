//go:build !linux

// 本文件验证非 Linux 平台的显式不支持语义。
package fsroot

import (
	"context"
	"errors"
	"testing"
)

func TestSnapshotTreeUnsupportedPlatform(t *testing.T) {
	root, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, err := root.SnapshotTree(context.Background(), ""); !errors.Is(err, ErrWalkUnsupported) {
		t.Fatalf("SnapshotTree() error = %v, want ErrWalkUnsupported", err)
	}
	if err := root.VerifyTreeSnapshot(context.Background(), TreeSnapshot{}); !errors.Is(err, ErrWalkUnsupported) {
		t.Fatalf("VerifyTreeSnapshot() error = %v, want ErrWalkUnsupported", err)
	}
}
