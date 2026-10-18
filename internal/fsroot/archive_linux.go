//go:build linux

// 本文件通过目录句柄打包所选子树，不跟随软链接。
package fsroot

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const archiveReadBufferSize = 256 << 10
const maxArchiveDepth = 128

type archiveParentRef struct {
	parent *os.File
	name   string
	info   os.FileInfo
}

type archiveParent struct {
	directory   *os.File
	directories []*os.File
	opened      []archiveParentRef
}

type archiveWriter struct {
	ctx        context.Context
	zip        *zip.Writer
	tar        *tar.Writer
	gzip       *gzip.Writer
	limits     ArchiveLimits
	entries    int
	totalBytes int64
}

// WriteArchive 兼容旧调用，默认生成 ZIP。
func (r *Root) WriteArchive(ctx context.Context, selectedPaths []string, dst io.Writer, limits ArchiveLimits) error {
	return r.WriteArchiveFormat(ctx, selectedPaths, dst, limits, ArchiveFormatZIP)
}

// WriteArchiveFormat 将所选根相对路径写入归档；错误时调用方必须丢弃输出。
func (r *Root) WriteArchiveFormat(ctx context.Context, selectedPaths []string, dst io.Writer, limits ArchiveLimits, format ArchiveFormat) error {
	if ctx == nil || dst == nil || limits.MaxEntries < 1 || limits.MaxBytes < 1 {
		return fsErrArchiveRequest()
	}
	if !validArchiveFormat(format) {
		return fsErrArchiveRequest()
	}
	if r == nil || r.root == nil {
		return fsErrArchiveRequest()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(selectedPaths) == 0 {
		return fsErrArchiveRequest()
	}
	if err := r.verifyRootPath(); err != nil {
		return fmt.Errorf("verify archive root: %w", err)
	}

	paths, err := normalizeArchivePaths(selectedPaths)
	if err != nil {
		return err
	}
	if format == ArchiveFormat7Z {
		return writeSevenZipArchive(ctx, r, paths, dst, limits)
	}
	root, err := r.root.Open(".")
	if err != nil {
		return fmt.Errorf("open archive root: %w", err)
	}
	defer root.Close()

	writer, finish, err := newArchiveWriter(ctx, dst, limits, format)
	if err != nil {
		return err
	}
	for _, relativePath := range paths {
		if err := writer.addSelected(root, relativePath); err != nil {
			_ = finish()
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		_ = finish()
		return err
	}
	if err := finish(); err != nil {
		return fmt.Errorf("finish archive: %w: %w", ErrArchiveStorage, err)
	}
	if err := r.verifyRootPath(); err != nil {
		return fmt.Errorf("verify archive root after scan: %w", err)
	}
	return ctx.Err()
}

func validArchiveFormat(format ArchiveFormat) bool {
	switch format {
	case ArchiveFormatZIP, ArchiveFormat7Z, ArchiveFormatTAR, ArchiveFormatTARGZ:
		return true
	default:
		return false
	}
}

// ValidateArchiveFormat 确认本机可创建指定格式。
func ValidateArchiveFormat(format ArchiveFormat) error {
	if !validArchiveFormat(format) {
		return ErrArchiveUnsupportedFile
	}
	if format == ArchiveFormat7Z {
		if _, err := sevenZipBinary(); err != nil {
			return fmt.Errorf("locate 7-Zip: %w: %w", ErrArchiveDependency, err)
		}
	}
	return nil
}

func newArchiveWriter(ctx context.Context, dst io.Writer, limits ArchiveLimits, format ArchiveFormat) (*archiveWriter, func() error, error) {
	writer := &archiveWriter{ctx: ctx, limits: limits}
	switch format {
	case ArchiveFormatZIP:
		writer.zip = zip.NewWriter(dst)
		return writer, writer.zip.Close, nil
	case ArchiveFormatTAR:
		writer.tar = tar.NewWriter(dst)
		return writer, writer.tar.Close, nil
	case ArchiveFormatTARGZ:
		writer.gzip = gzip.NewWriter(dst)
		writer.tar = tar.NewWriter(writer.gzip)
		return writer, func() error {
			tarErr := writer.tar.Close()
			gzipErr := writer.gzip.Close()
			if tarErr != nil {
				return tarErr
			}
			return gzipErr
		}, nil
	default:
		return nil, nil, fsErrArchiveRequest()
	}
}

func fsErrArchiveRequest() error {
	return fmt.Errorf("archive requires paths, a writer, and positive limits: %w", ErrInvalidPath)
}

func normalizeArchivePaths(selectedPaths []string) ([]string, error) {
	paths := append([]string(nil), selectedPaths...)
	for _, relativePath := range paths {
		if err := validateRelativePath(relativePath); err != nil || relativePath == "" || hasTrashComponent(relativePath) {
			return nil, fmt.Errorf("archive path %q: %w", relativePath, ErrInvalidPath)
		}
		for _, component := range strings.Split(relativePath, "/") {
			if !validArchiveComponent(component) {
				return nil, archiveUnsafePath(relativePath)
			}
		}
	}
	sort.Strings(paths)
	normalized := make([]string, 0, len(paths))
	kept := make(map[string]struct{}, len(paths))
	for _, relativePath := range paths {
		if archivePathCovered(relativePath, kept) {
			continue
		}
		normalized = append(normalized, relativePath)
		kept[relativePath] = struct{}{}
	}
	return normalized, nil
}

func archivePathCovered(relativePath string, kept map[string]struct{}) bool {
	for parent := path.Dir(relativePath); parent != "."; parent = path.Dir(parent) {
		if _, exists := kept[parent]; exists {
			return true
		}
	}
	_, exists := kept[relativePath]
	return exists
}

func (w *archiveWriter) addSelected(root *os.File, relativePath string) error {
	parent, name, err := openArchiveParent(root, relativePath)
	if err != nil {
		return fmt.Errorf("open selected path %q: %w", relativePath, err)
	}
	defer parent.Close()

	identity, err := openWalkIdentity(parent.directory, name)
	if err != nil {
		return fmt.Errorf("inspect selected path %q: %w", relativePath, err)
	}
	info, statErr := identity.Stat()
	closeErr := identity.Close()
	if statErr != nil {
		return fmt.Errorf("inspect selected path %q: %w", relativePath, statErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close selected path %q: %w", relativePath, closeErr)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return archiveUnsafePath(relativePath)
	}
	depth := len(strings.Split(relativePath, "/"))
	if depth > maxArchiveDepth {
		return fmt.Errorf("archive path %q: %w", relativePath, ErrArchiveLimitExceeded)
	}
	var addErr error
	if info.IsDir() {
		addErr = w.addDirectory(parent.directory, name, relativePath, relativePath, info, depth)
	} else if info.Mode().IsRegular() {
		addErr = w.addFile(parent.directory, name, relativePath, relativePath, info)
	} else {
		return archiveUnsafePath(relativePath)
	}
	if addErr != nil {
		return addErr
	}
	return parent.verify(relativePath)
}

func (w *archiveWriter) addDirectory(parent *os.File, name, relativePath, zipPath string, expected os.FileInfo, depth int) error {
	directory, err := openWalkDirectory(parent, name)
	if err != nil {
		return fmt.Errorf("open directory %q: %w", relativePath, err)
	}
	defer directory.Close()

	initial, err := directory.Stat()
	if err != nil {
		return fmt.Errorf("inspect directory %q: %w", relativePath, err)
	}
	if !initial.IsDir() || !os.SameFile(expected, initial) {
		return fmt.Errorf("directory %q: %w", relativePath, ErrPathChanged)
	}
	if err := verifyWalkIdentity(parent, name, initial, relativePath); err != nil {
		return err
	}
	if err := w.addDirectoryEntry(zipPath, initial); err != nil {
		return err
	}
	if err := w.walkDirectory(directory, relativePath, zipPath, depth); err != nil {
		return err
	}

	final, err := directory.Stat()
	if err != nil {
		return fmt.Errorf("recheck directory %q: %w", relativePath, err)
	}
	if !sameWalkDirectorySnapshot(initial, final) {
		return fmt.Errorf("directory %q: %w", relativePath, ErrPathChanged)
	}
	return verifyWalkIdentity(parent, name, final, relativePath)
}

func (w *archiveWriter) walkDirectory(directory *os.File, relativePath, zipPath string, depth int) error {
	initial, err := directory.Stat()
	if err != nil {
		return fmt.Errorf("inspect directory %q: %w", relativePath, err)
	}
	for {
		if err := w.ctx.Err(); err != nil {
			return err
		}
		children, readErr := directory.ReadDir(walkReadBatchSize)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return fmt.Errorf("read directory %q: %w", relativePath, readErr)
		}
		for _, child := range children {
			if err := w.ctx.Err(); err != nil {
				return err
			}
			name := child.Name()
			if name == TrashDirectoryName {
				continue
			}
			childPath := path.Join(relativePath, name)
			childZipPath := path.Join(zipPath, name)
			if !validArchiveComponent(name) {
				return archiveUnsafePath(childPath)
			}
			childDepth := depth + 1
			if childDepth > maxArchiveDepth {
				return fmt.Errorf("archive path %q: %w", childPath, ErrArchiveLimitExceeded)
			}
			if err := w.addChild(directory, name, childPath, childZipPath, childDepth); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) || len(children) == 0 {
			break
		}
	}
	final, err := directory.Stat()
	if err != nil {
		return fmt.Errorf("recheck directory %q: %w", relativePath, err)
	}
	if !sameWalkDirectorySnapshot(initial, final) {
		return fmt.Errorf("directory %q: %w", relativePath, ErrPathChanged)
	}
	return w.ctx.Err()
}

func (w *archiveWriter) addChild(parent *os.File, name, relativePath, zipPath string, depth int) error {
	identity, err := openWalkIdentity(parent, name)
	if err != nil {
		return fmt.Errorf("inspect entry %q: %w", relativePath, err)
	}
	info, statErr := identity.Stat()
	closeErr := identity.Close()
	if statErr != nil {
		return fmt.Errorf("inspect entry %q: %w", relativePath, statErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close entry %q: %w", relativePath, closeErr)
	}
	if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
		return archiveUnsafePath(relativePath)
	}
	if info.IsDir() {
		return w.addDirectory(parent, name, relativePath, zipPath, info, depth)
	}
	return w.addFile(parent, name, relativePath, zipPath, info)
}

func (w *archiveWriter) addDirectoryEntry(zipPath string, info os.FileInfo) error {
	if err := w.countEntry(zipPath); err != nil {
		return err
	}
	name := strings.TrimSuffix(zipPath, "/") + "/"
	if w.zip != nil {
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return fmt.Errorf("prepare directory %q: %w", zipPath, err)
		}
		header.Name = name
		header.Method = zip.Store
		header.SetMode(info.Mode().Perm()&0o777 | os.ModeDir)
		if _, err := w.zip.CreateHeader(header); err != nil {
			return fmt.Errorf("write directory %q: %w: %w", zipPath, ErrArchiveStorage, err)
		}
		return nil
	}
	if err := w.tar.WriteHeader(archiveTarHeader(name, info, tar.TypeDir)); err != nil {
		return fmt.Errorf("write directory %q: %w: %w", zipPath, ErrArchiveStorage, err)
	}
	return nil
}

func (w *archiveWriter) addFile(parent *os.File, name, relativePath, zipPath string, expected os.FileInfo) (result error) {
	if err := w.countEntry(zipPath); err != nil {
		return err
	}
	file, err := openArchiveFile(parent, name)
	if err != nil {
		return fmt.Errorf("open file %q: %w", relativePath, err)
	}
	defer func() {
		if err := file.Close(); result == nil && err != nil {
			result = fmt.Errorf("close file %q: %w", relativePath, err)
		}
	}()

	before, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect opened file %q: %w", relativePath, err)
	}
	if !before.Mode().IsRegular() || !os.SameFile(expected, before) {
		return fmt.Errorf("file %q: %w", relativePath, ErrPathChanged)
	}
	if before.Size() < 0 || before.Size() > w.limits.MaxBytes-w.totalBytes {
		return fmt.Errorf("file %q: %w", relativePath, ErrArchiveLimitExceeded)
	}

	var output io.Writer
	if w.zip != nil {
		header, headerErr := zip.FileInfoHeader(before)
		if headerErr != nil {
			return fmt.Errorf("prepare file %q: %w", zipPath, headerErr)
		}
		header.Name = zipPath
		header.Method = zip.Store
		header.SetMode(before.Mode().Perm() & 0o777)
		output, err = w.zip.CreateHeader(header)
	} else {
		header := archiveTarHeader(zipPath, before, tar.TypeReg)
		header.Size = before.Size()
		if err := w.tar.WriteHeader(header); err != nil {
			return fmt.Errorf("write file header %q: %w: %w", zipPath, ErrArchiveStorage, err)
		}
		output = w.tar
	}
	if err != nil {
		return fmt.Errorf("write file header %q: %w: %w", zipPath, ErrArchiveStorage, err)
	}
	if err := w.copyFile(output, file, relativePath); err != nil {
		return err
	}
	after, err := file.Stat()
	if err != nil {
		return fmt.Errorf("recheck file %q: %w", relativePath, err)
	}
	if !sameArchiveFileSnapshot(before, after) {
		return fmt.Errorf("file %q: %w", relativePath, ErrPathChanged)
	}
	if err := verifyWalkIdentity(parent, name, after, relativePath); err != nil {
		return err
	}
	return nil
}

