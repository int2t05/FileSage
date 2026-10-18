// 本文件验证压缩包解析与解压的路径和资源边界。
package fsroot

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestListAndReadZIPArchive(t *testing.T) {
	root := openArchiveReadTestRoot(t)
	writeArchiveReadTestFile(t, root, "bundle.zip", makeZIPReadTest(t, map[string]string{
		"docs/contract.txt": "signed",
		"readme.md":         "hello",
	}))
	limits := archiveReadTestLimits()

	entries, err := root.ListArchive(context.Background(), "bundle.zip", "", limits)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Path != "docs" || entries[0].Kind != "directory" || entries[1].Path != "readme.md" {
		t.Fatalf("root entries = %#v", entries)
	}
	children, err := root.ListArchive(context.Background(), "bundle.zip", "docs", limits)
	if err != nil || len(children) != 1 || children[0].Path != "docs/contract.txt" {
		t.Fatalf("docs entries = %#v, error = %v", children, err)
	}
	content, entry, err := root.ReadArchiveEntry(context.Background(), "bundle.zip", "docs/contract.txt", limits)
	if err != nil || string(content) != "signed" || entry.Size != 6 {
		t.Fatalf("content = %q, entry = %#v, error = %v", content, entry, err)
	}
}

func TestExtractZIPUsesNumberedNamesWithoutOverwriting(t *testing.T) {
	skipArchiveWritesUnlessLinux(t)
	root := openArchiveReadTestRoot(t)
	writeArchiveReadTestFile(t, root, "bundle.zip", makeZIPReadTest(t, map[string]string{
		"readme.txt": "archive",
		"docs/a.txt": "a",
	}))
	if err := os.WriteFile(filepath.Join(root.rootPath, "readme.txt"), []byte("existing"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root.rootPath, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root.rootPath, "docs", "keep.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := root.ExtractArchive(context.Background(), "bundle.zip", "", nil, archiveReadTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	if got := string(readArchiveReadTestFile(t, root, "readme (1).txt")); got != "archive" {
		t.Fatalf("numbered file = %q", got)
	}
	if got := string(readArchiveReadTestFile(t, root, "readme.txt")); got != "existing" {
		t.Fatalf("existing file was changed: %q", got)
	}
	if got := string(readArchiveReadTestFile(t, root, "docs (1)/a.txt")); got != "a" {
		t.Fatalf("numbered directory content = %q", got)
	}
	if got := string(readArchiveReadTestFile(t, root, "docs/keep.txt")); got != "keep" {
		t.Fatalf("existing directory was changed: %q", got)
	}
	if len(result.Paths) != 3 {
		t.Fatalf("published paths = %#v", result.Paths)
	}
}

func TestExtractArchiveApprovedUsesExactRootPathsWithoutRenaming(t *testing.T) {
	skipArchiveWritesUnlessLinux(t)
	root := openArchiveReadTestRoot(t)
	writeArchiveReadTestFile(t, root, "bundle.zip", makeZIPReadTest(t, map[string]string{
		"docs/readme.txt": "approved",
		"root.txt":        "root",
	}))
	limits := archiveReadTestLimits()
	manifest, err := root.InspectArchive(context.Background(), "bundle.zip", limits)
	if err != nil {
		t.Fatal(err)
	}
	rootSnapshot := Fingerprint{Exists: true, Kind: KindDirectory}
	paths := archiveOutputPaths("", manifest.Entries)

	result, err := root.ExtractArchiveApproved(context.Background(), "bundle.zip", "", manifest, rootSnapshot, paths, limits)
	if err != nil || !sameArchivePaths(result.Paths, paths) {
		t.Fatalf("approved root extraction = (%#v, %v), want %#v", result, err, paths)
	}
	if got := string(readArchiveReadTestFile(t, root, "docs/readme.txt")); got != "approved" {
		t.Fatalf("extracted content = %q", got)
	}

	conflictRoot := openArchiveReadTestRoot(t)
	writeArchiveReadTestFile(t, conflictRoot, "bundle.zip", makeZIPReadTest(t, map[string]string{"root.txt": "archive"}))
	conflictManifest, err := conflictRoot.InspectArchive(context.Background(), "bundle.zip", limits)
	if err != nil {
		t.Fatal(err)
	}
	writeArchiveReadTestFile(t, conflictRoot, "root.txt", []byte("keep"))
	conflictPaths := archiveOutputPaths("", conflictManifest.Entries)
	partial, err := conflictRoot.ExtractArchiveApproved(context.Background(), "bundle.zip", "", conflictManifest, rootSnapshot, conflictPaths, limits)
	if !errors.Is(err, ErrConflict) || len(partial.Paths) != 0 {
		t.Fatalf("conflicting approved extraction = (%#v, %v), want conflict without writes", partial, err)
	}
	if got := string(readArchiveReadTestFile(t, conflictRoot, "root.txt")); got != "keep" {
		t.Fatalf("existing target changed: %q", got)
	}
	if _, err := conflictRoot.LstatEntry("root (1).txt"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Agent extraction dynamically renamed conflict: %v", err)
	}
}

func TestExtractArchiveReportsPublishedPathsWhenDurabilityIsUncertain(t *testing.T) {
	skipArchiveWritesUnlessLinux(t)
	for _, test := range []struct {
		name    string
		entries map[string]string
	}{
		{name: "single file", entries: map[string]string{"readme.txt": "published"}},
		{name: "directory member", entries: map[string]string{"docs/": "", "docs/readme.txt": "published"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := openArchiveReadTestRoot(t)
			writeArchiveReadTestFile(t, root, "bundle.zip", makeZIPReadTest(t, test.entries))
			limits := archiveReadTestLimits()
			manifest, err := root.InspectArchive(context.Background(), "bundle.zip", limits)
			if err != nil {
				t.Fatal(err)
			}
			paths := archiveOutputPaths("", manifest.Entries)
			publish := func(relativePath string, source io.Reader, size int64, mode os.FileMode) (string, error) {
				digest, writeErr := root.WriteNewAtomicFrom(relativePath, source, size, mode)
				if writeErr == nil {
					writeErr = ErrPublishedNotDurable
				}
				return digest, writeErr
			}
			result, err := root.extractArchiveWithPublisher(context.Background(), "bundle.zip", "", nil, limits,
				&archiveExtractExpectation{
					manifest: manifest, destination: Fingerprint{Exists: true, Kind: KindDirectory}, paths: paths,
				}, publish)
			if !errors.Is(err, ErrPublishedNotDurable) || !sameArchivePaths(result.Paths, paths) {
				t.Fatalf("durability failure result = (%#v, %v), want published paths %#v", result, err, paths)
			}
			if len(result.Errors) == 0 || !strings.Contains(result.Errors[0].Message, "发布状态需核查") {
				t.Fatalf("durability result errors = %#v", result.Errors)
			}
		})
	}
}

func TestExtractTARPreservesSelectedDirectoryTree(t *testing.T) {
	skipArchiveWritesUnlessLinux(t)
	root := openArchiveReadTestRoot(t)
	writeArchiveReadTestFile(t, root, "bundle.tar", makeTARReadTest(t, []tarReadTestEntry{
		{name: "media", directory: true},
		{name: "media/photo.jpg", content: []byte("image")},
	}))
	result, err := root.ExtractArchive(context.Background(), "bundle.tar", "", []string{"media"}, archiveReadTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	if got := string(readArchiveReadTestFile(t, root, "media/photo.jpg")); got != "image" {
		t.Fatalf("extracted file = %q", got)
	}
	if len(result.Paths) != 2 {
		t.Fatalf("published paths = %#v", result.Paths)
	}
}

func TestListAndReadTARGZArchive(t *testing.T) {
	root := openArchiveReadTestRoot(t)
	tarBytes := makeTARReadTest(t, []tarReadTestEntry{{name: "reports/summary.txt", content: []byte("monthly")}})
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(tarBytes); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	writeArchiveReadTestFile(t, root, "bundle.tar.gz", compressed.Bytes())
	entries, err := root.ListArchive(context.Background(), "bundle.tar.gz", "reports", archiveReadTestLimits())
	if err != nil || len(entries) != 1 || entries[0].Path != "reports/summary.txt" {
		t.Fatalf("entries = %#v, error = %v", entries, err)
	}
	content, _, err := root.ReadArchiveEntry(context.Background(), "bundle.tar.gz", "reports/summary.txt", archiveReadTestLimits())
	if err != nil || string(content) != "monthly" {
		t.Fatalf("content = %q, error = %v", content, err)
	}
}

func TestTARGZTrailingExpandedDataIsBounded(t *testing.T) {
	root := openArchiveReadTestRoot(t)
	var packed bytes.Buffer
	compressor := gzip.NewWriter(&packed)
	tarWriter := tar.NewWriter(compressor)
	if err := tarWriter.WriteHeader(&tar.Header{Name: "small.txt", Mode: 0o600, Size: 1, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := compressor.Write(make([]byte, 2<<20)); err != nil {
		t.Fatal(err)
	}
	if err := compressor.Close(); err != nil {
		t.Fatal(err)
	}
	writeArchiveReadTestFile(t, root, "trailing.tar.gz", packed.Bytes())
	limits := archiveReadTestLimits()
	limits.MaxEntries = 1
	limits.MaxEntryBytes = 16
	limits.MaxTotalBytes = 16
	if _, err := root.ListArchive(context.Background(), "trailing.tar.gz", "", limits); !errors.Is(err, ErrArchiveLimitExceeded) {
		t.Fatalf("ListArchive() error = %v, want ErrArchiveLimitExceeded", err)
	}
}

func TestSevenZipInputUsesInheritedDescriptorOnLinux(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "archive-input")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString("archive bytes"); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	input, extraFiles, cleanup, err := prepareSevenZipInput(context.Background(), archiveOpened{
		file: file, entry: Entry{Size: int64(len("archive bytes"))}, format: archiveRAR,
	}, archiveReadTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if archiveProcFDPath() != "" {
		if input != "/proc/self/fd/3" || len(extraFiles) != 1 || extraFiles[0] != file {
			t.Fatalf("Linux 7-Zip input = %q, extra files = %#v", input, extraFiles)
		}
	} else if input == "" || len(extraFiles) != 0 {
		t.Fatalf("fallback 7-Zip input = %q, extra files = %#v", input, extraFiles)
	}
	if sevenZipTypeFlag(archive7z) != "-t7z" || sevenZipTypeFlag(archiveRAR) != "-trar" {
		t.Fatalf("archive type flags = %q, %q", sevenZipTypeFlag(archive7z), sevenZipTypeFlag(archiveRAR))
	}
}

func TestArchiveStageAllowsDirectoryLinkCountFromChildren(t *testing.T) {
	stage := t.TempDir()
	if err := os.Mkdir(filepath.Join(stage, "media"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "media", "photo.jpg"), []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	selected := []ArchiveEntry{
		{Path: "media", Kind: "directory"},
		{Path: "media/photo.jpg", Kind: "file", Size: int64(len("image"))},
	}
	if err := verifyArchiveStage(stage, selected); err != nil {
		t.Fatalf("verifyArchiveStage() = %v, want directory link count accepted", err)
	}
}

func TestSevenZipRoundTripWhenInstalled(t *testing.T) {
	skipArchiveWritesUnlessLinux(t)
	binary, err := sevenZipBinary()
	if err != nil {
		t.Skip("7-Zip is not installed")
	}
	root := openArchiveReadTestRoot(t)
	if err := os.WriteFile(filepath.Join(root.rootPath, "source.txt"), []byte("7z sample"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary, "a", "-t7z", "bundle.7z", "source.txt")
	command.Dir = root.rootPath
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create 7z sample: %v: %s", err, output)
	}
	entries, err := root.ListArchive(context.Background(), "bundle.7z", "", archiveReadTestLimits())
	if err != nil || len(entries) != 1 || entries[0].Path != "source.txt" {
		t.Fatalf("7z entries = %#v, error = %v", entries, err)
	}
	content, _, err := root.ReadArchiveEntry(context.Background(), "bundle.7z", "source.txt", archiveReadTestLimits())
	if err != nil || string(content) != "7z sample" {
		t.Fatalf("7z content = %q, error = %v", content, err)
	}
	if err := root.Mkdir("out", 0o755); err != nil {
		t.Fatal(err)
	}
	result, err := root.ExtractArchive(context.Background(), "bundle.7z", "out", []string{"source.txt"}, archiveReadTestLimits())
	if err != nil || len(result.Paths) != 1 || result.Paths[0] != "out/source.txt" {
		t.Fatalf("7z extract = %#v, error = %v", result, err)
	}
	if got := string(readArchiveReadTestFile(t, root, "out/source.txt")); got != "7z sample" {
		t.Fatalf("7z extracted content = %q", got)
	}
}

func TestArchiveRejectsTraversalLinksDuplicatesAndSpecialEntries(t *testing.T) {
	tests := []struct {
		name string
		zip  []byte
	}{
		{name: "traversal", zip: makeZIPHeaders(t, []zipReadTestEntry{{name: "../outside.txt", content: "bad"}})},
		{name: "absolute", zip: makeZIPHeaders(t, []zipReadTestEntry{{name: "/outside.txt", content: "bad"}})},
		{name: "symlink", zip: makeZIPHeaders(t, []zipReadTestEntry{{name: "link", content: "target", mode: os.ModeSymlink | 0o777}})},
		{name: "duplicate", zip: makeZIPHeaders(t, []zipReadTestEntry{{name: "same.txt", content: "one"}, {name: "same.txt", content: "two"}})},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := openArchiveReadTestRoot(t)
			writeArchiveReadTestFile(t, root, "unsafe.zip", test.zip)
			_, err := root.ListArchive(context.Background(), "unsafe.zip", "", archiveReadTestLimits())
			if !errors.Is(err, ErrArchiveUnsafeEntry) {
				t.Fatalf("error = %v, want unsafe entry", err)
			}
		})
	}
}

func TestArchiveRejectsCorruptionAndResourceLimits(t *testing.T) {
	root := openArchiveReadTestRoot(t)
	writeArchiveReadTestFile(t, root, "broken.zip", []byte("not a ZIP archive"))
	if _, err := root.ListArchive(context.Background(), "broken.zip", "", archiveReadTestLimits()); !errors.Is(err, ErrArchiveCorrupt) {
		t.Fatalf("corrupt archive error = %v", err)
	}

	writeArchiveReadTestFile(t, root, "large.zip", makeZIPReadTest(t, map[string]string{"large.txt": strings.Repeat("x", 128)}))
	limits := archiveReadTestLimits()
	limits.MaxTotalBytes = 32
	if _, err := root.ListArchive(context.Background(), "large.zip", "", limits); !errors.Is(err, ErrArchiveLimitExceeded) {
		t.Fatalf("limit error = %v", err)
	}
	limits = archiveReadTestLimits()
	limits.MaxPreviewBytes = 16
	if _, _, err := root.ReadArchiveEntry(context.Background(), "large.zip", "large.txt", limits); !errors.Is(err, ErrArchiveLimitExceeded) {
		t.Fatalf("preview limit error = %v", err)
	}
	limits = archiveReadTestLimits()
	limits.MaxRatio = 2
	if _, err := root.ListArchive(context.Background(), "large.zip", "", limits); !errors.Is(err, ErrArchiveLimitExceeded) {
		t.Fatalf("ratio limit error = %v", err)
	}
}

func TestZIPDirectoryLimitsAreCheckedBeforeStandardParsing(t *testing.T) {
	root := openArchiveReadTestRoot(t)
	content := makeZIPReadTest(t, map[string]string{"one.txt": "1", "two.txt": "2"})
	endRecord := bytes.LastIndex(content, []byte{'P', 'K', 5, 6})
	if endRecord < 0 {
		t.Fatal("ZIP end record is missing")
	}
	binary.LittleEndian.PutUint16(content[endRecord+8:endRecord+10], 1)
	binary.LittleEndian.PutUint16(content[endRecord+10:endRecord+12], 1)
	writeArchiveReadTestFile(t, root, "over-limit.zip", content)
	limits := archiveReadTestLimits()
	limits.MaxEntries = 1
	if _, err := root.ListArchive(context.Background(), "over-limit.zip", "", limits); !errors.Is(err, ErrArchiveLimitExceeded) {
		t.Fatalf("ListArchive() error = %v, want ErrArchiveLimitExceeded", err)
	}

	oversized := makeZIPReadTest(t, map[string]string{"one.txt": "1"})
	endRecord = bytes.LastIndex(oversized, []byte{'P', 'K', 5, 6})
	binary.LittleEndian.PutUint32(oversized[endRecord+12:endRecord+16], uint32(zipMaxDirectoryBytes+1))
	if err := validateZIPDirectoryLimits(bytes.NewReader(oversized), int64(len(oversized)), 10); !errors.Is(err, ErrArchiveLimitExceeded) {
		t.Fatalf("validateZIPDirectoryLimits() error = %v, want ErrArchiveLimitExceeded", err)
	}

	zip64 := makeZIP64ReadTest(t, makeZIPReadTest(t, map[string]string{"one.txt": "1"}))
	if err := validateZIPDirectoryLimits(bytes.NewReader(zip64), int64(len(zip64)), 10); err != nil {
		t.Fatalf("validateZIPDirectoryLimits() rejected valid ZIP64: %v", err)
	}
	if _, err := zip.NewReader(bytes.NewReader(zip64), int64(len(zip64))); err != nil {
		t.Fatalf("zip.NewReader() rejected valid ZIP64: %v", err)
	}
}

func TestArchiveRejectsUnsupportedTAREntries(t *testing.T) {
	root := openArchiveReadTestRoot(t)
	data := makeTARReadTest(t, []tarReadTestEntry{{name: "sym", link: "target", typeflag: tar.TypeSymlink}})
	writeArchiveReadTestFile(t, root, "unsafe.tar", data)
	if _, err := root.ListArchive(context.Background(), "unsafe.tar", "", archiveReadTestLimits()); !errors.Is(err, ErrArchiveUnsafeEntry) {
		t.Fatalf("special TAR error = %v", err)
	}
}

func TestArchiveRequiresCanonicalInnerPath(t *testing.T) {
	for _, value := range []string{"../secret", "/root", `a\\b`, "a//b", "a/./b", "C:/secret"} {
		if err := ValidateArchivePath(value, false); err == nil {
			t.Errorf("path %q was accepted", value)
		}
	}
	if err := ValidateArchivePath("docs/file.txt", false); err != nil {
		t.Fatalf("valid path rejected: %v", err)
	}
}

func TestArchiveTemporaryDirectoryIsHiddenAndReserved(t *testing.T) {
	root := openArchiveReadTestRoot(t)
	internalName := ".filesage-archive-stage-test"
	if err := os.Mkdir(filepath.Join(root.rootPath, internalName), 0o700); err != nil {
		t.Fatal(err)
	}
	entries, err := root.List("")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name == internalName {
			t.Fatal("private archive directory appeared in the file list")
		}
	}
	if _, _, err := root.OpenRead(internalName + "/secret.txt"); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("internal path error = %v", err)
	}
}

type zipReadTestEntry struct {
	name    string
	content string
	mode    os.FileMode
}

func makeZIPReadTest(t *testing.T, content map[string]string) []byte {
	t.Helper()
	entries := make([]zipReadTestEntry, 0, len(content))
	for name, value := range content {
		entries = append(entries, zipReadTestEntry{name: name, content: value, mode: 0o644})
	}
	return makeZIPHeaders(t, entries)
}

func makeZIP64ReadTest(t *testing.T, content []byte) []byte {
	t.Helper()
	endIndex := bytes.LastIndex(content, []byte{'P', 'K', 5, 6})
	if endIndex < 0 || len(content)-endIndex < zipEndRecordSize {
		t.Fatal("ZIP end record is missing")
	}
	endRecord := append([]byte(nil), content[endIndex:]...)
	entryCount := binary.LittleEndian.Uint16(endRecord[10:12])
	directorySize := binary.LittleEndian.Uint32(endRecord[12:16])
	directoryOffset := binary.LittleEndian.Uint32(endRecord[16:20])

	zip64End := make([]byte, zip64EndRecordSize)
	binary.LittleEndian.PutUint32(zip64End[0:4], zip64EndSignature)
	binary.LittleEndian.PutUint64(zip64End[4:12], zip64EndRecordSize-12)
	binary.LittleEndian.PutUint16(zip64End[12:14], 45)
	binary.LittleEndian.PutUint16(zip64End[14:16], 45)
	binary.LittleEndian.PutUint64(zip64End[24:32], uint64(entryCount))
	binary.LittleEndian.PutUint64(zip64End[32:40], uint64(entryCount))
	binary.LittleEndian.PutUint64(zip64End[40:48], uint64(directorySize))
	binary.LittleEndian.PutUint64(zip64End[48:56], uint64(directoryOffset))

	locator := make([]byte, zip64LocatorSize)
	binary.LittleEndian.PutUint32(locator[0:4], zip64LocatorSignature)
	binary.LittleEndian.PutUint64(locator[8:16], uint64(endIndex))
	binary.LittleEndian.PutUint32(locator[16:20], 1)
	binary.LittleEndian.PutUint16(endRecord[8:10], 0xffff)
	binary.LittleEndian.PutUint16(endRecord[10:12], 0xffff)
	binary.LittleEndian.PutUint32(endRecord[12:16], 0xffffffff)
	binary.LittleEndian.PutUint32(endRecord[16:20], 0xffffffff)

	result := append([]byte(nil), content[:endIndex]...)
	result = append(result, zip64End...)
	result = append(result, locator...)
	return append(result, endRecord...)
}

func makeZIPHeaders(t *testing.T, entries []zipReadTestEntry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Deflate}
		if entry.mode != 0 {
			header.SetMode(entry.mode)
		}
		file, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(file, entry.content); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

type tarReadTestEntry struct {
	name      string
	content   []byte
	directory bool
	link      string
	typeflag  byte
}

func makeTARReadTest(t *testing.T, entries []tarReadTestEntry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	for _, entry := range entries {
		typeflag := entry.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
			if entry.directory {
				typeflag = tar.TypeDir
			}
		}
		header := &tar.Header{Name: entry.name, Typeflag: typeflag, Mode: 0o644, ModTime: time.Now().UTC(), Size: int64(len(entry.content)), Linkname: entry.link}
		if entry.directory {
			header.Size = 0
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if len(entry.content) > 0 {
			if _, err := writer.Write(entry.content); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func openArchiveReadTestRoot(t *testing.T) *Root {
	t.Helper()
	root, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}

func writeArchiveReadTestFile(t *testing.T, root *Root, name string, content []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root.rootPath, filepath.FromSlash(name)), content, 0o644); err != nil {
		t.Fatal(err)
	}
}

func readArchiveReadTestFile(t *testing.T, root *Root, name string) []byte {
	t.Helper()
	file, _, err := root.OpenRead(name)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	content, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func archiveReadTestLimits() ArchiveReadLimits {
	return ArchiveReadLimits{MaxArchiveBytes: 1 << 20, MaxExternalArchiveBytes: 1 << 20, MaxEntries: 100, MaxEntryBytes: 1 << 16, MaxTotalBytes: 1 << 18, MaxPreviewBytes: 1 << 10, MaxRatio: 1000}
}

func skipArchiveWritesUnlessLinux(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("safe no-replace extraction is only implemented on Linux")
	}
}
