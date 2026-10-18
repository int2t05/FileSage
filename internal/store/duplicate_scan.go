// 本文件保存重复扫描任务、进度与完整结果。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrDuplicateScanUnavailable = errors.New("duplicate scan results are unavailable")
	ErrDuplicateScanCancelled   = errors.New("duplicate scan was cancelled before start")
)

// DuplicateScanStatus 表示扫描任务状态。
type DuplicateScanStatus string

const (
	// DuplicateScanQueued 等待扫描开始。
	DuplicateScanQueued DuplicateScanStatus = "queued"
	// DuplicateScanRunning 正在遍历或计算摘要。
	DuplicateScanRunning DuplicateScanStatus = "running"
	// DuplicateScanSucceeded 已完整完成并发布结果。
	DuplicateScanSucceeded DuplicateScanStatus = "succeeded"
	// DuplicateScanFailed 未完成且不保留部分结果。
	DuplicateScanFailed DuplicateScanStatus = "failed"
	// DuplicateScanCancelled 已取消且不保留部分结果。
	DuplicateScanCancelled DuplicateScanStatus = "cancelled"
)

// DuplicateScan 保存扫描历史和可恢复的运行状态。
type DuplicateScan struct {
	ID                 string
	TaskID             string
	ConversationID     string
	ScopePath          string
	Status             DuplicateScanStatus
	Phase              string
	EntriesVisited     int
	CandidateFiles     int
	FilesHashed        int
	BytesHashed        int64
	TotalBytes         int64
	GroupCount         int
	DuplicateFileCount int
	ReclaimableBytes   int64
	ErrorCode          string
	CancelRequested    bool
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// DuplicateScanProgress 保存扫描阶段和单调递增计数。
type DuplicateScanProgress struct {
	Phase          string
	EntriesVisited int
	CandidateFiles int
	FilesHashed    int
	BytesHashed    int64
	TotalBytes     int64
}

// DuplicateScanResultFile 保存报告中的路径、大小和修改时间。
type DuplicateScanResultFile struct {
	Path       string
	SizeBytes  int64
	ModifiedAt time.Time
}

// DuplicateScanResultGroup 保存完整校验后的一组重复文件。
type DuplicateScanResultGroup struct {
	SizeBytes int64
	Files     []DuplicateScanResultFile
}

// DuplicateScanGroup 是不含内容摘要的结果组摘要。
type DuplicateScanGroup struct {
	ID             int64
	SizeBytes      int64
	DuplicateCount int
}

// DuplicateScanFile 是不含内容摘要的报告文件项。
type DuplicateScanFile struct {
	Path       string
	SizeBytes  int64
	ModifiedAt time.Time
}

// DuplicateScanGroupPage 保存有界的结果组分页。
type DuplicateScanGroupPage struct {
	Groups  []DuplicateScanGroup
	Limit   int
	Offset  int
	HasMore bool
}

// DuplicateScanFilePage 保存有界的组内文件分页。
type DuplicateScanFilePage struct {
	Files   []DuplicateScanFile
	Limit   int
	Offset  int
	HasMore bool
}

// CreateDuplicateScan 原子创建任务、扫描记录与会话关联。
func (s *Store) CreateDuplicateScan(ctx context.Context, scan DuplicateScan, task TaskRecord) error {
	if scan.ID == "" || scan.TaskID != task.ID || scan.ConversationID == "" || task.Kind != "duplicate_scan" || task.Status != TaskQueued || scan.Status != DuplicateScanQueued || scan.Phase != "enumerating" || !validDuplicateScopePath(scan.ScopePath) {
		return errors.New("invalid duplicate scan")
	}
	now := timestampOrNow(scan.CreatedAt)
	updated := timestampOrNow(scan.UpdatedAt)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin duplicate scan: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := putTaskRecord(ctx, tx, task); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO duplicate_scan_runs (
		id, task_id, conversation_id, scope_path, status, phase, created_at_ns, updated_at_ns
	) VALUES (?, ?, ?, ?, 'queued', 'enumerating', ?, ?)`, scan.ID, scan.TaskID, scan.ConversationID, scan.ScopePath, now, updated); err != nil {
		return fmt.Errorf("insert duplicate scan: %w", err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE agent_conversations SET duplicate_scan_id = ?, updated_at_ns = ?
		WHERE id = ? AND state = 'analyzing' AND plan_id IS NULL`, scan.ID, updated, scan.ConversationID)
	if err != nil {
		return fmt.Errorf("link duplicate scan to conversation: %w", err)
	}
	if err := requireOneRow(result, "agent conversation"); err != nil {
		return ErrConversationConflict
	}
	if _, err := appendAuditRecord(ctx, tx, AuditRecord{
		TaskID: scan.TaskID, EventType: "duplicate_scan_queued", Actor: "agent", Outcome: "queued",
		Message: "已创建重复文件扫描任务", SourcePath: scan.ScopePath, CreatedAt: scan.CreatedAt,
	}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit duplicate scan: %w", err)
	}
	return nil
}

