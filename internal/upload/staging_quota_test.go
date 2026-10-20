// 本文件验证暂存容量限制和进程重启后的上传恢复。
package upload

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tus/tusd/v2/pkg/filestore"
	tusd "github.com/tus/tusd/v2/pkg/handler"
)

// TestStagingQuotaPersistsAcrossRestartAndFreesOnTermination 验证续传预留恢复与取消释放。
func TestStagingQuotaPersistsAcrossRestartAndFreesOnTermination(t *testing.T) {
	temp := t.TempDir()
	root := filepath.Join(temp, "managed")
	staging := filepath.Join(temp, "data", "upload-staging")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}

	start := func() (*Server, *httptest.Server) {
		t.Helper()
		application, err := New(Config{StagingDir: staging, ManagedRoot: root, MaxUploadSize: 20, MaxStagingBytes: 10})
		if err != nil {
			t.Fatalf("create upload server: %v", err)
		}
		mux := http.NewServeMux()
		mux.Handle("/api/v1/uploads/", application)
		return application, httptest.NewServer(mux)
	}
	closeServer := func(application *Server, server *httptest.Server) {
		t.Helper()
		server.Close()
		application.Close()
	}

	firstApp, firstServer := start()
	id, status := createQuotaTestUpload(t, firstServer.URL, 6)
	if status != http.StatusCreated {
		t.Fatalf("first upload status = %d, want %d", status, http.StatusCreated)
	}
	patchQuotaTestUpload(t, firstServer.URL, id, []byte("abc"))
	if _, status := createQuotaTestUpload(t, firstServer.URL, 5); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("aggregate-over-limit upload status = %d, want %d", status, http.StatusRequestEntityTooLarge)
	}
	closeServer(firstApp, firstServer)

	secondApp, secondServer := start()
	t.Cleanup(func() { closeServer(secondApp, secondServer) })
	if _, status := createQuotaTestUpload(t, secondServer.URL, 5); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("restart did not restore reservation: status = %d, want %d", status, http.StatusRequestEntityTooLarge)
	}
	terminateQuotaTestUpload(t, secondServer.URL, id)
	if _, status := createQuotaTestUpload(t, secondServer.URL, 10); status != http.StatusCreated {
		t.Fatalf("terminated upload did not free quota: status = %d, want %d", status, http.StatusCreated)
	}
}

// TestStagingQuotaRejectsNewUploadsWhenStateIsInvalid 验证异常状态保留原件并阻止新增。
func TestStagingQuotaRejectsNewUploadsWhenStateIsInvalid(t *testing.T) {
	temp := t.TempDir()
	root := filepath.Join(temp, "managed")
	staging := filepath.Join(temp, "data", "upload-staging")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	invalidInfo := filepath.Join(staging, strings.Repeat("a", 32)+".info")
	if err := os.WriteFile(invalidInfo, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	application, err := New(Config{StagingDir: staging, ManagedRoot: root, MaxStagingBytes: 10})
	if err != nil {
		t.Fatalf("create upload server: %v", err)
	}
	server := httptest.NewServer(application)
	t.Cleanup(func() {
		server.Close()
		application.Close()
	})
	if _, status := createQuotaTestUpload(t, server.URL, 1); status != http.StatusInsufficientStorage {
		t.Fatalf("invalid staging state status = %d, want %d", status, http.StatusInsufficientStorage)
	}
	if _, err := os.Stat(invalidInfo); err != nil {
		t.Fatalf("invalid resumable state was removed: %v", err)
	}
}

// TestDiscardCompletedFreesStagingQuota 验证完成上传清理释放预留。
func TestDiscardCompletedFreesStagingQuota(t *testing.T) {
	temp := t.TempDir()
	root := filepath.Join(temp, "managed")
	staging := filepath.Join(temp, "data", "upload-staging")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	application, err := New(Config{StagingDir: staging, ManagedRoot: root, MaxUploadSize: 10, MaxStagingBytes: 10})
	if err != nil {
		t.Fatalf("create upload server: %v", err)
	}
	server := httptest.NewServer(application)
	t.Cleanup(func() {
		server.Close()
		application.Close()
	})

	id, status := createQuotaTestUpload(t, server.URL, 10)
	if status != http.StatusCreated {
		t.Fatalf("upload create status = %d, want %d", status, http.StatusCreated)
	}
	patchQuotaTestUpload(t, server.URL, id, []byte("0123456789"))
	event, err := application.TransferStatus(id)
	if err != nil {
		t.Fatalf("read completed upload: %v", err)
	}
	if event.Kind != UploadCompleted {
		t.Fatalf("upload event kind = %q, want %q", event.Kind, UploadCompleted)
	}
	if err := application.DiscardCompleted(event); err != nil {
		t.Fatalf("discard published staging upload: %v", err)
	}
	if _, status := createQuotaTestUpload(t, server.URL, 10); status != http.StatusCreated {
		t.Fatalf("discarded upload did not free quota: status = %d, want %d", status, http.StatusCreated)
	}
}

// TestStagingQuotaSerializesConcurrentCreates 验证并发创建不会超卖容量。
func TestStagingQuotaSerializesConcurrentCreates(t *testing.T) {
	temp := t.TempDir()
	root := filepath.Join(temp, "managed")
	staging := filepath.Join(temp, "data", "upload-staging")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	application, err := New(Config{StagingDir: staging, ManagedRoot: root, MaxStagingBytes: 10})
	if err != nil {
		t.Fatalf("create upload server: %v", err)
	}
	server := httptest.NewServer(application)
	t.Cleanup(func() {
		server.Close()
		application.Close()
	})

	statuses := make(chan int, 2)
	errors := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			request, err := newQuotaTestRequest(server.URL, 6)
			if err != nil {
				errors <- err
				return
			}
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				errors <- err
				return
			}
			statuses <- response.StatusCode
			response.Body.Close()
		}()
	}
	workers.Wait()
	close(statuses)
	close(errors)
	for err := range errors {
		t.Fatalf("create concurrent upload: %v", err)
	}
	created, rejected := 0, 0
	for status := range statuses {
		switch status {
		case http.StatusCreated:
			created++
		case http.StatusRequestEntityTooLarge:
			rejected++
		default:
			t.Fatalf("unexpected create status %d", status)
		}
	}
	if created != 1 || rejected != 1 {
		t.Fatalf("created=%d rejected=%d, want one of each", created, rejected)
	}
}

