// 本文件验证文件写接口的冲突、安全边界与并发协调。
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"filesage/internal/coordination"
	"filesage/internal/fsroot"
	"filesage/internal/store"
)

type mutateAfterRenameAudit struct {
	taskReader
	mutate  func() error
	started bool
	outcome string
}

type replaceAfterRenameFileSystem struct {
	*fsroot.Root
	mutate func(string) error
}

type replaceBeforeDeleteFileSystem struct {
	*fsroot.Root
	rootPath string
	replaced bool
}

// replaceTarget 模拟条件删除边界上的外部路径替换。
func (files *replaceBeforeDeleteFileSystem) replaceTarget() error {
	if files.replaced {
		return nil
	}
	files.replaced = true
	target := filepath.Join(files.rootPath, "hello.txt")
	if err := os.Remove(target); err != nil {
		return err
	}
	return os.WriteFile(target, []byte("replacement"), 0o644)
}

// RemoveEntryIfUnchanged 在安全删除前替换路径，验证旧审批不会删新文件。
func (files *replaceBeforeDeleteFileSystem) RemoveEntryIfUnchanged(expected fsroot.Entry) error {
	if expected.Path == "hello.txt" {
		if err := files.replaceTarget(); err != nil {
			return err
		}
	}
	return files.Root.RemoveEntryIfUnchanged(expected)
}

// RenameNoReplace 在真实改名后注入目标变化。
func (fileSystem *replaceAfterRenameFileSystem) RenameNoReplace(oldPath, newPath string) error {
	if err := fileSystem.Root.RenameNoReplace(oldPath, newPath); err != nil {
		return err
	}
	if fileSystem.mutate != nil {
		return fileSystem.mutate(newPath)
	}
	return nil
}

func (audit *mutateAfterRenameAudit) BeginManualAudit(context.Context, store.AuditRecord) (int64, error) {
	audit.started = true
	if audit.mutate != nil {
		if err := audit.mutate(); err != nil {
			return 0, err
		}
	}
	return 1, nil
}

func (audit *mutateAfterRenameAudit) FinishManualAudit(_ context.Context, _ int64, outcome, _ string) error {
	audit.outcome = outcome
	return nil
}

func (*mutateAfterRenameAudit) AuditHistoryPage(context.Context, int64, int) (store.AuditHistoryPage, error) {
	return store.AuditHistoryPage{}, nil
}

