// 本文件提供压缩包只读浏览、条目读取和受控解压。
package fsroot

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var (
	ErrArchiveCorrupt         = errors.New("archive is damaged or malformed")
	ErrArchiveEncrypted       = errors.New("archive contents are encrypted")
	ErrArchiveUnsupportedFile = errors.New("archive format or entry is unsupported")
	ErrArchiveEntryNotFound   = errors.New("archive entry not found")
)

// ArchiveReadLimits 限制归档扫描、预览和解压的资源消耗。
type ArchiveReadLimits struct {
	MaxArchiveBytes         int64
	MaxExternalArchiveBytes int64
	MaxEntries              int
	MaxEntryBytes           int64
	MaxTotalBytes           int64
	MaxPreviewBytes         int64
	MaxRatio                int64
}

// ArchiveEntry 描述归档内不含链接的普通条目。
type ArchiveEntry struct {
	Name       string    `json:"name"`
	Path       string    `json:"path"`
	Kind       string    `json:"kind"`
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modifiedAt"`

	compressedSize int64
	method         uint16
	zipFile        *zip.File
}

// ArchiveExtractError 描述已完整预检后发布失败的条目。
type ArchiveExtractError struct {
	Entry   string `json:"entry"`
	Message string `json:"message"`
}

// ArchiveExtractResult 只包含服务端确认已发布的路径。
type ArchiveExtractResult struct {
	Paths  []string              `json:"paths"`
	Errors []ArchiveExtractError `json:"errors"`
}

// ArchiveManifest 绑定归档文件状态和安全扫描后的完整输出清单。
type ArchiveManifest struct {
	Fingerprint Fingerprint    `json:"fingerprint"`
	Entries     []ArchiveEntry `json:"entries"`
	Digest      string         `json:"digest"`
}

type archiveFormat uint8

const (
	archiveZIP archiveFormat = iota + 1
	archiveTAR
	archiveTGZ
	archive7z
	archiveRAR
)

type archiveIndex struct {
	format  archiveFormat
	entries map[string]ArchiveEntry
}

type archiveOpened struct {
	file     *os.File
	entry    Entry
	format   archiveFormat
	rootPath string
}

const (
	maxArchivePathDepth = 128
	maxArchivePathBytes = 4096
	maxArchiveNameBytes = 255
	sevenZipOutputLimit = 16 << 20
)

var sevenZipSlots = make(chan struct{}, 2)

// ValidateArchivePath 确认归档内路径为规范相对路径；空路径仅代表归档根目录。
func ValidateArchivePath(relativePath string, allowRoot bool) error {
	if relativePath == "" {
		if allowRoot {
			return nil
		}
		return ErrInvalidPath
	}
	normalized, skip, err := normalizeArchiveMember(relativePath, false)
	if err != nil || skip || normalized != relativePath {
		return fmt.Errorf("%q: %w", relativePath, ErrInvalidPath)
	}
	return nil
}

// ListArchive 返回指定归档目录的直接子条目。
func (r *Root) ListArchive(ctx context.Context, relativePath, directory string, limits ArchiveReadLimits) ([]ArchiveEntry, error) {
	opened, index, err := r.openArchiveIndex(ctx, relativePath, limits)
	if err != nil {
		return nil, err
	}
	defer opened.file.Close()
	if err := ValidateArchivePath(directory, true); err != nil {
		return nil, err
	}
	if directory != "" {
		entry, ok := index.entries[directory]
		if !ok {
			return nil, ErrArchiveEntryNotFound
		}
		if entry.Kind != "directory" {
			return nil, ErrNotDirectory
		}
	}

	children := make([]ArchiveEntry, 0)
	prefix := directory
	if prefix != "" {
		prefix += "/"
	}
	for _, entry := range index.entries {
		if !strings.HasPrefix(entry.Path, prefix) || entry.Path == directory {
			continue
		}
		remainder := strings.TrimPrefix(entry.Path, prefix)
		if strings.ContainsRune(remainder, '/') {
			continue
		}
		children = append(children, publicArchiveEntry(entry))
	}
	sort.Slice(children, func(i, j int) bool {
		if children[i].Kind != children[j].Kind {
			return children[i].Kind == "directory"
		}
		return children[i].Name < children[j].Name
	})
	return children, nil
}

// ReadArchiveEntry 读取单个小于预览上限的普通归档条目。
func (r *Root) ReadArchiveEntry(ctx context.Context, relativePath, entryPath string, limits ArchiveReadLimits) ([]byte, ArchiveEntry, error) {
	if err := ValidateArchivePath(entryPath, false); err != nil {
		return nil, ArchiveEntry{}, err
	}
	opened, index, err := r.openArchiveIndex(ctx, relativePath, limits)
	if err != nil {
		return nil, ArchiveEntry{}, err
	}
	defer opened.file.Close()
	entry, ok := index.entries[entryPath]
	if !ok {
		return nil, ArchiveEntry{}, ErrArchiveEntryNotFound
	}
	if entry.Kind != "file" {
		if entry.Kind == "directory" {
			return nil, ArchiveEntry{}, ErrNotRegularFile
		}
		return nil, ArchiveEntry{}, ErrArchiveUnsupportedFile
	}
	if entry.Size > limits.MaxPreviewBytes {
		return nil, ArchiveEntry{}, ErrArchiveLimitExceeded
	}
	if err := ctx.Err(); err != nil {
		return nil, ArchiveEntry{}, err
	}
	data, err := readArchiveContent(ctx, opened.file, opened.rootPath, opened.format, entry, limits)
	if err != nil {
		return nil, ArchiveEntry{}, err
	}
	if int64(len(data)) != entry.Size {
		return nil, ArchiveEntry{}, ErrArchiveCorrupt
	}
	return data, publicArchiveEntry(entry), nil
}

// InspectArchive 扫描整个归档并返回可安全解压的精确清单。
func (r *Root) InspectArchive(ctx context.Context, relativePath string, limits ArchiveReadLimits) (ArchiveManifest, error) {
	opened, index, err := r.openArchiveIndex(ctx, relativePath, limits)
	if err != nil {
		return ArchiveManifest{}, err
	}
	defer opened.file.Close()
	entries, err := selectArchiveEntries(index.entries, nil)
	if err != nil {
		return ArchiveManifest{}, err
	}
	manifest := ArchiveManifest{Fingerprint: opened.entry.Fingerprint(), Entries: make([]ArchiveEntry, 0, len(entries))}
	for _, entry := range entries {
		manifest.Entries = append(manifest.Entries, publicArchiveEntry(entry))
	}
	manifest.Digest = archiveManifestDigest(manifest.Entries)
	return manifest, nil
}

// ExtractArchive 先完整预检并暂存所选内容，再按无覆盖规则逐项发布。
func (r *Root) ExtractArchive(ctx context.Context, archivePath, destination string, selections []string, limits ArchiveReadLimits) (ArchiveExtractResult, error) {
	return r.extractArchive(ctx, archivePath, destination, selections, limits, nil)
}

// ExtractArchiveApproved 解压已审批的完整路径清单，任何目标冲突都失败且不改名。
func (r *Root) ExtractArchiveApproved(ctx context.Context, archivePath, destination string, expected ArchiveManifest, expectedDestination Fingerprint, expectedPaths []string, limits ArchiveReadLimits) (ArchiveExtractResult, error) {
	return r.extractArchive(ctx, archivePath, destination, nil, limits, &archiveExtractExpectation{
		manifest: expected, destination: expectedDestination, paths: expectedPaths,
	})
}

