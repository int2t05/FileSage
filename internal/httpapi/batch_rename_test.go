package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filesage/internal/coordination"
	"filesage/internal/fsroot"
	"filesage/internal/store"
)

type batchRenameTestFileSystem struct {
	entries       map[string]fsroot.Entry
	listErrorPath string
	listError     error
	failRename    string
	failRenameErr error
	afterRename   func(string, string) error
	renameCalls   int
}

func newBatchRenameTestFileSystem(entries ...fsroot.Entry) *batchRenameTestFileSystem {
	fileSystem := &batchRenameTestFileSystem{entries: make(map[string]fsroot.Entry, len(entries))}
	for _, entry := range entries {
		fileSystem.entries[entry.Path] = entry
	}
	return fileSystem
}

func batchRenameTestEntry(relativePath string, kind fsroot.Kind) fsroot.Entry {
	return fsroot.Entry{
		Name: path.Base(relativePath), Path: relativePath, Kind: kind,
		ModifiedAt: time.Unix(100, 0).UTC(), ChangedAt: time.Unix(100, 0).UTC(),
		Mode: 0o644, IdentityAvailable: true, ChangeTimeAvailable: true,
		Device: 1, Inode: uint64(len(relativePath) + 1), Links: 1,
	}
}

func (fileSystem *batchRenameTestFileSystem) List(relativePath string) ([]fsroot.Entry, error) {
	if relativePath == fileSystem.listErrorPath && fileSystem.listError != nil {
		return nil, fileSystem.listError
	}
	if relativePath != "" {
		entry, exists := fileSystem.entries[relativePath]
		if !exists {
			return nil, fs.ErrNotExist
		}
		if entry.Kind != fsroot.KindDirectory && !entry.Symlink {
			return nil, fsroot.ErrNotDirectory
		}
	}
	entries := make([]fsroot.Entry, 0)
	for _, entry := range fileSystem.entries {
		if parentPath(entry.Path) == relativePath {
			entries = append(entries, entry)
		}
	}
	return entries, nil
}

func (fileSystem *batchRenameTestFileSystem) LstatEntry(relativePath string) (fsroot.Entry, error) {
	entry, exists := fileSystem.entries[relativePath]
	if !exists {
		return fsroot.Entry{}, fs.ErrNotExist
	}
	return entry, nil
}

func (fileSystem *batchRenameTestFileSystem) OpenRead(string) (io.ReadCloser, fsroot.Entry, error) {
	return nil, fsroot.Entry{}, fs.ErrNotExist
}

func (fileSystem *batchRenameTestFileSystem) Mkdir(string, fs.FileMode) error {
	return fsroot.ErrNoReplaceUnsupported
}

func (fileSystem *batchRenameTestFileSystem) Remove(string) error {
	return fsroot.ErrNoReplaceUnsupported
}

func (fileSystem *batchRenameTestFileSystem) RenameNoReplace(oldPath, newPath string) error {
	fileSystem.renameCalls++
	if oldPath == fileSystem.failRename {
		return fileSystem.failRenameErr
	}
	if _, exists := fileSystem.entries[newPath]; exists {
		return fsroot.ErrConflict
	}
	entry, exists := fileSystem.entries[oldPath]
	if !exists {
		return fs.ErrNotExist
	}
	updates := make(map[string]fsroot.Entry)
	for currentPath, currentEntry := range fileSystem.entries {
		if currentPath != oldPath && !strings.HasPrefix(currentPath, oldPath+"/") {
			continue
		}
		updatedPath := newPath + strings.TrimPrefix(currentPath, oldPath)
		currentEntry.Path = updatedPath
		currentEntry.Name = path.Base(updatedPath)
		updates[updatedPath] = currentEntry
		delete(fileSystem.entries, currentPath)
	}
	if len(updates) == 0 {
		entry.Path = newPath
		entry.Name = path.Base(newPath)
		updates[newPath] = entry
	}
	for updatedPath, updatedEntry := range updates {
		fileSystem.entries[updatedPath] = updatedEntry
	}
	if fileSystem.afterRename != nil {
		return fileSystem.afterRename(oldPath, newPath)
	}
	return nil
}