func archiveTarHeader(name string, info os.FileInfo, kind byte) *tar.Header {
	return &tar.Header{
		Name:     name,
		Mode:     int64(info.Mode().Perm() & 0o777),
		ModTime:  info.ModTime(),
		Typeflag: kind,
		Format:   tar.FormatPAX,
	}
}

func (w *archiveWriter) copyFile(dst io.Writer, src *os.File, relativePath string) error {
	buffer := make([]byte, archiveReadBufferSize)
	for {
		if err := w.ctx.Err(); err != nil {
			return err
		}
		read, readErr := src.Read(buffer)
		if read > 0 {
			if int64(read) > w.limits.MaxBytes-w.totalBytes {
				return fmt.Errorf("file %q: %w", relativePath, ErrArchiveLimitExceeded)
			}
			written, writeErr := dst.Write(buffer[:read])
			w.totalBytes += int64(written)
			if writeErr != nil {
				return fmt.Errorf("write file %q: %w: %w", relativePath, ErrArchiveStorage, writeErr)
			}
			if written != read {
				return fmt.Errorf("write file %q: %w: %w", relativePath, ErrArchiveStorage, io.ErrShortWrite)
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return fmt.Errorf("read file %q: %w", relativePath, readErr)
		}
		if read == 0 {
			return fmt.Errorf("read file %q: %w", relativePath, io.ErrNoProgress)
		}
	}
}

func (w *archiveWriter) countEntry(relativePath string) error {
	w.entries++
	if w.entries > w.limits.MaxEntries {
		return fmt.Errorf("archive entry %q: %w", relativePath, ErrArchiveLimitExceeded)
	}
	return nil
}

func openArchiveParent(root *os.File, relativePath string) (*archiveParent, string, error) {
	components := strings.Split(relativePath, "/")
	if len(components) > maxArchiveDepth {
		return nil, "", fmt.Errorf("archive path %q: %w", relativePath, ErrArchiveLimitExceeded)
	}
	current, err := openWalkDirectory(root, ".")
	if err != nil {
		return nil, "", err
	}
	result := &archiveParent{directory: current, directories: []*os.File{current}}
	for _, component := range components[:len(components)-1] {
		next, openErr := openWalkDirectory(current, component)
		if openErr != nil {
			_ = result.Close()
			return nil, "", openErr
		}
		info, statErr := next.Stat()
		if statErr != nil {
			_ = next.Close()
			_ = result.Close()
			return nil, "", statErr
		}
		result.opened = append(result.opened, archiveParentRef{parent: current, name: component, info: info})
		result.directories = append(result.directories, next)
		current = next
	}
	result.directory = current
	return result, components[len(components)-1], nil
}

func (p *archiveParent) verify(relativePath string) error {
	for _, opened := range p.opened {
		if err := verifyWalkIdentity(opened.parent, opened.name, opened.info, relativePath); err != nil {
			return err
		}
	}
	return nil
}

func (p *archiveParent) Close() error {
	var firstErr error
	for index := len(p.directories) - 1; index >= 0; index-- {
		if err := p.directories[index].Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func openArchiveFile(parent *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			return nil, ErrPathChanged
		}
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

func sameArchiveFileSnapshot(before, after os.FileInfo) bool {
	if !os.SameFile(before, after) || before.Mode() != after.Mode() || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return false
	}
	beforeChange, beforeAvailable := fileChangeTime(before)
	afterChange, afterAvailable := fileChangeTime(after)
	return beforeAvailable == afterAvailable && (!beforeAvailable || beforeChange.Equal(afterChange))
}

func validArchiveComponent(component string) bool {
	if component == "" || component == "." || component == ".." || !utf8.ValidString(component) || strings.ContainsAny(component, "/\\:") || strings.HasSuffix(component, ".") || strings.HasSuffix(component, " ") || hasDriveVolume(component) {
		return false
	}
	for _, character := range component {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func archiveUnsafePath(relativePath string) error {
	return fmt.Errorf("%q: %w", relativePath, ErrArchiveUnsafeEntry)
}
