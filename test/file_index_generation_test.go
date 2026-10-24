package test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"filesage/internal/store"
)

func TestFileIndexGenerationMigrationPreservesLegacyRows(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "legacy.db")
	rawDB, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{
		"CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at_ns INTEGER NOT NULL)",
		"INSERT INTO schema_migrations (version, applied_at_ns) VALUES (1, 1), (2, 2), (3, 3), (4, 4)",
		"CREATE TABLE tasks (id TEXT PRIMARY KEY)",
		"CREATE TABLE plans (id TEXT PRIMARY KEY)",
		"CREATE TABLE operations (id TEXT PRIMARY KEY, target_state_json TEXT NOT NULL DEFAULT '{}')",
		"CREATE TABLE file_index (relative_path TEXT PRIMARY KEY, parent_path TEXT NOT NULL, name TEXT NOT NULL, kind TEXT NOT NULL, size_bytes INTEGER NOT NULL, modified_at_ns INTEGER NOT NULL, mode INTEGER NOT NULL, device INTEGER, inode INTEGER, hard_link_count INTEGER, media_type TEXT, captured_at_ns INTEGER, content_sha256 TEXT, indexed_at_ns INTEGER NOT NULL)",
		"INSERT INTO file_index (relative_path, parent_path, name, kind, size_bytes, modified_at_ns, mode, indexed_at_ns) VALUES ('old/report.pdf', 'old', 'report.pdf', 'file', 23, 77, 416, 88)",
	}
	for _, statement := range statements {
		if _, err := rawDB.ExecContext(ctx, statement); err != nil {
			t.Fatalf("prepare v4 database: %v", err)
		}
	}
	if err := rawDB.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatalf("upgrade v4 database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if version, err := db.SchemaVersion(ctx); err != nil || version != 31 {
		t.Fatalf("SchemaVersion() = (%d, %v), want (31, nil)", version, err)
	}
	record, err := db.FileIndex(ctx, "old/report.pdf")
	if err != nil {
		t.Fatalf("read legacy file index row: %v", err)
	}
	if record.ParentPath != "old" || record.Name != "report.pdf" || record.SizeBytes != 23 || record.Mode != 416 {
		t.Fatalf("legacy file index row changed during migration: %#v", record)
	}
	status, err := db.FileIndexStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.ActiveGeneration != 0 || status.State != store.FileIndexNotStarted {
		t.Fatalf("legacy rows were incorrectly published as a complete generation: %#v", status)
	}
}

