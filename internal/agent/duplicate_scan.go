// 本文件管理已确认的异步重复文件扫描。
package agent

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"filesage/internal/duplicates"
	"filesage/internal/fsroot"
	"filesage/internal/store"
)

type duplicateScanManager struct {
	store  *store.Store
	root   *fsroot.Root
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	active map[string]*duplicateScanJob
	closed bool
	wg     sync.WaitGroup
}

type duplicateScanJob struct {
	cancel     context.CancelFunc
	mu         sync.Mutex
	persistMu  sync.Mutex
	progress   duplicates.Progress
	lastSaved  time.Time
	lastPhase  string
	persistErr error
}

type DuplicateCleanupRequest struct {
	GroupID    int64
	KeeperPath string
	TrashPaths []string
}

func newDuplicateScanManager(metadata *store.Store, root *fsroot.Root) *duplicateScanManager {
	ctx, cancel := context.WithCancel(context.Background())
	return &duplicateScanManager{store: metadata, root: root, ctx: ctx, cancel: cancel, active: make(map[string]*duplicateScanJob)}
}

func (m *duplicateScanManager) Start(ctx context.Context, conversation store.AgentConversation, goal, scopePath string) (string, error) {
	if m == nil || m.store == nil || m.root == nil || conversation.ID == "" || goal == "" || !validScopePath(scopePath) {
		return "", ErrInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || len(m.active) != 0 {
		return "", ErrConflict
	}
	id, err := randomID()
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	task := store.TaskRecord{
		ID: id, Kind: "duplicate_scan", Status: store.TaskQueued,
		RequestText: goal, ScopePath: scopePath, CreatedAt: now, UpdatedAt: now,
	}
	scan := store.DuplicateScan{
		ID: id, TaskID: id, ConversationID: conversation.ID, ScopePath: scopePath,
		Status: store.DuplicateScanQueued, Phase: "enumerating", CreatedAt: now, UpdatedAt: now,
	}
	if err := m.store.CreateDuplicateScan(ctx, scan, task); err != nil {
		return "", err
	}
	jobCtx, cancel := context.WithCancel(m.ctx)
	job := &duplicateScanJob{cancel: cancel, progress: duplicates.Progress{Phase: "enumerating"}}
	m.active[id] = job
	m.wg.Add(1)
	go m.run(jobCtx, id, scopePath, job)
	return id, nil
}

func (m *duplicateScanManager) run(ctx context.Context, id, scopePath string, job *duplicateScanJob) {
	defer m.wg.Done()
	defer func() {
		job.cancel()
		m.mu.Lock()
		delete(m.active, id)
		m.mu.Unlock()
	}()
	if err := m.store.StartDuplicateScan(ctx, id, time.Now().UTC()); err != nil {
		logDuplicateScanStartFailure(slog.Default(), id, err)
		progress := duplicates.Progress{Phase: "enumerating"}
		status, errorCode := string(store.DuplicateScanFailed), "scan_start_failed"
		if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, store.ErrDuplicateScanCancelled) {
			status, errorCode = string(store.DuplicateScanCancelled), "cancelled"
		}
		if finishErr := m.store.FinishDuplicateScan(context.Background(), id, status, errorCode, toStoreDuplicateProgress(progress), nil, duplicateScanReply(status, errorCode, 0, 0), time.Now().UTC()); finishErr != nil {
			slog.Error("cannot fail queued duplicate scan", "scan_id", id, "error", finishErr)
		}
		return
	}
	var scopes []string
	if scopePath != "" {
		scopes = []string{scopePath}
	}
	groups, scanErr := duplicates.Scan(ctx, m.root, scopes, duplicates.Options{}, func(progress duplicates.Progress) {
		m.saveProgress(ctx, id, job, progress)
	})
	job.mu.Lock()
	progress, persistErr := job.progress, job.persistErr
	job.mu.Unlock()
	status, errorCode := string(store.DuplicateScanSucceeded), ""
	var results []store.DuplicateScanResultGroup
	if persistErr != nil {
		status, errorCode = string(store.DuplicateScanFailed), "storage_failed"
	} else if errors.Is(scanErr, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		status, errorCode = string(store.DuplicateScanCancelled), "cancelled"
	} else if scanErr != nil {
		status, errorCode = string(store.DuplicateScanFailed), duplicateScanErrorCode(scanErr)
	} else {
		if !validDuplicateResults(groups) {
			status, errorCode = string(store.DuplicateScanFailed), "invalid_result_path"
		} else {
			progress.Phase = "complete"
			results = make([]store.DuplicateScanResultGroup, 0, len(groups))
			for _, group := range groups {
				files := make([]store.DuplicateScanResultFile, 0, len(group.Files))
				for _, file := range group.Files {
					files = append(files, store.DuplicateScanResultFile{Path: file.Path, SizeBytes: file.Size, ModifiedAt: file.ModifiedAt})
				}
				results = append(results, store.DuplicateScanResultGroup{SizeBytes: group.Size, Files: files})
			}
		}
	}
	if progress.Phase == "" {
		progress.Phase = "enumerating"
	}
	reply := duplicateScanReply(status, errorCode, len(results), duplicateFileCount(results))
	if err := m.store.FinishDuplicateScan(context.Background(), id, status, errorCode, toStoreDuplicateProgress(progress), results, reply, time.Now().UTC()); err != nil {
		slog.Error("cannot finish duplicate scan", "scan_id", id, "error", err)
	}
}

