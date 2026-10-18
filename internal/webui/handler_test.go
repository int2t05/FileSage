// 本文件验证静态托管、SPA 回退和资源访问边界。
package webui

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHandlerServesBuildFilesAndFallsBackForHTMLRoutes 验证网页与 API 路由分流。
func TestHandlerServesBuildFilesAndFallsBackForHTMLRoutes(t *testing.T) {
	build := t.TempDir()
	writeBuildFile(t, build, "index.html", "<main>FileSage</main>")
	writeBuildFile(t, build, "assets/app.js", "console.log('app')")
	writeBuildFile(t, build, "assets/pdf.worker.mjs", "self.onmessage = () => {}")
	if err := os.Mkdir(filepath.Join(build, "assets", "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	api := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Fallback", "api")
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("API"))
	})
	handler, err := New(build, api)
	if err != nil {
		t.Fatal(err)
	}
	defer handler.Close()

	tests := []struct {
		name        string
		method      string
		path        string
		accept      string
		status      int
		body        string
		contentType string
		fallback    bool
	}{
		{name: "root", method: http.MethodGet, path: "/", status: http.StatusOK, body: "<main>FileSage</main>", contentType: "text/html"},
		{name: "spa route", method: http.MethodGet, path: "/agent/history", accept: "text/html", status: http.StatusOK, body: "<main>FileSage</main>", contentType: "text/html"},
		{name: "asset", method: http.MethodGet, path: "/assets/app.js", status: http.StatusOK, body: "console.log('app')", contentType: "javascript"},
		{name: "module worker", method: http.MethodGet, path: "/assets/pdf.worker.mjs", status: http.StatusOK, body: "self.onmessage = () => {}", contentType: "text/javascript"},
		{name: "missing asset", method: http.MethodGet, path: "/assets/missing.js", accept: "text/html", status: http.StatusNotFound},
		{name: "missing extensionless asset", method: http.MethodGet, path: "/assets/missing", accept: "text/html", status: http.StatusNotFound},
		{name: "directory", method: http.MethodGet, path: "/assets/nested/", accept: "text/html", status: http.StatusNotFound},
		{name: "unknown path without html accept", method: http.MethodGet, path: "/agent/history", status: http.StatusNotFound},
		{name: "unknown api", method: http.MethodGet, path: "/api/v1/missing", accept: "text/html", status: http.StatusTeapot, body: "API", fallback: true},
		{name: "upload api", method: http.MethodGet, path: "/api/v1/uploads/status/123", accept: "text/html", status: http.StatusTeapot, body: "API", fallback: true},
		{name: "upload stream", method: http.MethodPost, path: "/api/v1/uploads/123", status: http.StatusTeapot, body: "API", fallback: true},
		{name: "non get method", method: http.MethodPost, path: "/agent/history", status: http.StatusTeapot, body: "API", fallback: true},
		{name: "api prefix boundary", method: http.MethodGet, path: "/api-docs", accept: "text/html", status: http.StatusOK, body: "<main>FileSage</main>", contentType: "text/html"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, nil)
			request.Header.Set("Accept", test.accept)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d; body = %q", response.Code, test.status, response.Body.String())
			}
			if got := response.Body.String(); test.body != "" && got != test.body {
				t.Fatalf("body = %q, want %q", got, test.body)
			}
			gotType := response.Header().Get("Content-Type")
			if test.contentType == "javascript" && !strings.Contains(gotType, "javascript") {
				t.Fatalf("Content-Type = %q, want a JavaScript MIME type", gotType)
			}
			if test.contentType != "" && test.contentType != "javascript" && !strings.HasPrefix(gotType, test.contentType) {
				t.Fatalf("Content-Type = %q, want prefix %q", gotType, test.contentType)
			}
			if test.contentType != "" && response.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatalf("X-Content-Type-Options = %q, want nosniff", response.Header().Get("X-Content-Type-Options"))
			}
			if test.fallback != (response.Header().Get("X-Fallback") == "api") {
				t.Fatalf("fallback marker = %q, want fallback=%t", response.Header().Get("X-Fallback"), test.fallback)
			}
		})
	}
}

// TestHandlerSupportsHeadWithoutReturningBody 验证 HEAD 响应保留头信息但不返回正文。
func TestHandlerSupportsHeadWithoutReturningBody(t *testing.T) {
	build := t.TempDir()
	writeBuildFile(t, build, "index.html", "FileSage")
	handler, err := New(build, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer handler.Close()

	request := httptest.NewRequest(http.MethodHead, "/", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.Len() != 0 {
		t.Fatalf("HEAD response = status %d, body %q", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Content-Length"); got != "8" {
		t.Fatalf("Content-Length = %q, want 8", got)
	}
}

// TestHandlerRejectsTraversal 验证路径穿越不会读取构建目录外的文件。
func TestHandlerRejectsTraversal(t *testing.T) {
	build := t.TempDir()
	writeBuildFile(t, build, "index.html", "FileSage")
	handler, err := New(build, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer handler.Close()

	request := httptest.NewRequest(http.MethodGet, "/../secret", nil)
	request.Header.Set("Accept", "text/html")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound || strings.Contains(response.Body.String(), "FileSage") {
		t.Fatalf("traversal response = %d, %q", response.Code, response.Body.String())
	}
}

// TestHandlerDoesNotFollowSymlinksOutsideBuildDirectory 验证外部软链接不会泄露内容。
func TestHandlerDoesNotFollowSymlinksOutsideBuildDirectory(t *testing.T) {
	build := t.TempDir()
	outside := t.TempDir()
	writeBuildFile(t, build, "index.html", "FileSage")
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(build, "leak")); err != nil {
		t.Skipf("symbolic links are unavailable: %v", err)
	}
	handler, err := New(build, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer handler.Close()

	request := httptest.NewRequest(http.MethodGet, "/leak", nil)
	request.Header.Set("Accept", "text/html")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound || strings.Contains(response.Body.String(), "secret") {
		t.Fatalf("outside symlink response = %d, %q", response.Code, response.Body.String())
	}
}

// TestUnconfiguredHandlerPreservesFallbackBehavior 验证未配置时请求仍交给原处理器。
func TestUnconfiguredHandlerPreservesFallbackBehavior(t *testing.T) {
	fallback := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	handler, err := New("", fallback)
	if err != nil {
		t.Fatal(err)
	}
	defer handler.Close()

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("unconfigured response status = %d, want %d", response.Code, http.StatusNoContent)
	}
}

// TestNewRequiresBuildDirectoryAndIndex 验证启动前检查构建目录和入口文件。
func TestNewRequiresBuildDirectoryAndIndex(t *testing.T) {
	if _, err := New(filepath.Join(t.TempDir(), "missing"), nil); err == nil {
		t.Fatal("New() accepted a missing build directory")
	}
	if _, err := New(t.TempDir(), nil); err == nil {
		t.Fatal("New() accepted a build directory without index.html")
	}
}

// writeBuildFile 创建测试用构建资源。
func writeBuildFile(t *testing.T, root, relativePath, content string) {
	t.Helper()
	filePath := filepath.Join(root, filepath.FromSlash(relativePath))
	if err := os.MkdirAll(filepath.Dir(filePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filePath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