// StartDuplicateScan 登记扫描开始，并只记录一次状态审计。
func (s *Store) StartDuplicateScan(ctx context.Context, id string, at time.Time) error {
	atNS := timestampOrNow(at)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin duplicate scan start: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE duplicate_scan_runs SET status = 'running', updated_at_ns = ?
		WHERE id = ? AND status = 'queued' AND cancel_requested = 0`, atNS, id)
	if err != nil {
		return fmt.Errorf("start duplicate scan: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check duplicate scan start: %w", err)
	}
	if count == 0 {
		var status DuplicateScanStatus
		var cancelRequested int
		if err := tx.QueryRowContext(ctx, `SELECT status, cancel_requested FROM duplicate_scan_runs WHERE id = ?`, id).Scan(&status, &cancelRequested); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		} else if status == DuplicateScanQueued && cancelRequested != 0 {
			return ErrDuplicateScanCancelled
		} else if status != DuplicateScanRunning {
			return ErrConflict
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET status = 'running', updated_at_ns = ? WHERE id = ? AND status = 'queued'`, atNS, id); err != nil {
		return fmt.Errorf("start duplicate scan task: %w", err)
	}
	if count == 1 {
		if _, err := appendAuditRecord(ctx, tx, AuditRecord{
			TaskID: id, EventType: "duplicate_scan_started", Actor: "agent", Outcome: "running",
			Message: "重复文件扫描已开始", CreatedAt: at,
		}); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit duplicate scan start: %w", err)
	}
	return nil
}

// UpdateDuplicateScanProgress 保存单调递增的扫描进度，不逐文件写审计。
func (s *Store) UpdateDuplicateScanProgress(ctx context.Context, id string, progress DuplicateScanProgress, at time.Time) error {
	if !validDuplicateScanPhase(progress.Phase) || progress.EntriesVisited < 0 || progress.CandidateFiles < 0 || progress.FilesHashed < 0 || progress.BytesHashed < 0 || progress.TotalBytes < 0 {
		return errors.New("invalid duplicate scan progress")
	}
	_, err := s.db.ExecContext(ctx, `UPDATE duplicate_scan_runs SET
		phase = CASE
			WHEN phase = 'enumerating' AND ? IN ('hashing', 'complete') THEN ?
			WHEN phase = 'hashing' AND ? = 'complete' THEN ?
			ELSE phase END,
		entries_visited = MAX(entries_visited, ?),
		candidate_files = MAX(candidate_files, ?),
		files_hashed = MAX(files_hashed, ?),
		bytes_hashed = MAX(bytes_hashed, ?),
		total_bytes = MAX(total_bytes, ?),
		updated_at_ns = MAX(updated_at_ns, ?)
		WHERE id = ? AND status = 'running'`,
		progress.Phase, progress.Phase, progress.Phase, progress.Phase,
		progress.EntriesVisited, progress.CandidateFiles, progress.FilesHashed,
		progress.BytesHashed, progress.TotalBytes, timestampOrNow(at), id)
	if err != nil {
		return fmt.Errorf("update duplicate scan progress: %w", err)
	}
	return nil
}

// RequestDuplicateScanCancel 以持久化标记请求取消。
func (s *Store) RequestDuplicateScanCancel(ctx context.Context, id string, at time.Time) (DuplicateScan, error) {
	_, err := s.db.ExecContext(ctx, `UPDATE duplicate_scan_runs SET cancel_requested = 1, updated_at_ns = MAX(updated_at_ns, ?)
		WHERE id = ? AND status IN ('queued', 'running')`, timestampOrNow(at), id)
	if err != nil {
		return DuplicateScan{}, fmt.Errorf("request duplicate scan cancellation: %w", err)
	}
	return s.DuplicateScan(ctx, id)
}

