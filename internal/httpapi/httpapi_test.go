package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"filesage/internal/fsroot"
	"filesage/internal/logging"
)

func newTestHandler(t *testing.T) http.Handler {
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
		t.Fatalf("open filesystem root: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return New(root, nil)
}

func TestWriteFileErrorReportsPreservedRecoveryPath(t *testing.T) {
	response := httptest.NewRecorder()
	writeFileError(response, &fsroot.WriteRecoveryError{
		Path:  "notes/.filesage-write-0123456789abcdef",
		Cause: errors.Join(fsroot.ErrConflict, errors.New("rollback failed")),
	})
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusConflict)
	}
	if !strings.Contains(response.Body.String(), "notes/.filesage-write-0123456789abcdef") {
		t.Fatalf("recovery path missing from response: %s", response.Body.String())
	}
}

func TestWriteFileErrorReportsRetainedDeletePath(t *testing.T) {
	response := httptest.NewRecorder()
	writeFileError(response, &fsroot.EntryRemovalRecoveryError{
		Path:  "notes/.filesage-move-quarantine-0123456789abcdef01234567/source",
		Cause: errors.Join(fsroot.ErrConflict, errors.New("source path is occupied")),
	})
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusConflict)
	}
	if !strings.Contains(response.Body.String(), "notes/.filesage-move-quarantine-0123456789abcdef01234567/source") {
		t.Fatalf("retained delete path missing from response: %s", response.Body.String())
	}
}

func TestRequestLoggerUsesRouteTemplateAndOmitsRequestData(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	mux := http.NewServeMux()
	var handledRequestID string
	mux.HandleFunc("GET /api/v1/files/{path...}", func(w http.ResponseWriter, r *http.Request) {
		handledRequestID = logging.RequestID(r.Context())
		http.Error(w, "internal server error", http.StatusInternalServerError)
	})
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/files/private-report.txt?token=secret-token", nil)
	RequestLogger(logger, mux).ServeHTTP(response, request)

	if response.Code != http.StatusInternalServerError || response.Header().Get("X-Request-ID") == "" {
		t.Fatalf("response = %d, request id = %q", response.Code, response.Header().Get("X-Request-ID"))
	}
	if handledRequestID == "" || handledRequestID != response.Header().Get("X-Request-ID") {
		t.Fatalf("request context ID %q does not match response header %q", handledRequestID, response.Header().Get("X-Request-ID"))
	}
	output := logs.String()
	for _, want := range []string{"level=ERROR", "GET /api/v1/files/{path...}", "status=500", "request_id="} {
		if !strings.Contains(output, want) {
			t.Errorf("log output %q does not include %q", output, want)
		}
	}
	for _, forbidden := range []string{"private-report.txt", "secret-token"} {
		if strings.Contains(output, forbidden) {
			t.Errorf("log output contains request data %q: %s", forbidden, output)
		}
	}
}

func TestRequestLogLevelTreatsSuccessfulUploadsAsInformational(t *testing.T) {
	for _, test := range []struct {
		name     string
		method   string
		route    string
		status   int
		duration time.Duration
		level    slog.Level
		log      bool
	}{
		{name: "fast success omitted", method: http.MethodGet, route: "/api/v1/files/", status: http.StatusOK, duration: time.Second},
		{name: "slow tus upload informational", method: http.MethodPatch, route: tusUploadRoute, status: http.StatusNoContent, duration: 30 * time.Second, level: slog.LevelInfo, log: true},
		{name: "upload rejection warning", method: http.MethodPatch, route: tusUploadRoute, status: http.StatusRequestEntityTooLarge, duration: time.Second, level: slog.LevelWarn, log: true},
		{name: "slow api warning", method: http.MethodGet, route: "/api/v1/files/", status: http.StatusOK, duration: 3 * time.Second, level: slog.LevelWarn, log: true},
		{name: "server error", method: http.MethodPatch, route: tusUploadRoute, status: http.StatusInternalServerError, duration: 3 * time.Second, level: slog.LevelError, log: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			level, log := requestLogLevel(test.method, test.route, test.status, test.duration)
			if level != test.level || log != test.log {
				t.Fatalf("requestLogLevel() = (%v, %t), want (%v, %t)", level, log, test.level, test.log)
			}
		})
	}
}

