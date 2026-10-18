// 本文件验证目录复制 API 的安全约束与重名处理。
package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"filesage/internal/coordination"
	"filesage/internal/fsroot"
	"filesage/internal/store"
)

// TestCopyDirectoryCopiesNestedTreeToRoot 验证嵌套内容和空目录可复制到根目录。
func TestCopyDirectoryCopiesNestedTreeToRoot(t *testing.T) {
	requireLinuxTreeCopy(t)
	rootPath, handler := newDirectoryCopyHandler(t)
	sourceVersion := versionFromPathListing(t, handler, "source")

	response := postFileOperation(t, handler, "/api/v1/files/copy-directory", "source", sourceVersion, "", "")
	if response.Code != http.StatusCreated {
		t.Fatalf("copy status = %d, want 201: %s", response.Code, response.Body.String())
	}
	var result fileOperationResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Path != "source (1)" || result.Name != "source (1)" || result.Status != "" || result.Message != "" {
		t.Fatalf("response = %#v, want only the copied path and name", result)
	}
	assertFileContents(t, filepath.Join(rootPath, result.Path, "nested", "note.txt"), "nested source")
	if info, err := os.Stat(filepath.Join(rootPath, result.Path, "nested", "empty")); err != nil || !info.IsDir() {
		t.Fatalf("copied empty directory stat = (%v, %v)", info, err)
	}
	assertFileContents(t, filepath.Join(rootPath, "source", "nested", "note.txt"), "nested source")
}

func TestCopyDirectoryAuditUsesPublishedTarget(t *testing.T) {
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
		t, handler, "/api/v1/files/copy-directory", "source",
		versionFromListing(t, handler, "source"), "destination", versionFromListing(t, handler, "destination"),
	)
	if response.Code != http.StatusCreated {
		t.Fatalf("copy status = %d: %s", response.Code, response.Body.String())
	}
	page, err := metadata.AuditHistoryPage(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range page.Events {
		if event.EventType == "manual_copy_directory" {
			if event.SourcePath != "source" || event.TargetPath != "destination/source" || event.Outcome != "succeeded" {
				t.Fatalf("directory copy audit = %+v", event)
			}
			return
		}
	}
	t.Fatal("directory copy has no manual audit event")
}

// TestCopyDirectoryUsesNumberedNameWithoutOverwriting 验证目标重名时自动编号且不覆盖。
func TestCopyDirectoryUsesNumberedNameWithoutOverwriting(t *testing.T) {
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

	response := postFileOperation(t, handler, "/api/v1/files/copy-directory", "source", sourceVersion, "destination", destinationVersion)
	if response.Code != http.StatusCreated {
		t.Fatalf("copy status = %d, want 201: %s", response.Code, response.Body.String())
	}
	var result fileOperationResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Path != "destination/source (1)" || result.Name != "source (1)" {
		t.Fatalf("response = %#v, want numbered directory name", result)
	}
	assertFileContents(t, filepath.Join(rootPath, "destination", "source", "keep", "existing.txt"), "preserve")
	assertFileContents(t, filepath.Join(rootPath, result.Path, "nested", "note.txt"), "nested source")
}

// TestCopyDirectoryNumbersAfterTheFullDirectoryName 保留目录扩展样式并在末尾追加序号。
func TestCopyDirectoryNumbersAfterTheFullDirectoryName(t *testing.T) {
	requireLinuxTreeCopy(t)
	rootPath, handler := newDirectoryCopyHandler(t)
	if err := os.Rename(filepath.Join(rootPath, "source"), filepath.Join(rootPath, "project.backup")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(rootPath, "destination", "project.backup"), 0o755); err != nil {
		t.Fatal(err)
	}
	sourceVersion := versionFromPathListing(t, handler, "project.backup")
	destinationVersion := versionFromPathListing(t, handler, "destination")

	response := postFileOperation(t, handler, "/api/v1/files/copy-directory", "project.backup", sourceVersion, "destination", destinationVersion)
	if response.Code != http.StatusCreated {
		t.Fatalf("copy status = %d, want 201: %s", response.Code, response.Body.String())
	}
	var result fileOperationResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Path != "destination/project.backup (1)" || result.Name != "project.backup (1)" {
		t.Fatalf("response = %#v, want suffix after the complete directory name", result)
	}
}