func TestListFilesIncludesOpaqueVersionToken(t *testing.T) {
	rootPath, handler := newMutationHandler(t)
	response := performRequest(handler, http.MethodGet, "/api/v1/files", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("list status = %d: %s", response.Code, response.Body.String())
	}
	var result listingResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	var initial string
	for _, entry := range result.Entries {
		if entry.Name == "hello.txt" {
			initial = entry.Version
		}
	}
	if initial == "" {
		t.Fatal("listing did not include a version token for hello.txt")
	}
	if len(initial) != len("v1.")+43 {
		t.Fatalf("version token is not an opaque digest: %q", initial)
	}

	if err := os.WriteFile(filepath.Join(rootPath, "hello.txt"), []byte("changed size"), 0o644); err != nil {
		t.Fatal(err)
	}
	response = performRequest(handler, http.MethodGet, "/api/v1/files", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("second list status = %d: %s", response.Code, response.Body.String())
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	for _, entry := range result.Entries {
		if entry.Name == "hello.txt" && entry.Version == initial {
			t.Fatal("version token did not change after the file changed")
		}
	}
}

func TestCreateDirectoryUsesNumericSuffixAndReturnsCreatedPath(t *testing.T) {
	rootPath, handler := newMutationHandler(t)
	if err := os.Mkdir(filepath.Join(rootPath, "Photos"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Photos (2)", "Photos (3)"} {
		response := performJSONRequest(handler, http.MethodPost, "/api/v1/files/directories", `{"parent":"","name":"Photos"}`)
		if response.Code != http.StatusCreated {
			t.Fatalf("create status = %d: %s", response.Code, response.Body.String())
		}
		var result pathResponse
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		if result.Path != want || result.Name != want {
			t.Fatalf("create response = %#v, want path/name %q", result, want)
		}
		if info, err := os.Stat(filepath.Join(rootPath, want)); err != nil || !info.IsDir() {
			t.Fatalf("created path %q is not a directory: (%v, %v)", want, info, err)
		}
	}
	response := performJSONRequest(handler, http.MethodPost, "/api/v1/files/directories", `{"parent":"notes","name":"Archive"}`)
	if response.Code != http.StatusCreated || !strings.Contains(response.Body.String(), `"path":"notes/Archive"`) {
		t.Fatalf("nested create response = %d %s", response.Code, response.Body.String())
	}
}

func TestCreateDirectoryValidatesParentAndName(t *testing.T) {
	_, handler := newMutationHandler(t)
	for _, body := range []string{
		`{"name":"no-parent"}`,
		`{"parent":"../outside","name":"safe"}`,
		`{"parent":"","name":"../outside"}`,
		`{"parent":"","name":"a/b"}`,
		`{"parent":"","name":"bad\nname"}`,
		`{"parent":"","name":""}`,
		`{"parent":"","name":".."}`,
		`{"parent":"","name":"safe","extra":true}`,
		`{"parent":"","name":"safe"} {}`,
	} {
		response := performJSONRequest(handler, http.MethodPost, "/api/v1/files/directories", body)
		if response.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400: %s", body, response.Code, response.Body.String())
		}
	}
	if validEntryName(string([]byte{0xff})) {
		t.Fatal("invalid UTF-8 name was accepted")
	}
	if validEntryName(strings.Repeat("x", 256)) {
		t.Fatal("name longer than 255 bytes was accepted")
	}
	if !validEntryName(strings.Repeat("x", 255)) {
		t.Fatal("valid 255-byte name was rejected")
	}
}

func TestRenameRequiresCurrentVersionAndNeverOverwrites(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("mutation version identity is validated on Linux")
	}
	rootPath, handler := newMutationHandler(t)
	if err := os.WriteFile(filepath.Join(rootPath, "destination.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	version := versionFromListing(t, handler, "hello.txt")
	response := performJSONRequest(handler, http.MethodPost, "/api/v1/files/rename", `{"path":"hello.txt","name":"destination.txt","version":"`+version+`"}`)
	if response.Code != http.StatusConflict {
		t.Fatalf("rename collision status = %d, want 409: %s", response.Code, response.Body.String())
	}
	if data, err := os.ReadFile(filepath.Join(rootPath, "hello.txt")); err != nil || string(data) != "hello" {
		t.Fatalf("source changed on collision: (%q, %v)", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(rootPath, "destination.txt")); err != nil || string(data) != "keep" {
		t.Fatalf("destination overwritten: (%q, %v)", data, err)
	}

	response = performJSONRequest(handler, http.MethodPost, "/api/v1/files/rename", `{"path":"hello.txt","name":"renamed.txt"}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("rename without version status = %d, want 400: %s", response.Code, response.Body.String())
	}

	if err := os.WriteFile(filepath.Join(rootPath, "hello.txt"), []byte("changed after listing"), 0o644); err != nil {
		t.Fatal(err)
	}
	response = performJSONRequest(handler, http.MethodPost, "/api/v1/files/rename", `{"path":"hello.txt","name":"renamed.txt","version":"`+version+`"}`)
	if response.Code != http.StatusConflict {
		t.Fatalf("stale rename status = %d, want 409: %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(filepath.Join(rootPath, "hello.txt")); err != nil {
		t.Fatalf("stale rename changed source: %v", err)
	}
}

func TestRenameRejectsReplacementDuringAuditWrite(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("file identity validation is required")
	}
	rootPath := t.TempDir()
	approvedPath := filepath.Join(rootPath, "hello.txt")
	replacementPath := filepath.Join(rootPath, "replacement.txt")
	if err := os.WriteFile(approvedPath, []byte("approved contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	audit := &mutateAfterRenameAudit{mutate: func() error {
		if err := os.Rename(approvedPath, replacementPath); err != nil {
			return err
		}
		return os.WriteFile(approvedPath, []byte("replacement contents"), 0o600)
	}}
	handler := NewWithTaskStore(root, audit, nil, coordination.New())
	version := versionFromListing(t, handler, "hello.txt")
	response := performJSONRequest(handler, http.MethodPost, "/api/v1/files/rename", `{"path":"hello.txt","name":"renamed.txt","version":"`+version+`"}`)
	if response.Code != http.StatusConflict {
		t.Fatalf("rename status = %d, want conflict: %s", response.Code, response.Body.String())
	}
	if !audit.started || audit.outcome != "failed" {
		t.Fatalf("audit started=%v outcome=%q, want started and failed", audit.started, audit.outcome)
	}
	for path, want := range map[string]string{
		approvedPath:    "replacement contents",
		replacementPath: "approved contents",
	} {
		contents, err := os.ReadFile(path)
		if err != nil || string(contents) != want {
			t.Errorf("file %q = (%q, %v), want %q", path, contents, err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(rootPath, "renamed.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("renamed target exists after conflict: %v", err)
	}
}

func TestRenameReportsNeedsReviewWhenTargetChangesBeforeReturn(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("file identity validation is required")
	}
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "hello.txt"), []byte("approved contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	fileSystem := &replaceAfterRenameFileSystem{Root: root, mutate: func(newPath string) error {
		if err := os.Rename(filepath.Join(rootPath, newPath), filepath.Join(rootPath, "approved-item.txt")); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(rootPath, newPath), []byte("external replacement"), 0o600)
	}}
	audit := &mutateAfterRenameAudit{}
	handler := NewWithTaskStore(fileSystem, audit, nil, coordination.New())
	version := versionFromListing(t, handler, "hello.txt")
	response := performJSONRequest(handler, http.MethodPost, "/api/v1/files/rename", `{"path":"hello.txt","name":"renamed.txt","version":"`+version+`"}`)
	if response.Code != http.StatusConflict {
		t.Fatalf("rename status = %d, want 409: %s", response.Code, response.Body.String())
	}
	if audit.outcome != "needs_review" {
		t.Fatalf("audit outcome = %q, want needs_review", audit.outcome)
	}
	for name, want := range map[string]string{"approved-item.txt": "approved contents", "renamed.txt": "external replacement"} {
		contents, err := os.ReadFile(filepath.Join(rootPath, name))
		if err != nil || string(contents) != want {
			t.Errorf("%s = (%q, %v), want %q", name, contents, err, want)
		}
	}
}

func TestRenameReturnsNewPathAndAcceptsSymlinkAsFinalEntry(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("mutation version identity is validated on Linux")
	}
	rootPath, handler := newMutationHandler(t)
	version := versionFromListing(t, handler, "hello.txt")
	response := performJSONRequest(handler, http.MethodPost, "/api/v1/files/rename", `{"path":"hello.txt","name":"renamed.txt","version":"`+version+`"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("rename status = %d, want 200: %s", response.Code, response.Body.String())
	}
	var result pathResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Path != "renamed.txt" || result.Name != "renamed.txt" {
		t.Fatalf("rename response = %#v", result)
	}
	if _, err := os.Stat(filepath.Join(rootPath, "hello.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old source still exists after rename: %v", err)
	}

	if err := os.Symlink("notes/today.md", filepath.Join(rootPath, "alias.md")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink creation unavailable: %v", err)
		}
		t.Fatal(err)
	}
	linkVersion := versionFromListing(t, handler, "alias.md")
	response = performJSONRequest(handler, http.MethodPost, "/api/v1/files/rename", `{"path":"alias.md","name":"alias-renamed.md","version":"`+linkVersion+`"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("rename symlink entry status = %d: %s", response.Code, response.Body.String())
	}
	if _, err := os.Lstat(filepath.Join(rootPath, "alias-renamed.md")); err != nil {
		t.Fatalf("renamed symlink missing: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(rootPath, "notes", "today.md")); err != nil || string(data) != "today" {
		t.Fatalf("renaming symlink changed target: (%q, %v)", data, err)
	}
}

func TestRenameRejectsSameMetadataPathReplacement(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("inode identity is required for this check")
	}
	rootPath, handler := newMutationHandler(t)
	version := versionFromListing(t, handler, "hello.txt")
	original := filepath.Join(rootPath, "hello.txt")
	info, err := os.Stat(original)
	if err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(rootPath, "replacement.tmp")
	if err := os.WriteFile(replacement, []byte("hello"), info.Mode().Perm()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(replacement, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, original); err != nil {
		t.Fatal(err)
	}
	response := performJSONRequest(handler, http.MethodPost, "/api/v1/files/rename", `{"path":"hello.txt","name":"renamed.txt","version":"`+version+`"}`)
	if response.Code != http.StatusConflict {
		t.Fatalf("same-metadata replacement status = %d, want 409: %s", response.Code, response.Body.String())
	}
	if data, err := os.ReadFile(original); err != nil || string(data) != "hello" {
		t.Fatalf("stale rename changed replacement: (%q, %v)", data, err)
	}
}

func TestRenameAndDeleteHardLinkOnlyAffectSelectedPath(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("hard-link identity is validated on Linux")
	}
	rootPath, handler := newMutationHandler(t)
	original := filepath.Join(rootPath, "hello.txt")
	alias := filepath.Join(rootPath, "hardlink.txt")
	if err := os.Link(original, alias); err != nil {
		t.Skipf("hard links are unavailable: %v", err)
	}

	version := versionFromListing(t, handler, "hello.txt")
	response := performJSONRequest(handler, http.MethodPost, "/api/v1/files/rename", `{"path":"hello.txt","name":"renamed.txt","version":"`+version+`"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("rename hard-linked entry status = %d: %s", response.Code, response.Body.String())
	}
	moved := filepath.Join(rootPath, "renamed.txt")
	movedInfo, err := os.Stat(moved)
	if err != nil {
		t.Fatal(err)
	}
	aliasInfo, err := os.Stat(alias)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(movedInfo, aliasInfo) {
		t.Fatal("renamed entry no longer shares the hard link")
	}

	version = versionFromListing(t, handler, "renamed.txt")
	query := url.Values{"path": {"renamed.txt"}, "version": {version}, "confirmPermanent": {"true"}}
	response = performRequest(handler, http.MethodDelete, "/api/v1/files?"+query.Encode(), nil)
	if response.Code != http.StatusNoContent {
		t.Fatalf("delete hard-linked entry status = %d: %s", response.Code, response.Body.String())
	}
	if data, err := os.ReadFile(alias); err != nil || string(data) != "hello" {
		t.Fatalf("deleting one hard link changed the other: data=%q err=%v", data, err)
	}
}

func TestCopyMoveFileEndpointsPreserveContentsAndChooseUniqueNames(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("safe anonymous file publication is implemented on Linux")
	}
	rootPath, handler := newMutationHandler(t)
	sourceVersion := versionFromListing(t, handler, "hello.txt")
	destinationVersion := versionFromPathListing(t, handler, "notes")

	response := postFileOperation(t, handler, "/api/v1/files/copy", "hello.txt", sourceVersion, "notes", destinationVersion)
	if response.Code != http.StatusCreated {
		t.Fatalf("copy status = %d: %s", response.Code, response.Body.String())
	}
	var copied fileOperationResponse
	if err := json.NewDecoder(response.Body).Decode(&copied); err != nil {
		t.Fatal(err)
	}
	if copied.Path != "notes/hello.txt" || copied.Name != "hello.txt" || copied.Status != "" {
		t.Fatalf("copy response = %#v", copied)
	}
	assertFileContents(t, filepath.Join(rootPath, "hello.txt"), "hello")
	assertFileContents(t, filepath.Join(rootPath, "notes", "hello.txt"), "hello")

	destinationVersion = versionFromPathListing(t, handler, "notes")
	response = postFileOperation(t, handler, "/api/v1/files/copy", "hello.txt", sourceVersion, "notes", destinationVersion)
	if response.Code != http.StatusCreated {
		t.Fatalf("copy collision status = %d: %s", response.Code, response.Body.String())
	}
	if err := json.NewDecoder(response.Body).Decode(&copied); err != nil {
		t.Fatal(err)
	}
	if copied.Path != "notes/hello (1).txt" || copied.Name != "hello (1).txt" {
		t.Fatalf("numbered copy response = %#v", copied)
	}
	assertFileContents(t, filepath.Join(rootPath, "notes", "hello.txt"), "hello")

	destinationVersion = versionFromPathListing(t, handler, "notes")
	response = postFileOperation(t, handler, "/api/v1/files/move", "hello.txt", sourceVersion, "notes", destinationVersion)
	if response.Code != http.StatusOK {
		t.Fatalf("move status = %d: %s", response.Code, response.Body.String())
	}
	var moved fileOperationResponse
	if err := json.NewDecoder(response.Body).Decode(&moved); err != nil {
		t.Fatal(err)
	}
	if moved.Path != "notes/hello (2).txt" || moved.Name != "hello (2).txt" {
		t.Fatalf("numbered move response = %#v", moved)
	}
	if _, err := os.Stat(filepath.Join(rootPath, "hello.txt")); !os.IsNotExist(err) {
		t.Fatalf("move kept source: %v", err)
	}
	assertFileContents(t, filepath.Join(rootPath, moved.Path), "hello")
}

func TestFileOperationRejectsStaleVersionsAndUnsafePaths(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("safe file operations are validated on Linux")
	}
	rootPath, handler := newMutationHandler(t)
	sourceVersion := versionFromListing(t, handler, "hello.txt")
	destinationVersion := versionFromPathListing(t, handler, "notes")
	if err := os.WriteFile(filepath.Join(rootPath, "hello.txt"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	response := postFileOperation(t, handler, "/api/v1/files/copy", "hello.txt", sourceVersion, "notes", destinationVersion)
	if response.Code != http.StatusConflict {
		t.Fatalf("stale source status = %d, want 409: %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(filepath.Join(rootPath, "notes", "hello.txt")); !os.IsNotExist(err) {
		t.Fatalf("stale source produced a destination: %v", err)
	}

	sourceVersion = versionFromListing(t, handler, "hello.txt")
	destinationVersion = versionFromPathListing(t, handler, "notes")
	if err := os.WriteFile(filepath.Join(rootPath, "notes", "other.txt"), []byte("changed directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	changedDirectoryTime := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(filepath.Join(rootPath, "notes"), changedDirectoryTime, changedDirectoryTime); err != nil {
		t.Fatal(err)
	}
	if updatedVersion := versionFromPathListing(t, handler, "notes"); updatedVersion == destinationVersion {
		t.Fatal("destination directory change did not invalidate its version token")
	}
	response = postFileOperation(t, handler, "/api/v1/files/copy", "hello.txt", sourceVersion, "notes", destinationVersion)
	if response.Code != http.StatusConflict {
		t.Fatalf("stale destination status = %d, want 409: %s", response.Code, response.Body.String())
	}
	response = postFileOperation(t, handler, "/api/v1/files/copy", "hello.txt", sourceVersion, "", "unexpected")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("root destination with a version status = %d, want 400: %s", response.Code, response.Body.String())
	}
	response = postFileOperation(t, handler, "/api/v1/files/copy", "hello.txt", sourceVersion, "", "")
	if response.Code != http.StatusCreated {
		t.Fatalf("root destination without a version status = %d, want 201: %s", response.Code, response.Body.String())
	}

	response = postFileOperation(t, handler, "/api/v1/files/copy", "../hello.txt", sourceVersion, "notes", destinationVersion)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("traversal source status = %d, want 400: %s", response.Code, response.Body.String())
	}
	response = postFileOperation(t, handler, "/api/v1/files/copy", "hello.txt", sourceVersion, "notes", "")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("missing destination version status = %d, want 400: %s", response.Code, response.Body.String())
	}
}

func TestFileOperationRejectsSymlinkEntriesAndSymlinkDirectories(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("symbolic-link mutation validation is tested on Linux")
	}
	rootPath, handler := newMutationHandler(t)
	if err := os.Symlink("hello.txt", filepath.Join(rootPath, "alias.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("notes", filepath.Join(rootPath, "notes-link")); err != nil {
		t.Fatal(err)
	}
	linkVersion := versionFromListing(t, handler, "alias.txt")
	directoryLinkVersion := versionFromListing(t, handler, "notes-link")
	sourceVersion := versionFromListing(t, handler, "hello.txt")
	response := postFileOperation(t, handler, "/api/v1/files/copy", "alias.txt", linkVersion, "", "")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("symlink source status = %d, want 400: %s", response.Code, response.Body.String())
	}
	response = postFileOperation(t, handler, "/api/v1/files/copy", "hello.txt", sourceVersion, "notes-link", directoryLinkVersion)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("symlink destination status = %d, want 400: %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(filepath.Join(rootPath, "notes", "hello.txt")); !os.IsNotExist(err) {
		t.Fatalf("symlink destination wrote through the link: %v", err)
	}
}

func TestCrossMountMoveReportsPublishedDestinationWhenSourceRemovalFails(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("safe anonymous file publication is implemented on Linux")
	}
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "target"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "source.txt"), []byte("keep both until reviewed"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	handler := New(partialMoveFileSystem{Root: root}, nil)
	sourceVersion := versionFromListing(t, handler, "source.txt")
	destinationVersion := versionFromPathListing(t, handler, "target")

	response := postFileOperation(t, handler, "/api/v1/files/move", "source.txt", sourceVersion, "target", destinationVersion)
	if response.Code != http.StatusMultiStatus {
		t.Fatalf("partial move status = %d, want 207: %s", response.Code, response.Body.String())
	}
	var result fileOperationResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Path != "target/source.txt" || result.Status != "partial" || result.Message == "" {
		t.Fatalf("partial move response = %#v", result)
	}
	assertFileContents(t, filepath.Join(rootPath, "source.txt"), "keep both until reviewed")
	assertFileContents(t, filepath.Join(rootPath, result.Path), "keep both until reviewed")
}

type partialMoveFileSystem struct{ *fsroot.Root }

func (filesystem partialMoveFileSystem) MoveFileNoReplace(ctx context.Context, source, destination string, expected fsroot.Fingerprint) (fsroot.FileMoveResult, error) {
	copyResult, err := filesystem.Root.CopyFileNoReplace(ctx, source, destination, expected)
	return fsroot.FileMoveResult{Published: copyResult.Published}, errOrRemovalFailure(err)
}

func errOrRemovalFailure(err error) error {
	if err != nil {
		return err
	}
	return errors.New("injected source removal failure")
}

func postFileOperation(t *testing.T, handler http.Handler, endpoint, source, sourceVersion, destination, destinationVersion string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(fileOperationRequest{
		Path: source, Version: sourceVersion, Destination: destination, DestinationVersion: destinationVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	return performJSONRequest(handler, http.MethodPost, endpoint, string(body))
}

func versionFromPathListing(t *testing.T, handler http.Handler, directory string) string {
	t.Helper()
	parent := path.Dir(directory)
	if parent == "." {
		parent = ""
	}
	response := performRequest(handler, http.MethodGet, "/api/v1/files?path="+url.QueryEscape(parent), nil)
	if response.Code != http.StatusOK {
		t.Fatalf("list %q status = %d: %s", directory, response.Code, response.Body.String())
	}
	var result listingResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	for _, entry := range result.Entries {
		if entry.Path == directory {
			return entry.Version
		}
	}
	t.Fatalf("listing has no directory %q", directory)
	return ""
}

func assertFileContents(t *testing.T, path, expected string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != expected {
		t.Fatalf("file %q = (%q, %v), want %q", path, data, err, expected)
	}
}

func TestMutationsRejectSymlinkedParent(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux symlink semantics")
	}
	rootPath, handler := newMutationHandler(t)
	if err := os.Symlink("notes", filepath.Join(rootPath, "notes-link")); err != nil {
		t.Fatal(err)
	}
	response := performJSONRequest(handler, http.MethodPost, "/api/v1/files/directories", `{"parent":"notes-link","name":"new"}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("create through symlink parent status = %d, want 400: %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(filepath.Join(rootPath, "notes", "new")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("create through symlink changed target: %v", err)
	}

	query := url.Values{"path": {"notes-link"}}
	listing := performRequest(handler, http.MethodGet, "/api/v1/files?"+query.Encode(), nil)
	var result listingResponse
	if err := json.NewDecoder(listing.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	var version string
	for _, entry := range result.Entries {
		if entry.Name == "today.md" {
			version = entry.Version
		}
	}
	if version == "" {
		t.Fatal("symlinked directory listing did not return the nested entry version")
	}
	response = performJSONRequest(handler, http.MethodPost, "/api/v1/files/rename", `{"path":"notes-link/today.md","name":"renamed.md","version":"`+version+`"}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("rename through symlink parent status = %d, want 400: %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(filepath.Join(rootPath, "notes", "today.md")); err != nil {
		t.Fatalf("rename through symlink changed target: %v", err)
	}
	query = url.Values{"path": {"notes-link/today.md"}, "version": {version}, "confirmPermanent": {"true"}}
	response = performRequest(handler, http.MethodDelete, "/api/v1/files?"+query.Encode(), nil)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("delete through symlink parent status = %d, want 400: %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(filepath.Join(rootPath, "notes", "today.md")); err != nil {
		t.Fatalf("delete through symlink changed target: %v", err)
	}
}

func TestAPIUsesInjectedSharedCoordinator(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootPath, "notes", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	locks := coordination.New()
	handler := NewWithLocker(root, nil, locks)
	release, err := locks.Lock(context.Background(), "path:notes")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	finished := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		close(started)
		finished <- performJSONRequest(handler, http.MethodPost, "/api/v1/files/directories", `{"parent":"notes/sub","name":"queued"}`)
	}()
	<-started
	select {
	case response := <-finished:
		release()
		t.Fatalf("operation passed a held shared lock: %d %s", response.Code, response.Body.String())
	case <-time.After(50 * time.Millisecond):
	}
	release()
	select {
	case response := <-finished:
		if response.Code != http.StatusCreated {
			t.Fatalf("operation after lock release status = %d: %s", response.Code, response.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("operation did not resume after shared lock release")
	}
}

func TestDeleteDefaultsToTrashAndRejectsDuplicateConfirmation(t *testing.T) {
	rootPath, handler := newMutationHandler(t)
	version := versionFromListing(t, handler, "hello.txt")
	for _, test := range []struct {
		name       string
		confirm    []string
		includeKey bool
		status     int
	}{
		{name: "missing", status: http.StatusServiceUnavailable},
		{name: "false", confirm: []string{"false"}, includeKey: true, status: http.StatusServiceUnavailable},
		{name: "duplicate", confirm: []string{"true", "false"}, includeKey: true, status: http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			query := url.Values{"path": {"hello.txt"}, "version": {version}}
			if test.includeKey {
				query["confirmPermanent"] = test.confirm
			}
			response := performRequest(handler, http.MethodDelete, "/api/v1/files?"+query.Encode(), nil)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.status, response.Body.String())
			}
			if test.name == "duplicate" && !strings.Contains(response.Body.String(), "invalid or repeated") {
				t.Fatalf("duplicate confirmation was not rejected: %s", response.Body.String())
			}
		})
	}
	if data, err := os.ReadFile(filepath.Join(rootPath, "hello.txt")); err != nil || string(data) != "hello" {
		t.Fatalf("unconfirmed delete changed file: data=%q err=%v", data, err)
	}
}

func TestDeleteOnlyRemovesSingleEntryAndRequiresVersion(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("mutation version identity is validated on Linux")
	}
	rootPath, handler := newMutationHandler(t)
	query := url.Values{"path": {"hello.txt"}, "confirmPermanent": {"true"}}
	if response := performRequest(handler, http.MethodDelete, "/api/v1/files?"+query.Encode(), nil); response.Code != http.StatusBadRequest {
		t.Fatalf("delete without version status = %d, want 400: %s", response.Code, response.Body.String())
	}

	if err := os.Mkdir(filepath.Join(rootPath, "nonempty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "nonempty", "child.txt"), []byte("child"), 0o644); err != nil {
		t.Fatal(err)
	}
	version := versionFromListing(t, handler, "nonempty")
	query.Set("path", "nonempty")
	query.Set("version", version)
	response := performRequest(handler, http.MethodDelete, "/api/v1/files?"+query.Encode(), nil)
	if response.Code != http.StatusConflict {
		t.Fatalf("delete nonempty directory status = %d, want 409: %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(filepath.Join(rootPath, "nonempty", "child.txt")); err != nil {
		t.Fatalf("nonempty directory contents changed: %v", err)
	}

	version = versionFromListing(t, handler, "hello.txt")
	query.Set("path", "hello.txt")
	query.Set("version", version)
	response = performRequest(handler, http.MethodDelete, "/api/v1/files?"+query.Encode(), nil)
	if response.Code != http.StatusNoContent {
		t.Fatalf("delete file status = %d, want 204: %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(filepath.Join(rootPath, "hello.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted file still exists: %v", err)
	}
}

func TestPermanentDeletePreservesReplacementAfterVersionCheckWithoutTrash(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("conditional entry deletion is validated on Linux")
	}
	var files *replaceBeforeDeleteFileSystem
	rootPath, handler := newMutationHandlerWithFileSystem(t, func(rootPath string, root *fsroot.Root) fileSystem {
		files = &replaceBeforeDeleteFileSystem{Root: root, rootPath: rootPath}
		return files
	})
	version := versionFromListing(t, handler, "hello.txt")
	query := url.Values{
		"path":             {"hello.txt"},
		"version":          {version},
		"confirmPermanent": {"true"},
	}
	response := performRequest(handler, http.MethodDelete, "/api/v1/files?"+query.Encode(), nil)
	if response.Code != http.StatusConflict && response.Code != http.StatusNotImplemented {
		t.Fatalf("delete replaced entry status = %d, want conflict or unsupported: %s", response.Code, response.Body.String())
	}
	if !files.replaced {
		t.Fatalf("test did not replace the file at the delete boundary: status=%d body=%s", response.Code, response.Body.String())
	}
	data, err := os.ReadFile(filepath.Join(rootPath, "hello.txt"))
	if err != nil || string(data) != "replacement" {
		t.Fatalf("replacement file was not preserved: data=%q err=%v", data, err)
	}
}

func TestDeleteSymlinkDeletesOnlyTheLinkAndStaleVersionConflicts(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("mutation version identity is validated on Linux")
	}
	rootPath, handler := newMutationHandler(t)
	if err := os.Symlink("notes/today.md", filepath.Join(rootPath, "alias.md")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink creation unavailable: %v", err)
		}
		t.Fatal(err)
	}
	version := versionFromListing(t, handler, "alias.md")
	if err := os.Remove(filepath.Join(rootPath, "alias.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "alias.md"), []byte("replacement"), 0o644); err != nil {
		t.Fatal(err)
	}
	query := url.Values{"path": {"alias.md"}, "version": {version}, "confirmPermanent": {"true"}}
	response := performRequest(handler, http.MethodDelete, "/api/v1/files?"+query.Encode(), nil)
	if response.Code != http.StatusConflict {
		t.Fatalf("stale delete status = %d, want 409: %s", response.Code, response.Body.String())
	}
	if data, err := os.ReadFile(filepath.Join(rootPath, "alias.md")); err != nil || string(data) != "replacement" {
		t.Fatalf("stale delete removed replacement: (%q, %v)", data, err)
	}

	version = versionFromListing(t, handler, "alias.md")
	query.Set("version", version)
	response = performRequest(handler, http.MethodDelete, "/api/v1/files?"+query.Encode(), nil)
	if response.Code != http.StatusNoContent {
		t.Fatalf("delete regular replacement status = %d, want 204: %s", response.Code, response.Body.String())
	}
	if data, err := os.ReadFile(filepath.Join(rootPath, "notes", "today.md")); err != nil || string(data) != "today" {
		t.Fatalf("delete changed symlink target: (%q, %v)", data, err)
	}
}

func TestWriteEndpointsRejectCrossOriginRequests(t *testing.T) {
	rootPath, handler := newMutationHandler(t)
	version := versionFromListing(t, handler, "hello.txt")
	deleteQuery := url.Values{"path": {"hello.txt"}, "version": {version}, "confirmPermanent": {"true"}}
	requests := []struct {
		name    string
		request func() *http.Request
	}{
		{
			name: "create directory",
			request: func() *http.Request {
				return httptest.NewRequest(http.MethodPost, "/api/v1/files/directories", strings.NewReader(`{"parent":"","name":"blocked"}`))
			},
		},
		{
			name: "rename",
			request: func() *http.Request {
				return httptest.NewRequest(http.MethodPost, "/api/v1/files/rename", strings.NewReader(`{"path":"hello.txt","name":"blocked.txt","version":"`+version+`"}`))
			},
		},
		{
			name: "delete",
			request: func() *http.Request {
				return httptest.NewRequest(http.MethodDelete, "/api/v1/files?"+deleteQuery.Encode(), nil)
			},
		},
		{
			name: "copy",
			request: func() *http.Request {
				return httptest.NewRequest(http.MethodPost, "/api/v1/files/copy", strings.NewReader(`{"path":"hello.txt","version":"`+version+`","destination":"notes","destinationVersion":"stale"}`))
			},
		},
		{
			name: "move",
			request: func() *http.Request {
				return httptest.NewRequest(http.MethodPost, "/api/v1/files/move", strings.NewReader(`{"path":"hello.txt","version":"`+version+`","destination":"notes","destinationVersion":"stale"}`))
			},
		},
	}
	for _, test := range requests {
		t.Run(test.name, func(t *testing.T) {
			request := test.request()
			request.Host = "filesage.test:5173"
			request.Header.Set("Origin", "http://attacker.test:5173")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403: %s", response.Code, response.Body.String())
			}
		})
	}
	if _, err := os.Stat(filepath.Join(rootPath, "blocked")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cross-origin create changed filesystem: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rootPath, "hello.txt")); err != nil {
		t.Fatalf("cross-origin rename/delete changed source: %v", err)
	}
}