type archiveExtractExpectation struct {
	manifest    ArchiveManifest
	destination Fingerprint
	paths       []string
}

type archiveFilePublisher func(string, io.Reader, int64, fs.FileMode) (string, error)

func (r *Root) extractArchive(ctx context.Context, archivePath, destination string, selections []string, limits ArchiveReadLimits, expected *archiveExtractExpectation) (ArchiveExtractResult, error) {
	return r.extractArchiveWithPublisher(ctx, archivePath, destination, selections, limits, expected, r.WriteNewAtomicFrom)
}

func (r *Root) extractArchiveWithPublisher(ctx context.Context, archivePath, destination string, selections []string, limits ArchiveReadLimits, expected *archiveExtractExpectation, publish archiveFilePublisher) (ArchiveExtractResult, error) {
	result := ArchiveExtractResult{Paths: []string{}, Errors: []ArchiveExtractError{}}
	if publish == nil {
		return result, ErrArchiveStorage
	}
	if err := validateRelativePath(destination); err != nil || hasTrashComponent(destination) {
		return result, ErrInvalidPath
	}
	destinationSnapshot := Fingerprint{Exists: true, Kind: KindDirectory}
	if destination != "" {
		entry, err := r.LstatEntry(destination)
		if err != nil {
			return result, err
		}
		if entry.Symlink || entry.Kind != KindDirectory {
			return result, ErrNotDirectory
		}
		if !entry.IdentityAvailable {
			return result, ErrIdentityUnavailable
		}
		destinationSnapshot = entry.Fingerprint()
	}
	opened, index, err := r.openArchiveIndex(ctx, archivePath, limits)
	if err != nil {
		return result, err
	}
	defer opened.file.Close()
	if !opened.entry.IdentityAvailable {
		return result, ErrIdentityUnavailable
	}
	selected, err := selectArchiveEntries(index.entries, selections)
	if err != nil {
		return result, err
	}
	if len(selected) == 0 {
		return result, ErrArchiveEntryNotFound
	}
	if expected != nil {
		manifest := ArchiveManifest{Fingerprint: opened.entry.Fingerprint(), Entries: make([]ArchiveEntry, 0, len(selected))}
		for _, entry := range selected {
			manifest.Entries = append(manifest.Entries, publicArchiveEntry(entry))
		}
		manifest.Digest = archiveManifestDigest(manifest.Entries)
		outputs := archiveOutputPaths(destination, selected)
		if !sameFingerprint(manifest.Fingerprint, expected.manifest.Fingerprint) || manifest.Digest != expected.manifest.Digest ||
			!sameArchivePaths(outputs, expected.paths) || !sameDirectoryFingerprint(destinationSnapshot, expected.destination, true) {
			return result, ErrConflict
		}
		if err := r.ensureArchivePathsFree(outputs); err != nil {
			return result, err
		}
	}
	if err := r.verifyArchiveRoot(); err != nil {
		return result, err
	}
	stage, err := os.MkdirTemp(r.rootPath, ".filesage-archive-stage-")
	if err != nil {
		return result, fmt.Errorf("create private archive staging directory: %w", ErrArchiveStorage)
	}
	if err := os.Chmod(stage, 0o700); err != nil {
		_ = os.RemoveAll(stage)
		return result, fmt.Errorf("protect archive staging directory: %w", ErrArchiveStorage)
	}
	defer os.RemoveAll(stage)
	if err := stageArchiveEntries(ctx, opened.file, opened.format, index.entries, selected, stage, limits); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	currentArchive, err := r.LstatEntry(archivePath)
	if err != nil || currentArchive.Symlink || currentArchive.Kind != KindFile || !opened.entry.IdentityAvailable || !sameFingerprint(currentArchive.Fingerprint(), opened.entry.Fingerprint()) {
		if err != nil {
			return result, err
		}
		return result, ErrConflict
	}
	if destination != "" {
		current, statErr := r.LstatEntry(destination)
		if statErr != nil || current.Symlink || current.Kind != KindDirectory || !current.IdentityAvailable || !sameFingerprint(current.Fingerprint(), destinationSnapshot) {
			if statErr != nil {
				return result, statErr
			}
			return result, ErrConflict
		}
	}
	if expected != nil {
		if !sameDirectoryFingerprint(destinationSnapshot, expected.destination, true) {
			return result, ErrConflict
		}
		if err := r.ensureArchivePathsFree(expected.paths); err != nil {
			return result, err
		}
	}
	groups := selectedTopLevelGroups(selected)
	for _, top := range groups {
		if err := ctx.Err(); err != nil {
			return finishArchivePartial(result, top, err)
		}
		published, publishErr := r.publishArchiveGroup(ctx, stage, destination, top, selected, expected != nil, publish)
		result.Paths = append(result.Paths, published...)
		if publishErr != nil {
			return finishArchivePartial(result, top, publishErr)
		}
	}
	sort.Strings(result.Paths)
	return result, nil
}

