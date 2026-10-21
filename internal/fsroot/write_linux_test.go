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
	"testing"
	"time"

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

// TestMoveFileNoReplaceRestoresSourceReplacementAtCommitBoundary 验证同卷提交时替换来源不会被误移。
func TestMoveFileNoReplaceRestoresSourceReplacementAtCommitBoundary(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "target"), 0o755); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(rootPath, "source.txt")
	originalPath := filepath.Join(rootPath, "original.txt")
	destinationPath := filepath.Join(rootPath, "target", "moved.txt")
	if err := os.WriteFile(sourcePath, []byte("approved file"), 0o600); err != nil {
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
	ctx := &sameMountMoveRaceContext{
		Context: context.Background(), source: sourcePath, original: originalPath,
		replacement: []byte("changed file"), triggerAt: 3,
	}

	outcome, err := root.MoveFileNoReplace(ctx, "source.txt", "target/moved.txt", entry.Fingerprint())
	if !ctx.triggered || !errors.Is(err, ErrConflict) || outcome.Published || outcome.SourceRemoved {
		t.Fatalf("move result = (%#v, %v), race triggered %t; want rejected replacement", outcome, err, ctx.triggered)
	}
	if data, err := os.ReadFile(sourcePath); err != nil || string(data) != string(ctx.replacement) {
		t.Fatalf("replacement source = (%q, %v), want unchanged", data, err)
	}
	if data, err := os.ReadFile(originalPath); err != nil || string(data) != "approved file" {
		t.Fatalf("approved source = (%q, %v), want unchanged", data, err)
	}
	if _, err := os.Lstat(destinationPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("replacement was published at destination: %v", err)
	}
}

func TestMoveFileNoReplaceAcrossMountsCopiesThenRemovesSource(t *testing.T) {
	root, sourceDir, destinationDir := openMoveFileRaceRoots(t)

	sourcePath := filepath.Join(sourceDir, "source.txt")
	destinationPath := filepath.Join(destinationDir, "moved.txt")
	if err := os.WriteFile(sourcePath, []byte("cross mount contents"), 0o640); err != nil {
		t.Fatal(err)
	}
	sourceRelative := strings.TrimPrefix(filepath.ToSlash(sourcePath), "/")
	destinationRelative := strings.TrimPrefix(filepath.ToSlash(destinationPath), "/")

	entry, err := root.LstatEntry(sourceRelative)
	if err != nil {
		t.Fatal(err)
	}
	wantDigest := sha256.Sum256([]byte("cross mount contents"))
	outcome, err := root.MoveFileNoReplaceWithSHA256(context.Background(), sourceRelative, destinationRelative, entry.Fingerprint(), hex.EncodeToString(wantDigest[:]))
	if errors.Is(err, ErrAtomicWriteUnsupported) {
		t.Skipf("destination mount does not support anonymous publication: %v", err)
	}
	if err != nil || !outcome.Published || !outcome.SourceRemoved || outcome.SHA256 != hex.EncodeToString(wantDigest[:]) {
		t.Fatalf("cross-mount move outcome = %#v, error = %v", outcome, err)
	}
	if entries, err := readMoveRecoveryEntries(root.moveRecoveryRoot); err != nil || len(entries) != 0 {
		t.Fatalf("completed move journal = (%v, %v), want empty", entries, err)
	}
	if _, err := os.Stat(sourcePath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("cross-mount move kept source: %v", err)
	}
	if data, err := os.ReadFile(destinationPath); err != nil || string(data) != "cross mount contents" {
		t.Fatalf("cross-mount destination = (%q, %v)", data, err)
	}
}

// TestMoveFileNoReplaceAcrossMountsRejectsDigestMismatch 验证摘要不符时不发布目标。
func TestMoveFileNoReplaceAcrossMountsRejectsDigestMismatch(t *testing.T) {
	root, sourceDir, destinationDir := openMoveFileRaceRoots(t)

	sourcePath := filepath.Join(sourceDir, "source.txt")
	destinationPath := filepath.Join(destinationDir, "moved.txt")
	approvedContent := []byte("approved contents")
	if err := os.WriteFile(sourcePath, approvedContent, 0o640); err != nil {
		t.Fatal(err)
	}
	sourceRelative := strings.TrimPrefix(filepath.ToSlash(sourcePath), "/")
	destinationRelative := strings.TrimPrefix(filepath.ToSlash(destinationPath), "/")
	entry, err := root.LstatEntry(sourceRelative)
	if err != nil {
		t.Fatal(err)
	}
	wrongDigest := sha256.Sum256([]byte("external contents"))

	outcome, err := root.MoveFileNoReplaceWithSHA256(context.Background(), sourceRelative, destinationRelative, entry.Fingerprint(), hex.EncodeToString(wrongDigest[:]))
	if !errors.Is(err, ErrConflict) || outcome.Published || outcome.SourceRemoved {
		t.Fatalf("cross-mount move with wrong digest = (%#v, %v), want conflict before publish", outcome, err)
	}
	if data, err := os.ReadFile(sourcePath); err != nil || string(data) != string(approvedContent) {
		t.Fatalf("source after digest conflict = (%q, %v), want preserved", data, err)
	}
	if _, err := os.Lstat(destinationPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("cross-mount target after digest conflict = %v, want absent", err)
	}
}

