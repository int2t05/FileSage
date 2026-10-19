// 上传发布集成测试验证暂存、发布、重试与重启恢复。
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"filesage/internal/coordination"
	"filesage/internal/fsroot"
	"filesage/internal/store"
	"filesage/internal/upload"
)

type uploadIntegration struct {
	rootPath    string
	staging     string
	root        *fsroot.Root
	metadata    *store.Store
	uploads     *upload.Server
	manager     *publicationManager
	server      *httptest.Server
	cancel      context.CancelFunc
	managerDone chan error
}

func newUploadIntegration(t *testing.T) *uploadIntegration {
	t.Helper()
	temp := t.TempDir()
	rootPath := filepath.Join(temp, "managed")
	staging := filepath.Join(temp, "data", "staging")
	if err := os.MkdirAll(rootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := store.Open(context.Background(), filepath.Join(temp, "data", "filesage.sqlite"))
	if err != nil {
		_ = root.Close()
		t.Fatal(err)
	}
	uploads, err := upload.New(upload.Config{StagingDir: staging, ManagedRoot: rootPath, MaxUploadSize: 1 << 20})
	if err != nil {
		_ = metadata.Close()
		_ = root.Close()
		t.Fatal(err)
	}
	locks := coordination.New()
	manager := newPublicationManager(uploads, root, locks, metadata)
	mux := http.NewServeMux()
	mux.Handle(uploadStatusPath, manager)
	mux.Handle("/api/v1/uploads/", uploads)
	server := httptest.NewServer(mux)
	ctx, cancel := context.WithCancel(context.Background())
	managerDone := make(chan error, 1)
	go func() { managerDone <- manager.run(ctx) }()
	fixture := &uploadIntegration{
		rootPath: rootPath, staging: staging, root: root, metadata: metadata,
		uploads: uploads, manager: manager, server: server, cancel: cancel, managerDone: managerDone,
	}
	t.Cleanup(fixture.close)
	return fixture
}

func (fixture *uploadIntegration) close() {
	fixture.server.Close()
	fixture.cancel()
	fixture.uploads.Close()
	select {
	case <-fixture.managerDone:
	case <-time.After(3 * time.Second):
		panic("upload publication manager did not stop")
	}
	_ = fixture.root.Close()
	_ = fixture.metadata.Close()
}

func (fixture *uploadIntegration) upload(t *testing.T, filename, target, relativePath string, content []byte) string {
	t.Helper()
	id, err := fixture.startUpload(filename, target, relativePath, content)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (fixture *uploadIntegration) startUpload(filename, target, relativePath string, content []byte) (string, error) {
	metadata := map[string]string{
		"filename":     filename,
		"target":       target,
		"relativepath": relativePath,
	}
	encoded := make([]string, 0, len(metadata))
	for _, key := range []string{"filename", "target", "relativepath"} {
		encoded = append(encoded, key+" "+base64.StdEncoding.EncodeToString([]byte(metadata[key])))
	}
	create, err := http.NewRequest(http.MethodPost, fixture.server.URL+"/api/v1/uploads/", nil)
	if err != nil {
		return "", err
	}
	create.Header.Set("Tus-Resumable", "1.0.0")
	create.Header.Set("Upload-Length", fmt.Sprint(len(content)))
	create.Header.Set("Upload-Metadata", strings.Join(encoded, ","))
	response, err := fixture.server.Client().Do(create)
	if err != nil {
		return "", fmt.Errorf("create tus upload: %w", err)
	}
	if response.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		return "", fmt.Errorf("create tus upload returned %d: %s", response.StatusCode, body)
	}
	resource, err := url.Parse(response.Header.Get("Location"))
	if err != nil {
		_ = response.Body.Close()
		return "", fmt.Errorf("parse tus location: %w", err)
	}
	if !resource.IsAbs() {
		base, _ := url.Parse(fixture.server.URL)
		resource = base.ResolveReference(resource)
	}
	_ = response.Body.Close()
	id := path.Base(resource.Path)
	if !safeUploadID(id) {
		return "", fmt.Errorf("tus upload ID %q is not valid", id)
	}
	patch, err := http.NewRequest(http.MethodPatch, resource.String(), bytes.NewReader(content))
	if err != nil {
		return "", err
	}
	patch.Header.Set("Tus-Resumable", "1.0.0")
	patch.Header.Set("Upload-Offset", "0")
	patch.Header.Set("Content-Type", "application/offset+octet-stream")
	response, err = fixture.server.Client().Do(patch)
	if err != nil {
		return "", fmt.Errorf("patch tus upload: %w", err)
	}
	if response.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		return "", fmt.Errorf("patch tus upload returned %d: %s", response.StatusCode, body)
	}
	_ = response.Body.Close()
	return id, nil
}