func archiveManifestDigest(entries []ArchiveEntry) string {
	hash := sha256.New()
	for _, entry := range entries {
		_, _ = fmt.Fprintf(hash, "%d:%s\x00%s\x00%d\x00%d\n", len(entry.Path), entry.Path, entry.Kind, entry.Size, entry.ModifiedAt.UnixNano())
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func archiveOutputPaths(destination string, entries []ArchiveEntry) []string {
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		output := entry.Path
		if destination != "" {
			output = path.Join(destination, output)
		}
		paths = append(paths, output)
	}
	sort.Strings(paths)
	return paths
}

func sameArchivePaths(actual, expected []string) bool {
	if len(actual) != len(expected) {
		return false
	}
	for index := range actual {
		if actual[index] != expected[index] {
			return false
		}
	}
	return true
}

func sameDirectoryFingerprint(actual, expected Fingerprint, exists bool) bool {
	if !exists {
		return !expected.Exists && expected.Kind == KindDirectory
	}
	return actual.Exists && expected.Exists && actual.Kind == KindDirectory && expected.Kind == KindDirectory &&
		actual.IdentityAvailable == expected.IdentityAvailable &&
		(!actual.IdentityAvailable || actual.Device == expected.Device && actual.Inode == expected.Inode)
}

func (r *Root) ensureArchivePathsFree(paths []string) error {
	for _, output := range paths {
		if _, err := r.LstatEntry(output); err == nil {
			return ErrConflict
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (r *Root) verifyArchiveRoot() error {
	rootInfo, err := r.root.Stat(".")
	if err != nil {
		return err
	}
	pathInfo, err := os.Stat(r.rootPath)
	if err != nil {
		return err
	}
	if !os.SameFile(rootInfo, pathInfo) {
		return ErrPathChanged
	}
	return nil
}

func (r *Root) openArchiveIndex(ctx context.Context, relativePath string, limits ArchiveReadLimits) (archiveOpened, archiveIndex, error) {
	if ctx == nil || ctx.Err() != nil {
		if ctx == nil {
			return archiveOpened{}, archiveIndex{}, fs.ErrInvalid
		}
		return archiveOpened{}, archiveIndex{}, ctx.Err()
	}
	if err := validateRelativePath(relativePath); err != nil || relativePath == "" || hasTrashComponent(relativePath) {
		return archiveOpened{}, archiveIndex{}, ErrInvalidPath
	}
	if limits.MaxArchiveBytes < 1 || limits.MaxEntries < 1 || limits.MaxEntryBytes < 1 || limits.MaxTotalBytes < 1 || limits.MaxPreviewBytes < 1 || limits.MaxRatio < 1 {
		return archiveOpened{}, archiveIndex{}, ErrInvalidPath
	}
	format, err := archiveFormatFor(relativePath)
	if err != nil {
		return archiveOpened{}, archiveIndex{}, err
	}
	file, entry, err := r.OpenRead(relativePath)
	if err != nil {
		return archiveOpened{}, archiveIndex{}, err
	}
	openedFile, ok := file.(*os.File)
	if !ok {
		_ = file.Close()
		return archiveOpened{}, archiveIndex{}, ErrArchiveUnsupported
	}
	maxArchiveBytes := archiveInputLimit(format, limits)
	if entry.Symlink || entry.Kind != KindFile || entry.Size < 0 || entry.Size > maxArchiveBytes {
		_ = openedFile.Close()
		if entry.Size > maxArchiveBytes {
			return archiveOpened{}, archiveIndex{}, ErrArchiveLimitExceeded
		}
		return archiveOpened{}, archiveIndex{}, ErrNotRegularFile
	}
	opened := archiveOpened{file: openedFile, entry: entry, format: format, rootPath: r.rootPath}
	index, err := scanArchive(ctx, opened, limits)
	if err != nil {
		_ = opened.file.Close()
		return archiveOpened{}, archiveIndex{}, err
	}
	if _, err := opened.file.Seek(0, io.SeekStart); err != nil {
		_ = opened.file.Close()
		return archiveOpened{}, archiveIndex{}, ErrArchiveCorrupt
	}
	return opened, index, nil
}

func archiveFormatFor(name string) (archiveFormat, error) {
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		return archiveTGZ, nil
	case strings.HasSuffix(lower, ".zip"):
		return archiveZIP, nil
	case strings.HasSuffix(lower, ".tar"):
		return archiveTAR, nil
	case strings.HasSuffix(lower, ".7z"):
		return archive7z, nil
	case strings.HasSuffix(lower, ".rar"):
		return archiveRAR, nil
	default:
		return 0, ErrArchiveUnsupportedFile
	}
}

func archiveInputLimit(format archiveFormat, limits ArchiveReadLimits) int64 {
	if format == archive7z || format == archiveRAR {
		if archiveProcFDPath() == "" && limits.MaxExternalArchiveBytes > 0 && limits.MaxExternalArchiveBytes < limits.MaxArchiveBytes {
			return limits.MaxExternalArchiveBytes
		}
	}
	return limits.MaxArchiveBytes
}

func scanArchive(ctx context.Context, opened archiveOpened, limits ArchiveReadLimits) (archiveIndex, error) {
	var records []ArchiveEntry
	switch opened.format {
	case archiveZIP:
		if err := validateZIPDirectoryLimits(opened.file, opened.entry.Size, limits.MaxEntries); err != nil {
			return archiveIndex{}, err
		}
		reader, err := zip.NewReader(opened.file, opened.entry.Size)
		if err != nil {
			return archiveIndex{}, fmt.Errorf("read ZIP directory: %w", ErrArchiveCorrupt)
		}
		if len(reader.File) > limits.MaxEntries {
			return archiveIndex{}, ErrArchiveLimitExceeded
		}
		for _, file := range reader.File {
			if err := ctx.Err(); err != nil {
				return archiveIndex{}, err
			}
			if file.Flags&1 != 0 {
				return archiveIndex{}, ErrArchiveEncrypted
			}
			mode := file.Mode()
			if mode&os.ModeSymlink != 0 || (!file.FileInfo().IsDir() && !mode.IsRegular()) {
				return archiveIndex{}, ErrArchiveUnsafeEntry
			}
			kind := "file"
			if file.FileInfo().IsDir() {
				kind = "directory"
			}
			member, skip, err := normalizeArchiveMember(file.Name, kind == "directory")
			if err != nil {
				return archiveIndex{}, err
			}
			if skip {
				continue
			}
			if file.UncompressedSize64 > uint64(limits.MaxEntryBytes) || file.UncompressedSize64 > uint64(limits.MaxTotalBytes) || file.CompressedSize64 > uint64(limits.MaxArchiveBytes) {
				return archiveIndex{}, ErrArchiveLimitExceeded
			}
			if exceedsArchiveRatio(int64(file.UncompressedSize64), int64(file.CompressedSize64), limits.MaxRatio) {
				return archiveIndex{}, ErrArchiveLimitExceeded
			}
			if kind == "directory" && file.UncompressedSize64 != 0 {
				return archiveIndex{}, ErrArchiveCorrupt
			}
			if file.Method != zip.Store && file.Method != zip.Deflate {
				kind = "unsupported"
			}
			records = append(records, ArchiveEntry{Name: path.Base(member), Path: member, Kind: kind, Size: int64(file.UncompressedSize64), ModifiedAt: file.Modified, compressedSize: int64(file.CompressedSize64), method: file.Method, zipFile: file})
		}
	case archiveTAR, archiveTGZ:
		if _, err := opened.file.Seek(0, io.SeekStart); err != nil {
			return archiveIndex{}, ErrArchiveCorrupt
		}
		var reader io.Reader = opened.file
		var gz *gzip.Reader
		var expanded *archiveBudgetReader
		if opened.format == archiveTGZ {
			var err error
			gz, err = gzip.NewReader(opened.file)
			if err != nil {
				return archiveIndex{}, fmt.Errorf("read gzip header: %w", ErrArchiveCorrupt)
			}
			defer gz.Close()
			expanded = &archiveBudgetReader{reader: gz, remaining: archiveTarStreamBudget(limits)}
			reader = expanded
		}
		tr := tar.NewReader(reader)
		for {
			if err := ctx.Err(); err != nil {
				return archiveIndex{}, err
			}
			header, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return archiveIndex{}, fmt.Errorf("read TAR member: %w", archiveStreamError(err))
			}
			kind := ""
			switch header.Typeflag {
			case tar.TypeDir:
				kind = "directory"
			case tar.TypeReg, tar.TypeRegA:
				kind = "file"
			default:
				return archiveIndex{}, ErrArchiveUnsafeEntry
			}
			member, skip, err := normalizeArchiveMember(header.Name, kind == "directory")
			if err != nil {
				return archiveIndex{}, err
			}
			if skip {
				continue
			}
			if header.Size < 0 || header.Size > limits.MaxEntryBytes || header.Size > limits.MaxTotalBytes {
				return archiveIndex{}, ErrArchiveLimitExceeded
			}
			if kind == "directory" && header.Size != 0 {
				return archiveIndex{}, ErrArchiveCorrupt
			}
			records = append(records, ArchiveEntry{Name: path.Base(member), Path: member, Kind: kind, Size: header.Size, ModifiedAt: header.ModTime})
			if len(records) > limits.MaxEntries {
				return archiveIndex{}, ErrArchiveLimitExceeded
			}
			if kind == "file" {
				if _, err := io.Copy(io.Discard, &archiveContextReader{ctx: ctx, reader: tr}); err != nil {
					return archiveIndex{}, fmt.Errorf("read TAR data: %w", archiveStreamError(err))
				}
			}
		}
		if expanded != nil {
			if _, err := io.Copy(io.Discard, &archiveContextReader{ctx: ctx, reader: expanded}); err != nil {
				return archiveIndex{}, fmt.Errorf("verify gzip trailer: %w", archiveStreamError(err))
			}
		}
	case archive7z, archiveRAR:
		release, err := acquireSevenZip(ctx)
		if err != nil {
			return archiveIndex{}, err
		}
		defer release()
		archivePath, extraFiles, cleanup, err := prepareSevenZipInput(ctx, opened, limits)
		if err != nil {
			return archiveIndex{}, err
		}
		defer cleanup()
		records, err = listSevenZip(ctx, archivePath, extraFiles, opened.format, limits)
		if err != nil {
			return archiveIndex{}, err
		}
	default:
		return archiveIndex{}, ErrArchiveUnsupportedFile
	}
	index, err := buildArchiveIndex(records, limits)
	if err != nil {
		return archiveIndex{}, err
	}
	return archiveIndex{format: opened.format, entries: index}, nil
}

func buildArchiveIndex(records []ArchiveEntry, limits ArchiveReadLimits) (map[string]ArchiveEntry, error) {
	if len(records) > limits.MaxEntries {
		return nil, ErrArchiveLimitExceeded
	}
	index := make(map[string]ArchiveEntry, len(records))
	raw := make(map[string]struct{}, len(records))
	var total int64
	for _, record := range records {
		if record.Path == "" || record.Size < 0 || record.Size > limits.MaxEntryBytes {
			return nil, ErrArchiveLimitExceeded
		}
		if _, exists := raw[record.Path]; exists {
			return nil, fmt.Errorf("duplicate archive path %q: %w", record.Path, ErrArchiveUnsafeEntry)
		}
		raw[record.Path] = struct{}{}
		if record.Kind == "file" || record.Kind == "unsupported" {
			if record.Size > limits.MaxTotalBytes-total {
				return nil, ErrArchiveLimitExceeded
			}
			total += record.Size
		}
		index[record.Path] = record
	}
	for _, record := range records {
		for parent := path.Dir(record.Path); parent != "."; parent = path.Dir(parent) {
			if existing, ok := index[parent]; ok {
				if existing.Kind != "directory" {
					return nil, fmt.Errorf("file is also used as a directory %q: %w", parent, ErrArchiveUnsafeEntry)
				}
				continue
			}
			index[parent] = ArchiveEntry{Name: path.Base(parent), Path: parent, Kind: "directory"}
			if len(index) > limits.MaxEntries {
				return nil, ErrArchiveLimitExceeded
			}
		}
	}
	return index, nil
}

func normalizeArchiveMember(raw string, directory bool) (string, bool, error) {
	if raw == "" || len(raw) > maxArchivePathBytes || !utf8.ValidString(raw) || strings.ContainsAny(raw, "\\\x00") || strings.HasPrefix(raw, "/") || filepath.IsAbs(raw) || filepath.VolumeName(raw) != "" || hasDriveVolume(raw) {
		return "", false, fmt.Errorf("unsafe archive path %q: %w", raw, ErrArchiveUnsafeEntry)
	}
	if directory && strings.HasSuffix(raw, "/") {
		raw = strings.TrimSuffix(raw, "/")
		if strings.HasSuffix(raw, "/") {
			return "", false, fmt.Errorf("unsafe archive path %q: %w", raw, ErrArchiveUnsafeEntry)
		}
	}
	components := strings.Split(raw, "/")
	normalized := make([]string, 0, len(components))
	for index, component := range components {
		if component == "." && index == 0 {
			continue
		}
		if component == "" || component == "." || component == ".." || len(component) > maxArchiveNameBytes || strings.Contains(component, ":") || strings.HasSuffix(component, ".") || strings.HasSuffix(component, " ") || hasDriveVolume(component) {
			return "", false, fmt.Errorf("unsafe archive path %q: %w", raw, ErrArchiveUnsafeEntry)
		}
		for _, character := range component {
			if unicode.IsControl(character) {
				return "", false, fmt.Errorf("unsafe archive path %q: %w", raw, ErrArchiveUnsafeEntry)
			}
		}
		if component == TrashDirectoryName {
			return "", false, fmt.Errorf("reserved archive path %q: %w", raw, ErrArchiveUnsafeEntry)
		}
		if isArchiveStageComponent(component) {
			return "", false, fmt.Errorf("reserved archive path %q: %w", raw, ErrArchiveUnsafeEntry)
		}
		normalized = append(normalized, component)
		if len(normalized) > maxArchivePathDepth {
			return "", false, ErrArchiveLimitExceeded
		}
	}
	if len(normalized) == 0 {
		if directory {
			return "", true, nil
		}
		return "", false, fmt.Errorf("unsafe archive path %q: %w", raw, ErrArchiveUnsafeEntry)
	}
	return strings.Join(normalized, "/"), false, nil
}

func selectArchiveEntries(index map[string]ArchiveEntry, requested []string) ([]ArchiveEntry, error) {
	selected := make(map[string]ArchiveEntry)
	addWithParents := func(entry ArchiveEntry) error {
		selected[entry.Path] = entry
		for parent := path.Dir(entry.Path); parent != "."; parent = path.Dir(parent) {
			parentEntry, exists := index[parent]
			if !exists || parentEntry.Kind != "directory" {
				return ErrArchiveCorrupt
			}
			selected[parent] = parentEntry
		}
		return nil
	}
	if len(requested) == 0 {
		for name, entry := range index {
			selected[name] = entry
		}
	} else {
		for _, name := range requested {
			if err := ValidateArchivePath(name, false); err != nil {
				return nil, err
			}
			entry, ok := index[name]
			if !ok {
				return nil, ErrArchiveEntryNotFound
			}
			if entry.Kind == "unsupported" {
				return nil, ErrArchiveUnsupportedFile
			}
			if err := addWithParents(entry); err != nil {
				return nil, err
			}
			if entry.Kind == "directory" {
				for childPath, child := range index {
					if strings.HasPrefix(childPath, name+"/") {
						if child.Kind == "unsupported" {
							return nil, ErrArchiveUnsupportedFile
						}
						if err := addWithParents(child); err != nil {
							return nil, err
						}
					}
				}
			}
		}
	}
	for _, entry := range selected {
		if entry.Kind == "unsupported" {
			return nil, ErrArchiveUnsupportedFile
		}
	}
	result := make([]ArchiveEntry, 0, len(selected))
	for _, entry := range selected {
		result = append(result, entry)
	}
	sort.Slice(result, func(i, j int) bool {
		depthI, depthJ := strings.Count(result[i].Path, "/"), strings.Count(result[j].Path, "/")
		if depthI != depthJ {
			return depthI < depthJ
		}
		return result[i].Path < result[j].Path
	})
	return result, nil
}

func stageArchiveEntries(ctx context.Context, archiveFile *os.File, format archiveFormat, index map[string]ArchiveEntry, selected []ArchiveEntry, stage string, limits ArchiveReadLimits) error {
	selectedFiles := make(map[string]ArchiveEntry)
	orderedFiles := make([]ArchiveEntry, 0, len(selected))
	for _, entry := range selected {
		target := filepath.Join(stage, filepath.FromSlash(entry.Path))
		if err := ensureArchiveStageDirectory(stage, filepath.Dir(target)); err != nil {
			return fmt.Errorf("prepare private archive entry: %w", ErrArchiveStorage)
		}
		if entry.Kind == "directory" {
			if err := os.MkdirAll(target, 0o700); err != nil {
				return fmt.Errorf("prepare private archive directory: %w", ErrArchiveStorage)
			}
			continue
		}
		selectedFiles[entry.Path] = entry
		orderedFiles = append(orderedFiles, entry)
	}
	switch format {
	case archiveZIP:
		for _, entry := range orderedFiles {
			if err := ctx.Err(); err != nil {
				return err
			}
			if entry.zipFile == nil {
				return ErrArchiveCorrupt
			}
			source, err := entry.zipFile.Open()
			if err != nil {
				return classifyArchiveReadError(err)
			}
			target := filepath.Join(stage, filepath.FromSlash(entry.Path))
			if err := writeArchiveStageFile(ctx, target, source, entry.Size); err != nil {
				_ = source.Close()
				return err
			}
			if err := source.Close(); err != nil {
				return ErrArchiveCorrupt
			}
		}
	case archiveTAR, archiveTGZ:
		if _, err := archiveFile.Seek(0, io.SeekStart); err != nil {
			return ErrArchiveCorrupt
		}
		var reader io.Reader = archiveFile
		var gz *gzip.Reader
		var expanded *archiveBudgetReader
		if format == archiveTGZ {
			var err error
			gz, err = gzip.NewReader(archiveFile)
			if err != nil {
				return archiveStreamError(err)
			}
			defer gz.Close()
			expanded = &archiveBudgetReader{reader: gz, remaining: archiveTarStreamBudget(limits)}
			reader = expanded
		}
		tr := tar.NewReader(reader)
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			header, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return archiveStreamError(err)
			}
			member, skip, err := normalizeArchiveMember(header.Name, header.Typeflag == tar.TypeDir)
			if err != nil {
				return err
			}
			if skip {
				continue
			}
			entry, wanted := selectedFiles[member]
			if !wanted {
				continue
			}
			if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA || header.Size != entry.Size {
				return ErrArchiveCorrupt
			}
			target := filepath.Join(stage, filepath.FromSlash(member))
			if err := writeArchiveStageFile(ctx, target, tr, entry.Size); err != nil {
				return err
			}
			delete(selectedFiles, member)
		}
		if len(selectedFiles) != 0 {
			return ErrArchiveCorrupt
		}
		if expanded != nil {
			if _, err := io.Copy(io.Discard, &archiveContextReader{ctx: ctx, reader: expanded}); err != nil {
				return archiveStreamError(err)
			}
		}
	case archive7z, archiveRAR:
		release, err := acquireSevenZip(ctx)
		if err != nil {
			return err
		}
		defer release()
		copyPath, extraFiles, cleanup, err := prepareSevenZipInput(ctx, archiveOpened{file: archiveFile, entry: Entry{Size: fileSize(archiveFile)}, format: format, rootPath: filepath.Dir(stage)}, limits)
		if err != nil {
			return err
		}
		defer cleanup()
		for _, entry := range orderedFiles {
			if err := ctx.Err(); err != nil {
				return err
			}
			if strings.HasPrefix(path.Base(entry.Path), "-") {
				return fmt.Errorf("7-Zip cannot safely address this entry name: %w", ErrArchiveUnsupportedFile)
			}
			target := filepath.Join(stage, filepath.FromSlash(entry.Path))
			if err := extractSevenZipEntry(ctx, copyPath, extraFiles, format, entry.Path, target, entry.Size); err != nil {
				return err
			}
		}
	default:
		return ErrArchiveUnsupportedFile
	}
	return verifyArchiveStage(stage, selected)
}

