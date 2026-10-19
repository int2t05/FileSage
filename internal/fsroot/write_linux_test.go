//go:build linux

// 本文件验证 Linux 原子写入的文件状态冲突检测。
package fsroot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestCopyFileNoReplaceCopiesRegularFileWithoutOverwriting(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "target"), 0o755); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(rootPath, "source.txt")
	if err := os.WriteFile(sourcePath, []byte("source contents"), 0o640); err != nil {
		t.Fatal(err)
	}
	root, err := Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	entry, err := root.LstatEntry("source.txt")
	if err != nil {
		t.Fatal(err)
	}
	result, err := root.CopyFileNoReplace(context.Background(), "source.txt", "target/copied.txt", entry.Fingerprint())
	if err != nil || !result.Published {
		t.Fatalf("copy result = %#v, error = %v", result, err)
	}
	if data, err := os.ReadFile(sourcePath); err != nil || string(data) != "source contents" {
		t.Fatalf("source changed: (%q, %v)", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(rootPath, "target", "copied.txt")); err != nil || string(data) != "source contents" {
		t.Fatalf("copy content = (%q, %v)", data, err)
	}
	if info, err := os.Stat(filepath.Join(rootPath, "target", "copied.txt")); err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("copy mode = (%v, %v), want 0640", info, err)
	}

	if err := os.WriteFile(filepath.Join(rootPath, "target", "existing.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := root.CopyFileNoReplace(context.Background(), "source.txt", "target/existing.txt", entry.Fingerprint()); !errors.Is(err, ErrConflict) {
		t.Fatalf("copy collision error = %v, want conflict", err)
	}
	if data, err := os.ReadFile(filepath.Join(rootPath, "target", "existing.txt")); err != nil || string(data) != "keep" {
		t.Fatalf("existing destination changed: (%q, %v)", data, err)
	}
	targetEntries, err := os.ReadDir(filepath.Join(rootPath, "target"))
	if err != nil || len(targetEntries) != 2 {
		t.Fatalf("copy left visible staging entries: (%v, %v)", targetEntries, err)
	}
}

