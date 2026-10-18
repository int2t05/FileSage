package httpapi

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"filesage/internal/coordination"
	"filesage/internal/fsroot"
	"filesage/internal/office"
)

func TestOfficePreviewRejectsInvalidQueryAndUnsupportedFormat(t *testing.T) {
	api, root, _ := newOfficePreviewAPI(t, map[string][]byte{"document.docx": []byte("not-a-docx"), "notes.txt": []byte("text")})
	version := officeVersion(t, root, "document.docx")
	tests := []struct {
		name     string
		query    string
		wantCode int
	}{
		{name: "missing version", query: "path=document.docx", wantCode: http.StatusBadRequest},
		{name: "unsupported format", query: "path=notes.txt", wantCode: http.StatusUnsupportedMediaType},
		{name: "repeated path", query: "path=document.docx&path=document.docx&version=" + url.QueryEscape(version), wantCode: http.StatusBadRequest},
		{name: "unknown parameter", query: "path=document.docx&version=" + url.QueryEscape(version) + "&extra=x", wantCode: http.StatusBadRequest},
		{name: "unclean path", query: "path=..%2Fdocument.docx&version=" + url.QueryEscape(version), wantCode: http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := callOfficePreview(api, test.query)
			if response.Code != test.wantCode {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.wantCode, response.Body.String())
			}
		})
	}
}

func TestOfficePreviewRejectsStaleVersionAndOversizedFiles(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("fsroot only exposes stable file identities on Linux")
	}
	api, root, rootPath := newOfficePreviewAPI(t, map[string][]byte{"document.docx": []byte("old")})
	staleVersion := officeVersion(t, root, "document.docx")
	if err := os.WriteFile(filepath.Join(rootPath, "document.docx"), []byte("new content"), 0o600); err != nil {
		t.Fatal(err)
	}
	query := "path=document.docx&version=" + url.QueryEscape(staleVersion)
	if response := callOfficePreview(api, query); response.Code != http.StatusConflict {
		t.Fatalf("stale version status = %d, want %d: %s", response.Code, http.StatusConflict, response.Body.String())
	}

	largePath := filepath.Join(rootPath, "large.docx")
	if err := os.WriteFile(largePath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(largePath, office.MaxInputBytes+1); err != nil {
		t.Fatal(err)
	}
	largeVersion := officeVersion(t, root, "large.docx")
	largeQuery := "path=large.docx&version=" + url.QueryEscape(largeVersion)
	if response := callOfficePreview(api, largeQuery); response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized file status = %d, want %d: %s", response.Code, http.StatusRequestEntityTooLarge, response.Body.String())
	}
}

func TestOfficePreviewRejectsExcessConcurrencyBeforeReadingFiles(t *testing.T) {
	api, _, _ := newOfficePreviewAPI(t, nil)
	for range office.MaxConcurrentPreviews {
		release, ok := tryAcquireOfficePreviewSlot()
		if !ok {
			t.Fatal("failed to reserve office preview slot")
		}
		defer release()
	}
	response := callOfficePreview(api, "path=missing.docx&version=opaque")
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("busy status = %d, want %d: %s", response.Code, http.StatusServiceUnavailable, response.Body.String())
	}
	if got := response.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1", got)
	}
}

func TestOfficePreviewRouteIsRegistered(t *testing.T) {
	rootPath := t.TempDir()
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	handler := NewWithAgentAndIndexAndConversation(root, nil, nil, nil, nil, nil, nil, coordination.New())
	request := httptest.NewRequest(http.MethodGet, "/api/v1/files/office/preview?path=notes.txt&version=opaque", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("route status = %d, want %d: %s", response.Code, http.StatusUnsupportedMediaType, response.Body.String())
	}
}

func TestOfficePreviewConvertsValidDocumentOrReportsMissingDependencies(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("DOCX conversion requires the Linux Bubblewrap sandbox")
	}
	if os.Getenv("FILESAGE_OFFICE_INTEGRATION") != "1" {
		t.Skip("set FILESAGE_OFFICE_INTEGRATION=1 to run real LibreOffice conversion")
	}
	docx, err := os.ReadFile(filepath.Join("..", "office", "testdata", "preview.docx"))
	if err != nil {
		t.Fatal(err)
	}
	api, root, _ := newOfficePreviewAPI(t, map[string][]byte{"document.docx": docx})
	version := officeVersion(t, root, "document.docx")
	query := "path=document.docx&version=" + url.QueryEscape(version)
	response := callOfficePreview(api, query)
	if _, err := office.New(); err != nil {
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("missing dependency status = %d, want %d: %s", response.Code, http.StatusServiceUnavailable, response.Body.String())
		}
		return
	}
	if response.Code != http.StatusOK {
		_, convertErr := office.Convert(context.Background(), docx, ".docx")
		t.Fatalf("status = %d, want %d: %s; direct conversion: %v", response.Code, http.StatusOK, response.Body.String(), convertErr)
	}
	if !bytes.HasPrefix(response.Body.Bytes(), []byte("%PDF-")) {
		t.Fatalf("body does not contain a PDF: %q", response.Body.Bytes()[:min(32, response.Body.Len())])
	}
	if got := response.Header().Get("Content-Type"); got != "application/pdf" {
		t.Fatalf("Content-Type = %q, want application/pdf", got)
	}
	if got := response.Header().Get("X-FileSage-Version"); got != version {
		t.Fatalf("version header = %q, want %q", got, version)
	}
}

func TestOfficePreviewErrorMappingMakesUnavailableExplicit(t *testing.T) {
	response := httptest.NewRecorder()
	writeOfficePreviewError(response, office.ErrUnavailable)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "Bubblewrap") {
		t.Fatalf("unavailable response = %d %q", response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	writeOfficePreviewError(response, context.DeadlineExceeded)
	if response.Code != http.StatusGatewayTimeout {
		t.Fatalf("timeout status = %d, want %d", response.Code, http.StatusGatewayTimeout)
	}
}

func newOfficePreviewAPI(t *testing.T, files map[string][]byte) (*API, *fsroot.Root, string) {
	t.Helper()
	rootPath := t.TempDir()
	for relativePath, data := range files {
		absolutePath := filepath.Join(rootPath, filepath.FromSlash(relativePath))
		if err := os.MkdirAll(filepath.Dir(absolutePath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(absolutePath, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return &API{files: root, locks: coordination.New()}, root, rootPath
}

func callOfficePreview(api *API, query string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/files/office/preview?"+query, nil)
	api.officePreview(response, request)
	return response
}

func officeVersion(t *testing.T, root *fsroot.Root, relativePath string) string {
	t.Helper()
	entries, err := root.List(parentPath(relativePath))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Path == filepath.ToSlash(relativePath) {
			return versionForEntry(entry)
		}
	}
	t.Fatalf("file %q is missing from root listing", relativePath)
	return ""
}