func ensureArchiveStageDirectory(stage, directory string) error {
	relative, err := filepath.Rel(stage, directory)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return ErrArchiveUnsafeEntry
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	return nil
}

func writeArchiveStageFile(ctx context.Context, target string, source io.Reader, expected int64) error {
	file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create staged archive file: %w", ErrArchiveStorage)
	}
	copyErr := copyArchiveExact(file, &archiveContextReader{ctx: ctx, reader: source}, expected)
	syncErr := file.Sync()
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	if syncErr != nil || closeErr != nil {
		return fmt.Errorf("persist staged archive file: %w", ErrArchiveStorage)
	}
	return nil
}

func copyArchiveExact(destination io.Writer, source io.Reader, expected int64) error {
	written, err := io.CopyN(destination, source, expected)
	if err != nil || written != expected {
		return fmt.Errorf("archive member data is incomplete: %w", archiveStreamError(err))
	}
	var extra [1]byte
	n, readErr := source.Read(extra[:])
	if n != 0 || (readErr != nil && !errors.Is(readErr, io.EOF)) {
		return fmt.Errorf("archive member size differs from its header: %w", archiveStreamError(readErr))
	}
	return nil
}

func verifyArchiveStage(stage string, selected []ArchiveEntry) error {
	for _, entry := range selected {
		filename := filepath.Join(stage, filepath.FromSlash(entry.Path))
		info, err := os.Lstat(filename)
		if err != nil {
			return fmt.Errorf("inspect staged archive entry: %w", ErrArchiveCorrupt)
		}
		if entry.Kind == "directory" {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return ErrArchiveUnsafeEntry
			}
		} else if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() != entry.Size {
			return ErrArchiveUnsafeEntry
		}
		_, _, links, available := fileIdentity(info)
		if entry.Kind == "file" && available && links > 1 {
			return ErrArchiveUnsafeEntry
		}
	}
	return nil
}

