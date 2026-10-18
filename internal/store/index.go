package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"mime"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/tus/lockfile"
)

var (
	ErrFileIndexScanInProgress     = errors.New("file index scan already in progress")
	ErrFileIndexScanInactive       = errors.New("file index scan is not active")
	ErrFileIndexGenerationNotReady = errors.New("file index generation is not complete")
	// ErrFileIndexCaptureMetadataUnavailable 表示索引代次未包含拍摄日期元数据。
	ErrFileIndexCaptureMetadataUnavailable = errors.New("file index generation has no capture metadata")
)

var fileIndexScanLocks = struct {
	sync.Mutex
	byStore map[*Store]lockfile.Lockfile
}{byStore: make(map[*Store]lockfile.Lockfile)}

// FileIndexScanState 描述当前索引代次状态。
type FileIndexScanState string

const (
	FileIndexNotStarted FileIndexScanState = "not_started"
	FileIndexBuilding   FileIndexScanState = "building"
	FileIndexReady      FileIndexScanState = "ready"
	FileIndexStale      FileIndexScanState = "stale"
	FileIndexFailed     FileIndexScanState = "failed"
)

// FileIndexStatus 汇总活动代次和最近一次扫描。
type FileIndexStatus struct {
	ActiveGeneration   int64
	BuildingGeneration int64
	State              FileIndexScanState
	Scanned            int64
	StartedAt          *time.Time
	CompletedAt        *time.Time
	LastError          string
}

// FileIndexQuery 固定代次查询文件索引，分页偏移在该代次内保持稳定。
type FileIndexQuery struct {
	Generation     int64
	ParentPath     string
	ExcludePath    string
	Recursive      bool
	Keyword        string
	Type           string
	MinSize        *int64
	MaxSize        *int64
	ModifiedFrom   *time.Time
	ModifiedBefore *time.Time
	CapturedFrom   *time.Time
	CapturedBefore *time.Time
	Sort           string
	Limit          int
	Offset         int
}

type fileIndexCategorySpec struct {
	name         string
	mimePrefixes []string
	mimeTypes    []string
	extensions   []string
}

var fileIndexCategories = []fileIndexCategorySpec{
	{
		name: "image", mimePrefixes: []string{"image/"},
		extensions: []string{".jpg", ".jpeg", ".jpe", ".png", ".gif", ".webp", ".bmp", ".dib", ".tif", ".tiff", ".avif", ".heic", ".heif", ".svg", ".ico", ".icns", ".raw", ".dng", ".cr2", ".cr3", ".nef", ".arw", ".orf", ".rw2"},
	},
	{
		name: "video", mimePrefixes: []string{"video/"},
		extensions: []string{".mp4", ".m4v", ".mov", ".mkv", ".avi", ".wmv", ".webm", ".mpg", ".mpeg", ".mpe", ".3gp", ".3g2", ".mts", ".m2ts", ".ts", ".flv", ".ogv", ".vob"},
	},
	{
		name: "audio", mimePrefixes: []string{"audio/"},
		extensions: []string{".mp3", ".wav", ".flac", ".aac", ".m4a", ".oga", ".ogg", ".opus", ".wma", ".aiff", ".aif", ".alac", ".amr", ".mid", ".midi"},
	},
	{
		name: "document", mimePrefixes: []string{"text/"},
		mimeTypes: []string{
			"application/pdf", "application/msword", "application/rtf", "application/epub+zip",
			"application/json", "application/xml", "application/yaml", "application/x-yaml", "application/toml",
			"application/vnd.ms-excel", "application/vnd.ms-powerpoint",
			"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
			"application/vnd.openxmlformats-officedocument.wordprocessingml.template",
			"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
			"application/vnd.openxmlformats-officedocument.spreadsheetml.template",
			"application/vnd.openxmlformats-officedocument.presentationml.presentation",
			"application/vnd.openxmlformats-officedocument.presentationml.slideshow",
			"application/vnd.oasis.opendocument.text", "application/vnd.oasis.opendocument.spreadsheet",
			"application/vnd.oasis.opendocument.presentation",
		},
		extensions: []string{".pdf", ".doc", ".docx", ".docm", ".odt", ".rtf", ".xls", ".xlsx", ".xlsm", ".ods", ".csv", ".tsv", ".ppt", ".pptx", ".pptm", ".odp", ".txt", ".md", ".markdown", ".rst", ".tex", ".html", ".htm", ".xml", ".json", ".yaml", ".yml", ".toml", ".ini", ".log", ".epub", ".mobi"},
	},
	{
		name: "archive",
		mimeTypes: []string{
			"application/zip", "application/x-zip-compressed", "application/x-tar", "application/x-gtar",
			"application/gzip", "application/x-gzip", "application/vnd.rar", "application/x-rar-compressed",
			"application/x-7z-compressed", "application/x-bzip2", "application/x-xz", "application/zstd",
			"application/x-zstd", "application/x-lz4", "application/x-lzip", "application/x-lzma",
			"application/x-compress", "application/x-iso9660-image", "application/vnd.ms-cab-compressed",
		},
		extensions: []string{".zip", ".tar", ".gz", ".tgz", ".bz2", ".tbz", ".tbz2", ".xz", ".txz", ".7z", ".rar", ".zst", ".zstd", ".lz", ".lzma", ".lz4", ".cab", ".arj", ".cpio", ".iso"},
	},
}

