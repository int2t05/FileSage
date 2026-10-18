//go:build linux

package fsroot

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestWatchDoesNotFollowSymlinkOrTrash(t *testing.T) {
	base := t.TempDir()
	managed := filepath.Join(base, "managed")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(managed, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	trashFile := filepath.Join(managed, TrashDirectoryName, "hidden.txt")
	if err := os.MkdirAll(filepath.Dir(trashFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(trashFile, []byte("initial"), 0o600); err != nil {
		t.Fatal(err)
	}
	outsideFile := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("initial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(managed, "external")); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}

	root := openWalkTestRoot(t, managed)
	changes, cancel, done := startRootWatch(t, root)
	waitWatchChange(t, changes)
	assertWatchQuiet(t, changes, 300*time.Millisecond)

	if err := os.WriteFile(outsideFile, []byte("external update"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertWatchQuiet(t, changes, 300*time.Millisecond)
	if err := os.WriteFile(trashFile, []byte("trash update"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertWatchQuiet(t, changes, 300*time.Millisecond)
	cancelRootWatch(t, cancel, done)
}

func TestWatchAddsNestedDirectoriesBeforeNotifying(t *testing.T) {
	rootPath := t.TempDir()
	root := openWalkTestRoot(t, rootPath)
	changes, cancel, done := startRootWatch(t, root)
	waitWatchChange(t, changes)

	nested := filepath.Join(rootPath, "one", "two", "three")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	waitWatchChange(t, changes)
	drainWatchChanges(t, changes)

	file := filepath.Join(nested, "new.txt")
	if err := os.WriteFile(file, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitWatchChange(t, changes)
	if err := os.Chmod(file, 0o640); err != nil {
		t.Fatal(err)
	}
	waitWatchChange(t, changes)
	cancelRootWatch(t, cancel, done)
}

func TestWatchRebuildsAfterDirectoryMove(t *testing.T) {
	rootPath := t.TempDir()
	oldDirectory := filepath.Join(rootPath, "old")
	if err := os.Mkdir(oldDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	root := openWalkTestRoot(t, rootPath)
	changes, cancel, done := startRootWatch(t, root)
	waitWatchChange(t, changes)

	newDirectory := filepath.Join(rootPath, "new")
	if err := os.Rename(oldDirectory, newDirectory); err != nil {
		t.Fatal(err)
	}
	waitWatchChange(t, changes)
	drainWatchChanges(t, changes)
	if err := os.WriteFile(filepath.Join(newDirectory, "after-move.txt"), []byte("moved"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitWatchChange(t, changes)
	cancelRootWatch(t, cancel, done)
}

func TestWatchIncludesNestedMount(t *testing.T) {
	rootPath := t.TempDir()
	mountPath := filepath.Join(rootPath, "mounted")
	if err := os.Mkdir(mountPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mount("tmpfs", mountPath, "tmpfs", 0, "size=1m"); err != nil {
		t.Skipf("nested tmpfs mount unavailable: %v", err)
	}
	t.Cleanup(func() {
		if err := unix.Unmount(mountPath, 0); err != nil {
			t.Errorf("unmount test tmpfs: %v", err)
		}
	})
	file := filepath.Join(mountPath, "mounted.txt")
	if err := os.WriteFile(file, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}

	root := openWalkTestRoot(t, rootPath)
	changes, cancel, done := startRootWatch(t, root)
	waitWatchChange(t, changes)
	if err := os.WriteFile(file, []byte("after"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitWatchChange(t, changes)
	cancelRootWatch(t, cancel, done)
}

func TestParseInotifyOverflowRequestsRebuild(t *testing.T) {
	event := make([]byte, unix.SizeofInotifyEvent)
	binary.NativeEndian.PutUint32(event[0:4], ^uint32(0))
	binary.NativeEndian.PutUint32(event[4:8], unix.IN_Q_OVERFLOW)
	batch, err := parseInotifyEvents(event, make(map[int]int))
	if err != nil {
		t.Fatal(err)
	}
	if !batch.changed || !batch.rebuild || !batch.overflow {
		t.Fatalf("overflow batch = %+v, want changed, rebuild and overflow", batch)
	}
	var steps []string
	if err := dispatchInotifyBatch(batch, func() error {
		steps = append(steps, "rebuild")
		return nil
	}, func() {
		steps = append(steps, "changed")
	}); err != nil {
		t.Fatal(err)
	}
	if len(steps) != 2 || steps[0] != "rebuild" || steps[1] != "changed" {
		t.Fatalf("overflow handling order = %v, want [rebuild changed]", steps)
	}
}

func TestParseInotifyDirectoryEventRequestsRebuild(t *testing.T) {
	event := make([]byte, unix.SizeofInotifyEvent)
	binary.NativeEndian.PutUint32(event[0:4], 1)
	binary.NativeEndian.PutUint32(event[4:8], unix.IN_CREATE|unix.IN_ISDIR)
	batch, err := parseInotifyEvents(event, make(map[int]int))
	if err != nil {
		t.Fatal(err)
	}
	if !batch.changed || !batch.rebuild {
		t.Fatalf("directory batch = %+v, want changed and rebuild", batch)
	}
}

func TestParseInotifyUnmountRequestsRebuild(t *testing.T) {
	event := make([]byte, unix.SizeofInotifyEvent)
	binary.NativeEndian.PutUint32(event[0:4], 1)
	binary.NativeEndian.PutUint32(event[4:8], unix.IN_UNMOUNT)
	batch, err := parseInotifyEvents(event, make(map[int]int))
	if err != nil {
		t.Fatal(err)
	}
	if !batch.changed || !batch.rebuild {
		t.Fatalf("unmount batch = %+v, want changed and rebuild", batch)
	}
}

func startRootWatch(t *testing.T, root *Root) (<-chan struct{}, context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	changes := make(chan struct{}, 32)
	done := make(chan error, 1)
	go func() {
		err := root.Watch(ctx, func() {
			select {
			case changes <- struct{}{}:
			default:
			}
		})
		done <- err
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err, ok := <-done:
			if ok && err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("Watch() exit error = %v, want context.Canceled", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("Watch() did not stop after cancellation")
		}
	})
	return changes, cancel, done
}

func waitWatchChange(t *testing.T, changes <-chan struct{}) {
	t.Helper()
	select {
	case <-changes:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for filesystem change")
	}
}

func assertWatchQuiet(t *testing.T, changes <-chan struct{}, duration time.Duration) {
	t.Helper()
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-changes:
		t.Fatal("unexpected filesystem change notification")
	case <-timer.C:
	}
}

func drainWatchChanges(t *testing.T, changes <-chan struct{}) {
	t.Helper()
	quiet := time.NewTimer(300 * time.Millisecond)
	defer quiet.Stop()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case <-changes:
			if !quiet.Stop() {
				select {
				case <-quiet.C:
				default:
				}
			}
			quiet.Reset(300 * time.Millisecond)
		case <-quiet.C:
			return
		case <-deadline.C:
			t.Fatal("filesystem topology events did not settle")
		}
	}
}

func cancelRootWatch(t *testing.T, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Watch() error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Watch() did not stop after cancellation")
	}
}
