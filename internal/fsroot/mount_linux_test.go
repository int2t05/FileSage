//go:build linux

// 本文件验证 Linux 子卷卸载后的降级与写入保护。

package fsroot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestMountOptionsReadOnly(t *testing.T) {
	for _, test := range []struct {
		options string
		want    bool
	}{
		{options: "rw,nosuid,nodev", want: false},
		{options: "ro,nosuid,nodev", want: true},
		{options: "errors=remount-ro,rw", want: false},
	} {
		if got := mountOptionsReadOnly(test.options); got != test.want {
			t.Errorf("mountOptionsReadOnly(%q) = %t, want %t", test.options, got, test.want)
		}
	}
}

func TestNestedMountDisappearanceFailsClosed(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		runtime.LockOSThread()
		runNestedMountDisappearanceTest(t)
	}()
	<-done
}

func runNestedMountDisappearanceTest(t *testing.T) {
	if err := unix.Unshare(unix.CLONE_NEWNS); err != nil {
		skipWithoutMountCapability(t, err)
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		skipWithoutMountCapability(t, err)
	}

	rootPath := t.TempDir()
	volumePath := filepath.Join(rootPath, "volume")
	if err := os.Mkdir(volumePath, 0o755); err != nil {
		t.Fatal(err)
	}
	backingPath := filepath.Join(volumePath, "backing-only.txt")
	if err := os.WriteFile(backingPath, []byte("backing"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "available.txt"), []byte("available"), 0o600); err != nil {
		t.Fatal(err)
	}
	sourcePath := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourcePath, "mounted-only.txt"), []byte("mounted"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mount(sourcePath, volumePath, "", unix.MS_BIND, ""); err != nil {
		skipWithoutMountCapability(t, err)
	}
	mounted := true
	defer func() {
		if mounted {
			if err := unix.Unmount(volumePath, unix.MNT_DETACH); err != nil {
				t.Errorf("unmount test volume: %v", err)
			}
		}
	}()

	root, err := Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	initial, err := root.List("")
	if err != nil {
		t.Fatalf("list mounted root: %v", err)
	}
	if got := findEntry(initial, "volume"); got.MountUnavailable || got.ReadOnly {
		t.Fatalf("mounted volume entry = %#v, want available", got)
	}

	if err := unix.Unmount(volumePath, unix.MNT_DETACH); err != nil {
		t.Fatal(err)
	}
	mounted = false

	entries, err := root.List("")
	if err != nil {
		t.Fatalf("list root after unmount: %v", err)
	}
	volume := findEntry(entries, "volume")
	if !volume.MountUnavailable || !volume.ReadOnly {
		t.Fatalf("unmounted volume entry = %#v, want unavailable and read-only", volume)
	}
	if findEntry(entries, "available.txt").Name != "available.txt" {
		t.Fatal("root listing lost an available sibling")
	}
	if _, err := root.List("volume"); !errors.Is(err, ErrVolumeUnavailable) {
		t.Fatalf("list unmounted volume = %v, want ErrVolumeUnavailable", err)
	}
	if file, _, err := root.OpenRead("volume/backing-only.txt"); file != nil || !errors.Is(err, ErrVolumeUnavailable) {
		if file != nil {
			_ = file.Close()
		}
		t.Fatalf("open backing file = %v, %v; want ErrVolumeUnavailable", file, err)
	}
	if err := root.Mkdir("volume/new-dir", 0o700); !errors.Is(err, ErrVolumeUnavailable) {
		t.Fatalf("mkdir below unmounted volume = %v, want ErrVolumeUnavailable", err)
	}
	if _, err := root.MkdirAllNoFollow("volume/new/nested", 0o700); !errors.Is(err, ErrVolumeUnavailable) {
		t.Fatalf("mkdir all below unmounted volume = %v, want ErrVolumeUnavailable", err)
	}
	if _, err := root.WriteNewAtomicFrom("volume/new-file.txt", strings.NewReader("new"), 3, 0o600); !errors.Is(err, ErrVolumeUnavailable) {
		t.Fatalf("write below unmounted volume = %v, want ErrVolumeUnavailable", err)
	}
	if err := root.WriteAtomicIfUnchanged("volume/new-atomic.txt", Fingerprint{}, []byte("new"), 0o600); !errors.Is(err, ErrVolumeUnavailable) {
		t.Fatalf("atomic write below unmounted volume = %v, want ErrVolumeUnavailable", err)
	}
	if err := root.Remove("volume/backing-only.txt"); !errors.Is(err, ErrVolumeUnavailable) {
		t.Fatalf("remove below unmounted volume = %v, want ErrVolumeUnavailable", err)
	}
	if err := root.RenameNoReplace("volume/backing-only.txt", "moved.txt"); !errors.Is(err, ErrVolumeUnavailable) {
		t.Fatalf("rename below unmounted volume = %v, want ErrVolumeUnavailable", err)
	}

	var walked []Entry
	if err := root.Walk(context.Background(), func(entry Entry) error {
		walked = append(walked, entry)
		return nil
	}); err != nil {
		t.Fatalf("walk root after unmount: %v", err)
	}
	if got := findEntry(walked, "volume"); !got.MountUnavailable || !got.ReadOnly {
		t.Fatalf("walk volume entry = %#v, want unavailable and read-only", got)
	}
	if findEntry(walked, "volume/backing-only.txt").Name != "" {
		t.Fatal("walk exposed a backing-filesystem file below the unmounted volume")
	}

	snapshot, err := root.SnapshotTree(context.Background(), "")
	if err != nil {
		t.Fatalf("snapshot root after unmount: %v", err)
	}
	for _, item := range snapshot.Entries {
		if item.Entry.Path == "volume/backing-only.txt" {
			t.Fatal("snapshot exposed a backing-filesystem file below the unmounted volume")
		}
	}
	if got := findSnapshotEntry(snapshot, "volume"); !got.MountUnavailable || !got.ReadOnly {
		t.Fatalf("snapshot volume entry = %#v, want unavailable and read-only", got)
	}
	if _, err := os.Stat(backingPath); err != nil {
		t.Fatalf("backing file was changed or removed: %v", err)
	}
}

func skipWithoutMountCapability(t *testing.T, err error) {
	t.Helper()
	if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EOPNOTSUPP) {
		t.Skipf("mount namespace capability unavailable: %v", err)
	}
	t.Fatalf("mount setup failed: %v", err)
}

func findEntry(entries []Entry, relativePath string) Entry {
	for _, entry := range entries {
		if entry.Path == relativePath {
			return entry
		}
	}
	return Entry{}
}

func findSnapshotEntry(snapshot TreeSnapshot, relativePath string) Entry {
	for _, item := range snapshot.Entries {
		if item.Entry.Path == relativePath {
			return item.Entry
		}
	}
	return Entry{}
}
