// 本文件验证文本保存的边界、版本冲突和新建副本语义。
package httpapi

import (
	"encoding/json"
	"errors"
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
)

// TestWriteTextFileRejectsInvalidRequests 验证文本接口拒绝越界输入。
func TestWriteTextFileRejectsInvalidRequests(t *testing.T) {
	_, handler := newMutationHandler(t)
	tests := []struct {
		name        string
		path        string
		body        []byte
		contentType string
		version     string
		ifNoneMatch string
		origin      string
		wantStatus  int
	}{
		{name: "missing precondition", path: "hello.txt", body: []byte("x"), contentType: "text/plain; charset=utf-8", wantStatus: http.StatusBadRequest},
		{name: "unsupported file", path: "image.png", body: []byte("x"), contentType: "text/plain; charset=utf-8", version: "v1.old", wantStatus: http.StatusUnsupportedMediaType},
		{name: "unsupported charset", path: "hello.txt", body: []byte("x"), contentType: "text/plain; charset=iso-8859-1", version: "v1.old", wantStatus: http.StatusUnsupportedMediaType},
		{name: "invalid utf8", path: "hello.txt", body: []byte{0xff}, contentType: "text/plain; charset=utf-8", version: "v1.old", wantStatus: http.StatusBadRequest},
		{name: "binary null", path: "hello.txt", body: []byte{'a', 0, 'b'}, contentType: "text/plain; charset=utf-8", version: "v1.old", wantStatus: http.StatusBadRequest},
		{name: "too large", path: "hello.txt", body: []byte(strings.Repeat("x", maxEditableTextSize+1)), contentType: "text/plain; charset=utf-8", version: "v1.old", wantStatus: http.StatusRequestEntityTooLarge},
		{name: "cross origin", path: "hello.txt", body: []byte("x"), contentType: "text/plain; charset=utf-8", version: "v1.old", origin: "https://attacker.test", wantStatus: http.StatusForbidden},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := textEditRequest(handler, http.MethodPut, test.path, test.version, test.ifNoneMatch, test.contentType, test.body, test.origin)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.wantStatus, response.Body.String())
			}
		})
	}
}

// TestWriteTextFileRequiresCurrentVersionAndPreservesConflictingContent 验证旧版本不能覆盖新内容。
func TestWriteTextFileRequiresCurrentVersionAndPreservesConflictingContent(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("atomic text replacement is validated on Linux")
	}
	rootPath, handler := newMutationHandler(t)
	version := contentVersion(t, handler, "hello.txt")
	response := textEditRequest(handler, http.MethodPut, "hello.txt", version, "", "text/plain; charset=utf-8", []byte("你好\n"), "")
	if response.Code != http.StatusOK {
		t.Fatalf("save status = %d: %s", response.Code, response.Body.String())
	}
	var result textWriteResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Path != "hello.txt" || result.Version == "" || result.Version == version {
		t.Fatalf("save result = %#v", result)
	}
	if got, err := os.ReadFile(filepath.Join(rootPath, "hello.txt")); err != nil || string(got) != "你好\n" {
		t.Fatalf("saved content = %q, error = %v", got, err)
	}

	response = textEditRequest(handler, http.MethodPut, "hello.txt", version, "", "text/plain; charset=utf-8", []byte("stale"), "")
	if response.Code != http.StatusConflict {
		t.Fatalf("stale save status = %d, want 409: %s", response.Code, response.Body.String())
	}
	if got, err := os.ReadFile(filepath.Join(rootPath, "hello.txt")); err != nil || string(got) != "你好\n" {
		t.Fatalf("stale save changed content to %q, error = %v", got, err)
	}
}

// TestWriteTextFileDetectsSameSizeExternalChanges 验证旧令牌不能覆盖恢复过修改时间的外部改写。
func TestWriteTextFileDetectsSameSizeExternalChanges(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux change-time conflict detection is required")
	}
	rootPath, handler := newMutationHandler(t)
	version := contentVersion(t, handler, "hello.txt")
	info, err := os.Stat(filepath.Join(rootPath, "hello.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "hello.txt"), []byte("world"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(rootPath, "hello.txt"), info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	response := textEditRequest(handler, http.MethodPut, "hello.txt", version, "", "text/plain; charset=utf-8", []byte("stale"), "")
	if response.Code != http.StatusConflict {
		t.Fatalf("stale save status = %d, want 409: %s", response.Code, response.Body.String())
	}
	if got, err := os.ReadFile(filepath.Join(rootPath, "hello.txt")); err != nil || string(got) != "world" {
		t.Fatalf("external content changed to %q, error = %v", got, err)
	}
}

// TestWriteTextFileCreatesSaveAsWithoutOverwriting 验证另存目标仅创建且不覆盖。
func TestWriteTextFileCreatesSaveAsWithoutOverwriting(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("atomic text creation is validated on Linux")
	}
	rootPath, handler := newMutationHandler(t)
	response := textEditRequest(handler, http.MethodPut, "notes/draft.txt", "", "*", "text/plain; charset=utf-8", []byte("draft"), "")
	if response.Code != http.StatusOK {
		t.Fatalf("save-as status = %d: %s", response.Code, response.Body.String())
	}
	if got, err := os.ReadFile(filepath.Join(rootPath, "notes", "draft.txt")); err != nil || string(got) != "draft" {
		t.Fatalf("save-as content = %q, error = %v", got, err)
	}
	response = textEditRequest(handler, http.MethodPut, "notes/draft.txt", "", "*", "text/plain; charset=utf-8", []byte("overwrite"), "")
	if response.Code != http.StatusConflict {
		t.Fatalf("duplicate save-as status = %d, want 409: %s", response.Code, response.Body.String())
	}
	if got, err := os.ReadFile(filepath.Join(rootPath, "notes", "draft.txt")); err != nil || string(got) != "draft" {
		t.Fatalf("duplicate save-as changed content to %q, error = %v", got, err)
	}
}