type batchRenameTestIndexer struct {
	refreshes int
}

func (index *batchRenameTestIndexer) Status(context.Context) (store.FileIndexStatus, error) {
	return store.FileIndexStatus{}, nil
}

func (index *batchRenameTestIndexer) Query(context.Context, store.FileIndexQuery) ([]store.FileIndexRecord, error) {
	return nil, nil
}

func (index *batchRenameTestIndexer) Refresh(context.Context) {
	index.refreshes++
}

func newBatchRenameHandler(fileSystem *batchRenameTestFileSystem, index fileIndexer) http.Handler {
	return NewWithAgentAndIndex(fileSystem, nil, nil, nil, index, nil, coordination.New())
}

type batchRenameAuditStub struct {
	taskReader
	beginErr     error
	finishErr    error
	onBegin      func() error
	finished     int
	outcomes     []string
	finishCtxErr error
}

func (stub *batchRenameAuditStub) BeginManualAudit(context.Context, store.AuditRecord) (int64, error) {
	if stub.beginErr != nil {
		return 0, stub.beginErr
	}
	if stub.onBegin != nil {
		if err := stub.onBegin(); err != nil {
			return 0, err
		}
	}
	return 1, nil
}

func (stub *batchRenameAuditStub) FinishManualAudit(ctx context.Context, _ int64, outcome, _ string) error {
	stub.finished++
	stub.outcomes = append(stub.outcomes, outcome)
	stub.finishCtxErr = ctx.Err()
	return stub.finishErr
}

func (*batchRenameAuditStub) AuditHistoryPage(context.Context, int64, int) (store.AuditHistoryPage, error) {
	return store.AuditHistoryPage{}, nil
}

func TestFinishManualAuditSurvivesRequestCancellation(t *testing.T) {
	audit := &batchRenameAuditStub{}
	api := &API{audits: audit}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := finishManualAuditAfterWrite(api, ctx, 1, "succeeded", "操作完成。"); err != nil {
		t.Fatalf("finishManualAuditAfterWrite() error = %v", err)
	}
	if audit.finished != 1 || audit.finishCtxErr != nil {
		t.Fatalf("finish count = %d, finish context error = %v", audit.finished, audit.finishCtxErr)
	}
}

func TestBatchRenameRequiresAuditBeforeChangingPaths(t *testing.T) {
	for _, test := range []struct {
		name           string
		beginErr       error
		finishErr      error
		wantStatus     int
		wantRenamed    bool
		wantRenameCall int
		wantFinished   int
		wantAuditError string
	}{
		{name: "begin failure", beginErr: errors.New("audit unavailable"), wantStatus: http.StatusServiceUnavailable, wantAuditError: "操作记录暂不可用"},
		{name: "finish failure", finishErr: errors.New("audit completion unavailable"), wantStatus: http.StatusInternalServerError, wantRenamed: true, wantRenameCall: 1, wantFinished: 1, wantAuditError: "文件可能已变更，审计结果待核查"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fileSystem := newBatchRenameTestFileSystem(batchRenameTestEntry("old.txt", fsroot.KindFile))
			audit := &batchRenameAuditStub{beginErr: test.beginErr, finishErr: test.finishErr}
			handler := NewWithTaskStore(fileSystem, audit, nil, coordination.New())
			request := batchRenameInputFor(fileSystem.entries["old.txt"], "new.txt")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/files/batch-rename", strings.NewReader(batchRenameBody(t, request))).
				WithContext(context.Background()))
			if response.Code != test.wantStatus || !strings.Contains(response.Body.String(), test.wantAuditError) {
				t.Fatalf("response = %d %s", response.Code, response.Body.String())
			}
			if audit.finished != test.wantFinished {
				t.Fatalf("finished audit calls = %d, want %d", audit.finished, test.wantFinished)
			}
			_, renamed := fileSystem.entries["new.txt"]
			if renamed != test.wantRenamed || fileSystem.renameCalls != test.wantRenameCall {
				t.Fatalf("renamed = %v, rename calls = %d", renamed, fileSystem.renameCalls)
			}
		})
	}
}