func (fixture *uploadIntegration) status(t *testing.T, id string) publicationResult {
	t.Helper()
	response, err := fixture.server.Client().Get(fixture.server.URL + uploadStatusPath + id)
	if err != nil {
		t.Fatalf("get upload status: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("upload status returned %d: %s", response.StatusCode, body)
	}
	var result publicationResult
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode upload status: %v", err)
	}
	return result
}

func (fixture *uploadIntegration) startPartial(t *testing.T, content []byte) (string, int64) {
	t.Helper()
	metadata := strings.Join([]string{
		"filename " + base64.StdEncoding.EncodeToString([]byte("partial.bin")),
		"target " + base64.StdEncoding.EncodeToString([]byte(".")),
		"relativepath " + base64.StdEncoding.EncodeToString([]byte("partial.bin")),
	}, ",")
	create, err := http.NewRequest(http.MethodPost, fixture.server.URL+"/api/v1/uploads/", nil)
	if err != nil {
		t.Fatal(err)
	}
	create.Header.Set("Tus-Resumable", "1.0.0")
	create.Header.Set("Upload-Length", fmt.Sprint(len(content)))
	create.Header.Set("Upload-Metadata", metadata)
	response, err := fixture.server.Client().Do(create)
	if err != nil {
		t.Fatalf("create partial upload: %v", err)
	}
	if response.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		t.Fatalf("create partial upload returned %d: %s", response.StatusCode, body)
	}
	resource, err := url.Parse(response.Header.Get("Location"))
	if err != nil {
		_ = response.Body.Close()
		t.Fatal(err)
	}
	if !resource.IsAbs() {
		base, _ := url.Parse(fixture.server.URL)
		resource = base.ResolveReference(resource)
	}
	_ = response.Body.Close()
	id := path.Base(resource.Path)
	patch, err := http.NewRequest(http.MethodPatch, resource.String(), bytes.NewReader(content[:3]))
	if err != nil {
		t.Fatal(err)
	}
	patch.Header.Set("Tus-Resumable", "1.0.0")
	patch.Header.Set("Upload-Offset", "0")
	patch.Header.Set("Content-Type", "application/offset+octet-stream")
	response, err = fixture.server.Client().Do(patch)
	if err != nil {
		t.Fatalf("patch partial upload: %v", err)
	}
	if response.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		t.Fatalf("patch partial upload returned %d: %s", response.StatusCode, body)
	}
	_ = response.Body.Close()
	return id, 3
}