// TestWriteTextFileRejectsHardLinkedFiles 验证保存不会破坏硬链接关系。
func TestWriteTextFileRejectsHardLinkedFiles(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("hard-link write protection is validated on Linux")
	}
	rootPath, handler := newMutationHandler(t)
	if err := os.Link(filepath.Join(rootPath, "hello.txt"), filepath.Join(rootPath, "alias.txt")); err != nil {
		t.Skipf("hard links are unavailable: %v", err)
	}
	version := contentVersion(t, handler, "hello.txt")
	response := textEditRequest(handler, http.MethodPut, "hello.txt", version, "", "text/plain; charset=utf-8", []byte("new"), "")
	if response.Code != http.StatusConflict {
		t.Fatalf("hard-link save status = %d, want 409: %s", response.Code, response.Body.String())
	}
	for _, name := range []string{"hello.txt", "alias.txt"} {
		if got, err := os.ReadFile(filepath.Join(rootPath, name)); err != nil || string(got) != "hello" {
			t.Fatalf("hard link %q changed to %q, error = %v", name, got, err)
		}
	}
}

func TestWriteTextFileStopsOnAuditBeginFailure(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("atomic text writing is validated on Linux")
	}
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "hello.txt"), []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	audit := &batchRenameAuditStub{beginErr: errors.New("audit unavailable")}
	handler := NewWithTaskStore(root, audit, nil, coordination.New())
	version := contentVersion(t, handler, "hello.txt")
	response := textEditRequest(handler, http.MethodPut, "hello.txt", version, "", "text/plain; charset=utf-8", []byte("after"), "")
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "操作记录暂不可用") {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	if got, err := os.ReadFile(filepath.Join(rootPath, "hello.txt")); err != nil || string(got) != "before" {
		t.Fatalf("content after rejected save = %q, error = %v", got, err)
	}
}

func TestWriteTextFileReportsAuditFinishFailureAfterSaving(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("atomic text writing is validated on Linux")
	}
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "hello.txt"), []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	audit := &batchRenameAuditStub{finishErr: errors.New("audit completion unavailable")}
	handler := NewWithTaskStore(root, audit, nil, coordination.New())
	version := contentVersion(t, handler, "hello.txt")
	response := textEditRequest(handler, http.MethodPut, "hello.txt", version, "", "text/plain; charset=utf-8", []byte("after"), "")
	if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), "文件可能已变更，审计结果待核查") {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	if got, err := os.ReadFile(filepath.Join(rootPath, "hello.txt")); err != nil || string(got) != "after" {
		t.Fatalf("content after save = %q, error = %v", got, err)
	}
}

// textEditRequest 构造文本保存请求。
func textEditRequest(handler http.Handler, method, relativePath, version, ifNoneMatch, contentType string, body []byte, origin string) *httptest.ResponseRecorder {
	query := url.Values{"path": {relativePath}}
	request := httptest.NewRequest(method, "/api/v1/files/content?"+query.Encode(), strings.NewReader(string(body)))
	request.Header.Set("Content-Type", contentType)
	if version != "" {
		request.Header.Set("X-FileSage-Version", version)
	}
	if ifNoneMatch != "" {
		request.Header.Set("If-None-Match", ifNoneMatch)
	}
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// contentVersion 从内容响应读取版本令牌。
func contentVersion(t *testing.T, handler http.Handler, relativePath string) string {
	t.Helper()
	query := url.Values{"path": {relativePath}}
	response := performRequest(handler, http.MethodGet, "/api/v1/files/content?"+query.Encode(), nil)
	if response.Code != http.StatusOK {
		t.Fatalf("read status = %d: %s", response.Code, response.Body.String())
	}
	version := response.Header().Get("X-FileSage-Version")
	if version == "" {
		t.Fatal("file content response has no version header")
	}
	return version
}