func readArchiveContent(ctx context.Context, file *os.File, rootPath string, format archiveFormat, entry ArchiveEntry, limits ArchiveReadLimits) ([]byte, error) {
	limit := limits.MaxPreviewBytes
	switch format {
	case archiveZIP:
		if entry.zipFile == nil {
			return nil, ErrArchiveCorrupt
		}
		opened, err := entry.zipFile.Open()
		if err != nil {
			return nil, classifyArchiveReadError(err)
		}
		defer opened.Close()
		return readArchiveLimited(ctx, opened, entry.Size, limit)
	case archiveTAR, archiveTGZ:
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return nil, ErrArchiveCorrupt
		}
		var reader io.Reader = file
		var gz *gzip.Reader
		var expanded *archiveBudgetReader
		if format == archiveTGZ {
			var err error
			gz, err = gzip.NewReader(file)
			if err != nil {
				return nil, archiveStreamError(err)
			}
			defer gz.Close()
			expanded = &archiveBudgetReader{reader: gz, remaining: archiveTarStreamBudget(limits)}
			reader = expanded
		}
		tr := tar.NewReader(reader)
		var data []byte
		found := false
		for {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			header, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, archiveStreamError(err)
			}
			member, skip, err := normalizeArchiveMember(header.Name, header.Typeflag == tar.TypeDir)
			if err != nil {
				return nil, err
			}
			if skip || member != entry.Path {
				continue
			}
			if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA || header.Size != entry.Size {
				return nil, ErrArchiveCorrupt
			}
			data, err = readArchiveLimited(ctx, tr, entry.Size, limit)
			if err != nil {
				return nil, err
			}
			found = true
		}
		if !found {
			return nil, ErrArchiveEntryNotFound
		}
		if expanded != nil {
			if _, err := io.Copy(io.Discard, &archiveContextReader{ctx: ctx, reader: expanded}); err != nil {
				return nil, archiveStreamError(err)
			}
		}
		return data, nil
	case archive7z, archiveRAR:
		release, err := acquireSevenZip(ctx)
		if err != nil {
			return nil, err
		}
		defer release()
		copyPath, extraFiles, cleanup, err := prepareSevenZipInput(ctx, archiveOpened{file: file, entry: Entry{Size: fileSize(file)}, format: format, rootPath: rootPath}, limits)
		if err != nil {
			return nil, err
		}
		defer cleanup()
		return readSevenZipEntry(ctx, copyPath, extraFiles, format, entry.Path, entry.Size, limit)
	default:
		return nil, ErrArchiveUnsupportedFile
	}
}

