// Package store 保存可重建的文件元数据和服务流程记录，不保存文件内容或旧版本。
package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	busyTimeoutMS            = 5000
	maxOpenConns             = 8
	maxInt64Value            = uint64(^uint64(0) >> 1)
	maxAgentTextPayloadBytes = 32 << 10
)

var ErrNotFound = errors.New("store record not found")

// ErrConflict 表示记录状态已被其他操作更新。
var ErrConflict = errors.New("store state conflict")

// Store 管理 SQLite 连接池，数据库路径由调用方提供并应位于文件根目录之外。
type Store struct {
	db *sql.DB
}

// Open 打开指定绝对路径的 SQLite 数据库，配置连接并应用迁移。
func Open(ctx context.Context, databasePath string) (*Store, error) {
	if strings.TrimSpace(databasePath) == "" {
		return nil, errors.New("database path is required")
	}
	if !filepath.IsAbs(databasePath) {
		return nil, errors.New("database path must be absolute")
	}
	if databasePath == ":memory:" {
		return nil, errors.New("database path must name a local file")
	}

	absolutePath := filepath.Clean(databasePath)
	if err := os.MkdirAll(filepath.Dir(absolutePath), 0o700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	if info, err := os.Lstat(absolutePath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("database path must not be a symbolic link")
		}
		if !info.Mode().IsRegular() {
			return nil, errors.New("database path must be a regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect database path: %w", err)
	}

	dsn, err := dataSourceName(absolutePath)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open SQLite database: %w", err)
	}
	db.SetMaxOpenConns(maxOpenConns)
	db.SetMaxIdleConns(maxOpenConns)

	store := &Store{db: db}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect to SQLite database: %w", err)
	}
	if err := verifyConnectionPolicy(ctx, db, true); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("verify SQLite connection policy: %w", err)
	}
	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate SQLite database: %w", err)
	}
	return store, nil
}

func dataSourceName(databasePath string) (string, error) {
	uriPath := filepath.ToSlash(databasePath)
	if !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	uri := url.URL{Scheme: "file", Path: uriPath}
	query := url.Values{}
	query.Set("_busy_timeout", fmt.Sprint(busyTimeoutMS))
	query.Set("_defensive", "true")
	query.Set("_foreign_keys", "on")
	query.Set("_journal_mode", "WAL")
	query.Set("_synchronous", "FULL")
	// 写事务先取得锁，避免并发读后升级导致 SQLITE_BUSY。
	query.Set("_txlock", "immediate")
	uri.RawQuery = query.Encode()
	if uri.String() == "" {
		return "", errors.New("build SQLite data source name")
	}
	return uri.String(), nil
}

// Close 释放数据库连接，可重复调用。
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	closeErr := s.db.Close()
	lockErr := releaseFileIndexScanLock(s)
	return errors.Join(closeErr, lockErr)
}

// Health 检查数据库连接和运行配置。
func (s *Store) Health(ctx context.Context) error {
	if s == nil || s.db == nil {
		return errors.New("store is not open")
	}
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping SQLite database: %w", err)
	}
	return verifyConnectionPolicy(ctx, s.db, false)
}

func verifyConnectionPolicy(ctx context.Context, db *sql.DB, checkDefensive bool) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("get SQLite connection: %w", err)
	}
	defer conn.Close()
	if checkDefensive {
		if err := verifyDefensiveMode(ctx, conn); err != nil {
			return err
		}
	}

	var journalMode string
	var synchronous, foreignKeys, busyTimeout int
	if err := conn.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		return fmt.Errorf("read journal mode: %w", err)
	}
	if err := conn.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&synchronous); err != nil {
		return fmt.Errorf("read synchronous mode: %w", err)
	}
	if err := conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		return fmt.Errorf("read foreign key setting: %w", err)
	}
	if err := conn.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		return fmt.Errorf("read busy timeout: %w", err)
	}
	if strings.ToLower(journalMode) != "wal" || synchronous != 2 || foreignKeys != 1 || busyTimeout != busyTimeoutMS {
		return fmt.Errorf("unexpected connection pragmas: journal_mode=%q synchronous=%d foreign_keys=%d busy_timeout=%d", journalMode, synchronous, foreignKeys, busyTimeout)
	}
	return nil
}

func verifyDefensiveMode(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, `PRAGMA writable_schema = ON`); err != nil {
		return fmt.Errorf("probe defensive mode: %w", err)
	}
	var writableSchema int
	readErr := conn.QueryRowContext(ctx, `PRAGMA writable_schema`).Scan(&writableSchema)
	_, resetErr := conn.ExecContext(context.Background(), `PRAGMA writable_schema = OFF`)
	if readErr != nil {
		return fmt.Errorf("read defensive mode probe: %w", readErr)
	}
	if resetErr != nil {
		return fmt.Errorf("reset defensive mode probe: %w", resetErr)
	}
	if writableSchema != 0 {
		return errors.New("SQLite defensive mode is not enabled on the connection")
	}
	return nil
}

// SchemaVersion 返回已成功应用的最新迁移版本。
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var version int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&version); err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return version, nil
}

// FileIndexRecord 仅保存文件系统元数据和当前内容指纹，不保存文件内容。
type FileIndexRecord struct {
	RelativePath  string
	ParentPath    string
	Name          string
	Kind          string
	SizeBytes     int64
	ModifiedAt    time.Time
	Mode          uint32
	Device        *uint64
	Inode         *uint64
	HardLinkCount *uint64
	MediaType     string
	CapturedAt    *time.Time
	ContentSHA256 string
	IndexedAt     time.Time
}

