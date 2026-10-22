package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filesage/internal/coordination"
	"filesage/internal/fsroot"
	"filesage/internal/store"
)

type testFileIndexer struct {
	data     *store.Store
	state    store.FileIndexScanState
	queryErr error
}

type unavailableVolumeFileSystem struct {
	root *fsroot.Root
}

func (files unavailableVolumeFileSystem) List(relativePath string) ([]fsroot.Entry, error) {
	return files.root.List(relativePath)
}

func (files unavailableVolumeFileSystem) LstatEntry(relativePath string) (fsroot.Entry, error) {
	switch relativePath {
	case "volume":
		return fsroot.Entry{Name: "volume", Path: "volume", Kind: fsroot.KindDirectory, MountUnavailable: true, ReadOnly: true}, nil
	case "volume/old.txt":
		return fsroot.Entry{}, fsroot.ErrVolumeUnavailable
	default:
		return files.root.LstatEntry(relativePath)
	}
}

func (files unavailableVolumeFileSystem) OpenRead(relativePath string) (io.ReadCloser, fsroot.Entry, error) {
	return files.root.OpenRead(relativePath)
}

func (files unavailableVolumeFileSystem) Mkdir(relativePath string, perm fs.FileMode) error {
	return files.root.Mkdir(relativePath, perm)
}

func (files unavailableVolumeFileSystem) RenameNoReplace(oldPath, newPath string) error {
	return files.root.RenameNoReplace(oldPath, newPath)
}

// RemoveEntryIfUnchanged 委托给真实根目录的条件删除实现。
func (files unavailableVolumeFileSystem) RemoveEntryIfUnchanged(entry fsroot.Entry) error {
	return files.root.RemoveEntryIfUnchanged(entry)
}

func (index *testFileIndexer) Status(ctx context.Context) (store.FileIndexStatus, error) {
	status, err := index.data.FileIndexStatus(ctx)
	if err == nil && index.state != "" {
		status.State = index.state
	}
	return status, err
}

func (index *testFileIndexer) Query(ctx context.Context, query store.FileIndexQuery) ([]store.FileIndexRecord, error) {
	if index.queryErr != nil {
		return nil, index.queryErr
	}
	return index.data.QueryFileIndex(ctx, query)
}

func (index *testFileIndexer) Refresh(context.Context) {
	index.state = store.FileIndexStale
}