// logDuplicateScanStartFailure 将预期取消与真实存储故障分开记录。
func logDuplicateScanStartFailure(logger *slog.Logger, id string, err error) {
	level, reason := slog.LevelError, "scan_start_failed"
	if errors.Is(err, context.Canceled) || errors.Is(err, store.ErrDuplicateScanCancelled) {
		level, reason = slog.LevelInfo, "cancelled"
	}
	logger.LogAttrs(context.Background(), level, "duplicate scan start failed",
		slog.String("scan_id", id), slog.String("reason", reason))
}

func (m *duplicateScanManager) saveProgress(ctx context.Context, id string, job *duplicateScanJob, progress duplicates.Progress) {
	now := time.Now().UTC()
	job.mu.Lock()
	job.progress = progress
	if now.Sub(job.lastSaved) < 300*time.Millisecond && progress.Phase == job.lastPhase {
		job.mu.Unlock()
		return
	}
	job.lastSaved, job.lastPhase = now, progress.Phase
	job.mu.Unlock()
	job.persistMu.Lock()
	err := m.store.UpdateDuplicateScanProgress(ctx, id, toStoreDuplicateProgress(progress), now)
	job.persistMu.Unlock()
	if err != nil && ctx.Err() == nil {
		job.mu.Lock()
		if job.persistErr == nil {
			job.persistErr = err
		}
		job.mu.Unlock()
		job.cancel()
	}
}

func (m *duplicateScanManager) Cancel(ctx context.Context, id string) (store.DuplicateScan, error) {
	scan, err := m.store.RequestDuplicateScanCancel(ctx, id, time.Now().UTC())
	if err != nil {
		return store.DuplicateScan{}, err
	}
	if scan.CancelRequested {
		m.mu.Lock()
		job := m.active[id]
		m.mu.Unlock()
		if job != nil {
			job.cancel()
		}
	}
	return scan, nil
}

func (m *duplicateScanManager) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	if !m.closed {
		m.closed = true
		m.cancel()
		for _, job := range m.active {
			job.cancel()
		}
	}
	m.mu.Unlock()
	m.wg.Wait()
}

func (m *duplicateScanManager) GetDuplicateScan(ctx context.Context, id string) (store.DuplicateScan, error) {
	return m.store.DuplicateScan(ctx, id)
}

func (m *duplicateScanManager) ListDuplicateGroups(ctx context.Context, id string, limit, offset int) (store.DuplicateScanGroupPage, error) {
	return m.store.ListDuplicateScanGroups(ctx, id, limit, offset)
}

func (m *duplicateScanManager) ListDuplicateFiles(ctx context.Context, id string, groupID int64, limit, offset int) (store.DuplicateScanFilePage, error) {
	return m.store.ListDuplicateScanFiles(ctx, id, groupID, limit, offset)
}

// GetDuplicateScan 返回扫描状态和进度。
func (s *ConversationService) GetDuplicateScan(ctx context.Context, id string) (store.DuplicateScan, error) {
	if s.duplicateScan == nil {
		return store.DuplicateScan{}, ErrUnsupported
	}
	return s.duplicateScan.GetDuplicateScan(ctx, id)
}

// CancelDuplicateScan 请求取消正在运行的扫描。
func (s *ConversationService) CancelDuplicateScan(ctx context.Context, id string) (store.DuplicateScan, error) {
	if s.duplicateScan == nil {
		return store.DuplicateScan{}, ErrUnsupported
	}
	return s.duplicateScan.Cancel(ctx, id)
}

// ListDuplicateGroups 返回成功扫描的结果组页。
func (s *ConversationService) ListDuplicateGroups(ctx context.Context, id string, limit, offset int) (store.DuplicateScanGroupPage, error) {
	if s.duplicateScan == nil {
		return store.DuplicateScanGroupPage{}, ErrUnsupported
	}
	return s.duplicateScan.ListDuplicateGroups(ctx, id, limit, offset)
}

// ListDuplicateFiles 返回成功扫描的组内文件页。
func (s *ConversationService) ListDuplicateFiles(ctx context.Context, id string, groupID int64, limit, offset int) (store.DuplicateScanFilePage, error) {
	if s.duplicateScan == nil {
		return store.DuplicateScanFilePage{}, ErrUnsupported
	}
	return s.duplicateScan.ListDuplicateFiles(ctx, id, groupID, limit, offset)
}