func (s *Store) UpsertFileIndex(ctx context.Context, record FileIndexRecord) error {
	if record.RelativePath == "" || filepath.IsAbs(record.RelativePath) {
		return errors.New("file index path must be root-relative")
	}
	if record.Name == "" {
		return errors.New("file index name is required")
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
	if record.ContentSHA256 != "" && !isSHA256(record.ContentSHA256) {
		return errors.New("content fingerprint must be a 64-character SHA-256 hex value")
	}
	indexedAt := timestampOrNow(record.IndexedAt)
	modifiedAt := zeroTimeValue(record.ModifiedAt)
	var capturedAt any
	if record.CapturedAt != nil {
		capturedAt = record.CapturedAt.UTC().UnixNano()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO file_index (
			relative_path, parent_path, name, kind, size_bytes, modified_at_ns,
			mode, device, inode, hard_link_count, media_type, captured_at_ns,
			content_sha256, indexed_at_ns
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(relative_path) DO UPDATE SET
			parent_path = excluded.parent_path,
			name = excluded.name,
			kind = excluded.kind,
			size_bytes = excluded.size_bytes,
			modified_at_ns = excluded.modified_at_ns,
			mode = excluded.mode,
			device = excluded.device,
			inode = excluded.inode,
			hard_link_count = excluded.hard_link_count,
			media_type = excluded.media_type,
			captured_at_ns = excluded.captured_at_ns,
			content_sha256 = excluded.content_sha256,
			indexed_at_ns = excluded.indexed_at_ns`,
		record.RelativePath, record.ParentPath, record.Name, record.Kind,
		record.SizeBytes, modifiedAt, record.Mode, nullableUint(record.Device),
		nullableUint(record.Inode), nullableUint(record.HardLinkCount),
		record.MediaType, capturedAt, nullableString(record.ContentSHA256), indexedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert file index record: %w", err)
	}
	return nil
}

func (s *Store) FileIndex(ctx context.Context, relativePath string) (FileIndexRecord, error) {
	var record FileIndexRecord
	var modifiedAt, indexedAt int64
	var capturedAt sql.NullInt64
	var device, inode, hardLinks sql.NullInt64
	var mediaType, contentSHA256 sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT relative_path, parent_path, name, kind, size_bytes, modified_at_ns,
			mode, device, inode, hard_link_count, media_type, captured_at_ns,
			content_sha256, indexed_at_ns
		FROM file_index WHERE relative_path = ?`, relativePath).Scan(
		&record.RelativePath, &record.ParentPath, &record.Name, &record.Kind,
		&record.SizeBytes, &modifiedAt, &record.Mode, &device, &inode, &hardLinks,
		&mediaType, &capturedAt, &contentSHA256, &indexedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return FileIndexRecord{}, ErrNotFound
	}
	if err != nil {
		return FileIndexRecord{}, fmt.Errorf("read file index record: %w", err)
	}
	record.ModifiedAt = time.Unix(0, modifiedAt).UTC()
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

type TaskStatus string

const (
	TaskQueued           TaskStatus = "queued"
	TaskAnalyzing        TaskStatus = "analyzing"
	TaskAwaitingApproval TaskStatus = "awaiting_approval"
	TaskRunning          TaskStatus = "running"
	TaskPaused           TaskStatus = "paused"
	TaskSucceeded        TaskStatus = "succeeded"
	TaskPartial          TaskStatus = "partial"
	TaskFailed           TaskStatus = "failed"
	TaskCancelled        TaskStatus = "cancelled"
)

type TaskRecord struct {
	ID            string
	Kind          string
	Status        TaskStatus
	RequestText   string
	ScopePath     string
	ModelProvider string
	ModelName     string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// TaskListQuery 限制任务列表范围和页大小。
type TaskListQuery struct {
	Kind   string
	Status TaskStatus
	Limit  int
	Offset int
}

// TaskSummary 是不含请求正文的任务列表摘要。
type TaskSummary struct {
	ID        string
	Kind      string
	Status    TaskStatus
	ScopePath string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// TaskListPage 保存按更新时间倒序排列的任务页。
type TaskListPage struct {
	Tasks   []TaskSummary
	Limit   int
	Offset  int
	HasMore bool
}

// TaskAuditPage 保存按创建时间倒序排列的审计页。
type TaskAuditPage struct {
	Records []AuditRecord
	Limit   int
	Offset  int
	HasMore bool
}

const (
	defaultTaskPageLimit = 50
	maxTaskPageLimit     = 100
	maxTaskPageOffset    = 1000000
)

func (s *Store) PutTask(ctx context.Context, task TaskRecord) error {
	return putTaskRecord(ctx, s.db, task)
}

// PutTaskAndAudit 在同一事务中写入任务状态和对应审计记录。
func (s *Store) PutTaskAndAudit(ctx context.Context, task TaskRecord, audit AuditRecord) error {
	if audit.TaskID != task.ID {
		return errors.New("task audit must reference the task being saved")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin task audit transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := putTaskRecord(ctx, tx, task); err != nil {
		return err
	}
	if _, err := appendAuditRecord(ctx, tx, audit); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit task audit transaction: %w", err)
	}
	return nil
}

type storeExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func putTaskRecord(ctx context.Context, execer storeExecer, task TaskRecord) error {
	if task.ID == "" || task.Kind == "" {
		return errors.New("task ID and kind are required")
	}
	if !validTaskStatus(task.Status) {
		return fmt.Errorf("unsupported task status %q", task.Status)
	}
	createdAt := timestampOrNow(task.CreatedAt)
	updatedAt := timestampOrNow(task.UpdatedAt)
	_, err := execer.ExecContext(ctx, `
		INSERT INTO tasks (
			id, kind, status, request_text, scope_path, model_provider,
			model_name, created_at_ns, updated_at_ns
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			kind = excluded.kind,
			status = excluded.status,
			request_text = excluded.request_text,
			scope_path = excluded.scope_path,
			model_provider = excluded.model_provider,
			model_name = excluded.model_name,
			updated_at_ns = excluded.updated_at_ns`,
		task.ID, task.Kind, task.Status, task.RequestText, task.ScopePath,
		task.ModelProvider, task.ModelName, createdAt, updatedAt,
	)
	if err != nil {
		return fmt.Errorf("save task record: %w", err)
	}
	return nil
}

func (s *Store) Task(ctx context.Context, id string) (TaskRecord, error) {
	var task TaskRecord
	var createdAt, updatedAt int64
	err := s.db.QueryRowContext(ctx, `
		SELECT id, kind, status, request_text, scope_path, model_provider,
			model_name, created_at_ns, updated_at_ns
		FROM tasks WHERE id = ?`, id).Scan(
		&task.ID, &task.Kind, &task.Status, &task.RequestText, &task.ScopePath,
		&task.ModelProvider, &task.ModelName, &createdAt, &updatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return TaskRecord{}, ErrNotFound
	}
	if err != nil {
		return TaskRecord{}, fmt.Errorf("read task record: %w", err)
	}
	task.CreatedAt = time.Unix(0, createdAt).UTC()
	task.UpdatedAt = time.Unix(0, updatedAt).UTC()
	return task, nil
}

// ListTasks 返回不含请求正文的任务摘要，按更新时间倒序稳定分页。
func (s *Store) ListTasks(ctx context.Context, query TaskListQuery) (TaskListPage, error) {
	if err := validateTaskPage(query.Limit, query.Offset); err != nil {
		return TaskListPage{}, err
	}
	if query.Status != "" && !validTaskStatus(query.Status) {
		return TaskListPage{}, fmt.Errorf("unsupported task status %q", query.Status)
	}
	if len(query.Kind) > 128 || strings.ContainsRune(query.Kind, 0) {
		return TaskListPage{}, errors.New("invalid task kind filter")
	}
	if query.Limit == 0 {
		query.Limit = defaultTaskPageLimit
	}

	conditions := make([]string, 0, 2)
	args := make([]any, 0, 4)
	if query.Kind != "" {
		conditions = append(conditions, "kind = ?")
		args = append(args, query.Kind)
	}
	if query.Status != "" {
		conditions = append(conditions, "status = ?")
		args = append(args, query.Status)
	}
	statement := `SELECT id, kind, status, scope_path, created_at_ns, updated_at_ns FROM tasks`
	if len(conditions) != 0 {
		statement += ` WHERE ` + strings.Join(conditions, ` AND `)
	}
	statement += ` ORDER BY updated_at_ns DESC, id DESC LIMIT ? OFFSET ?`
	args = append(args, query.Limit+1, query.Offset)

	rows, err := s.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return TaskListPage{}, fmt.Errorf("list task summaries: %w", err)
	}
	defer rows.Close()

	page := TaskListPage{Tasks: make([]TaskSummary, 0, query.Limit), Limit: query.Limit, Offset: query.Offset}
	for rows.Next() {
		var task TaskSummary
		var createdAt, updatedAt int64
		if err := rows.Scan(&task.ID, &task.Kind, &task.Status, &task.ScopePath, &createdAt, &updatedAt); err != nil {
			return TaskListPage{}, fmt.Errorf("scan task summary: %w", err)
		}
		task.CreatedAt = time.Unix(0, createdAt).UTC()
		task.UpdatedAt = time.Unix(0, updatedAt).UTC()
		page.Tasks = append(page.Tasks, task)
	}
	if err := rows.Err(); err != nil {
		return TaskListPage{}, fmt.Errorf("iterate task summaries: %w", err)
	}
	if len(page.Tasks) > query.Limit {
		page.Tasks = page.Tasks[:query.Limit]
		page.HasMore = true
	}
	return page, nil
}

func validateTaskPage(limit, offset int) error {
	if limit < 0 || limit > maxTaskPageLimit {
		return fmt.Errorf("task page limit must be between 0 and %d", maxTaskPageLimit)
	}
	if offset < 0 || offset > maxTaskPageOffset {
		return fmt.Errorf("task page offset must be between 0 and %d", maxTaskPageOffset)
	}
	return nil
}

type PlanStatus string

const (
	PlanDraft            PlanStatus = "draft"
	PlanAwaitingApproval PlanStatus = "awaiting_approval"
	PlanApproved         PlanStatus = "approved"
	PlanExecuting        PlanStatus = "executing"
	PlanCompleted        PlanStatus = "completed"
	PlanFailed           PlanStatus = "failed"
	PlanInvalidated      PlanStatus = "invalidated"
	PlanRejected         PlanStatus = "rejected"
	PlanCancelled        PlanStatus = "cancelled"
)

type ApprovalState string

const (
	ApprovalPending     ApprovalState = "pending"
	ApprovalApproved    ApprovalState = "approved"
	ApprovalRejected    ApprovalState = "rejected"
	ApprovalInvalidated ApprovalState = "invalidated"
)

type OperationStatus string

const (
	OperationPending   OperationStatus = "pending"
	OperationRunning   OperationStatus = "running"
	OperationSucceeded OperationStatus = "succeeded"
	OperationFailed    OperationStatus = "failed"
	OperationSkipped   OperationStatus = "skipped"
	OperationPartial   OperationStatus = "partial"
	OperationCancelled OperationStatus = "cancelled"
)

type PlanRecord struct {
	ID            string
	TaskID        string
	Version       int
	Status        PlanStatus
	StopRequested bool
	ExecutionMode string
	ReviewedAt    *time.Time
	ScopePath     string
	ModelProvider string
	ModelName     string
	DataScopeJSON string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	Operations    []OperationRecord
}

type OperationRecord struct {
	ID         string
	PlanID     string
	Ordinal    int
	Action     string
	SourcePath string
	TargetPath string
	// Reason 和 Evidence 仅保存操作说明，不得存放旧内容或内容差异。
	Reason                       string
	Evidence                     string
	Mode                         *uint32
	OwnerUID                     *uint32
	GroupGID                     *uint32
	SourceStateJSON              string
	TargetStateJSON              string
	ProposedContentSHA256        string
	ProposedContentSize          int64
	TargetConflictPolicy         string
	RequiresExplicitConfirmation bool
	Approval                     ApprovalState
	ApprovedBy                   string
	ApprovedAt                   *time.Time
	Status                       OperationStatus
	ResultMessage                string
	CreatedAt                    time.Time
	UpdatedAt                    time.Time
}

func validOperationMetadata(action, sourcePath, targetPath string, mode, ownerUID, groupGID *uint32) bool {
	switch action {
	case "edit_text":
		return sourcePath != "" && sourcePath == targetPath && mode == nil && ownerUID == nil && groupGID == nil
	case "overwrite_file":
		return sourcePath != "" && sourcePath == targetPath && mode == nil && ownerUID == nil && groupGID == nil
	case "create_file":
		parent := path.Dir(targetPath)
		if parent == "." {
			parent = ""
		}
		return targetPath != "" && sourcePath == parent && mode == nil && ownerUID == nil && groupGID == nil
	case "set_permissions":
		return sourcePath == targetPath && mode != nil && *mode <= 0o777 && ownerUID == nil && groupGID == nil
	case "set_owner":
		return sourcePath == targetPath && mode == nil && (ownerUID != nil || groupGID != nil)
	default:
		return mode == nil && ownerUID == nil && groupGID == nil
	}
}

func nullableUint32(value *uint32) any {
	if value == nil {
		return nil
	}
	return int64(*value)
}

func optionalUint32(value sql.NullInt64, maximum uint64) (*uint32, bool) {
	if !value.Valid {
		return nil, true
	}
	if value.Int64 < 0 || uint64(value.Int64) > maximum {
		return nil, false
	}
	converted := uint32(value.Int64)
	return &converted, true
}

// CreatePlan 在一个事务中写入不可变的方案版本及其操作。
func (s *Store) CreatePlan(ctx context.Context, plan PlanRecord) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin plan transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := createPlanRecord(ctx, tx, plan); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit plan transaction: %w", err)
	}
	return nil
}

func createPlanRecord(ctx context.Context, execer storeExecer, plan PlanRecord) error {
	if plan.ID == "" || plan.TaskID == "" || plan.Version < 1 {
		return errors.New("plan ID, task ID, and positive version are required")
	}
	if !validPlanStatus(plan.Status) {
		return fmt.Errorf("unsupported plan status %q", plan.Status)
	}
	if plan.DataScopeJSON == "" {
		plan.DataScopeJSON = "{}"
	}
	if !jsonObject(plan.DataScopeJSON) {
		return errors.New("plan data scope must be a JSON object")
	}
	createdAt := timestampOrNow(plan.CreatedAt)
	updatedAt := timestampOrNow(plan.UpdatedAt)

	_, err := execer.ExecContext(ctx, `
		INSERT INTO plans (
			id, task_id, version, status, scope_path, model_provider,
			model_name, data_scope_json, created_at_ns, updated_at_ns
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		plan.ID, plan.TaskID, plan.Version, plan.Status, plan.ScopePath,
		plan.ModelProvider, plan.ModelName, plan.DataScopeJSON, createdAt, updatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert plan record: %w", err)
	}

	for i := range plan.Operations {
		operation := plan.Operations[i]
		if operation.ID == "" {
			return fmt.Errorf("operation %d ID is required", i)
		}
		if operation.PlanID != "" && operation.PlanID != plan.ID {
			return fmt.Errorf("operation %q belongs to another plan", operation.ID)
		}
		if operation.Ordinal < 0 || operation.Action == "" {
			return fmt.Errorf("operation %q requires a non-negative ordinal and action", operation.ID)
		}
		if operation.TargetConflictPolicy == "" {
			operation.TargetConflictPolicy = "fail"
		}
		if operation.SourceStateJSON == "" {
			operation.SourceStateJSON = "{}"
		}
		if !jsonObject(operation.SourceStateJSON) {
			return fmt.Errorf("operation %q source state must be a JSON object", operation.ID)
		}
		if operation.TargetStateJSON == "" {
			operation.TargetStateJSON = "{}"
		}
		if !jsonObject(operation.TargetStateJSON) {
			return fmt.Errorf("operation %q target state must be a JSON object", operation.ID)
		}
		if operation.Approval == "" {
			operation.Approval = ApprovalPending
		}
		if operation.Status == "" {
			operation.Status = OperationPending
		}
		if !validApprovalState(operation.Approval) || !validOperationStatus(operation.Status) {
			return fmt.Errorf("operation %q has an unsupported state", operation.ID)
		}
		if !validOperationMetadata(operation.Action, operation.SourcePath, operation.TargetPath, operation.Mode, operation.OwnerUID, operation.GroupGID) {
			return fmt.Errorf("operation %q has invalid metadata parameters", operation.ID)
		}
		if !validProposedTextMetadata(operation) {
			return fmt.Errorf("operation %q has invalid proposed content metadata", operation.ID)
		}
		if operation.Approval == ApprovalApproved && (operation.ApprovedAt == nil || operation.ApprovedBy == "") {
			return fmt.Errorf("approved operation %q requires approver and timestamp", operation.ID)
		}
		if operation.Approval != ApprovalApproved && (operation.ApprovedAt != nil || operation.ApprovedBy != "") {
			return fmt.Errorf("unapproved operation %q cannot include approver metadata", operation.ID)
		}
		operationCreatedAt := timestampOrNow(operation.CreatedAt)
		operationUpdatedAt := timestampOrNow(operation.UpdatedAt)
		var approvedAt any
		if operation.ApprovedAt != nil {
			approvedAt = operation.ApprovedAt.UTC().UnixNano()
		}
		_, err := execer.ExecContext(ctx, `
			INSERT INTO operations (
				id, plan_id, ordinal, action, source_path, target_path, reason,
				evidence, mode, owner_uid, group_gid, source_state_json, target_state_json, target_conflict_policy,
				requires_explicit_confirmation, approval_state, approved_by,
				approved_at_ns, execution_status, result_message,
				created_at_ns, updated_at_ns, proposed_content_sha256, proposed_content_size
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			operation.ID, plan.ID, operation.Ordinal, operation.Action,
			operation.SourcePath, operation.TargetPath, operation.Reason,
			operation.Evidence, nullableUint32(operation.Mode), nullableUint32(operation.OwnerUID), nullableUint32(operation.GroupGID),
			operation.SourceStateJSON, operation.TargetStateJSON,
			operation.TargetConflictPolicy, boolInt(operation.RequiresExplicitConfirmation),
			operation.Approval, operation.ApprovedBy, approvedAt,
			operation.Status, operation.ResultMessage,
			operationCreatedAt, operationUpdatedAt,
			operation.ProposedContentSHA256, operation.ProposedContentSize,
		)
		if err != nil {
			return fmt.Errorf("insert operation %q: %w", operation.ID, err)
		}
	}
	return nil
}

func validProposedTextMetadata(operation OperationRecord) bool {
	if operation.Action != "edit_text" && operation.Action != "create_file" && operation.Action != "overwrite_file" {
		return operation.ProposedContentSHA256 == "" && operation.ProposedContentSize == 0
	}
	if operation.ProposedContentSize < 0 || operation.ProposedContentSize > maxAgentTextPayloadBytes || operation.Action == "edit_text" && operation.ProposedContentSize == 0 || len(operation.ProposedContentSHA256) != 64 {
		return false
	}
	_, err := hex.DecodeString(operation.ProposedContentSHA256)
	return err == nil
}

func (s *Store) Plan(ctx context.Context, id string) (PlanRecord, error) {
	var plan PlanRecord
	var createdAt, updatedAt int64
	var reviewedAt sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT id, task_id, version, status, scope_path, model_provider,
			model_name, data_scope_json, created_at_ns, updated_at_ns, stop_requested,
			execution_mode, reviewed_at_ns
		FROM plans WHERE id = ?`, id).Scan(
		&plan.ID, &plan.TaskID, &plan.Version, &plan.Status, &plan.ScopePath,
		&plan.ModelProvider, &plan.ModelName, &plan.DataScopeJSON, &createdAt, &updatedAt, &plan.StopRequested,
		&plan.ExecutionMode, &reviewedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return PlanRecord{}, ErrNotFound
	}
	if err != nil {
		return PlanRecord{}, fmt.Errorf("read plan record: %w", err)
	}
	plan.CreatedAt = time.Unix(0, createdAt).UTC()
	plan.UpdatedAt = time.Unix(0, updatedAt).UTC()
	if reviewedAt.Valid {
		value := time.Unix(0, reviewedAt.Int64).UTC()
		plan.ReviewedAt = &value
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, plan_id, ordinal, action, source_path, target_path, reason,
			evidence, mode, owner_uid, group_gid, source_state_json, target_state_json, target_conflict_policy,
			requires_explicit_confirmation, approval_state, approved_by,
			approved_at_ns, execution_status, result_message,
			created_at_ns, updated_at_ns, proposed_content_sha256, proposed_content_size
		FROM operations WHERE plan_id = ? ORDER BY ordinal`, id)
	if err != nil {
		return PlanRecord{}, fmt.Errorf("read plan operations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var operation OperationRecord
		var requiresConfirmation int
		var approvedAt sql.NullInt64
		var mode, ownerUID, groupGID sql.NullInt64
		var operationCreatedAt, operationUpdatedAt int64
		if err := rows.Scan(
			&operation.ID, &operation.PlanID, &operation.Ordinal, &operation.Action,
			&operation.SourcePath, &operation.TargetPath, &operation.Reason,
			&operation.Evidence, &mode, &ownerUID, &groupGID, &operation.SourceStateJSON, &operation.TargetStateJSON,
			&operation.TargetConflictPolicy, &requiresConfirmation,
			&operation.Approval, &operation.ApprovedBy, &approvedAt,
			&operation.Status, &operation.ResultMessage,
			&operationCreatedAt, &operationUpdatedAt,
			&operation.ProposedContentSHA256, &operation.ProposedContentSize,
		); err != nil {
			return PlanRecord{}, fmt.Errorf("scan operation record: %w", err)
		}
		operation.RequiresExplicitConfirmation = requiresConfirmation != 0
		var valid bool
		if operation.Mode, valid = optionalUint32(mode, 0o777); !valid {
			return PlanRecord{}, errors.New("operation permission mode is invalid")
		}
		if operation.OwnerUID, valid = optionalUint32(ownerUID, uint64(^uint32(0))); !valid {
			return PlanRecord{}, errors.New("operation owner uid is invalid")
		}
		if operation.GroupGID, valid = optionalUint32(groupGID, uint64(^uint32(0))); !valid {
			return PlanRecord{}, errors.New("operation group gid is invalid")
		}
		if approvedAt.Valid {
			value := time.Unix(0, approvedAt.Int64).UTC()
			operation.ApprovedAt = &value
		}
		operation.CreatedAt = time.Unix(0, operationCreatedAt).UTC()
		operation.UpdatedAt = time.Unix(0, operationUpdatedAt).UTC()
		plan.Operations = append(plan.Operations, operation)
	}
	if err := rows.Err(); err != nil {
		return PlanRecord{}, fmt.Errorf("iterate plan operations: %w", err)
	}
	return plan, nil
}

func (s *Store) SetPlanStatus(ctx context.Context, id string, status PlanStatus, updatedAt time.Time) error {
	if !validPlanStatus(status) {
		return fmt.Errorf("unsupported plan status %q", status)
	}
	result, err := s.db.ExecContext(ctx, `UPDATE plans SET status = ?, updated_at_ns = ? WHERE id = ?`, status, timestampOrNow(updatedAt), id)
	if err != nil {
		return fmt.Errorf("update plan status: %w", err)
	}
	return requireOneRow(result, "plan")
}

func (s *Store) SetOperationApproval(ctx context.Context, id string, state ApprovalState, approvedBy string, at time.Time) error {
	if !validApprovalState(state) {
		return fmt.Errorf("unsupported approval state %q", state)
	}
	var approvedAt any
	if state == ApprovalApproved {
		if approvedBy == "" || at.IsZero() {
			return errors.New("approved operations require an approver and timestamp")
		}
		approvedAt = at.UTC().UnixNano()
	} else {
		approvedBy = ""
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE operations SET approval_state = ?, approved_by = ?, approved_at_ns = ?,
			updated_at_ns = ? WHERE id = ?`,
		state, approvedBy, approvedAt, timestampOrNow(at), id,
	)
	if err != nil {
		return fmt.Errorf("update operation approval: %w", err)
	}
	return requireOneRow(result, "operation")
}

type AuditRecord struct {
	ID          int64
	TaskID      string
	PlanID      string
	OperationID string
	EventType   string
	Actor       string
	Outcome     string
	Message     string
	SourcePath  string
	TargetPath  string
	CreatedAt   time.Time
}

type TrashStatus string

const (
	TrashMoving         TrashStatus = "moving"
	TrashTrashed        TrashStatus = "trashed"
	TrashRestoring      TrashStatus = "restoring"
	TrashPurging        TrashStatus = "purging"
	TrashNeedsAttention TrashStatus = "needs_attention"
	TrashRestored       TrashStatus = "restored"
	TrashPurged         TrashStatus = "purged"
)

// TrashRecord 保存回收项元数据，不保存文件内容或旧版本。
type TrashRecord struct {
	ID                    int64
	StateSeq              int64
	OriginalPath          string
	TrashPath             string
	MountRoot             string
	Kind                  string
	SizeBytes             int64
	ModifiedAt            time.Time
	Mode                  uint32
	Device                *uint64
	Inode                 *uint64
	LinkCount             *uint64
	IdentityAvailable     bool
	Symlink               bool
	LinkDevice            *uint64
	LinkInode             *uint64
	LinkCountForLink      *uint64
	LinkIdentityAvailable bool
	Status                TrashStatus
	DeletedAt             time.Time
	RestorePath           string
	Message               string
	// 审计关联只用于初始事件，不持久化到回收项元数据。
	Actor            string
	AuditTaskID      string
	AuditPlanID      string
	AuditOperationID string
}

// TrashPage 保存回收站快照与分页结果。
type TrashPage struct {
	Items    []TrashRecord
	Limit    int
	HasMore  bool
	Snapshot int64
}

// BeginTrash 登记移动意图并写入同一事务的审计事件。
func (s *Store) BeginTrash(ctx context.Context, record TrashRecord) (int64, error) {
	if record.OriginalPath == "" || !validTrashKind(record.Kind) || record.SizeBytes < 0 {
		return 0, errors.New("invalid trash record")
	}
	if err := validateTrashIdentity(record); err != nil {
		return 0, err
	}
	actor := record.Actor
	if actor == "" {
		actor = "user"
	}
	if actor != "user" && actor != "agent" || actor == "agent" && (record.AuditTaskID == "" || record.AuditPlanID == "" || record.AuditOperationID == "") {
		return 0, errors.New("invalid trash audit actor")
	}
	deletedAt := timestampOrNow(record.DeletedAt)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin trash transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `
		INSERT INTO trash_items (
			original_path, trash_path, mount_root, kind, size_bytes, modified_at_ns,
			mode, device, inode, link_count, identity_available, symlink, link_device,
			link_inode, link_count_for_link, link_identity_available, status,
			deleted_at_ns, restore_path, message
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.OriginalPath, "pending", record.MountRoot, record.Kind,
		record.SizeBytes, zeroTimeValue(record.ModifiedAt), record.Mode,
		nullableUint(record.Device), nullableUint(record.Inode), nullableUint(record.LinkCount),
		boolInt(record.IdentityAvailable), boolInt(record.Symlink), nullableUint(record.LinkDevice), nullableUint(record.LinkInode),
		nullableUint(record.LinkCountForLink), boolInt(record.LinkIdentityAvailable), TrashMoving,
		deletedAt, "", "",
	)
	if err != nil {
		return 0, fmt.Errorf("insert trash intent: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("read trash item ID: %w", err)
	}
	stateSeq, err := appendAuditRecord(ctx, tx, AuditRecord{
		TaskID: record.AuditTaskID, PlanID: record.AuditPlanID, OperationID: record.AuditOperationID,
		EventType: "trash_moving", Actor: actor, Outcome: "started",
		Message: "已登记回收移动", SourcePath: record.OriginalPath,
	})
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE trash_items SET state_seq = ? WHERE id = ?`, stateSeq, id); err != nil {
		return 0, fmt.Errorf("set initial trash state sequence: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit trash intent: %w", err)
	}
	return id, nil
}

// SetTrashDestination 登记用户操作的回收目标。
func (s *Store) SetTrashDestination(ctx context.Context, id int64, destination string) error {
	return s.setTrashDestination(ctx, id, destination, AuditRecord{Actor: "user"})
}

// SetTrashDestinationForAgent 保留 Agent 任务与操作的审计关联。
func (s *Store) SetTrashDestinationForAgent(ctx context.Context, id int64, destination, taskID, planID, operationID string) error {
	if taskID == "" || planID == "" || operationID == "" {
		return errors.New("agent trash audit identifiers are required")
	}
	return s.setTrashDestination(ctx, id, destination, AuditRecord{Actor: "agent", TaskID: taskID, PlanID: planID, OperationID: operationID})
}

func (s *Store) setTrashDestination(ctx context.Context, id int64, destination string, audit AuditRecord) error {
	if id < 1 || !validTrashPath(destination) {
		return errors.New("invalid trash destination")
	}
	if audit.Actor != "user" && audit.Actor != "agent" || audit.Actor == "agent" && (audit.TaskID == "" || audit.PlanID == "" || audit.OperationID == "") {
		return errors.New("invalid trash audit actor")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin trash destination transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE trash_items SET trash_path = ? WHERE id = ? AND status = 'moving' AND trash_path = 'pending'`, destination, id)
	if err != nil {
		return fmt.Errorf("set trash destination: %w", err)
	}
	if err := requireOneRow(result, "trash item"); err != nil {
		return err
	}
	var sourcePath string
	if err := tx.QueryRowContext(ctx, `SELECT original_path FROM trash_items WHERE id = ?`, id).Scan(&sourcePath); err != nil {
		return err
	}
	audit.EventType, audit.Outcome, audit.Message = "trash_destination_ready", "moving", "已确定回收目标"
	audit.SourcePath, audit.TargetPath = sourcePath, destination
	stateSeq, err := appendAuditRecord(ctx, tx, audit)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE trash_items SET state_seq = ? WHERE id = ?`, stateSeq, id); err != nil {
		return fmt.Errorf("update trash state sequence: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit trash destination: %w", err)
	}
	return nil
}

func (s *Store) SetTrashStatus(ctx context.Context, id int64, expected, status TrashStatus, restorePath, message, actor, eventType string) error {
	return s.setTrashStatus(ctx, id, expected, nil, status, restorePath, message, AuditRecord{Actor: actor, EventType: eventType})
}

// SetTrashStatusForAgent 记录带任务关联的 Agent 回收状态。
func (s *Store) SetTrashStatusForAgent(ctx context.Context, id int64, expected, status TrashStatus, restorePath, message, taskID, planID, operationID, eventType string) error {
	if taskID == "" || planID == "" || operationID == "" {
		return errors.New("agent trash audit identifiers are required")
	}
	return s.setTrashStatus(ctx, id, expected, nil, status, restorePath, message, AuditRecord{
		Actor: "agent", TaskID: taskID, PlanID: planID, OperationID: operationID, EventType: eventType,
	})
}

// SetTrashStatusForAgentAtSeq 用任务审计关联和状态序号更新回收阶段。
func (s *Store) SetTrashStatusForAgentAtSeq(ctx context.Context, id int64, expected TrashStatus, expectedSeq int64, status TrashStatus, restorePath, message, taskID, planID, operationID, eventType string) error {
	if taskID == "" || planID == "" || operationID == "" || expectedSeq < 0 {
		return errors.New("invalid agent trash audit or state sequence")
	}
	return s.setTrashStatus(ctx, id, expected, &expectedSeq, status, restorePath, message, AuditRecord{
		Actor: "agent", TaskID: taskID, PlanID: planID, OperationID: operationID, EventType: eventType,
	})
}

// SetTrashStatusAtSeq 仅当状态和最近事件序号均未变化时更新阶段。
func (s *Store) SetTrashStatusAtSeq(ctx context.Context, id int64, expected TrashStatus, expectedSeq int64, status TrashStatus, restorePath, message, actor, eventType string) error {
	if expectedSeq < 0 {
		return errors.New("invalid expected trash sequence")
	}
	return s.setTrashStatus(ctx, id, expected, &expectedSeq, status, restorePath, message, AuditRecord{Actor: actor, EventType: eventType})
}

func (s *Store) setTrashStatus(ctx context.Context, id int64, expected TrashStatus, expectedSeq *int64, status TrashStatus, restorePath, message string, audit AuditRecord) error {
	if id < 1 || !validTrashStatus(expected) || !validTrashStatus(status) || audit.Actor == "" || audit.EventType == "" {
		return errors.New("invalid trash status update")
	}
	if audit.Actor == "agent" && (audit.TaskID == "" || audit.PlanID == "" || audit.OperationID == "") {
		return errors.New("agent trash audit identifiers are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin trash status transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	query := `UPDATE trash_items SET status = ?, restore_path = ?, message = ? WHERE id = ? AND status = ?`
	args := []any{status, restorePath, message, id, expected}
	if expectedSeq != nil {
		query += ` AND state_seq = ?`
		args = append(args, *expectedSeq)
	}
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("update trash status: %w", err)
	}
	if err := requireOneRow(result, "trash item"); err != nil {
		if expectedSeq != nil && errors.Is(err, ErrNotFound) {
			return ErrConflict
		}
		return err
	}
	var sourcePath, targetPath string
	if err := tx.QueryRowContext(ctx, `SELECT original_path, trash_path FROM trash_items WHERE id = ?`, id).Scan(&sourcePath, &targetPath); err != nil {
		return fmt.Errorf("read trash paths for audit: %w", err)
	}
	if restorePath != "" {
		targetPath = restorePath
	}
	audit.Outcome, audit.Message = string(status), message
	audit.SourcePath, audit.TargetPath = sourcePath, targetPath
	stateSeq, err := appendAuditRecord(ctx, tx, audit)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE trash_items SET state_seq = ? WHERE id = ?`, stateSeq, id); err != nil {
		return fmt.Errorf("update trash state sequence: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit trash status: %w", err)
	}
	return nil
}

// Trash 返回指定回收项。
func (s *Store) Trash(ctx context.Context, id int64) (TrashRecord, error) {
	if id < 1 {
		return TrashRecord{}, ErrNotFound
	}
	record, err := scanTrashRecord(s.db.QueryRowContext(ctx, trashSelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return TrashRecord{}, ErrNotFound
	}
	if err != nil {
		return TrashRecord{}, fmt.Errorf("read trash item: %w", err)
	}
	return record, nil
}

const trashSelect = `SELECT id, original_path, trash_path, mount_root, kind, size_bytes,
	modified_at_ns, mode, device, inode, link_count, identity_available, symlink,
	link_device, link_inode, link_count_for_link, link_identity_available, status,
	deleted_at_ns, restore_path, message, state_seq FROM trash_items`

// ListTrash 按审计序号和降序 ID 返回活动项游标页；snapshot 为 -1 时在事务内创建快照。
func (s *Store) ListTrash(ctx context.Context, limit int, snapshot, beforeID int64) (TrashPage, error) {
	if limit < 1 || limit > 100 || snapshot < -1 || beforeID < 0 {
		return TrashPage{}, errors.New("invalid trash page")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return TrashPage{}, fmt.Errorf("begin trash snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var latestSeq int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM audit_events`).Scan(&latestSeq); err != nil {
		return TrashPage{}, fmt.Errorf("read trash snapshot: %w", err)
	}
	if snapshot < 0 {
		snapshot = latestSeq
	} else if snapshot > latestSeq {
		return TrashPage{}, ErrConflict
	}
	query := trashSelect + ` WHERE state_seq <= ? AND status IN ('trashed', 'needs_attention')`
	args := []any{snapshot}
	if beforeID > 0 {
		query += ` AND id < ?`
		args = append(args, beforeID)
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return TrashPage{}, fmt.Errorf("list trash items: %w", err)
	}
	page := TrashPage{Items: make([]TrashRecord, 0, limit), Limit: limit, Snapshot: snapshot}
	for rows.Next() {
		record, err := scanTrashRecord(rows)
		if err != nil {
			return TrashPage{}, fmt.Errorf("scan trash item: %w", err)
		}
		page.Items = append(page.Items, record)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return TrashPage{}, fmt.Errorf("iterate trash items: %w", err)
	}
	if err := rows.Close(); err != nil {
		return TrashPage{}, fmt.Errorf("close trash list: %w", err)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.HasMore = true
	}
	if err := tx.Commit(); err != nil {
		return TrashPage{}, fmt.Errorf("commit trash snapshot: %w", err)
	}
	return page, nil
}

// ListTrashForPurge 返回快照内下一批活动项。
func (s *Store) ListTrashForPurge(ctx context.Context, snapshot, afterID int64, limit int) ([]TrashRecord, error) {
	if snapshot < 0 || afterID < 0 || limit < 1 || limit > 100 {
		return nil, errors.New("invalid trash purge page")
	}
	var latestSeq int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM audit_events`).Scan(&latestSeq); err != nil {
		return nil, fmt.Errorf("read trash purge snapshot: %w", err)
	}
	if snapshot > latestSeq {
		return nil, ErrConflict
	}
	rows, err := s.db.QueryContext(ctx, trashSelect+`
		WHERE id > ? AND state_seq <= ? AND status IN ('trashed', 'needs_attention')
		ORDER BY id LIMIT ?`, afterID, snapshot, limit)
	if err != nil {
		return nil, fmt.Errorf("list trash purge items: %w", err)
	}
	defer rows.Close()
	items := make([]TrashRecord, 0, limit)
	for rows.Next() {
		record, err := scanTrashRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("scan trash purge item: %w", err)
		}
		items = append(items, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate trash purge items: %w", err)
	}
	return items, nil
}

// ListPendingTrash 返回需要启动恢复核对的阶段记录。
func (s *Store) ListPendingTrash(ctx context.Context, afterID int64, limit int) ([]TrashRecord, error) {
	if afterID < 0 || limit < 1 || limit > 100 {
		return nil, errors.New("invalid pending trash page")
	}
	rows, err := s.db.QueryContext(ctx, trashSelect+`
		WHERE id > ? AND status IN ('moving', 'restoring', 'purging')
		ORDER BY id LIMIT ?`, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("list pending trash items: %w", err)
	}
	defer rows.Close()
	items := make([]TrashRecord, 0, limit)
	for rows.Next() {
		record, err := scanTrashRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("scan pending trash item: %w", err)
		}
		items = append(items, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending trash items: %w", err)
	}
	return items, nil
}

type trashScanner interface{ Scan(...any) error }

func scanTrashRecord(row trashScanner) (TrashRecord, error) {
	var record TrashRecord
	var modifiedAt, deletedAt int64
	var device, inode, links, linkDevice, linkInode, linkLinks sql.NullInt64
	var identity, symlink, linkIdentity int
	err := row.Scan(&record.ID, &record.OriginalPath, &record.TrashPath, &record.MountRoot,
		&record.Kind, &record.SizeBytes, &modifiedAt, &record.Mode, &device, &inode, &links,
		&identity, &symlink, &linkDevice, &linkInode, &linkLinks, &linkIdentity, &record.Status,
		&deletedAt, &record.RestorePath, &record.Message, &record.StateSeq)
	if err != nil {
		return TrashRecord{}, err
	}
	record.ModifiedAt = time.Unix(0, modifiedAt).UTC()
	record.DeletedAt = time.Unix(0, deletedAt).UTC()
	record.IdentityAvailable, record.Symlink, record.LinkIdentityAvailable = identity != 0, symlink != 0, linkIdentity != 0
	record.Device, record.Inode, record.LinkCount = uintPointer(device), uintPointer(inode), uintPointer(links)
	record.LinkDevice, record.LinkInode, record.LinkCountForLink = uintPointer(linkDevice), uintPointer(linkInode), uintPointer(linkLinks)
	return record, nil
}

func uintPointer(value sql.NullInt64) *uint64 {
	if !value.Valid || value.Int64 < 0 {
		return nil
	}
	converted := uint64(value.Int64)
	return &converted
}

func validateTrashIdentity(record TrashRecord) error {
	for _, value := range []*uint64{record.Device, record.Inode, record.LinkCount, record.LinkDevice, record.LinkInode, record.LinkCountForLink} {
		if value != nil && *value > maxInt64Value {
			return errors.New("trash identity exceeds SQLite integer range")
		}
	}
	if record.Symlink && !record.LinkIdentityAvailable {
		return errors.New("trash symbolic link identity is required")
	}
	if !record.Symlink && !record.IdentityAvailable {
		return errors.New("trash file identity is required")
	}
	return nil
}

func validTrashKind(kind string) bool {
	return kind == "file" || kind == "directory" || kind == "symlink"
}

func validTrashPath(value string) bool {
	if value == "" || strings.Contains(value, `\`) || strings.HasPrefix(value, "/") || filepath.IsAbs(value) {
		return false
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || component == "." || component == ".." {
			return false
		}
	}
	return true
}

func validTrashStatus(status TrashStatus) bool {
	switch status {
	case TrashMoving, TrashTrashed, TrashRestoring, TrashPurging, TrashNeedsAttention, TrashRestored, TrashPurged:
		return true
	default:
		return false
	}
}

// AppendAudit 仅保存流程元数据和结果说明，不记录文件内容或备份。
func (s *Store) AppendAudit(ctx context.Context, record AuditRecord) (int64, error) {
	return appendAuditRecord(ctx, s.db, record)
}

func appendAuditRecord(ctx context.Context, execer storeExecer, record AuditRecord) (int64, error) {
	if record.EventType == "" || record.Actor == "" || record.Outcome == "" {
		return 0, errors.New("audit event type, actor, and outcome are required")
	}
	result, err := execer.ExecContext(ctx, `
		INSERT INTO audit_events (
			task_id, plan_id, operation_id, event_type, actor, outcome,
			message, source_path, target_path, created_at_ns
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		nullableString(record.TaskID), nullableString(record.PlanID),
		nullableString(record.OperationID), record.EventType, record.Actor,
		record.Outcome, record.Message, record.SourcePath, record.TargetPath,
		timestampOrNow(record.CreatedAt),
	)
	if err != nil {
		return 0, fmt.Errorf("append audit record: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("read audit record ID: %w", err)
	}
	return id, nil
}

func (s *Store) AuditForTask(ctx context.Context, taskID string) ([]AuditRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, COALESCE(task_id, ''), COALESCE(plan_id, ''),
			COALESCE(operation_id, ''), event_type, actor, outcome, message,
			source_path, target_path, created_at_ns
		FROM audit_events WHERE task_id = ? ORDER BY id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("read task audit records: %w", err)
	}
	defer rows.Close()
	var records []AuditRecord
	for rows.Next() {
		var record AuditRecord
		var createdAt int64
		if err := rows.Scan(
			&record.ID, &record.TaskID, &record.PlanID, &record.OperationID,
			&record.EventType, &record.Actor, &record.Outcome, &record.Message,
			&record.SourcePath, &record.TargetPath, &createdAt,
		); err != nil {
			return nil, fmt.Errorf("scan audit record: %w", err)
		}
		record.CreatedAt = time.Unix(0, createdAt).UTC()
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate task audit records: %w", err)
	}
	return records, nil
}

// AuditForTaskPage 返回任务存在性校验后的审计记录页。
func (s *Store) AuditForTaskPage(ctx context.Context, taskID string, limit, offset int) (TaskAuditPage, error) {
	if taskID == "" {
		return TaskAuditPage{}, errors.New("task ID is required")
	}
	if err := validateTaskPage(limit, offset); err != nil {
		return TaskAuditPage{}, err
	}
	if limit == 0 {
		limit = defaultTaskPageLimit
	}

	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tasks WHERE id = ?)`, taskID).Scan(&exists); err != nil {
		return TaskAuditPage{}, fmt.Errorf("check task for audit: %w", err)
	}
	if exists == 0 {
		return TaskAuditPage{}, ErrNotFound
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, COALESCE(task_id, ''), COALESCE(plan_id, ''),
			COALESCE(operation_id, ''), event_type, actor, outcome, message,
			source_path, target_path, created_at_ns
		FROM audit_events WHERE task_id = ?
		ORDER BY created_at_ns DESC, id DESC LIMIT ? OFFSET ?`,
		taskID, limit+1, offset,
	)
	if err != nil {
		return TaskAuditPage{}, fmt.Errorf("read task audit page: %w", err)
	}
	defer rows.Close()

	page := TaskAuditPage{Records: make([]AuditRecord, 0, limit), Limit: limit, Offset: offset}
	for rows.Next() {
		var record AuditRecord
		var createdAt int64
		if err := rows.Scan(
			&record.ID, &record.TaskID, &record.PlanID, &record.OperationID,
			&record.EventType, &record.Actor, &record.Outcome, &record.Message,
			&record.SourcePath, &record.TargetPath, &createdAt,
		); err != nil {
			return TaskAuditPage{}, fmt.Errorf("scan task audit page: %w", err)
		}
		record.CreatedAt = time.Unix(0, createdAt).UTC()
		page.Records = append(page.Records, record)
	}
	if err := rows.Err(); err != nil {
		return TaskAuditPage{}, fmt.Errorf("iterate task audit page: %w", err)
	}
	if len(page.Records) > limit {
		page.Records = page.Records[:limit]
		page.HasMore = true
	}
	return page, nil
}

func validTaskStatus(status TaskStatus) bool {
	switch status {
	case TaskQueued, TaskAnalyzing, TaskAwaitingApproval, TaskRunning, TaskPaused,
		TaskSucceeded, TaskPartial, TaskFailed, TaskCancelled:
		return true
	default:
		return false
	}
}

func validPlanStatus(status PlanStatus) bool {
	switch status {
	case PlanDraft, PlanAwaitingApproval, PlanApproved, PlanExecuting,
		PlanCompleted, PlanFailed, PlanInvalidated, PlanRejected, PlanCancelled:
		return true
	default:
		return false
	}
}

func validApprovalState(state ApprovalState) bool {
	switch state {
	case ApprovalPending, ApprovalApproved, ApprovalRejected, ApprovalInvalidated:
		return true
	default:
		return false
	}
}

func validOperationStatus(status OperationStatus) bool {
	switch status {
	case OperationPending, OperationRunning, OperationSucceeded, OperationFailed,
		OperationSkipped, OperationPartial, OperationCancelled:
		return true
	default:
		return false
	}
}

func timestampOrNow(value time.Time) int64 {
	if value.IsZero() {
		return time.Now().UTC().UnixNano()
	}
	return value.UTC().UnixNano()
}

func zeroTimeValue(value time.Time) int64 {
	if value.IsZero() {
		return 0
	}
	return value.UTC().UnixNano()
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableUint(value *uint64) any {
	if value == nil {
		return nil
	}
	return int64(*value)
}

func isSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') && (char < 'A' || char > 'F') {
			return false
		}
	}
	return true
}

func jsonObject(value string) bool {
	trimmed := strings.TrimSpace(value)
	return strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}") && json.Valid([]byte(trimmed))
}

func requireOneRow(result sql.Result, kind string) error {
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count updated %s rows: %w", kind, err)
	}
	if rows == 0 {
		return fmt.Errorf("%s: %w", kind, ErrNotFound)
	}
	return nil
}