func readArchiveLimited(ctx context.Context, source io.Reader, expected, limit int64) ([]byte, error) {
	if expected > limit {
		return nil, ErrArchiveLimitExceeded
	}
	data := make([]byte, 0, expected)
	reader := &archiveContextReader{ctx: ctx, reader: io.LimitReader(source, limit+1)}
	var buffer [64 << 10]byte
	for {
		n, err := reader.Read(buffer[:])
		if n > 0 {
			data = append(data, buffer[:n]...)
			if int64(len(data)) > limit {
				return nil, ErrArchiveLimitExceeded
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, classifyArchiveReadError(err)
		}
	}
	if int64(len(data)) != expected {
		return nil, ErrArchiveCorrupt
	}
	return data, nil
}

type archiveContextReader struct {
	ctx    context.Context
	reader io.Reader
}

type archiveBudgetReader struct {
	reader    io.Reader
	remaining int64
}

func (r *archiveBudgetReader) Read(buffer []byte) (int, error) {
	if r.remaining < 0 {
		return 0, ErrArchiveLimitExceeded
	}
	maxInt := int64(^uint64(0) >> 1)
	if r.remaining < maxInt && int64(len(buffer)) > r.remaining+1 {
		buffer = buffer[:int(r.remaining+1)]
	}
	n, err := r.reader.Read(buffer)
	r.remaining -= int64(n)
	if r.remaining < 0 {
		return n, ErrArchiveLimitExceeded
	}
	return n, err
}

func archiveTarStreamBudget(limits ArchiveReadLimits) int64 {
	base := limits.MaxTotalBytes
	if base < 0 {
		return 0
	}
	const overheadPerEntry = int64(2048)
	const trailerBudget = int64(1 << 20)
	maxInt := int64(^uint64(0) >> 1)
	if base > maxInt-trailerBudget {
		return int64(^uint64(0) >> 1)
	}
	if int64(limits.MaxEntries) > (maxInt-base-trailerBudget)/overheadPerEntry {
		return maxInt
	}
	return base + int64(limits.MaxEntries)*overheadPerEntry + trailerBudget
}

func archiveStreamError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrArchiveLimitExceeded) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("archive stream is incomplete: %w", ErrArchiveCorrupt)
}

func prepareSevenZipInput(ctx context.Context, opened archiveOpened, limits ArchiveReadLimits) (string, []*os.File, func(), error) {
	if procPath := archiveProcFDPath(); procPath != "" {
		if err := ctx.Err(); err != nil {
			return "", nil, func() {}, err
		}
		if _, err := opened.file.Seek(0, io.SeekStart); err != nil {
			return "", nil, func() {}, ErrArchiveCorrupt
		}
		return procPath, archiveProcFDExtraFiles(opened.file), func() {}, nil
	}
	archivePath, cleanup, err := copyArchiveForCommand(ctx, opened, limits)
	if err != nil {
		return "", nil, func() {}, err
	}
	return archivePath, nil, cleanup, nil
}

func sevenZipTypeFlag(format archiveFormat) string {
	if format == archiveRAR {
		// RAR 的旧版与新版格式由文件签名区分。
		return "-t*"
	}
	return "-t7z"
}

func (r *archiveContextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}

func copyArchiveForCommand(ctx context.Context, opened archiveOpened, limits ArchiveReadLimits) (string, func(), error) {
	if err := ctx.Err(); err != nil {
		return "", func() {}, err
	}
	if _, err := opened.file.Seek(0, io.SeekStart); err != nil {
		return "", func() {}, ErrArchiveCorrupt
	}
	tempRoot := opened.rootPath
	if tempRoot == "" {
		tempRoot = os.TempDir()
	}
	directory, err := os.MkdirTemp(tempRoot, ".filesage-archive-stage-")
	if err != nil {
		return "", func() {}, ErrArchiveStorage
	}
	cleanup := func() { _ = os.RemoveAll(directory) }
	if err := os.Chmod(directory, 0o700); err != nil {
		cleanup()
		return "", func() {}, ErrArchiveStorage
	}
	archivePath := filepath.Join(directory, "input.archive")
	output, err := os.OpenFile(archivePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		cleanup()
		return "", func() {}, ErrArchiveStorage
	}
	maxBytes := archiveInputLimit(opened.format, limits)
	written, copyErr := io.Copy(output, &archiveContextReader{ctx: ctx, reader: io.LimitReader(opened.file, maxBytes+1)})
	if copyErr == nil && written > maxBytes {
		copyErr = ErrArchiveLimitExceeded
	}
	if copyErr == nil && opened.entry.Size > 0 && written != opened.entry.Size {
		copyErr = ErrArchiveCorrupt
	}
	syncErr := output.Sync()
	closeErr := output.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil {
		cleanup()
		if errors.Is(copyErr, ErrArchiveLimitExceeded) {
			return "", func() {}, ErrArchiveLimitExceeded
		}
		if ctx.Err() != nil {
			return "", func() {}, ctx.Err()
		}
		if copyErr != nil {
			return "", func() {}, archiveStreamError(copyErr)
		}
		return "", func() {}, ErrArchiveStorage
	}
	return archivePath, cleanup, nil
}

