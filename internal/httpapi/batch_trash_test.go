// 本文件验证批量回收站接口的版本、重叠选择与部分失败行为。
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"filesage/internal/fsroot"
	"filesage/internal/store"
	trashsvc "filesage/internal/trash"
)

func TestBatchTrashMovesFilesAndDirectories(t *testing.T) {
	requireLinuxTrash(t)
	rootPath, handler, manager := newBatchTrashHandler(t, "")
	if err := os.Mkdir(filepath.Join(rootPath, "folder"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "folder", "inside.txt"), []byte("inside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "single.txt"), []byte("single"), 0o644); err != nil {
		t.Fatal(err)
	}

	items := []batchTrashInput{
		{Path: "folder", Version: batchTrashVersion(t, handler, "folder")},
		{Path: "single.txt", Version: batchTrashVersion(t, handler, "single.txt")},
	}
	response := performBatchTrashRequest(handler, items, "")
	result := decodeBatchTrashResponse(t, response)
	if response.Code != http.StatusOK || result.Succeeded != 2 || result.Failed != 0 {
		t.Fatalf("response = %d %#v", response.Code, result)
	}
	if result.Items[0].Status != "trashed" || result.Items[1].Status != "trashed" {
		t.Fatalf("statuses = %#v", result.Items)
	}
	if _, err := os.Lstat(filepath.Join(rootPath, "folder")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("directory was not moved: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(rootPath, "single.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file was not moved: %v", err)
	}
	page, err := manager.List(context.Background(), 100, "")
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("trash entries = %d, err = %v", len(page.Items), err)
	}
	auditResponse := performRequest(handler, http.MethodGet, "/api/v1/audit", nil)
	if auditResponse.Code != http.StatusOK {
		t.Fatalf("audit history status = %d: %s", auditResponse.Code, auditResponse.Body.String())
	}
	var auditPage auditHistoryResponse
	if err := json.NewDecoder(auditResponse.Body).Decode(&auditPage); err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, event := range auditPage.Events {
		if event.EventType == "manual_batch_trash" {
			if event.Outcome != "succeeded" {
				t.Fatalf("trash audit outcome = %+v", event)
			}
			seen[event.SourcePath] = event.TargetPath
		}
	}
	if len(seen) != 2 || seen["folder"] != "" || seen["single.txt"] != "" {
		t.Fatalf("trash audit paths = %#v", seen)
	}
}

func TestBatchTrashRejectsStaleVersionWithoutMovingAnyItem(t *testing.T) {
	requireLinuxTrash(t)
	rootPath, handler, manager := newBatchTrashHandler(t, "")
	if err := os.WriteFile(filepath.Join(rootPath, "second.txt"), []byte("second"), 0o644); err != nil {
		t.Fatal(err)
	}
	items := []batchTrashInput{
		{Path: "hello.txt", Version: batchTrashVersion(t, handler, "hello.txt")},
		{Path: "second.txt", Version: "v1.forged-version"},
	}
	response := performBatchTrashRequest(handler, items, "")
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", response.Code, response.Body.String())
	}
	for _, name := range []string{"hello.txt", "second.txt"} {
		if _, err := os.Lstat(filepath.Join(rootPath, name)); err != nil {
			t.Fatalf("%s changed after failed preflight: %v", name, err)
		}
	}
	page, err := manager.List(context.Background(), 100, "")
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("trash entries = %d, err = %v", len(page.Items), err)
	}
}

func TestBatchTrashRejectsCrossOriginRequest(t *testing.T) {
	requireLinuxTrash(t)
	rootPath, handler, _ := newBatchTrashHandler(t, "")
	items := []batchTrashInput{{Path: "hello.txt", Version: batchTrashVersion(t, handler, "hello.txt")}}
	response := performBatchTrashRequest(handler, items, "http://attacker.test")
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", response.Code, response.Body.String())
	}
	if _, err := os.Lstat(filepath.Join(rootPath, "hello.txt")); err != nil {
		t.Fatalf("cross-origin request moved source: %v", err)
	}
}

func TestBatchTrashMarksSelectedChildIncludedInDirectory(t *testing.T) {
	requireLinuxTrash(t)
	rootPath, handler, manager := newBatchTrashHandler(t, "")
	if err := os.Mkdir(filepath.Join(rootPath, "folder"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "folder", "child.txt"), []byte("child"), 0o644); err != nil {
		t.Fatal(err)
	}
	items := []batchTrashInput{
		{Path: "folder/child.txt", Version: batchTrashVersion(t, handler, "folder/child.txt")},
		{Path: "folder", Version: batchTrashVersion(t, handler, "folder")},
	}
	response := performBatchTrashRequest(handler, items, "")
	result := decodeBatchTrashResponse(t, response)
	if response.Code != http.StatusOK || result.Succeeded != 2 || result.Failed != 0 {
		t.Fatalf("response = %d %#v", response.Code, result)
	}
	if result.Items[0].Status != "included" || result.Items[0].IncludedIn != "folder" || result.Items[1].Status != "trashed" {
		t.Fatalf("items = %#v", result.Items)
	}
	page, err := manager.List(context.Background(), 100, "")
	if err != nil || len(page.Items) != 1 || page.Items[0].OriginalPath != "folder" {
		t.Fatalf("trash page = %#v, err = %v", page, err)
	}
}