// TestMoveFilePreservesSourceChangedAfterCopy 验证复制发布后源文件变化时不会删除外部内容。
func TestMoveFilePreservesSourceChangedAfterCopy(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "target"), 0o755); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(rootPath, "source.txt")
	approvedContent := []byte("approved source")
	externalContent := []byte("external update")
	if len(approvedContent) != len(externalContent) {
		t.Fatal("test contents must have the same size")
	}
	if err := os.WriteFile(sourcePath, approvedContent, 0o640); err != nil {
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
	copyResult, err := root.CopyFileNoReplace(context.Background(), "source.txt", "target/moved.txt", entry.Fingerprint())
	if err != nil || !copyResult.Published {
		t.Fatalf("copy result = (%#v, %v)", copyResult, err)
	}
	if err := os.WriteFile(sourcePath, externalContent, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(sourcePath, entry.ModifiedAt, entry.ModifiedAt); err != nil {
		t.Fatal(err)
	}

	removed, retained, err := platformRemoveFileIfUnchanged(context.Background(), root, "source.txt", "destination.txt", entry.Fingerprint(), copyResult.SHA256)
	if removed || retained != "" || !errors.Is(err, ErrConflict) {
		t.Fatalf("remove changed source = (%t, %q, %v), want conflict without removal", removed, retained, err)
	}
	if content, err := os.ReadFile(sourcePath); err != nil || string(content) != string(externalContent) {
		t.Fatalf("external source content = (%q, %v)", content, err)
	}
	if content, err := os.ReadFile(filepath.Join(rootPath, "target", "moved.txt")); err != nil || string(content) != string(approvedContent) {
		t.Fatalf("copied target content = (%q, %v)", content, err)
	}
}

// TestMoveFilePreservesReplacementAtRemovalBoundary 验证删除前替换的来源不会被误删。
func TestMoveFilePreservesReplacementAtRemovalBoundary(t *testing.T) {
	root, sourceBase, destinationBase := openMoveFileRaceRoots(t)
	sourcePath := filepath.Join(sourceBase, "source.txt")
	originalPath := filepath.Join(sourceBase, "original.txt")
	destinationPath := filepath.Join(destinationBase, "moved.txt")
	approvedContent := []byte("approved file")
	externalContent := []byte("external file")
	if len(approvedContent) != len(externalContent) {
		t.Fatal("fixture contents must have equal sizes")
	}
	if err := os.WriteFile(sourcePath, approvedContent, 0o600); err != nil {
		t.Fatal(err)
	}
	sourceRelative := strings.TrimPrefix(filepath.ToSlash(sourcePath), "/")
	destinationRelative := strings.TrimPrefix(filepath.ToSlash(destinationPath), "/")
	entry, err := root.LstatEntry(sourceRelative)
	if err != nil {
		t.Fatal(err)
	}
	ctx := &moveFileUnlinkHookContext{Context: context.Background(), destination: destinationPath, hook: func() error {
		if err := os.Rename(sourcePath, originalPath); err != nil {
			return err
		}
		return os.WriteFile(sourcePath, externalContent, 0o600)
	}}

	outcome, err := root.MoveFileNoReplace(ctx, sourceRelative, destinationRelative, entry.Fingerprint())
	if !ctx.triggered {
		t.Fatal("source replacement hook did not run at the removal boundary")
	}
	if outcome.SourceRemoved || !errors.Is(err, ErrConflict) {
		t.Fatalf("move outcome = (%+v, %v), want conflict with source retained", outcome, err)
	}
	if data, err := os.ReadFile(sourcePath); err != nil || string(data) != string(externalContent) {
		t.Fatalf("replacement source = (%q, %v), want external content", data, err)
	}
	if data, err := os.ReadFile(originalPath); err != nil || string(data) != string(approvedContent) {
		t.Fatalf("original source = (%q, %v), want approved content", data, err)
	}
	if data, err := os.ReadFile(destinationPath); err != nil || string(data) != string(approvedContent) {
		t.Fatalf("destination = (%q, %v), want approved content", data, err)
	}
}

// TestMoveFileRetainsQuarantinedSourceWhenRestoreConflicts 保留无法安全恢复的隔离来源路径。
func TestMoveFileRetainsQuarantinedSourceWhenRestoreConflicts(t *testing.T) {
	root, sourceBase, destinationBase := openMoveFileRaceRoots(t)
	sourcePath := filepath.Join(sourceBase, "source.txt")
	destinationPath := filepath.Join(destinationBase, "moved.txt")
	approvedContent := []byte("approved file")
	externalContent := []byte("external file")
	changedContent := []byte("modified file")
	if len(approvedContent) != len(externalContent) || len(approvedContent) != len(changedContent) {
		t.Fatal("fixture contents must have equal sizes")
	}
	if err := os.WriteFile(sourcePath, approvedContent, 0o600); err != nil {
		t.Fatal(err)
	}
	sourceRelative := strings.TrimPrefix(filepath.ToSlash(sourcePath), "/")
	destinationRelative := strings.TrimPrefix(filepath.ToSlash(destinationPath), "/")
	entry, err := root.LstatEntry(sourceRelative)
	if err != nil {
		t.Fatal(err)
	}
	ctx := &moveFileQuarantineHookContext{Context: context.Background(), source: sourcePath, destination: destinationPath, hook: func(quarantined string) error {
		if err := os.WriteFile(quarantined, changedContent, 0o600); err != nil {
			return err
		}
		if err := os.Chtimes(quarantined, entry.ModifiedAt, entry.ModifiedAt); err != nil {
			return err
		}
		return os.WriteFile(sourcePath, externalContent, 0o600)
	}}

	outcome, err := root.MoveFileNoReplace(ctx, sourceRelative, destinationRelative, entry.Fingerprint())
	if !ctx.triggered {
		t.Fatal("quarantine mutation hook did not run")
	}
	if outcome.SourceRemoved || outcome.SourceRetained == "" || !errors.Is(err, ErrConflict) {
		t.Fatalf("move outcome = (%+v, %v), want retained quarantine and conflict", outcome, err)
	}
	retainedPath := filepath.Join("/", filepath.FromSlash(outcome.SourceRetained))
	if data, err := os.ReadFile(retainedPath); err != nil || string(data) != string(changedContent) {
		t.Fatalf("retained source = (%q, %v), want modified content", data, err)
	}
	if data, err := os.ReadFile(sourcePath); err != nil || string(data) != string(externalContent) {
		t.Fatalf("replacement source = (%q, %v), want external content", data, err)
	}
	if data, err := os.ReadFile(destinationPath); err != nil || string(data) != string(approvedContent) {
		t.Fatalf("destination = (%q, %v), want approved content", data, err)
	}
	if err := os.Remove(sourcePath); err != nil {
		t.Fatal(err)
	}
	report, err := root.RecoverInterruptedMoves(context.Background())
	if err != nil || report.Restored != 1 || report.Pending != 0 {
		t.Fatalf("RecoverInterruptedMoves() = (%+v, %v), want retained source restored", report, err)
	}
	if data, err := os.ReadFile(sourcePath); err != nil || string(data) != string(changedContent) {
		t.Fatalf("recovered changed source = (%q, %v), want preserved quarantine content", data, err)
	}
}

func openMoveFileRaceRoots(t *testing.T) (*Root, string, string) {
	t.Helper()
	rootFS, err := os.OpenRoot("/")
	if err != nil {
		t.Skipf("open host filesystem root: %v", err)
	}
	root := &Root{root: rootFS, rootPath: "/"}
	t.Cleanup(func() { _ = root.Close() })
	if err := root.ConfigureMoveRecoveryDirectory(filepath.Join(t.TempDir(), "move-recovery")); err != nil {
		t.Fatal(err)
	}
	sourceBase, err := os.MkdirTemp("/tmp", "filesage-file-race-source-")
	if err != nil {
		t.Skipf("create source fixture: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sourceBase) })
	destinationBase, err := os.MkdirTemp("/dev/shm", "filesage-file-race-target-")
	if err != nil {
		t.Skipf("create destination fixture: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(destinationBase) })
	sourceMount, err := root.MountID(strings.TrimPrefix(filepath.ToSlash(sourceBase), "/"))
	if err != nil {
		t.Skipf("read source mount identity: %v", err)
	}
	destinationMount, err := root.MountID(strings.TrimPrefix(filepath.ToSlash(destinationBase), "/"))
	if err != nil {
		t.Skipf("read destination mount identity: %v", err)
	}
	if sourceMount == destinationMount {
		t.Skip("/tmp and /dev/shm share a mount in this environment")
	}
	return root, sourceBase, destinationBase
}

type moveFileUnlinkHookContext struct {
	context.Context
	destination string
	hook        func() error
	checks      int
	triggered   bool
}

// sameMountMoveRaceContext 在同卷文件校验后模拟外部替换。
type sameMountMoveRaceContext struct {
	context.Context
	source      string
	original    string
	replacement []byte
	triggerAt   int
	checks      int
	triggered   bool
}

// Err 在指定检查点替换来源。
func (ctx *sameMountMoveRaceContext) Err() error {
	ctx.checks++
	if !ctx.triggered && ctx.checks == ctx.triggerAt {
		ctx.triggered = true
		if err := os.Rename(ctx.source, ctx.original); err != nil {
			return err
		}
		if err := os.WriteFile(ctx.source, ctx.replacement, 0o600); err != nil {
			return err
		}
	}
	return ctx.Context.Err()
}

type moveFileQuarantineHookContext struct {
	context.Context
	source      string
	destination string
	hook        func(string) error
	triggered   bool
}

func (ctx *moveFileQuarantineHookContext) Err() error {
	if !ctx.triggered {
		if _, err := os.Lstat(ctx.destination); err == nil {
			entries, readErr := os.ReadDir(filepath.Dir(ctx.source))
			if readErr != nil {
				return readErr
			}
			for _, entry := range entries {
				if !strings.HasPrefix(entry.Name(), ".filesage-move-quarantine-") {
					continue
				}
				quarantined := filepath.Join(filepath.Dir(ctx.source), entry.Name(), "source")
				if _, err := os.Lstat(quarantined); err == nil {
					ctx.triggered = true
					if err := ctx.hook(quarantined); err != nil {
						return err
					}
					break
				} else if !os.IsNotExist(err) {
					return err
				}
			}
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	return ctx.Context.Err()
}

func (ctx *moveFileUnlinkHookContext) Err() error {
	if !ctx.triggered {
		if _, err := os.Lstat(ctx.destination); err == nil {
			ctx.checks++
			if ctx.checks == 2 {
				ctx.triggered = true
				if err := ctx.hook(); err != nil {
					return err
				}
			}
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	return ctx.Context.Err()
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

func TestReplaceAtomicIfUnchangedPreservesReplacementAtPublishBoundary(t *testing.T) {
	for _, test := range []struct {
		name       string
		mutate     func(string, string, time.Time) error
		wantDigest []byte
	}{
		{
			name: "path replacement",
			mutate: func(rootPath, target string, _ time.Time) error {
				replacement := filepath.Join(rootPath, "replacement.txt")
				if err := os.WriteFile(replacement, []byte("external update"), 0o640); err != nil {
					return err
				}
				return os.Rename(replacement, target)
			},
			wantDigest: []byte("external update"),
		},
		{
			name: "same-size in-place update with restored mtime",
			mutate: func(_, target string, modified time.Time) error {
				if err := os.WriteFile(target, []byte("edited!"), 0o640); err != nil {
					return err
				}
				return os.Chtimes(target, modified, modified)
			},
			wantDigest: []byte("edited!"),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			rootPath := t.TempDir()
			target := filepath.Join(rootPath, "note.txt")
			const original = "initial"
			const candidate = "approved"
			if err := os.WriteFile(target, []byte(original), 0o640); err != nil {
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
			modified := entry.ModifiedAt
			tempRel := ".filesage-write-test"
			if err := os.WriteFile(filepath.Join(rootPath, tempRel), []byte(candidate), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := test.mutate(rootPath, target, modified); err != nil {
				t.Fatal(err)
			}

			err = platformReplaceAtomicIfUnchanged(root, "note.txt", tempRel, entry.Fingerprint(), sha256.Sum256([]byte(original)), sha256.Sum256([]byte(candidate)), int64(len(candidate)), 0o600)
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("publish error = %v, want ErrConflict", err)
			}
			if got, err := os.ReadFile(target); err != nil || string(got) != string(test.wantDigest) {
				t.Fatalf("external file = (%q, %v), want %q", got, err, test.wantDigest)
			}
			if got, err := os.ReadFile(filepath.Join(rootPath, tempRel)); err != nil || string(got) != candidate {
				t.Fatalf("candidate after rollback = (%q, %v), want %q", got, err, candidate)
			}
		})
	}
}

func TestReplaceAtomicIfUnchangedPublishesExpectedVersion(t *testing.T) {
	rootPath := t.TempDir()
	target := filepath.Join(rootPath, "note.txt")
	const original = "initial"
	const candidate = "approved"
	if err := os.WriteFile(target, []byte(original), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, ".filesage-write-test"), []byte(candidate), 0o600); err != nil {
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
	if err := platformReplaceAtomicIfUnchanged(root, "note.txt", ".filesage-write-test", entry.Fingerprint(), sha256.Sum256([]byte(original)), sha256.Sum256([]byte(candidate)), int64(len(candidate)), 0o600); err != nil {
		t.Fatalf("publish expected version: %v", err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != candidate {
		t.Fatalf("published file = (%q, %v), want %q", got, err, candidate)
	}
	if _, err := os.Lstat(filepath.Join(rootPath, ".filesage-write-test")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replaced source remains at staging path: %v", err)
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
