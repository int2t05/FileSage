// 本文件覆盖文件索引监听与后台任务的生命周期边界。
package indexer

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"filesage/internal/fsroot"
	"filesage/internal/store"
)

type signalWatchRoot struct {
	root   *fsroot.Root
	events chan struct{}
}

func (r *signalWatchRoot) Walk(ctx context.Context, visit func(fsroot.Entry) error) error {
	return r.root.Walk(ctx, visit)
}

func (r *signalWatchRoot) Watch(ctx context.Context, changed func()) error {
	changed()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-r.events:
			changed()
		}
	}
}

func TestWatcherChangeRebuildsIndex(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("safe tree walking is currently implemented on Linux")
	}

	managedRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(managedRoot, "before.txt"), []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := fsroot.Open(managedRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	metadata, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()

	watchRoot := &signalWatchRoot{root: root, events: make(chan struct{}, 1)}
	service, err := Open(context.Background(), watchRoot, metadata)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()

	initial := waitForReadyIndex(t, service)
	if err := os.WriteFile(filepath.Join(managedRoot, "after.txt"), []byte("after"), 0o600); err != nil {
		t.Fatal(err)
	}
	watchRoot.events <- struct{}{}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		status, err := service.Status(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if status.ActiveGeneration > initial.ActiveGeneration && status.State == store.FileIndexReady {
			records, err := service.Query(context.Background(), store.FileIndexQuery{
				Generation: status.ActiveGeneration,
				Recursive:  true,
				Limit:      100,
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, record := range records {
				if record.RelativePath == "after.txt" {
					return
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("watcher change did not publish a generation containing the new file")
}

// TestCloseWaitsForWatcher 验证关闭服务会等待监听 goroutine 退出。
func TestCloseWaitsForWatcher(t *testing.T) {
	metadata, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()

	watchRoot := &joiningWatchRoot{
		started: make(chan struct{}), canceled: make(chan struct{}), release: make(chan struct{}),
	}
	service, err := Open(context.Background(), watchRoot, metadata)
	if err != nil {
		t.Fatal(err)
	}
	<-watchRoot.started
	closed := make(chan struct{})
	go func() {
		service.Close()
		close(closed)
	}()
	<-watchRoot.canceled
	select {
	case <-closed:
		t.Fatal("Close returned before Watch exited")
	case <-time.After(50 * time.Millisecond):
	}
	close(watchRoot.release)
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return after Watch exited")
	}
}

// TestUnsupportedWatcherKeepsIndexStale 验证无监听能力时不会误报索引最新。
func TestUnsupportedWatcherKeepsIndexStale(t *testing.T) {
	metadata, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()

	service, err := Open(context.Background(), &unsupportedWatchRoot{}, metadata)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	waitForGeneration(t, service)
	status, err := service.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "stale" {
		t.Fatalf("unsupported watcher state = %q, want stale", status.State)
	}
}

// TestPeriodicReconciliationSkipsActiveScan 验证周期校准不会排队重复全量扫描。
func TestPeriodicReconciliationSkipsActiveScan(t *testing.T) {
	metadata, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()

	walkRoot := &blockedWalkRoot{started: make(chan struct{}), release: make(chan struct{})}
	service, err := Open(context.Background(), walkRoot, metadata)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	<-walkRoot.started
	service.reconcileIfDue(context.Background())
	service.mu.Lock()
	rescan := service.rescan
	service.mu.Unlock()
	if rescan {
		t.Fatal("periodic reconciliation queued another scan during an active scan")
	}
	close(walkRoot.release)
	waitForReadyIndex(t, service)
	if calls := walkRoot.calls.Load(); calls != 1 {
		t.Fatalf("walk count = %d, want 1", calls)
	}
}

type joiningWatchRoot struct {
	started  chan struct{}
	canceled chan struct{}
	release  chan struct{}
}

// Walk 提供空目录扫描，隔离服务关闭测试。
func (r *joiningWatchRoot) Walk(ctx context.Context, visit func(fsroot.Entry) error) error {
	return nil
}

// Watch 模拟退出前仍在清理的监听协程。
func (r *joiningWatchRoot) Watch(ctx context.Context, _ func()) error {
	close(r.started)
	<-ctx.Done()
	close(r.canceled)
	<-r.release
	return ctx.Err()
}

type unsupportedWatchRoot struct{}

// Walk 提供空目录扫描，隔离平台监听能力测试。
func (r *unsupportedWatchRoot) Walk(ctx context.Context, visit func(fsroot.Entry) error) error {
	return nil
}

// Watch 模拟当前平台不支持文件监听。
func (r *unsupportedWatchRoot) Watch(context.Context, func()) error {
	return fsroot.ErrWatchUnsupported
}

type blockedWalkRoot struct {
	started chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

// Walk 阻塞一轮扫描，用于验证周期任务不会重复排队。
func (r *blockedWalkRoot) Walk(ctx context.Context, visit func(fsroot.Entry) error) error {
	r.calls.Add(1)
	select {
	case <-r.started:
	default:
		close(r.started)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.release:
		return nil
	}
}

// waitForGeneration 等待首个完整索引代次发布。
func waitForGeneration(t *testing.T, service *Service) store.FileIndexStatus {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		status, err := service.Status(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if status.ActiveGeneration > 0 && status.BuildingGeneration == 0 {
			return status
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("index generation did not complete")
	return store.FileIndexStatus{}
}

func waitForReadyIndex(t *testing.T, service *Service) store.FileIndexStatus {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		status, err := service.Status(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if status.State == store.FileIndexReady && status.ActiveGeneration > 0 && status.BuildingGeneration == 0 {
			return status
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("index did not become ready")
	return store.FileIndexStatus{}
}
