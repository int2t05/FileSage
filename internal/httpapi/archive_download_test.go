// 本文件验证原生 ZIP 下载接口的版本检查和响应格式。
package httpapi

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"filesage/internal/fsroot"
)

func TestDownloadArchiveUsesCurrentVersions(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootPath, "photos", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "photos", "photo.txt"), []byte("photo data"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	entries, err := root.List("")
	if err != nil {
		t.Fatal(err)
	}
	selection := []archiveSelection{{Path: "photos", Version: versionForEntry(entries[0])}}
	handler := New(root, nil)
	response := postArchiveSelection(t, handler, selection)
	if runtime.GOOS != "linux" {
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want platform fallback %d: %s", response.Code, http.StatusServiceUnavailable, response.Body.String())
		}
		return
	}
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}
	if response.Header().Get("Content-Type") != "application/zip" || !strings.Contains(response.Header().Get("Content-Disposition"), "filesage-selection.zip") {
		t.Fatalf("download headers = %#v", response.Header())
	}
	reader, err := zip.NewReader(bytes.NewReader(response.Body.Bytes()), int64(response.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	contentsByPath := make(map[string]string, len(reader.File))
	for _, file := range reader.File {
		if file.FileInfo().IsDir() {
			contentsByPath[file.Name] = ""
			continue
		}
		opened, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		contents, readErr := io.ReadAll(opened)
		closeErr := opened.Close()
		if readErr != nil || closeErr != nil {
			t.Fatalf("read %q: read=%v close=%v", file.Name, readErr, closeErr)
		}
		contentsByPath[file.Name] = string(contents)
	}
	if contentsByPath["photos/photo.txt"] != "photo data" {
		t.Fatalf("ZIP entries = %#v", contentsByPath)
	}
	if _, exists := contentsByPath["photos/empty/"]; !exists {
		t.Fatalf("ZIP did not preserve the empty directory: %#v", contentsByPath)
	}
}

func TestDownloadArchiveRejectsStaleVersion(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "item.txt"), []byte("ready"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	response := postArchiveSelection(t, New(root, nil), []archiveSelection{{Path: "item.txt", Version: "stale"}})
	if runtime.GOOS != "linux" {
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want platform fallback %d", response.Code, http.StatusServiceUnavailable)
		}
		return
	}
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusConflict, response.Body.String())
	}
	if !strings.Contains(response.Header().Get("Content-Type"), "text/html") || !strings.Contains(response.Body.String(), "刷新后重试") {
		t.Fatalf("stale response = %q, headers=%#v", response.Body.String(), response.Header())
	}
}

func TestDownloadArchiveRejectsInvalidForm(t *testing.T) {
	handler := New(nil, nil)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/files/archive", strings.NewReader("selection=not-json"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusBadRequest)
	}
}

func TestArchiveStorageErrorsAreRecoverable(t *testing.T) {
	status, _ := archiveErrorResponse(fsroot.ErrArchiveStorage)
	if status != http.StatusInsufficientStorage {
		t.Fatalf("archive storage status = %d, want %d", status, http.StatusInsufficientStorage)
	}
	status, message := archiveErrorResponse(fsroot.ErrArchiveIsolation)
	if status != http.StatusServiceUnavailable || message != "服务器无法安全启动归档工具，操作未执行。" {
		t.Fatalf("archive isolation status = %d, message = %q", status, message)
	}
}

func TestCollapseArchiveSelectionsPrunesInterleavedChildren(t *testing.T) {
	selections := []archiveSelection{{Path: "a"}, {Path: "a-b/x"}, {Path: "a/z"}}
	collapsed := collapseArchiveSelections(selections)
	want := []string{"a", "a-b/x"}
	if len(collapsed) != len(want) {
		t.Fatalf("collapsed paths = %#v, want %#v", collapsed, want)
	}
	for index := range want {
		if collapsed[index] != want[index] {
			t.Fatalf("collapsed paths = %#v, want %#v", collapsed, want)
		}
	}
}

func TestCleanupArchiveTempsRemovesOnlyOldManagedDirectories(t *testing.T) {
	tempRoot := t.TempDir()
	cutoff := time.Now().Add(-24 * time.Hour)
	stale := filepath.Join(tempRoot, ".filesage-archive-stale")
	if err := os.Mkdir(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	staleArchive := filepath.Join(stale, "archive.zip")
	if err := os.WriteFile(staleArchive, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(staleArchive, cutoff.Add(-time.Hour), cutoff.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(stale, cutoff.Add(-time.Hour), cutoff.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	staleTARGZ := filepath.Join(tempRoot, ".filesage-archive-stale-targz")
	if err := os.Mkdir(staleTARGZ, 0o700); err != nil {
		t.Fatal(err)
	}
	staleTARGZArchive := filepath.Join(staleTARGZ, "archive.tar.gz")
	if err := os.WriteFile(staleTARGZArchive, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(staleTARGZArchive, cutoff.Add(-time.Hour), cutoff.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(staleTARGZ, cutoff.Add(-time.Hour), cutoff.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	recent := filepath.Join(tempRoot, ".filesage-archive-recent")
	if err := os.Mkdir(recent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(recent, "archive.zip"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	active := filepath.Join(tempRoot, ".filesage-archive-active")
	if err := os.Mkdir(active, 0o700); err != nil {
		t.Fatal(err)
	}
	activeArchive := filepath.Join(active, "archive.zip")
	if err := os.WriteFile(activeArchive, []byte("active"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(activeArchive, cutoff.Add(-time.Hour), cutoff.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(active, cutoff.Add(-time.Hour), cutoff.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	stopTracking := trackArchiveTemp(active)
	defer stopTracking()
	extra := filepath.Join(tempRoot, ".filesage-archive-extra")
	if err := os.Mkdir(extra, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extra, "keep.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	cleanupArchiveTempsIn(tempRoot, cutoff)
	for _, removed := range []string{stale, staleTARGZ} {
		if _, err := os.Stat(removed); !os.IsNotExist(err) {
			t.Errorf("stale archive directory %q remains: %v", removed, err)
		}
	}
	for _, keep := range []string{recent, active, extra} {
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("managed but non-stale directory %q was removed: %v", keep, err)
		}
	}
}

func postArchiveSelection(t *testing.T, handler http.Handler, selections []archiveSelection) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(selections)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"selection": []string{string(encoded)}}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/files/archive", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