func TestWriteFileErrorReportsReadOnlyAndFullVolumes(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		code int
		text string
	}{
		{name: "read-only", err: &os.PathError{Op: "write", Path: "private.txt", Err: syscall.EROFS}, code: http.StatusForbidden, text: "只读"},
		{name: "full", err: &os.PathError{Op: "write", Path: "private.txt", Err: syscall.ENOSPC}, code: http.StatusInsufficientStorage, text: "空间不足"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			writeFileError(response, test.err)
			if response.Code != test.code || !strings.Contains(response.Body.String(), test.text) {
				t.Fatalf("response = %d %q", response.Code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), "private.txt") {
				t.Fatalf("response exposed filesystem path: %q", response.Body.String())
			}
		})
	}
}

func TestListFilesReturnsRealDirectoryEntries(t *testing.T) {
	handler := newTestHandler(t)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/files", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}
	var result listingResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if result.Path != "" || len(result.Entries) != 2 {
		t.Fatalf("listing = %#v, want root and two entries", result)
	}
	if result.Entries[0].Name != "notes" || result.Entries[0].Kind != fsroot.KindDirectory {
		t.Fatalf("first entry = %#v, want notes directory", result.Entries[0])
	}
	if result.Entries[1].Name != "hello.txt" || result.Entries[1].Kind != fsroot.KindFile {
		t.Fatalf("second entry = %#v, want hello.txt file", result.Entries[1])
	}
	if !strings.Contains(response.Header().Get("X-Content-Type-Options"), "nosniff") {
		t.Fatal("response is missing nosniff header")
	}
}

func TestListFilesRejectsPathsOutsideRoot(t *testing.T) {
	handler := newTestHandler(t)
	for _, rawQuery := range []string{"path=..", "path=notes%2F..%2F..", "path=%zz"} {
		t.Run(rawQuery, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/api/v1/files?"+rawQuery, nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusBadRequest, response.Body.String())
			}
		})
	}
}

func TestReadFileReturnsAttachmentWithoutSniffableContentType(t *testing.T) {
	handler := newTestHandler(t)
	query := url.Values{"path": {"notes/today.md"}}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/files/content?"+query.Encode(), nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "today" {
		t.Fatalf("body = %q, want %q", body, "today")
	}
	if got := response.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Fatalf("Content-Type = %q, want application/octet-stream", got)
	}
	if got := response.Header().Get("Content-Disposition"); !strings.Contains(got, "attachment") {
		t.Fatalf("Content-Disposition = %q, want attachment", got)
	}
}

func TestReadFileSupportsByteRanges(t *testing.T) {
	handler := newTestHandler(t)
	query := url.Values{"path": {"hello.txt"}}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/files/content?"+query.Encode(), nil)
	request.Header.Set("Range", "bytes=1-3")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusPartialContent, response.Body.String())
	}
	if response.Body.String() != "ell" {
		t.Fatalf("body = %q, want %q", response.Body.String(), "ell")
	}
	if got := response.Header().Get("Content-Range"); got != "bytes 1-3/5" {
		t.Fatalf("Content-Range = %q, want %q", got, "bytes 1-3/5")
	}
	if got := response.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Fatalf("Content-Type = %q, want application/octet-stream", got)
	}
}

func TestReadFileSupportsHead(t *testing.T) {
	handler := newTestHandler(t)
	query := url.Values{"path": {"hello.txt"}}
	request := httptest.NewRequest(http.MethodHead, "/api/v1/files/content?"+query.Encode(), nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if response.Body.Len() != 0 {
		t.Fatalf("HEAD returned %d body bytes", response.Body.Len())
	}
	if got := response.Header().Get("Content-Length"); got != "5" {
		t.Fatalf("Content-Length = %q, want %q", got, "5")
	}
	if got := response.Header().Get("X-FileSage-Version"); got == "" {
		t.Fatal("HEAD response is missing the file version header")
	}
}

func TestPreviewStreamsSafeImageTypeAndSupportsRanges(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "photo.png"), []byte("image-data"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	handler := New(root, nil)
	query := url.Values{"path": {"photo.png"}}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/files/preview?"+query.Encode(), nil)
	request.Header.Set("Range", "bytes=0-4")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusPartialContent, response.Body.String())
	}
	if response.Body.String() != "image" {
		t.Fatalf("body = %q, want %q", response.Body.String(), "image")
	}
	if got := response.Header().Get("Content-Type"); got != "image/png" {
		t.Fatalf("Content-Type = %q, want image/png", got)
	}
	if got := response.Header().Get("Content-Disposition"); !strings.HasPrefix(got, "inline") {
		t.Fatalf("Content-Disposition = %q, want inline", got)
	}
	if got := response.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
}

