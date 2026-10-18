package duplicates

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"path"
	"sort"
	"sync"
	"testing"
	"time"

	"filesage/internal/fsroot"
)

type fakeFileSystem struct {
	mu           sync.RWMutex
	entries      []fsroot.Entry
	current      map[string]fsroot.Entry
	contents     map[string][]byte
	mutateOnEOF  string
	mutateOnOpen string
}

func (f *fakeFileSystem) Walk(ctx context.Context, visit func(fsroot.Entry) error) error {
	for _, entry := range f.entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := visit(entry); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeFileSystem) LstatEntry(name string) (fsroot.Entry, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	entry, ok := f.current[name]
	if !ok {
		return fsroot.Entry{}, errors.New("missing")
	}
	return entry, nil
}

func (f *fakeFileSystem) OpenRead(name string) (io.ReadCloser, fsroot.Entry, error) {
	f.mu.RLock()
	entry, ok := f.current[name]
	if !ok {
		f.mu.RUnlock()
		return nil, fsroot.Entry{}, errors.New("missing")
	}
	content := f.contents[name]
	mutateOnOpen := name == f.mutateOnOpen
	mutateOnEOF := name == f.mutateOnEOF
	f.mu.RUnlock()
	if mutateOnOpen {
		entry.Size++
	}
	reader := &fakeReadCloser{Reader: bytes.NewReader(content), onEOF: func() {
		if !mutateOnEOF {
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		current := f.current[name]
		current.ModifiedAt = current.ModifiedAt.Add(time.Second)
		current.ChangedAt = current.ChangedAt.Add(time.Second)
		f.current[name] = current
	}}
	return reader, entry, nil
}

type fakeReadCloser struct {
	*bytes.Reader
	onEOF  func()
	called bool
}

func (f *fakeReadCloser) Read(p []byte) (int, error) {
	n, err := f.Reader.Read(p)
	if err == io.EOF && !f.called {
		f.called = true
		f.onEOF()
	}
	return n, err
}

func (f *fakeReadCloser) Close() error { return nil }

func newFakeFileSystem(contents map[string][]byte) *fakeFileSystem {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	filesystem := &fakeFileSystem{current: make(map[string]fsroot.Entry), contents: contents}
	for index, name := range sortedContentPaths(contents) {
		entry := fsroot.Entry{
			Name: path.Base(name), Path: name, Kind: fsroot.KindFile, Size: int64(len(contents[name])),
			ModifiedAt: now, Mode: 0o644, Device: 1, Inode: uint64(index + 1), Links: 1,
			IdentityAvailable: true, ChangedAt: now, ChangeTimeAvailable: true,
		}
		filesystem.entries = append(filesystem.entries, entry)
		filesystem.current[name] = entry
	}
	return filesystem
}

func sortedContentPaths(contents map[string][]byte) []string {
	paths := make([]string, 0, len(contents))
	for name := range contents {
		paths = append(paths, name)
	}
	sort.Strings(paths)
	return paths
}

func TestScanRequiresFullContentMatch(t *testing.T) {
	filesystem := newFakeFileSystem(map[string][]byte{
		"a.txt": []byte("same"),
		"b.txt": []byte("same"),
		"c.txt": []byte("sane"),
		"d.txt": []byte("other length"),
	})

	groups, err := Scan(context.Background(), filesystem, nil, Options{Workers: 2}, nil)
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if len(groups) != 1 || len(groups[0].Files) != 2 {
		t.Fatalf("Scan() groups = %#v, want one two-file group", groups)
	}
	wantHash := sha256.Sum256([]byte("same"))
	if groups[0].SHA256 != hex.EncodeToString(wantHash[:]) || groups[0].Size != 4 {
		t.Fatalf("Scan() group = %#v, want full SHA-256 and size", groups[0])
	}
	if groups[0].Files[0].Path != "a.txt" || groups[0].Files[1].Path != "b.txt" {
		t.Fatalf("Scan() paths = %#v, want stable path order", groups[0].Files)
	}
}

func TestScanScopesAndCollapsesHardLinkAliases(t *testing.T) {
	filesystem := newFakeFileSystem(map[string][]byte{
		"docs/a.txt":   []byte("same"),
		"docs/alias":   []byte("same"),
		"docs/b.txt":   []byte("same"),
		"other/c.txt":  []byte("same"),
		"docs/link":    []byte("same"),
		"docs/ignored": []byte("unique"),
	})
	directory := fsroot.Entry{Name: "docs", Path: "docs", Kind: fsroot.KindDirectory}
	otherDirectory := fsroot.Entry{Name: "other", Path: "other", Kind: fsroot.KindDirectory}
	filesystem.current["docs"] = directory
	filesystem.current["other"] = otherDirectory
	filesystem.entries = append(filesystem.entries, directory, otherDirectory)
	for _, entry := range filesystem.entries {
		if entry.Path == "docs/alias" {
			alias := filesystem.current["docs/a.txt"]
			alias.Path, alias.Name, alias.Links = entry.Path, entry.Name, 2
			filesystem.current[entry.Path] = alias
			for index := range filesystem.entries {
				if filesystem.entries[index].Path == entry.Path {
					filesystem.entries[index] = alias
				}
			}
		}
		if entry.Path == "docs/link" {
			link := entry
			link.Symlink, link.Kind = true, fsroot.KindOther
			filesystem.current[entry.Path] = link
			for index := range filesystem.entries {
				if filesystem.entries[index].Path == entry.Path {
					filesystem.entries[index] = link
				}
			}
		}
	}

	groups, err := Scan(context.Background(), filesystem, []string{"docs", "docs/a.txt"}, Options{}, nil)
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if len(groups) != 1 || len(groups[0].Files) != 2 {
		t.Fatalf("Scan() groups = %#v, want alias excluded and outside scope ignored", groups)
	}
	if groups[0].Files[0].Path != "docs/a.txt" || groups[0].Files[1].Path != "docs/b.txt" {
		t.Fatalf("Scan() paths = %#v", groups[0].Files)
	}
}

func TestScanRejectsChangedFilesAndInvalidScopes(t *testing.T) {
	filesystem := newFakeFileSystem(map[string][]byte{"a": []byte("same"), "b": []byte("same")})
	filesystem.mutateOnEOF = "a"
	if _, err := Scan(context.Background(), filesystem, nil, Options{}, nil); !errors.Is(err, ErrFileChanged) {
		t.Fatalf("Scan() error = %v, want ErrFileChanged", err)
	}

	filesystem = newFakeFileSystem(map[string][]byte{"a": []byte("same"), "b": []byte("same")})
	if _, err := Scan(context.Background(), filesystem, []string{"../outside"}, Options{}, nil); !errors.Is(err, ErrInvalidScope) {
		t.Fatalf("Scan() error = %v, want ErrInvalidScope", err)
	}
	if _, err := Scan(context.Background(), filesystem, []string{"", "a"}, Options{}, nil); !errors.Is(err, ErrInvalidScope) {
		t.Fatalf("mixed root scope error = %v, want ErrInvalidScope", err)
	}
}

func TestScanRejectsTreeChangesAfterEnumeration(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*fakeFileSystem)
	}{
		{
			name: "新增重复文件",
			mutate: func(filesystem *fakeFileSystem) {
				addFakeFile(filesystem, "c.txt", []byte("same"), 3)
				addFakeFile(filesystem, "d.txt", []byte("same"), 4)
			},
		},
		{
			name: "删除非候选文件",
			mutate: func(filesystem *fakeFileSystem) {
				delete(filesystem.current, "unrelated.bin")
				delete(filesystem.contents, "unrelated.bin")
				for index, entry := range filesystem.entries {
					if entry.Path == "unrelated.bin" {
						filesystem.entries = append(filesystem.entries[:index], filesystem.entries[index+1:]...)
						break
					}
				}
			},
		},
		{
			name: "新增空目录",
			mutate: func(filesystem *fakeFileSystem) {
				entry := fsroot.Entry{Name: "new-folder", Path: "new-folder", Kind: fsroot.KindDirectory, Mode: 0o755}
				filesystem.current[entry.Path] = entry
				filesystem.entries = append(filesystem.entries, entry)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			filesystem := newFakeFileSystem(map[string][]byte{
				"a.txt":         []byte("same"),
				"b.txt":         []byte("same"),
				"unrelated.bin": []byte("different size"),
			})
			changed := false
			_, err := Scan(context.Background(), filesystem, nil, Options{}, func(progress Progress) {
				if progress.Phase == "hashing" && !changed {
					changed = true
					test.mutate(filesystem)
				}
			})
			if !changed || !errors.Is(err, ErrFileChanged) {
				t.Fatalf("Scan() error = %v, mutation observed = %t; want ErrFileChanged", err, changed)
			}
		})
	}
}