func TestBatchTrashContinuesAfterIndependentFailure(t *testing.T) {
	requireLinuxTrash(t)
	rootPath, handler, manager := newBatchTrashHandler(t, "folder")
	if err := os.Mkdir(filepath.Join(rootPath, "folder"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "folder", "child.txt"), []byte("child"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "other.txt"), []byte("other"), 0o644); err != nil {
		t.Fatal(err)
	}
	items := []batchTrashInput{
		{Path: "folder/child.txt", Version: batchTrashVersion(t, handler, "folder/child.txt")},
		{Path: "other.txt", Version: batchTrashVersion(t, handler, "other.txt")},
		{Path: "folder", Version: batchTrashVersion(t, handler, "folder")},
	}
	response := performBatchTrashRequest(handler, items, "")
	result := decodeBatchTrashResponse(t, response)
	if response.Code != http.StatusMultiStatus || result.Succeeded != 1 || result.Failed != 2 {
		t.Fatalf("response = %d %#v", response.Code, result)
	}
	if result.Items[0].Status != "failed" || result.Items[1].Status != "trashed" || result.Items[2].Status != "failed" {
		t.Fatalf("items = %#v", result.Items)
	}
	if strings.Contains(response.Body.String(), "SECRET_INTERNAL") {
		t.Fatal("response exposed internal service error")
	}
	if _, err := os.Lstat(filepath.Join(rootPath, "folder")); err != nil {
		t.Fatalf("failed ancestor disappeared: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(rootPath, "other.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("independent item was not moved: %v", err)
	}
	page, err := manager.List(context.Background(), 100, "")
	if err != nil || len(page.Items) != 1 || page.Items[0].OriginalPath != "other.txt" {
		t.Fatalf("trash page = %#v, err = %v", page, err)
	}
}

func TestBatchTrashRejectsDuplicatePaths(t *testing.T) {
	_, handler, _ := newBatchTrashHandler(t, "")
	version := batchTrashVersion(t, handler, "hello.txt")
	response := performBatchTrashRequest(handler, []batchTrashInput{
		{Path: "hello.txt", Version: version},
		{Path: "hello.txt", Version: version},
	}, "")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", response.Code, response.Body.String())
	}
}

func TestBatchTrashRejectsMoreThan256Items(t *testing.T) {
	_, handler, _ := newBatchTrashHandler(t, "")
	items := make([]batchTrashInput, maxBatchTrashItems+1)
	for index := range items {
		items[index] = batchTrashInput{Path: "missing/" + strconv.Itoa(index), Version: "v1.fake"}
	}
	response := performBatchTrashRequest(handler, items, "")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", response.Code, response.Body.String())
	}
}

type failingBatchTrashService struct {
	trashService
	failPath string
}

func (service failingBatchTrashService) MoveToTrash(ctx context.Context, entry fsroot.Entry) (trashsvc.Item, error) {
	if entry.Path == service.failPath {
		return trashsvc.Item{}, errors.New("SECRET_INTERNAL filesystem location")
	}
	return service.trashService.MoveToTrash(ctx, entry)
}

func newBatchTrashHandler(t *testing.T, failPath string) (string, http.Handler, *trashsvc.Manager) {
	t.Helper()
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "hello.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	database, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "filesage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	manager, err := trashsvc.Open(context.Background(), root, database)
	if err != nil {
		t.Fatal(err)
	}
	var service trashService = manager
	if failPath != "" {
		service = failingBatchTrashService{trashService: manager, failPath: failPath}
	}
	return rootPath, NewWithTrash(root, database, service, nil, nil), manager
}

func batchTrashVersion(t *testing.T, handler http.Handler, relativePath string) string {
	t.Helper()
	parent := pathParent(relativePath)
	target := "/api/v1/files"
	if parent != "" {
		target += "?path=" + url.QueryEscape(parent)
	}
	response := performRequest(handler, http.MethodGet, target, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("list %q status = %d: %s", parent, response.Code, response.Body.String())
	}
	var page listingResponse
	if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	for _, entry := range page.Entries {
		if entry.Path == relativePath {
			return entry.Version
		}
	}
	t.Fatalf("listing has no entry %q", relativePath)
	return ""
}

func pathParent(relativePath string) string {
	if index := strings.LastIndexByte(relativePath, '/'); index >= 0 {
		return relativePath[:index]
	}
	return ""
}

func performBatchTrashRequest(handler http.Handler, items []batchTrashInput, origin string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(batchTrashRequest{Items: items})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/files/batch-trash", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func decodeBatchTrashResponse(t *testing.T, response *httptest.ResponseRecorder) batchTrashResponse {
	t.Helper()
	var result batchTrashResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v (%s)", err, response.Body.String())
	}
	return result
}

func requireLinuxTrash(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("真实回收站移动仅在 Linux 上验证")
	}
}
