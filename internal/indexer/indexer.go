// Package indexer 维护原生文件系统的可重建元数据索引。
package indexer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"filesage/internal/fsroot"
	"filesage/internal/store"
)

const scanBatchSize = 500
const reconciliationInterval = 15 * time.Minute
const watchRetryInterval = 5 * time.Second
const watchRetryMax = 5 * time.Minute
const refreshDebounce = 300 * time.Millisecond

type scanStore interface {
	BeginFileIndexScan(context.Context) (int64, error)
	UpsertFileIndexBatch(context.Context, int64, []store.FileIndexRecord) error
	CompleteFileIndexScan(context.Context, int64) error
	FailFileIndexScan(context.Context, int64, string) error
	FileIndexStatus(context.Context) (store.FileIndexStatus, error)
	QueryFileIndex(context.Context, store.FileIndexQuery) ([]store.FileIndexRecord, error)
}

type treeWalker interface {
	Walk(context.Context, func(fsroot.Entry) error) error
}

type changeWatcher interface {
	// Watch 注册完成后先提示一次，之后在文件变化时继续提示。
	Watch(context.Context, func()) error
}

// Service 在后台构建索引，已完成的代次可继续用于查询。
type Service struct {
	root  treeWalker
	store scanStore
	ctx   context.Context
	stop  context.CancelFunc

	mu           sync.Mutex
	scanCancel   context.CancelFunc
	done         chan struct{}
	watchDone    chan struct{}
	scanning     bool
	closed       bool
	rescan       bool
	stale        bool
	watchEnabled bool
	watching     bool
	lastScanAt   time.Time
}

// Open 启动服务并异步校准一次文件索引。
func Open(parent context.Context, root treeWalker, metadata scanStore) (*Service, error) {
	if root == nil || metadata == nil {
		return nil, fmt.Errorf("index root and store are required")
	}
	if _, err := metadata.FileIndexStatus(parent); err != nil {
		return nil, fmt.Errorf("read file index status: %w", err)
	}
	ctx, stop := context.WithCancel(parent)
	service := &Service{root: root, store: metadata, ctx: ctx, stop: stop, stale: true}
	if watcher, ok := root.(changeWatcher); ok {
		service.watchEnabled = true
		service.watchDone = make(chan struct{})
		go service.watchLoop(ctx, watcher)
	} else {
		service.Refresh(ctx)
	}
	return service, nil
}

// Close 停止后台任务并等待扫描与监听退出。
func (s *Service) Close() {
	s.mu.Lock()
	s.closed = true
	stop, cancel, done, watchDone := s.stop, s.scanCancel, s.done, s.watchDone
	s.mu.Unlock()
	if stop != nil {
		stop()
	}
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
	if watchDone != nil {
		<-watchDone
	}
}

// Refresh 在后台重新扫描；并行请求合并为一次后续校准。
func (s *Service) Refresh(parent context.Context) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.stale = true
	if s.done != nil {
		if s.scanning {
			s.rescan = true
		}
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	s.scanCancel, s.done = cancel, done
	s.mu.Unlock()

	go s.scanLoop(ctx, done)
}

// Status 返回索引校准状态。
func (s *Service) Status(ctx context.Context) (store.FileIndexStatus, error) {
	status, err := s.store.FileIndexStatus(ctx)
	if err != nil {
		return status, err
	}
	s.mu.Lock()
	if (s.stale || (s.watchEnabled && !s.watching)) && status.ActiveGeneration > 0 {
		status.State = "stale"
	}
	s.mu.Unlock()
	return status, nil
}

// reconcileIfDue 仅在索引空闲且校准到期时启动扫描。
func (s *Service) reconcileIfDue(parent context.Context) {
	s.mu.Lock()
	if s.closed || s.done != nil || !s.lastScanAt.IsZero() && time.Since(s.lastScanAt) < reconciliationInterval {
		s.mu.Unlock()
		return
	}
	s.stale = true
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	s.scanCancel, s.done = cancel, done
	s.mu.Unlock()
	go s.scanLoop(ctx, done)
}

// watcherChanged 记录监听健康状态并合并变化通知。
func (s *Service) watcherChanged(changes chan<- struct{}) {
	s.mu.Lock()
	if !s.closed {
		s.watching = true
		s.stale = true
		if s.scanning {
			s.rescan = true
		}
	}
	s.mu.Unlock()
	select {
	case changes <- struct{}{}:
	default:
	}
}

// Query 返回指定完整代次中的分页结果。
func (s *Service) Query(ctx context.Context, query store.FileIndexQuery) ([]store.FileIndexRecord, error) {
	// 扫描期间继续查询旧完整代次，避免退化为整目录读取。
	return s.store.QueryFileIndex(ctx, query)
}

// QueryFileIndex 查询固定代次，供需要一致快照的服务使用。
func (s *Service) QueryFileIndex(ctx context.Context, query store.FileIndexQuery) ([]store.FileIndexRecord, error) {
	return s.Query(ctx, query)
}

func (s *Service) scanLoop(ctx context.Context, done chan struct{}) {
	defer func() {
		s.mu.Lock()
		if s.done == done {
			s.scanCancel = nil
			s.done = nil
			s.scanning = false
		}
		rescan := s.rescan && !s.closed
		s.rescan = false
		s.mu.Unlock()
		close(done)
		if rescan {
			s.Refresh(s.ctx)
		}
	}()
	timer := time.NewTimer(refreshDebounce)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}
	s.mu.Lock()
	if s.done == done {
		s.scanning = true
	}
	s.mu.Unlock()
	if err := s.scan(ctx); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("file index scan failed", "error", err)
	}
}

