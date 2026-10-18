//go:build linux

package httpapi

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"

	"filesage/internal/coordination"
	"filesage/internal/fsroot"
	"filesage/internal/store"
)

func TestPatchFileMetadataReturnsOwnershipAndAuditsChanges(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "note.txt"), []byte("note"), 0o644); err != nil {
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
	handler := NewWithTaskStore(root, metadata, nil, coordination.New())

	listing := performRequest(handler, http.MethodGet, "/api/v1/files", nil)
	if listing.Code != http.StatusOK {
		t.Fatalf("list status = %d: %s", listing.Code, listing.Body.String())
	}
	var page listingResponse
	if err := json.NewDecoder(listing.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	var listed fileEntry
	for _, entry := range page.Entries {
		if entry.Path == "note.txt" {
			listed = entry
		}
	}
	if listed.Version == "" || !listed.OwnershipAvailable {
		t.Fatalf("listed ownership or version unavailable: %+v", listed)
	}

	response := performJSONRequest(handler, http.MethodPatch, "/api/v1/files/metadata", `{"path":"note.txt","version":"`+listed.Version+`","mode":384}`)
	if response.Code != http.StatusOK {
		t.Fatalf("permission update status = %d: %s", response.Code, response.Body.String())
	}
	var updated metadataPatchResponse
	if err := json.NewDecoder(response.Body).Decode(&updated); err != nil {
		t.Fatal(err)
	}
	if updated.Entry.Mode != 384 || updated.Entry.OwnerUID != listed.OwnerUID || updated.Entry.GroupGID != listed.GroupGID || !updated.Entry.OwnershipAvailable || updated.Entry.Version == listed.Version {
		t.Fatalf("permission update response = %+v", updated.Entry)
	}
	if info, err := os.Stat(filepath.Join(rootPath, "note.txt")); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %04o, want 0600", info.Mode().Perm())
	}

	stale := performJSONRequest(handler, http.MethodPatch, "/api/v1/files/metadata", `{"path":"note.txt","version":"`+listed.Version+`","mode":420}`)
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale version status = %d, want 409: %s", stale.Code, stale.Body.String())
	}

	ownership := performJSONRequest(handler, http.MethodPatch, "/api/v1/files/metadata", `{"path":"note.txt","version":"`+updated.Entry.Version+`","ownerUid":`+strconv.FormatUint(uint64(listed.OwnerUID), 10)+`}`)
	if ownership.Code != http.StatusOK {
		t.Fatalf("ownership update status = %d: %s", ownership.Code, ownership.Body.String())
	}
	var ownershipResult metadataPatchResponse
	if err := json.NewDecoder(ownership.Body).Decode(&ownershipResult); err != nil {
		t.Fatal(err)
	}
	if !ownershipResult.Entry.OwnershipAvailable || ownershipResult.Entry.OwnerUID != listed.OwnerUID || ownershipResult.Entry.GroupGID != listed.GroupGID {
		t.Fatalf("ownership update response = %+v", ownershipResult.Entry)
	}
	group := performJSONRequest(handler, http.MethodPatch, "/api/v1/files/metadata", `{"path":"note.txt","version":"`+ownershipResult.Entry.Version+`","groupGid":`+strconv.FormatUint(uint64(listed.GroupGID), 10)+`}`)
	if group.Code != http.StatusOK {
		t.Fatalf("group update status = %d: %s", group.Code, group.Body.String())
	}

	history, err := metadata.AuditHistoryPage(context.Background(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Events) != 3 {
		t.Fatalf("audit events = %+v", history.Events)
	}
	seen := make(map[string]store.AuditRecord)
	for _, event := range history.Events {
		seen[event.EventType] = event
	}
	permissionAudit := seen["manual_update_permissions"]
	if permissionAudit.Outcome != "succeeded" || permissionAudit.SourcePath != "note.txt" || !strings.Contains(permissionAudit.Message, "644 → 600") {
		t.Fatalf("permission audit = %+v", permissionAudit)
	}
	ownershipAudit := seen["manual_update_ownership"]
	if ownershipAudit.Outcome != "succeeded" || !strings.Contains(ownershipAudit.Message, "→") || !strings.Contains(ownershipAudit.Message, "mode:") {
		t.Fatalf("ownership audit = %+v", ownershipAudit)
	}
}

