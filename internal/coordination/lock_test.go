package coordination

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestPathKeysIncludeTheSameAncestorChainForNestedPaths(t *testing.T) {
	keys := PathKeys("photos/2026/a.jpg", "photos/2026/b.jpg")
	want := map[string]bool{
		"path:photos/2026/a.jpg":  true,
		"path:photos/2026/b.jpg":  true,
		"path-intent:photos/2026": true,
		"path-intent:photos":      true,
		"path-intent:":            true,
	}
	if len(keys) != len(want) {
		t.Fatalf("PathKeys() = %v, want %d unique path/ancestor keys", keys, len(want))
	}
	for _, key := range keys {
		if !want[key] {
			t.Errorf("unexpected path key %q", key)
		}
	}
}

func TestPathKeysUpgradeAnAncestorWhenItIsAlsoATarget(t *testing.T) {
	keys := PathKeys("photos", "photos/2026/a.jpg")
	want := map[string]bool{
		"path:photos":             true,
		"path:photos/2026/a.jpg":  true,
		"path-intent:":            true,
		"path-intent:photos/2026": true,
	}
	if len(keys) != len(want) {
		t.Fatalf("PathKeys() = %v, want %d unique keys", keys, len(want))
	}
	for _, key := range keys {
		if !want[key] {
			t.Errorf("unexpected path key %q", key)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	unlock, err := New().Lock(ctx, keys...)
	if err != nil {
		t.Fatalf("lock overlapping targets: %v", err)
	}
	unlock()
}

func TestDifferentPathLeavesRunInParallel(t *testing.T) {
	locker := New()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	unlockA, err := locker.Lock(ctx, PathKeys("photos/a.jpg")...)
	if err != nil {
		t.Fatalf("lock first leaf: %v", err)
	}
	defer unlockA()

	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		unlock, err := locker.Lock(ctx, PathKeys("photos/b.jpg")...)
		if err != nil {
			done <- err
			return
		}
		close(entered)
		unlock()
		done <- nil
	}()

	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("different leaf lock failed: %v", err)
	case <-ctx.Done():
		t.Fatal("different leaf waited on the first leaf")
	}
}

func TestSamePathLeafWaits(t *testing.T) {
	locker := New()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	unlockOwner, err := locker.Lock(ctx, PathKeys("photos/a.jpg")...)
	if err != nil {
		t.Fatalf("lock path: %v", err)
	}
	defer unlockOwner()

	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		unlock, err := locker.Lock(ctx, PathKeys("photos/a.jpg")...)
		if err != nil {
			done <- err
			return
		}
		close(entered)
		unlock()
		done <- nil
	}()

	select {
	case <-entered:
		t.Fatal("same path acquired while its owner was holding it")
	case err := <-done:
		t.Fatalf("same path waiter failed early: %v", err)
	case <-time.After(30 * time.Millisecond):
	}

	unlockOwner()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("same path waiter: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("same path waiter did not proceed after unlock")
	}
}

func TestDirectoryPathWaitsForDescendants(t *testing.T) {
	locker := New()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	unlockDirectory, err := locker.Lock(ctx, PathKeys("photos/2026")...)
	if err != nil {
		t.Fatalf("lock directory: %v", err)
	}
	defer unlockDirectory()

	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		unlock, err := locker.Lock(ctx, PathKeys("photos/2026/a.jpg")...)
		if err != nil {
			done <- err
			return
		}
		close(entered)
		unlock()
		done <- nil
	}()

	select {
	case <-entered:
		t.Fatal("descendant acquired while the directory was locked")
	case err := <-done:
		t.Fatalf("descendant waiter failed early: %v", err)
	case <-time.After(30 * time.Millisecond):
	}

	unlockDirectory()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("descendant waiter: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("descendant waiter did not proceed after directory unlock")
	}
}

func TestDescendantPathBlocksDirectory(t *testing.T) {
	locker := New()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	unlockFile, err := locker.Lock(ctx, PathKeys("photos/2026/a.jpg")...)
	if err != nil {
		t.Fatalf("lock descendant: %v", err)
	}
	defer unlockFile()

	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		unlock, err := locker.Lock(ctx, PathKeys("photos/2026")...)
		if err != nil {
			done <- err
			return
		}
		close(entered)
		unlock()
		done <- nil
	}()

	select {
	case <-entered:
		t.Fatal("directory acquired while a descendant was locked")
	case err := <-done:
		t.Fatalf("directory waiter failed early: %v", err)
	case <-time.After(30 * time.Millisecond):
	}

	unlockFile()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("directory waiter: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("directory waiter did not proceed after descendant unlock")
	}
}

