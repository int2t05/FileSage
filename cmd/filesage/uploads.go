// 本文件连接 tus 暂存、文件系统发布及上传结果查询。
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"filesage/internal/coordination"
	"filesage/internal/fsroot"
	"filesage/internal/origincheck"
	"filesage/internal/store"
	"filesage/internal/upload"
)

const uploadStatusPath = "/api/v1/uploads/status/"
const uploadTUSPath = "/api/v1/uploads/"

type publicationStatus string

const (
	publicationReceiving  publicationStatus = "receiving"
	publicationQueued     publicationStatus = "queued"
	publicationRunning    publicationStatus = "publishing"
	publicationSucceeded  publicationStatus = "published"
	publicationPartial    publicationStatus = "partial"
	publicationFailed     publicationStatus = "failed"
	publicationCancelling publicationStatus = "cancelling"
	publicationCancelled  publicationStatus = "cancelled"
)

type publicationResult struct {
	UploadID           string            `json:"uploadId"`
	Status             publicationStatus `json:"status"`
	Size               int64             `json:"size"`
	Offset             int64             `json:"offset"`
	TargetPath         string            `json:"targetPath"`
	CandidatePath      string            `json:"candidatePath,omitempty"`
	PublishedPath      string            `json:"publishedPath,omitempty"`
	SHA256             string            `json:"sha256,omitempty"`
	Error              string            `json:"error,omitempty"`
	CreatedDirectories []string          `json:"createdDirectories,omitempty"`
	UpdatedAt          time.Time         `json:"updatedAt"`
}

type publicationManager struct {
	uploads      *upload.Server
	root         *fsroot.Root
	locks        *coordination.Locker
	store        *store.Store
	refreshIndex func(context.Context)

	mu          sync.Mutex
	persistMu   sync.Mutex
	results     map[string]publicationResult
	cancel      map[string]context.CancelFunc
	workerDone  map[string]chan struct{}
	terminating map[string]bool
	terminated  map[string]bool
	scheduled   map[string]bool
	jobs        chan upload.Event
	ctx         context.Context
}

func newPublicationManager(uploads *upload.Server, root *fsroot.Root, locks *coordination.Locker, metadata *store.Store) *publicationManager {
	if locks == nil {
		locks = coordination.New()
	}
	manager := &publicationManager{
		uploads:     uploads,
		root:        root,
		locks:       locks,
		store:       metadata,
		results:     make(map[string]publicationResult),
		cancel:      make(map[string]context.CancelFunc),
		workerDone:  make(map[string]chan struct{}),
		terminating: make(map[string]bool),
		terminated:  make(map[string]bool),
		scheduled:   make(map[string]bool),
		jobs:        make(chan upload.Event, 64),
	}
	if uploads != nil {
		uploads.SetTerminateHook(manager.cancelInFlight)
	}
	return manager
}

// SetIndexRefresh 注册发布后的索引校准回调。
func (manager *publicationManager) SetIndexRefresh(refresh func(context.Context)) {
	manager.refreshIndex = refresh
}