func listSevenZip(ctx context.Context, archivePath string, extraFiles []*os.File, format archiveFormat, limits ArchiveReadLimits) ([]ArchiveEntry, error) {
	binary, err := sevenZipBinary()
	if err != nil {
		return nil, fmt.Errorf("7-Zip is unavailable: %w", ErrArchiveUnsupported)
	}
	stdout := &boundedBuffer{limit: sevenZipOutputLimit}
	stderr := &boundedBuffer{limit: 64 << 10}
	command := exec.CommandContext(ctx, binary, "l", "-slt", sevenZipTypeFlag(format), archivePath)
	command.ExtraFiles = extraFiles
	command.Stdout, command.Stderr = stdout, stderr
	if err := command.Run(); err != nil {
		if stdout.exceeded {
			return nil, ErrArchiveLimitExceeded
		}
		return nil, classifySevenZipListingError(ctx, stdout.String(), stderr.String(), err)
	}
	records := make([]ArchiveEntry, 0)
	outputText := strings.ReplaceAll(stdout.String(), "\r\n", "\n")
	for _, block := range strings.Split(outputText, "\n\n") {
		fields := make(map[string]string)
		for _, line := range strings.Split(strings.TrimSpace(block), "\n") {
			key, value, ok := strings.Cut(strings.TrimSuffix(line, "\r"), " = ")
			if !ok {
				continue
			}
			fields[key] = value
		}
		if _, isHeader := fields["Type"]; isHeader {
			if fields["Multivolume"] == "+" {
				return nil, ErrArchiveUnsupportedFile
			}
			continue
		}
		name, hasName := fields["Path"]
		if !hasName || name == "" {
			continue
		}
		if fields["Encrypted"] == "+" {
			return nil, ErrArchiveEncrypted
		}
		if fields["Symbolic Link"] != "" || fields["Hard Link"] != "" || fields["Link"] != "" || fields["Copy Link"] != "" {
			return nil, ErrArchiveUnsafeEntry
		}
		attributes := strings.TrimSpace(fields["Attributes"])
		attributeMode := sevenZipAttributeMode(attributes)
		if attributeMode == 'l' || attributeMode == 'b' || attributeMode == 'c' || attributeMode == 'p' || attributeMode == 's' {
			return nil, ErrArchiveUnsafeEntry
		}
		isDirectory := fields["Folder"] == "+" || attributeMode == 'd' || strings.HasPrefix(attributes, "D ") || attributes == "D" || strings.HasSuffix(name, "/")
		kind := "file"
		if isDirectory {
			kind = "directory"
		}
		member, skip, err := normalizeArchiveMember(name, isDirectory)
		if err != nil {
			return nil, err
		}
		if skip {
			continue
		}
		size := int64(0)
		if raw, exists := fields["Size"]; exists && raw != "" {
			size, err = strconv.ParseInt(raw, 10, 64)
			if err != nil || size < 0 {
				return nil, ErrArchiveCorrupt
			}
		}
		packed := int64(0)
		if raw, exists := fields["Packed Size"]; exists && raw != "" {
			packed, err = strconv.ParseInt(raw, 10, 64)
			if err != nil || packed < 0 {
				return nil, ErrArchiveCorrupt
			}
		}
		if size > limits.MaxEntryBytes || packed > archiveInputLimit(archive7z, limits) || exceedsArchiveRatio(size, packed, limits.MaxRatio) {
			return nil, ErrArchiveLimitExceeded
		}
		if isDirectory && size != 0 {
			return nil, ErrArchiveCorrupt
		}
		modifiedAt, _ := time.Parse("2006-01-02 15:04:05.0000000", fields["Modified"])
		records = append(records, ArchiveEntry{Name: path.Base(member), Path: member, Kind: kind, Size: size, ModifiedAt: modifiedAt, compressedSize: packed})
		if len(records) > limits.MaxEntries {
			return nil, ErrArchiveLimitExceeded
		}
	}
	return records, nil
}

func readSevenZipEntry(ctx context.Context, archivePath string, extraFiles []*os.File, format archiveFormat, entry string, size, limit int64) ([]byte, error) {
	binary, err := sevenZipBinary()
	if err != nil {
		return nil, ErrArchiveUnsupported
	}
	if strings.HasPrefix(path.Base(entry), "-") {
		return nil, ErrArchiveUnsupportedFile
	}
	output := &boundedBuffer{limit: int(limit)}
	stderr := &boundedBuffer{limit: 64 << 10}
	command := exec.CommandContext(ctx, binary, "x", "-so", "-spd", sevenZipTypeFlag(format), archivePath, entry)
	command.ExtraFiles = extraFiles
	command.Stdout, command.Stderr = output, stderr
	if err := command.Run(); err != nil {
		if output.exceeded {
			return nil, ErrArchiveLimitExceeded
		}
		return nil, classifySevenZipError(ctx, stderr.String(), err)
	}
	if output.exceeded || int64(output.Len()) > limit {
		return nil, ErrArchiveLimitExceeded
	}
	if int64(output.Len()) != size {
		return nil, ErrArchiveCorrupt
	}
	return output.Bytes(), nil
}

func extractSevenZipEntry(ctx context.Context, archivePath string, extraFiles []*os.File, format archiveFormat, entry, target string, size int64) error {
	binary, err := sevenZipBinary()
	if err != nil {
		return ErrArchiveUnsupported
	}
	if strings.HasPrefix(path.Base(entry), "-") {
		return ErrArchiveUnsupportedFile
	}
	file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return ErrArchiveStorage
	}
	limited := &exactLimitWriter{writer: file, limit: size}
	stderr := &boundedBuffer{limit: 64 << 10}
	command := exec.CommandContext(ctx, binary, "x", "-so", "-spd", sevenZipTypeFlag(format), archivePath, entry)
	command.ExtraFiles = extraFiles
	command.Stdout, command.Stderr = limited, stderr
	commandErr := command.Run()
	syncErr := file.Sync()
	closeErr := file.Close()
	if limited.exceeded {
		return ErrArchiveLimitExceeded
	}
	if commandErr != nil {
		return classifySevenZipError(ctx, stderr.String(), commandErr)
	}
	if syncErr != nil || closeErr != nil {
		return ErrArchiveStorage
	}
	if limited.written != size {
		return ErrArchiveCorrupt
	}
	return nil
}

type exactLimitWriter struct {
	writer   io.Writer
	limit    int64
	written  int64
	exceeded bool
}

func (w *exactLimitWriter) Write(buffer []byte) (int, error) {
	if int64(len(buffer)) > w.limit-w.written {
		w.exceeded = true
		return 0, ErrArchiveLimitExceeded
	}
	n, err := w.writer.Write(buffer)
	w.written += int64(n)
	return n, err
}

type boundedBuffer struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

func (b *boundedBuffer) Write(buffer []byte) (int, error) {
	remaining := b.limit - b.Len()
	if remaining <= 0 {
		b.exceeded = true
		return 0, ErrArchiveLimitExceeded
	}
	if len(buffer) > remaining {
		_, _ = b.Buffer.Write(buffer[:remaining])
		b.exceeded = true
		return remaining, ErrArchiveLimitExceeded
	}
	return b.Buffer.Write(buffer)
}

func sevenZipBinary() (string, error) {
	for _, name := range []string{"7z", "7zz", "7za"} {
		if binary, err := exec.LookPath(name); err == nil {
			return binary, nil
		}
	}
	return "", fs.ErrNotExist
}