// FileIndexCategory 按媒体类型和常见扩展名归类索引项。
func FileIndexCategory(name, mediaType, kind string) string {
	if kind == "directory" {
		return "directory"
	}
	if kind != "file" {
		return "other"
	}
	parsedType, _, err := mime.ParseMediaType(mediaType)
	if err == nil {
		mediaType = strings.ToLower(parsedType)
	} else {
		mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	}
	if mediaType == "" {
		mediaType = strings.ToLower(mime.TypeByExtension(path.Ext(name)))
		if parsedType, _, err := mime.ParseMediaType(mediaType); err == nil {
			mediaType = strings.ToLower(parsedType)
		}
	}
	for _, category := range fileIndexCategories {
		if categoryMatchesMIME(category, mediaType) {
			return category.name
		}
	}
	extension := strings.ToLower(path.Ext(name))
	for _, category := range fileIndexCategories {
		if containsString(category.extensions, extension) {
			return category.name
		}
	}
	return "other"
}

func categoryMatchesMIME(category fileIndexCategorySpec, mediaType string) bool {
	for _, prefix := range category.mimePrefixes {
		if strings.HasPrefix(mediaType, prefix) {
			return true
		}
	}
	return containsString(category.mimeTypes, mediaType)
}

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func fileIndexTypeExpression() (string, []any) {
	clauses := []string{"WHEN kind = 'directory' THEN 'directory'", "WHEN kind <> 'file' THEN 'other'"}
	args := make([]any, 0, 64)
	for _, category := range fileIndexCategories {
		predicate, predicateArgs := categoryPredicateSQL(category, true)
		if predicate == "" {
			continue
		}
		clauses = append(clauses, "WHEN ("+predicate+") THEN '"+category.name+"'")
		args = append(args, predicateArgs...)
	}
	for _, category := range fileIndexCategories {
		predicate, predicateArgs := categoryPredicateSQL(category, false)
		if predicate == "" {
			continue
		}
		clauses = append(clauses, "WHEN ("+predicate+") THEN '"+category.name+"'")
		args = append(args, predicateArgs...)
	}
	return "CASE " + strings.Join(clauses, " ") + " ELSE 'other' END", args
}

func categoryPredicateSQL(category fileIndexCategorySpec, mediaType bool) (string, []any) {
	parts := make([]string, 0, len(category.mimePrefixes)+len(category.mimeTypes)+len(category.extensions))
	args := make([]any, 0, cap(parts))
	if mediaType {
		normalized := "lower(trim(CASE WHEN instr(media_type, ';') > 0 THEN substr(media_type, 1, instr(media_type, ';') - 1) ELSE media_type END))"
		for _, prefix := range category.mimePrefixes {
			parts = append(parts, normalized+" LIKE ?")
			args = append(args, strings.ToLower(prefix)+"%")
		}
		for _, value := range category.mimeTypes {
			parts = append(parts, "("+normalized+" = ? OR "+normalized+" LIKE ?)")
			args = append(args, strings.ToLower(value), strings.ToLower(value)+";%")
		}
	} else {
		for _, extension := range category.extensions {
			parts = append(parts, "lower(name) LIKE ? ESCAPE '\\'")
			args = append(args, "%"+escapeLike(extension))
		}
	}
	return strings.Join(parts, " OR "), args
}