// tusHandler 用持久发布状态恢复已清理上传的 TUS 确认。
func (manager *publicationManager) tusHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead && r.Header.Get("Tus-Resumable") == "1.0.0" {
			uploadID := strings.TrimPrefix(r.URL.Path, uploadTUSPath)
			if safeUploadID(uploadID) {
				result, err := manager.loadResult(uploadID)
				if err != nil && !errors.Is(err, store.ErrNotFound) {
					slog.Warn("upload publication state unavailable", "upload_id", uploadID, "error", err)
					response := httptest.NewRecorder()
					next.ServeHTTP(response, r)
					if response.Code != http.StatusNotFound {
						copyTUSHeadResponse(w, response)
						return
					}
					writeTUSUnavailable(w)
					return
				}
				if err == nil && isPublishedResult(result) {
					if result.Size < 0 {
						slog.Warn("published upload has invalid size", "upload_id", uploadID)
						writeTUSUnavailable(w)
						return
					}
					w.Header().Set("Tus-Resumable", "1.0.0")
					w.Header().Set("Upload-Offset", strconv.FormatInt(result.Size, 10))
					w.Header().Set("Upload-Length", strconv.FormatInt(result.Size, 10))
					w.Header().Set("Cache-Control", "no-store")
					w.WriteHeader(http.StatusOK)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// copyTUSHeadResponse 转发活动上传资源的状态与响应头。
func copyTUSHeadResponse(w http.ResponseWriter, response *httptest.ResponseRecorder) {
	for key, values := range response.Header() {
		w.Header()[key] = append([]string(nil), values...)
	}
	w.WriteHeader(response.Code)
}

// writeTUSUnavailable 返回可重试错误，避免把暂时不可读的状态误判为不存在。
func writeTUSUnavailable(w http.ResponseWriter) {
	w.Header().Set("Tus-Resumable", "1.0.0")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusServiceUnavailable)
}

func (manager *publicationManager) run(ctx context.Context) error {
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	manager.mu.Lock()
	manager.ctx = runCtx
	manager.mu.Unlock()

	const workerCount = 3
	var workers sync.WaitGroup
	for range workerCount {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-runCtx.Done():
					return
				case event := <-manager.jobs:
					manager.publish(runCtx, event)
				}
			}
		}()
	}

	if err := manager.recoverCompleted(runCtx); err != nil {
		stop()
		workers.Wait()
		return err
	}

	events := manager.uploads.Events()
	for {
		select {
		case <-runCtx.Done():
			stop()
			workers.Wait()
			return runCtx.Err()
		case event, ok := <-events:
			if !ok {
				stop()
				workers.Wait()
				return nil
			}
			switch event.Kind {
			case upload.UploadCreated:
				manager.recordReceiving(event)
			case upload.UploadCompleted:
				manager.enqueue(ctx, event)
			case upload.UploadTerminated:
				manager.recordTermination(event)
			}
		}
	}
}

func (manager *publicationManager) recoverCompleted(ctx context.Context) error {
	events, err := manager.uploads.CompletedUploads()
	if err != nil {
		return err
	}
	for _, event := range events {
		result, loadErr := manager.loadResult(event.UploadID)
		if loadErr == nil && result.Status == publicationSucceeded {
			if err := manager.uploads.DiscardCompleted(event); err != nil {
				slog.Warn("published upload staging cleanup failed", "upload_id", event.UploadID, "reason", uploadLogReason(err))
			}
			continue
		}
		manager.enqueue(ctx, event)
	}
	return nil
}

func (manager *publicationManager) enqueue(ctx context.Context, event upload.Event) {
	manager.mu.Lock()
	if manager.scheduled[event.UploadID] {
		manager.mu.Unlock()
		return
	}
	manager.scheduled[event.UploadID] = true
	manager.mu.Unlock()

	result := newPublicationResult(event)
	result.Status = publicationQueued
	if prior, err := manager.loadResult(event.UploadID); err == nil {
		result.CandidatePath = prior.CandidatePath
		result.CreatedDirectories = append([]string(nil), prior.CreatedDirectories...)
	}
	if err := manager.persist(result); err != nil {
		slog.Error("cannot persist queued upload", "upload_id", event.UploadID, "reason", uploadLogReason(err))
		manager.clearScheduled(event.UploadID)
		return
	}
	select {
	case manager.jobs <- event:
	case <-ctx.Done():
		manager.clearScheduled(event.UploadID)
	}
}

func (manager *publicationManager) recordReceiving(event upload.Event) {
	manager.persistMu.Lock()
	defer manager.persistMu.Unlock()
	current, err := manager.loadResult(event.UploadID)
	if err == nil && current.Status != publicationReceiving {
		return
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		slog.Error("cannot inspect upload state before recording reception", "upload_id", event.UploadID, "reason", uploadLogReason(err))
		return
	}
	result := newPublicationResult(event)
	result.Status = publicationReceiving
	if err := manager.persistLocked(result); err != nil {
		slog.Error("cannot persist upload reception state", "upload_id", event.UploadID, "reason", uploadLogReason(err))
	}
}