func TestWriteEndpointsAllowSameOriginViteProxyAndOriginlessClient(t *testing.T) {
	_, handler := newMutationHandler(t)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/files/directories", strings.NewReader(`{"parent":"","name":"same-origin"}`))
	request.Host = "filesage.test:5173"
	request.Header.Set("Origin", "http://filesage.test:5173")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("same-origin proxy request status = %d, want 201: %s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "/api/v1/files/directories", strings.NewReader(`{"parent":"","name":"local-client"}`))
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("request without Origin status = %d, want 201: %s", response.Code, response.Body.String())
	}
}

func TestWriteEndpointsRejectCrossSiteFetchWithoutOrigin(t *testing.T) {
	rootPath, handler := newMutationHandler(t)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/files/directories", strings.NewReader(`{"parent":"","name":"blocked"}`))
	request.Header.Set("Sec-Fetch-Site", "cross-site")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("cross-site request without Origin status = %d, want 403: %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(filepath.Join(rootPath, "blocked")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cross-site request changed filesystem: %v", err)
	}
}

func TestSameOriginRejectsProtocolMismatchAndMalformedOrigins(t *testing.T) {
	for _, test := range []struct {
		origin string
		scheme string
		host   string
	}{
		{origin: "https://filesage.test:5173", scheme: "http", host: "filesage.test:5173"},
		{origin: "null", scheme: "http", host: "filesage.test:5173"},
		{origin: "http://filesage.test:5173/other", scheme: "http", host: "filesage.test:5173"},
		{origin: "http://filesage.test:5173?query", scheme: "http", host: "filesage.test:5173"},
		{origin: "http://filesage.test:5173", scheme: "http", host: "attacker.test:5173"},
	} {
		if sameOrigin(test.origin, test.scheme, test.host) {
			t.Errorf("sameOrigin(%q, %q, %q) = true", test.origin, test.scheme, test.host)
		}
	}
	if !sameOrigin("https://filesage.test", "https", "filesage.test:443") {
		t.Fatal("same https origin with an explicit default port was rejected")
	}
}