// FinishDuplicateScan 发布完整成功结果，失败和取消始终清除结果。
func (s *Store) FinishDuplicateScan(ctx context.Context, id, status, errorCode string, progress DuplicateScanProgress, groups []DuplicateScanResultGroup, reply string, at time.Time) error {
	if status != string(DuplicateScanSucceeded) && status != string(DuplicateScanFailed) && status != string(DuplicateScanCancelled) {
		return errors.New("invalid duplicate scan terminal status")
	}
	if !validDuplicateScanPhase(progress.Phase) || len(errorCode) > 64 || strings.ContainsRune(errorCode, 0) || len(reply) > 4096 {
		return errors.New("invalid duplicate scan result")
	}
	if status == string(DuplicateScanSucceeded) && (errorCode != "" || progress.Phase != "complete") {
		return errors.New("successful duplicate scan must be complete")
	}
	if status != string(DuplicateScanSucceeded) && len(groups) != 0 {
		return errors.New("failed duplicate scan cannot publish results")
	}
	atNS := timestampOrNow(at)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin duplicate scan finish: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var taskID, conversationID string
	var cancelRequested int
	if err := tx.QueryRowContext(ctx, `SELECT task_id, conversation_id, cancel_requested FROM duplicate_scan_runs WHERE id = ? AND status IN ('queued', 'running')`, id).Scan(&taskID, &conversationID, &cancelRequested); errors.Is(err, sql.ErrNoRows) {
		return ErrConflict
	} else if err != nil {
		return fmt.Errorf("read duplicate scan finish target: %w", err)
	}
	if status == string(DuplicateScanSucceeded) && cancelRequested != 0 {
		status = string(DuplicateScanCancelled)
		errorCode = "cancelled"
		groups = nil
		reply = "扫描已取消，未保留部分结果，也未修改文件。"
	}
	groupCount, fileCount, reclaimable, err := persistDuplicateGroups(ctx, tx, id, groups, status == string(DuplicateScanSucceeded))
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE duplicate_scan_runs SET status = ?, phase = ?,
		entries_visited = MAX(entries_visited, ?), candidate_files = MAX(candidate_files, ?),
		files_hashed = MAX(files_hashed, ?), bytes_hashed = MAX(bytes_hashed, ?), total_bytes = MAX(total_bytes, ?),
		group_count = ?, duplicate_file_count = ?, reclaimable_bytes = ?, error_code = ?, updated_at_ns = ?
		WHERE id = ? AND status IN ('queued', 'running')`,
		status, progress.Phase, progress.EntriesVisited, progress.CandidateFiles, progress.FilesHashed,
		progress.BytesHashed, progress.TotalBytes, groupCount, fileCount, reclaimable, errorCode, atNS, id)
	if err != nil {
		return fmt.Errorf("finish duplicate scan state: %w", err)
	}
	if err := requireOneRow(result, "duplicate scan"); err != nil {
		return ErrConflict
	}
	taskStatus := TaskFailed
	if status == string(DuplicateScanSucceeded) {
		taskStatus = TaskSucceeded
	} else if status == string(DuplicateScanCancelled) {
		taskStatus = TaskCancelled
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET status = ?, updated_at_ns = ? WHERE id = ?`, taskStatus, atNS, taskID); err != nil {
		return fmt.Errorf("finish duplicate scan task: %w", err)
	}
	conversationState := ConversationFailed
	if status == string(DuplicateScanSucceeded) {
		conversationState = ConversationAnalysisComplete
	}
	result, err = tx.ExecContext(ctx, `UPDATE agent_conversations SET state = ?, error_code = ?, updated_at_ns = ?
		WHERE id = ? AND duplicate_scan_id = ? AND state = 'analyzing'`, conversationState, errorCode, atNS, conversationID, id)
	if err != nil {
		return fmt.Errorf("finish duplicate scan conversation: %w", err)
	}
	if err := requireOneRow(result, "agent conversation"); err != nil {
		return ErrConversationConflict
	}
	var messageCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_conversation_messages WHERE conversation_id = ?`, conversationID).Scan(&messageCount); err != nil {
		return fmt.Errorf("count duplicate scan messages: %w", err)
	}
	if reply != "" && messageCount < maxConversationMessages {
		if err := s.insertConversationMessage(ctx, tx, conversationID, messageCount+1, "assistant", reply, at); err != nil {
			return err
		}
	}
	outcome, eventType, auditMessage := status, "duplicate_scan_finished", "重复文件扫描已结束"
	if status == string(DuplicateScanSucceeded) {
		auditMessage = "重复文件扫描已完成"
	} else if status == string(DuplicateScanCancelled) {
		auditMessage = "重复文件扫描已取消"
	}
	if _, err := appendAuditRecord(ctx, tx, AuditRecord{
		TaskID: taskID, EventType: eventType, Actor: "agent", Outcome: outcome,
		Message: auditMessage, CreatedAt: at,
	}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit duplicate scan finish: %w", err)
	}
	return nil
}

// RecoverDuplicateScans 丢弃重启前未完成任务的所有结果。
func (s *Store) RecoverDuplicateScans(ctx context.Context, at time.Time) error {
	atNS := timestampOrNow(at)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin duplicate scan recovery: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT id, task_id, conversation_id FROM duplicate_scan_runs WHERE status IN ('queued', 'running')`)
	if err != nil {
		return fmt.Errorf("list interrupted duplicate scans: %w", err)
	}
	type interrupted struct{ id, taskID, conversationID string }
	var scans []interrupted
	for rows.Next() {
		var scan interrupted
		if err := rows.Scan(&scan.id, &scan.taskID, &scan.conversationID); err != nil {
			_ = rows.Close()
			return fmt.Errorf("read interrupted duplicate scan: %w", err)
		}
		scans = append(scans, scan)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate interrupted duplicate scans: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close interrupted duplicate scans: %w", err)
	}
	for _, scan := range scans {
		if _, err := tx.ExecContext(ctx, `DELETE FROM duplicate_scan_groups WHERE scan_id = ?`, scan.id); err != nil {
			return fmt.Errorf("discard interrupted duplicate groups: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE duplicate_scan_runs SET status = 'failed', error_code = 'service_restarted',
			group_count = 0, duplicate_file_count = 0, reclaimable_bytes = 0, updated_at_ns = ? WHERE id = ?`, atNS, scan.id); err != nil {
			return fmt.Errorf("fail interrupted duplicate scan: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET status = 'failed', updated_at_ns = ? WHERE id = ?`, atNS, scan.taskID); err != nil {
			return fmt.Errorf("fail interrupted duplicate scan task: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE agent_conversations SET state = 'failed', error_code = 'service_restarted', updated_at_ns = ?
			WHERE id = ? AND duplicate_scan_id = ? AND state = 'analyzing'`, atNS, scan.conversationID, scan.id); err != nil {
			return fmt.Errorf("recover interrupted duplicate scan conversation: %w", err)
		}
		if _, err := appendAuditRecord(ctx, tx, AuditRecord{
			TaskID: scan.taskID, EventType: "duplicate_scan_finished", Actor: "system", Outcome: "failed",
			Message: "服务重启后未发布未完成的扫描结果", CreatedAt: at,
		}); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit duplicate scan recovery: %w", err)
	}
	return nil
}

// DuplicateScan 返回扫描状态与进度，不包含文件正文或摘要。
func (s *Store) DuplicateScan(ctx context.Context, id string) (DuplicateScan, error) {
	var scan DuplicateScan
	var createdAt, updatedAt int64
	var cancelRequested int
	err := s.db.QueryRowContext(ctx, `SELECT id, task_id, conversation_id, scope_path, status, phase,
		entries_visited, candidate_files, files_hashed, bytes_hashed, total_bytes,
		group_count, duplicate_file_count, reclaimable_bytes, error_code, cancel_requested, created_at_ns, updated_at_ns
		FROM duplicate_scan_runs WHERE id = ?`, id).Scan(
		&scan.ID, &scan.TaskID, &scan.ConversationID, &scan.ScopePath, &scan.Status, &scan.Phase,
		&scan.EntriesVisited, &scan.CandidateFiles, &scan.FilesHashed, &scan.BytesHashed, &scan.TotalBytes,
		&scan.GroupCount, &scan.DuplicateFileCount, &scan.ReclaimableBytes, &scan.ErrorCode, &cancelRequested, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return DuplicateScan{}, ErrNotFound
	}
	if err != nil {
		return DuplicateScan{}, fmt.Errorf("read duplicate scan: %w", err)
	}
	scan.CancelRequested = cancelRequested != 0
	scan.CreatedAt = time.Unix(0, createdAt).UTC()
	scan.UpdatedAt = time.Unix(0, updatedAt).UTC()
	return scan, nil
}

// ListDuplicateScanGroups 仅在扫描成功后返回有界结果页。
func (s *Store) ListDuplicateScanGroups(ctx context.Context, id string, limit, offset int) (DuplicateScanGroupPage, error) {
	if err := validDuplicateScanPage(limit, offset); err != nil {
		return DuplicateScanGroupPage{}, err
	}
	if err := requireCompletedDuplicateScan(ctx, s.db, id); err != nil {
		return DuplicateScanGroupPage{}, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT group_id, size_bytes, duplicate_count FROM duplicate_scan_groups
		WHERE scan_id = ? ORDER BY group_id LIMIT ? OFFSET ?`, id, limit+1, offset)
	if err != nil {
		return DuplicateScanGroupPage{}, fmt.Errorf("list duplicate groups: %w", err)
	}
	defer rows.Close()
	page := DuplicateScanGroupPage{Groups: make([]DuplicateScanGroup, 0, limit), Limit: limit, Offset: offset}
	for rows.Next() {
		var group DuplicateScanGroup
		if err := rows.Scan(&group.ID, &group.SizeBytes, &group.DuplicateCount); err != nil {
			return DuplicateScanGroupPage{}, fmt.Errorf("read duplicate group: %w", err)
		}
		page.Groups = append(page.Groups, group)
	}
	if err := rows.Err(); err != nil {
		return DuplicateScanGroupPage{}, fmt.Errorf("iterate duplicate groups: %w", err)
	}
	if len(page.Groups) > limit {
		page.Groups = page.Groups[:limit]
		page.HasMore = true
	}
	return page, nil
}

// ListDuplicateScanFiles 仅在扫描成功后返回组内文件页。
func (s *Store) ListDuplicateScanFiles(ctx context.Context, id string, groupID int64, limit, offset int) (DuplicateScanFilePage, error) {
	if groupID < 1 || validDuplicateScanPage(limit, offset) != nil {
		return DuplicateScanFilePage{}, errors.New("invalid duplicate scan file page")
	}
	if err := requireCompletedDuplicateScan(ctx, s.db, id); err != nil {
		return DuplicateScanFilePage{}, err
	}
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM duplicate_scan_groups WHERE scan_id = ? AND group_id = ?)`, id, groupID).Scan(&exists); err != nil {
		return DuplicateScanFilePage{}, fmt.Errorf("check duplicate group: %w", err)
	}
	if exists == 0 {
		return DuplicateScanFilePage{}, ErrNotFound
	}
	rows, err := s.db.QueryContext(ctx, `SELECT path, size_bytes, modified_at_ns FROM duplicate_scan_files
		WHERE scan_id = ? AND group_id = ? ORDER BY path LIMIT ? OFFSET ?`, id, groupID, limit+1, offset)
	if err != nil {
		return DuplicateScanFilePage{}, fmt.Errorf("list duplicate files: %w", err)
	}
	defer rows.Close()
	page := DuplicateScanFilePage{Files: make([]DuplicateScanFile, 0, limit), Limit: limit, Offset: offset}
	for rows.Next() {
		var file DuplicateScanFile
		var modifiedAt int64
		if err := rows.Scan(&file.Path, &file.SizeBytes, &modifiedAt); err != nil {
			return DuplicateScanFilePage{}, fmt.Errorf("read duplicate file: %w", err)
		}
		file.ModifiedAt = time.Unix(0, modifiedAt).UTC()
		page.Files = append(page.Files, file)
	}
	if err := rows.Err(); err != nil {
		return DuplicateScanFilePage{}, fmt.Errorf("iterate duplicate files: %w", err)
	}
	if len(page.Files) > limit {
		page.Files = page.Files[:limit]
		page.HasMore = true
	}
	return page, nil
}

// DuplicateScanFilesByPaths 核对一组候选路径仍属于指定扫描组。
func (s *Store) DuplicateScanFilesByPaths(ctx context.Context, id string, groupID int64, paths []string) ([]DuplicateScanFile, error) {
	if groupID < 1 || len(paths) < 2 || len(paths) > 21 {
		return nil, errors.New("invalid duplicate scan selection")
	}
	if err := requireCompletedDuplicateScan(ctx, s.db, id); err != nil {
		return nil, err
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(paths)), ",")
	args := make([]any, 0, len(paths)+2)
	args = append(args, id, groupID)
	for _, value := range paths {
		if !validDuplicateFilePath(value) {
			return nil, errors.New("invalid duplicate scan path")
		}
		args = append(args, value)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT path, size_bytes, modified_at_ns FROM duplicate_scan_files
		WHERE scan_id = ? AND group_id = ? AND path IN (`+placeholders+`) ORDER BY path`, args...)
	if err != nil {
		return nil, fmt.Errorf("verify duplicate scan selection: %w", err)
	}
	defer rows.Close()
	files := make([]DuplicateScanFile, 0, len(paths))
	for rows.Next() {
		var file DuplicateScanFile
		var modifiedAt int64
		if err := rows.Scan(&file.Path, &file.SizeBytes, &modifiedAt); err != nil {
			return nil, fmt.Errorf("read duplicate scan selection: %w", err)
		}
		file.ModifiedAt = time.Unix(0, modifiedAt).UTC()
		files = append(files, file)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate duplicate scan selection: %w", err)
	}
	return files, nil
}