func (manager *publicationManager) recordTermination(event upload.Event) {
	if current, err := manager.loadResult(event.UploadID); err == nil && isPublishedResult(current) {
		return
	}
	manager.mu.Lock()
	manager.terminating[event.UploadID] = false
	manager.terminated[event.UploadID] = true
	cancel := manager.cancel[event.UploadID]
	manager.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	result := newPublicationResult(event)
	result.Status = publicationCancelled
	result.Error = "上传已取消，暂存数据已移除。"
	if err := manager.persist(result); err != nil {
		slog.Error("cannot persist upload cancellation", "upload_id", event.UploadID, "reason", uploadLogReason(err))
	}
}

func (manager *publicationManager) cancelInFlight(uploadID string) {
	if current, err := manager.loadResult(uploadID); err == nil && isPublishedResult(current) {
		return
	}
	manager.mu.Lock()
	manager.terminating[uploadID] = true
	cancel := manager.cancel[uploadID]
	done := manager.workerDone[uploadID]
	manager.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (manager *publicationManager) publish(parent context.Context, event upload.Event) {
	defer manager.clearScheduled(event.UploadID)
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	manager.mu.Lock()
	if manager.terminating[event.UploadID] || manager.terminated[event.UploadID] {
		manager.mu.Unlock()
		cancel()
		return
	}
	manager.cancel[event.UploadID] = cancel
	manager.workerDone[event.UploadID] = done
	manager.mu.Unlock()
	defer func() {
		cancel()
		manager.mu.Lock()
		delete(manager.cancel, event.UploadID)
		delete(manager.workerDone, event.UploadID)
		manager.mu.Unlock()
		close(done)
	}()

	prior, _ := manager.loadResult(event.UploadID)
	if prior.Status == publicationCancelled {
		return
	}
	relativeTarget := path.Join(event.Metadata.Target, event.Metadata.RelativePath)
	result := newPublicationResult(event)
	result.Status = publicationRunning
	if prior.CandidatePath != "" && candidateAllowed(relativeTarget, prior.CandidatePath) {
		result.CandidatePath = prior.CandidatePath
		result.CreatedDirectories = append([]string(nil), prior.CreatedDirectories...)
	}

	result.TargetPath = relativeTarget
	keys := uploadLockKeys(relativeTarget)
	release, err := manager.locks.Lock(ctx, keys...)
	if err != nil {
		manager.finishFailure(event, result, err, ctx)
		return
	}
	defer release()

	if err := validateRealDirectory(manager.root, event.Metadata.Target); err != nil {
		manager.finishFailure(event, result, err, ctx)
		return
	}
	created, err := ensureRelativeParentDirectories(manager.root, event.Metadata.Target, path.Dir(event.Metadata.RelativePath))
	result.CreatedDirectories = append(result.CreatedDirectories, created...)
	if err != nil {
		manager.finishFailure(event, result, err, ctx)
		return
	}

	if prior.CandidatePath != "" {
		if digest, err := manager.stagedHash(event); err == nil {
			if candidateHash, ok := hashRegularFile(manager.root, prior.CandidatePath); ok && candidateHash == digest {
				result.Status = publicationSucceeded
				result.CandidatePath = prior.CandidatePath
				result.PublishedPath = prior.CandidatePath
				result.SHA256 = digest
				result.Error = ""
				if err := manager.persist(result); err != nil {
					slog.Error("cannot persist recovered upload publication", "upload_id", event.UploadID, "reason", uploadLogReason(err))
					return
				}
				manager.cleanupPublished(event)
				return
			}
		}
	}

	parentPath := path.Dir(relativeTarget)
	filename := path.Base(relativeTarget)
	for suffix := 1; suffix <= 10000; suffix++ {
		candidateName := filename
		if suffix > 1 {
			candidateName = numberedFileName(filename, suffix)
		}
		candidate := candidateName
		if parentPath != "." {
			candidate = path.Join(parentPath, candidateName)
		}
		if pathExists(manager.root, candidate) {
			result.CandidatePath = ""
			if err := manager.persist(result); err != nil {
				slog.Error("cannot clear occupied upload candidate", "upload_id", event.UploadID, "reason", uploadLogReason(err))
				return
			}
			continue
		}
		result.CandidatePath = candidate
		result.Status = publicationRunning
		result.Error = ""
		if err := manager.persist(result); err != nil {
			slog.Error("cannot persist upload candidate before publication", "upload_id", event.UploadID, "reason", uploadLogReason(err))
			return
		}

		reader, metadata, size, err := manager.uploads.OpenCompleted(event)
		if err != nil {
			manager.finishFailure(event, result, err, ctx)
			return
		}
		if metadata != event.Metadata || size != event.Size {
			_ = reader.Close()
			manager.finishFailure(event, result, errors.New("staged upload changed before publication"), ctx)
			return
		}
		digest, writeErr := manager.root.WriteNewAtomicFrom(candidate, contextReader{ctx: ctx, source: reader}, size, 0o644)
		closeErr := reader.Close()
		if closeErr != nil {
			slog.Warn("cannot close staged upload reader", "upload_id", event.UploadID, "reason", uploadLogReason(closeErr))
		}
		if (writeErr == nil || errors.Is(writeErr, fsroot.ErrPublishedNotDurable)) && manager.refreshIndex != nil {
			manager.refreshIndex(context.Background())
		}
		if errors.Is(writeErr, fsroot.ErrConflict) {
			result.CandidatePath = ""
			if err := manager.persist(result); err != nil {
				slog.Error("cannot clear conflicted upload candidate", "upload_id", event.UploadID, "reason", uploadLogReason(err))
				return
			}
			continue
		}
		if writeErr != nil {
			if errors.Is(writeErr, fsroot.ErrPublishedNotDurable) {
				result.Status = publicationPartial
				result.PublishedPath = candidate
				result.SHA256 = digest
				result.Error = "文件已发布，但目录同步失败；请检查目标目录后再处理。"
				if err := manager.persist(result); err != nil {
					slog.Error("cannot persist partially durable upload", "upload_id", event.UploadID, "reason", uploadLogReason(err))
				}
				return
			}
			manager.finishFailure(event, result, writeErr, ctx)
			return
		}

		result.Status = publicationSucceeded
		result.PublishedPath = candidate
		result.SHA256 = digest
		result.Error = ""
		if err := manager.persist(result); err != nil {
			slog.Error("upload published but result persistence failed", "upload_id", event.UploadID, "reason", uploadLogReason(err))
			return
		}
		manager.cleanupPublished(event)
		return
	}

	manager.finishFailure(event, result, errors.New("no available file name after 10000 conflicts"), ctx)
}

func (manager *publicationManager) finishFailure(event upload.Event, result publicationResult, cause error, ctx context.Context) {
	if ctx.Err() != nil {
		manager.mu.Lock()
		terminating := manager.terminating[event.UploadID]
		terminated := manager.terminated[event.UploadID]
		manager.mu.Unlock()
		if terminating && !terminated {
			if manager.completedStageAvailable(event) {
				result.Status = publicationCancelled
				result.Error = "发布已取消，完整暂存文件仍保留，可重试。"
				if err := manager.persist(result); err != nil {
					slog.Error("cannot persist upload cancellation while deletion is pending", "upload_id", event.UploadID, "reason", uploadLogReason(err))
				}
			}
			return
		}
		result.Status = publicationCancelled
		if terminated {
			result.Error = "上传已取消，暂存数据已移除。"
		} else if manager.completedStageAvailable(event) {
			result.Error = "发布已取消，完整暂存文件仍保留，可重试。"
		} else {
			result.Error = "发布已取消，完整暂存文件未能确认保留。"
		}
	} else if len(result.CreatedDirectories) > 0 {
		result.Status = publicationPartial
		result.Error = manager.safePublicationError(cause)
	} else {
		result.Status = publicationFailed
		result.Error = manager.safePublicationError(cause)
	}
	if err := manager.persist(result); err != nil {
		slog.Error("cannot persist upload publication failure", "upload_id", event.UploadID, "reason", uploadLogReason(err), "cause_reason", uploadLogReason(cause))
	}
	if result.Status != publicationCancelled {
		slog.Error("upload publication failed", "upload_id", event.UploadID, "status", result.Status, "reason", uploadLogReason(cause))
	}
}

func (manager *publicationManager) cleanupPublished(event upload.Event) {
	if err := manager.uploads.DiscardCompleted(event); err != nil {
		slog.Warn("published upload staging cleanup failed", "upload_id", event.UploadID, "reason", uploadLogReason(err))
	}
}

func (manager *publicationManager) completedStageAvailable(event upload.Event) bool {
	reader, _, _, err := manager.uploads.OpenCompleted(event)
	if err != nil {
		return false
	}
	return reader.Close() == nil
}

func isPublishedResult(result publicationResult) bool {
	return result.Status == publicationSucceeded || (result.Status == publicationPartial && result.PublishedPath != "")
}

func (manager *publicationManager) stagedHash(event upload.Event) (string, error) {
	reader, _, _, err := manager.uploads.OpenCompleted(event)
	if err != nil {
		return "", err
	}
	defer reader.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, reader); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func (manager *publicationManager) persist(result publicationResult) error {
	manager.persistMu.Lock()
	defer manager.persistMu.Unlock()
	return manager.persistLocked(result)
}

func (manager *publicationManager) persistLocked(result publicationResult) error {
	previous, previousErr := manager.loadResult(result.UploadID)
	if previousErr != nil && !errors.Is(previousErr, store.ErrNotFound) {
		return fmt.Errorf("read prior upload publication state: %w", previousErr)
	}
	if previousErr == nil && isPublishedResult(previous) && !isPublishedResult(result) {
		return nil
	}
	if previousErr == nil && previous.Status == publicationCancelled && strings.Contains(previous.Error, "暂存数据已移除") && !isPublishedResult(result) {
		return nil
	}
	manager.mu.Lock()
	terminated := manager.terminated[result.UploadID]
	manager.mu.Unlock()
	if terminated && (errors.Is(previousErr, store.ErrNotFound) || !isPublishedResult(previous)) && !isPublishedResult(result) {
		result.Status = publicationCancelled
		result.Error = "上传已取消，暂存数据已移除。"
	}
	result.UpdatedAt = time.Now().UTC()
	data, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("encode upload publication state: %w", err)
	}
	status, err := taskStatusForPublication(result.Status)
	if err != nil {
		return err
	}
	stateChanged := errors.Is(previousErr, store.ErrNotFound) || previous.Status != result.Status || previous.Error != result.Error || previous.PublishedPath != result.PublishedPath
	task := store.TaskRecord{
		ID:          result.UploadID,
		Kind:        "upload",
		Status:      status,
		RequestText: string(data),
		ScopePath:   result.TargetPath,
		UpdatedAt:   result.UpdatedAt,
	}
	if stateChanged {
		targetPath := result.PublishedPath
		if targetPath == "" {
			targetPath = result.CandidatePath
		}
		if err := manager.store.PutTaskAndAudit(context.Background(), task, store.AuditRecord{
			TaskID:     result.UploadID,
			EventType:  "upload.publication",
			Actor:      "system",
			Outcome:    string(result.Status),
			Message:    result.Error,
			TargetPath: targetPath,
			CreatedAt:  result.UpdatedAt,
		}); err != nil {
			return fmt.Errorf("persist upload publication state and audit event: %w", err)
		}
	} else if err := manager.store.PutTask(context.Background(), task); err != nil {
		return fmt.Errorf("persist upload publication state: %w", err)
	}
	manager.mu.Lock()
	manager.results[result.UploadID] = result
	manager.mu.Unlock()
	return nil
}