func TestDifferentResourcesRunInParallelAndSameResourceWaits(t *testing.T) {
	locker := New()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	unlockA, err := locker.Lock(ctx, "device:1/inode:10")
	if err != nil {
		t.Fatalf("lock first resource: %v", err)
	}
	defer unlockA()

	enteredA := make(chan struct{})
	waiterDone := make(chan error, 1)
	go func() {
		unlock, err := locker.Lock(ctx, "device:1/inode:10")
		if err != nil {
			waiterDone <- err
			return
		}
		close(enteredA)
		unlock()
		waiterDone <- nil
	}()

	select {
	case <-enteredA:
		t.Fatal("second writer entered while the resource was held")
	case <-time.After(30 * time.Millisecond):
	}

	unlockB, err := locker.Lock(ctx, "device:2/inode:20")
	if err != nil {
		t.Fatalf("different resource did not proceed in parallel: %v", err)
	}
	unlockB()

	unlockA()
	select {
	case err := <-waiterDone:
		if err != nil {
			t.Fatalf("waiting writer: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("waiting writer did not proceed after unlock")
	}
}

func TestMultiResourceLockDeduplicatesKeys(t *testing.T) {
	locker := New()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	unlock, err := locker.Lock(ctx, "device:1/inode:10", "device:1/inode:10", "path:photos/a.jpg")
	if err != nil {
		t.Fatalf("lock duplicate resource keys: %v", err)
	}
	defer unlock()

	gotLock := make(chan struct{})
	go func() {
		unlock, err := locker.Lock(ctx, "path:photos/a.jpg", "device:1/inode:10")
		if err == nil {
			close(gotLock)
			unlock()
		}
	}()

	select {
	case <-gotLock:
		t.Fatal("alias key set acquired a resource held by another lock")
	case <-time.After(30 * time.Millisecond):
	}

	unlock()
	select {
	case <-gotLock:
	case <-ctx.Done():
		t.Fatal("alias key set did not proceed after unlock")
	}
}

func TestInodeKeyUsesStableDeviceAndInodeIdentity(t *testing.T) {
	if first, second := InodeKey(7, 19), InodeKey(7, 19); first != second || first != "inode:0000000000000007:0000000000000013" {
		t.Fatalf("InodeKey() = %q and %q, want a stable device/inode key", first, second)
	}
	if InodeKey(7, 19) == InodeKey(7, 20) || InodeKey(7, 19) == InodeKey(8, 19) {
		t.Fatal("different device/inode identities shared a lock key")
	}
}

func TestOpposingKeyOrderDoesNotDeadlock(t *testing.T) {
	locker := New()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	const workers = 16
	const iterations = 30
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for iteration := 0; iteration < iterations; iteration++ {
				paths := []string{"alpha/a.txt", "beta/b.txt"}
				if (worker+iteration)%2 == 1 {
					paths[0], paths[1] = paths[1], paths[0]
				}
				unlock, err := locker.Lock(ctx, PathKeys(paths...)...)
				if err != nil {
					errs <- fmt.Errorf("worker %d iteration %d: %w", worker, iteration, err)
					return
				}
				runtime.Gosched()
				unlock()
			}
		}(worker)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("concurrent locks requested in opposite orders deadlocked")
	}
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestCanceledPathWaiterReleasesAllResources(t *testing.T) {
	locker := New()
	ownerCtx := context.Background()
	unlockOwner, err := locker.Lock(ownerCtx, PathKeys("photos/a.jpg")...)
	if err != nil {
		t.Fatalf("lock owner path: %v", err)
	}

	waitCtx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if unlock, err := locker.Lock(waitCtx, PathKeys("photos/a.jpg")...); !errors.Is(err, context.DeadlineExceeded) {
		if unlock != nil {
			unlock()
		}
		t.Fatalf("canceled path lock error = %v, want deadline exceeded", err)
	}

	unlockOwner()
	if got := len(locker.resources); got != 0 {
		t.Fatalf("retained %d path resources after cancellation and unlock; want none", got)
	}
}

func TestCanceledWaiterReleasesInternalReferences(t *testing.T) {
	locker := New()
	ownerCtx, ownerCancel := context.WithTimeout(context.Background(), time.Second)
	defer ownerCancel()
	unlockOwner, err := locker.Lock(ownerCtx, "device:1/inode:10")
	if err != nil {
		t.Fatalf("lock resource: %v", err)
	}

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer waitCancel()
	if unlock, err := locker.Lock(waitCtx, "device:1/inode:10", "device:1/inode:20"); !errors.Is(err, context.DeadlineExceeded) {
		if unlock != nil {
			unlock()
		}
		t.Fatalf("canceled lock error = %v, want deadline exceeded", err)
	}

	unlockOwner()
	unlockOwner()
	unlockNext, err := locker.Lock(ownerCtx, "device:1/inode:10", "device:1/inode:20")
	if err != nil {
		t.Fatalf("lock after canceled waiter: %v", err)
	}
	unlockNext()

	if got := len(locker.resources); got != 0 {
		t.Fatalf("retained %d internal resources after release; want none", got)
	}
}

func TestLockRejectsInvalidRequest(t *testing.T) {
	locker := New()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	for _, keys := range [][]string{nil, {""}, {"valid", ""}} {
		if unlock, err := locker.Lock(ctx, keys...); err == nil || unlock != nil {
			t.Fatalf("Lock(%q) returned unlock=%t, err=%v; want an error and no unlock", keys, unlock != nil, err)
		}
	}

	canceledCtx, cancelRequest := context.WithCancel(context.Background())
	cancelRequest()
	if unlock, err := locker.Lock(canceledCtx, "device:1/inode:10"); !errors.Is(err, context.Canceled) || unlock != nil {
		t.Fatalf("Lock with canceled context returned unlock=%t, err=%v; want context.Canceled and no unlock", unlock != nil, err)
	}
	if got := len(locker.resources); got != 0 {
		t.Fatalf("invalid requests retained %d internal resources; want none", got)
	}
}
