package test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
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

	"filesage/internal/upload"
)

const uploadBasePath = "/api/v1/uploads/"

type runningUploadServer struct {
	application *upload.Server
	http        *httptest.Server
}

func startUploadServer(t *testing.T, managedRoot, stagingDir string) *runningUploadServer {
	t.Helper()
	application, err := upload.New(upload.Config{
		ManagedRoot:   managedRoot,
		StagingDir:    stagingDir,
		BasePath:      uploadBasePath,
		MaxUploadSize: 1 << 20,
	})
	if err != nil {
		t.Fatalf("create upload handler: %v", err)
	}
	httpServer := httptest.NewServer(application)
	running := &runningUploadServer{application: application, http: httpServer}
	t.Cleanup(running.Close)
	return running
}

func (server *runningUploadServer) Close() {
	server.http.Close()
	server.application.Close()
}

func TestTusProtocolAndRestartResume(t *testing.T) {
	tempDir := t.TempDir()
	managedRoot := filepath.Join(tempDir, "managed")
	stagingDir := filepath.Join(tempDir, "service-data", "upload-staging")
	if err := os.MkdirAll(managedRoot, 0o700); err != nil {
		t.Fatal(err)
	}

	first := startUploadServer(t, managedRoot, stagingDir)
	optionsResponse := request(t, first.http.Client(), http.MethodOptions, first.http.URL+uploadBasePath, nil, map[string]string{})
	if optionsResponse.StatusCode != http.StatusOK {
		closeBody(optionsResponse)
		t.Fatalf("OPTIONS returned %d, want %d", optionsResponse.StatusCode, http.StatusOK)
	}
	if got := optionsResponse.Header.Get("Tus-Version"); got != "1.0.0" {
		closeBody(optionsResponse)
		t.Fatalf("Tus-Version = %q, want 1.0.0", got)
	}
	extensions := optionsResponse.Header.Get("Tus-Extension")
	if !strings.Contains(extensions, "creation") || !strings.Contains(extensions, "termination") {
		closeBody(optionsResponse)
		t.Fatalf("Tus-Extension = %q, want creation and termination", extensions)
	}
	if strings.Contains(extensions, "concatenation") || strings.Contains(extensions, "creation-defer-length") {
		closeBody(optionsResponse)
		t.Fatalf("unsupported tus extensions were advertised: %q", extensions)
	}
	closeBody(optionsResponse)

	metadata := map[string]string{
		"filename":     "photo.png",
		"target":       "photos",
		"relativepath": "2025/08/photo.png",
		"filetype":     "image/png",
	}
	createResponse := request(t, first.http.Client(), http.MethodPost, first.http.URL+uploadBasePath, nil, map[string]string{
		"Tus-Resumable":   "1.0.0",
		"Upload-Length":   "6",
		"Upload-Metadata": encodeMetadata(metadata),
	})
	if createResponse.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(createResponse.Body)
		closeBody(createResponse)
		t.Fatalf("create returned %d, want %d: %s", createResponse.StatusCode, http.StatusCreated, body)
	}
	resourceURL := resolveLocation(t, first.http.URL, createResponse.Header.Get("Location"))
	closeBody(createResponse)
	created := receiveEvent(t, first.application.Events(), upload.UploadCreated)
	if created.Size != 6 || created.Metadata.FileName != "photo.png" || created.Metadata.Target != "photos" || created.Metadata.RelativePath != "2025/08/photo.png" || created.Metadata.ContentType != "image/png" {
		t.Fatalf("created event = %#v, unexpected validated upload fields", created)
	}

	headBeforeResume := request(t, first.http.Client(), http.MethodHead, resourceURL, nil, map[string]string{"Tus-Resumable": "1.0.0"})
	assertStatus(t, headBeforeResume, http.StatusOK)
	if got := headBeforeResume.Header.Get("Upload-Offset"); got != "0" {
		closeBody(headBeforeResume)
		t.Fatalf("initial Upload-Offset = %q, want 0", got)
	}
	if got := headBeforeResume.Header.Get("Upload-Length"); got != "6" {
		closeBody(headBeforeResume)
		t.Fatalf("Upload-Length = %q, want 6", got)
	}
	closeBody(headBeforeResume)

	firstPatch := patchUpload(t, first.http.Client(), resourceURL, 0, "abc")
	assertStatus(t, firstPatch, http.StatusNoContent)
	if got := firstPatch.Header.Get("Upload-Offset"); got != "3" {
		closeBody(firstPatch)
		t.Fatalf("PATCH Upload-Offset = %q, want 3", got)
	}
	closeBody(firstPatch)
	assertOpenCompletedRejected(t, first.application, created)
	partialEvent := created
	partialEvent.Kind = upload.UploadCompleted
	assertOpenCompletedRejected(t, first.application, partialEvent)

	wrongOffset := patchUpload(t, first.http.Client(), resourceURL, 0, "xxx")
	assertStatus(t, wrongOffset, http.StatusConflict)
	closeBody(wrongOffset)

	// Simulate a service restart after a partial transfer. tusd's metadata and
	// bytes remain in the private disk store and the new handler resumes at the
	// persisted offset.
	first.Close()
	second := startUploadServer(t, managedRoot, stagingDir)
	restartedResourceURL := useServerBase(t, second.http.URL, resourceURL)
	headAfterRestart := request(t, second.http.Client(), http.MethodHead, restartedResourceURL, nil, map[string]string{"Tus-Resumable": "1.0.0"})
	assertStatus(t, headAfterRestart, http.StatusOK)
	if got := headAfterRestart.Header.Get("Upload-Offset"); got != "3" {
		closeBody(headAfterRestart)
		t.Fatalf("restarted Upload-Offset = %q, want 3", got)
	}
	closeBody(headAfterRestart)

	finalPatch := patchUpload(t, second.http.Client(), restartedResourceURL, 3, "def")
	assertStatus(t, finalPatch, http.StatusNoContent)
	if got := finalPatch.Header.Get("Upload-Offset"); got != "6" {
		closeBody(finalPatch)
		t.Fatalf("final Upload-Offset = %q, want 6", got)
	}
	closeBody(finalPatch)
	completed := receiveEvent(t, second.application.Events(), upload.UploadCompleted)
	if completed.UploadID != created.UploadID || completed.Size != 6 || completed.Metadata != created.Metadata {
		t.Fatalf("completed event = %#v, want same upload identity, size and metadata as creation", completed)
	}
	stagedReader, stagedMetadata, stagedSize, err := second.application.OpenCompleted(completed)
	if err != nil {
		t.Fatalf("open completed upload: %v", err)
	}
	stagedBytesFromAPI, err := io.ReadAll(stagedReader)
	if closeErr := stagedReader.Close(); closeErr != nil {
		t.Fatalf("close completed upload reader: %v", closeErr)
	}
	if err != nil {
		t.Fatalf("read completed upload: %v", err)
	}
	if string(stagedBytesFromAPI) != "abcdef" || stagedMetadata != completed.Metadata || stagedSize != 6 {
		t.Fatalf("OpenCompleted = (%q, %#v, %d), want (%q, %#v, 6)", stagedBytesFromAPI, stagedMetadata, stagedSize, "abcdef", completed.Metadata)
	}

	wrongID := completed
	wrongID.UploadID = "../managed/secret.txt"
	assertOpenCompletedRejected(t, second.application, wrongID)
	unknownID := completed
	unknownID.UploadID = "00000000000000000000000000000000"
	assertOpenCompletedRejected(t, second.application, unknownID)
	wrongSize := completed
	wrongSize.Size++
	assertOpenCompletedRejected(t, second.application, wrongSize)
	wrongMetadata := completed
	wrongMetadata.Metadata.FileName = "other.png"
	assertOpenCompletedRejected(t, second.application, wrongMetadata)

	getResponse := request(t, second.http.Client(), http.MethodGet, restartedResourceURL, nil, nil)
	assertStatus(t, getResponse, http.StatusMethodNotAllowed)
	closeBody(getResponse)

	stagedBytes, err := os.ReadFile(filepath.Join(stagingDir, completed.UploadID))
	if err != nil {
		t.Fatalf("read privately staged upload: %v", err)
	}
	if string(stagedBytes) != "abcdef" {
		t.Fatalf("staged bytes = %q, want abcdef", stagedBytes)
	}
	rootEntries, err := os.ReadDir(managedRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(rootEntries) != 0 {
		t.Fatalf("managed root contains staged content: %v", rootEntries)
	}
	if runtime.GOOS != "windows" {
		stagingInfo, err := os.Stat(stagingDir)
		if err != nil {
			t.Fatal(err)
		}
		if got := stagingInfo.Mode().Perm(); got != 0o700 {
			t.Fatalf("staging directory permissions = %04o, want 0700", got)
		}
		fileInfo, err := os.Stat(filepath.Join(stagingDir, completed.UploadID))
		if err != nil {
			t.Fatal(err)
		}
		if got := fileInfo.Mode().Perm(); got != 0o600 {
			t.Fatalf("staged file permissions = %04o, want 0600", got)
		}
	}

	if err := os.WriteFile(filepath.Join(stagingDir, completed.UploadID), []byte("changed"), 0o600); err != nil {
		t.Fatalf("change staged file for validation: %v", err)
	}
	assertOpenCompletedRejected(t, second.application, completed)

	managedSentinel := filepath.Join(managedRoot, "secret.txt")
	if err := os.WriteFile(managedSentinel, []byte("managed data"), 0o600); err != nil {
		t.Fatalf("create managed sentinel: %v", err)
	}
	infoPath := filepath.Join(stagingDir, completed.UploadID+".info")
	originalInfo, err := os.ReadFile(infoPath)
	if err != nil {
		t.Fatalf("read staged upload info: %v", err)
	}
	var storedInfo map[string]json.RawMessage
	if err := json.Unmarshal(originalInfo, &storedInfo); err != nil {
		t.Fatalf("decode staged upload info: %v", err)
	}
	var storage map[string]string
	if err := json.Unmarshal(storedInfo["Storage"], &storage); err != nil {
		t.Fatalf("decode staged storage path: %v", err)
	}
	storage["Path"] = managedSentinel
	storedInfo["Storage"], err = json.Marshal(storage)
	if err != nil {
		t.Fatalf("encode tampered storage path: %v", err)
	}
	tamperedInfo, err := json.Marshal(storedInfo)
	if err != nil {
		t.Fatalf("encode staged upload info: %v", err)
	}
	if err := os.WriteFile(infoPath, tamperedInfo, 0o600); err != nil {
		t.Fatalf("tamper staged upload info: %v", err)
	}
	assertOpenCompletedRejected(t, second.application, completed)
	if got, err := os.ReadFile(managedSentinel); err != nil || string(got) != "managed data" {
		t.Fatalf("managed sentinel changed while rejecting staged path: data=%q err=%v", got, err)
	}
}

