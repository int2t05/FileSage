// 本文件验证根目录写入、流式发布与并发冲突。
package test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"filesage/internal/fsroot"
)

func TestRootMkdirRemoveAndRenameNoReplace(t *testing.T) {
	rootPath := t.TempDir()
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })

	if err := root.Mkdir("created", 0o750); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	if err := root.Mkdir("created", 0o750); !errors.Is(err, fsroot.ErrConflict) {
		t.Errorf("Mkdir(existing) error = %v, want ErrConflict", err)
	}
	if err := root.Remove(""); !errors.Is(err, fsroot.ErrInvalidPath) {
		t.Errorf("Remove(root) error = %v, want ErrInvalidPath", err)
	}

	if err := os.WriteFile(filepath.Join(rootPath, "source.txt"), []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := root.Mkdir("../outside", 0o700); !errors.Is(err, fsroot.ErrInvalidPath) {
		t.Errorf("Mkdir(traversal) error = %v, want ErrInvalidPath", err)
	}
	if err := root.RenameNoReplace("source.txt", "../outside.txt"); !errors.Is(err, fsroot.ErrInvalidPath) {
		t.Errorf("RenameNoReplace(traversal) error = %v, want ErrInvalidPath", err)
	}
	if err := root.Remove("../source.txt"); !errors.Is(err, fsroot.ErrInvalidPath) {
		t.Errorf("Remove(traversal) error = %v, want ErrInvalidPath", err)
	}
	if err := root.WriteAtomicIfUnchanged("../new.txt", fsroot.Fingerprint{}, []byte("new"), 0o600); !errors.Is(err, fsroot.ErrInvalidPath) {
		t.Errorf("WriteAtomicIfUnchanged(traversal) error = %v, want ErrInvalidPath", err)
	}
	if runtime.GOOS != "linux" {
		if err := root.RenameNoReplace("source.txt", "renamed.txt"); !errors.Is(err, fsroot.ErrNoReplaceUnsupported) {
			t.Errorf("RenameNoReplace() on %s error = %v, want unsupported", runtime.GOOS, err)
		}
	} else {
		if err := root.RenameNoReplace("source.txt", "renamed.txt"); err != nil {
			t.Fatalf("RenameNoReplace() error = %v", err)
		}
		if data, err := os.ReadFile(filepath.Join(rootPath, "renamed.txt")); err != nil || string(data) != "source" {
			t.Fatalf("renamed file = (%q, %v), want source", data, err)
		}

		if err := os.WriteFile(filepath.Join(rootPath, "still-here.txt"), []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := root.RenameNoReplace("renamed.txt", "still-here.txt"); !errors.Is(err, fsroot.ErrConflict) {
			t.Fatalf("RenameNoReplace(existing destination) error = %v, want ErrConflict", err)
		}
		if data, err := os.ReadFile(filepath.Join(rootPath, "still-here.txt")); err != nil || string(data) != "keep" {
			t.Fatalf("existing destination changed: (%q, %v)", data, err)
		}
		if _, err := os.Stat(filepath.Join(rootPath, "renamed.txt")); err != nil {
			t.Fatalf("source disappeared after destination conflict: %v", err)
		}
	}

	if err := os.Mkdir(filepath.Join(rootPath, "nonempty"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "nonempty", "child"), []byte("child"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := root.Remove("nonempty"); err == nil {
		t.Fatal("Remove(nonempty directory) succeeded")
	}
	if _, err := os.Stat(filepath.Join(rootPath, "nonempty", "child")); err != nil {
		t.Fatalf("failed Remove(nonempty) changed its contents: %v", err)
	}
	if err := root.Remove("nonempty/child"); err != nil {
		t.Fatalf("Remove(file) error = %v", err)
	}
	if err := root.Remove("nonempty"); err != nil {
		t.Fatalf("Remove(empty directory) error = %v", err)
	}
}

func TestRootRemoveDeletesLinkItselfAndRejectsEscapingParents(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux symlink semantics")
	}
	base := t.TempDir()
	rootPath := filepath.Join(base, "managed")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(rootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	outsideFile := filepath.Join(outside, "important.txt")
	if err := os.WriteFile(outsideFile, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideFile, filepath.Join(rootPath, "outside-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(rootPath, "outside-dir")); err != nil {
		t.Fatal(err)
	}
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })

	if err := root.Remove("outside-dir/important.txt"); !errors.Is(err, fsroot.ErrOutsideRoot) {
		t.Errorf("Remove(through outside link) error = %v, want ErrOutsideRoot", err)
	}
	if err := root.Mkdir("outside-dir/new", 0o700); !errors.Is(err, fsroot.ErrOutsideRoot) {
		t.Errorf("Mkdir(through outside link) error = %v, want ErrOutsideRoot", err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "source.txt"), []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := root.RenameNoReplace("source.txt", "outside-dir/moved.txt"); !errors.Is(err, fsroot.ErrOutsideRoot) {
		t.Errorf("RenameNoReplace(through outside link) error = %v, want ErrOutsideRoot", err)
	}
	if err := root.Remove("outside-link"); err != nil {
		t.Fatalf("Remove(outside symlink itself) error = %v", err)
	}
	if data, err := os.ReadFile(outsideFile); err != nil || string(data) != "keep" {
		t.Fatalf("removing link changed outside target: (%q, %v)", data, err)
	}
}

func TestRootWriteAtomicIfUnchangedCreatesAndReplaces(t *testing.T) {
	rootPath := t.TempDir()
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	if runtime.GOOS != "linux" {
		if err := root.WriteAtomicIfUnchanged("new.txt", fsroot.Fingerprint{}, []byte("new"), 0o600); !errors.Is(err, fsroot.ErrAtomicWriteUnsupported) {
			t.Fatalf("WriteAtomicIfUnchanged() on %s error = %v, want unsupported", runtime.GOOS, err)
		}
		return
	}

	if err := root.WriteAtomicIfUnchanged("new.txt", fsroot.Fingerprint{}, []byte("first"), 0o640); err != nil {
		t.Fatalf("create with absent fingerprint: %v", err)
	}
	if err := root.WriteAtomicIfUnchanged("new.txt", fsroot.Fingerprint{}, []byte("must not replace"), 0o600); !errors.Is(err, fsroot.ErrConflict) {
		t.Fatalf("create over existing file error = %v, want ErrConflict", err)
	}
	if data, err := os.ReadFile(filepath.Join(rootPath, "new.txt")); err != nil || string(data) != "first" {
		t.Fatalf("unexpected new.txt after conflict: (%q, %v)", data, err)
	}
	if info, err := os.Stat(filepath.Join(rootPath, "new.txt")); err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("new file mode = (%v, %v), want 0640", info, err)
	}

	entry := fsrootEntry(t, root, "new.txt")
	oldFingerprint := entry.Fingerprint()
	if err := root.WriteAtomicIfUnchanged("new.txt", oldFingerprint, []byte("second complete version"), 0o600); err != nil {
		t.Fatalf("replace with matching fingerprint: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(rootPath, "new.txt")); err != nil || string(data) != "second complete version" {
		t.Fatalf("replacement content = (%q, %v)", data, err)
	}
	if info, err := os.Stat(filepath.Join(rootPath, "new.txt")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("replacement mode = (%v, %v), want 0600", info, err)
	}
	if err := root.WriteAtomicIfUnchanged("new.txt", oldFingerprint, []byte("stale"), 0o600); !errors.Is(err, fsroot.ErrConflict) {
		t.Fatalf("stale fingerprint error = %v, want ErrConflict", err)
	}
	if data, err := os.ReadFile(filepath.Join(rootPath, "new.txt")); err != nil || string(data) != "second complete version" {
		t.Fatalf("stale write changed content: (%q, %v)", data, err)
	}
}

func TestRootWriteAtomicIfUnchangedRejectsStateReplacementAndHardLinks(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux inode identity")
	}
	rootPath := t.TempDir()
	filePath := filepath.Join(rootPath, "state.txt")
	if err := os.WriteFile(filePath, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	initial := fsrootEntry(t, root, "state.txt").Fingerprint()
	unknownIdentity := initial
	unknownIdentity.IdentityAvailable = false
	if err := root.WriteAtomicIfUnchanged("state.txt", unknownIdentity, []byte("unknown"), 0o600); !errors.Is(err, fsroot.ErrIdentityUnavailable) {
		t.Fatalf("write without identity error = %v, want ErrIdentityUnavailable", err)
	}

	if err := os.Chmod(filePath, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteAtomicIfUnchanged("state.txt", initial, []byte("stale mode"), 0o600); !errors.Is(err, fsroot.ErrConflict) {
		t.Fatalf("write after mode change error = %v, want ErrConflict", err)
	}
	if err := os.WriteFile(filePath, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	replacementExpected := fsrootEntry(t, root, "state.txt").Fingerprint()
	oldInfo, err := os.Stat(filePath)
	if err != nil {
		t.Fatal(err)
	}
	replacementPath := filepath.Join(rootPath, "replacement.tmp")
	if err := os.WriteFile(replacementPath, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(replacementPath, oldInfo.ModTime(), oldInfo.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacementPath, filePath); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteAtomicIfUnchanged("state.txt", replacementExpected, []byte("stale path"), 0o600); !errors.Is(err, fsroot.ErrConflict) {
		t.Fatalf("write after path replacement error = %v, want ErrConflict", err)
	}
	if data, err := os.ReadFile(filePath); err != nil || string(data) != "before" {
		t.Fatalf("state conflict changed replacement: (%q, %v)", data, err)
	}

	if err := os.WriteFile(filePath, []byte("shared"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filePath, filepath.Join(rootPath, "shared-alias.txt")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	shared := fsrootEntry(t, root, "state.txt")
	if shared.Links <= 1 {
		t.Fatalf("fixture is not hard-linked: %+v", shared)
	}
	if err := root.WriteAtomicIfUnchanged("state.txt", shared.Fingerprint(), []byte("blocked"), 0o600); !errors.Is(err, fsroot.ErrHardLinked) {
		t.Fatalf("write to hard-linked file error = %v, want ErrHardLinked", err)
	}
	for _, name := range []string{"state.txt", "shared-alias.txt"} {
		if data, err := os.ReadFile(filepath.Join(rootPath, name)); err != nil || string(data) != "shared" {
			t.Errorf("hard-linked file %s changed: (%q, %v)", name, data, err)
		}
	}
}

func TestRootWriteAtomicIfUnchangedRequiresExplicitLinkTarget(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux symlink semantics")
	}
	rootPath := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("protected"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "target.txt"), []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target.txt", filepath.Join(rootPath, "alias.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".", filepath.Join(rootPath, "dir-alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(rootPath, "outside-alias")); err != nil {
		t.Fatal(err)
	}
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	alias := fsrootEntry(t, root, "alias.txt")
	if err := root.WriteAtomicIfUnchanged("alias.txt", alias.Fingerprint(), []byte("through link"), 0o600); !errors.Is(err, fsroot.ErrSymlinkPath) {
		t.Fatalf("write through final symlink error = %v, want ErrSymlinkPath", err)
	}
	target := fsrootEntry(t, root, "target.txt")
	if err := root.WriteAtomicIfUnchanged("dir-alias/target.txt", target.Fingerprint(), []byte("through parent"), 0o600); !errors.Is(err, fsroot.ErrSymlinkPath) {
		t.Fatalf("write through parent symlink error = %v, want ErrSymlinkPath", err)
	}
	if err := root.WriteAtomicIfUnchanged("outside-alias", fsroot.Fingerprint{}, []byte("outside write"), 0o600); !errors.Is(err, fsroot.ErrSymlinkPath) {
		t.Fatalf("write through outside symlink error = %v, want ErrSymlinkPath", err)
	}
	if err := root.WriteAtomicIfUnchanged("target.txt", target.Fingerprint(), []byte("direct write"), 0o600); err != nil {
		t.Fatalf("write to explicit direct target error = %v", err)
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "protected" {
		t.Fatalf("rejected outside write changed target: (%q, %v)", data, err)
	}
}

func TestRootAtomicReplacementReadersSeeOnlyWholeFiles(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux atomic rename semantics")
	}
	rootPath := t.TempDir()
	const size = 256 * 1024
	first := bytes.Repeat([]byte{'A'}, size)
	second := bytes.Repeat([]byte{'B'}, size)
	if err := os.WriteFile(filepath.Join(rootPath, "live.bin"), first, 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })

	var readerCount atomic.Int64
	stop := make(chan struct{})
	errCh := make(chan error, 1)
	var readers sync.WaitGroup
	for range 3 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				file, _, err := root.OpenRead("live.bin")
				if errors.Is(err, fsroot.ErrPathChanged) {
					continue
				}
				if err != nil {
					select {
					case errCh <- err:
					default:
					}
					return
				}
				data, readErr := io.ReadAll(file)
				closeErr := file.Close()
				if readErr != nil || closeErr != nil {
					select {
					case errCh <- errors.Join(readErr, closeErr):
					default:
					}
					return
				}
				if !bytes.Equal(data, first) && !bytes.Equal(data, second) {
					select {
					case errCh <- errors.New("reader observed partial or mixed file contents"):
					default:
					}
					return
				}
				readerCount.Add(1)
			}
		}()
	}

	for i := range 12 {
		entry := fsrootEntry(t, root, "live.bin")
		contents := first
		if i%2 == 0 {
			contents = second
		}
		if err := root.WriteAtomicIfUnchanged("live.bin", entry.Fingerprint(), contents, 0o600); err != nil {
			t.Fatalf("atomic replacement %d failed: %v", i, err)
		}
	}
	close(stop)
	readers.Wait()
	select {
	case err := <-errCh:
		t.Fatal(err)
	default:
	}
	if readerCount.Load() == 0 {
		t.Fatal("no concurrent read completed during atomic replacement")
	}
}

func TestFsrootWriteNewAtomicFromStreamsHashesAndSetsMode(t *testing.T) {
	rootPath := t.TempDir()
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })

	data := bytes.Repeat([]byte("streamed-file-sample-"), 32*1024)
	reader := &fsrootChunkReader{reader: bytes.NewReader(data), maxChunk: 4093}
	hash, err := root.WriteNewAtomicFrom("new.bin", reader, int64(len(data)), 0o640)
	if runtime.GOOS != "linux" {
		if !errors.Is(err, fsroot.ErrNoReplaceUnsupported) {
			t.Fatalf("WriteNewAtomicFrom() on %s error = %v, want unsupported", runtime.GOOS, err)
		}
		if _, statErr := os.Lstat(filepath.Join(rootPath, "new.bin")); !errors.Is(statErr, fs.ErrNotExist) {
			t.Fatalf("unsupported platform created target: %v", statErr)
		}
		assertNoFsrootTempFiles(t, rootPath)
		return
	}
	if err != nil {
		t.Fatalf("WriteNewAtomicFrom() error = %v", err)
	}
	checksum := sha256.Sum256(data)
	if want := hex.EncodeToString(checksum[:]); hash != want {
		t.Fatalf("hash = %q, want %q", hash, want)
	}
	if reader.bytesRead != int64(len(data)) {
		t.Fatalf("stream consumed %d bytes, want %d", reader.bytesRead, len(data))
	}
	got, err := os.ReadFile(filepath.Join(rootPath, "new.bin"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("published contents length = %d, err = %v", len(got), err)
	}
	info, err := os.Stat(filepath.Join(rootPath, "new.bin"))
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("published mode = (%v, %v), want 0640", info, err)
	}
	assertNoFsrootTempFiles(t, rootPath)
}

func TestFsrootWriteNewAtomicFromRejectsLengthErrorsAndCleansUp(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux no-replace rename support")
	}

	readFailure := errors.New("source read failed")
	tests := []struct {
		name         string
		source       io.Reader
		expectedSize int64
		wantErr      error
	}{
		{name: "short", source: bytes.NewReader([]byte("short")), expectedSize: 6, wantErr: fsroot.ErrSizeMismatch},
		{name: "long", source: bytes.NewReader([]byte("longer")), expectedSize: 5, wantErr: fsroot.ErrSizeMismatch},
		{name: "read failure", source: &fsrootFailingReader{err: readFailure}, expectedSize: 6, wantErr: readFailure},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rootPath := t.TempDir()
			root, err := fsroot.Open(rootPath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = root.Close() })

			if hash, err := root.WriteNewAtomicFrom("failed.bin", test.source, test.expectedSize, 0o600); !errors.Is(err, test.wantErr) || hash != "" {
				t.Fatalf("WriteNewAtomicFrom() = (%q, %v), want empty hash and %v", hash, err, test.wantErr)
			}
			if _, err := os.Lstat(filepath.Join(rootPath, "failed.bin")); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("failed write left target behind: %v", err)
			}
			assertNoFsrootTempFiles(t, rootPath)
		})
	}
}

