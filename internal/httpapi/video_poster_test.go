package httpapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filesage/internal/fsroot"
	"filesage/internal/videoposter"
)

type videoPosterGeneratorStub struct {
	calls int
	input string
	image []byte
	err   error
	after func()
}

func (stub *videoPosterGeneratorStub) Generate(_ context.Context, input io.Reader) ([]byte, error) {
	stub.calls++
	data, err := io.ReadAll(input)
	if err != nil {
		return nil, err
	}
	stub.input = string(data)
	if stub.after != nil {
		stub.after()
	}
	if stub.err != nil {
		return nil, stub.err
	}
	return append([]byte(nil), stub.image...), nil
}

func TestVideoPosterReturnsVersionedJPEGCachedByBrowser(t *testing.T) {
	root, _ := newVideoPosterTest(t)
	poster := &videoPosterGeneratorStub{image: []byte{0xff, 0xd8, 0xff, 0xd9}}
	handler := NewVideoPosterHandler(root, poster)
	version := videoPosterVersion(t, root, "clip.mp4")
	requestURL := videoPosterURL("clip.mp4", version)

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, requestURL, nil))
	if first.Code != http.StatusOK || string(first.Body.Bytes()) != string(poster.image) {
		t.Fatalf("first response = %d %q", first.Code, first.Body.Bytes())
	}
	if first.Header().Get("Content-Type") != "image/jpeg" || first.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("unsafe image headers: %v", first.Header())
	}
	if first.Header().Get("ETag") != `"`+version+`"` || first.Header().Get("X-FileSage-Version") != version || first.Header().Get("Cache-Control") == "" {
		t.Fatalf("missing versioned cache headers: %v", first.Header())
	}
	if poster.calls != 1 || poster.input != "sample video" {
		t.Fatalf("generator calls=%d input=%q", poster.calls, poster.input)
	}

	secondRequest := httptest.NewRequest(http.MethodGet, requestURL, nil)
	secondRequest.Header.Set("If-None-Match", first.Header().Get("ETag"))
	second := httptest.NewRecorder()
	handler.ServeHTTP(second, secondRequest)
	if second.Code != http.StatusNotModified || second.Body.Len() != 0 || poster.calls != 1 {
		t.Fatalf("cached response = %d body=%d generator calls=%d", second.Code, second.Body.Len(), poster.calls)
	}
}

func TestVideoPosterRejectsInvalidOrStaleRequestsBeforeDecode(t *testing.T) {
	root, _ := newVideoPosterTest(t)
	poster := &videoPosterGeneratorStub{image: []byte{0xff, 0xd8, 0xff, 0xd9}}
	handler := NewVideoPosterHandler(root, poster)
	version := videoPosterVersion(t, root, "clip.mp4")
	for _, test := range []struct {
		name string
		url  string
		want int
	}{
		{name: "path traversal", url: videoPosterURL("../clip.mp4", version), want: http.StatusBadRequest},
		{name: "missing version", url: "/api/v1/files/video-poster?path=clip.mp4", want: http.StatusBadRequest},
		{name: "extra query", url: videoPosterURL("clip.mp4", version) + "&extra=1", want: http.StatusBadRequest},
		{name: "duplicate path", url: videoPosterURL("clip.mp4", version) + "&path=clip.mp4", want: http.StatusBadRequest},
		{name: "unsupported type", url: videoPosterURL("notes.txt", version), want: http.StatusUnsupportedMediaType},
		{name: "stale version", url: videoPosterURL("clip.mp4", "stale"), want: http.StatusConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.url, nil))
			if response.Code != test.want {
				t.Fatalf("status=%d want=%d body=%s", response.Code, test.want, response.Body.String())
			}
		})
	}
	if poster.calls != 0 {
		t.Fatalf("invalid requests reached decoder %d times", poster.calls)
	}
}

