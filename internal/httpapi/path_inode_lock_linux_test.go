//go:build linux

// 本文件验证硬链接写协调与路径替换复核。
package httpapi

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"filesage/internal/coordination"
	"filesage/internal/fsroot"
)

func TestMutationPathsCoordinateHardLinkAliases(t *testing.T) {
	rootPath := t.TempDir()
	firstPath := filepath.Join(rootPath, "first.txt")
	if err := os.WriteFile(firstPath, []byte("linked"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(firstPath, filepath.Join(rootPath, "alias.txt")); err != nil {
		t.Fatal(err)
	}
	base, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	root := &signalingRoot{Root: base, path: "alias.txt", seen: make(chan struct{})}

	api := &API{files: root, locks: coordination.New()}
	firstRelease, ok := api.lockMutationPaths(httptest.NewRecorder(), httptest.NewRequest("POST", "/", nil), "first.txt")
	if !ok {
		t.Fatal("first path lock was not acquired")
	}
	defer firstRelease()

	entered := make(chan struct{})
	finished := make(chan bool, 1)
	go func() {
		response := httptest.NewRecorder()
		release, acquired := api.lockMutationPaths(response, httptest.NewRequest("POST", "/", nil), "alias.txt")
		if acquired {
			close(entered)
			release()
		}
		finished <- acquired
	}()
	select {
	case <-root.seen:
	case <-time.After(2 * time.Second):
		t.Fatal("alias mutation did not inspect its inode")
	}

	select {
	case <-entered:
		t.Fatal("hard-link alias acquired a concurrent write lock")
	case <-time.After(40 * time.Millisecond):
	}
	firstRelease()
	select {
	case acquired := <-finished:
		if !acquired {
			t.Fatal("alias lock failed after the first operation released it")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("alias lock did not resume after release")
	}
}

type signalingRoot struct {
	*fsroot.Root
	path string
	seen chan struct{}
	once sync.Once
}

func (root *signalingRoot) LstatEntry(relativePath string) (fsroot.Entry, error) {
	entry, err := root.Root.LstatEntry(relativePath)
	if relativePath == root.path {
		root.once.Do(func() { close(root.seen) })
	}
	return entry, err
}

func TestMutationPathLockRejectsPathReplacementWhileWaiting(t *testing.T) {
	rootPath := t.TempDir()
	path := filepath.Join(rootPath, "source.txt")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	base, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	root := &signalingRoot{Root: base, path: "source.txt", seen: make(chan struct{})}
	locks := coordination.New()
	api := &API{files: root, locks: locks}

	pathRelease, err := locks.Lock(context.Background(), coordination.PathKeys("source.txt")...)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	done := make(chan bool, 1)
	go func() {
		release, acquired := api.lockMutationPaths(response, httptest.NewRequest("POST", "/", nil), "source.txt")
		if acquired {
			release()
		}
		done <- acquired
	}()
	select {
	case <-root.seen:
	case <-time.After(2 * time.Second):
		pathRelease()
		t.Fatal("mutation did not snapshot the path before waiting for its lock")
	}

	replacement := filepath.Join(rootPath, "replacement.tmp")
	if err := os.WriteFile(replacement, []byte("replacement"), 0o600); err != nil {
		pathRelease()
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		pathRelease()
		t.Fatal(err)
	}
	pathRelease()

	select {
	case acquired := <-done:
		if acquired {
			t.Fatal("path lock accepted an inode different from its pre-lock snapshot")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("mutation did not finish after the path lock was released")
	}
	if response.Code != 409 {
		t.Fatalf("replacement response status = %d, want 409: %s", response.Code, response.Body.String())
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "replacement" {
		t.Fatalf("replacement data was changed: content=%q err=%v", content, err)
	}
}