func acquireSevenZip(ctx context.Context) (func(), error) {
	select {
	case sevenZipSlots <- struct{}{}:
		return func() { <-sevenZipSlots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func classifySevenZipError(ctx context.Context, stderr string, cause error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	lower := strings.ToLower(stderr)
	if strings.Contains(lower, "wrong password") || strings.Contains(lower, "password is incorrect") || strings.Contains(lower, "encrypted") {
		return ErrArchiveEncrypted
	}
	if strings.Contains(lower, "unsupported method") || strings.Contains(lower, "method is not supported") {
		return ErrArchiveUnsupportedFile
	}
	if strings.Contains(lower, "can not open the file as archive") || strings.Contains(lower, "is not archive") || strings.Contains(lower, "headers error") || strings.Contains(lower, "data error") || strings.Contains(lower, "unexpected end") {
		return ErrArchiveCorrupt
	}
	return fmt.Errorf("7-Zip command failed: %w", ErrArchiveCorrupt)
}

func classifySevenZipListingError(ctx context.Context, stdout, stderr string, cause error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if strings.Contains(strings.ToLower(stdout), "multivolume = +") {
		return ErrArchiveUnsupportedFile
	}
	for _, line := range strings.Split(stdout, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(trimmed), "error:") {
			stderr += "\n" + trimmed
		}
	}
	return classifySevenZipError(ctx, stderr, cause)
}

func classifyArchiveReadError(err error) error {
	if errors.Is(err, ErrArchiveLimitExceeded) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if errors.Is(err, zip.ErrAlgorithm) {
		return ErrArchiveUnsupportedFile
	}
	if strings.Contains(strings.ToLower(err.Error()), "password") || strings.Contains(strings.ToLower(err.Error()), "encrypt") {
		return ErrArchiveEncrypted
	}
	return fmt.Errorf("read archive member: %w", ErrArchiveCorrupt)
}

func sevenZipAttributeMode(attributes string) rune {
	for _, field := range strings.Fields(attributes) {
		if len(field) > 0 && strings.ContainsRune("-dlbcps", rune(field[0])) && (len(field) == 10 || len(field) == 11) {
			return rune(field[0])
		}
	}
	return 0
}

func exceedsArchiveRatio(expanded, packed, limit int64) bool {
	if expanded == 0 {
		return false
	}
	if packed <= 0 || limit <= 0 {
		return true
	}
	if packed > int64(^uint64(0)>>1)/limit {
		return false
	}
	return expanded > packed*limit
}

func validArchiveOutputName(name string) bool {
	if name == "" || len(name) > maxArchiveNameBytes || name == "." || name == ".." || strings.ContainsAny(name, "/\\") || !utf8.ValidString(name) || hasDriveVolume(name) {
		return false
	}
	for _, character := range name {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func publicArchiveEntry(entry ArchiveEntry) ArchiveEntry {
	return ArchiveEntry{Name: entry.Name, Path: entry.Path, Kind: entry.Kind, Size: entry.Size, ModifiedAt: entry.ModifiedAt}
}

func fileSize(file *os.File) int64 {
	info, err := file.Stat()
	if err != nil {
		return -1
	}
	return info.Size()
}

func selectedTopLevelGroups(selected []ArchiveEntry) []string {
	set := make(map[string]struct{})
	for _, entry := range selected {
		first, _, _ := strings.Cut(entry.Path, "/")
		set[first] = struct{}{}
	}
	groups := make([]string, 0, len(set))
	for name := range set {
		groups = append(groups, name)
	}
	sort.Strings(groups)
	return groups
}

func numberedArchiveName(name string, suffix int) string {
	if suffix == 0 {
		return name
	}
	extension := path.Ext(name)
	base := strings.TrimSuffix(name, extension)
	if base == "" {
		base, extension = name, ""
	}
	return fmt.Sprintf("%s (%d)%s", base, suffix, extension)
}

func (r *Root) publishArchiveGroup(ctx context.Context, stage, destination, top string, selected []ArchiveEntry, noRename bool, publish archiveFilePublisher) ([]string, error) {
	group := make([]ArchiveEntry, 0)
	for _, entry := range selected {
		first, _, _ := strings.Cut(entry.Path, "/")
		if first == top {
			group = append(group, entry)
		}
	}
	if len(group) == 0 {
		return nil, ErrArchiveEntryNotFound
	}
	sourceRoot := group[0]
	for _, entry := range group {
		if entry.Path == top {
			sourceRoot = entry
			break
		}
	}
	for suffix := 0; suffix <= 10000; suffix++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		targetTopName := numberedArchiveName(top, suffix)
		targetTop := path.Join(destination, targetTopName)
		if destination == "" {
			targetTop = targetTopName
		}
		if !validArchiveOutputName(targetTopName) || targetTopName == TrashDirectoryName {
			return nil, ErrArchiveUnsafeEntry
		}
		_, err := r.root.Lstat(targetTop)
		if err == nil {
			if noRename {
				return nil, ErrConflict
			}
			continue
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		if sourceRoot.Kind == "file" {
			source := filepath.Join(stage, filepath.FromSlash(sourceRoot.Path))
			file, openErr := os.Open(source)
			if openErr != nil {
				return nil, ErrArchiveStorage
			}
			digest, writeErr := publish(targetTop, &archiveContextReader{ctx: ctx, reader: file}, sourceRoot.Size, 0o644)
			closeErr := file.Close()
			if digest != "" {
				published := []string{targetTop}
				if writeErr != nil || closeErr != nil {
					if writeErr != nil {
						writeErr = fmt.Errorf("publish archive file %q: %w", targetTop, writeErr)
					}
					return published, errors.Join(writeErr, closeErr)
				}
				return published, nil
			}
			if errors.Is(writeErr, fs.ErrExist) || errors.Is(writeErr, ErrConflict) {
				if noRename {
					return nil, ErrConflict
				}
				continue
			}
			if writeErr != nil {
				return nil, errors.Join(fmt.Errorf("publish archive file %q: %w", targetTop, writeErr), closeErr)
			}
			if closeErr != nil {
				return nil, closeErr
			}
			return nil, ErrArchiveStorage
		}
		if err := r.Mkdir(targetTop, 0o755); err != nil {
			if errors.Is(err, fs.ErrExist) || errors.Is(err, ErrConflict) {
				if noRename {
					return nil, ErrConflict
				}
				continue
			}
			return nil, fmt.Errorf("create archive directory %q: %w", targetTop, err)
		}
		published := []string{targetTop}
		for _, entry := range group {
			if err := ctx.Err(); err != nil {
				return published, err
			}
			if entry.Path == top {
				continue
			}
			suffixPath := strings.TrimPrefix(entry.Path, top+"/")
			target := path.Join(targetTop, suffixPath)
			if entry.Kind == "directory" {
				if err := r.Mkdir(target, 0o755); err != nil {
					return published, fmt.Errorf("create archive directory %q: %w", target, err)
				}
				published = append(published, target)
				continue
			}
			source := filepath.Join(stage, filepath.FromSlash(entry.Path))
			file, err := os.Open(source)
			if err != nil {
				return published, ErrArchiveStorage
			}
			digest, writeErr := publish(target, &archiveContextReader{ctx: ctx, reader: file}, entry.Size, 0o644)
			closeErr := file.Close()
			if digest != "" {
				published = append(published, target)
			}
			if writeErr != nil {
				return published, errors.Join(fmt.Errorf("publish archive file %q: %w", target, writeErr), closeErr)
			}
			if closeErr != nil {
				return published, closeErr
			}
			if digest == "" {
				return published, ErrArchiveStorage
			}
		}
		return published, nil
	}
	return nil, ErrConflict
}

func finishArchivePartial(result ArchiveExtractResult, entry string, err error) (ArchiveExtractResult, error) {
	if len(result.Paths) > 0 {
		sort.Strings(result.Paths)
		result.Errors = append(result.Errors, ArchiveExtractError{Entry: entry, Message: "该条目的发布状态需核查，请刷新目录确认。"})
		return result, err
	}
	return result, err
}

// ArchiveMimeType 返回归档条目的只读预览 MIME 类型。
func ArchiveMimeType(name string) string {
	if value := mime.TypeByExtension(strings.ToLower(filepath.Ext(name))); value != "" {
		return value
	}
	return "application/octet-stream"
}