func TestBatchRenameRejectsReplacementDuringAuditWrite(t *testing.T) {
	entry := batchRenameTestEntry("old.txt", fsroot.KindFile)
	fileSystem := newBatchRenameTestFileSystem(entry)
	audit := &batchRenameAuditStub{onBegin: func() error {
		entry := fileSystem.entries["old.txt"]
		entry.Inode++
		fileSystem.entries["old.txt"] = entry
		return nil
	}}
	handler := NewWithTaskStore(fileSystem, audit, nil, coordination.New())
	request := batchRenameBody(t, batchRenameInputFor(entry, "new.txt"))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/files/batch-rename", strings.NewReader(request)))
	if response.Code != http.StatusMultiStatus {
		t.Fatalf("rename status = %d, want partial result: %s", response.Code, response.Body.String())
	}
	if fileSystem.renameCalls != 0 || fileSystem.entries["old.txt"].Inode != entry.Inode+1 {
		t.Fatalf("changed entry was renamed: calls=%d entries=%+v", fileSystem.renameCalls, fileSystem.entries)
	}
	if _, exists := fileSystem.entries["new.txt"]; exists {
		t.Fatal("renamed target exists after version conflict")
	}
	if audit.finished != 1 {
		t.Fatalf("finished audits = %d, want 1", audit.finished)
	}
}

func TestBatchRenameReportsUncertainResultWhenTargetChangesAfterRename(t *testing.T) {
	entry := batchRenameTestEntry("old.txt", fsroot.KindFile)
	next := batchRenameTestEntry("next.txt", fsroot.KindFile)
	fileSystem := newBatchRenameTestFileSystem(entry, next)
	fileSystem.afterRename = func(_, target string) error {
		current := fileSystem.entries[target]
		current.Inode++
		fileSystem.entries[target] = current
		return nil
	}
	audit := &batchRenameAuditStub{}
	handler := NewWithTaskStore(fileSystem, audit, nil, coordination.New())
	response := performJSONRequest(handler, http.MethodPost, "/api/v1/files/batch-rename", batchRenameBody(t,
		batchRenameInputFor(entry, "new.txt"), batchRenameInputFor(next, "next-new.txt"),
	))
	if response.Code != http.StatusMultiStatus {
		t.Fatalf("rename status = %d, want 207: %s", response.Code, response.Body.String())
	}
	result := decodeBatchRenameResponse(t, response)
	if result.Status != "partial" || len(result.Items) != 2 || result.Items[0].Status != "failed" || !strings.Contains(result.Items[0].Message, "核对") || result.Items[1].Status != "not_started" {
		t.Fatalf("uncertain rename result = %#v", result)
	}
	if _, exists := fileSystem.entries["next-new.txt"]; exists {
		t.Fatal("later item was renamed after the result became uncertain")
	}
	if len(audit.outcomes) != 1 || audit.outcomes[0] != "needs_review" {
		t.Fatalf("audit outcomes = %v, want needs_review", audit.outcomes)
	}
}

func TestRenameReportsNeedsReviewWhenTargetChangesAfterRename(t *testing.T) {
	entry := batchRenameTestEntry("old.txt", fsroot.KindFile)
	fileSystem := newBatchRenameTestFileSystem(entry)
	fileSystem.afterRename = func(_, target string) error {
		current := fileSystem.entries[target]
		current.Inode++
		fileSystem.entries[target] = current
		return nil
	}
	audit := &batchRenameAuditStub{}
	index := &batchRenameTestIndexer{}
	handler := NewWithAgentAndIndexAndConversation(fileSystem, audit, nil, nil, index, nil, nil, coordination.New())
	body := `{"path":"old.txt","name":"new.txt","version":"` + versionForEntry(entry) + `"}`
	response := performJSONRequest(handler, http.MethodPost, "/api/v1/files/rename", body)
	if response.Code != http.StatusConflict {
		t.Fatalf("rename status = %d, want 409: %s", response.Code, response.Body.String())
	}
	if len(audit.outcomes) != 1 || audit.outcomes[0] != "needs_review" {
		t.Fatalf("audit outcomes = %v, want needs_review", audit.outcomes)
	}
	if index.refreshes != 1 {
		t.Fatalf("index refreshes = %d, want 1 after uncertain rename", index.refreshes)
	}
}