// BeginFileIndexScan 分配并开始一个新索引代次。
func (s *Store) BeginFileIndexScan(ctx context.Context) (generation int64, returnErr error) {
	if err := acquireFileIndexScanLock(ctx, s); err != nil {
		return 0, err
	}
	keepLock := false
	defer func() {
		if !keepLock {
			if err := releaseFileIndexScanLock(s); err != nil && returnErr == nil {
				returnErr = fmt.Errorf("release failed file index scan lock: %w", err)
			}
		}
	}()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin file index scan: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().UTC().UnixNano()
	if err := recoverInterruptedFileIndexScan(ctx, tx, now); err != nil {
		return 0, err
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE file_index_state
		SET building_generation = next_generation,
			next_generation = next_generation + 1,
			state = 'building', scanned_count = 0, started_at_ns = ?, last_error = ''
		WHERE singleton = 1 AND building_generation IS NULL`, now)
	if err != nil {
		return 0, fmt.Errorf("reserve file index generation: %w", err)
	}
	if err := requireFileIndexUpdate(result, ErrFileIndexScanInProgress); err != nil {
		return 0, err
	}

	if err := tx.QueryRowContext(ctx, `SELECT building_generation FROM file_index_state WHERE singleton = 1`).Scan(&generation); err != nil {
		return 0, fmt.Errorf("read reserved file index generation: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO file_index_scan_runs (generation, state, started_at_ns)
		VALUES (?, 'building', ?)`, generation, now); err != nil {
		return 0, fmt.Errorf("create file index scan record: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit file index scan start: %w", err)
	}
	keepLock = true
	return generation, nil
}