// TestCopyDirectoryRejectsSymlinksAndDestinationInsideSource 验证危险内容和自我嵌套目标会被拒绝。
func TestCopyDirectoryRejectsSymlinksAndDestinationInsideSource(t *testing.T) {
	requireLinuxTreeCopy(t)
	t.Run("symlink content", func(t *testing.T) {
		rootPath, handler := newDirectoryCopyHandler(t)
		if err := os.Symlink("nested/note.txt", filepath.Join(rootPath, "source", "link.txt")); err != nil {
			t.Fatal(err)
		}
		sourceVersion := versionFromPathListing(t, handler, "source")
		response := postFileOperation(t, handler, "/api/v1/files/copy-directory", "source", sourceVersion, "", "")
		if response.Code != http.StatusBadRequest {
			t.Fatalf("copy status = %d, want 400: %s", response.Code, response.Body.String())
		}
		if _, err := os.Lstat(filepath.Join(rootPath, "source (1)")); !os.IsNotExist(err) {
			t.Fatalf("unsafe copy created a target: %v", err)
		}
	})

	t.Run("destination inside source", func(t *testing.T) {
		rootPath, handler := newDirectoryCopyHandler(t)
		sourceVersion := versionFromPathListing(t, handler, "source")
		destinationVersion := versionFromPathListing(t, handler, "source/nested")
		response := postFileOperation(t, handler, "/api/v1/files/copy-directory", "source", sourceVersion, "source/nested", destinationVersion)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("copy status = %d, want 400: %s", response.Code, response.Body.String())
		}
		if _, err := os.Lstat(filepath.Join(rootPath, "source", "nested", "source")); !os.IsNotExist(err) {
			t.Fatalf("nested copy created a target: %v", err)
		}
	})
}

// TestCopyDirectoryRejectsStaleSourceAndDestinationVersions 验证过期版本不会产生副本。
func TestCopyDirectoryRejectsStaleSourceAndDestinationVersions(t *testing.T) {
	requireLinuxTreeCopy(t)
	t.Run("stale source", func(t *testing.T) {
		rootPath, handler := newDirectoryCopyHandler(t)
		sourceVersion := versionFromPathListing(t, handler, "source")
		if err := os.WriteFile(filepath.Join(rootPath, "source", "new.txt"), []byte("changed"), 0o600); err != nil {
			t.Fatal(err)
		}
		response := postFileOperation(t, handler, "/api/v1/files/copy-directory", "source", sourceVersion, "", "")
		if response.Code != http.StatusConflict {
			t.Fatalf("copy status = %d, want 409: %s", response.Code, response.Body.String())
		}
		if _, err := os.Lstat(filepath.Join(rootPath, "source (1)")); !os.IsNotExist(err) {
			t.Fatalf("stale source created a target: %v", err)
		}
	})

	t.Run("stale destination", func(t *testing.T) {
		rootPath, handler := newDirectoryCopyHandler(t)
		sourceVersion := versionFromPathListing(t, handler, "source")
		destinationVersion := versionFromPathListing(t, handler, "destination")
		if err := os.WriteFile(filepath.Join(rootPath, "destination", "changed.txt"), []byte("changed"), 0o600); err != nil {
			t.Fatal(err)
		}
		response := postFileOperation(t, handler, "/api/v1/files/copy-directory", "source", sourceVersion, "destination", destinationVersion)
		if response.Code != http.StatusConflict {
			t.Fatalf("copy status = %d, want 409: %s", response.Code, response.Body.String())
		}
		if _, err := os.Lstat(filepath.Join(rootPath, "destination", "source")); !os.IsNotExist(err) {
			t.Fatalf("stale destination created a target: %v", err)
		}
	})
}

// TestCopyDirectoryDoesNotRetrySourceConflictAsNameCollision 验证源冲突不会被误判为目标重名。
func TestCopyDirectoryDoesNotRetrySourceConflictAsNameCollision(t *testing.T) {
	requireLinuxTreeCopy(t)
	rootPath, _ := newDirectoryCopyHandler(t)
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	filesystem := &sourceConflictTreeCopyFileSystem{Root: root, rootPath: rootPath}
	handler := New(filesystem, nil)
	sourceVersion := versionFromPathListing(t, handler, "source")

	response := postFileOperation(t, handler, "/api/v1/files/copy-directory", "source", sourceVersion, "", "")
	if response.Code != http.StatusConflict {
		t.Fatalf("copy status = %d, want 409: %s", response.Code, response.Body.String())
	}
	if filesystem.calls != 1 {
		t.Fatalf("copy attempts = %d, want one source-conflict attempt", filesystem.calls)
	}
	if _, err := os.Lstat(filepath.Join(rootPath, "source (1)")); !os.IsNotExist(err) {
		t.Fatalf("source conflict created a target: %v", err)
	}
}

type sourceConflictTreeCopyFileSystem struct {
	*fsroot.Root
	rootPath string
	calls    int
}

func (filesystem *sourceConflictTreeCopyFileSystem) CopyTreeNoReplace(context.Context, string, string, fsroot.TreeSnapshot) (fsroot.TreeCopyResult, error) {
	filesystem.calls++
	if err := os.WriteFile(filepath.Join(filesystem.rootPath, "source", "nested", "raced.txt"), []byte("changed during copy"), 0o600); err != nil {
		return fsroot.TreeCopyResult{}, err
	}
	return fsroot.TreeCopyResult{}, fsroot.ErrConflict
}

// newDirectoryCopyHandler 创建包含嵌套来源与目标目录的测试服务。
func newDirectoryCopyHandler(t *testing.T) (string, http.Handler) {
	t.Helper()
	rootPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootPath, "source", "nested", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(rootPath, "destination"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "source", "nested", "note.txt"), []byte("nested source"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return rootPath, New(root, nil)
}

// requireLinuxTreeCopy 跳过未实现安全目录复制的平台。
func requireLinuxTreeCopy(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("directory tree copy is implemented on Linux")
	}
}