func (manager *publicationManager) loadResult(uploadID string) (publicationResult, error) {
	manager.mu.Lock()
	cached, ok := manager.results[uploadID]
	manager.mu.Unlock()
	if ok {
		return cached, nil
	}
	record, err := manager.store.Task(context.Background(), uploadID)
	if err != nil {
		return publicationResult{}, err
	}
	if record.Kind != "upload" {
		return publicationResult{}, store.ErrNotFound
	}
	var result publicationResult
	if err := json.Unmarshal([]byte(record.RequestText), &result); err != nil || result.UploadID != uploadID {
		return publicationResult{}, errors.New("stored upload publication state is invalid")
	}
	manager.mu.Lock()
	manager.results[uploadID] = result
	manager.mu.Unlock()
	return result, nil
}

func (manager *publicationManager) clearScheduled(uploadID string) {
	manager.mu.Lock()
	delete(manager.scheduled, uploadID)
	manager.mu.Unlock()
}

func (manager *publicationManager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	remaining := strings.TrimPrefix(r.URL.Path, uploadStatusPath)
	parts := strings.Split(strings.TrimSuffix(remaining, "/"), "/")
	if len(parts) < 1 || !safeUploadID(parts[0]) {
		writeUploadStatusError(w, http.StatusBadRequest, "invalid upload ID")
		return
	}
	if len(parts) == 1 && r.Method == http.MethodGet {
		manager.serveStatus(w, r, parts[0])
		return
	}
	if len(parts) == 2 && parts[1] == "retry" && r.Method == http.MethodPost {
		if !origincheck.AllowsMutation(r) {
			writeUploadStatusError(w, http.StatusForbidden, "cross-origin upload changes are not allowed")
			return
		}
		manager.retry(w, r, parts[0])
		return
	}
	writeUploadStatusError(w, http.StatusNotFound, "upload status endpoint not found")
}

