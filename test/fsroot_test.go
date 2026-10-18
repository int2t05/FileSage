package test

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"filesage/internal/fsroot"
)

func TestRootListReadAndValidateRootRelativePaths(t *testing.T) {
	base := t.TempDir()
	rootPath := filepath.Join(base, "managed")
	if err := os.MkdirAll(filepath.Join(rootPath, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rootPath, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	contents := []byte("hello FileSage\n")
	if err := os.WriteFile(filepath.Join(rootPath, "docs", "readme.txt"), contents, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "z-last.txt"), []byte("z"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "a-first.txt"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}

	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })

	entries, err := root.List("")
	if err != nil {
		t.Fatalf("List(root) error = %v", err)
	}
	if len(entries) != 4 {
		t.Fatalf("List(root) returned %d entries, want 4: %+v", len(entries), entries)
	}
	if entries[0].Name != "a-first.txt" || entries[1].Name != "docs" || entries[2].Name != "empty" || entries[3].Name != "z-last.txt" {
		t.Fatalf("List(root) is not sorted by name: %+v", entries)
	}
	if entries[1].Kind != fsroot.KindDirectory || entries[0].Kind != fsroot.KindFile {
		t.Fatalf("List(root) returned unexpected kinds: %+v", entries)
	}

	empty, err := root.List("empty")
	if err != nil || len(empty) != 0 {
		t.Fatalf("List(empty) = (%+v, %v), want an empty slice and nil error", empty, err)
	}
	documents, err := root.List("docs")
	if err != nil || len(documents) != 1 || documents[0].Path != "docs/readme.txt" {
		t.Fatalf("List(docs) = (%+v, %v), want only docs/readme.txt", documents, err)
	}

	reader, entry, err := root.OpenRead("docs/readme.txt")
	if err != nil {
		t.Fatalf("OpenRead() error = %v", err)
	}
	readContents, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read/close returned (%v, %v)", readErr, closeErr)
	}
	if string(readContents) != string(contents) {
		t.Fatalf("OpenRead() content = %q, want %q", readContents, contents)
	}
	if entry.Path != "docs/readme.txt" || entry.Name != "readme.txt" || entry.Kind != fsroot.KindFile || entry.Size != int64(len(contents)) {
		t.Fatalf("OpenRead() entry has unexpected metadata: %+v", entry)
	}

	for _, invalid := range []string{"../outside", "docs/../a-first.txt", "/etc/passwd", `C:\Windows\win.ini`, "a//b", "./docs", "docs/"} {
		if _, err := root.List(invalid); !errors.Is(err, fsroot.ErrInvalidPath) {
			t.Errorf("List(%q) error = %v, want ErrInvalidPath", invalid, err)
		}
		if _, _, err := root.OpenRead(invalid); !errors.Is(err, fsroot.ErrInvalidPath) {
			t.Errorf("OpenRead(%q) error = %v, want ErrInvalidPath", invalid, err)
		}
	}
	if _, err := root.List("docs/readme.txt"); !errors.Is(err, fsroot.ErrNotDirectory) {
		t.Errorf("List(file) error = %v, want ErrNotDirectory", err)
	}
	if _, _, err := root.OpenRead("docs"); !errors.Is(err, fsroot.ErrNotRegularFile) {
		t.Errorf("OpenRead(directory) error = %v, want ErrNotRegularFile", err)
	}
}

func TestRootRejectsNonDirectoryRoot(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := fsroot.Open(file); !errors.Is(err, fsroot.ErrNotDirectory) {
		t.Fatalf("Open(file) error = %v, want ErrNotDirectory", err)
	}
}

