//go:build linux

package office

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCleanupStaleOfficeDirectories(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	stale := filepath.Join(root, "job-stale")
	fresh := filepath.Join(root, "job-fresh")
	unrelated := filepath.Join(root, "keep-this")
	target := t.TempDir()
	link := filepath.Join(root, "job-link")
	for _, directory := range []string{stale, fresh, unrelated} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(stale, "old-input"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-staleOfficeDirectoryAge - time.Minute)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	if err := cleanupStaleOfficeDirs(root, now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale directory remains, stat error = %v", err)
	}
	for _, path := range []string{fresh, unrelated, link, target} {
		if _, err := os.Lstat(path); err != nil {
			t.Errorf("cleanup removed or lost %q: %v", path, err)
		}
	}
}

func TestSandboxArgsHideHostRootAndHome(t *testing.T) {
	paths := executionPaths{
		oosplash:   "/usr/lib/libreoffice/program/oosplash",
		programDir: "/usr/lib/libreoffice/program",
		bwrap:      "/usr/bin/bwrap",
		prlimit:    "/usr/bin/prlimit",
		setpriv:    "/usr/bin/setpriv",
		readOnlyDir: []bindMount{
			{source: "/usr/lib/libreoffice", target: "/usr/lib/libreoffice"},
			{source: "/usr/lib/x86_64-linux-gnu", target: "/usr/lib/x86_64-linux-gnu"},
		},
		readOnlyFile: []bindMount{{source: "/lib/ld.so", target: "/lib64/ld-linux-x86-64.so.2"}},
		loaderPath:   "/usr/lib/libreoffice/program:/usr/lib/x86_64-linux-gnu",
	}
	args, err := sandboxArgs(paths, "/tmp/filesage-office-job", officeFormatMust(t, ".docx"))
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, "\x00")
	for _, required := range []string{"--unshare-all", "--unshare-user", "--disable-userns", "--tmpfs\x00/", "--clearenv", "--bind\x00/tmp/filesage-office-job\x00/work"} {
		if !strings.Contains(joined, required) {
			t.Errorf("sandbox arguments missing %q", required)
		}
	}
	if strings.Contains(joined, "--share-net") || strings.Contains(joined, "--ro-bind\x00/\x00/") {
		t.Fatal("sandbox shares the network or host root")
	}
	for index, arg := range args {
		if arg != "--ro-bind" && arg != "--bind" {
			continue
		}
		if index+2 >= len(args) {
			t.Fatalf("incomplete bind arguments at %d", index)
		}
		target := args[index+2]
		if arg == "--bind" && target != "/work" {
			t.Fatalf("unexpected writable mount target %q", target)
		}
		if arg == "--ro-bind" && !allowedSystemPath(target) {
			t.Fatalf("unexpected system mount target %q", target)
		}
		if strings.HasPrefix(target, "/home/") || strings.HasPrefix(target, "/root/") {
			t.Fatalf("user home was mounted at %q", target)
		}
	}
}

func TestConverterSuccessRemovesPrivateTemporaryFiles(t *testing.T) {
	converter, tempRoot := fakeConverter(t, "work=; while [ \"$#\" -gt 0 ]; do if [ \"$1\" = \"--bind\" ]; then work=\"$2\"; shift 3; else shift; fi; done; printf '%%PDF-1.4\\npreview' > \"$work/out/source.pdf\"", time.Second)
	pdf, err := converter.Convert(context.Background(), officeZip(t, "word/document.xml"), ".docx")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(pdf), "%PDF-") {
		t.Fatalf("preview = %q, want PDF bytes", pdf)
	}
	assertTempRootEmpty(t, tempRoot)
}

func TestConverterTimeoutAndCancellationRemovePrivateFiles(t *testing.T) {
	converter, tempRoot := fakeConverter(t, "exec /bin/sleep 4", 150*time.Millisecond)
	_, err := converter.Convert(context.Background(), officeZip(t, "word/document.xml"), ".docx")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error = %v, want deadline exceeded", err)
	}
	assertTempRootEmpty(t, tempRoot)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = converter.Convert(ctx, officeZip(t, "word/document.xml"), ".docx")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v, want canceled", err)
	}
	assertTempRootEmpty(t, tempRoot)
}

func fakeConverter(t *testing.T, script string, timeout time.Duration) (*Converter, string) {
	t.Helper()
	prlimit, err := exec.LookPath("prlimit")
	if err != nil {
		t.Skip("prlimit is required for Linux subprocess tests")
	}
	setpriv, err := exec.LookPath("setpriv")
	if err != nil {
		t.Skip("setpriv is required for Linux subprocess tests")
	}
	dependencyDir := t.TempDir()
	bwrap := filepath.Join(dependencyDir, "fake-bwrap")
	if err := os.WriteFile(bwrap, []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	tempRoot := t.TempDir()
	paths := executionPaths{
		oosplash:   "/usr/lib/libreoffice/program/oosplash",
		programDir: "/usr/lib/libreoffice/program",
		bwrap:      bwrap,
		prlimit:    prlimit,
		setpriv:    setpriv,
		readOnlyDir: []bindMount{{
			source: dependencyDir,
			target: "/usr/lib/libreoffice",
		}},
		loaderPath: "/usr/lib/libreoffice/program",
	}
	converter, err := newConverter(paths, converterOptions{timeout: timeout, tempDir: tempRoot})
	if err != nil {
		t.Fatal(err)
	}
	return converter, tempRoot
}

func assertTempRootEmpty(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary files remain after request: %v", entries)
	}
}