// CreateDuplicateCleanupPlan 只为用户选择的副本创建待审批回收方案。
func (s *ConversationService) CreateDuplicateCleanupPlan(ctx context.Context, scanID string, request DuplicateCleanupRequest) (store.PlanRecord, error) {
	if s == nil || s.store == nil || s.plans == nil || request.GroupID < 1 || len(request.TrashPaths) < 1 || len(request.TrashPaths) > 20 {
		return store.PlanRecord{}, ErrInvalid
	}
	scan, err := s.store.DuplicateScan(ctx, scanID)
	if err != nil {
		return store.PlanRecord{}, err
	}
	if scan.Status != store.DuplicateScanSucceeded {
		return store.PlanRecord{}, store.ErrDuplicateScanUnavailable
	}
	if !cleanPath(request.KeeperPath, false) || reservedPath(request.KeeperPath) || !withinScope(scan.ScopePath, request.KeeperPath) {
		return store.PlanRecord{}, ErrInvalid
	}
	selected := make([]string, 0, len(request.TrashPaths)+1)
	selected = append(selected, request.KeeperPath)
	seen := map[string]struct{}{request.KeeperPath: {}}
	for _, relativePath := range request.TrashPaths {
		if !cleanPath(relativePath, false) || reservedPath(relativePath) || !withinScope(scan.ScopePath, relativePath) {
			return store.PlanRecord{}, ErrInvalid
		}
		if _, exists := seen[relativePath]; exists {
			return store.PlanRecord{}, ErrInvalid
		}
		seen[relativePath] = struct{}{}
		selected = append(selected, relativePath)
	}
	files, err := s.store.DuplicateScanFilesByPaths(ctx, scanID, request.GroupID, selected)
	if err != nil {
		return store.PlanRecord{}, err
	}
	if len(files) != len(selected) {
		return store.PlanRecord{}, store.ErrDuplicateScanUnavailable
	}
	requestText := "重复文件整理：保留 " + request.KeeperPath + "，将 " + strconv.Itoa(len(request.TrashPaths)) + " 个选定副本移入回收站"
	proposals := make([]Proposal, 0, len(request.TrashPaths))
	for _, relativePath := range request.TrashPaths {
		proposals = append(proposals, Proposal{
			RequestText: requestText, ScopePath: scan.ScopePath, Action: "trash_duplicate",
			SourcePath: relativePath, TargetPath: request.KeeperPath,
			Reason:          "你已选择保留同组文件，并明确勾选此副本移入回收站。",
			Evidence:        "方案生成时服务端会重新核对扫描组、文件状态与内容。",
			DuplicateScanID: scanID, DuplicateGroupID: request.GroupID,
		})
	}
	return s.plans.ProposeMany(ctx, proposals)
}

func toStoreDuplicateProgress(progress duplicates.Progress) store.DuplicateScanProgress {
	return store.DuplicateScanProgress{
		Phase: progress.Phase, EntriesVisited: progress.EntriesVisited, CandidateFiles: progress.CandidateFiles,
		FilesHashed: progress.FilesHashed, BytesHashed: progress.BytesHashed, TotalBytes: progress.TotalBytes,
	}
}

func duplicateScanErrorCode(err error) string {
	switch {
	case errors.Is(err, duplicates.ErrFileChanged):
		return "file_changed"
	case errors.Is(err, duplicates.ErrLimitExceeded):
		return "scan_limit_exceeded"
	case errors.Is(err, duplicates.ErrUnavailable):
		return "filesystem_identity_unavailable"
	case errors.Is(err, duplicates.ErrInvalidScope):
		return "invalid_scope"
	default:
		return "scan_failed"
	}
}

func duplicateScanReply(status, errorCode string, groupCount, fileCount int) string {
	if status == string(store.DuplicateScanSucceeded) {
		return "扫描完成，共发现 " + strconv.Itoa(groupCount) + " 组重复文件（" + strconv.Itoa(fileCount) + " 个文件）。结果仅供审查，未修改文件。"
	}
	if status == string(store.DuplicateScanCancelled) {
		return "扫描已取消，未保留部分结果，也未修改文件。"
	}
	switch errorCode {
	case "file_changed":
		return "扫描范围内的文件或目录发生变化，已丢弃全部结果；请确认文件稳定后重试。"
	case "scan_limit_exceeded":
		return "扫描范围超过当前安全限制，已丢弃全部结果；请缩小范围后重试。"
	case "service_restarted":
		return "服务重启中断了扫描，未保留部分结果；请重新发起扫描。"
	default:
		return "扫描未能完成，未保留部分结果；请检查范围后重试。"
	}
}

func duplicateFileCount(groups []store.DuplicateScanResultGroup) int {
	total := 0
	for _, group := range groups {
		total += len(group.Files)
	}
	return total
}

func validDuplicateResults(groups []duplicates.Group) bool {
	if len(groups) > 50_000 {
		return false
	}
	files := 0
	for _, group := range groups {
		files += len(group.Files)
		if files > 100_000 || group.Size < 0 || len(group.Files) < 2 {
			return false
		}
		for _, file := range group.Files {
			if !safeConversationPath(file.Path) || file.Size != group.Size {
				return false
			}
		}
	}
	return true
}

func validScopePath(value string) bool {
	if value == "" {
		return true
	}
	return safeConversationPath(value)
}