func TestRootListsAndReadsInternalSymlinksWithoutEscaping(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink permission and target semantics vary on Windows")
	}
	base := t.TempDir()
	rootPath := filepath.Join(base, "managed")
	outside := filepath.Join(base, "outside.txt")
	if err := os.MkdirAll(filepath.Join(rootPath, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "docs", "inside.txt"), []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(rootPath, "folder"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "folder", "child.txt"), []byte("child"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	links := []struct {
		target, name string
		directory    bool
	}{
		{target: "docs/inside.txt", name: "internal-file"},
		{target: "folder", name: "internal-dir", directory: true},
		{target: "../outside.txt", name: "escape"},
		{target: outside, name: "absolute-escape"},
	}
	for _, link := range links {
		if err := os.Symlink(link.target, filepath.Join(rootPath, link.name)); err != nil {
			t.Skipf("cannot create symlinks on this filesystem: %v", err)
		}
	}

	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })

	entries, err := root.List("")
	if err != nil {
		t.Fatal(err)
	}
	byName := make(map[string]fsroot.Entry, len(entries))
	for _, entry := range entries {
		byName[entry.Name] = entry
	}
	for _, name := range []string{"internal-file", "internal-dir"} {
		if entry := byName[name]; !entry.Symlink || entry.LinkStatus != fsroot.LinkStatusInternal {
			t.Errorf("%s entry = %+v, want an internal symlink", name, entry)
		}
	}
	if byName["internal-file"].Kind != fsroot.KindFile || byName["internal-dir"].Kind != fsroot.KindDirectory {
		t.Errorf("internal link target kinds are wrong: file=%+v dir=%+v", byName["internal-file"], byName["internal-dir"])
	}
	if byName["escape"].LinkStatus != fsroot.LinkStatusOutsideRoot || byName["absolute-escape"].LinkStatus != fsroot.LinkStatusOutsideRoot {
		t.Errorf("external links were not marked outside_root: escape=%+v absolute=%+v", byName["escape"], byName["absolute-escape"])
	}

	reader, entry, err := root.OpenRead("internal-file")
	if err != nil {
		t.Fatalf("OpenRead(internal symlink) error = %v", err)
	}
	data, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || string(data) != "inside" || !entry.Symlink || entry.LinkStatus != fsroot.LinkStatusInternal {
		t.Fatalf("OpenRead(internal symlink) = (%q, %+v, %v)", data, entry, err)
	}
	children, err := root.List("internal-dir")
	if err != nil || len(children) != 1 || children[0].Path != "internal-dir/child.txt" {
		t.Fatalf("List(internal directory link) = (%+v, %v)", children, err)
	}
	if _, _, err := root.OpenRead("escape"); !errors.Is(err, fsroot.ErrOutsideRoot) {
		t.Errorf("OpenRead(escape) error = %v, want ErrOutsideRoot", err)
	}
	if _, err := root.List("escape"); !errors.Is(err, fsroot.ErrOutsideRoot) {
		t.Errorf("List(escape) error = %v, want ErrOutsideRoot", err)
	}
}

func TestRootReportsDanglingAndCyclicSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink permission and target semantics vary on Windows")
	}
	rootPath := t.TempDir()
	for _, link := range []struct{ target, name string }{
		{target: "missing.txt", name: "dangling"},
		{target: "cycle-b", name: "cycle-a"},
		{target: "cycle-a", name: "cycle-b"},
	} {
		if err := os.Symlink(link.target, filepath.Join(rootPath, link.name)); err != nil {
			t.Skipf("cannot create symlinks on this filesystem: %v", err)
		}
	}
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	entries, err := root.List("")
	if err != nil {
		t.Fatal(err)
	}
	byName := make(map[string]fsroot.Entry, len(entries))
	for _, entry := range entries {
		byName[entry.Name] = entry
	}
	if byName["dangling"].LinkStatus != fsroot.LinkStatusDangling {
		t.Errorf("dangling link = %+v, want dangling status", byName["dangling"])
	}
	if byName["cycle-a"].LinkStatus != fsroot.LinkStatusCycle || byName["cycle-b"].LinkStatus != fsroot.LinkStatusCycle {
		t.Errorf("cycle links have wrong statuses: a=%+v b=%+v", byName["cycle-a"], byName["cycle-b"])
	}
	if _, _, err := root.OpenRead("dangling"); !errors.Is(err, fsroot.ErrDanglingLink) {
		t.Errorf("OpenRead(dangling) error = %v, want ErrDanglingLink", err)
	}
	if _, _, err := root.OpenRead("cycle-a"); !errors.Is(err, fsroot.ErrLinkCycle) {
		t.Errorf("OpenRead(cycle) error = %v, want ErrLinkCycle", err)
	}
}

func TestRootReportsHardLinkIdentityWhenAvailable(t *testing.T) {
	rootPath := t.TempDir()
	original := filepath.Join(rootPath, "original.txt")
	alias := filepath.Join(rootPath, "alias.txt")
	if err := os.WriteFile(original, []byte("same inode"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(original, alias); err != nil {
		t.Skipf("hard links unavailable on this filesystem: %v", err)
	}
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	entries, err := root.List("")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("List() returned %d entries, want 2", len(entries))
	}
	if entries[0].IdentityAvailable && entries[1].IdentityAvailable {
		if entries[0].Device != entries[1].Device || entries[0].Inode != entries[1].Inode {
			t.Errorf("hard-linked files have different identities: %+v %+v", entries[0], entries[1])
		}
		if entries[0].Links < 2 || entries[1].Links < 2 {
			t.Errorf("hard link count is less than 2: %+v %+v", entries[0], entries[1])
		}
	}
}

func TestRootDoesNotOpenSpecialFilesAsRegularFiles(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("FIFO test uses Linux mkfifo")
	}
	rootPath := t.TempDir()
	fifo := filepath.Join(rootPath, "pipe")
	if err := exec.Command("mkfifo", fifo).Run(); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	entries, err := root.List("")
	if err != nil || len(entries) != 1 || entries[0].Kind != fsroot.KindOther {
		t.Fatalf("List(FIFO) = (%+v, %v), want one other entry", entries, err)
	}
	if _, _, err := root.OpenRead("pipe"); !errors.Is(err, fsroot.ErrNotRegularFile) {
		t.Errorf("OpenRead(FIFO) error = %v, want ErrNotRegularFile", err)
	}
}