// watchLoop 合并文件变化事件，并定期全量校准索引。
func (s *Service) watchLoop(ctx context.Context, watcher changeWatcher) {
	defer close(s.watchDone)
	changes := make(chan struct{}, 1)
	periodic := time.NewTicker(reconciliationInterval)
	defer periodic.Stop()
	var retryC <-chan time.Time
	var retry *time.Timer
	defer func() {
		if retry != nil {
			retry.Stop()
		}
	}()
	var watchResult <-chan error
	var watchExited <-chan struct{}
	watchOutage := false
	retryDelay := watchRetryInterval
	watchStarted := time.Now()
	startWatcher := func() {
		result := make(chan error, 1)
		exited := make(chan struct{})
		watchResult = result
		watchExited = exited
		watchStarted = time.Now()
		go func() {
			defer close(exited)
			result <- watcher.Watch(ctx, func() {
				s.watcherChanged(changes)
			})
		}()
	}
	startWatcher()

	for {
		select {
		case <-ctx.Done():
			if watchExited != nil {
				<-watchExited
			}
			s.mu.Lock()
			s.watching = false
			s.mu.Unlock()
			return
		case <-changes:
			watchOutage = false
			s.Refresh(ctx)
		case <-periodic.C:
			s.reconcileIfDue(ctx)
		case err := <-watchResult:
			watchResult = nil
			if watchExited != nil {
				<-watchExited
				watchExited = nil
			}
			if ctx.Err() != nil {
				s.mu.Lock()
				s.watching = false
				s.mu.Unlock()
				return
			}
			s.mu.Lock()
			s.watching = false
			s.stale = true
			s.mu.Unlock()
			if errors.Is(err, fsroot.ErrWatchUnsupported) {
				slog.Info("filesystem watcher unavailable; periodic index calibration remains enabled")
				if !watchOutage {
					s.Refresh(ctx)
				}
				watcher = nil
				continue
			}
			if err != nil {
				slog.Warn("filesystem watcher stopped; index calibration scheduled", "error", err)
			} else {
				slog.Warn("filesystem watcher stopped unexpectedly; restarting")
			}
			if time.Since(watchStarted) >= time.Minute {
				retryDelay = watchRetryInterval
			}
			if !watchOutage {
				s.Refresh(ctx)
				watchOutage = true
			}
			if retry == nil {
				retry = time.NewTimer(retryDelay)
			} else {
				retry.Reset(retryDelay)
			}
			retryDelay = min(retryDelay*2, watchRetryMax)
			retryC = retry.C
		case <-retryC:
			retryC = nil
			if watcher != nil && watchResult == nil {
				startWatcher()
			}
		}
	}
}

func (s *Service) scan(ctx context.Context) error {
	generation, err := s.store.BeginFileIndexScan(ctx)
	if err != nil {
		return fmt.Errorf("begin file index scan: %w", err)
	}

	batch := make([]store.FileIndexRecord, 0, scanBatchSize)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := s.store.UpsertFileIndexBatch(ctx, generation, batch); err != nil {
			return err
		}
		batch = batch[:0]
		return nil
	}
	err = s.root.Walk(ctx, func(entry fsroot.Entry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		record := indexRecord(entry)
		record.CapturedAt = captureDate(ctx, s.root, entry)
		batch = append(batch, record)
		if len(batch) >= scanBatchSize {
			return flush()
		}
		return nil
	})
	if err == nil {
		err = flush()
	}
	if err != nil {
		_ = s.store.FailFileIndexScan(context.Background(), generation, err.Error())
		return fmt.Errorf("walk managed files: %w", err)
	}
	if err := s.store.CompleteFileIndexScan(ctx, generation); err != nil {
		_ = s.store.FailFileIndexScan(context.Background(), generation, err.Error())
		return fmt.Errorf("complete file index scan: %w", err)
	}
	s.mu.Lock()
	s.lastScanAt = time.Now()
	if !s.rescan {
		s.stale = false
	}
	s.mu.Unlock()
	return nil
}

func indexRecord(entry fsroot.Entry) store.FileIndexRecord {
	kind := string(entry.Kind)
	if entry.Symlink {
		kind = "symlink"
	}
	parent := path.Dir(entry.Path)
	if parent == "." {
		parent = ""
	}
	mediaType := mime.TypeByExtension(strings.ToLower(filepath.Ext(entry.Name)))
	record := store.FileIndexRecord{
		RelativePath: entry.Path,
		ParentPath:   parent,
		Name:         path.Base(entry.Path),
		Kind:         kind,
		SizeBytes:    max(entry.Size, 0),
		ModifiedAt:   entry.ModifiedAt,
		Mode:         uint32(entry.Mode),
		MediaType:    mediaType,
		IndexedAt:    time.Now().UTC(),
	}
	if entry.IdentityAvailable {
		record.Device = uint64Pointer(entry.Device)
		record.Inode = uint64Pointer(entry.Inode)
		record.HardLinkCount = uint64Pointer(entry.Links)
	}
	return record
}

func uint64Pointer(value uint64) *uint64 { return &value }