func acquireFileIndexScanLock(ctx context.Context, s *Store) error {
	rows, err := s.db.QueryContext(ctx, "PRAGMA database_list")
	if err != nil {
		return fmt.Errorf("locate file index database: %w", err)
	}
	var databasePath string
	for rows.Next() {
		var sequence int
		var name, filename string
		if err := rows.Scan(&sequence, &name, &filename); err != nil {
			_ = rows.Close()
			return fmt.Errorf("read file index database path: %w", err)
		}
		if name == "main" {
			databasePath = filename
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate SQLite database list: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close SQLite database list: %w", err)
	}
	if databasePath == "" || !filepath.IsAbs(databasePath) {
		return errors.New("file index scan requires an absolute SQLite database path")
	}
	lock, err := lockfile.New(databasePath + ".index-scan.lock")
	if err != nil {
		return fmt.Errorf("create file index scan lock: %w", err)
	}
	if err := lock.TryLock(); err != nil {
		if errors.Is(err, lockfile.ErrBusy) {
			return ErrFileIndexScanInProgress
		}
		return fmt.Errorf("acquire file index scan lock: %w", err)
	}
	fileIndexScanLocks.Lock()
	fileIndexScanLocks.byStore[s] = lock
	fileIndexScanLocks.Unlock()
	return nil
}

func releaseFileIndexScanLock(s *Store) error {
	fileIndexScanLocks.Lock()
	defer fileIndexScanLocks.Unlock()
	lock, found := fileIndexScanLocks.byStore[s]
	if !found {
		return nil
	}
	if err := lock.Unlock(); err != nil {
		return fmt.Errorf("unlock file index scan: %w", err)
	}
	delete(fileIndexScanLocks.byStore, s)
	return nil
}

func recoverInterruptedFileIndexScan(ctx context.Context, tx *sql.Tx, now int64) error {
	var generation sql.NullInt64
	if err := tx.QueryRowContext(ctx, "SELECT building_generation FROM file_index_state WHERE singleton = 1").Scan(&generation); err != nil {
		return fmt.Errorf("check interrupted file index scan: %w", err)
	}
	if !generation.Valid {
		return nil
	}
	const message = "扫描进程中断，未完成索引已清理"
	result, err := tx.ExecContext(ctx, "UPDATE file_index_scan_runs SET state = 'failed', finished_at_ns = ?, error_message = ? WHERE generation = ? AND state = 'building'", now, message, generation.Int64)
	if err != nil {
		return fmt.Errorf("mark interrupted file index scan failed: %w", err)
	}
	if err := requireFileIndexUpdate(result, ErrFileIndexScanInactive); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM file_index_generation_entries WHERE generation = ?", generation.Int64); err != nil {
		return fmt.Errorf("remove interrupted file index entries: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE file_index_state SET building_generation = NULL, state = CASE WHEN active_generation > 0 THEN 'stale' ELSE 'failed' END, last_error = ? WHERE singleton = 1 AND building_generation = ?", message, generation.Int64); err != nil {
		return fmt.Errorf("clear interrupted file index scan: %w", err)
	}
	return nil
}

// UpsertFileIndexBatch 原子写入一批记录并推进扫描计数。
func (s *Store) UpsertFileIndexBatch(ctx context.Context, generation int64, records []FileIndexRecord) error {
	if generation <= 0 {
		return errors.New("file index generation must be positive")
	}
	seen := make(map[string]struct{}, len(records))
	for i, record := range records {
		if err := validateGenerationRecord(record); err != nil {
			return fmt.Errorf("validate file index record %d: %w", i, err)
		}
		if _, exists := seen[record.RelativePath]; exists {
			return fmt.Errorf("duplicate file index path %q in batch", record.RelativePath)
		}
		seen[record.RelativePath] = struct{}{}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin file index batch: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `
		UPDATE file_index_state
		SET scanned_count = scanned_count + ?
		WHERE singleton = 1 AND building_generation = ? AND state = 'building'`, len(records), generation)
	if err != nil {
		return fmt.Errorf("claim file index batch: %w", err)
	}
	if err := requireFileIndexUpdate(result, ErrFileIndexScanInactive); err != nil {
		return err
	}
	result, err = tx.ExecContext(ctx, `
		UPDATE file_index_scan_runs
		SET scanned_count = scanned_count + ?
		WHERE generation = ? AND state = 'building'`, len(records), generation)
	if err != nil {
		return fmt.Errorf("update file index scan count: %w", err)
	}
	if err := requireFileIndexUpdate(result, ErrFileIndexScanInactive); err != nil {
		return err
	}

	for _, record := range records {
		if err := upsertGenerationRecord(ctx, tx, generation, record); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit file index batch: %w", err)
	}
	return nil
}

// CompleteFileIndexScan 原子发布新代次并保留最近两代完整索引。
func (s *Store) CompleteFileIndexScan(ctx context.Context, generation int64) error {
	if generation <= 0 {
		return errors.New("file index generation must be positive")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin file index scan completion: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().UTC().UnixNano()
	result, err := tx.ExecContext(ctx, `
		UPDATE file_index_state
		SET active_generation = ?, building_generation = NULL, state = 'ready',
			completed_at_ns = ?, last_error = ''
		WHERE singleton = 1 AND building_generation = ? AND state = 'building'`, generation, now, generation)
	if err != nil {
		return fmt.Errorf("publish file index generation: %w", err)
	}
	if err := requireFileIndexUpdate(result, ErrFileIndexScanInactive); err != nil {
		return err
	}
	result, err = tx.ExecContext(ctx, `
		UPDATE file_index_scan_runs
		SET state = 'complete', finished_at_ns = ?, capture_metadata_ready = 1
		WHERE generation = ? AND state = 'building'`, now, generation)
	if err != nil {
		return fmt.Errorf("complete file index scan record: %w", err)
	}
	if err := requireFileIndexUpdate(result, ErrFileIndexScanInactive); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM file_index_generation_entries WHERE generation IN (SELECT generation FROM file_index_scan_runs WHERE state = 'complete' ORDER BY generation DESC LIMIT -1 OFFSET 2)"); err != nil {
		return fmt.Errorf("remove expired file index entries: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM file_index_scan_runs WHERE state = 'complete' AND generation NOT IN (SELECT generation FROM file_index_scan_runs WHERE state = 'complete' ORDER BY generation DESC LIMIT 2)"); err != nil {
		return fmt.Errorf("remove expired file index generations: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit file index scan completion: %w", err)
	}
	if err := releaseFileIndexScanLock(s); err != nil {
		return fmt.Errorf("release completed file index scan lock: %w", err)
	}
	return nil
}

// FailFileIndexScan 终止扫描但保留当前活动代次。
func (s *Store) FailFileIndexScan(ctx context.Context, generation int64, message string) error {
	if generation <= 0 {
		return errors.New("file index generation must be positive")
	}
	message = strings.TrimSpace(message)
	if message == "" {
		return errors.New("file index scan failure message is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin file index scan failure: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().UTC().UnixNano()
	result, err := tx.ExecContext(ctx, `
		UPDATE file_index_state
		SET building_generation = NULL,
			state = CASE WHEN active_generation > 0 THEN 'stale' ELSE 'failed' END,
			last_error = ?
		WHERE singleton = 1 AND building_generation = ? AND state = 'building'`, message, generation)
	if err != nil {
		return fmt.Errorf("record file index scan failure: %w", err)
	}
	if err := requireFileIndexUpdate(result, ErrFileIndexScanInactive); err != nil {
		return err
	}
	result, err = tx.ExecContext(ctx, `
		UPDATE file_index_scan_runs
		SET state = 'failed', finished_at_ns = ?, error_message = ?
		WHERE generation = ? AND state = 'building'`, now, message, generation)
	if err != nil {
		return fmt.Errorf("fail file index scan record: %w", err)
	}
	if err := requireFileIndexUpdate(result, ErrFileIndexScanInactive); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM file_index_generation_entries WHERE generation = ?", generation); err != nil {
		return fmt.Errorf("remove failed file index entries: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit file index scan failure: %w", err)
	}
	if err := releaseFileIndexScanLock(s); err != nil {
		return fmt.Errorf("release failed file index scan lock: %w", err)
	}
	return nil
}

// FileIndexStatus 返回活动代次和最近一次扫描状态。
func (s *Store) FileIndexStatus(ctx context.Context) (FileIndexStatus, error) {
	var status FileIndexStatus
	var buildingGeneration, startedAt, completedAt sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT active_generation, building_generation, state, scanned_count,
			started_at_ns, completed_at_ns, last_error
		FROM file_index_state WHERE singleton = 1`).Scan(
		&status.ActiveGeneration, &buildingGeneration, &status.State, &status.Scanned,
		&startedAt, &completedAt, &status.LastError,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return FileIndexStatus{}, errors.New("file index state is missing")
	}
	if err != nil {
		return FileIndexStatus{}, fmt.Errorf("read file index status: %w", err)
	}
	if buildingGeneration.Valid {
		status.BuildingGeneration = buildingGeneration.Int64
	}
	status.StartedAt = nullableTime(startedAt)
	status.CompletedAt = nullableTime(completedAt)
	return status, nil
}

// QueryFileIndex 只查询已完成代次，目录优先并用路径稳定打破并列。
func (s *Store) QueryFileIndex(ctx context.Context, query FileIndexQuery) ([]FileIndexRecord, error) {
	if query.Generation <= 0 {
		return nil, errors.New("file index generation must be positive")
	}
	if err := validateIndexPath(query.ParentPath, true); err != nil {
		return nil, fmt.Errorf("invalid file index query path: %w", err)
	}
	if query.ExcludePath != "" {
		if err := validateIndexPath(query.ExcludePath, false); err != nil || query.ParentPath != "" && query.ExcludePath != query.ParentPath && !strings.HasPrefix(query.ExcludePath, query.ParentPath+"/") {
			return nil, errors.New("invalid excluded file index path")
		}
	}
	if query.Limit <= 0 || query.Limit == int(^uint(0)>>1) || query.Offset < 0 {
		return nil, errors.New("file index query requires a positive limit and non-negative offset")
	}
	if query.Type != "" && query.Type != "directory" && query.Type != "other" && !isKnownFileCategory(query.Type) {
		return nil, fmt.Errorf("unsupported file index type %q", query.Type)
	}
	if query.MinSize != nil && *query.MinSize < 0 || query.MaxSize != nil && *query.MaxSize < 0 {
		return nil, errors.New("file index size filters cannot be negative")
	}
	if query.MinSize != nil && query.MaxSize != nil && *query.MinSize > *query.MaxSize {
		return nil, errors.New("file index minimum size exceeds maximum size")
	}
	if query.ModifiedFrom != nil && query.ModifiedBefore != nil && query.ModifiedFrom.After(*query.ModifiedBefore) {
		return nil, errors.New("file index modification time range is reversed")
	}
	if query.CapturedFrom != nil && query.CapturedBefore != nil && query.CapturedFrom.After(*query.CapturedBefore) {
		return nil, errors.New("file index capture time range is reversed")
	}
	var state string
	var captureMetadataReady int
	err := s.db.QueryRowContext(ctx, `
		SELECT state, capture_metadata_ready
		FROM file_index_scan_runs WHERE generation = ?`, query.Generation).Scan(&state, &captureMetadataReady)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && state != "complete") {
		return nil, ErrFileIndexGenerationNotReady
	}
	if err != nil {
		return nil, fmt.Errorf("check file index generation: %w", err)
	}
	// 旧代次仍可用于不依赖拍摄日期的筛选。
	needsCaptureMetadata := query.CapturedFrom != nil || query.CapturedBefore != nil || query.Sort == "captured-asc" || query.Sort == "captured-desc"
	if needsCaptureMetadata && captureMetadataReady != 1 {
		return nil, ErrFileIndexCaptureMetadataUnavailable
	}

	sortSQL, err := fileIndexSortSQL(query.Sort)
	if err != nil {
		return nil, err
	}
	conditions := []string{"generation = ?"}
	args := []any{query.Generation}
	if query.Recursive {
		if query.ParentPath != "" {
			conditions = append(conditions, "(relative_path = ? OR relative_path LIKE ? ESCAPE '\\')")
			args = append(args, query.ParentPath, escapeLike(query.ParentPath)+"/%")
		}
	} else {
		conditions = append(conditions, "parent_path = ?")
		args = append(args, query.ParentPath)
	}
	if query.ExcludePath != "" {
		conditions = append(conditions, "relative_path <> ? AND relative_path NOT LIKE ? ESCAPE '\\'")
		args = append(args, query.ExcludePath, escapeLike(query.ExcludePath)+"/%")
	}
	if query.Keyword != "" {
		pattern := "%" + escapeLike(query.Keyword) + "%"
		conditions = append(conditions, "(name LIKE ? ESCAPE '\\' COLLATE NOCASE OR relative_path LIKE ? ESCAPE '\\' COLLATE NOCASE)")
		args = append(args, pattern, pattern)
	}
	if query.Type != "" {
		expression, typeArgs := fileIndexTypeExpression()
		conditions = append(conditions, expression+" = ?")
		args = append(args, typeArgs...)
		args = append(args, query.Type)
	}
	if query.MinSize != nil {
		conditions = append(conditions, "kind = 'file' AND size_bytes >= ?")
		args = append(args, *query.MinSize)
	}
	if query.MaxSize != nil {
		conditions = append(conditions, "kind = 'file' AND size_bytes <= ?")
		args = append(args, *query.MaxSize)
	}
	if query.ModifiedFrom != nil {
		conditions = append(conditions, "modified_at_ns >= ?")
		args = append(args, query.ModifiedFrom.UTC().UnixNano())
	}
	if query.ModifiedBefore != nil {
		conditions = append(conditions, "modified_at_ns < ?")
		args = append(args, query.ModifiedBefore.UTC().UnixNano())
	}
	if query.CapturedFrom != nil {
		conditions = append(conditions, "captured_at_ns >= ?")
		args = append(args, query.CapturedFrom.UTC().UnixNano())
	}
	if query.CapturedBefore != nil {
		conditions = append(conditions, "captured_at_ns < ?")
		args = append(args, query.CapturedBefore.UTC().UnixNano())
	}

	statement := `
		SELECT relative_path, parent_path, name, kind, size_bytes, modified_at_ns,
			mode, device, inode, hard_link_count, media_type, captured_at_ns,
			content_sha256, indexed_at_ns
		FROM file_index_generation_entries
		WHERE ` + strings.Join(conditions, " AND ") + `
		ORDER BY CASE WHEN kind = 'directory' THEN 0 ELSE 1 END, ` + sortSQL + `, relative_path ASC
		LIMIT ? OFFSET ?`
	args = append(args, query.Limit+1, query.Offset)
	rows, err := s.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("query file index: %w", err)
	}
	defer rows.Close()

	records := make([]FileIndexRecord, 0, min(query.Limit+1, 128))
	for rows.Next() {
		record, err := scanFileIndexRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("scan file index record: %w", err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate file index records: %w", err)
	}
	return records, nil
}

func isKnownFileCategory(category string) bool {
	for _, spec := range fileIndexCategories {
		if spec.name == category {
			return true
		}
	}
	return false
}

func upsertGenerationRecord(ctx context.Context, tx *sql.Tx, generation int64, record FileIndexRecord) error {
	indexedAt := timestampOrNow(record.IndexedAt)
	var capturedAt any
	if record.CapturedAt != nil {
		capturedAt = record.CapturedAt.UTC().UnixNano()
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO file_index_generation_entries (
			generation, relative_path, parent_path, name, kind, size_bytes, modified_at_ns,
			mode, device, inode, hard_link_count, media_type, captured_at_ns,
			content_sha256, indexed_at_ns
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(generation, relative_path) DO UPDATE SET
			parent_path = excluded.parent_path, name = excluded.name, kind = excluded.kind,
			size_bytes = excluded.size_bytes, modified_at_ns = excluded.modified_at_ns,
			mode = excluded.mode, device = excluded.device, inode = excluded.inode,
			hard_link_count = excluded.hard_link_count, media_type = excluded.media_type,
			captured_at_ns = excluded.captured_at_ns, content_sha256 = excluded.content_sha256,
			indexed_at_ns = excluded.indexed_at_ns`,
		generation, record.RelativePath, record.ParentPath, record.Name, record.Kind,
		record.SizeBytes, zeroTimeValue(record.ModifiedAt), record.Mode,
		nullableUint(record.Device), nullableUint(record.Inode), nullableUint(record.HardLinkCount),
		nullableString(record.MediaType), capturedAt, nullableString(record.ContentSHA256), indexedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert generation file index record %q: %w", record.RelativePath, err)
	}
	return nil
}

func validateGenerationRecord(record FileIndexRecord) error {
	if err := validateIndexPath(record.RelativePath, false); err != nil {
		return fmt.Errorf("relative path: %w", err)
	}
	if err := validateIndexPath(record.ParentPath, true); err != nil {
		return fmt.Errorf("parent path: %w", err)
	}
	if record.Name == "" || path.Base(record.RelativePath) != record.Name {
		return errors.New("name must match the final relative path component")
	}
	parent := path.Dir(record.RelativePath)
	if parent == "." {
		parent = ""
	}
	if parent != record.ParentPath {
		return errors.New("parent path must match the relative path")
	}
	if record.Kind != "file" && record.Kind != "directory" && record.Kind != "symlink" && record.Kind != "other" {
		return fmt.Errorf("unsupported file index kind %q", record.Kind)
	}
	if record.SizeBytes < 0 {
		return errors.New("file index size cannot be negative")
	}
	for _, field := range []struct {
		name  string
		value *uint64
	}{{"device", record.Device}, {"inode", record.Inode}, {"hard link count", record.HardLinkCount}} {
		if field.value != nil && *field.value > maxInt64Value {
			return fmt.Errorf("file index %s exceeds SQLite integer range", field.name)
		}
	}
	if record.HardLinkCount != nil && *record.HardLinkCount == 0 {
		return errors.New("file index hard link count must be positive")
	}
	if record.ContentSHA256 != "" && !isSHA256(record.ContentSHA256) {
		return errors.New("content fingerprint must be a 64-character SHA-256 hex value")
	}
	return nil
}

func validateIndexPath(value string, allowEmpty bool) error {
	if value == "" {
		if allowEmpty {
			return nil
		}
		return errors.New("path is required")
	}
	if strings.HasPrefix(value, "/") || (len(value) >= 2 && isASCIILetter(value[0]) && value[1] == ':') {
		return errors.New("path must be root-relative")
	}
	if strings.ContainsRune(value, '\\') {
		return errors.New("path must use slash separators")
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return errors.New("path contains a control character")
		}
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || component == "." || component == ".." {
			return errors.New("path contains an invalid component")
		}
		if component == ".filesage-trash" {
			return errors.New("path enters the hidden trash directory")
		}
	}
	return nil
}

func isASCIILetter(value byte) bool {
	return (value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z')
}

func fileIndexSortSQL(value string) (string, error) {
	switch value {
	case "", "name-asc":
		return "name COLLATE NOCASE ASC", nil
	case "name-desc":
		return "name COLLATE NOCASE DESC", nil
	case "size-asc":
		return "size_bytes ASC", nil
	case "size-desc":
		return "size_bytes DESC", nil
	case "modified-asc":
		return "modified_at_ns ASC", nil
	case "modified-desc":
		return "modified_at_ns DESC", nil
	case "captured-asc":
		return "CASE WHEN captured_at_ns IS NULL THEN 1 ELSE 0 END ASC, captured_at_ns ASC", nil
	case "captured-desc":
		return "CASE WHEN captured_at_ns IS NULL THEN 1 ELSE 0 END ASC, captured_at_ns DESC", nil
	default:
		return "", fmt.Errorf("unsupported file index sort %q", value)
	}
}

func escapeLike(value string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(value)
}

func nullableTime(value sql.NullInt64) *time.Time {
	if !value.Valid {
		return nil
	}
	result := time.Unix(0, value.Int64).UTC()
	return &result
}

func scanFileIndexRecord(row interface{ Scan(...any) error }) (FileIndexRecord, error) {
	var record FileIndexRecord
	var modifiedAt, indexedAt int64
	var mode int64
	var capturedAt, device, inode, hardLinks sql.NullInt64
	var mediaType, contentSHA256 sql.NullString
	if err := row.Scan(
		&record.RelativePath, &record.ParentPath, &record.Name, &record.Kind,
		&record.SizeBytes, &modifiedAt, &mode, &device, &inode, &hardLinks,
		&mediaType, &capturedAt, &contentSHA256, &indexedAt,
	); err != nil {
		return FileIndexRecord{}, err
	}
	record.ModifiedAt = time.Unix(0, modifiedAt).UTC()
	record.Mode = uint32(mode)
	record.IndexedAt = time.Unix(0, indexedAt).UTC()
	if device.Valid && device.Int64 >= 0 {
		value := uint64(device.Int64)
		record.Device = &value
	}
	if inode.Valid && inode.Int64 >= 0 {
		value := uint64(inode.Int64)
		record.Inode = &value
	}
	if hardLinks.Valid && hardLinks.Int64 >= 0 {
		value := uint64(hardLinks.Int64)
		record.HardLinkCount = &value
	}
	if mediaType.Valid {
		record.MediaType = mediaType.String
	}
	if capturedAt.Valid {
		value := time.Unix(0, capturedAt.Int64).UTC()
		record.CapturedAt = &value
	}
	if contentSHA256.Valid {
		record.ContentSHA256 = contentSHA256.String
	}
	return record, nil
}

func requireFileIndexUpdate(result sql.Result, missing error) error {
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count updated file index rows: %w", err)
	}
	if rows == 0 {
		return missing
	}
	return nil
}
