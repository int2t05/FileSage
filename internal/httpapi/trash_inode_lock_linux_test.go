//go:build linux

// 本文件验证回收站操作与硬链接别名共享写锁。
package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"filesage/internal/coordination"
	"filesage/internal/fsroot"
	"filesage/internal/store"
	trashsvc "filesage/internal/trash"
)

func TestTrashRestoreAndPurgeCoordinateHardLinkAliases(t *testing.T) {
	for _, operation := range []string{"restore", "purge"} {
		t.Run(operation, func(t *testing.T) {
			rootPath := t.TempDir()
			filePath := filepath.Join(rootPath, "source.txt")
			aliasPath := filepath.Join(rootPath, "alias.txt")
			if err := os.WriteFile(filePath, []byte("shared"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(filePath, aliasPath); err != nil {
				t.Fatal(err)
			}
			base, err := fsroot.Open(rootPath)
			if err != nil {
				t.Fatal(err)
			}
			defer base.Close()
			root := &trashLockProbe{Root: base, target: "source.txt", observed: make(chan struct{})}

			database, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "filesage.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			manager, err := trashsvc.Open(context.Background(), root, database)
			if err != nil {
				t.Fatal(err)
			}
			entry, err := root.LstatEntry("source.txt")
			if err != nil {
				t.Fatal(err)
			}
			item, err := manager.MoveToTrash(context.Background(), entry)
			if err != nil {
				t.Fatal(err)
			}

			locks := coordination.New()
			handler := NewWithTrash(root, database, manager, nil, locks)
			api := &API{files: root, locks: locks}
			aliasRelease, ok := api.lockMutationPaths(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil), "alias.txt")
			if !ok {
				t.Fatal("alias lock was not acquired")
			}
			root.armed.Store(true)

			target := "/api/v1/trash/" + item.ID
			method := http.MethodDelete
			if operation == "restore" {
				target += "/restore"
				method = http.MethodPost
			} else {
				target += "?confirmPermanent=true"
			}
			response := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				handler.ServeHTTP(response, httptest.NewRequest(method, target, nil))
				close(done)
			}()
			select {
			case <-root.observed:
			case <-time.After(2 * time.Second):
				aliasRelease()
				t.Fatal("trash mutation did not snapshot the original path")
			}
			select {
			case <-done:
				aliasRelease()
				t.Fatal("trash mutation completed while a hard-link alias held the write lock")
			case <-time.After(40 * time.Millisecond):
			}
			aliasRelease()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("trash mutation did not resume after the alias lock was released")
			}

			if operation == "restore" {
				if response.Code != http.StatusOK {
					t.Fatalf("restore status = %d: %s", response.Code, response.Body.String())
				}
				if !sameFile(t, filePath, aliasPath) {
					t.Fatal("restored file no longer shares its inode with the hard-link alias")
				}
			} else {
				if response.Code != http.StatusNoContent {
					t.Fatalf("purge status = %d: %s", response.Code, response.Body.String())
				}
				content, err := os.ReadFile(aliasPath)
				if err != nil || string(content) != "shared" {
					t.Fatalf("purge changed the remaining alias: content=%q err=%v", content, err)
				}
			}
		})
	}
}

func sameFile(t *testing.T, first, second string) bool {
	t.Helper()
	firstInfo, err := os.Stat(first)
	if err != nil {
		t.Fatal(err)
	}
	secondInfo, err := os.Stat(second)
	if err != nil {
		t.Fatal(err)
	}
	return os.SameFile(firstInfo, secondInfo)
}

type trashLockProbe struct {
	*fsroot.Root
	target   string
	observed chan struct{}
	armed    atomic.Bool
	once     sync.Once
}

func (root *trashLockProbe) LstatEntry(relativePath string) (fsroot.Entry, error) {
	entry, err := root.Root.LstatEntry(relativePath)
	if relativePath == root.target && root.armed.Load() {
		root.once.Do(func() { close(root.observed) })
	}
	return entry, err
}