// TestStagingQuotaLimitsOutstandingUploadCount 验证空文件也受资源数限制。
func TestStagingQuotaLimitsOutstandingUploadCount(t *testing.T) {
	temp := t.TempDir()
	root := filepath.Join(temp, "managed")
	staging := filepath.Join(temp, "data", "upload-staging")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	store := filestore.New(staging)
	store.DirModePerm = 0o700
	store.FileModePerm = 0o600
	for range maxStagedUploadCount {
		if _, err := store.NewUpload(context.Background(), tusd.FileInfo{Size: 0}); err != nil {
			t.Fatalf("create existing staged upload: %v", err)
		}
	}

	application, err := New(Config{StagingDir: staging, ManagedRoot: root, MaxStagingBytes: 1})
	if err != nil {
		t.Fatalf("create upload server: %v", err)
	}
	server := httptest.NewServer(application)
	t.Cleanup(func() {
		server.Close()
		application.Close()
	})
	if _, status := createQuotaTestUpload(t, server.URL, 0); status != http.StatusTooManyRequests {
		t.Fatalf("resource-count limit status = %d, want %d", status, http.StatusTooManyRequests)
	}
}

func createQuotaTestUpload(t *testing.T, serverURL string, size int64) (string, int) {
	t.Helper()
	request, err := newQuotaTestRequest(serverURL, size)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("create upload: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		_, _ = io.Copy(io.Discard, response.Body)
		return "", response.StatusCode
	}
	resource, err := url.Parse(response.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse upload location: %v", err)
	}
	id := path.Base(resource.Path)
	if !validTusUploadID(id) {
		t.Fatalf("upload ID is invalid: %q", id)
	}
	return id, response.StatusCode
}

func newQuotaTestRequest(serverURL string, size int64) (*http.Request, error) {
	metadata := strings.Join([]string{
		"filename " + base64.StdEncoding.EncodeToString([]byte("quota.bin")),
		"target " + base64.StdEncoding.EncodeToString([]byte(".")),
		"relativepath " + base64.StdEncoding.EncodeToString([]byte("quota.bin")),
	}, ",")
	request, err := http.NewRequest(http.MethodPost, serverURL+defaultBasePath, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Tus-Resumable", "1.0.0")
	request.Header.Set("Upload-Length", fmt.Sprint(size))
	request.Header.Set("Upload-Metadata", metadata)
	return request, nil
}

func patchQuotaTestUpload(t *testing.T, serverURL, id string, content []byte) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPatch, serverURL+defaultBasePath+id, bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Tus-Resumable", "1.0.0")
	request.Header.Set("Upload-Offset", "0")
	request.Header.Set("Content-Type", "application/offset+octet-stream")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("patch upload: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("patch status = %d, want %d: %s", response.StatusCode, http.StatusNoContent, body)
	}
}

func terminateQuotaTestUpload(t *testing.T, serverURL, id string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodDelete, serverURL+defaultBasePath+id, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Tus-Resumable", "1.0.0")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("terminate upload: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("termination status = %d, want %d: %s", response.StatusCode, http.StatusNoContent, body)
	}
}