func TestPreviewStreamsPDFInlineWithRangesAndSameOriginFraming(t *testing.T) {
	rootPath := t.TempDir()
	content := []byte("%PDF-1.7\nsample-pdf-content")
	if err := os.WriteFile(filepath.Join(rootPath, "document.pdf"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	handler := New(root, nil)
	query := url.Values{"path": {"document.pdf"}}

	request := httptest.NewRequest(http.MethodGet, "/api/v1/files/preview?"+query.Encode(), nil)
	request.Header.Set("Range", "bytes=0-7")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusPartialContent {
		t.Fatalf("range status = %d, want %d: %s", response.Code, http.StatusPartialContent, response.Body.String())
	}
	if response.Body.String() != "%PDF-1.7" {
		t.Fatalf("range body = %q, want PDF header", response.Body.String())
	}
	if got := response.Header().Get("Content-Type"); got != "application/pdf" {
		t.Fatalf("Content-Type = %q, want application/pdf", got)
	}
	if got := response.Header().Get("Content-Disposition"); !strings.HasPrefix(got, "inline") {
		t.Fatalf("Content-Disposition = %q, want inline", got)
	}
	if got := response.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := response.Header().Get("X-Frame-Options"); got != "SAMEORIGIN" {
		t.Fatalf("X-Frame-Options = %q, want SAMEORIGIN", got)
	}

	request = httptest.NewRequest(http.MethodHead, "/api/v1/files/preview?"+query.Encode(), nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("HEAD status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}
	if response.Body.Len() != 0 {
		t.Fatalf("HEAD returned %d body bytes", response.Body.Len())
	}
	if got := response.Header().Get("Content-Length"); got != fmt.Sprint(len(content)) {
		t.Fatalf("HEAD Content-Length = %q, want %d", got, len(content))
	}
}

func TestPreviewRejectsTextAndActiveContent(t *testing.T) {
	handler := newTestHandler(t)
	for _, name := range []string{"hello.txt", "page.html", "drawing.svg"} {
		t.Run(name, func(t *testing.T) {
			query := url.Values{"path": {name}}
			request := httptest.NewRequest(http.MethodGet, "/api/v1/files/preview?"+query.Encode(), nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusUnsupportedMediaType {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusUnsupportedMediaType)
			}
		})
	}
}

func TestHealthReflectsReadiness(t *testing.T) {
	handler := New(nil, func(context.Context) error { return errors.New("database unavailable") })
	request := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
}

func TestWriteFileErrorMapsConflictAndUnsupportedOperations(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "stale target", err: fsroot.ErrConflict, want: http.StatusConflict},
		{name: "hard link", err: fsroot.ErrHardLinked, want: http.StatusConflict},
		{name: "symlink write", err: fsroot.ErrSymlinkPath, want: http.StatusBadRequest},
		{name: "unsupported atomic write", err: fsroot.ErrAtomicWriteUnsupported, want: http.StatusNotImplemented},
		{name: "unavailable mounted volume", err: fsroot.ErrVolumeUnavailable, want: http.StatusServiceUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			writeFileError(response, test.err)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d", response.Code, test.want)
			}
		})
	}
}

func TestFileEntryExposesUnavailableVolumeAsReadOnly(t *testing.T) {
	response := toFileEntry(fsroot.Entry{
		Name: "archive", Path: "archive", Kind: fsroot.KindDirectory,
		MountUnavailable: true, ReadOnly: true,
	}, nil)
	data, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["mountUnavailable"] != true || fields["readOnly"] != true {
		t.Fatalf("unavailable volume fields = %#v, want mountUnavailable and readOnly true", fields)
	}
}
