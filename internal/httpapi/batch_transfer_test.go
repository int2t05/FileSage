// 本文件验证批量复制与移动的预检、编号和逐项结果。
package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"filesage/internal/coordination"
	"filesage/internal/fsroot"
	"filesage/internal/store"
)

type batchTransferTestRequest struct {
	Items              []batchTransferInput `json:"items"`
	Destination        string               `json:"destination"`
	DestinationVersion string               `json:"destinationVersion"`
}

func newBatchTransferTestRoot(t *testing.T) (string, *fsroot.Root) {
	t.Helper()
	rootPath := t.TempDir()
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return rootPath, root
}

func newBatchTransferTestHandler(files fileSystem, index fileIndexer) http.Handler {
	return NewWithAgentAndIndex(files, nil, nil, nil, index, nil, coordination.New())
}

func batchTransferBody(t *testing.T, items []batchTransferInput, destination, destinationVersion string) string {
	t.Helper()
	data, err := json.Marshal(batchTransferTestRequest{
		Items: items, Destination: destination, DestinationVersion: destinationVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func decodeBatchTransferResponse(t *testing.T, responseBody io.Reader) batchTransferResponse {
	t.Helper()
	var result batchTransferResponse
	if err := json.NewDecoder(responseBody).Decode(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

func postBatchTransfer(t *testing.T, handler http.Handler, endpoint string, items []batchTransferInput, destination, destinationVersion string) *httptest.ResponseRecorder {
	t.Helper()
	return performJSONRequest(handler, http.MethodPost, endpoint, batchTransferBody(t, items, destination, destinationVersion))
}

func batchTransferInputForPath(t *testing.T, handler http.Handler, relativePath string) batchTransferInput {
	t.Helper()
	return batchTransferInput{Path: relativePath, Version: versionFromPathListing(t, handler, relativePath)}
}

// TestBatchCopyNumbersConflictsWithoutOverwriting 验证多个来源重名时逐项编号。
func TestBatchCopyNumbersConflictsWithoutOverwriting(t *testing.T) {
	requireLinuxTreeCopy(t)
	rootPath, root := newBatchTransferTestRoot(t)
	for _, directory := range []string{"first", "second", "destination"} {
		if err := os.Mkdir(filepath.Join(rootPath, directory), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, contents := range map[string]string{
		"first/report.txt":       "first source",
		"second/report.txt":      "second source",
		"destination/report.txt": "keep existing",
	} {
		if err := os.WriteFile(filepath.Join(rootPath, filepath.FromSlash(name)), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	index := &batchRenameTestIndexer{}
	handler := newBatchTransferTestHandler(root, index)
	items := []batchTransferInput{
		batchTransferInputForPath(t, handler, "first/report.txt"),
		batchTransferInputForPath(t, handler, "second/report.txt"),
	}
	destinationVersion := versionFromPathListing(t, handler, "destination")
	response := postBatchTransfer(t, handler, "/api/v1/files/batch-copy", items, "destination", destinationVersion)
	if response.Code != http.StatusOK {
		t.Fatalf("batch copy status = %d: %s", response.Code, response.Body.String())
	}
	result := decodeBatchTransferResponse(t, response.Body)
	if result.Status != "complete" || len(result.Items) != 2 {
		t.Fatalf("batch copy result = %#v", result)
	}
	if result.Items[0].TargetPath != "destination/report (1).txt" || result.Items[1].TargetPath != "destination/report (2).txt" {
		t.Fatalf("numbered targets = %#v", result.Items)
	}
	for relativePath, contents := range map[string]string{
		"destination/report.txt":     "keep existing",
		"destination/report (1).txt": "first source",
		"destination/report (2).txt": "second source",
		"first/report.txt":           "first source",
		"second/report.txt":          "second source",
	} {
		data, err := os.ReadFile(filepath.Join(rootPath, filepath.FromSlash(relativePath)))
		if err != nil || string(data) != contents {
			t.Errorf("%s = %q, %v; want %q", relativePath, data, err, contents)
		}
	}
	if index.refreshes != 1 {
		t.Fatalf("index refreshes = %d, want one", index.refreshes)
	}
}

// TestBatchTransferPreflightRejectsStaleSourceWithoutWrites 验证版本错误不会部分写入。
func TestBatchTransferPreflightRejectsStaleSourceWithoutWrites(t *testing.T) {
	requireLinuxTreeCopy(t)
	rootPath, root := newBatchTransferTestRoot(t)
	if err := os.Mkdir(filepath.Join(rootPath, "destination"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"first.txt", "second.txt"} {
		if err := os.WriteFile(filepath.Join(rootPath, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	handler := newBatchTransferTestHandler(root, nil)
	first := batchTransferInputForPath(t, handler, "first.txt")
	second := batchTransferInputForPath(t, handler, "second.txt")
	second.Version = "stale"
	response := postBatchTransfer(t, handler, "/api/v1/files/batch-copy", []batchTransferInput{first, second}, "", "")
	if response.Code != http.StatusConflict {
		t.Fatalf("stale preflight status = %d: %s", response.Code, response.Body.String())
	}
	for _, name := range []string{"first.txt", "second.txt"} {
		if _, err := os.Stat(filepath.Join(rootPath, name)); err != nil {
			t.Errorf("source %s changed: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(rootPath, "first (1).txt")); !os.IsNotExist(err) {
		t.Fatalf("preflight created a target: %v", err)
	}
}

// TestBatchTransferPreflightRejectsStaleDestinationWithoutWrites 验证目标版本仅在首项写入前校验。
func TestBatchTransferPreflightRejectsStaleDestinationWithoutWrites(t *testing.T) {
	requireLinuxTreeCopy(t)
	rootPath, root := newBatchTransferTestRoot(t)
	if err := os.Mkdir(filepath.Join(rootPath, "destination"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "source.txt"), []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	handler := newBatchTransferTestHandler(root, nil)
	item := batchTransferInputForPath(t, handler, "source.txt")
	destinationVersion := versionFromPathListing(t, handler, "destination")
	if err := os.WriteFile(filepath.Join(rootPath, "destination", "external.txt"), []byte("external"), 0o600); err != nil {
		t.Fatal(err)
	}
	response := postBatchTransfer(t, handler, "/api/v1/files/batch-copy", []batchTransferInput{item}, "destination", destinationVersion)
	if response.Code != http.StatusConflict {
		t.Fatalf("stale destination status = %d: %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(filepath.Join(rootPath, "destination", "source.txt")); !os.IsNotExist(err) {
		t.Fatalf("stale destination created a target: %v", err)
	}
}

// TestBatchCopyFoldsSelectedDescendantIntoSelectedDirectory 验证所选父目录覆盖其后代。
func TestBatchCopyFoldsSelectedDescendantIntoSelectedDirectory(t *testing.T) {
	requireLinuxTreeCopy(t)
	rootPath, root := newBatchTransferTestRoot(t)
	if err := os.MkdirAll(filepath.Join(rootPath, "parent", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(rootPath, "destination"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "parent", "nested", "note.txt"), []byte("contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "audit.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	handler := NewWithAgentAndIndex(root, metadata, nil, nil, nil, nil, coordination.New())
	child := batchTransferInputForPath(t, handler, "parent/nested/note.txt")
	parent := batchTransferInputForPath(t, handler, "parent")
	destinationVersion := versionFromPathListing(t, handler, "destination")
	response := postBatchTransfer(t, handler, "/api/v1/files/batch-copy", []batchTransferInput{child, parent}, "destination", destinationVersion)
	if response.Code != http.StatusOK {
		t.Fatalf("folded copy status = %d: %s", response.Code, response.Body.String())
	}
	result := decodeBatchTransferResponse(t, response.Body)
	if result.Items[0].Status != "included" || result.Items[0].IncludedIn != "parent" || result.Items[0].TargetPath != "destination/parent/nested/note.txt" {
		t.Fatalf("included child result = %#v", result.Items[0])
	}
	if result.Items[1].Status != "copied" || result.Items[1].TargetPath != "destination/parent" {
		t.Fatalf("parent result = %#v", result.Items[1])
	}
	data, err := os.ReadFile(filepath.Join(rootPath, "destination", "parent", "nested", "note.txt"))
	if err != nil || string(data) != "contents" {
		t.Fatalf("copied tree contents = %q, %v", data, err)
	}
	page, err := metadata.AuditHistoryPage(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var events []store.AuditRecord
	for _, event := range page.Events {
		if event.EventType == "manual_batch_copy" {
			events = append(events, event)
		}
	}
	if len(events) != 1 || events[0].SourcePath != "parent" || events[0].TargetPath != "destination/parent" || events[0].Outcome != "succeeded" {
		t.Fatalf("folded copy audit events = %#v", events)
	}
}

type batchTransferFailTreeCopyFS struct{ *fsroot.Root }

func (fileSystem *batchTransferFailTreeCopyFS) CopyTreeNoReplace(context.Context, string, string, fsroot.TreeSnapshot) (fsroot.TreeCopyResult, error) {
	return fsroot.TreeCopyResult{}, fs.ErrPermission
}

// TestBatchCopyMarksFoldedChildrenFailedWhenParentCannotPublish 验证父目录失败时子项不单独执行。
func TestBatchCopyMarksFoldedChildrenFailedWhenParentCannotPublish(t *testing.T) {
	requireLinuxTreeCopy(t)
	rootPath, root := newBatchTransferTestRoot(t)
	if err := os.MkdirAll(filepath.Join(rootPath, "parent", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "parent", "nested", "note.txt"), []byte("contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "later.txt"), []byte("later"), 0o600); err != nil {
		t.Fatal(err)
	}
	fileSystem := &batchTransferFailTreeCopyFS{Root: root}
	handler := newBatchTransferTestHandler(fileSystem, nil)
	child := batchTransferInputForPath(t, handler, "parent/nested/note.txt")
	parent := batchTransferInputForPath(t, handler, "parent")
	later := batchTransferInputForPath(t, handler, "later.txt")
	response := postBatchTransfer(t, handler, "/api/v1/files/batch-copy", []batchTransferInput{child, parent, later}, "", "")
	if response.Code != http.StatusMultiStatus {
		t.Fatalf("failed parent status = %d: %s", response.Code, response.Body.String())
	}
	result := decodeBatchTransferResponse(t, response.Body)
	if result.Items[0].Status != "failed" || result.Items[1].Status != "failed" || result.Items[2].Status != "not_started" {
		t.Fatalf("failed parent results = %#v", result.Items)
	}
	if _, err := os.Stat(filepath.Join(rootPath, "parent (1)")); !os.IsNotExist(err) {
		t.Fatalf("failed directory copy published a target: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rootPath, "later (1).txt")); !os.IsNotExist(err) {
		t.Fatalf("later item ran after parent failure: %v", err)
	}
}

// TestBatchTransferRejectsDestinationInsideAnySelectedDirectory 验证目录目标不能落入所选树。
func TestBatchTransferRejectsDestinationInsideAnySelectedDirectory(t *testing.T) {
	requireLinuxTreeCopy(t)
	for _, endpoint := range []string{"/api/v1/files/batch-copy", "/api/v1/files/batch-move"} {
		for _, destination := range []string{"source", "source/nested"} {
			t.Run(filepath.Base(endpoint)+"/"+filepath.Base(destination), func(t *testing.T) {
				rootPath, root := newBatchTransferTestRoot(t)
				if err := os.MkdirAll(filepath.Join(rootPath, "source", "nested"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(rootPath, "source", "item.txt"), []byte("item"), 0o600); err != nil {
					t.Fatal(err)
				}
				handler := newBatchTransferTestHandler(root, nil)
				item := batchTransferInputForPath(t, handler, "source")
				destinationVersion := versionFromPathListing(t, handler, destination)
				response := postBatchTransfer(t, handler, endpoint, []batchTransferInput{item}, destination, destinationVersion)
				if response.Code != http.StatusBadRequest {
					t.Fatalf("destination %q status = %d: %s", destination, response.Code, response.Body.String())
				}
				if _, err := os.Stat(filepath.Join(rootPath, "source", "item.txt")); err != nil {
					t.Fatalf("rejected request changed the source: %v", err)
				}
			})
		}
	}
}

// TestBatchMoveRejectsCurrentParent 验证移动到来源当前目录会在预检时被拒绝。
func TestBatchMoveRejectsCurrentParent(t *testing.T) {
	requireLinuxTreeCopy(t)
	rootPath, root := newBatchTransferTestRoot(t)
	if err := os.Mkdir(filepath.Join(rootPath, "folder"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "folder", "item.txt"), []byte("item"), 0o600); err != nil {
		t.Fatal(err)
	}
	handler := newBatchTransferTestHandler(root, nil)
	item := batchTransferInputForPath(t, handler, "folder/item.txt")
	destinationVersion := versionFromPathListing(t, handler, "folder")
	response := postBatchTransfer(t, handler, "/api/v1/files/batch-move", []batchTransferInput{item}, "folder", destinationVersion)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("same-parent move status = %d: %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(filepath.Join(rootPath, "folder", "item.txt")); err != nil {
		t.Fatalf("rejected move changed the source: %v", err)
	}
}

// TestBatchMoveHandlesFilesAndDirectoriesAfterOneDestinationCheck 验证目录写入后仍可继续同批移动。
func TestBatchMoveHandlesFilesAndDirectoriesAfterOneDestinationCheck(t *testing.T) {
	requireLinuxTreeCopy(t)
	rootPath, root := newBatchTransferTestRoot(t)
	if err := os.MkdirAll(filepath.Join(rootPath, "source-dir", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(rootPath, "destination"), 0o755); err != nil {
		t.Fatal(err)
	}
	for relativePath, contents := range map[string]string{
		"single.txt":                 "single file",
		"source-dir/nested/note.txt": "directory file",
	} {
		if err := os.WriteFile(filepath.Join(rootPath, filepath.FromSlash(relativePath)), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	handler := newBatchTransferTestHandler(root, nil)
	items := []batchTransferInput{
		batchTransferInputForPath(t, handler, "single.txt"),
		batchTransferInputForPath(t, handler, "source-dir"),
	}
	destinationVersion := versionFromPathListing(t, handler, "destination")
	response := postBatchTransfer(t, handler, "/api/v1/files/batch-move", items, "destination", destinationVersion)
	if response.Code != http.StatusOK {
		t.Fatalf("file and directory move status = %d: %s", response.Code, response.Body.String())
	}
	result := decodeBatchTransferResponse(t, response.Body)
	if result.Status != "complete" || result.Items[0].Status != "moved" || result.Items[1].Status != "moved" {
		t.Fatalf("file and directory move result = %#v", result)
	}
	for _, source := range []string{"single.txt", "source-dir"} {
		if _, err := os.Lstat(filepath.Join(rootPath, source)); !os.IsNotExist(err) {
			t.Errorf("source %s remains after move: %v", source, err)
		}
	}
	for relativePath, contents := range map[string]string{
		"destination/single.txt":                 "single file",
		"destination/source-dir/nested/note.txt": "directory file",
	} {
		data, err := os.ReadFile(filepath.Join(rootPath, filepath.FromSlash(relativePath)))
		if err != nil || string(data) != contents {
			t.Errorf("%s = %q, %v; want %q", relativePath, data, err, contents)
		}
	}
}

type batchTransferMutatingFS struct {
	*fsroot.Root
	rootPath string
	mutated  bool
}

func (fileSystem *batchTransferMutatingFS) CopyFileNoReplace(ctx context.Context, source, destination string, expected fsroot.Fingerprint) (fsroot.FileCopyResult, error) {
	result, err := fileSystem.Root.CopyFileNoReplace(ctx, source, destination, expected)
	if err == nil && result.Published && !fileSystem.mutated {
		fileSystem.mutated = true
		err = os.WriteFile(filepath.Join(fileSystem.rootPath, "second.txt"), []byte("changed after first copy"), 0o600)
	}
	return result, err
}

// TestBatchCopyStopsAfterAChangedSourceAndMarksLaterItemsUnstarted 验证执行期冲突会停止后续项。
func TestBatchCopyStopsAfterAChangedSourceAndMarksLaterItemsUnstarted(t *testing.T) {
	requireLinuxTreeCopy(t)
	rootPath, root := newBatchTransferTestRoot(t)
	for _, name := range []string{"first.txt", "second.txt", "third.txt"} {
		if err := os.WriteFile(filepath.Join(rootPath, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fileSystem := &batchTransferMutatingFS{Root: root, rootPath: rootPath}
	handler := newBatchTransferTestHandler(fileSystem, nil)
	items := []batchTransferInput{
		batchTransferInputForPath(t, handler, "first.txt"),
		batchTransferInputForPath(t, handler, "second.txt"),
		batchTransferInputForPath(t, handler, "third.txt"),
	}
	response := postBatchTransfer(t, handler, "/api/v1/files/batch-copy", items, "", "")
	if response.Code != http.StatusMultiStatus {
		t.Fatalf("changed-source response = %d: %s", response.Code, response.Body.String())
	}
	result := decodeBatchTransferResponse(t, response.Body)
	if result.Status != "partial" || result.Items[0].Status != "copied" || result.Items[1].Status != "failed" || result.Items[2].Status != "not_started" {
		t.Fatalf("changed-source results = %#v", result)
	}
	if _, err := os.Stat(filepath.Join(rootPath, "first (1).txt")); err != nil {
		t.Fatalf("first copy missing: %v", err)
	}
	for _, name := range []string{"second (1).txt", "third (1).txt"} {
		if _, err := os.Stat(filepath.Join(rootPath, name)); !os.IsNotExist(err) {
			t.Errorf("unexpected target %s: %v", name, err)
		}
	}
}

type batchTransferPartialMoveFS struct{ *fsroot.Root }

func (fileSystem *batchTransferPartialMoveFS) MoveFileNoReplace(ctx context.Context, source, destination string, expected fsroot.Fingerprint) (fsroot.FileMoveResult, error) {
	result, err := fileSystem.Root.CopyFileNoReplace(ctx, source, destination, expected)
	return fsroot.FileMoveResult{Published: result.Published}, err
}

// TestBatchMoveReportsPublishedCopyWhenSourceRemovalFails 验证跨挂载式部分移动明确报告两处状态。
func TestBatchMoveReportsPublishedCopyWhenSourceRemovalFails(t *testing.T) {
	requireLinuxTreeCopy(t)
	rootPath, root := newBatchTransferTestRoot(t)
	if err := os.Mkdir(filepath.Join(rootPath, "destination"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "source.txt"), []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	fileSystem := &batchTransferPartialMoveFS{Root: root}
	handler := newBatchTransferTestHandler(fileSystem, nil)
	item := batchTransferInputForPath(t, handler, "source.txt")
	destinationVersion := versionFromPathListing(t, handler, "destination")
	response := postBatchTransfer(t, handler, "/api/v1/files/batch-move", []batchTransferInput{item}, "destination", destinationVersion)
	if response.Code != http.StatusMultiStatus {
		t.Fatalf("partial move status = %d: %s", response.Code, response.Body.String())
	}
	result := decodeBatchTransferResponse(t, response.Body)
	if result.Status != "partial" || result.Items[0].Status != "partial" || result.Items[0].Message == "" {
		t.Fatalf("partial move result = %#v", result)
	}
	if _, err := os.Stat(filepath.Join(rootPath, "source.txt")); err != nil {
		t.Fatalf("source should remain after partial move: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(rootPath, "destination", "source.txt")); err != nil || string(data) != "source" {
		t.Fatalf("published copy = %q, %v", data, err)
	}
}