func TestPatchFileMetadataRejectsMalformedOrUnsafeRequests(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "note.txt"), []byte("note"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("note.txt", filepath.Join(rootPath, "alias.txt")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(rootPath, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	handler := New(root, nil)
	version := versionFromListing(t, handler, "note.txt")
	invalidBodies := []string{
		`{"path":"note.txt","version":"` + version + `","mode":512}`,
		`{"path":"note.txt","version":"` + version + `","mode":-1}`,
		`{"path":"note.txt","version":"` + version + `","mode":1.5}`,
		`{"path":"note.txt","version":"` + version + `","mode":384,"ownerUid":1}`,
		`{"path":"note.txt","version":"` + version + `"}`,
		`{"path":"note.txt","version":"` + version + `","ownerUid":null}`,
		`{"path":"note.txt","path":"alias.txt","version":"` + version + `","mode":384}`,
		`{"path":"note.txt","version":"` + version + `","mode":384,"extra":true}`,
	}
	for _, body := range invalidBodies {
		response := performJSONRequest(handler, http.MethodPatch, "/api/v1/files/metadata", body)
		if response.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400: %s", body, response.Code, response.Body.String())
		}
	}
	for _, name := range []string{"alias.txt", "pipe"} {
		entryVersion := versionFromListing(t, handler, name)
		response := performJSONRequest(handler, http.MethodPatch, "/api/v1/files/metadata", `{"path":"`+name+`","version":"`+entryVersion+`","mode":384}`)
		if response.Code != http.StatusBadRequest {
			t.Errorf("%s update status = %d, want 400: %s", name, response.Code, response.Body.String())
		}
	}
}

func TestEntryVersionIncludesOwnerAndGroup(t *testing.T) {
	entry := fsroot.Entry{Name: "note.txt", Path: "note.txt", Kind: fsroot.KindFile, OwnershipAvailable: true}
	first := versionForEntry(entry)
	entry.OwnerUID++
	if versionForEntry(entry) == first {
		t.Fatal("version did not change when owner UID changed")
	}
	entry.OwnerUID--
	entry.GroupGID++
	if versionForEntry(entry) == first {
		t.Fatal("version did not change when group GID changed")
	}
}

func TestConcurrentMetadataUpdatesOnHardLinksSerializeByInode(t *testing.T) {
	rootPath := t.TempDir()
	firstPath, aliasPath := filepath.Join(rootPath, "first.txt"), filepath.Join(rootPath, "alias.txt")
	if err := os.WriteFile(firstPath, []byte("note"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(firstPath, aliasPath); err != nil {
		t.Fatal(err)
	}
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	first, err := root.LstatEntry("first.txt")
	if err != nil {
		t.Fatal(err)
	}
	alias, err := root.LstatEntry("alias.txt")
	if err != nil {
		t.Fatal(err)
	}
	if first.Device != alias.Device || first.Inode != alias.Inode || !first.IdentityAvailable {
		t.Fatalf("hard links do not share an available identity: first=%+v alias=%+v", first, alias)
	}

	filesystem := &metadataRaceFileSystem{Root: root, bothSnapshotted: make(chan struct{})}
	metadata, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "audit.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	handler := NewWithTaskStore(filesystem, metadata, nil, coordination.New())
	makeRequest := func(entry fsroot.Entry) string {
		return `{"path":"` + entry.Path + `","version":"` + versionForEntry(entry) + `","mode":384}`
	}
	responses := make(chan *httptest.ResponseRecorder, 2)
	var workers sync.WaitGroup
	for _, entry := range []fsroot.Entry{first, alias} {
		workers.Add(1)
		go func(body string) {
			defer workers.Done()
			responses <- performJSONRequest(handler, http.MethodPatch, "/api/v1/files/metadata", body)
		}(makeRequest(entry))
	}
	workers.Wait()
	close(responses)
	successes, conflicts := 0, 0
	for response := range responses {
		switch response.Code {
		case http.StatusOK:
			successes++
		case http.StatusConflict:
			conflicts++
		default:
			t.Errorf("metadata response = %d: %s", response.Code, response.Body.String())
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes/conflicts = %d/%d, want 1/1", successes, conflicts)
	}
	if info, err := os.Stat(firstPath); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("hard-link mode = %04o, want 0600", info.Mode().Perm())
	}

	history, err := metadata.AuditHistoryPage(context.Background(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Events) != 1 || history.Events[0].EventType != "manual_update_permissions" || history.Events[0].Outcome != "succeeded" || !strings.Contains(history.Events[0].Message, "644 → 600") {
		t.Fatalf("audit history = %+v; want one successful, accurate event", history.Events)
	}
}

func TestPatchFileMetadataAuditsPermissionAndUncertainFailures(t *testing.T) {
	for _, test := range []struct {
		name       string
		injected   error
		status     int
		outcome    string
		messageHas string
	}{
		{name: "permission denied", injected: fs.ErrPermission, status: http.StatusForbidden, outcome: "failed", messageHas: "644 → 644"},
		{name: "result needs review", injected: fsroot.ErrMetadataNeedsReview, status: http.StatusConflict, outcome: "needs_review", messageHas: "新值未知"},
	} {
		t.Run(test.name, func(t *testing.T) {
			rootPath := t.TempDir()
			if err := os.WriteFile(filepath.Join(rootPath, "note.txt"), []byte("note"), 0o644); err != nil {
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
			filesystem := &metadataFailureFileSystem{Root: root, updateErr: test.injected}
			handler := NewWithTaskStore(filesystem, metadata, nil, coordination.New())
			version := versionFromListing(t, handler, "note.txt")
			response := performJSONRequest(handler, http.MethodPatch, "/api/v1/files/metadata", `{"path":"note.txt","version":"`+version+`","mode":384}`)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.status, response.Body.String())
			}
			history, err := metadata.AuditHistoryPage(context.Background(), 0, 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(history.Events) != 1 {
				t.Fatalf("audit events = %+v", history.Events)
			}
			event := history.Events[0]
			if event.EventType != "manual_update_permissions" || event.Outcome != test.outcome || event.SourcePath != "note.txt" || !strings.Contains(event.Message, test.messageHas) {
				t.Fatalf("audit event = %+v", event)
			}
		})
	}
}

type metadataRaceFileSystem struct {
	*fsroot.Root
	mu              sync.Mutex
	initial         int
	bothSnapshotted chan struct{}
}

type metadataFailureFileSystem struct {
	*fsroot.Root
	updateErr error
}

func (filesystem *metadataFailureFileSystem) UpdateMetadata(string, fsroot.Fingerprint, fsroot.MetadataUpdate) (fsroot.Entry, error) {
	return fsroot.Entry{}, filesystem.updateErr
}

func (filesystem *metadataRaceFileSystem) LstatEntry(relativePath string) (fsroot.Entry, error) {
	entry, err := filesystem.Root.LstatEntry(relativePath)
	filesystem.mu.Lock()
	filesystem.initial++
	wait := filesystem.initial <= 2
	if filesystem.initial == 2 {
		close(filesystem.bothSnapshotted)
	}
	filesystem.mu.Unlock()
	if wait {
		<-filesystem.bothSnapshotted
	}
	return entry, err
}