func TestBatchRenameStoresPerItemAuditPaths(t *testing.T) {
	metadata, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "audit.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	fileSystem := newBatchRenameTestFileSystem(
		batchRenameTestEntry("one.txt", fsroot.KindFile),
		batchRenameTestEntry("two.txt", fsroot.KindFile),
	)
	handler := NewWithTaskStore(fileSystem, metadata, nil, coordination.New())
	response := httptest.NewRecorder()
	request := batchRenameBody(t,
		batchRenameInputFor(fileSystem.entries["one.txt"], "one-new.txt"),
		batchRenameInputFor(fileSystem.entries["two.txt"], "two-new.txt"),
	)
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/files/batch-rename", strings.NewReader(request)))
	if response.Code != http.StatusOK {
		t.Fatalf("rename status = %d: %s", response.Code, response.Body.String())
	}
	page, err := metadata.AuditHistoryPage(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, event := range page.Events {
		if event.EventType == "manual_batch_rename" {
			if event.Outcome != "succeeded" || event.Message != "重命名完成。" {
				t.Fatalf("unexpected event outcome/message: %+v", event)
			}
			seen[event.SourcePath] = event.TargetPath
		}
	}
	if len(seen) != 2 || seen["one.txt"] != "one-new.txt" || seen["two.txt"] != "two-new.txt" {
		t.Fatalf("manual rename audit paths = %#v", seen)
	}
}