func addFakeFile(filesystem *fakeFileSystem, name string, content []byte, inode uint64) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	entry := fsroot.Entry{
		Name: path.Base(name), Path: name, Kind: fsroot.KindFile, Size: int64(len(content)),
		ModifiedAt: now, Mode: 0o644, Device: 1, Inode: inode, Links: 1,
		IdentityAvailable: true, ChangedAt: now, ChangeTimeAvailable: true,
	}
	filesystem.current[name] = entry
	filesystem.contents[name] = content
	filesystem.entries = append(filesystem.entries, entry)
}

func TestScanEnforcesEntryAndByteLimits(t *testing.T) {
	filesystem := newFakeFileSystem(map[string][]byte{"a": []byte("same"), "b": []byte("same"), "c": []byte("same")})
	if _, err := Scan(context.Background(), filesystem, nil, Options{MaxEntries: 2}, nil); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("entry limit error = %v, want ErrLimitExceeded", err)
	}
	if _, err := Scan(context.Background(), filesystem, nil, Options{MaxBytes: 4}, nil); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("byte limit error = %v, want ErrLimitExceeded", err)
	}
}

func TestScanReportsProgressAndCancellation(t *testing.T) {
	filesystem := newFakeFileSystem(map[string][]byte{"a": []byte("same"), "b": []byte("same")})
	var reports []Progress
	groups, err := Scan(context.Background(), filesystem, nil, Options{}, func(progress Progress) {
		reports = append(reports, progress)
	})
	if err != nil || len(groups) != 1 || len(reports) < 2 {
		t.Fatalf("Scan() = (%#v, %v), progress reports = %d", groups, err, len(reports))
	}
	last := reports[len(reports)-1]
	if last.FilesHashed != 2 || last.BytesHashed != 8 || last.TotalBytes != 8 {
		t.Fatalf("final progress = %#v", last)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Scan(ctx, filesystem, nil, Options{}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Scan() error = %v, want context.Canceled", err)
	}
}
