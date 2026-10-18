// 本文件验证软链接创建接口、重名竞争与操作审计。
package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"filesage/internal/coordination"
	"filesage/internal/fsroot"
	"filesage/internal/store"
)

type competingSymlinkFileSystem struct {
	*fsroot.Root
	rootPath  string
	competeAt string
	competed  bool
}

func (fileSystem *competingSymlinkFileSystem) SymlinkNoReplace(targetPath, linkPath string) error {
	if !fileSystem.competed && linkPath == fileSystem.competeAt {
		fileSystem.competed = true
		if err := os.WriteFile(filepath.Join(fileSystem.rootPath, filepath.FromSlash(linkPath)), []byte("external"), 0o644); err != nil {
			return err
		}
		return fsroot.ErrConflict
	}
	return fileSystem.Root.SymlinkNoReplace(targetPath, linkPath)
}

func TestCreateSymlinkNumbersNameAndAuditsResolvedPaths(t *testing.T) {
	rootPath := t.TempDir()
	for _, directory := range []string{"docs", "links", "links/old"} {
		if err := os.MkdirAll(filepath.Join(rootPath, filepath.FromSlash(directory)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(rootPath, "docs", "manual.pdf"), []byte("manual"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"manual", "manual (2)"} {
		if err := os.WriteFile(filepath.Join(rootPath, "links", name), []byte("keep"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	requireSymlinkCreateSupport(t, root, "docs/manual.pdf")
	metadata, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "audit.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	api := &API{files: root, audits: metadata, locks: coordination.New()}
	response := performSymlinkCreate(api, `{"parent":"links","name":"manual","target":"docs/manual.pdf"}`, nil)
	if response.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", response.Code, response.Body.String())
	}
	var result pathResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Path != "links/manual (3)" || result.Name != "manual (3)" {
		t.Fatalf("create response = %+v", result)
	}
	entry, err := root.LstatEntry(result.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !entry.Symlink || entry.LinkStatus != fsroot.LinkStatusInternal || entry.LinkTarget != "../docs/manual.pdf" {
		t.Fatalf("created link entry = %+v", entry)
	}

	page, err := metadata.AuditHistoryPage(context.Background(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 {
		t.Fatalf("audit records = %+v", page.Events)
	}
	event := page.Events[0]
	if event.EventType != "manual_create_symlink" || event.SourcePath != "docs/manual.pdf" || event.TargetPath != result.Path || event.Outcome != "succeeded" {
		t.Fatalf("symlink audit = %+v", event)
	}
}

func TestCreateSymlinkRecordsFailedTargetAndRejectsBadRequests(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "links"), 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	metadata, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "audit.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	api := &API{files: root, audits: metadata, locks: coordination.New()}

	for _, body := range []string{
		`{"name":"alias","target":"target"}`,
		`{"parent":"links","name":"alias"}`,
		`{"parent":"links","name":"alias","target":"../outside"}`,
		`{"parent":"../outside","name":"alias","target":"target"}`,
		`{"parent":"links","name":"a/b","target":"target"}`,
		`{"parent":"links","name":"alias","target":"target","extra":true}`,
	} {
		response := performSymlinkCreate(api, body, nil)
		if response.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400: %s", body, response.Code, response.Body.String())
		}
	}
	crossOrigin := http.Header{"Origin": []string{"http://attacker.test"}}
	response := performSymlinkCreate(api, `{"parent":"links","name":"alias","target":"missing"}`, crossOrigin)
	if response.Code != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d, want 403: %s", response.Code, response.Body.String())
	}

	response = performSymlinkCreate(api, `{"parent":"links","name":"alias","target":"missing"}`, nil)
	if response.Code != http.StatusNotFound {
		t.Fatalf("missing target status = %d, want 404: %s", response.Code, response.Body.String())
	}
	page, err := metadata.AuditHistoryPage(context.Background(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.Events[0].EventType != "manual_create_symlink" || page.Events[0].SourcePath != "missing" || page.Events[0].TargetPath != "links/alias" || page.Events[0].Outcome != "failed" {
		t.Fatalf("failed link audit = %+v", page.Events)
	}
}

func TestCreateSymlinkRetriesExternalNameRace(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "links"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "target"), []byte("target"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	requireSymlinkCreateSupport(t, root, "target")
	fileSystem := &competingSymlinkFileSystem{Root: root, rootPath: rootPath, competeAt: "links/shortcut"}
	metadata, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "audit.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	api := &API{files: fileSystem, audits: metadata, locks: coordination.New()}
	response := performSymlinkCreate(api, `{"parent":"links","name":"shortcut","target":"target"}`, nil)
	if response.Code != http.StatusCreated || !strings.Contains(response.Body.String(), `"path":"links/shortcut (2)"`) {
		t.Fatalf("race retry response = %d %s", response.Code, response.Body.String())
	}
	if contents, err := os.ReadFile(filepath.Join(rootPath, "links", "shortcut")); err != nil || string(contents) != "external" {
		t.Fatalf("external competing entry changed: contents=%q err=%v", contents, err)
	}
	if entry, err := root.LstatEntry("links/shortcut (2)"); err != nil || !entry.Symlink {
		t.Fatalf("numbered link entry = %+v err=%v", entry, err)
	}
	page, err := metadata.AuditHistoryPage(context.Background(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 2 || page.Events[0].Outcome != "succeeded" || page.Events[0].TargetPath != "links/shortcut (2)" || page.Events[1].Outcome != "failed" || page.Events[1].TargetPath != "links/shortcut" {
		t.Fatalf("race audit records = %+v", page.Events)
	}
}

func requireSymlinkCreateSupport(t *testing.T, root *fsroot.Root, target string) {
	t.Helper()
	err := root.SymlinkNoReplace(target, "symlink-support-check")
	if err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symbolic links are unavailable: %v", err)
		}
		t.Fatal(err)
	}
	if err := root.Remove("symlink-support-check"); err != nil {
		t.Fatal(err)
	}
}

func performSymlinkCreate(api *API, body string, headers http.Header) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "http://filesage.test/api/v1/files/symlinks", strings.NewReader(body))
	for name, values := range headers {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	response := httptest.NewRecorder()
	api.createSymlink(response, request)
	return response
}

var _ symlinkFileSystem = (*fsroot.Root)(nil)
var _ symlinkFileSystem = (*competingSymlinkFileSystem)(nil)