func (manager *publicationManager) serveStatus(w http.ResponseWriter, r *http.Request, uploadID string) {
	result, err := manager.loadResult(uploadID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeUploadStatusError(w, http.StatusInternalServerError, "upload status is unavailable")
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		event, transferErr := manager.uploads.TransferStatus(uploadID)
		if transferErr != nil {
			manager.mu.Lock()
			cancelling := manager.terminating[uploadID] || manager.terminated[uploadID]
			manager.mu.Unlock()
			if cancelling {
				writeUploadStatusJSON(w, http.StatusOK, publicationResult{
					UploadID:  uploadID,
					Status:    publicationCancelling,
					Error:     "取消仍在处理中，等待服务端确认最终状态。",
					UpdatedAt: time.Now().UTC(),
				})
				return
			}
			writeUploadStatusError(w, http.StatusNotFound, "upload was not found")
			return
		}
		result = newPublicationResult(event)
		if event.Kind == upload.UploadCompleted {
			result.Status = publicationQueued
		} else {
			result.Status = publicationReceiving
		}
	}
	if result.Status == publicationReceiving {
		if event, transferErr := manager.uploads.TransferStatus(uploadID); transferErr == nil {
			result.Size = event.Size
			result.Offset = event.Offset
			if event.Kind == upload.UploadCompleted {
				result.Status = publicationQueued
			}
		}
	}
	writeUploadStatusJSON(w, http.StatusOK, result)
}

