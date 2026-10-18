//go:build linux

// 本文件验证 Linux 特殊文件不会进入文件复制和移动流程。
package httpapi

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"filesage/internal/coordination"
	"filesage/internal/fsroot"
	"filesage/internal/store"
	"golang.org/x/sys/unix"
)

func TestFileOperationRejectsSpecialFile(t *testing.T) {
	rootPath, handler := newMutationHandler(t)
	pipePath := filepath.Join(rootPath, "pipe")
	if err := unix.Mkfifo(pipePath, 0o600); err != nil {
		t.Fatal(err)
	}
	version := versionFromListing(t, handler, "pipe")
	response := postFileOperation(t, handler, "/api/v1/files/copy", "pipe", version, "", "")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("special-file copy status = %d, want 400: %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(filepath.Join(rootPath, "pipe (1)")); !os.IsNotExist(err) {
		t.Fatalf("special-file copy created a target: %v", err)
	}
}

func TestFileCopyAndMoveAuditActualPaths(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "archive"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"copy.txt": "copy", "move.txt": "move"} {
		if err := os.WriteFile(filepath.Join(rootPath, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	metadata, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "audit.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	handler := NewWithTaskStore(root, metadata, nil, coordination.New())

	copyResponse := postFileOperation(t, handler, "/api/v1/files/copy", "copy.txt", versionFromListing(t, handler, "copy.txt"), "", "")
	if copyResponse.Code != http.StatusCreated {
		t.Fatalf("copy status = %d: %s", copyResponse.Code, copyResponse.Body.String())
	}
	moveResponse := postFileOperation(t, handler, "/api/v1/files/move", "move.txt", versionFromListing(t, handler, "move.txt"), "archive", versionFromListing(t, handler, "archive"))
	if moveResponse.Code != http.StatusOK {
		t.Fatalf("move status = %d: %s", moveResponse.Code, moveResponse.Body.String())
	}

	page, err := metadata.AuditHistoryPage(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]store.AuditRecord{}
	for _, event := range page.Events {
		if event.EventType == "manual_copy_file" || event.EventType == "manual_move_file" {
			seen[event.EventType] = event
		}
	}
	if len(seen) != 2 || seen["manual_copy_file"].SourcePath != "copy.txt" || seen["manual_copy_file"].TargetPath != "copy (1).txt" || seen["manual_copy_file"].Outcome != "succeeded" {
		t.Fatalf("copy audit = %#v", seen["manual_copy_file"])
	}
	if seen["manual_move_file"].SourcePath != "move.txt" || seen["manual_move_file"].TargetPath != "archive/move.txt" || seen["manual_move_file"].Outcome != "succeeded" {
		t.Fatalf("move audit = %#v", seen["manual_move_file"])
	}
}
