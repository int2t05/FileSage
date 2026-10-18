// 本文件验证图片预览接口的版本、安全与错误处理行为。
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
	"filesage/internal/imagepreview"
)

type imagePreviewGeneratorStub struct {
	calls int
	path  string
	size  imagepreview.Size
	input string
	image []byte
	err   error
	after func()
}

// Generate 记录预览请求并返回测试用 JPEG。
func (stub *imagePreviewGeneratorStub) Generate(_ context.Context, relativePath string, input io.Reader, size imagepreview.Size) ([]byte, error) {
	stub.calls++
	stub.path, stub.size = relativePath, size
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

// TestImagePreviewReturnsVersionedJPEGAndUsesConditionalCache 验证版本响应和条件缓存。
func TestImagePreviewReturnsVersionedJPEGAndUsesConditionalCache(t *testing.T) {
	root, _ := newImagePreviewTest(t)
	generator := &imagePreviewGeneratorStub{image: []byte{0xff, 0xd8, 0xff, 0xd9}}
	handler := NewImagePreviewHandler(root, generator)
	version := imagePreviewVersion(t, root, "photo.heic")
	requestURL := imagePreviewURL("photo.heic", version, imagepreview.Thumbnail)

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, requestURL, nil))
	if first.Code != http.StatusOK || string(first.Body.Bytes()) != string(generator.image) {
		t.Fatalf("first response = %d %q", first.Code, first.Body.Bytes())
	}
	if first.Header().Get("Content-Type") != "image/jpeg" || first.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("unsafe image headers: %v", first.Header())
	}
	if first.Header().Get("ETag") != `"`+version+"-thumb"+`"` || first.Header().Get("X-FileSage-Version") != version || first.Header().Get("Cache-Control") == "" {
		t.Fatalf("missing versioned cache headers: %v", first.Header())
	}
	if generator.calls != 1 || generator.input != "sample image" || generator.path != "photo.heic" || generator.size != imagepreview.Thumbnail {
		t.Fatalf("generator received path=%q size=%q input=%q calls=%d", generator.path, generator.size, generator.input, generator.calls)
	}

	conditional := httptest.NewRequest(http.MethodGet, requestURL, nil)
	conditional.Header.Set("If-None-Match", first.Header().Get("ETag"))
	second := httptest.NewRecorder()
	handler.ServeHTTP(second, conditional)
	if second.Code != http.StatusNotModified || second.Body.Len() != 0 || generator.calls != 1 {
		t.Fatalf("cached response = %d body=%d generator calls=%d", second.Code, second.Body.Len(), generator.calls)
	}
}

// TestImagePreviewRejectsInvalidOrStaleRequestsBeforeDecode 验证解码前拦截无效或过期请求。
func TestImagePreviewRejectsInvalidOrStaleRequestsBeforeDecode(t *testing.T) {
	root, _ := newImagePreviewTest(t)
	generator := &imagePreviewGeneratorStub{image: []byte{0xff, 0xd8, 0xff, 0xd9}}
	handler := NewImagePreviewHandler(root, generator)
	version := imagePreviewVersion(t, root, "photo.heic")
	for _, test := range []struct {
		name string
		url  string
		want int
	}{
		{name: "path traversal", url: imagePreviewURL("../photo.heic", version, imagepreview.Viewer), want: http.StatusBadRequest},
		{name: "missing version", url: "/api/v1/files/image-preview?path=photo.heic&size=view", want: http.StatusBadRequest},
		{name: "missing size", url: "/api/v1/files/image-preview?path=photo.heic&version=" + url.QueryEscape(version), want: http.StatusBadRequest},
		{name: "invalid size", url: imagePreviewURL("photo.heic", version, "original"), want: http.StatusBadRequest},
		{name: "extra query", url: imagePreviewURL("photo.heic", version, imagepreview.Viewer) + "&extra=1", want: http.StatusBadRequest},
		{name: "duplicate path", url: imagePreviewURL("photo.heic", version, imagepreview.Viewer) + "&path=photo.heic", want: http.StatusBadRequest},
		{name: "unsupported type", url: imagePreviewURL("notes.txt", version, imagepreview.Viewer), want: http.StatusUnsupportedMediaType},
		{name: "stale version", url: imagePreviewURL("photo.heic", "stale", imagepreview.Viewer), want: http.StatusConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.url, nil))
			if response.Code != test.want {
				t.Fatalf("status=%d want=%d body=%s", response.Code, test.want, response.Body.String())
			}
		})
	}
	if generator.calls != 0 {
		t.Fatalf("invalid requests reached decoder %d times", generator.calls)
	}
}