func TestRejectUnsafeUploadMetadata(t *testing.T) {
	tempDir := t.TempDir()
	managedRoot := filepath.Join(tempDir, "managed")
	if err := os.MkdirAll(managedRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	server := startUploadServer(t, managedRoot, filepath.Join(tempDir, "staging"))

	tests := []struct {
		name   string
		header string
	}{
		{name: "missing filename", header: encodeMetadata(map[string]string{"target": "."})},
		{name: "filename traversal", header: encodeMetadata(map[string]string{"filename": "../secret.txt"})},
		{name: "reserved filename", header: encodeMetadata(map[string]string{"filename": ".filesage-trash"})},
		{name: "target traversal", header: encodeMetadata(map[string]string{"filename": "photo.jpg", "target": "../outside"})},
		{name: "reserved target", header: encodeMetadata(map[string]string{"filename": "photo.jpg", "target": ".filesage-trash"})},
		{name: "absolute target", header: encodeMetadata(map[string]string{"filename": "photo.jpg", "target": "/outside"})},
		{name: "relative path mismatch", header: encodeMetadata(map[string]string{"filename": "photo.jpg", "relativepath": "folder/other.jpg"})},
		{name: "reserved relative path component", header: encodeMetadata(map[string]string{"filename": "photo.jpg", "relativepath": "folder/.filesage-trash/photo.jpg"})},
		{name: "unknown key", header: encodeMetadata(map[string]string{"filename": "photo.jpg", "storage": "managed/file"})},
		{name: "duplicate key", header: "filename " + b64("photo.jpg") + ",filename " + b64("other.jpg")},
		{name: "invalid base64", header: "filename !!!"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := request(t, server.http.Client(), http.MethodPost, server.http.URL+uploadBasePath, nil, map[string]string{
				"Tus-Resumable":   "1.0.0",
				"Upload-Length":   "1",
				"Upload-Metadata": test.header,
			})
			if response.StatusCode != http.StatusBadRequest {
				body, _ := io.ReadAll(response.Body)
				closeBody(response)
				t.Fatalf("POST returned %d, want %d: %s", response.StatusCode, http.StatusBadRequest, body)
			}
			closeBody(response)
			select {
			case event := <-server.application.Events():
				t.Fatalf("invalid metadata emitted an event: %#v", event)
			default:
			}
		})
	}
}