func batchRenameBody(t *testing.T, items ...batchRenameInput) string {
	t.Helper()
	data, err := json.Marshal(batchRenameRequest{Items: items})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func batchRenameInputFor(entry fsroot.Entry, name string) batchRenameInput {
	return batchRenameInput{Path: entry.Path, Version: versionForEntry(entry), Name: name}
}

func decodeBatchRenameResponse(t *testing.T, response *httptest.ResponseRecorder) batchRenameResponse {
	t.Helper()
	var result batchRenameResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestBatchRenamePreflightConflictsDoNotChangeAnyPath(t *testing.T) {
	for _, test := range []struct {
		name  string
		items func(fsroot.Entry, fsroot.Entry) []batchRenameInput
	}{
		{
			name: "stale version",
			items: func(first, second fsroot.Entry) []batchRenameInput {
				stale := batchRenameInputFor(second, "second-new.txt")
				stale.Version = "stale-version"
				return []batchRenameInput{batchRenameInputFor(first, "first-new.txt"), stale}
			},
		},
		{
			name: "target occupied by another source",
			items: func(first, second fsroot.Entry) []batchRenameInput {
				return []batchRenameInput{batchRenameInputFor(first, second.Name), batchRenameInputFor(second, "second-new.txt")}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			first := batchRenameTestEntry("first.txt", fsroot.KindFile)
			second := batchRenameTestEntry("second.txt", fsroot.KindFile)
			fileSystem := newBatchRenameTestFileSystem(first, second)
			response := performJSONRequest(newBatchRenameHandler(fileSystem, nil), http.MethodPost, "/api/v1/files/batch-rename", batchRenameBody(t, test.items(first, second)...))
			if response.Code != http.StatusConflict {
				t.Fatalf("preflight status = %d, want 409: %s", response.Code, response.Body.String())
			}
			if fileSystem.renameCalls != 0 || fileSystem.entries[first.Path].Path != first.Path || fileSystem.entries[second.Path].Path != second.Path {
				t.Fatalf("preflight changed paths: calls=%d entries=%v", fileSystem.renameCalls, fileSystem.entries)
			}
		})
	}
}

func TestBatchRenameRejectsDuplicatePathsAndTargets(t *testing.T) {
	first := batchRenameTestEntry("first.txt", fsroot.KindFile)
	second := batchRenameTestEntry("second.txt", fsroot.KindFile)
	for _, test := range []struct {
		name  string
		items []batchRenameInput
	}{
		{name: "duplicate source", items: []batchRenameInput{batchRenameInputFor(first, "first-new.txt"), batchRenameInputFor(first, "other.txt")}},
		{name: "duplicate target", items: []batchRenameInput{batchRenameInputFor(first, "shared.txt"), batchRenameInputFor(second, "shared.txt")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fileSystem := newBatchRenameTestFileSystem(first, second)
			response := performJSONRequest(newBatchRenameHandler(fileSystem, nil), http.MethodPost, "/api/v1/files/batch-rename", batchRenameBody(t, test.items...))
			if response.Code != http.StatusBadRequest || fileSystem.renameCalls != 0 {
				t.Fatalf("duplicate response = %d calls=%d: %s", response.Code, fileSystem.renameCalls, response.Body.String())
			}
		})
	}
}

func TestBatchRenameRejectsUnknownDuplicateAndOversizedJSON(t *testing.T) {
	entry := batchRenameTestEntry("first.txt", fsroot.KindFile)
	fileSystem := newBatchRenameTestFileSystem(entry)
	handler := newBatchRenameHandler(fileSystem, nil)
	version := versionForEntry(entry)
	for _, body := range []string{
		`{"items":[{"path":"first.txt","version":"` + version + `","name":"new.txt","unexpected":true}]}`,
		`{"items":[{"path":"first.txt","path":"first.txt","version":"` + version + `","name":"new.txt"}]}`,
		`{"items":[],"items":[]}`,
	} {
		response := performJSONRequest(handler, http.MethodPost, "/api/v1/files/batch-rename", body)
		if response.Code != http.StatusBadRequest {
			t.Errorf("invalid body status = %d, want 400: %s", response.Code, response.Body.String())
		}
	}
	response := performJSONRequest(handler, http.MethodPost, "/api/v1/files/batch-rename", strings.Repeat("x", maxBatchRenameBodyBytes+1))
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body status = %d, want 413", response.Code)
	}
	if fileSystem.renameCalls != 0 {
		t.Fatalf("invalid requests called RenameNoReplace %d times", fileSystem.renameCalls)
	}
}

func TestBatchRenameRejectsSymlinkedParentAndPermissionFailure(t *testing.T) {
	linkedParent := batchRenameTestEntry("linked", fsroot.KindDirectory)
	linkedParent.Symlink = true
	linkedParent.LinkIdentityAvailable = true
	child := batchRenameTestEntry("linked/child.txt", fsroot.KindFile)
	fileSystem := newBatchRenameTestFileSystem(linkedParent, child)
	response := performJSONRequest(newBatchRenameHandler(fileSystem, nil), http.MethodPost, "/api/v1/files/batch-rename", batchRenameBody(t, batchRenameInputFor(child, "new.txt")))
	if response.Code != http.StatusBadRequest || fileSystem.renameCalls != 0 {
		t.Fatalf("symlink parent response = %d calls=%d: %s", response.Code, fileSystem.renameCalls, response.Body.String())
	}

	first := batchRenameTestEntry("first.txt", fsroot.KindFile)
	permissionFS := newBatchRenameTestFileSystem(first)
	permissionFS.listErrorPath = ""
	permissionFS.listError = fs.ErrPermission
	response = performJSONRequest(newBatchRenameHandler(permissionFS, nil), http.MethodPost, "/api/v1/files/batch-rename", batchRenameBody(t, batchRenameInputFor(first, "new.txt")))
	if response.Code != http.StatusForbidden || permissionFS.renameCalls != 0 {
		t.Fatalf("permission preflight response = %d calls=%d: %s", response.Code, permissionFS.renameCalls, response.Body.String())
	}
}

func TestBatchRenameContinuesNoFurtherItemsAfterExecutionFailureAndRefreshesIndex(t *testing.T) {
	first := batchRenameTestEntry("first.txt", fsroot.KindFile)
	second := batchRenameTestEntry("second.txt", fsroot.KindFile)
	third := batchRenameTestEntry("third.txt", fsroot.KindFile)
	fileSystem := newBatchRenameTestFileSystem(first, second, third)
	fileSystem.failRename = second.Path
	fileSystem.failRenameErr = fs.ErrPermission
	index := &batchRenameTestIndexer{}
	handler := newBatchRenameHandler(fileSystem, index)
	response := performJSONRequest(handler, http.MethodPost, "/api/v1/files/batch-rename", batchRenameBody(t,
		batchRenameInputFor(first, "first-new.txt"),
		batchRenameInputFor(second, "second-new.txt"),
		batchRenameInputFor(third, "third-new.txt"),
	))
	if response.Code != http.StatusMultiStatus {
		t.Fatalf("partial status = %d, want 207: %s", response.Code, response.Body.String())
	}
	result := decodeBatchRenameResponse(t, response)
	if result.Status != "partial" || len(result.Items) != 3 {
		t.Fatalf("partial response = %#v", result)
	}
	if result.Items[0].Status != "renamed" || result.Items[0].TargetPath != "first-new.txt" || result.Items[0].Name != "first-new.txt" {
		t.Fatalf("first result = %#v", result.Items[0])
	}
	if result.Items[1].Status != "failed" || result.Items[1].Message == "" {
		t.Fatalf("failed result = %#v", result.Items[1])
	}
	if result.Items[2].Status != "not_started" || result.Items[2].TargetPath != "third-new.txt" {
		t.Fatalf("unstarted result = %#v", result.Items[2])
	}
	if _, exists := fileSystem.entries["first-new.txt"]; !exists {
		t.Fatal("first item was not renamed")
	}
	if _, exists := fileSystem.entries[second.Path]; !exists {
		t.Fatal("failed item source was changed")
	}
	if _, exists := fileSystem.entries[third.Path]; !exists {
		t.Fatal("not-started item source was changed")
	}
	if index.refreshes != 1 {
		t.Fatalf("index refreshes = %d, want 1 after HTTP 207", index.refreshes)
	}
}

func TestBatchRenameSupportsDirectoryAndFinalSymlinkEntries(t *testing.T) {
	directory := batchRenameTestEntry("folder", fsroot.KindDirectory)
	child := batchRenameTestEntry("folder/child.txt", fsroot.KindFile)
	link := batchRenameTestEntry("alias.txt", fsroot.KindFile)
	link.Symlink = true
	link.LinkStatus = fsroot.LinkStatusInternal
	link.LinkTarget = "folder/child.txt"
	link.IdentityAvailable = false
	link.LinkIdentityAvailable = true
	fileSystem := newBatchRenameTestFileSystem(directory, child, link)
	response := performJSONRequest(newBatchRenameHandler(fileSystem, nil), http.MethodPost, "/api/v1/files/batch-rename", batchRenameBody(t,
		batchRenameInputFor(directory, "renamed-folder"),
		batchRenameInputFor(link, "renamed-alias.txt"),
	))
	if response.Code != http.StatusOK {
		t.Fatalf("rename status = %d, want 200: %s", response.Code, response.Body.String())
	}
	result := decodeBatchRenameResponse(t, response)
	if result.Status != "completed" || result.Items[0].Status != "renamed" || result.Items[1].Status != "renamed" {
		t.Fatalf("rename response = %#v", result)
	}
	if _, exists := fileSystem.entries["renamed-folder/child.txt"]; !exists {
		t.Fatal("directory contents did not move with the renamed parent")
	}
	if movedLink, exists := fileSystem.entries["renamed-alias.txt"]; !exists || !movedLink.Symlink || movedLink.LinkTarget != link.LinkTarget {
		t.Fatalf("symlink entry was not renamed as a link: %#v", movedLink)
	}
}
