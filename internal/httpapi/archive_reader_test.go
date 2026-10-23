// 本文件验证压缩包 API 的版本、读取和解压响应。
package httpapi

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"filesage/internal/coordination"
	"filesage/internal/fsroot"
	"filesage/internal/store"
)

func TestArchiveReadSlotsLimitConcurrencyAndHonorCancellation(t *testing.T) {
	api := &API{archiveReadSlots: make(chan struct{}, maxConcurrentArchiveReads)}
	if cap(api.archiveReadSlots) != maxConcurrentArchiveReads {
		t.Fatalf("archive read slots = %d, want %d", cap(api.archiveReadSlots), maxConcurrentArchiveReads)
	}
	releases := make([]func(), 0, maxConcurrentArchiveReads)
	for range maxConcurrentArchiveReads {
		release, ok := api.acquireArchiveReadSlot(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		if !ok {
			t.Fatal("available archive slot was rejected")
		}
		releases = append(releases, release)
	}
	defer func() {
		for _, release := range releases {
			release()
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	if _, ok := api.acquireArchiveReadSlot(response, request); ok {
		t.Fatal("cancelled request acquired an occupied archive slot")
	}
	if response.Code != http.StatusRequestTimeout {
		t.Fatalf("cancelled response status = %d, want %d", response.Code, http.StatusRequestTimeout)
	}
}

func TestArchiveEntriesAndContentRoutes(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("stable file identity validation is available on Linux")
	}
	root := openArchiveHTTPTestRoot(t)
	writeArchiveHTTPTestZIP(t, root, "bundle.zip", map[string]string{"docs/brief.txt": "review me", "plain.txt": "text"})
	version := archiveHTTPTestVersion(t, root, "bundle.zip")
	handler := New(root, nil)

	entriesURL := "/api/v1/files/archive/entries?path=bundle.zip&version=" + url.QueryEscape(version) + "&directory="
	entriesResponse := serveArchiveHTTPTest(t, handler, http.MethodGet, entriesURL, nil, "")
	if entriesResponse.Code != http.StatusOK {
		t.Fatalf("entries status = %d: %s", entriesResponse.Code, entriesResponse.Body.String())
	}
	var page archiveEntriesResponse
	if err := json.Unmarshal(entriesResponse.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.ArchivePath != "bundle.zip" || page.Version != version || page.Directory != "" || len(page.Entries) != 2 || page.Entries[0].Path != "docs" {
		t.Fatalf("entries response = %#v", page)
	}

	contentURL := "/api/v1/files/archive/content?path=bundle.zip&version=" + url.QueryEscape(version) + "&entry=docs%2Fbrief.txt"
	contentResponse := serveArchiveHTTPTest(t, handler, http.MethodGet, contentURL, nil, "")
	if contentResponse.Code != http.StatusOK || contentResponse.Body.String() != "review me" {
		t.Fatalf("content status/body = %d / %q", contentResponse.Code, contentResponse.Body.String())
	}
	if contentResponse.Header().Get("Cache-Control") != "no-store" || contentResponse.Header().Get("X-FileSage-Version") != version || !strings.HasPrefix(contentResponse.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("content headers = %#v", contentResponse.Header())
	}
}

func TestArchiveRoutesRejectStaleVersionsAndUnsafeQueries(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("stable file identity validation is available on Linux")
	}
	root := openArchiveHTTPTestRoot(t)
	writeArchiveHTTPTestZIP(t, root, "bundle.zip", map[string]string{"brief.txt": "body"})
	handler := New(root, nil)
	stale := serveArchiveHTTPTest(t, handler, http.MethodGet, "/api/v1/files/archive/entries?path=bundle.zip&version=stale&directory=", nil, "")
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale status = %d: %s", stale.Code, stale.Body.String())
	}
	unsafe := serveArchiveHTTPTest(t, handler, http.MethodGet, "/api/v1/files/archive/entries?path=bundle.zip&version=stale&directory=..%2Fprivate", nil, "")
	if unsafe.Code != http.StatusBadRequest {
		t.Fatalf("unsafe path status = %d: %s", unsafe.Code, unsafe.Body.String())
	}
	unknown := serveArchiveHTTPTest(t, handler, http.MethodGet, "/api/v1/files/archive/entries?path=bundle.zip&version=stale&directory=&x=y", nil, "")
	if unknown.Code != http.StatusBadRequest {
		t.Fatalf("unknown query status = %d", unknown.Code)
	}
}

func TestExtractArchiveRouteRequiresSameOriginAndReturnsCompleteResult(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("safe no-replace extraction is only implemented on Linux")
	}
	root := openArchiveHTTPTestRoot(t)
	writeArchiveHTTPTestZIP(t, root, "bundle.zip", map[string]string{"notes.txt": "archive"})
	if err := writeArchiveHTTPTestFile(t, root, "notes.txt", []byte("existing")); err != nil {
		t.Fatal(err)
	}
	version := archiveHTTPTestVersion(t, root, "bundle.zip")
	metadata, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "audit.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	handler := NewWithTaskStore(root, metadata, nil, coordination.New())
	body, err := json.Marshal(archiveExtractRequest{Path: "bundle.zip", Version: version, Entries: []string{}})
	if err != nil {
		t.Fatal(err)
	}

	blocked := serveArchiveHTTPTest(t, handler, http.MethodPost, "/api/v1/files/archive/extract", body, "https://attacker.invalid")
	if blocked.Code != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d: %s", blocked.Code, blocked.Body.String())
	}
	complete := serveArchiveHTTPTest(t, handler, http.MethodPost, "/api/v1/files/archive/extract", body, "")
	if complete.Code != http.StatusCreated {
		t.Fatalf("extract status = %d: %s", complete.Code, complete.Body.String())
	}
	var result archiveExtractResponse
	if err := json.Unmarshal(complete.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "complete" || len(result.Paths) != 1 || result.Paths[0] != "notes (1).txt" {
		t.Fatalf("extract response = %#v", result)
	}
	content := readArchiveHTTPTestFile(t, root, "notes (1).txt")
	if string(content) != "archive" {
		t.Fatalf("published content = %q, error = %v", content, err)
	}
	page, err := metadata.AuditHistoryPage(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var events []store.AuditRecord
	for _, event := range page.Events {
		if event.EventType == "manual_archive_extract" {
			events = append(events, event)
		}
	}
	if len(events) != 1 || events[0].SourcePath != "bundle.zip" || events[0].TargetPath != "" || events[0].Outcome != "succeeded" {
		t.Fatalf("archive extraction audit events = %#v", events)
	}
}

func TestArchiveReadErrorMessagesAreSpecific(t *testing.T) {
	status, message := archiveReadErrorMessage(fsroot.ErrArchiveEncrypted)
	if status != http.StatusUnprocessableEntity || message == "" {
		t.Fatalf("encrypted mapping = %d, %q", status, message)
	}
	status, message = archiveReadErrorMessage(fsroot.ErrArchiveIsolation)
	if status != http.StatusServiceUnavailable || message != "服务器无法安全启动归档工具，操作未执行。" {
		t.Fatalf("isolation mapping = %d, %q", status, message)
	}
}

func archiveReadErrorMessage(err error) (int, string) {
	response := httptest.NewRecorder()
	writeArchiveReadError(response, err)
	var payload map[string]string
	_ = json.Unmarshal(response.Body.Bytes(), &payload)
	return response.Code, payload["error"]
}

func openArchiveHTTPTestRoot(t *testing.T) *fsroot.Root {
	t.Helper()
	root, err := fsroot.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}

func writeArchiveHTTPTestZIP(t *testing.T, root *fsroot.Root, name string, entries map[string]string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("atomic root writes are only implemented on Linux")
	}
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for path, content := range entries {
		file, err := writer.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(file, content); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writeArchiveHTTPTestFile(t, root, name, buffer.Bytes()); err != nil {
		t.Fatal(err)
	}
}

func writeArchiveHTTPTestFile(t *testing.T, root *fsroot.Root, name string, content []byte) error {
	t.Helper()
	_, err := root.WriteNewAtomicFrom(name, bytes.NewReader(content), int64(len(content)), 0o644)
	return err
}

func readArchiveHTTPTestFile(t *testing.T, root *fsroot.Root, name string) []byte {
	t.Helper()
	file, _, err := root.OpenRead(name)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	content, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func archiveHTTPTestVersion(t *testing.T, root *fsroot.Root, name string) string {
	t.Helper()
	entries, err := root.List("")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Path == name {
			return versionForEntry(entry)
		}
	}
	t.Fatalf("archive %q missing", name)
	return ""
}

func serveArchiveHTTPTest(t *testing.T, handler http.Handler, method, target string, body []byte, origin string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, bytes.NewReader(body))
	if method == http.MethodPost {
		request.Header.Set("Content-Type", "application/json")
	}
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestArchiveExtractRequestRejectsOversizedBody(t *testing.T) {
	handler := New(nil, nil)
	body := `{"entries":["` + strings.Repeat("x", maxArchiveExtractBodyBytes) + `"]}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/files/archive/extract", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestArchiveExtractRequestHasNoTrailingJSON(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"path":"a.zip","version":"v","destination":""} {}`))
	response := httptest.NewRecorder()
	if _, ok := parseArchiveExtractRequest(response, request); ok || response.Code != http.StatusBadRequest {
		t.Fatalf("trailing JSON accepted: status=%d", response.Code)
	}
}

func TestArchiveExtractIndexRefreshClassification(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/api/v1/files/archive/extract", strings.NewReader("{}"))
	if !isIndexedMutation(request) {
		t.Fatal("archive extraction does not refresh the file index")
	}
}