func TestListFilePageUsesStableRecursiveCursor(t *testing.T) {
	ctx := context.Background()
	managedRoot := t.TempDir()
	if err := os.Mkdir(filepath.Join(managedRoot, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a-needle.txt", "b-needle.txt"} {
		if err := os.WriteFile(filepath.Join(managedRoot, "sub", name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := fsroot.Open(managedRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	metadata, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	generation, err := metadata.BeginFileIndexScan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := metadata.UpsertFileIndexBatch(ctx, generation, []store.FileIndexRecord{
		fileIndexRecordForTest(t, root, "sub"),
		fileIndexRecordForTest(t, root, "sub/a-needle.txt"),
		fileIndexRecordForTest(t, root, "sub/b-needle.txt"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := metadata.CompleteFileIndexScan(ctx, generation); err != nil {
		t.Fatal(err)
	}

	index := &testFileIndexer{data: metadata, state: store.FileIndexStale}
	handler := NewWithAgentAndIndex(root, nil, nil, nil, index, nil, coordination.New())
	first := requestFilePage(t, handler, "q=needle&scope=recursive&limit=1")
	if first.Code != http.StatusOK {
		t.Fatalf("first page status = %d, body = %s", first.Code, first.Body.String())
	}
	firstPage := decodeListing(t, first)
	if len(firstPage.Entries) != 1 || firstPage.Entries[0].Path != "sub/a-needle.txt" || !firstPage.HasMore || firstPage.NextCursor == "" || firstPage.IndexState != string(store.FileIndexStale) {
		t.Fatalf("first page = %+v", firstPage)
	}

	if err := os.WriteFile(filepath.Join(managedRoot, "sub", "z-needle.txt"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	newGeneration, err := metadata.BeginFileIndexScan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	newRecords := []store.FileIndexRecord{
		fileIndexRecordForTest(t, root, "sub"),
		fileIndexRecordForTest(t, root, "sub/a-needle.txt"),
		fileIndexRecordForTest(t, root, "sub/b-needle.txt"),
		fileIndexRecordForTest(t, root, "sub/z-needle.txt"),
	}
	if err := metadata.UpsertFileIndexBatch(ctx, newGeneration, newRecords); err != nil {
		t.Fatal(err)
	}
	if err := metadata.CompleteFileIndexScan(ctx, newGeneration); err != nil {
		t.Fatal(err)
	}
	index.state = ""

	secondQuery := url.Values{
		"q":      {"needle"},
		"scope":  {"recursive"},
		"limit":  {"1"},
		"cursor": {firstPage.NextCursor},
	}
	second := requestFilePage(t, handler, secondQuery.Encode())
	if second.Code != http.StatusOK {
		t.Fatalf("second page status = %d, body = %s", second.Code, second.Body.String())
	}
	secondPage := decodeListing(t, second)
	if len(secondPage.Entries) != 1 || secondPage.Entries[0].Path != "sub/b-needle.txt" || secondPage.HasMore || secondPage.IndexState != string(store.FileIndexStale) {
		t.Fatalf("second page = %+v", secondPage)
	}

	changedQuery := url.Values{
		"q":      {"other"},
		"scope":  {"recursive"},
		"limit":  {"1"},
		"cursor": {firstPage.NextCursor},
	}
	invalid := requestFilePage(t, handler, changedQuery.Encode())
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("changed query status = %d, want %d", invalid.Code, http.StatusBadRequest)
	}
}

func TestRecursiveFilePageKeepsAvailableFilesWhenVolumeIsUnavailable(t *testing.T) {
	ctx := context.Background()
	managedRoot := t.TempDir()
	if err := os.Mkdir(filepath.Join(managedRoot, "volume"), 0o755); err != nil {
		t.Fatal(err)
	}
	for relativePath, contents := range map[string]string{
		"volume/old.txt": "old",
		"available.txt":  "available",
	} {
		if err := os.WriteFile(filepath.Join(managedRoot, filepath.FromSlash(relativePath)), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := fsroot.Open(managedRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	metadata, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	generation, err := metadata.BeginFileIndexScan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := metadata.UpsertFileIndexBatch(ctx, generation, []store.FileIndexRecord{
		fileIndexRecordForTest(t, root, "volume"),
		fileIndexRecordForTest(t, root, "volume/old.txt"),
		fileIndexRecordForTest(t, root, "available.txt"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := metadata.CompleteFileIndexScan(ctx, generation); err != nil {
		t.Fatal(err)
	}

	index := &testFileIndexer{data: metadata}
	handler := NewWithAgentAndIndex(unavailableVolumeFileSystem{root: root}, nil, nil, nil, index, nil, coordination.New())
	response := requestFilePage(t, handler, "scope=recursive")
	if response.Code != http.StatusOK {
		t.Fatalf("recursive page status = %d, body = %s", response.Code, response.Body.String())
	}
	page := decodeListing(t, response)
	foundUnavailable, foundAvailable := false, false
	for _, entry := range page.Entries {
		switch entry.Path {
		case "volume":
			foundUnavailable = entry.MountUnavailable && entry.ReadOnly
		case "available.txt":
			foundAvailable = true
		case "volume/old.txt":
			t.Fatal("recursive listing exposed a stale child inside the unavailable volume")
		}
	}
	if !foundUnavailable || !foundAvailable || page.IndexState != string(store.FileIndexStale) {
		t.Fatalf("recursive page = %+v; want unavailable volume, available sibling, and stale index state", page)
	}
}

// TestListFilePageReportsUnavailableCaptureMetadata 验证拍摄元数据未就绪时返回明确提示。
func TestListFilePageReportsUnavailableCaptureMetadata(t *testing.T) {
	ctx := context.Background()
	managedRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(managedRoot, "photo.jpg"), []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := fsroot.Open(managedRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	metadata, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	generation, err := metadata.BeginFileIndexScan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := metadata.UpsertFileIndexBatch(ctx, generation, []store.FileIndexRecord{
		fileIndexRecordForTest(t, root, "photo.jpg"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := metadata.CompleteFileIndexScan(ctx, generation); err != nil {
		t.Fatal(err)
	}

	index := &testFileIndexer{data: metadata, queryErr: store.ErrFileIndexCaptureMetadataUnavailable}
	handler := NewWithAgentAndIndex(root, nil, nil, nil, index, nil, coordination.New())
	response := requestFilePage(t, handler, "capturedFrom=2024-08-09T00%3A00%3A00Z")
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "需要完成一次新") {
		t.Fatalf("capture metadata response = (%d, %s)", response.Code, response.Body.String())
	}
}

func TestListFilePageUsesLastCompleteGenerationWhileRefreshing(t *testing.T) {
	ctx := context.Background()
	managedRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(managedRoot, "needle.txt"), []byte("current"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := fsroot.Open(managedRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	metadata, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	generation, err := metadata.BeginFileIndexScan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := metadata.UpsertFileIndexBatch(ctx, generation, []store.FileIndexRecord{
		fileIndexRecordForTest(t, root, "needle.txt"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := metadata.CompleteFileIndexScan(ctx, generation); err != nil {
		t.Fatal(err)
	}

	index := &testFileIndexer{data: metadata, state: store.FileIndexStale}
	handler := NewWithAgentAndIndex(root, nil, nil, nil, index, nil, coordination.New())
	response := requestFilePage(t, handler, "q=needle&scope=recursive")
	if response.Code != http.StatusOK {
		t.Fatalf("stale index status = %d, body = %s", response.Code, response.Body.String())
	}
	page := decodeListing(t, response)
	if len(page.Entries) != 1 || page.Entries[0].Path != "needle.txt" || page.IndexState != string(store.FileIndexStale) {
		t.Fatalf("stale index page = %+v", page)
	}
}

func TestFilePageRejectsInvalidFilters(t *testing.T) {
	root, err := fsroot.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	handler := NewWithAgentAndIndex(root, nil, nil, nil, nil, nil, coordination.New())

	cases := []struct {
		name  string
		query url.Values
	}{
		{"duplicate type", url.Values{"type": {"image", "video"}}},
		{"unknown type", url.Values{"type": {"executable"}}},
		{"duplicate size", url.Values{"minSize": {"1", "2"}}},
		{"negative size", url.Values{"minSize": {"-1"}}},
		{"malformed size", url.Values{"maxSize": {"1.5"}}},
		{"reversed size", url.Values{"minSize": {"2"}, "maxSize": {"1"}}},
		{"malformed modified date", url.Values{"modifiedFrom": {"yesterday"}}},
		{"duplicate captured date", url.Values{"capturedBefore": {"2025-01-01T00:00:00Z", "2026-01-01T00:00:00Z"}}},
		{"reversed modified range", url.Values{"modifiedFrom": {"2025-01-02T00:00:00Z"}, "modifiedBefore": {"2025-01-01T00:00:00Z"}}},
		{"reversed captured range", url.Values{"capturedFrom": {"2025-01-02T00:00:00Z"}, "capturedBefore": {"2025-01-01T00:00:00Z"}}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			response := requestFilePage(t, handler, testCase.query.Encode())
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s; want %d", response.Code, response.Body.String(), http.StatusBadRequest)
			}
		})
	}
}

func TestFilePageCursorBindsEveryFilter(t *testing.T) {
	root, err := fsroot.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	handler := NewWithAgentAndIndex(root, nil, nil, nil, nil, nil, coordination.New())
	base := url.Values{
		"scope":          {"current"},
		"limit":          {"20"},
		"type":           {"document"},
		"minSize":        {"1"},
		"maxSize":        {"100"},
		"modifiedFrom":   {"2025-01-01T00:00:00Z"},
		"modifiedBefore": {"2025-02-01T00:00:00Z"},
		"capturedFrom":   {"2025-01-01T00:00:00Z"},
		"capturedBefore": {"2025-02-01T00:00:00Z"},
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/files?"+base.Encode(), nil)
	parsed, err := parseFilePageQuery(request, "")
	if err != nil {
		t.Fatal(err)
	}
	filters := []struct {
		name  string
		value string
	}{
		{"type", "image"},
		{"minSize", "2"},
		{"maxSize", "99"},
		{"modifiedFrom", "2025-01-02T00:00:00Z"},
		{"modifiedBefore", "2025-01-31T00:00:00Z"},
		{"capturedFrom", "2025-01-02T00:00:00Z"},
		{"capturedBefore", "2025-01-31T00:00:00Z"},
	}
	for _, filter := range filters {
		t.Run(filter.name, func(t *testing.T) {
			query := base.Clone()
			query.Set(filter.name, filter.value)
			query.Set("cursor", encodeFileCursor(fileCursor{
				Offset: 1, QueryHash: fileQueryHash(parsed),
			}))
			response := requestFilePage(t, handler, query.Encode())
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "游标") {
				t.Fatalf("changed %s status=%d body=%s; want cursor mismatch", filter.name, response.Code, response.Body.String())
			}
		})
	}
}

func TestFilePageFiltersUseFilesystemFallbackAndRequireIndexForCaptureDate(t *testing.T) {
	managedRoot := t.TempDir()
	if err := os.Mkdir(filepath.Join(managedRoot, "folder"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, contents := range map[string]string{"photo.jpg": "abc", "note.txt": "abcdef", "unknown.bin": "?"} {
		if err := os.WriteFile(filepath.Join(managedRoot, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	modified := time.Date(2025, 3, 4, 5, 6, 7, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(managedRoot, "photo.jpg"), modified, modified); err != nil {
		t.Fatal(err)
	}
	root, err := fsroot.Open(managedRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	handler := NewWithAgentAndIndex(root, nil, nil, nil, nil, nil, coordination.New())

	filtered := requestFilePage(t, handler, url.Values{
		"type":           {"image"},
		"minSize":        {"3"},
		"maxSize":        {"3"},
		"modifiedFrom":   {modified.Format(time.RFC3339)},
		"modifiedBefore": {modified.Add(time.Second).Format(time.RFC3339)},
	}.Encode())
	if filtered.Code != http.StatusOK {
		t.Fatalf("filesystem fallback status=%d body=%s", filtered.Code, filtered.Body.String())
	}
	page := decodeListing(t, filtered)
	if len(page.Entries) != 1 || page.Entries[0].Name != "photo.jpg" {
		t.Fatalf("filesystem fallback entries = %+v", page.Entries)
	}

	directories := requestFilePage(t, handler, "type=directory")
	if directories.Code != http.StatusOK {
		t.Fatalf("directory fallback status=%d body=%s", directories.Code, directories.Body.String())
	}
	directoryPage := decodeListing(t, directories)
	if len(directoryPage.Entries) != 1 || directoryPage.Entries[0].Name != "folder" {
		t.Fatalf("directory fallback entries = %+v", directoryPage.Entries)
	}

	zeroSize := requestFilePage(t, handler, "minSize=0&maxSize=0")
	if zeroSize.Code != http.StatusOK || len(decodeListing(t, zeroSize).Entries) != 0 {
		t.Fatalf("size filter must exclude directories: status=%d body=%s", zeroSize.Code, zeroSize.Body.String())
	}

	captured := requestFilePage(t, handler, "capturedFrom=2025-01-01T00%3A00%3A00Z")
	if captured.Code != http.StatusServiceUnavailable || !strings.Contains(captured.Body.String(), "索引") {
		t.Fatalf("capture filter without index status=%d body=%s", captured.Code, captured.Body.String())
	}
}

func TestFilePageFiltersUsePreviousCompleteGenerationDuringBuild(t *testing.T) {
	ctx := context.Background()
	managedRoot := t.TempDir()
	if err := os.Mkdir(filepath.Join(managedRoot, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	imagePath := filepath.Join(managedRoot, "docs", "photo.jpg")
	if err := os.WriteFile(imagePath, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := fsroot.Open(managedRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	metadata, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()

	modified := time.Date(2025, 3, 4, 5, 6, 7, 0, time.UTC)
	if err := os.Chtimes(imagePath, modified, modified); err != nil {
		t.Fatal(err)
	}
	capturedAt := time.Date(2024, 8, 9, 10, 11, 12, 0, time.UTC)
	imageRecord := fileIndexRecordForTest(t, root, "docs/photo.jpg")
	imageRecord.MediaType = "image/jpeg"
	imageRecord.CapturedAt = &capturedAt
	firstGeneration, err := metadata.BeginFileIndexScan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := metadata.UpsertFileIndexBatch(ctx, firstGeneration, []store.FileIndexRecord{
		fileIndexRecordForTest(t, root, "docs"), imageRecord,
	}); err != nil {
		t.Fatal(err)
	}
	if err := metadata.CompleteFileIndexScan(ctx, firstGeneration); err != nil {
		t.Fatal(err)
	}

	secondGeneration, err := metadata.BeginFileIndexScan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := metadata.UpsertFileIndexBatch(ctx, secondGeneration, []store.FileIndexRecord{
		fileIndexRecordForTest(t, root, "docs"),
	}); err != nil {
		t.Fatal(err)
	}
	index := &testFileIndexer{data: metadata}
	handler := NewWithAgentAndIndex(root, nil, nil, nil, index, nil, coordination.New())
	response := requestFilePage(t, handler, url.Values{
		"scope":          {"recursive"},
		"type":           {"image"},
		"minSize":        {"3"},
		"maxSize":        {"3"},
		"modifiedFrom":   {modified.Format(time.RFC3339Nano)},
		"modifiedBefore": {modified.Add(time.Nanosecond).Format(time.RFC3339Nano)},
		"capturedFrom":   {capturedAt.Format(time.RFC3339Nano)},
		"capturedBefore": {capturedAt.Add(time.Nanosecond).Format(time.RFC3339Nano)},
	}.Encode())
	if response.Code != http.StatusOK {
		t.Fatalf("filter against previous generation status=%d body=%s", response.Code, response.Body.String())
	}
	page := decodeListing(t, response)
	if page.IndexState != string(store.FileIndexBuilding) || len(page.Entries) != 1 || page.Entries[0].Path != "docs/photo.jpg" {
		t.Fatalf("filter while replacement is building = %+v", page)
	}

	// 新代次未完成时，拍摄日期仍按上一完整代次查询。
	if err := metadata.FailFileIndexScan(ctx, secondGeneration, "test cleanup"); err != nil {
		t.Fatal(err)
	}
}

func TestPhotoTimelineSortsAndReturnsCaptureMetadata(t *testing.T) {
	ctx := context.Background()
	managedRoot := t.TempDir()
	for _, name := range []string{"earlier.jpg", "later.jpg", "unknown.jpg"} {
		if err := os.WriteFile(filepath.Join(managedRoot, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := fsroot.Open(managedRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	metadata, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()

	earlierAt := time.Date(2024, 5, 1, 9, 0, 0, 0, time.UTC)
	laterAt := time.Date(2025, 6, 2, 10, 0, 0, 0, time.UTC)
	records := make([]store.FileIndexRecord, 0, 3)
	for _, item := range []struct {
		name       string
		capturedAt *time.Time
	}{{"earlier.jpg", &earlierAt}, {"later.jpg", &laterAt}, {"unknown.jpg", nil}} {
		record := fileIndexRecordForTest(t, root, item.name)
		record.MediaType = "image/jpeg"
		record.CapturedAt = item.capturedAt
		records = append(records, record)
	}
	generation, err := metadata.BeginFileIndexScan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := metadata.UpsertFileIndexBatch(ctx, generation, records); err != nil {
		t.Fatal(err)
	}
	if err := metadata.CompleteFileIndexScan(ctx, generation); err != nil {
		t.Fatal(err)
	}

	handler := NewWithAgentAndIndex(root, nil, nil, nil, &testFileIndexer{data: metadata}, nil, coordination.New())
	response := requestFilePage(t, handler, "scope=recursive&type=image&sort=captured-desc")
	if response.Code != http.StatusOK {
		t.Fatalf("photo query status = %d, body = %s", response.Code, response.Body.String())
	}
	page := decodeListing(t, response)
	if len(page.Entries) != 3 || page.Entries[0].Name != "later.jpg" || page.Entries[1].Name != "earlier.jpg" || page.Entries[2].Name != "unknown.jpg" {
		t.Fatalf("photo timeline order = %+v", page.Entries)
	}
	if page.Entries[0].CapturedAt == nil || *page.Entries[0].CapturedAt != laterAt.Format(time.RFC3339Nano) || page.Entries[2].CapturedAt != nil {
		t.Fatalf("capture metadata = %+v", page.Entries)
	}
}

func TestListFilePageSkipsDeletedIndexRowsWithoutReturningEmptyCursorPage(t *testing.T) {
	ctx := context.Background()
	managedRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(managedRoot, "needle-100.txt"), []byte("current"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := fsroot.Open(managedRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	metadata, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	generation, err := metadata.BeginFileIndexScan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	records := make([]store.FileIndexRecord, 0, 101)
	for index := 0; index <= 100; index++ {
		name := fmt.Sprintf("needle-%03d.txt", index)
		if index == 100 {
			records = append(records, fileIndexRecordForTest(t, root, name))
			continue
		}
		records = append(records, store.FileIndexRecord{
			RelativePath: name, ParentPath: "", Name: name, Kind: "file",
			SizeBytes: 7, ModifiedAt: now, IndexedAt: now,
		})
	}
	if err := metadata.UpsertFileIndexBatch(ctx, generation, records); err != nil {
		t.Fatal(err)
	}
	if err := metadata.CompleteFileIndexScan(ctx, generation); err != nil {
		t.Fatal(err)
	}

	handler := NewWithAgentAndIndex(root, nil, nil, nil, &testFileIndexer{data: metadata}, nil, coordination.New())
	response := requestFilePage(t, handler, "q=needle&scope=recursive&limit=100")
	if response.Code != http.StatusOK {
		t.Fatalf("page status = %d, body = %s", response.Code, response.Body.String())
	}
	page := decodeListing(t, response)
	if len(page.Entries) != 1 || page.Entries[0].Path != "needle-100.txt" || page.HasMore || page.NextCursor != "" || page.IndexState != "stale" {
		t.Fatalf("page after deleted indexed rows = %+v", page)
	}
}

func TestListFilePagesAdvanceCursorAcrossDeletedIndexRows(t *testing.T) {
	ctx := context.Background()
	managedRoot := t.TempDir()
	for _, name := range []string{"needle-003.txt", "needle-004.txt"} {
		if err := os.WriteFile(filepath.Join(managedRoot, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := fsroot.Open(managedRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	metadata, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	generation, err := metadata.BeginFileIndexScan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	records := make([]store.FileIndexRecord, 0, 5)
	for index := 0; index < 5; index++ {
		name := fmt.Sprintf("needle-%03d.txt", index)
		if index >= 3 {
			records = append(records, fileIndexRecordForTest(t, root, name))
			continue
		}
		records = append(records, store.FileIndexRecord{
			RelativePath: name, ParentPath: "", Name: name, Kind: "file",
			SizeBytes: 1, ModifiedAt: now, IndexedAt: now,
		})
	}
	if err := metadata.UpsertFileIndexBatch(ctx, generation, records); err != nil {
		t.Fatal(err)
	}
	if err := metadata.CompleteFileIndexScan(ctx, generation); err != nil {
		t.Fatal(err)
	}
	handler := NewWithAgentAndIndex(root, nil, nil, nil, &testFileIndexer{data: metadata}, nil, coordination.New())
	first := requestFilePage(t, handler, "q=needle&scope=recursive&limit=1")
	if first.Code != http.StatusOK {
		t.Fatalf("first page status = %d, body = %s", first.Code, first.Body.String())
	}
	firstPage := decodeListing(t, first)
	if len(firstPage.Entries) != 1 || firstPage.Entries[0].Path != "needle-003.txt" || !firstPage.HasMore || firstPage.NextCursor == "" || firstPage.IndexState != string(store.FileIndexStale) {
		t.Fatalf("first page after deleted index rows = %+v", firstPage)
	}
	secondQuery := url.Values{"q": {"needle"}, "scope": {"recursive"}, "limit": {"1"}, "cursor": {firstPage.NextCursor}}
	second := requestFilePage(t, handler, secondQuery.Encode())
	if second.Code != http.StatusOK {
		t.Fatalf("second page status = %d, body = %s", second.Code, second.Body.String())
	}
	secondPage := decodeListing(t, second)
	if len(secondPage.Entries) != 1 || secondPage.Entries[0].Path != "needle-004.txt" || secondPage.HasMore || secondPage.IndexState != string(store.FileIndexStale) {
		t.Fatalf("second page after deleted index rows = %+v", secondPage)
	}
}

func fileIndexRecordForTest(t *testing.T, root *fsroot.Root, relativePath string) store.FileIndexRecord {
	t.Helper()
	entry, err := root.LstatEntry(relativePath)
	if err != nil {
		t.Fatalf("inspect %q: %v", relativePath, err)
	}
	kind := string(entry.Kind)
	if entry.Symlink {
		kind = "symlink"
	}
	record := store.FileIndexRecord{
		RelativePath: relativePath, ParentPath: filepath.ToSlash(filepath.Dir(relativePath)),
		Name: filepath.Base(relativePath), Kind: kind, SizeBytes: entry.Size,
		ModifiedAt: entry.ModifiedAt, Mode: uint32(entry.Mode), IndexedAt: time.Now().UTC(),
	}
	if record.ParentPath == "." {
		record.ParentPath = ""
	}
	if entry.IdentityAvailable {
		device, inode, links := entry.Device, entry.Inode, entry.Links
		record.Device, record.Inode, record.HardLinkCount = &device, &inode, &links
	}
	return record
}

func requestFilePage(t *testing.T, handler http.Handler, query string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/files?"+query, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func decodeListing(t *testing.T, response *httptest.ResponseRecorder) listingResponse {
	t.Helper()
	var listing listingResponse
	if err := json.Unmarshal(response.Body.Bytes(), &listing); err != nil {
		t.Fatalf("decode listing: %v", err)
	}
	return listing
}