func TestRejectStagingDirectoryOverlappingManagedRoot(t *testing.T) {
	tempDir := t.TempDir()
	managedRoot := filepath.Join(tempDir, "managed")
	if err := os.MkdirAll(managedRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	stagingDir := filepath.Join(managedRoot, ".private-staging")
	server, err := upload.New(upload.Config{ManagedRoot: managedRoot, StagingDir: stagingDir})
	if err == nil {
		server.Close()
		t.Fatal("upload handler accepted staging inside the managed root")
	}
	if _, err := os.Stat(stagingDir); !os.IsNotExist(err) {
		t.Fatalf("rejected staging path was created: stat error = %v", err)
	}
}

func patchUpload(t *testing.T, client *http.Client, resourceURL string, offset int64, data string) *http.Response {
	t.Helper()
	return request(t, client, http.MethodPatch, resourceURL, strings.NewReader(data), map[string]string{
		"Tus-Resumable": "1.0.0",
		"Upload-Offset": fmt.Sprint(offset),
		"Content-Type":  "application/offset+octet-stream",
	})
}

func request(t *testing.T, client *http.Client, method, address string, body io.Reader, headers map[string]string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, address, body)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func receiveEvent(t *testing.T, events <-chan upload.Event, kind upload.EventKind) upload.Event {
	t.Helper()
	select {
	case event, ok := <-events:
		if !ok {
			t.Fatal("upload event stream closed unexpectedly")
		}
		if event.Kind != kind {
			t.Fatalf("event kind = %q, want %q", event.Kind, kind)
		}
		return event
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %q upload event", kind)
		return upload.Event{}
	}
}

func assertOpenCompletedRejected(t *testing.T, server *upload.Server, event upload.Event) {
	t.Helper()
	reader, _, _, err := server.OpenCompleted(event)
	if reader != nil {
		_ = reader.Close()
		t.Fatal("OpenCompleted returned a reader for an invalid upload event")
	}
	if err == nil {
		t.Fatalf("OpenCompleted accepted invalid upload event: %#v", event)
	}
}

func encodeMetadata(values map[string]string) string {
	parts := make([]string, 0, len(values))
	for key, value := range values {
		parts = append(parts, key+" "+b64(value))
	}
	return strings.Join(parts, ",")
}

func b64(value string) string {
	return base64.StdEncoding.EncodeToString([]byte(value))
}

func resolveLocation(t *testing.T, baseURL, location string) string {
	t.Helper()
	base, err := url.Parse(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(location)
	if err != nil {
		t.Fatal(err)
	}
	return base.ResolveReference(parsed).String()
}

func useServerBase(t *testing.T, serverBaseURL, priorResourceURL string) string {
	t.Helper()
	resource, err := url.Parse(priorResourceURL)
	if err != nil {
		t.Fatal(err)
	}
	base, err := url.Parse(serverBaseURL)
	if err != nil {
		t.Fatal(err)
	}
	resource.Scheme = ""
	resource.Host = ""
	return base.ResolveReference(resource).String()
}

func assertStatus(t *testing.T, response *http.Response, expected int) {
	t.Helper()
	if response.StatusCode != expected {
		body, _ := io.ReadAll(response.Body)
		closeBody(response)
		t.Fatalf("HTTP status = %d, want %d: %s", response.StatusCode, expected, body)
	}
}

func closeBody(response *http.Response) {
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
}
