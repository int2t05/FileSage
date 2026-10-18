//go:build linux

// 本文件验证 Agent 操作按真实 inode 协调硬链接别名。
package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"filesage/internal/coordination"
	"filesage/internal/fsroot"
	"filesage/internal/store"
)

func TestAgentFileOperationsCoordinateHardLinkAliases(t *testing.T) {
	rootPath := t.TempDir()
	firstPath := filepath.Join(rootPath, "first.txt")
	if err := os.WriteFile(firstPath, []byte("linked"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(firstPath, filepath.Join(rootPath, "alias.txt")); err != nil {
		t.Fatal(err)
	}
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	entry, err := root.LstatEntry("first.txt")
	if err != nil || !entry.IdentityAvailable {
		t.Fatalf("source identity = (%#v, %v)", entry, err)
	}
	sourceState, err := json.Marshal(entry.Fingerprint())
	if err != nil {
		t.Fatal(err)
	}

	service := &Service{root: root, locks: coordination.New()}
	firstRelease, err := service.lockOperation(context.Background(), store.OperationRecord{
		Action: "rename", SourcePath: "first.txt", TargetPath: "renamed.txt", SourceStateJSON: string(sourceState),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer firstRelease()

	entered := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		release, err := service.lockOperation(context.Background(), store.OperationRecord{
			Action: "move_file", SourcePath: "alias.txt", TargetPath: "moved.txt", SourceStateJSON: string(sourceState),
		})
		if err == nil {
			close(entered)
			release()
		}
		finished <- err
	}()

	select {
	case <-entered:
		t.Fatal("Agent operations through hard-link aliases acquired concurrent write locks")
	case <-time.After(40 * time.Millisecond):
	}
	firstRelease()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("alias lock after release: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Agent alias lock did not resume after release")
	}
}