func (manager *publicationManager) retry(w http.ResponseWriter, r *http.Request, uploadID string) {
	event, err := manager.uploads.TransferStatus(uploadID)
	if err != nil || event.Kind != upload.UploadCompleted {
		writeUploadStatusError(w, http.StatusConflict, "only a complete staged upload can be retried")
		return
	}
	current, err := manager.loadResult(uploadID)
	if err == nil && (current.Status == publicationSucceeded || (current.Status == publicationPartial && current.PublishedPath != "")) {
		writeUploadStatusError(w, http.StatusConflict, "this upload already has a published result; refresh its status")
		return
	}
	manager.mu.Lock()
	if manager.scheduled[uploadID] {
		manager.mu.Unlock()
		writeUploadStatusJSON(w, http.StatusAccepted, current)
		return
	}
	manager.scheduled[uploadID] = true
	ctx := manager.ctx
	manager.mu.Unlock()
	if ctx == nil {
		manager.clearScheduled(uploadID)
		writeUploadStatusError(w, http.StatusServiceUnavailable, "upload publisher is not running")
		return
	}
	result := newPublicationResult(event)
	result.Status = publicationQueued
	result.Error = ""
	result.CandidatePath = current.CandidatePath
	result.CreatedDirectories = append([]string(nil), current.CreatedDirectories...)
	if err := manager.persist(result); err != nil {
		manager.clearScheduled(uploadID)
		writeUploadStatusError(w, http.StatusServiceUnavailable, "upload retry could not be recorded")
		return
	}
	select {
	case manager.jobs <- event:
		writeUploadStatusJSON(w, http.StatusAccepted, result)
	case <-r.Context().Done():
		manager.clearScheduled(uploadID)
		writeUploadStatusError(w, http.StatusRequestTimeout, "upload retry was cancelled")
	case <-ctx.Done():
		manager.clearScheduled(uploadID)
		writeUploadStatusError(w, http.StatusServiceUnavailable, "upload publisher is stopping")
	}
}