// TestImagePreviewRequiresRegularFileAndRechecksVersionAfterDecode 验证普通文件与解码后版本复核。
func TestImagePreviewRequiresRegularFileAndRechecksVersionAfterDecode(t *testing.T) {
	root, rootPath := newImagePreviewTest(t)
	generator := &imagePreviewGeneratorStub{image: []byte{0xff, 0xd8, 0xff, 0xd9}}
	if err := os.Mkdir(filepath.Join(rootPath, "folder.heic"), 0700); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	NewImagePreviewHandler(root, generator).ServeHTTP(response,
		httptest.NewRequest(http.MethodGet, imagePreviewURL("folder.heic", "any", imagepreview.Viewer), nil))
	if response.Code != http.StatusBadRequest || generator.calls != 0 {
		t.Fatalf("directory response=%d generator calls=%d", response.Code, generator.calls)
	}

	version := imagePreviewVersion(t, root, "photo.heic")
	generator.after = func() {
		if err := os.WriteFile(filepath.Join(rootPath, "photo.heic"), []byte("changed image"), 0600); err != nil {
			t.Errorf("change source during generation: %v", err)
		}
	}
	response = httptest.NewRecorder()
	NewImagePreviewHandler(root, generator).ServeHTTP(response,
		httptest.NewRequest(http.MethodGet, imagePreviewURL("photo.heic", version, imagepreview.Viewer), nil))
	if response.Code != http.StatusConflict || strings.Contains(response.Header().Get("Content-Type"), "image/jpeg") {
		t.Fatalf("post-generation conflict response=%d headers=%v", response.Code, response.Header())
	}
}

// TestImagePreviewMapsDecoderFailuresAndRejectsHead 验证错误映射和方法限制。
func TestImagePreviewMapsDecoderFailuresAndRejectsHead(t *testing.T) {
	root, _ := newImagePreviewTest(t)
	version := imagePreviewVersion(t, root, "photo.heic")
	for _, test := range []struct {
		name string
		err  error
		want int
	}{
		{name: "decoder disabled", err: imagepreview.ErrDisabled, want: http.StatusServiceUnavailable},
		{name: "queue full", err: imagepreview.ErrBusy, want: http.StatusServiceUnavailable},
		{name: "timeout", err: imagepreview.ErrTimeout, want: http.StatusGatewayTimeout},
		{name: "large input", err: imagepreview.ErrInputTooLarge, want: http.StatusRequestEntityTooLarge},
		{name: "invalid image", err: imagepreview.ErrInvalidImage, want: http.StatusUnprocessableEntity},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			NewImagePreviewHandler(root, &imagePreviewGeneratorStub{err: test.err}).ServeHTTP(response,
				httptest.NewRequest(http.MethodGet, imagePreviewURL("photo.heic", version, imagepreview.Viewer), nil))
			if response.Code != test.want {
				t.Fatalf("status=%d want=%d body=%s", response.Code, test.want, response.Body.String())
			}
		})
	}
	busy := httptest.NewRecorder()
	NewImagePreviewHandler(root, &imagePreviewGeneratorStub{err: imagepreview.ErrBusy}).ServeHTTP(busy,
		httptest.NewRequest(http.MethodGet, imagePreviewURL("photo.heic", version, imagepreview.Viewer), nil))
	if busy.Header().Get("Retry-After") != "1" {
		t.Fatalf("busy response Retry-After = %q", busy.Header().Get("Retry-After"))
	}

	response := httptest.NewRecorder()
	NewImagePreviewHandler(root, &imagePreviewGeneratorStub{}).ServeHTTP(response,
		httptest.NewRequest(http.MethodHead, imagePreviewURL("photo.heic", version, imagepreview.Viewer), nil))
	if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("HEAD response=%d allow=%q", response.Code, response.Header().Get("Allow"))
	}
}

// newImagePreviewTest 创建临时文件系统与预览处理器。
func newImagePreviewTest(t *testing.T) (*fsroot.Root, string) {
	t.Helper()
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "photo.heic"), []byte("sample image"), 0600); err != nil {
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

// imagePreviewVersion 读取测试文件的当前版本号。
func imagePreviewVersion(t *testing.T, root *fsroot.Root, relativePath string) string {
	t.Helper()
	file, entry, err := root.OpenRead(relativePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return versionForEntry(entry)
}

// imagePreviewURL 构造带版本和尺寸的预览请求地址。
func imagePreviewURL(relativePath, version string, size imagepreview.Size) string {
	query := url.Values{"path": {relativePath}, "version": {version}, "size": {string(size)}}
	return "/api/v1/files/image-preview?" + query.Encode()
}