func TestVideoPosterHeadDoesNotStartDecoder(t *testing.T) {
	root, _ := newVideoPosterTest(t)
	poster := &videoPosterGeneratorStub{image: []byte{0xff, 0xd8, 0xff, 0xd9}}
	version := videoPosterVersion(t, root, "clip.mp4")
	response := httptest.NewRecorder()
	NewVideoPosterHandler(root, poster).ServeHTTP(response,
		httptest.NewRequest(http.MethodHead, videoPosterURL("clip.mp4", version), nil))
	if response.Code != http.StatusMethodNotAllowed || poster.calls != 0 || response.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("HEAD response=%d allow=%q decoder calls=%d", response.Code, response.Header().Get("Allow"), poster.calls)
	}
}

func TestVideoPosterRequiresRegularFileAndRechecksAfterGeneration(t *testing.T) {
	root, rootPath := newVideoPosterTest(t)
	poster := &videoPosterGeneratorStub{image: []byte{0xff, 0xd8, 0xff, 0xd9}}
	handler := NewVideoPosterHandler(root, poster)
	if err := os.Mkdir(filepath.Join(rootPath, "folder.mp4"), 0700); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet,
		videoPosterURL("folder.mp4", "any"), nil))
	if response.Code != http.StatusBadRequest || poster.calls != 0 {
		t.Fatalf("directory response=%d generator calls=%d", response.Code, poster.calls)
	}

	version := videoPosterVersion(t, root, "clip.mp4")
	poster.after = func() {
		if err := os.WriteFile(filepath.Join(rootPath, "clip.mp4"), []byte("changed content"), 0600); err != nil {
			t.Errorf("change source during generation: %v", err)
		}
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet,
		videoPosterURL("clip.mp4", version), nil))
	if response.Code != http.StatusConflict || strings.Contains(response.Header().Get("Content-Type"), "image/jpeg") {
		t.Fatalf("post-generation conflict response=%d headers=%v", response.Code, response.Header())
	}
}

func TestVideoPosterMapsDisabledAndTimeoutAndSuppressesCanceledResponse(t *testing.T) {
	root, _ := newVideoPosterTest(t)
	version := videoPosterVersion(t, root, "clip.mp4")
	for _, test := range []struct {
		name string
		err  error
		want int
	}{
		{name: "ffmpeg disabled", err: videoposter.ErrDisabled, want: http.StatusServiceUnavailable},
		{name: "timeout", err: videoposter.ErrTimeout, want: http.StatusGatewayTimeout},
		{name: "internal cancellation", err: context.Canceled, want: http.StatusRequestTimeout},
	} {
		t.Run(test.name, func(t *testing.T) {
			poster := &videoPosterGeneratorStub{err: test.err}
			response := httptest.NewRecorder()
			NewVideoPosterHandler(root, poster).ServeHTTP(response,
				httptest.NewRequest(http.MethodGet, videoPosterURL("clip.mp4", version), nil))
			if response.Code != test.want {
				t.Fatalf("status=%d want=%d body=%s", response.Code, test.want, response.Body.String())
			}
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	response := httptest.NewRecorder()
	NewVideoPosterHandler(root, &videoPosterGeneratorStub{err: context.Canceled}).ServeHTTP(response,
		httptest.NewRequest(http.MethodGet, videoPosterURL("clip.mp4", version), nil).WithContext(ctx))
	if response.Body.Len() != 0 {
		t.Fatalf("canceled request wrote a response body: %s", response.Body.String())
	}
}

func newVideoPosterTest(t *testing.T) (*fsroot.Root, string) {
	t.Helper()
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "clip.mp4"), []byte("sample video"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "notes.txt"), []byte("text"), 0600); err != nil {
		t.Fatal(err)
	}
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root, rootPath
}

func videoPosterVersion(t *testing.T, root *fsroot.Root, path string) string {
	t.Helper()
	file, entry, err := root.OpenRead(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return versionForEntry(entry)
}

func videoPosterURL(path, version string) string {
	query := url.Values{"path": {path}, "version": {version}}
	return "/api/v1/files/video-poster?" + query.Encode()
}