func TestFsrootWriteNewAtomicFromRejectsConflictsAndUnsafePaths(t *testing.T) {
	rootPath := t.TempDir()
	outsidePath := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outsidePath, []byte("protected"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "exists.bin"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(rootPath, "real"), 0o700); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "linux" {
		if err := os.Symlink("real", filepath.Join(rootPath, "alias")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	}
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })

	if _, err := root.WriteNewAtomicFrom("exists.bin", bytes.NewReader([]byte("replace")), 7, 0o600); !errors.Is(err, fsroot.ErrConflict) {
		t.Fatalf("write over existing target error = %v, want ErrConflict", err)
	}
	if _, err := root.WriteNewAtomicFrom("../outside.txt", bytes.NewReader([]byte("escape")), 6, 0o600); !errors.Is(err, fsroot.ErrInvalidPath) {
		t.Fatalf("traversal path error = %v, want ErrInvalidPath", err)
	}
	if runtime.GOOS == "linux" {
		if _, err := root.WriteNewAtomicFrom("alias/new.bin", bytes.NewReader([]byte("link")), 4, 0o600); !errors.Is(err, fsroot.ErrSymlinkPath) {
			t.Fatalf("symlink parent error = %v, want ErrSymlinkPath", err)
		}
	}
	if _, err := root.WriteNewAtomicFrom("invalid-mode.bin", bytes.NewReader(nil), 0, 0o1000); !errors.Is(err, fs.ErrInvalid) {
		t.Fatalf("invalid mode error = %v, want fs.ErrInvalid", err)
	}
	if data, err := os.ReadFile(filepath.Join(rootPath, "exists.bin")); err != nil || string(data) != "keep" {
		t.Fatalf("conflict changed existing target: (%q, %v)", data, err)
	}
	if data, err := os.ReadFile(outsidePath); err != nil || string(data) != "protected" {
		t.Fatalf("unsafe write changed outside target: (%q, %v)", data, err)
	}
	assertNoFsrootTempFiles(t, rootPath)
}

