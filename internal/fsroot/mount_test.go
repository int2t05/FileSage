// 本文件验证登记子卷离线后的状态标记与写入拒绝。

package fsroot

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegisteredMountForPathUsesDeepestComponentMatch(t *testing.T) {
	mounts := []registeredMount{
		{path: "storage", id: 10},
		{path: "storage/archive", id: 20},
		{path: "storage-old", id: 30},
	}
	for _, test := range []struct {
		path string
		want string
	}{
		{path: "storage", want: "storage"},
		{path: "storage/photos/2025.jpg", want: "storage"},
		{path: "storage/archive", want: "storage/archive"},
		{path: "storage/archive/report.pdf", want: "storage/archive"},
		{path: "storage-old/file.txt", want: "storage-old"},
		{path: "other/file.txt", want: ""},
	} {
		got, ok := registeredMountForPath(mounts, test.path)
		if test.want == "" {
			if ok {
				t.Fatalf("registeredMountForPath(%q) = %#v, want no match", test.path, got)
			}
			continue
		}
		if !ok || got.path != test.want {
			t.Fatalf("registeredMountForPath(%q) = %#v, %t; want %q", test.path, got, ok, test.want)
		}
	}
}

func TestReadOnlyPolicyUsesDeepestMountedVolume(t *testing.T) {
	root := &Root{
		rootReadOnly: true,
		registeredMounts: []registeredMount{
			{path: "writable", id: 10, readOnly: false},
			{path: "readonly", id: 20, readOnly: true},
		},
	}
	for _, test := range []struct {
		path string
		want bool
	}{
		{path: "", want: true},
		{path: "writable/file.txt", want: false},
		{path: "readonly/file.txt", want: true},
		{path: "other/file.txt", want: true},
	} {
		entry := Entry{}
		root.applyReadOnly(&entry, test.path)
		if entry.ReadOnly != test.want {
			t.Errorf("read-only for %q = %t, want %t", test.path, entry.ReadOnly, test.want)
		}
	}
}

func TestUnavailableRegisteredMountStaysVisibleAndRejectsAccess(t *testing.T) {
	rootPath := t.TempDir()
	volumePath := filepath.Join(rootPath, "volume")
	if err := os.Mkdir(volumePath, 0o755); err != nil {
		t.Fatal(err)
	}
	backingFile := filepath.Join(volumePath, "backing.txt")
	if err := os.WriteFile(backingFile, []byte("backing"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "available.txt"), []byte("available"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	root.registeredMounts = []registeredMount{{path: "volume", id: ^uint64(0)}}

	entries, err := root.List("")
	if err != nil {
		t.Fatalf("list root: %v", err)
	}
	var volume Entry
	for _, entry := range entries {
		if entry.Path == "volume" {
			volume = entry
		}
	}
	if !volume.MountUnavailable || !volume.ReadOnly {
		t.Fatalf("volume entry = %#v, want unavailable and read-only", volume)
	}
	volume, err = root.LstatEntry("volume")
	if err != nil {
		t.Fatalf("lstat unavailable volume root: %v", err)
	}
	if !volume.MountUnavailable || !volume.ReadOnly {
		t.Fatalf("volume entry = %#v, want unavailable and read-only", volume)
	}
	if _, err := root.List("volume"); !errors.Is(err, ErrVolumeUnavailable) {
		t.Fatalf("list unavailable volume error = %v, want ErrVolumeUnavailable", err)
	}
	if file, _, err := root.OpenRead("volume/backing.txt"); file != nil || !errors.Is(err, ErrVolumeUnavailable) {
		if file != nil {
			_ = file.Close()
		}
		t.Fatalf("open backing file = %v, %v; want ErrVolumeUnavailable", file, err)
	}

	checks := []struct {
		name string
		call func() error
	}{
		{name: "mkdir", call: func() error { return root.Mkdir("volume/new", 0o700) }},
		{name: "mkdir all", call: func() error { _, err := root.MkdirAllNoFollow("volume/new/nested", 0o700); return err }},
		{name: "stream write", call: func() error {
			_, err := root.WriteNewAtomicFrom("volume/new.txt", strings.NewReader("x"), 1, 0o600)
			return err
		}},
		{name: "atomic write", call: func() error {
			return root.WriteAtomicIfUnchanged("volume/new-atomic.txt", Fingerprint{}, []byte("x"), 0o600)
		}},
		{name: "remove", call: func() error { return root.Remove("volume/backing.txt") }},
		{name: "rename", call: func() error { return root.RenameNoReplace("volume/backing.txt", "moved.txt") }},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			if err := check.call(); !errors.Is(err, ErrVolumeUnavailable) {
				t.Fatalf("error = %v, want ErrVolumeUnavailable", err)
			}
		})
	}
	if _, err := os.Stat(backingFile); err != nil {
		t.Fatalf("backing file changed or disappeared: %v", err)
	}
	for _, rel := range []string{"new", "new.txt", "new-atomic.txt"} {
		if _, err := os.Lstat(filepath.Join(volumePath, rel)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unexpected write to backing path %q: %v", rel, err)
		}
	}
	if err := os.RemoveAll(volumePath); err != nil {
		t.Fatal(err)
	}
	entries, err = root.List("")
	if err != nil {
		t.Fatalf("list root with missing mountpoint: %v", err)
	}
	volume = Entry{}
	for _, entry := range entries {
		if entry.Path == "volume" {
			volume = entry
		}
	}
	if !volume.MountUnavailable || !volume.ReadOnly || volume.Kind != KindDirectory {
		t.Fatalf("missing mountpoint entry = %#v, want unavailable read-only directory placeholder", volume)
	}
}