func TestFileIndexGenerationsQueryAndRetainStablePages(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "index.db")
	db, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	initial, err := db.FileIndexStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if initial.State != store.FileIndexNotStarted || initial.ActiveGeneration != 0 || initial.StartedAt != nil {
		t.Fatalf("initial status = %#v", initial)
	}

	generation, err := db.BeginFileIndexScan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.BeginFileIndexScan(ctx); !errors.Is(err, store.ErrFileIndexScanInProgress) {
		t.Fatalf("concurrent BeginFileIndexScan() error = %v, want scan-in-progress", err)
	}
	baseTime := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	records := []store.FileIndexRecord{
		generationRecord("docs/folder", "directory", 0, baseTime),
		generationRecord("docs/zeta", "directory", 0, baseTime.Add(24*time.Hour)),
		generationRecord("docs/report.pdf", "file", 40, baseTime.Add(48*time.Hour)),
		generationRecord("docs/photo.jpg", "file", 10, baseTime.Add(72*time.Hour)),
		generationRecord("docs/100%_done.txt", "file", 20, baseTime.Add(96*time.Hour)),
		generationRecord("docs/alpha.txt", "file", 20, baseTime.Add(96*time.Hour)),
		generationRecord("docs/folder/note.txt", "file", 3, baseTime.Add(120*time.Hour)),
	}
	if err := db.UpsertFileIndexBatch(ctx, generation, records); err != nil {
		t.Fatalf("UpsertFileIndexBatch() error = %v", err)
	}
	if _, err := db.QueryFileIndex(ctx, store.FileIndexQuery{
		Generation: generation, ParentPath: "docs", Limit: 2,
	}); !errors.Is(err, store.ErrFileIndexGenerationNotReady) {
		t.Fatalf("query incomplete generation error = %v", err)
	}
	if err := db.CompleteFileIndexScan(ctx, generation); err != nil {
		t.Fatalf("CompleteFileIndexScan() error = %v", err)
	}
	status, err := db.FileIndexStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != store.FileIndexReady || status.ActiveGeneration != generation ||
		status.BuildingGeneration != 0 || status.Scanned != int64(len(records)) ||
		status.StartedAt == nil || status.CompletedAt == nil || status.LastError != "" {
		t.Fatalf("completed status = %#v", status)
	}

	firstPage, err := db.QueryFileIndex(ctx, store.FileIndexQuery{
		Generation: generation, ParentPath: "docs", Sort: "size-desc", Limit: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := recordPaths(firstPage); !reflect.DeepEqual(got, []string{"docs/folder", "docs/zeta", "docs/report.pdf"}) {
		t.Fatalf("directory-first size page = %#v", got)
	}
	secondPage, err := db.QueryFileIndex(ctx, store.FileIndexQuery{
		Generation: generation, ParentPath: "docs", Sort: "size-desc", Limit: 2, Offset: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := recordPaths(secondPage); !reflect.DeepEqual(got, []string{"docs/report.pdf", "docs/100%_done.txt", "docs/alpha.txt"}) {
		t.Fatalf("second size page = %#v", got)
	}

	sorts := []struct {
		sort string
		want []string
	}{
		{"name-asc", []string{"100%_done.txt", "alpha.txt", "photo.jpg", "report.pdf"}},
		{"name-desc", []string{"report.pdf", "photo.jpg", "alpha.txt", "100%_done.txt"}},
		{"size-asc", []string{"photo.jpg", "100%_done.txt", "alpha.txt", "report.pdf"}},
		{"size-desc", []string{"report.pdf", "100%_done.txt", "alpha.txt", "photo.jpg"}},
		{"modified-asc", []string{"report.pdf", "photo.jpg", "100%_done.txt", "alpha.txt"}},
		{"modified-desc", []string{"100%_done.txt", "alpha.txt", "photo.jpg", "report.pdf"}},
	}
	for _, testCase := range sorts {
		page, err := db.QueryFileIndex(ctx, store.FileIndexQuery{
			Generation: generation, ParentPath: "docs", Sort: testCase.sort, Limit: 20,
		})
		if err != nil {
			t.Fatalf("sort %s: %v", testCase.sort, err)
		}
		var got []string
		for _, record := range page {
			if record.Kind != "directory" {
				got = append(got, record.Name)
			}
		}
		if !reflect.DeepEqual(got, testCase.want) {
			t.Errorf("sort %s = %#v, want %#v", testCase.sort, got, testCase.want)
		}
	}

	literalFilter, err := db.QueryFileIndex(ctx, store.FileIndexQuery{
		Generation: generation, ParentPath: "docs", Keyword: "%_", Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := recordPaths(literalFilter); !reflect.DeepEqual(got, []string{"docs/100%_done.txt"}) {
		t.Fatalf("escaped keyword filter = %#v", got)
	}
	recursive, err := db.QueryFileIndex(ctx, store.FileIndexQuery{
		Generation: generation, ParentPath: "docs/folder", Recursive: true, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := recordPaths(recursive); !reflect.DeepEqual(got, []string{"docs/folder", "docs/folder/note.txt"}) {
		t.Fatalf("recursive query did not retain full paths: %#v", got)
	}
	currentListing, err := db.QueryFileIndex(ctx, store.FileIndexQuery{
		Generation: generation, ParentPath: "docs/folder", Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := recordPaths(currentListing); !reflect.DeepEqual(got, []string{"docs/folder/note.txt"}) {
		t.Fatalf("current listing did not match parent exactly: %#v", got)
	}

	stableBefore := recordPaths(secondPage)
	failedGeneration, err := db.BeginFileIndexScan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertFileIndexBatch(ctx, failedGeneration, []store.FileIndexRecord{
		generationRecord("docs/new.txt", "file", 1, baseTime),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.QueryFileIndex(ctx, store.FileIndexQuery{
		Generation: failedGeneration, ParentPath: "docs", Limit: 10,
	}); !errors.Is(err, store.ErrFileIndexGenerationNotReady) {
		t.Fatalf("query building generation error = %v", err)
	}
	if err := db.FailFileIndexScan(ctx, failedGeneration, "test scan failure"); err != nil {
		t.Fatal(err)
	}
	rawDB, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer rawDB.Close()
	var failedEntries int
	if err := rawDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM file_index_generation_entries WHERE generation = ?", failedGeneration).Scan(&failedEntries); err != nil {
		t.Fatal(err)
	}
	if failedEntries != 0 {
		t.Fatalf("failed generation retained %d partial entries", failedEntries)
	}
	if _, err := db.QueryFileIndex(ctx, store.FileIndexQuery{
		Generation: failedGeneration, ParentPath: "docs", Limit: 10,
	}); !errors.Is(err, store.ErrFileIndexGenerationNotReady) {
		t.Fatalf("query failed generation error = %v", err)
	}
	stableAfter, err := db.QueryFileIndex(ctx, store.FileIndexQuery{
		Generation: generation, ParentPath: "docs", Sort: "size-desc", Limit: 2, Offset: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := recordPaths(stableAfter); !reflect.DeepEqual(got, stableBefore) {
		t.Fatalf("old generation page changed: before=%#v after=%#v", stableBefore, got)
	}
	status, err = db.FileIndexStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != store.FileIndexStale || status.ActiveGeneration != generation ||
		status.BuildingGeneration != 0 || status.Scanned != 1 ||
		status.LastError != "test scan failure" || status.CompletedAt == nil {
		t.Fatalf("failed replacement status = %#v", status)
	}
}

func TestQueryFileIndexTypeSizeAndTimeFilters(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	generation, err := db.BeginFileIndexScan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	captureBase := base.Add(10 * time.Hour)
	makeRecord := func(relativePath, kind string, size int64, modified time.Time, mediaType string, captured *time.Time) store.FileIndexRecord {
		record := generationRecord(relativePath, kind, size, modified)
		record.MediaType = mediaType
		record.CapturedAt = captured
		return record
	}
	captureNext := captureBase.Add(time.Hour)
	records := []store.FileIndexRecord{
		makeRecord("docs/folder", "directory", 0, base, "", nil),
		makeRecord("docs/photo.jpg", "file", 100, base, "application/octet-stream", &captureBase),
		makeRecord("docs/wrong.jpg", "file", 200, base.Add(time.Hour), "application/pdf", &captureNext),
		makeRecord("docs/clip.bin", "file", 300, base.Add(2*time.Hour), "video/mp4", nil),
		makeRecord("docs/song.mp3", "file", 400, base.Add(3*time.Hour), "", nil),
		makeRecord("docs/report.pdf", "file", 500, base.Add(4*time.Hour), "application/pdf", &captureNext),
		makeRecord("docs/bundle.zip", "file", 600, base.Add(5*time.Hour), "application/zip", nil),
		makeRecord("docs/mystery.bin", "file", 700, base.Add(6*time.Hour), "application/octet-stream", nil),
		makeRecord("docs/link.jpg", "symlink", 800, base.Add(7*time.Hour), "image/jpeg", nil),
	}
	if err := db.UpsertFileIndexBatch(ctx, generation, records); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteFileIndexScan(ctx, generation); err != nil {
		t.Fatal(err)
	}
	query := func(filters store.FileIndexQuery) []string {
		t.Helper()
		filters.Generation = generation
		filters.ParentPath = "docs"
		filters.Limit = 20
		page, err := db.QueryFileIndex(ctx, filters)
		if err != nil {
			t.Fatalf("QueryFileIndex(%+v): %v", filters, err)
		}
		return recordPaths(page)
	}
	cases := []struct {
		name   string
		typeOf string
		want   []string
	}{
		{"image", "image", []string{"docs/photo.jpg"}},
		{"MIME wins over conflicting extension", "document", []string{"docs/report.pdf", "docs/wrong.jpg"}},
		{"video MIME", "video", []string{"docs/clip.bin"}},
		{"audio extension fallback", "audio", []string{"docs/song.mp3"}},
		{"archive MIME", "archive", []string{"docs/bundle.zip"}},
		{"directory", "directory", []string{"docs/folder"}},
		{"unknown and non-file entries", "other", []string{"docs/link.jpg", "docs/mystery.bin"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := query(store.FileIndexQuery{Type: testCase.typeOf}); !reflect.DeepEqual(got, testCase.want) {
				t.Fatalf("type %q = %#v, want %#v", testCase.typeOf, got, testCase.want)
			}
		})
	}

	minSize, maxSize := int64(100), int64(100)
	if got := query(store.FileIndexQuery{MinSize: &minSize, MaxSize: &maxSize}); !reflect.DeepEqual(got, []string{"docs/photo.jpg"}) {
		t.Fatalf("inclusive size boundaries = %#v", got)
	}
	modifiedFrom, modifiedBefore := base.Add(time.Hour), base.Add(3*time.Hour)
	if got := query(store.FileIndexQuery{ModifiedFrom: &modifiedFrom, ModifiedBefore: &modifiedBefore}); !reflect.DeepEqual(got, []string{"docs/clip.bin", "docs/wrong.jpg"}) {
		t.Fatalf("half-open modification range = %#v", got)
	}
	capturedFrom, capturedBefore := captureBase, captureNext
	if got := query(store.FileIndexQuery{CapturedFrom: &capturedFrom, CapturedBefore: &capturedBefore}); !reflect.DeepEqual(got, []string{"docs/photo.jpg"}) {
		t.Fatalf("half-open capture range = %#v", got)
	}
	combinedMax := int64(200)
	if got := query(store.FileIndexQuery{
		Keyword: "wrong", Type: "document", MaxSize: &combinedMax,
		ModifiedFrom: &modifiedFrom, ModifiedBefore: &modifiedBefore,
	}); !reflect.DeepEqual(got, []string{"docs/wrong.jpg"}) {
		t.Fatalf("combined filters = %#v", got)
	}

	negative := int64(-1)
	if _, err := db.QueryFileIndex(ctx, store.FileIndexQuery{Generation: generation, ParentPath: "docs", Limit: 1, MinSize: &negative}); err == nil {
		t.Fatal("negative minimum size was accepted")
	}
	reversedMin, reversedMax := int64(200), int64(100)
	if _, err := db.QueryFileIndex(ctx, store.FileIndexQuery{Generation: generation, ParentPath: "docs", Limit: 1, MinSize: &reversedMin, MaxSize: &reversedMax}); err == nil {
		t.Fatal("reversed size range was accepted")
	}
	reversedTime := modifiedBefore
	if _, err := db.QueryFileIndex(ctx, store.FileIndexQuery{Generation: generation, ParentPath: "docs", Limit: 1, ModifiedFrom: &reversedTime, ModifiedBefore: &modifiedFrom}); err == nil {
		t.Fatal("reversed modification time range was accepted")
	}
}

func TestFileIndexBatchRejectsUnsafeOrInconsistentPaths(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	generation, err := db.BeginFileIndexScan(ctx)
	if err != nil {
		t.Fatal(err)
	}

	valid := generationRecord("safe.txt", "file", 1, time.Now())
	cases := []struct {
		name   string
		record store.FileIndexRecord
	}{
		{"absolute", generationRecord("/etc/passwd", "file", 1, time.Now())},
		{"windows absolute", generationRecord("C:/private.txt", "file", 1, time.Now())},
		{"parent traversal", generationRecord("../secret.txt", "file", 1, time.Now())},
		{"dot component", generationRecord("docs/./secret.txt", "file", 1, time.Now())},
		{"backslash", generationRecord("docs\\secret.txt", "file", 1, time.Now())},
		{"control character", generationRecord("docs/\x01secret.txt", "file", 1, time.Now())},
		{"hidden trash", generationRecord("docs/.filesage-trash/item", "file", 1, time.Now())},
	}
	nameMismatch := generationRecord("docs/actual.txt", "file", 1, time.Now())
	nameMismatch.Name = "other.txt"
	cases = append(cases, struct {
		name   string
		record store.FileIndexRecord
	}{"name mismatch", nameMismatch})
	parentMismatch := generationRecord("docs/actual.txt", "file", 1, time.Now())
	parentMismatch.ParentPath = "other"
	cases = append(cases, struct {
		name   string
		record store.FileIndexRecord
	}{"parent mismatch", parentMismatch})

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if err := db.UpsertFileIndexBatch(ctx, generation, []store.FileIndexRecord{valid, testCase.record}); err == nil {
				t.Fatalf("accepted unsafe record %#v", testCase.record)
			}
		})
	}
	status, err := db.FileIndexStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Scanned != 0 {
		t.Fatalf("invalid batches advanced scan count: %d", status.Scanned)
	}
	if err := db.FailFileIndexScan(ctx, generation, "invalid path test complete"); err != nil {
		t.Fatal(err)
	}
}

func TestFileIndexInterruptedScanIsRecoveredAfterStoreClose(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "index.db")
	first, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	interrupted, err := first.BeginFileIndexScan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.UpsertFileIndexBatch(ctx, interrupted, []store.FileIndexRecord{
		generationRecord("partial.txt", "file", 3, time.Now()),
	}); err != nil {
		t.Fatal(err)
	}
	parallelStore, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatalf("opening a second store was blocked by an active scan: %v", err)
	}
	if _, err := parallelStore.BeginFileIndexScan(ctx); !errors.Is(err, store.ErrFileIndexScanInProgress) {
		t.Fatalf("second Store BeginFileIndexScan() error = %v, want scan-in-progress", err)
	}
	if err := parallelStore.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	next, err := second.BeginFileIndexScan(ctx)
	if err != nil {
		t.Fatalf("BeginFileIndexScan() did not recover an interrupted scan: %v", err)
	}
	if next <= interrupted {
		t.Fatalf("recovered generation = %d, want after %d", next, interrupted)
	}
	status, err := second.FileIndexStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.BuildingGeneration != next || status.State != store.FileIndexBuilding || status.Scanned != 0 {
		t.Fatalf("status after interrupted scan recovery = %#v", status)
	}
	if _, err := second.QueryFileIndex(ctx, store.FileIndexQuery{
		Generation: interrupted, ParentPath: "", Limit: 10,
	}); !errors.Is(err, store.ErrFileIndexGenerationNotReady) {
		t.Fatalf("query interrupted generation error = %v", err)
	}
	rawDB, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer rawDB.Close()
	var runState, runError string
	var partialEntries int
	if err := rawDB.QueryRowContext(ctx, "SELECT state, error_message FROM file_index_scan_runs WHERE generation = ?", interrupted).Scan(&runState, &runError); err != nil {
		t.Fatal(err)
	}
	if err := rawDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM file_index_generation_entries WHERE generation = ?", interrupted).Scan(&partialEntries); err != nil {
		t.Fatal(err)
	}
	if runState != "failed" || runError == "" || partialEntries != 0 {
		t.Fatalf("recovered scan state=%q error=%q entries=%d", runState, runError, partialEntries)
	}
	if err := second.FailFileIndexScan(ctx, next, "recovery test complete"); err != nil {
		t.Fatal(err)
	}
}

func TestFileIndexRetentionExpiresOldGenerationCursors(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var generations []int64
	for i := 0; i < 3; i++ {
		generation, err := db.BeginFileIndexScan(ctx)
		if err != nil {
			t.Fatal(err)
		}
		generations = append(generations, generation)
		if err := db.UpsertFileIndexBatch(ctx, generation, []store.FileIndexRecord{
			generationRecord(fmt.Sprintf("run-%d.txt", i), "file", int64(i), time.Now()),
		}); err != nil {
			t.Fatal(err)
		}
		if err := db.CompleteFileIndexScan(ctx, generation); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.QueryFileIndex(ctx, store.FileIndexQuery{
		Generation: generations[0], ParentPath: "", Limit: 10,
	}); !errors.Is(err, store.ErrFileIndexGenerationNotReady) {
		t.Fatalf("expired generation cursor error = %v, want not-ready", err)
	}
	for _, generation := range generations[1:] {
		if _, err := db.QueryFileIndex(ctx, store.FileIndexQuery{
			Generation: generation, ParentPath: "", Limit: 10,
		}); err != nil {
			t.Errorf("retained generation %d query error = %v", generation, err)
		}
	}
}

func TestFileIndexScanLockProcessHelper(t *testing.T) {
	if os.Getenv("FILESAGE_INDEX_LOCK_HELPER") != "1" {
		return
	}
	ctx := context.Background()
	db, err := store.Open(ctx, os.Getenv("FILESAGE_INDEX_LOCK_DB"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	generation, err := db.BeginFileIndexScan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertFileIndexBatch(ctx, generation, []store.FileIndexRecord{
		generationRecord("partial.txt", "file", 3, time.Now()),
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("FILESAGE_INDEX_LOCK_STARTED"), []byte(fmt.Sprint(generation)), 0o600); err != nil {
		t.Fatal(err)
	}
	releasePath := os.Getenv("FILESAGE_INDEX_LOCK_RELEASE")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(releasePath); err == nil {
			if os.Getenv("FILESAGE_INDEX_LOCK_CRASH") == "1" {
				os.Exit(0)
			}
			if err := db.FailFileIndexScan(ctx, generation, "子进程锁测试结束"); err != nil {
				t.Fatal(err)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for lock release signal")
}

func TestFileIndexScanLockRecoversCrashedProcess(t *testing.T) {
	directory := t.TempDir()
	databasePath := filepath.Join(directory, "index.db")
	startedPath := filepath.Join(directory, "started")
	releasePath := filepath.Join(directory, "release")
	command := exec.Command(os.Args[0], "-test.run=^TestFileIndexScanLockProcessHelper$")
	command.Env = append(os.Environ(),
		"FILESAGE_INDEX_LOCK_HELPER=1",
		"FILESAGE_INDEX_LOCK_CRASH=1",
		"FILESAGE_INDEX_LOCK_DB="+databasePath,
		"FILESAGE_INDEX_LOCK_STARTED="+startedPath,
		"FILESAGE_INDEX_LOCK_RELEASE="+releasePath,
	)
	outputFile, err := os.Create(filepath.Join(directory, "child.log"))
	if err != nil {
		t.Fatal(err)
	}
	command.Stdout = outputFile
	command.Stderr = outputFile
	if err := command.Start(); err != nil {
		_ = outputFile.Close()
		t.Fatal(err)
	}
	defer func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
		_ = outputFile.Close()
	}()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(startedPath); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(startedPath); err != nil {
		output, _ := os.ReadFile(filepath.Join(directory, "child.log"))
		t.Fatalf("child process did not start its scan: %v; output: %s", err, output)
	}
	if err := os.WriteFile(releasePath, []byte("crash"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		output, _ := os.ReadFile(filepath.Join(directory, "child.log"))
		t.Fatalf("crashed child process failed unexpectedly: %v; output: %s", err, output)
	}

	db, err := store.Open(context.Background(), databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	next, err := db.BeginFileIndexScan(context.Background())
	if err != nil {
		t.Fatalf("BeginFileIndexScan() did not recover the crashed process: %v", err)
	}
	status, err := db.FileIndexStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.BuildingGeneration != next || status.Scanned != 0 || status.State != store.FileIndexBuilding {
		t.Fatalf("status after process recovery = %#v", status)
	}
	rawDB, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer rawDB.Close()
	var runState, runError string
	var partialEntries int
	if err := rawDB.QueryRowContext(context.Background(), "SELECT state, error_message FROM file_index_scan_runs WHERE generation = 1").Scan(&runState, &runError); err != nil {
		t.Fatal(err)
	}
	if err := rawDB.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM file_index_generation_entries WHERE generation = 1").Scan(&partialEntries); err != nil {
		t.Fatal(err)
	}
	if runState != "failed" || runError == "" || partialEntries != 0 {
		t.Fatalf("crashed scan state=%q error=%q entries=%d", runState, runError, partialEntries)
	}
	if err := db.FailFileIndexScan(context.Background(), next, "崩溃恢复测试结束"); err != nil {
		t.Fatal(err)
	}
}

func TestFileIndexScanLockSerializesProcesses(t *testing.T) {
	directory := t.TempDir()
	databasePath := filepath.Join(directory, "index.db")
	startedPath := filepath.Join(directory, "started")
	releasePath := filepath.Join(directory, "release")
	command := exec.Command(os.Args[0], "-test.run=^TestFileIndexScanLockProcessHelper$")
	command.Env = append(os.Environ(),
		"FILESAGE_INDEX_LOCK_HELPER=1",
		"FILESAGE_INDEX_LOCK_DB="+databasePath,
		"FILESAGE_INDEX_LOCK_STARTED="+startedPath,
		"FILESAGE_INDEX_LOCK_RELEASE="+releasePath,
	)
	outputFile, err := os.Create(filepath.Join(directory, "child.log"))
	if err != nil {
		t.Fatal(err)
	}
	command.Stdout = outputFile
	command.Stderr = outputFile
	if err := command.Start(); err != nil {
		_ = outputFile.Close()
		t.Fatal(err)
	}
	defer func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
		_ = outputFile.Close()
	}()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(startedPath); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(startedPath); err != nil {
		output, _ := os.ReadFile(filepath.Join(directory, "child.log"))
		t.Fatalf("child process did not start its scan: %v; output: %s", err, output)
	}

	db, err := store.Open(context.Background(), databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.BeginFileIndexScan(context.Background()); !errors.Is(err, store.ErrFileIndexScanInProgress) {
		t.Fatalf("second process BeginFileIndexScan() error = %v, want scan-in-progress", err)
	}
	if err := os.WriteFile(releasePath, []byte("release"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		output, _ := os.ReadFile(filepath.Join(directory, "child.log"))
		t.Fatalf("child process failed: %v; output: %s", err, output)
	}
	generation, err := db.BeginFileIndexScan(context.Background())
	if err != nil {
		t.Fatalf("scan lock remained after child released it: %v", err)
	}
	if err := db.FailFileIndexScan(context.Background(), generation, "子进程锁测试完成"); err != nil {
		t.Fatal(err)
	}
}

func generationRecord(relativePath, kind string, size int64, modifiedAt time.Time) store.FileIndexRecord {
	parent := path.Dir(relativePath)
	if parent == "." {
		parent = ""
	}
	return store.FileIndexRecord{
		RelativePath: relativePath,
		ParentPath:   parent,
		Name:         path.Base(relativePath),
		Kind:         kind,
		SizeBytes:    size,
		ModifiedAt:   modifiedAt,
		Mode:         0o640,
		IndexedAt:    modifiedAt,
	}
}

func recordPaths(records []store.FileIndexRecord) []string {
	paths := make([]string, 0, len(records))
	for _, record := range records {
		paths = append(paths, record.RelativePath)
	}
	return paths
}