func uploadLockKeys(relativeTarget string) []string {
	return coordination.PathKeys(relativeTarget)
}

func validateRealDirectory(root *fsroot.Root, relativePath string) error {
	if relativePath == "." || relativePath == "" {
		return nil
	}
	current := ""
	for _, component := range strings.Split(relativePath, "/") {
		next := path.Join(current, component)
		entries, err := root.List(current)
		if err != nil {
			return fmt.Errorf("inspect target directory %q: %w", current, err)
		}
		found := false
		for _, entry := range entries {
			if entry.Path != next {
				continue
			}
			if entry.Symlink {
				return fmt.Errorf("target directory %q traverses a symbolic link: %w", next, fsroot.ErrSymlinkPath)
			}
			if entry.Kind != fsroot.KindDirectory {
				return fmt.Errorf("target path %q: %w", next, fsroot.ErrNotDirectory)
			}
			found = true
			break
		}
		if !found {
			return fmt.Errorf("target directory %q: %w", next, fs.ErrNotExist)
		}
		current = next
	}
	return nil
}

func pathExists(root *fsroot.Root, relativePath string) bool {
	parent := path.Dir(relativePath)
	if parent == "." {
		parent = ""
	}
	entries, err := root.List(parent)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.Path == relativePath {
			return true
		}
	}
	return false
}

func candidateAllowed(target, candidate string) bool {
	if candidate == "" || path.IsAbs(candidate) || path.Clean(candidate) != candidate || strings.Contains(candidate, "\\") || path.Dir(target) != path.Dir(candidate) {
		return false
	}
	wanted := path.Base(target)
	if path.Base(candidate) == wanted {
		return true
	}
	for suffix := 2; suffix <= 10000; suffix++ {
		if path.Base(candidate) == numberedFileName(wanted, suffix) {
			return true
		}
	}
	return false
}

func ensureRelativeParentDirectories(root *fsroot.Root, target, relativeParent string) ([]string, error) {
	if relativeParent == "." || relativeParent == "" {
		return nil, nil
	}
	current := target
	if current == "." {
		current = ""
	}
	created := make([]string, 0)
	for _, component := range strings.Split(relativeParent, "/") {
		next := path.Join(current, component)
		entries, err := root.List(current)
		if err != nil {
			return created, fmt.Errorf("inspect upload directory %q: %w", current, err)
		}
		found := false
		for _, entry := range entries {
			if entry.Path != next {
				continue
			}
			if entry.Symlink {
				return created, fmt.Errorf("upload directory %q traverses a symbolic link: %w", next, fsroot.ErrSymlinkPath)
			}
			if entry.Kind != fsroot.KindDirectory {
				return created, fmt.Errorf("upload path %q: %w", next, fsroot.ErrNotDirectory)
			}
			found = true
			break
		}
		if !found {
			if err := root.Mkdir(next, 0o755); err != nil {
				if !errors.Is(err, fsroot.ErrConflict) {
					return created, fmt.Errorf("create upload directory %q: %w", next, err)
				}
				entries, listErr := root.List(current)
				if listErr != nil {
					return created, fmt.Errorf("recheck upload directory %q: %w", next, listErr)
				}
				for _, entry := range entries {
					if entry.Path != next {
						continue
					}
					if entry.Symlink {
						return created, fmt.Errorf("upload directory %q traverses a symbolic link: %w", next, fsroot.ErrSymlinkPath)
					}
					if entry.Kind != fsroot.KindDirectory {
						return created, fmt.Errorf("upload path %q: %w", next, fsroot.ErrNotDirectory)
					}
					found = true
					break
				}
				if !found {
					return created, fmt.Errorf("upload directory changed concurrently %q: %w", next, err)
				}
			} else {
				created = append(created, next)
			}
		}
		current = next
	}
	return created, nil
}

func numberedFileName(filename string, suffix int) string {
	extension := path.Ext(filename)
	if extension == filename {
		extension = ""
	}
	stem := strings.TrimSuffix(filename, extension)
	return fmt.Sprintf("%s (%d)%s", stem, suffix, extension)
}