func TestCopyFileNoReplaceRejectsStaleLinksSpecialFilesAndCancelledContext(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "target"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "source.txt"), []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("source.txt", filepath.Join(rootPath, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(rootPath, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	entry, err := root.LstatEntry("source.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "source.txt"), []byte("after"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := root.CopyFileNoReplace(context.Background(), "source.txt", "target/stale.txt", entry.Fingerprint()); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale copy error = %v, want conflict", err)
	}
	if _, err := root.LstatEntry("target/stale.txt"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stale copy published a target: %v", err)
	}

	link, err := root.LstatEntry("link.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := root.CopyFileNoReplace(context.Background(), "link.txt", "target/link.txt", link.Fingerprint()); !errors.Is(err, ErrNotRegularFile) {
		t.Fatalf("symlink copy error = %v, want non-regular-file error", err)
	}
	pipe, err := root.LstatEntry("pipe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := root.CopyFileNoReplace(context.Background(), "pipe", "target/pipe", pipe.Fingerprint()); !errors.Is(err, ErrNotRegularFile) {
		t.Fatalf("special-file copy error = %v, want non-regular-file error", err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	current, err := root.LstatEntry("source.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := root.CopyFileNoReplace(cancelled, "source.txt", "target/cancelled.txt", current.Fingerprint()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled copy error = %v, want context cancellation", err)
	}
	if _, err := root.LstatEntry("target/cancelled.txt"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("cancelled copy published a target: %v", err)
	}
	targetEntries, err := os.ReadDir(filepath.Join(rootPath, "target"))
	if err != nil || len(targetEntries) != 0 {
		t.Fatalf("failed copies left visible staging entries: (%v, %v)", targetEntries, err)
	}
}

func TestMoveFileNoReplaceUsesAtomicSameMountMove(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "target"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "source.txt"), []byte("move me"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	entry, err := root.LstatEntry("source.txt")
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := root.MoveFileNoReplace(context.Background(), "source.txt", "target/source.txt", entry.Fingerprint())
	if err != nil || !outcome.Published || !outcome.SourceRemoved || outcome.SHA256 != "" {
		t.Fatalf("move outcome = %#v, error = %v", outcome, err)
	}
	if _, err := root.LstatEntry("source.txt"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("move kept source: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(rootPath, "target", "source.txt")); err != nil || string(data) != "move me" {
		t.Fatalf("moved content = (%q, %v)", data, err)
	}

	if err := os.WriteFile(filepath.Join(rootPath, "source.txt"), []byte("still here"), 0o600); err != nil {
		t.Fatal(err)
	}
	entry, err = root.LstatEntry("source.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := root.MoveFileNoReplace(context.Background(), "source.txt", "target/source.txt", entry.Fingerprint()); !errors.Is(err, ErrConflict) {
		t.Fatalf("move collision error = %v, want conflict", err)
	}
	if data, err := os.ReadFile(filepath.Join(rootPath, "source.txt")); err != nil || string(data) != "still here" {
		t.Fatalf("source changed on collision: (%q, %v)", data, err)
	}
}

func TestMoveFileNoReplaceAcrossMountsCopiesThenRemovesSource(t *testing.T) {
	sourceDir, err := os.MkdirTemp("/tmp", "filesage-move-source-")
	if err != nil {
		t.Skipf("temporary source mount unavailable: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sourceDir) })
	destinationDir, err := os.MkdirTemp("/dev/shm", "filesage-move-destination-")
	if err != nil {
		t.Skipf("temporary destination mount unavailable: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(destinationDir) })

	sourcePath := filepath.Join(sourceDir, "source.txt")
	destinationPath := filepath.Join(destinationDir, "moved.txt")
	if err := os.WriteFile(sourcePath, []byte("cross mount contents"), 0o640); err != nil {
		t.Fatal(err)
	}
	root, err := Open("/")
	if err != nil {
		if errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM) {
			t.Skipf("host denies inspection of an unrelated system mount: %v", err)
		}
		t.Fatal(err)
	}
	defer root.Close()
	sourceRelative := strings.TrimPrefix(filepath.ToSlash(sourcePath), "/")
	destinationRelative := strings.TrimPrefix(filepath.ToSlash(destinationPath), "/")
	sourceMount, err := root.MountID(sourceRelative)
	if err != nil {
		t.Skipf("source mount identity unavailable: %v", err)
	}
	destinationMount, err := root.MountID(strings.TrimPrefix(filepath.ToSlash(destinationDir), "/"))
	if err != nil {
		t.Skipf("destination mount identity unavailable: %v", err)
	}
	if sourceMount == destinationMount {
		t.Skip("/tmp and /dev/shm share a mount in this environment")
	}

	entry, err := root.LstatEntry(sourceRelative)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := root.MoveFileNoReplace(context.Background(), sourceRelative, destinationRelative, entry.Fingerprint())
	if errors.Is(err, ErrAtomicWriteUnsupported) {
		t.Skipf("destination mount does not support anonymous publication: %v", err)
	}
	wantDigest := sha256.Sum256([]byte("cross mount contents"))
	if err != nil || !outcome.Published || !outcome.SourceRemoved || outcome.SHA256 != hex.EncodeToString(wantDigest[:]) {
		t.Fatalf("cross-mount move outcome = %#v, error = %v", outcome, err)
	}
	if _, err := os.Stat(sourcePath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("cross-mount move kept source: %v", err)
	}
	if data, err := os.ReadFile(destinationPath); err != nil || string(data) != "cross mount contents" {
		t.Fatalf("cross-mount destination = (%q, %v)", data, err)
	}
}

// TestWriteAtomicIfUnchangedDetectsRestoredModTime 验证 ctime 可发现伪装成旧版本的原地改写。
func TestWriteAtomicIfUnchangedDetectsRestoredModTime(t *testing.T) {
	rootPath := t.TempDir()
	target := filepath.Join(rootPath, "note.txt")
	if err := os.WriteFile(target, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	entry, err := root.LstatEntry("note.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !entry.ChangeTimeAvailable {
		t.Fatal("Linux file change time is unavailable")
	}
	if err := os.WriteFile(target, []byte("second"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(target, entry.ModifiedAt, entry.ModifiedAt); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteAtomicIfUnchanged("note.txt", entry.Fingerprint(), []byte("stale!"), 0o644); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale write error = %v, want conflict", err)
	}
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "second" {
		t.Fatalf("external content changed to %q", content)
	}
}

func TestWriteAtomicFromReplacesOnlyExpectedVersion(t *testing.T) {
	rootPath := t.TempDir()
	target := filepath.Join(rootPath, "note.txt")
	if err := os.WriteFile(target, []byte("before"), 0o640); err != nil {
		t.Fatal(err)
	}
	root, err := Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	entry, err := root.LstatEntry("note.txt")
	if err != nil {
		t.Fatal(err)
	}
	want := []byte("approved candidate")
	wantDigest := sha256.Sum256(want)
	digest, err := root.WriteAtomicFrom("note.txt", entry.Fingerprint(), strings.NewReader(string(want)), int64(len(want)), 0o600)
	if err != nil || digest != hex.EncodeToString(wantDigest[:]) {
		t.Fatalf("atomic replacement digest = %q, error = %v", digest, err)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != string(want) {
		t.Fatalf("replacement content = (%q, %v)", data, err)
	}
	if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("replacement mode = (%v, %v), want 0600", info, err)
	}
}

func TestWriteAtomicFromRejectsStaleVersionAndWrongSize(t *testing.T) {
	rootPath := t.TempDir()
	target := filepath.Join(rootPath, "note.txt")
	if err := os.WriteFile(target, []byte("before"), 0o640); err != nil {
		t.Fatal(err)
	}
	root, err := Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	stale, err := root.LstatEntry("note.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("outside update"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := root.WriteAtomicFrom("note.txt", stale.Fingerprint(), strings.NewReader("candidate"), 9, 0o600); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale write error = %v, want conflict", err)
	}
	current, err := root.LstatEntry("note.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := root.WriteAtomicFrom("note.txt", current.Fingerprint(), strings.NewReader("short"), 8, 0o600); !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("short write error = %v, want size mismatch", err)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "outside update" {
		t.Fatalf("failed replacement changed original: (%q, %v)", data, err)
	}
	entries, err := os.ReadDir(rootPath)
	if err != nil || len(entries) != 1 || entries[0].Name() != "note.txt" {
		t.Fatalf("failed replacement left temporary files: (%v, %v)", entries, err)
	}
}

func TestWriteAtomicFromRejectsExternalChangeDuringStreaming(t *testing.T) {
	rootPath := t.TempDir()
	target := filepath.Join(rootPath, "note.txt")
	if err := os.WriteFile(target, []byte("before"), 0o640); err != nil {
		t.Fatal(err)
	}
	root, err := Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	entry, err := root.LstatEntry("note.txt")
	if err != nil {
		t.Fatal(err)
	}
	reader := &externalChangeReader{
		reader: strings.NewReader("approved candidate"),
		change: func() error {
			if err := os.WriteFile(target, []byte("update"), 0o640); err != nil {
				return err
			}
			if entry.Fingerprint().ChangeTimeAvailable {
				return os.Chtimes(target, entry.ModifiedAt, entry.ModifiedAt)
			}
			return nil
		},
	}
	if _, err := root.WriteAtomicFrom("note.txt", entry.Fingerprint(), reader, int64(len("approved candidate")), 0o600); !errors.Is(err, ErrConflict) {
		t.Fatalf("write during external change error = %v, want conflict", err)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "update" {
		t.Fatalf("external content changed to (%q, %v)", data, err)
	}
	entries, err := os.ReadDir(rootPath)
	if err != nil || len(entries) != 1 || entries[0].Name() != "note.txt" {
		t.Fatalf("external conflict left temporary files: (%v, %v)", entries, err)
	}
}

// 外部进程在候选写入期间修改原文件，模拟进程内锁无法协调的变化。
type externalChangeReader struct {
	reader io.Reader
	change func() error
	done   bool
}

func (r *externalChangeReader) Read(buffer []byte) (int, error) {
	n, err := r.reader.Read(buffer)
	if !r.done {
		r.done = true
		if changeErr := r.change(); changeErr != nil {
			return n, changeErr
		}
	}
	return n, err
}