func newMutationHandler(t *testing.T) (string, http.Handler) {
	return newMutationHandlerWithFileSystem(t, nil)
}

func newMutationHandlerWithFileSystem(t *testing.T, wrap func(string, *fsroot.Root) fileSystem) (string, http.Handler) {
	t.Helper()
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "hello.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "notes", "today.md"), []byte("today"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	if err := root.ConfigureMoveRecoveryDirectory(filepath.Join(t.TempDir(), "move-recovery")); err != nil {
		t.Fatal(err)
	}
	files := fileSystem(root)
	if wrap != nil {
		files = wrap(rootPath, root)
	}
	return rootPath, New(files, nil)
}

func performJSONRequest(handler http.Handler, method, target, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func performRequest(handler http.Handler, method, target string, body io.Reader) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, body)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func versionFromListing(t *testing.T, handler http.Handler, name string) string {
	t.Helper()
	response := performRequest(handler, http.MethodGet, "/api/v1/files", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("list status = %d: %s", response.Code, response.Body.String())
	}
	var result listingResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	for _, entry := range result.Entries {
		if entry.Name == name {
			if entry.Version == "" {
				t.Fatalf("listing entry %q has no version", name)
			}
			return entry.Version
		}
	}
	t.Fatalf("listing has no entry %q", name)
	return ""
}