func hashRegularFile(root *fsroot.Root, relativePath string) (string, bool) {
	file, entry, err := root.OpenRead(relativePath)
	if err != nil || entry.Kind != fsroot.KindFile || entry.Symlink {
		if file != nil {
			_ = file.Close()
		}
		return "", false
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", false
	}
	return hex.EncodeToString(digest.Sum(nil)), true
}

type contextReader struct {
	ctx    context.Context
	source io.Reader
}

func (reader contextReader) Read(target []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.source.Read(target)
}

func newPublicationResult(event upload.Event) publicationResult {
	return publicationResult{
		UploadID:   event.UploadID,
		Status:     publicationQueued,
		Size:       event.Size,
		Offset:     event.Offset,
		TargetPath: path.Join(event.Metadata.Target, event.Metadata.RelativePath),
	}
}

func taskStatusForPublication(status publicationStatus) (store.TaskStatus, error) {
	switch status {
	case publicationReceiving:
		return store.TaskRunning, nil
	case publicationQueued:
		return store.TaskQueued, nil
	case publicationRunning:
		return store.TaskRunning, nil
	case publicationSucceeded:
		return store.TaskSucceeded, nil
	case publicationPartial:
		return store.TaskPartial, nil
	case publicationFailed:
		return store.TaskFailed, nil
	case publicationCancelled:
		return store.TaskCancelled, nil
	default:
		return "", fmt.Errorf("unsupported upload publication state %q", status)
	}
}

func (manager *publicationManager) safePublicationError(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "发布已取消，完整暂存文件仍保留。"
	case errors.Is(err, fs.ErrPermission):
		return "目标目录或暂存文件权限不足。"
	case errors.Is(err, fs.ErrNotExist):
		return "暂存文件或目标目录不存在。"
	case errors.Is(err, fsroot.ErrSymlinkPath), errors.Is(err, fsroot.ErrOutsideRoot), errors.Is(err, fsroot.ErrInvalidPath):
		return "目标路径不安全或包含软链接。"
	case errors.Is(err, fsroot.ErrNoReplaceUnsupported):
		return "当前文件系统不支持安全的禁止覆盖发布。"
	case errors.Is(err, fsroot.ErrSizeMismatch):
		return "暂存文件长度与接收记录不一致。"
	case errors.Is(err, fsroot.ErrConflict):
		return "目标状态发生变化，发布未完成。"
	case errors.Is(err, fsroot.ErrNotDirectory):
		return "目标路径中的目录不存在或不是文件夹。"
	case errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EDQUOT):
		return "服务器存储空间或配额不足，文件未发布；完整暂存文件保留，清理空间或调整配额后可重试。"
	default:
		return "发布失败：" + strings.TrimSpace(manager.uploads.SafeErrorMessage(err))
	}
}

// uploadLogReason 将底层错误归类，避免把用户路径写入系统日志。
func uploadLogReason(err error) string {
	switch {
	case err == nil:
		return "unknown"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, fs.ErrPermission):
		return "permission"
	case errors.Is(err, fs.ErrNotExist):
		return "missing_state"
	case errors.Is(err, fsroot.ErrSymlinkPath), errors.Is(err, fsroot.ErrOutsideRoot), errors.Is(err, fsroot.ErrInvalidPath):
		return "unsafe_path"
	case errors.Is(err, fsroot.ErrConflict):
		return "conflict"
	case errors.Is(err, fsroot.ErrSizeMismatch):
		return "size_mismatch"
	case errors.Is(err, fsroot.ErrPublishedNotDurable):
		return "published_not_durable"
	case errors.Is(err, fsroot.ErrNoReplaceUnsupported):
		return "atomic_publish_unsupported"
	case errors.Is(err, fsroot.ErrNotDirectory):
		return "not_directory"
	case errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EDQUOT):
		return "storage_full"
	default:
		return "internal_failure"
	}
}

func safeUploadID(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func writeUploadStatusJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeUploadStatusError(w http.ResponseWriter, status int, message string) {
	writeUploadStatusJSON(w, status, map[string]string{"error": message})
}