func (fixture *uploadIntegration) waitStatus(t *testing.T, id string, want publicationStatus) publicationResult {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		result := fixture.status(t, id)
		if result.Status == want {
			return result
		}
		if result.Status == publicationFailed || result.Status == publicationPartial || result.Status == publicationCancelled {
			t.Fatalf("upload entered terminal state %q while waiting for %q: %#v", result.Status, want, result)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("upload did not reach %q; latest status: %#v", want, fixture.status(t, id))
	return publicationResult{}
}

func TestUploadPublicationUsesNumberedNameAndPersistsAudit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows file store currently fails closed when atomic no-replace publication is unavailable")
	}
	fixture := newUploadIntegration(t)
	if err := os.Mkdir(filepath.Join(fixture.rootPath, "docs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.rootPath, "docs", "report.txt"), []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.rootPath, "docs", "report (2).txt"), []byte("existing suffix"), 0o600); err != nil {
		t.Fatal(err)
	}
	content := []byte("new report")
	id := fixture.upload(t, "report.txt", "docs", "report.txt", content)
	result := fixture.waitStatus(t, id, publicationSucceeded)
	if result.PublishedPath != "docs/report (3).txt" || result.Offset != int64(len(content)) || result.Size != int64(len(content)) {
		t.Fatalf("published result = %#v", result)
	}
	got, err := os.ReadFile(filepath.Join(fixture.rootPath, filepath.FromSlash(result.PublishedPath)))
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("published bytes = %q, err=%v", got, err)
	}
	existing, err := os.ReadFile(filepath.Join(fixture.rootPath, "docs", "report.txt"))
	if err != nil || string(existing) != "existing" {
		t.Fatalf("existing file changed: %q, err=%v", existing, err)
	}
	if _, err := os.Stat(filepath.Join(fixture.staging, id)); !os.IsNotExist(err) {
		t.Fatalf("successful publication should remove staging data, stat err=%v", err)
	}
	audit, err := fixture.metadata.AuditForTask(context.Background(), id)
	if err != nil || len(audit) != 4 {
		t.Fatalf("audit events = %d, err=%v; expected one each for receiving, queued, publishing and published", len(audit), err)
	}
}

func TestUploadPublicationFailureRetainsStageAndCanRetry(t *testing.T) {
	fixture := newUploadIntegration(t)
	content := []byte("retry me")
	id := fixture.upload(t, "retry.txt", "missing", "retry.txt", content)
	failed := fixture.waitStatus(t, id, publicationFailed)
	if failed.Error == "" || failed.PublishedPath != "" {
		t.Fatalf("failure result should explain failure without a published path: %#v", failed)
	}
	if got, err := os.ReadFile(filepath.Join(fixture.staging, id)); err != nil || !bytes.Equal(got, content) {
		t.Fatalf("failed publication must retain staged bytes: %q, err=%v", got, err)
	}
	if err := os.Mkdir(filepath.Join(fixture.rootPath, "missing"), 0o700); err != nil {
		t.Fatal(err)
	}
	retry, err := http.NewRequest(http.MethodPost, fixture.server.URL+uploadStatusPath+id+"/retry", nil)
	if err != nil {
		t.Fatal(err)
	}
	retry.Header.Set("Origin", fixture.server.URL)
	retry.Header.Set("Sec-Fetch-Site", "same-origin")
	response, err := fixture.server.Client().Do(retry)
	if err != nil {
		t.Fatalf("retry upload publication: %v", err)
	}
	if response.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		t.Fatalf("retry returned %d: %s", response.StatusCode, body)
	}
	_ = response.Body.Close()
	if runtime.GOOS == "windows" {
		failed = fixture.waitStatus(t, id, publicationFailed)
		if !strings.Contains(failed.Error, "禁止覆盖") {
			t.Fatalf("Windows publication should fail closed without no-replace support: %#v", failed)
		}
		if got, err := os.ReadFile(filepath.Join(fixture.staging, id)); err != nil || !bytes.Equal(got, content) {
			t.Fatalf("unsupported publication must retain staged data: %q, err=%v", got, err)
		}
		return
	}
	result := fixture.waitStatus(t, id, publicationSucceeded)
	if result.PublishedPath != "missing/retry.txt" {
		t.Fatalf("retry published path = %q", result.PublishedPath)
	}
}

func TestUploadPublicationExplainsInsufficientStorage(t *testing.T) {
	manager := &publicationManager{}
	want := "服务器存储空间或配额不足，文件未发布；完整暂存文件保留，清理空间或调整配额后可重试。"
	for _, cause := range []error{syscall.ENOSPC, syscall.EDQUOT} {
		got := manager.safePublicationError(fmt.Errorf("write candidate: %w", cause))
		if got != want {
			t.Errorf("safePublicationError(%v) = %q, want %q", cause, got, want)
		}
	}
}

