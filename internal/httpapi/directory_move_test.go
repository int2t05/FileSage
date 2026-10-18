// 本文件验证目录移动 API 的版本保护、无覆盖与 partial 契约。
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"filesage/internal/coordination"
	"filesage/internal/fsroot"
	"filesage/internal/store"
)

// TestMoveDirectoryMovesNestedTreeAndAutoNumbers 验证目录原子移动及重名自动编号。
func TestMoveDirectoryMovesNestedTreeAndAutoNumbers(t *testing.T) {
	requireLinuxTreeCopy(t)
	rootPath, handler := newDirectoryCopyHandler(t)
	if err := os.MkdirAll(filepath.Join(rootPath, "destination", "source", "keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "destination", "source", "keep", "existing.txt"), []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	sourceVersion := versionFromPathListing(t, handler, "source")
	destinationVersion := versionFromPathListing(t, handler, "destination")

	response := postFileOperation(t, handler, "/api/v1/files/move-directory", "source", sourceVersion, "destination", destinationVersion)
	if response.Code != http.StatusOK {
		t.Fatalf("move status = %d, want 200: %s", response.Code, response.Body.String())
	}
	var result fileOperationResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Path != "destination/source (1)" || result.Name != "source (1)" || result.Status != "" {
		t.Fatalf("move response = %#v, want numbered completed result", result)
	}
	if _, err := os.Lstat(filepath.Join(rootPath, "source")); !os.IsNotExist(err) {
		t.Fatalf("source remains after move: %v", err)
	}
	assertFileContents(t, filepath.Join(rootPath, result.Path, "nested", "note.txt"), "nested source")
	assertFileContents(t, filepath.Join(rootPath, "destination", "source", "keep", "existing.txt"), "preserve")
}

func TestMoveDirectoryAuditStoresSourceAndTarget(t *testing.T) {
	requireLinuxTreeCopy(t)
	rootPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootPath, "source", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(rootPath, "destination"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "source", "nested", "note.txt"), []byte("contents"), 0o600); err != nil {
		t.Fatal(err)
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
	response := postFileOperation(
		t, handler, "/api/v1/files/move-directory", "source",
		versionFromListing(t, handler, "source"), "destination", versionFromListing(t, handler, "destination"),
	)
	if response.Code != http.StatusOK {
		t.Fatalf("move status = %d: %s", response.Code, response.Body.String())
	}
	page, err := metadata.AuditHistoryPage(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range page.Events {
		if event.EventType == "manual_move_directory" {
			if event.SourcePath != "source" || event.TargetPath != "destination/source" || event.Outcome != "succeeded" {
				t.Fatalf("directory move audit = %+v", event)
			}
			return
		}
	}
	t.Fatal("directory move has no manual audit event")
}

// TestMoveDirectoryRejectsStaleSourceBeforePublication 验证目录树竞态不会删除源或发布目标。
func TestMoveDirectoryRejectsStaleSourceBeforePublication(t *testing.T) {
	requireLinuxTreeCopy(t)
	rootPath, _ := newDirectoryCopyHandler(t)
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	filesystem := &sourceConflictDirectoryMoveFileSystem{Root: root, rootPath: rootPath}
	handler := New(filesystem, nil)
	sourceVersion := versionFromPathListing(t, handler, "source")
	destinationVersion := versionFromPathListing(t, handler, "destination")

	response := postFileOperation(t, handler, "/api/v1/files/move-directory", "source", sourceVersion, "destination", destinationVersion)
	if response.Code != http.StatusConflict {
		t.Fatalf("move status = %d, want 409: %s", response.Code, response.Body.String())
	}
	if filesystem.calls != 1 {
		t.Fatalf("move calls = %d, want one", filesystem.calls)
	}
	assertFileContents(t, filepath.Join(rootPath, "source", "raced.txt"), "external update")
	if _, err := os.Lstat(filepath.Join(rootPath, "destination", "source")); !os.IsNotExist(err) {
		t.Fatalf("stale move published a target: %v", err)
	}
}

// TestMoveDirectoryRejectsStaleDestinationVersion 验证目标目录版本过期时不移动来源。
func TestMoveDirectoryRejectsStaleDestinationVersion(t *testing.T) {
	requireLinuxTreeCopy(t)
	rootPath, handler := newDirectoryCopyHandler(t)
	sourceVersion := versionFromPathListing(t, handler, "source")
	destinationVersion := versionFromPathListing(t, handler, "destination")
	if err := os.WriteFile(filepath.Join(rootPath, "destination", "new.txt"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}

	response := postFileOperation(t, handler, "/api/v1/files/move-directory", "source", sourceVersion, "destination", destinationVersion)
	if response.Code != http.StatusConflict {
		t.Fatalf("stale destination status = %d, want 409: %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(filepath.Join(rootPath, "source", "nested", "note.txt")); err != nil {
		t.Fatalf("stale destination changed source: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(rootPath, "destination", "source")); !os.IsNotExist(err) {
		t.Fatalf("stale destination created a move target: %v", err)
	}
}

// TestMoveDirectoryRejectsSymlinkAndNestedDestination 验证链接目录树和自我嵌套目标会被拒绝。
func TestMoveDirectoryRejectsSymlinkAndNestedDestination(t *testing.T) {
	requireLinuxTreeCopy(t)
	t.Run("symlink", func(t *testing.T) {
		rootPath, handler := newDirectoryCopyHandler(t)
		if err := os.Symlink("nested/note.txt", filepath.Join(rootPath, "source", "link.txt")); err != nil {
			t.Fatal(err)
		}
		sourceVersion := versionFromPathListing(t, handler, "source")
		response := postFileOperation(t, handler, "/api/v1/files/move-directory", "source", sourceVersion, "destination", versionFromPathListing(t, handler, "destination"))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("symlink move status = %d, want 400: %s", response.Code, response.Body.String())
		}
		if _, err := os.Stat(filepath.Join(rootPath, "source", "nested", "note.txt")); err != nil {
			t.Fatalf("source changed after rejected move: %v", err)
		}
		if _, err := os.Lstat(filepath.Join(rootPath, "destination", "source")); !os.IsNotExist(err) {
			t.Fatalf("unsafe move created a target: %v", err)
		}
	})

	t.Run("nested destination", func(t *testing.T) {
		rootPath, handler := newDirectoryCopyHandler(t)
		sourceVersion := versionFromPathListing(t, handler, "source")
		destinationVersion := versionFromPathListing(t, handler, "source/nested")
		response := postFileOperation(t, handler, "/api/v1/files/move-directory", "source", sourceVersion, "source/nested", destinationVersion)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("nested move status = %d, want 400: %s", response.Code, response.Body.String())
		}
		if _, err := os.Stat(filepath.Join(rootPath, "source", "nested", "note.txt")); err != nil {
			t.Fatalf("nested move changed source: %v", err)
		}
	})
}

// TestMoveDirectoryRejectsCurrentParent 验证移动到当前父目录不会隐式重命名。
func TestMoveDirectoryRejectsCurrentParent(t *testing.T) {
	requireLinuxTreeCopy(t)
	rootPath, handler := newDirectoryCopyHandler(t)
	sourceVersion := versionFromPathListing(t, handler, "source")
	response := postFileOperation(t, handler, "/api/v1/files/move-directory", "source", sourceVersion, "", "")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("same-parent move status = %d, want 400: %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(filepath.Join(rootPath, "source", "nested", "note.txt")); err != nil {
		t.Fatalf("same-parent move changed source: %v", err)
	}
}

// TestMoveDirectoryReturnsPartialWhenSourceRemovalIsUnsafe 验证目标完整但源未安全移除时返回 partial。
func TestMoveDirectoryReturnsPartialWhenSourceRemovalIsUnsafe(t *testing.T) {
	requireLinuxTreeCopy(t)
	rootPath, _ := newDirectoryCopyHandler(t)
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	handler := New(&partialDirectoryMoveFileSystem{Root: root}, nil)
	sourceVersion := versionFromPathListing(t, handler, "source")
	destinationVersion := versionFromPathListing(t, handler, "destination")

	response := postFileOperation(t, handler, "/api/v1/files/move-directory", "source", sourceVersion, "destination", destinationVersion)
	if response.Code != http.StatusMultiStatus {
		t.Fatalf("partial move status = %d, want 207: %s", response.Code, response.Body.String())
	}
	var result fileOperationResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "partial" || result.Path != "destination/source" || result.Message == "" {
		t.Fatalf("partial move response = %#v", result)
	}
}

type sourceConflictDirectoryMoveFileSystem struct {
	*fsroot.Root
	rootPath string
	calls    int
}

func (filesystem *sourceConflictDirectoryMoveFileSystem) MoveTreeNoReplace(ctx context.Context, source, destination string, expected fsroot.TreeSnapshot) (fsroot.TreeMoveResult, error) {
	filesystem.calls++
	if err := os.WriteFile(filepath.Join(filesystem.rootPath, "source", "raced.txt"), []byte("external update"), 0o600); err != nil {
		return fsroot.TreeMoveResult{}, err
	}
	return filesystem.Root.MoveTreeNoReplace(ctx, source, destination, expected)
}

type partialDirectoryMoveFileSystem struct{ *fsroot.Root }

func (*partialDirectoryMoveFileSystem) MoveTreeNoReplace(context.Context, string, string, fsroot.TreeSnapshot) (fsroot.TreeMoveResult, error) {
	return fsroot.TreeMoveResult{
		Published: true, SourceRetained: "source/.filesage-move-quarantine-test",
	}, errors.New("source could not be safely removed")
}