func persistDuplicateGroups(ctx context.Context, tx *sql.Tx, id string, groups []DuplicateScanResultGroup, publish bool) (int, int, int64, error) {
	if _, err := tx.ExecContext(ctx, `DELETE FROM duplicate_scan_groups WHERE scan_id = ?`, id); err != nil {
		return 0, 0, 0, fmt.Errorf("clear duplicate scan results: %w", err)
	}
	if !publish {
		return 0, 0, 0, nil
	}
	if len(groups) > 50_000 {
		return 0, 0, 0, errors.New("too many duplicate groups")
	}
	groupCount, fileCount := 0, 0
	var reclaimable int64
	for index, group := range groups {
		if group.SizeBytes < 0 || len(group.Files) < 2 || len(group.Files) > 100_000 || index >= 50_000 || fileCount+len(group.Files) > 100_000 {
			return 0, 0, 0, errors.New("invalid duplicate group")
		}
		deduplicated := int64(len(group.Files) - 1)
		if group.SizeBytes != 0 && deduplicated > (int64(^uint64(0)>>1)-reclaimable)/group.SizeBytes {
			return 0, 0, 0, errors.New("duplicate reclaimable size overflow")
		}
		reclaimable += group.SizeBytes * deduplicated
		groupID := index + 1
		if _, err := tx.ExecContext(ctx, `INSERT INTO duplicate_scan_groups(scan_id, group_id, size_bytes, duplicate_count) VALUES (?, ?, ?, ?)`, id, groupID, group.SizeBytes, len(group.Files)); err != nil {
			return 0, 0, 0, fmt.Errorf("save duplicate group: %w", err)
		}
		for _, file := range group.Files {
			if !validDuplicateFilePath(file.Path) || file.SizeBytes != group.SizeBytes {
				return 0, 0, 0, errors.New("invalid duplicate file")
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO duplicate_scan_files(scan_id, group_id, path, size_bytes, modified_at_ns) VALUES (?, ?, ?, ?, ?)`, id, groupID, file.Path, file.SizeBytes, zeroTimeValue(file.ModifiedAt)); err != nil {
				return 0, 0, 0, fmt.Errorf("save duplicate file: %w", err)
			}
			fileCount++
		}
		groupCount++
	}
	return groupCount, fileCount, reclaimable, nil
}

func requireCompletedDuplicateScan(ctx context.Context, queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id string) error {
	var status DuplicateScanStatus
	if err := queryer.QueryRowContext(ctx, `SELECT status FROM duplicate_scan_runs WHERE id = ?`, id).Scan(&status); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	} else if status != DuplicateScanSucceeded {
		return ErrDuplicateScanUnavailable
	}
	return nil
}

func validDuplicateScanPage(limit, offset int) error {
	if limit < 1 || limit > 100 || offset < 0 || offset > 100_000 {
		return errors.New("invalid duplicate scan page")
	}
	return nil
}

func validDuplicateScanPhase(phase string) bool {
	return phase == "enumerating" || phase == "hashing" || phase == "complete"
}

func validDuplicateScopePath(value string) bool {
	return value == "" || validDuplicateFilePath(value)
}

func validDuplicateFilePath(value string) bool {
	if value == "" || len(value) > 4096 || strings.ContainsAny(value, "\\\x00") || strings.HasPrefix(value, "/") {
		return false
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || component == "." || component == ".." || len(component) > 255 {
			return false
		}
	}
	return true
}