func TestUploadStatusRefreshesPersistedReceivingOffset(t *testing.T) {
	fixture := newUploadIntegration(t)
	content := []byte("eightbytes")
	id, offset := fixture.startPartial(t, content)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		result, err := fixture.manager.loadResult(id)
		if err == nil && result.Status == publicationReceiving {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	result := fixture.status(t, id)
	if result.Status != publicationReceiving || result.Offset != offset || result.Size != int64(len(content)) {
		t.Fatalf("receiving upload status = %#v, want offset=%d size=%d", result, offset, len(content))
	}
}

func TestTusTerminationPersistsCancelledStatus(t *testing.T) {
	fixture := newUploadIntegration(t)
	content := []byte("partial content")
	id, _ := fixture.startPartial(t, content)
	request, err := http.NewRequest(http.MethodDelete, fixture.server.URL+"/api/v1/uploads/"+id, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Tus-Resumable", "1.0.0")
	response, err := fixture.server.Client().Do(request)
	if err != nil {
		t.Fatalf("terminate tus upload: %v", err)
	}
	if response.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		t.Fatalf("terminate tus upload returned %d: %s", response.StatusCode, body)
	}
	_ = response.Body.Close()
	result := fixture.waitStatus(t, id, publicationCancelled)
	if result.Error == "" {
		t.Fatalf("cancelled upload should include a result message: %#v", result)
	}
	if _, err := os.Stat(filepath.Join(fixture.staging, id)); !os.IsNotExist(err) {
		t.Fatalf("terminated upload should remove private staging data, stat err=%v", err)
	}
}

func TestUploadStatusReportsTerminationWhileEventIsPending(t *testing.T) {
	fixture := newUploadIntegration(t)
	fixture.manager.persistMu.Lock()
	lockHeld := true
	defer func() {
		if lockHeld {
			fixture.manager.persistMu.Unlock()
		}
	}()
	id, _ := fixture.startPartial(t, []byte("partial content"))
	request, err := http.NewRequest(http.MethodDelete, fixture.server.URL+"/api/v1/uploads/"+id, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Tus-Resumable", "1.0.0")
	response, err := fixture.server.Client().Do(request)
	if err != nil {
		t.Fatalf("terminate tus upload: %v", err)
	}
	if response.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		t.Fatalf("terminate tus upload returned %d: %s", response.StatusCode, body)
	}
	_ = response.Body.Close()
	result := fixture.status(t, id)
	if result.Status != publicationCancelling {
		t.Fatalf("status during pending termination = %#v, want cancelling", result)
	}

	fixture.manager.persistMu.Unlock()
	lockHeld = false
	result = fixture.waitStatus(t, id, publicationCancelled)
	if !strings.Contains(result.Error, "暂存数据已移除") {
		t.Fatalf("final cancellation status = %#v", result)
	}
}

func TestConcurrentTusDeleteCancelsPublicationAndRecordsRemovedStage(t *testing.T) {
	fixture := newUploadIntegration(t)
	if err := os.Mkdir(filepath.Join(fixture.rootPath, "docs"), 0o700); err != nil {
		t.Fatal(err)
	}

	target := "docs/cancel.bin"
	release, err := fixture.manager.locks.Lock(context.Background(), uploadLockKeys(target)...)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	content := []byte("cancel while publication waits for the target lock")
	id := fixture.upload(t, "cancel.bin", "docs", "cancel.bin", content)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		fixture.manager.mu.Lock()
		_, publishing := fixture.manager.workerDone[id]
		fixture.manager.mu.Unlock()
		if publishing {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	fixture.manager.mu.Lock()
	_, publishing := fixture.manager.workerDone[id]
	fixture.manager.mu.Unlock()
	if !publishing {
		t.Fatal("upload publisher did not start while target lock was held")
	}

	request, err := http.NewRequest(http.MethodDelete, fixture.server.URL+"/api/v1/uploads/"+id, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Tus-Resumable", "1.0.0")
	response, err := fixture.server.Client().Do(request)
	if err != nil {
		t.Fatalf("delete upload during publication: %v", err)
	}
	if response.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		t.Fatalf("delete upload returned %d: %s", response.StatusCode, body)
	}
	_ = response.Body.Close()

	deadline = time.Now().Add(5 * time.Second)
	var result publicationResult
	for time.Now().Before(deadline) {
		result = fixture.status(t, id)
		_, stageErr := os.Stat(filepath.Join(fixture.staging, id))
		if result.Status == publicationCancelled && strings.Contains(result.Error, "暂存数据已移除") && os.IsNotExist(stageErr) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if result.Status != publicationCancelled || !strings.Contains(result.Error, "暂存数据已移除") || strings.Contains(result.Error, "可重试") || strings.Contains(result.Error, "仍保留") {
		t.Fatalf("deleted staging data must be reported as removed, not retryable: %#v", result)
	}
	if _, err := os.Stat(filepath.Join(fixture.staging, id)); !os.IsNotExist(err) {
		t.Fatalf("DELETE should remove staging data, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(fixture.rootPath, filepath.FromSlash(target))); !os.IsNotExist(err) {
		t.Fatalf("cancelled publication must not create its target, stat err=%v", err)
	}
}

func TestUploadStatusReconcilesTusCompletionAndKeepsPublishedResult(t *testing.T) {
	temp := t.TempDir()
	rootPath := filepath.Join(temp, "managed")
	staging := filepath.Join(temp, "data", "staging")
	if err := os.MkdirAll(rootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	metadata, err := store.Open(context.Background(), filepath.Join(temp, "data", "filesage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	application, err := upload.New(upload.Config{StagingDir: staging, ManagedRoot: rootPath})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	httpServer := httptest.NewServer(application)
	defer httpServer.Close()
	fixture := &uploadIntegration{server: httpServer}
	content := []byte("complete")
	id := fixture.upload(t, "complete.txt", ".", "complete.txt", content)
	manager := newPublicationManager(application, root, coordination.New(), metadata)
	event, err := application.TransferStatus(id)
	if err != nil || event.Kind != upload.UploadCompleted {
		t.Fatalf("completed transfer status = %#v, err=%v", event, err)
	}
	manager.recordReceiving(event)
	request := httptest.NewRequest(http.MethodGet, uploadStatusPath+id, nil)
	response := httptest.NewRecorder()
	manager.ServeHTTP(response, request)
	var refreshed publicationResult
	if err := json.Unmarshal(response.Body.Bytes(), &refreshed); err != nil {
		t.Fatal(err)
	}
	if refreshed.Status != publicationQueued || refreshed.Offset != int64(len(content)) {
		t.Fatalf("completed tus transfer should no longer appear receiving: %#v", refreshed)
	}
	published := refreshed
	published.Status = publicationSucceeded
	published.PublishedPath = "complete.txt"
	published.SHA256 = "test-digest"
	if err := manager.persist(published); err != nil {
		t.Fatal(err)
	}
	if err := application.DiscardCompleted(event); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	manager.ServeHTTP(response, request)
	if err := json.Unmarshal(response.Body.Bytes(), &refreshed); err != nil {
		t.Fatal(err)
	}
	if refreshed.Status != publicationSucceeded || refreshed.PublishedPath != "complete.txt" {
		t.Fatalf("cleaned tus stage replaced persisted terminal result: %#v", refreshed)
	}
}

func TestLateCreatedEventDoesNotRegressUploadState(t *testing.T) {
	temp := t.TempDir()
	metadata, err := store.Open(context.Background(), filepath.Join(temp, "filesage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	manager := newPublicationManager(nil, nil, nil, metadata)
	event := upload.Event{Kind: upload.UploadCompleted, UploadID: "0123456789abcdef0123456789abcdef", Size: 4, Offset: 4,
		Metadata: upload.Metadata{FileName: "a.txt", Target: ".", RelativePath: "a.txt"}}
	queued := newPublicationResult(event)
	queued.Status = publicationQueued
	if err := manager.persist(queued); err != nil {
		t.Fatal(err)
	}
	event.Kind = upload.UploadCreated
	manager.recordReceiving(event)
	current, err := manager.loadResult(event.UploadID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != publicationQueued {
		t.Fatalf("late created event regressed upload state to %q", current.Status)
	}
	audit, err := metadata.AuditForTask(context.Background(), event.UploadID)
	if err != nil || len(audit) != 1 || audit[0].Outcome != string(publicationQueued) {
		t.Fatalf("audit after reordered event = %#v, err=%v", audit, err)
	}
}

func TestRemovedTerminationStateCannotBeOverwrittenAfterRestart(t *testing.T) {
	temp := t.TempDir()
	metadata, err := store.Open(context.Background(), filepath.Join(temp, "filesage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()

	event := upload.Event{
		Kind:     upload.UploadTerminated,
		UploadID: "abcdef0123456789abcdef0123456789",
		Size:     4,
		Offset:   4,
		Metadata: upload.Metadata{FileName: "removed.txt", Target: ".", RelativePath: "removed.txt"},
	}
	manager := newPublicationManager(nil, nil, nil, metadata)
	manager.recordTermination(event)

	restarted := newPublicationManager(nil, nil, nil, metadata)
	stale := newPublicationResult(event)
	stale.Status = publicationCancelled
	stale.Error = "发布已取消，完整暂存文件仍保留，可重试。"
	if err := restarted.persist(stale); err != nil {
		t.Fatalf("persist stale worker state: %v", err)
	}
	current, err := restarted.loadResult(event.UploadID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != publicationCancelled || !strings.Contains(current.Error, "暂存数据已移除") || strings.Contains(current.Error, "可重试") {
		t.Fatalf("removed termination state was overwritten by stale worker state: %#v", current)
	}
}

func TestUploadRetryOriginValidation(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "http://filesage.test/api/v1/uploads/status/id/retry", nil)
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	request.Header.Set("Origin", "http://filesage.test:80")
	if !sameOriginMutation(request) {
		t.Fatal("equivalent default-port origin should be accepted")
	}
	request.Header.Set("Origin", "https://filesage.test")
	if sameOriginMutation(request) {
		t.Fatal("different origin scheme must be rejected")
	}
	request.Header.Set("Origin", "http://evil.test")
	if sameOriginMutation(request) {
		t.Fatal("cross-site origin must be rejected")
	}
}

func TestUploadStateDoesNotAdvanceWhenStoreWriteFails(t *testing.T) {
	temp := t.TempDir()
	metadata, err := store.Open(context.Background(), filepath.Join(temp, "filesage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	manager := newPublicationManager(nil, nil, nil, metadata)
	event := upload.Event{UploadID: "abcdef0123456789abcdef0123456789", Size: 3, Offset: 3,
		Metadata: upload.Metadata{FileName: "a.txt", Target: ".", RelativePath: "a.txt"}}
	queued := newPublicationResult(event)
	queued.Status = publicationQueued
	if err := manager.persist(queued); err != nil {
		t.Fatal(err)
	}
	if err := metadata.Close(); err != nil {
		t.Fatal(err)
	}
	published := queued
	published.Status = publicationSucceeded
	published.PublishedPath = "a.txt"
	if err := manager.persist(published); err == nil {
		t.Fatal("closed store must reject state and audit persistence")
	}
	current, err := manager.loadResult(event.UploadID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != publicationQueued {
		t.Fatalf("failed persistence advanced cached state to %q", current.Status)
	}
}

func TestUploadPublicationRecoveryAfterCommitBeforeSuccessRecord(t *testing.T) {
	temp := t.TempDir()
	rootPath := filepath.Join(temp, "managed")
	staging := filepath.Join(temp, "data", "staging")
	if err := os.MkdirAll(filepath.Join(rootPath, "docs"), 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := store.Open(context.Background(), filepath.Join(temp, "data", "filesage.sqlite"))
	if err != nil {
		_ = root.Close()
		t.Fatal(err)
	}
	firstUploads, err := upload.New(upload.Config{StagingDir: staging, ManagedRoot: rootPath, MaxUploadSize: 1 << 20})
	if err != nil {
		_ = metadata.Close()
		_ = root.Close()
		t.Fatal(err)
	}
	firstHTTP := httptest.NewServer(firstUploads)
	first := &uploadIntegration{server: firstHTTP, staging: staging}
	content := []byte("committed before status save")
	id := first.upload(t, "recover.txt", "docs", "recover.txt", content)
	firstHTTP.Close()
	firstUploads.Close()

	candidate := "docs/recover.txt"
	if err := os.WriteFile(filepath.Join(rootPath, filepath.FromSlash(candidate)), content, 0o600); err != nil {
		t.Fatal(err)
	}
	preRecoveryUploads, err := upload.New(upload.Config{StagingDir: staging, ManagedRoot: rootPath, MaxUploadSize: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer preRecoveryUploads.Close()
	defer root.Close()
	defer metadata.Close()
	manager := newPublicationManager(preRecoveryUploads, root, coordination.New(), metadata)
	event, err := preRecoveryUploads.TransferStatus(id)
	if err != nil || event.Kind != upload.UploadCompleted {
		t.Fatalf("completed upload status = %#v, err=%v", event, err)
	}
	prior := newPublicationResult(event)
	prior.Status = publicationRunning
	prior.CandidatePath = candidate
	if err := manager.persist(prior); err != nil {
		t.Fatalf("persist pre-crash candidate: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- manager.run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	var recovered publicationResult
	for time.Now().Before(deadline) {
		recovered, err = manager.loadResult(id)
		if err == nil && recovered.Status == publicationSucceeded {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if recovered.Status != publicationSucceeded || recovered.PublishedPath != candidate {
		cancel()
		<-done
		t.Fatalf("recovered status = %#v", recovered)
	}
	if _, err := os.Stat(filepath.Join(staging, id)); !os.IsNotExist(err) {
		cancel()
		<-done
		t.Fatalf("recovery should clean published staging data, stat err=%v", err)
	}
	cancel()
	if err := <-done; err != nil && err != context.Canceled {
		t.Fatalf("stop recovery manager: %v", err)
	}
}

func TestConcurrentUploadsReceiveUniqueNames(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows file store currently fails closed when atomic no-replace publication is unavailable")
	}
	fixture := newUploadIntegration(t)
	if err := os.Mkdir(filepath.Join(fixture.rootPath, "docs"), 0o700); err != nil {
		t.Fatal(err)
	}
	type uploaded struct {
		id  string
		err error
	}
	results := make(chan uploaded, 2)
	for index := 0; index < 2; index++ {
		go func() {
			id, err := fixture.startUpload("same.bin", "docs", "same.bin", []byte("same bytes"))
			results <- uploaded{id: id, err: err}
		}()
	}
	ids := make([]string, 0, 2)
	for range 2 {
		select {
		case result := <-results:
			if result.err != nil {
				t.Fatal(result.err)
			}
			ids = append(ids, result.id)
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent tus uploads did not finish")
		}
	}
	paths := map[string]bool{}
	for _, id := range ids {
		result := fixture.waitStatus(t, id, publicationSucceeded)
		paths[result.PublishedPath] = true
	}
	if len(paths) != 2 || !paths["docs/same.bin"] || !paths["docs/same (2).bin"] {
		t.Fatalf("concurrent upload paths = %#v", paths)
	}
}