func TestFsrootWriteNewAtomicFromConcurrentNoReplace(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux no-replace rename support")
	}
	rootPath := t.TempDir()
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })

	const workers = 12
	type writeResult struct {
		published bool
		conflict  bool
		err       error
	}
	start := make(chan struct{})
	results := make(chan writeResult, workers)
	var wait sync.WaitGroup
	for worker := range workers {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			<-start
			data := bytes.Repeat([]byte{byte('a' + worker)}, 16*1024)
			hash, err := root.WriteNewAtomicFrom("contended.bin", bytes.NewReader(data), int64(len(data)), 0o600)
			if err == nil {
				checksum := sha256.Sum256(data)
				if hash != hex.EncodeToString(checksum[:]) {
					err = fmt.Errorf("worker %d received incorrect hash %q", worker, hash)
				}
				results <- writeResult{published: err == nil, err: err}
				return
			} else if errors.Is(err, fsroot.ErrConflict) {
				results <- writeResult{conflict: true}
				return
			} else {
				err = fmt.Errorf("worker %d: %w", worker, err)
			}
			results <- writeResult{err: err}
		}(worker)
	}
	close(start)
	done := make(chan struct{})
	go func() {
		wait.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent writes did not finish")
	}
	close(results)

	successes := 0
	conflicts := 0
	for result := range results {
		if result.published {
			successes++
		} else if result.conflict {
			conflicts++
		} else if result.err != nil {
			t.Error(result.err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful writers = %d, want exactly one", successes)
	}
	if conflicts != workers-1 {
		t.Fatalf("conflicting writers = %d, want %d", conflicts, workers-1)
	}
	contents, err := os.ReadFile(filepath.Join(rootPath, "contended.bin"))
	if err != nil || len(contents) != 16*1024 || !bytes.Equal(contents, bytes.Repeat(contents[:1], len(contents))) {
		t.Fatalf("concurrent target is not one complete writer payload: len=%d err=%v", len(contents), err)
	}
	assertNoFsrootTempFiles(t, rootPath)
}

type fsrootChunkReader struct {
	reader    *bytes.Reader
	maxChunk  int
	bytesRead int64
}

func (reader *fsrootChunkReader) Read(buffer []byte) (int, error) {
	if len(buffer) > reader.maxChunk {
		buffer = buffer[:reader.maxChunk]
	}
	n, err := reader.reader.Read(buffer)
	reader.bytesRead += int64(n)
	return n, err
}

type fsrootFailingReader struct {
	err error
}

func (reader *fsrootFailingReader) Read(buffer []byte) (int, error) {
	return 0, reader.err
}

func assertNoFsrootTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".filesage-write-") {
			t.Errorf("temporary write file remains: %s", entry.Name())
		}
	}
}

func fsrootEntry(t *testing.T, root *fsroot.Root, rel string) fsroot.Entry {
	t.Helper()
	entries, err := root.List(pathParent(rel))
	if err != nil {
		t.Fatalf("List(%q) error = %v", pathParent(rel), err)
	}
	for _, entry := range entries {
		if entry.Path == rel {
			return entry
		}
	}
	t.Fatalf("List(%q) did not return %q", pathParent(rel), rel)
	return fsroot.Entry{}
}

func pathParent(rel string) string {
	parent := filepath.ToSlash(filepath.Dir(filepath.FromSlash(rel)))
	if parent == "." {
		return ""
	}
	return parent
}
