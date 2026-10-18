//go:build linux

// 本文件验证 ZIP 打包的路径边界和变化检测。
package fsroot

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestWriteArchiveFormatsPreserveContentsAndEmptyDirectories(t *testing.T) {
	for _, format := range []ArchiveFormat{ArchiveFormatZIP, ArchiveFormat7Z, ArchiveFormatTAR, ArchiveFormatTARGZ} {
		t.Run(string(format), func(t *testing.T) {
			requireSevenZipArchiveTest(t, format)
			rootPath := t.TempDir()
			if err := os.MkdirAll(filepath.Join(rootPath, "bundle", "nested"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(rootPath, "bundle", "empty"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(rootPath, "bundle", "nested", "报告.txt"), []byte("ready"), 0o644); err != nil {
				t.Fatal(err)
			}
			root := openArchiveTestRoot(t, rootPath)
			var output bytes.Buffer
			err := root.WriteArchiveFormat(context.Background(), []string{"bundle"}, &output, ArchiveLimits{MaxEntries: 20, MaxBytes: 1 << 20}, format)
			if err != nil {
				t.Fatalf("WriteArchiveFormat() error = %v", err)
			}
			entries := readTestArchive(t, format, output.Bytes())
			for _, name := range []string{"bundle/", "bundle/empty/", "bundle/nested/", "bundle/nested/报告.txt"} {
				if _, exists := entries[name]; !exists {
					t.Errorf("archive is missing %q: %#v", name, entries)
				}
			}
			if got := entries["bundle/nested/报告.txt"]; got != "ready" {
				t.Errorf("file content = %q, want ready", got)
			}
		})
	}
}

func TestWriteArchiveFormatsRetainSafetyLimitsAndChangeDetection(t *testing.T) {
	for _, format := range []ArchiveFormat{ArchiveFormatZIP, ArchiveFormat7Z, ArchiveFormatTAR, ArchiveFormatTARGZ} {
		t.Run(string(format), func(t *testing.T) {
			requireSevenZipArchiveTest(t, format)
			t.Run("limit", func(t *testing.T) {
				rootPath := t.TempDir()
				if err := os.WriteFile(filepath.Join(rootPath, "item.txt"), []byte("more than one byte"), 0o600); err != nil {
					t.Fatal(err)
				}
				root := openArchiveTestRoot(t, rootPath)
				var output bytes.Buffer
				err := root.WriteArchiveFormat(context.Background(), []string{"item.txt"}, &output, ArchiveLimits{MaxEntries: 1, MaxBytes: 1}, format)
				if !errors.Is(err, ErrArchiveLimitExceeded) {
					t.Fatalf("WriteArchiveFormat() error = %v, want ErrArchiveLimitExceeded", err)
				}
			})
			t.Run("entry limit", func(t *testing.T) {
				rootPath := t.TempDir()
				if err := os.Mkdir(filepath.Join(rootPath, "bundle"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(rootPath, "bundle", "item.txt"), []byte("ready"), 0o600); err != nil {
					t.Fatal(err)
				}
				root := openArchiveTestRoot(t, rootPath)
				var output bytes.Buffer
				err := root.WriteArchiveFormat(context.Background(), []string{"bundle"}, &output, ArchiveLimits{MaxEntries: 1, MaxBytes: 1 << 20}, format)
				if !errors.Is(err, ErrArchiveLimitExceeded) {
					t.Fatalf("WriteArchiveFormat() error = %v, want ErrArchiveLimitExceeded", err)
				}
			})

			t.Run("links and special files", func(t *testing.T) {
				rootPath := t.TempDir()
				if err := os.Mkdir(filepath.Join(rootPath, "bundle"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(rootPath, "target.txt"), []byte("private"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(rootPath, "target.txt"), filepath.Join(rootPath, "bundle", "alias.txt")); err != nil {
					t.Fatal(err)
				}
				if err := unix.Mkfifo(filepath.Join(rootPath, "bundle", "pipe"), 0o600); err != nil {
					t.Fatal(err)
				}
				root := openArchiveTestRoot(t, rootPath)
				var output bytes.Buffer
				err := root.WriteArchiveFormat(context.Background(), []string{"bundle"}, &output, ArchiveLimits{MaxEntries: 20, MaxBytes: 1 << 20}, format)
				if !errors.Is(err, ErrArchiveUnsafeEntry) {
					t.Fatalf("WriteArchiveFormat() error = %v, want ErrArchiveUnsafeEntry", err)
				}
			})

			t.Run("cancellation", func(t *testing.T) {
				rootPath := t.TempDir()
				if err := os.WriteFile(filepath.Join(rootPath, "item.txt"), []byte("ready"), 0o600); err != nil {
					t.Fatal(err)
				}
				root := openArchiveTestRoot(t, rootPath)
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				var output bytes.Buffer
				err := root.WriteArchiveFormat(ctx, []string{"item.txt"}, &output, ArchiveLimits{MaxEntries: 2, MaxBytes: 1 << 20}, format)
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("WriteArchiveFormat() error = %v, want context.Canceled", err)
				}
			})
			t.Run("cancellation during output", func(t *testing.T) {
				rootPath := t.TempDir()
				if err := os.WriteFile(filepath.Join(rootPath, "item.txt"), []byte("ready"), 0o600); err != nil {
					t.Fatal(err)
				}
				root := openArchiveTestRoot(t, rootPath)
				ctx, cancel := context.WithCancel(context.Background())
				output := &archiveCancelWriter{cancel: cancel}
				err := root.WriteArchiveFormat(ctx, []string{"item.txt"}, output, ArchiveLimits{MaxEntries: 2, MaxBytes: 1 << 20}, format)
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("WriteArchiveFormat() error = %v, want context.Canceled", err)
				}
			})

			if format != ArchiveFormat7Z {
				t.Run("source changes", func(t *testing.T) {
					rootPath := t.TempDir()
					filePath := filepath.Join(rootPath, "item.txt")
					if err := os.WriteFile(filePath, bytes.Repeat([]byte("a"), 1<<20), 0o600); err != nil {
						t.Fatal(err)
					}
					root := openArchiveTestRoot(t, rootPath)
					output := &archiveMutatingWriter{mutate: func() {
						if err := os.WriteFile(filePath, []byte("changed while packing"), 0o600); err != nil {
							t.Errorf("mutate test file: %v", err)
						}
					}}
					err := root.WriteArchiveFormat(context.Background(), []string{"item.txt"}, output, ArchiveLimits{MaxEntries: 20, MaxBytes: 2 << 20}, format)
					if !errors.Is(err, ErrPathChanged) {
						t.Fatalf("WriteArchiveFormat() error = %v, want ErrPathChanged", err)
					}
				})
			}
		})
	}
}

func requireSevenZipArchiveTest(t *testing.T, format ArchiveFormat) {
	t.Helper()
	if format == ArchiveFormat7Z {
		if _, err := sevenZipBinary(); err != nil {
			t.Skip("requires the 7-Zip command")
		}
	}
}

func readTestArchive(t *testing.T, format ArchiveFormat, data []byte) map[string]string {
	t.Helper()
	entries := make(map[string]string)
	if format == ArchiveFormat7Z {
		requireSevenZipArchiveTest(t, format)
		archivePath := filepath.Join(t.TempDir(), "archive.7z")
		if err := os.WriteFile(archivePath, data, 0o600); err != nil {
			t.Fatal(err)
		}
		limits := ArchiveReadLimits{
			MaxArchiveBytes: 1 << 20, MaxExternalArchiveBytes: 1 << 20, MaxEntries: 20,
			MaxEntryBytes: 1 << 20, MaxTotalBytes: 1 << 20, MaxPreviewBytes: 1 << 20, MaxRatio: 1 << 20,
		}
		items, err := listSevenZip(context.Background(), archivePath, nil, archive7z, limits)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range items {
			if item.Kind == "directory" {
				entries[strings.TrimSuffix(item.Path, "/")+"/"] = ""
				continue
			}
			contents, readErr := readSevenZipEntry(context.Background(), archivePath, nil, archive7z, item.Path, item.Size, 1<<20)
			if readErr != nil {
				t.Fatalf("read 7z entry %q: %v", item.Path, readErr)
			}
			entries[item.Path] = string(contents)
		}
		return entries
	}
	if format == ArchiveFormatZIP {
		reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range reader.File {
			if item.FileInfo().IsDir() {
				entries[item.Name] = ""
				continue
			}
			file, err := item.Open()
			if err != nil {
				t.Fatal(err)
			}
			contents, readErr := io.ReadAll(file)
			closeErr := file.Close()
			if readErr != nil || closeErr != nil {
				t.Fatalf("read ZIP item %q: read=%v close=%v", item.Name, readErr, closeErr)
			}
			entries[item.Name] = string(contents)
		}
		return entries
	}
	reader := io.Reader(bytes.NewReader(data))
	if format == ArchiveFormatTARGZ {
		gzipReader, err := gzip.NewReader(reader)
		if err != nil {
			t.Fatal(err)
		}
		defer gzipReader.Close()
		reader = gzipReader
	}
	tarReader := tar.NewReader(reader)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeDir {
			entries[header.Name] = ""
			continue
		}
		contents, err := io.ReadAll(tarReader)
		if err != nil {
			t.Fatal(err)
		}
		entries[header.Name] = string(contents)
	}
	return entries
}

func TestWriteArchivePreservesPathsAndEmptyDirectories(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootPath, "bundle", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(rootPath, "bundle", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "bundle", "nested", "report.txt"), []byte("ready"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := openArchiveTestRoot(t, rootPath)

	var output bytes.Buffer
	err := root.WriteArchive(context.Background(), []string{"bundle", "bundle/nested/report.txt"}, &output, ArchiveLimits{MaxEntries: 20, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatalf("WriteArchive() error = %v", err)
	}

	reader, err := zip.NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatal(err)
	}
	entries := make(map[string]*zip.File, len(reader.File))
	for _, file := range reader.File {
		if _, exists := entries[file.Name]; exists {
			t.Fatalf("duplicate ZIP path %q", file.Name)
		}
		entries[file.Name] = file
	}
	for _, name := range []string{"bundle/", "bundle/empty/", "bundle/nested/", "bundle/nested/report.txt"} {
		if _, exists := entries[name]; !exists {
			t.Errorf("ZIP is missing %q", name)
		}
	}
	file, err := entries["bundle/nested/report.txt"].Open()
	if err != nil {
		t.Fatal(err)
	}
	contents, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read ZIP entry: read=%v close=%v", readErr, closeErr)
	}
	if string(contents) != "ready" {
		t.Fatalf("file contents = %q, want ready", contents)
	}
}

func TestWriteArchiveCollapsesInterleavedSelections(t *testing.T) {
	rootPath := t.TempDir()
	for _, relativePath := range []string{"a/z", "a-b/x"} {
		fullPath := filepath.Join(rootPath, filepath.FromSlash(relativePath))
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, []byte(relativePath), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	root := openArchiveTestRoot(t, rootPath)

	var output bytes.Buffer
	err := root.WriteArchive(context.Background(), []string{"a", "a-b/x", "a/z"}, &output, ArchiveLimits{MaxEntries: 20, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatalf("WriteArchive() error = %v", err)
	}
	reader, err := zip.NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool, len(reader.File))
	for _, file := range reader.File {
		if seen[file.Name] {
			t.Fatalf("duplicate ZIP path %q", file.Name)
		}
		seen[file.Name] = true
	}
	for _, name := range []string{"a/", "a/z", "a-b/x"} {
		if !seen[name] {
			t.Errorf("ZIP is missing %q", name)
		}
	}
}

func TestWriteArchiveRejectsDeepDirectoryTree(t *testing.T) {
	rootPath := t.TempDir()
	current := filepath.Join(rootPath, "bundle")
	if err := os.Mkdir(current, 0o755); err != nil {
		t.Fatal(err)
	}
	for depth := 0; depth < maxArchiveDepth; depth++ {
		current = filepath.Join(current, "d")
		if err := os.Mkdir(current, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	root := openArchiveTestRoot(t, rootPath)

	var output bytes.Buffer
	err := root.WriteArchive(context.Background(), []string{"bundle"}, &output, ArchiveLimits{MaxEntries: 1000, MaxBytes: 1 << 20})
	if !errors.Is(err, ErrArchiveLimitExceeded) {
		t.Fatalf("WriteArchive() error = %v, want ErrArchiveLimitExceeded", err)
	}
}

func TestWriteArchiveRejectsLinksAndSpecialFiles(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "bundle"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "target.txt"), []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(rootPath, "target.txt"), filepath.Join(rootPath, "bundle", "alias.txt")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(rootPath, "bundle", "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := openArchiveTestRoot(t, rootPath)

	var output bytes.Buffer
	err := root.WriteArchive(context.Background(), []string{"bundle"}, &output, ArchiveLimits{MaxEntries: 20, MaxBytes: 1 << 20})
	if !errors.Is(err, ErrArchiveUnsafeEntry) {
		t.Fatalf("WriteArchive() error = %v, want ErrArchiveUnsafeEntry", err)
	}
}

func TestWriteArchiveDetectsChangesDuringCopy(t *testing.T) {
	rootPath := t.TempDir()
	filePath := filepath.Join(rootPath, "item.txt")
	if err := os.WriteFile(filePath, bytes.Repeat([]byte("a"), 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	root := openArchiveTestRoot(t, rootPath)
	output := &archiveMutatingWriter{mutate: func() {
		if err := os.WriteFile(filePath, []byte("changed while packing"), 0o600); err != nil {
			t.Errorf("mutate test file: %v", err)
		}
	}}

	err := root.WriteArchive(context.Background(), []string{"item.txt"}, output, ArchiveLimits{MaxEntries: 20, MaxBytes: 1 << 20})
	if !errors.Is(err, ErrPathChanged) {
		t.Fatalf("WriteArchive() error = %v, want ErrPathChanged", err)
	}
}

func TestWriteArchiveEnforcesLimitsAndRejectsUnsafeNames(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "bundle"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "bundle", "item.txt"), []byte("more than one byte"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := openArchiveTestRoot(t, rootPath)
	var output bytes.Buffer
	err := root.WriteArchive(context.Background(), []string{"bundle"}, &output, ArchiveLimits{MaxEntries: 2, MaxBytes: 1})
	if !errors.Is(err, ErrArchiveLimitExceeded) {
		t.Fatalf("WriteArchive() error = %v, want ErrArchiveLimitExceeded", err)
	}

	unsafeName := "..\\escape.txt"
	if err := os.WriteFile(filepath.Join(rootPath, unsafeName), []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = root.WriteArchive(context.Background(), []string{unsafeName}, &output, ArchiveLimits{MaxEntries: 2, MaxBytes: 1 << 20})
	if !errors.Is(err, ErrInvalidPath) && !errors.Is(err, ErrArchiveUnsafeEntry) {
		t.Fatalf("unsafe path error = %v", err)
	}

	if err := os.WriteFile(filepath.Join(rootPath, "bundle", "alternate:stream"), []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = root.WriteArchive(context.Background(), []string{"bundle"}, &output, ArchiveLimits{MaxEntries: 20, MaxBytes: 1 << 20})
	if !errors.Is(err, ErrArchiveUnsafeEntry) {
		t.Fatalf("platform-ambiguous name error = %v, want ErrArchiveUnsafeEntry", err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "bundle", string([]byte{'b', 'a', 'd', 0xff})), []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = root.WriteArchive(context.Background(), []string{"bundle"}, &output, ArchiveLimits{MaxEntries: 20, MaxBytes: 1 << 20})
	if !errors.Is(err, ErrArchiveUnsafeEntry) {
		t.Fatalf("invalid UTF-8 name error = %v, want ErrArchiveUnsafeEntry", err)
	}
}

func TestWriteArchiveHonorsCancellation(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "item.txt"), []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := openArchiveTestRoot(t, rootPath)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var output bytes.Buffer
	err := root.WriteArchive(ctx, []string{"item.txt"}, &output, ArchiveLimits{MaxEntries: 2, MaxBytes: 1 << 20})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WriteArchive() error = %v, want context.Canceled", err)
	}
}

func openArchiveTestRoot(t *testing.T, path string) *Root {
	t.Helper()
	root, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}

type archiveMutatingWriter struct {
	buffer bytes.Buffer
	mutate func()
	done   bool
}

type archiveCancelWriter struct {
	buffer bytes.Buffer
	cancel context.CancelFunc
	done   bool
}

func (w *archiveCancelWriter) Write(contents []byte) (int, error) {
	if !w.done {
		w.done = true
		w.cancel()
	}
	return w.buffer.Write(contents)
}

func (w *archiveMutatingWriter) Write(contents []byte) (int, error) {
	if !w.done {
		w.done = true
		w.mutate()
	}
	return w.buffer.Write(contents)
}
