//go:build linux

// tree_linux.go 以 no-follow 文件描述符检查私有暂存树和内容差异。
package shellstage

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

type treeEntry struct {
	state  FileState
	binary bool
}

type fileTree struct {
	entries map[string]treeEntry
	bytes   int64
	count   int
	device  uint64
}

// scanTree 以 no-follow 文件描述符遍历暂存树并计算内容摘要。
func scanTree(root *os.File, limits Limits) (fileTree, error) {
	var rootStat unix.Stat_t
	if err := unix.Fstat(int(root.Fd()), &rootStat); err != nil {
		return fileTree{}, fmt.Errorf("%w: inspect staging root: %v", ErrUnsafeStaging, err)
	}
	if rootStat.Mode&unix.S_IFMT != unix.S_IFDIR || rootStat.Uid != uint32(os.Geteuid()) || rootStat.Mode&0o077 != 0 {
		return fileTree{}, fmt.Errorf("%w: staging root must be a private directory owned by the service account", ErrUnsafeStaging)
	}
	tree := fileTree{
		entries: make(map[string]treeEntry),
		device:  uint64(rootStat.Dev),
	}
	rootMode := rootStat.Mode & 0o7777
	if rootMode&(unix.S_ISUID|unix.S_ISGID|unix.S_ISVTX) != 0 {
		return fileTree{}, fmt.Errorf("%w: special mode bits are not allowed", ErrUnsafeStaging)
	}
	tree.entries["."] = treeEntry{state: FileState{Mode: rootMode, IsDirectory: true}}
	rootFD, err := unix.Openat(int(root.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fileTree{}, fmt.Errorf("%w: open staging root: %v", ErrUnsafeStaging, err)
	}
	defer unix.Close(rootFD)
	if err := scanDirectory(rootFD, ".", 1, &tree, limits); err != nil {
		return fileTree{}, err
	}
	return tree, nil
}

// scanDirectory 逐层遍历目录，限制深度、节点数和总字节数。
func scanDirectory(dirFD int, relative string, depth int, tree *fileTree, limits Limits) error {
	if depth > limits.MaxDepth {
		return fmt.Errorf("%w: directory depth exceeds %d", ErrUnsafeStaging, limits.MaxDepth)
	}
	dupFD, err := unix.Dup(dirFD)
	if err != nil {
		return fmt.Errorf("%w: duplicate directory handle: %v", ErrUnsafeStaging, err)
	}
	unix.CloseOnExec(dupFD)
	file := os.NewFile(uintptr(dupFD), relative)
	children, err := file.ReadDir(-1)
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return fmt.Errorf("%w: read directory %q: %v", ErrUnsafeStaging, relative, errors.Join(err, closeErr))
	}
	sort.Slice(children, func(i, j int) bool { return children[i].Name() < children[j].Name() })
	for _, child := range children {
		name := child.Name()
		if !utf8.ValidString(name) || strings.ContainsRune(name, '\\') || name == "." || name == ".." || strings.ContainsRune(name, '/') {
			return fmt.Errorf("%w: invalid path component under %q", ErrUnsafeStaging, relative)
		}
		rel := name
		if relative != "." {
			rel = path.Join(relative, name)
		}
		if !fs.ValidPath(rel) {
			return fmt.Errorf("%w: invalid relative path %q", ErrUnsafeStaging, rel)
		}
		tree.count++
		if tree.count > limits.MaxFiles {
			return fmt.Errorf("%w: file count exceeds %d", ErrUnsafeStaging, limits.MaxFiles)
		}
		var before unix.Stat_t
		if err := unix.Fstatat(dirFD, name, &before, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return fmt.Errorf("%w: inspect %q: %v", ErrUnsafeStaging, rel, err)
		}
		if uint64(before.Dev) != tree.device || before.Uid != uint32(os.Geteuid()) || before.Mode&(unix.S_ISUID|unix.S_ISGID|unix.S_ISVTX) != 0 {
			return fmt.Errorf("%w: %q has unsafe device, ownership, or mode", ErrUnsafeStaging, rel)
		}
		mode := before.Mode & 0o7777
		switch before.Mode & unix.S_IFMT {
		case unix.S_IFDIR:
			tree.entries[rel] = treeEntry{state: FileState{Mode: mode, IsDirectory: true}}
			nextFD, err := unix.Openat(dirFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if err != nil {
				return fmt.Errorf("%w: open directory %q: %v", ErrUnsafeStaging, rel, err)
			}
			var after unix.Stat_t
			statErr := unix.Fstat(nextFD, &after)
			if statErr != nil || before.Dev != after.Dev || before.Ino != after.Ino || after.Mode&unix.S_IFMT != unix.S_IFDIR {
				unix.Close(nextFD)
				return fmt.Errorf("%w: directory %q changed during inspection", ErrUnsafeStaging, rel)
			}
			walkErr := scanDirectory(nextFD, rel, depth+1, tree, limits)
			unix.Close(nextFD)
			if walkErr != nil {
				return walkErr
			}
		case unix.S_IFREG:
			if before.Nlink != 1 {
				return fmt.Errorf("%w: hard-linked file %q is not accepted", ErrUnsafeStaging, rel)
			}
			if before.Size < 0 || before.Size > limits.FileSizeBytes {
				return fmt.Errorf("%w: file %q exceeds the per-file limit", ErrUnsafeStaging, rel)
			}
			digest, binary, size, err := hashRegularAt(dirFD, name, rel, &before)
			if err != nil {
				return err
			}
			if size > limits.StagingBytes-tree.bytes {
				return fmt.Errorf("%w: staging tree exceeds its byte limit", ErrStagingLimit)
			}
			tree.bytes += size
			tree.entries[rel] = treeEntry{state: FileState{SHA256: digest, Mode: mode, Size: size}, binary: binary}
		default:
			return fmt.Errorf("%w: special file %q is not accepted", ErrUnsafeStaging, rel)
		}
	}
	return nil
}

// hashRegularAt 用 no-follow 打开普通文件，并校验读取期间文件身份未变化。
func hashRegularAt(parentFD int, name, relative string, before *unix.Stat_t) (string, bool, int64, error) {
	fd, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", false, 0, fmt.Errorf("%w: open file %q: %v", ErrUnsafeStaging, relative, err)
	}
	file := os.NewFile(uintptr(fd), relative)
	defer file.Close()
	var opened unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil || !sameInode(before, &opened) || opened.Mode&unix.S_IFMT != unix.S_IFREG || opened.Nlink != 1 {
		return "", false, 0, fmt.Errorf("%w: file %q changed during inspection", ErrUnsafeStaging, relative)
	}
	state := sha256.New()
	var size int64
	pending := make([]byte, 0, utf8.UTFMax)
	binary := false
	buffer := make([]byte, 64<<10)
	for {
		n, readErr := file.Read(buffer)
		if n > 0 {
			size += int64(n)
			if size > before.Size {
				return "", false, 0, fmt.Errorf("%w: file %q grew during inspection", ErrUnsafeStaging, relative)
			}
			_, _ = state.Write(buffer[:n])
			pending, binary = updateTextState(pending, buffer[:n], binary)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return "", false, 0, fmt.Errorf("%w: read file %q: %v", ErrUnsafeStaging, relative, readErr)
		}
	}
	if len(pending) != 0 || size != before.Size {
		binary = true
	}
	var after unix.Stat_t
	if err := unix.Fstat(fd, &after); err != nil || !sameFileSnapshot(before, &after) {
		return "", false, 0, fmt.Errorf("%w: file %q changed during inspection", ErrUnsafeStaging, relative)
	}
	return hex.EncodeToString(state.Sum(nil)), binary, size, nil
}

// sameInode 检查路径重新打开后仍指向相同 inode。
func sameInode(a, b *unix.Stat_t) bool {
	return a.Dev == b.Dev && a.Ino == b.Ino
}

// sameFileSnapshot 检查摘要读取期间没有并发修改文件。
func sameFileSnapshot(a, b *unix.Stat_t) bool {
	return sameInode(a, b) && a.Size == b.Size && a.Mode == b.Mode && a.Nlink == b.Nlink &&
		a.Mtim == b.Mtim && a.Ctim == b.Ctim
}

// updateTextState 跨读取块验证 UTF-8，并把 NUL 或非法字节标记为二进制。
func updateTextState(pending, chunk []byte, alreadyBinary bool) ([]byte, bool) {
	data := append(pending, chunk...)
	for len(data) > 0 {
		if !utf8.FullRune(data) {
			break
		}
		r, size := utf8.DecodeRune(data)
		if r == 0 || (r == utf8.RuneError && size == 1 && data[0] >= utf8.RuneSelf) {
			alreadyBinary = true
		}
		data = data[size:]
	}
	return append([]byte(nil), data...), alreadyBinary
}

// summarizeWorkspace 快速统计运行中的工作区，不读取文件内容。
func summarizeWorkspace(root *os.File, maxFiles, maxDepth int, maxBytes int64) (int64, error) {
	var rootStat unix.Stat_t
	if err := unix.Fstat(int(root.Fd()), &rootStat); err != nil || rootStat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return 0, fmt.Errorf("%w: inspect workspace root", ErrUnsafeStaging)
	}
	fd, err := unix.Openat(int(root.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0, fmt.Errorf("%w: open workspace root", ErrUnsafeStaging)
	}
	defer unix.Close(fd)
	var total int64
	count := 0
	if err := summarizeDirectory(fd, 1, uint64(rootStat.Dev), uint32(os.Geteuid()), maxFiles, maxDepth, maxBytes, &count, &total); err != nil {
		return total, err
	}
	return total, nil
}

// summarizeDirectory 在运行期间检查新增特殊路径并限制文件数和占用。
func summarizeDirectory(dirFD, depth int, device uint64, owner uint32, maxFiles, maxDepth int, maxBytes int64, count *int, total *int64) error {
	if depth > maxDepth {
		return fmt.Errorf("%w: workspace depth exceeds %d", ErrStagingLimit, maxDepth)
	}
	dupFD, err := unix.Dup(dirFD)
	if err != nil {
		return fmt.Errorf("%w: inspect workspace", ErrUnsafeStaging)
	}
	unix.CloseOnExec(dupFD)
	dir := os.NewFile(uintptr(dupFD), "workspace")
	children, err := dir.ReadDir(-1)
	closeErr := dir.Close()
	if err != nil || closeErr != nil {
		return fmt.Errorf("%w: workspace directory unreadable", ErrUnsafeStaging)
	}
	for _, entry := range children {
		name := entry.Name()
		if !utf8.ValidString(name) || strings.ContainsRune(name, '\\') || strings.ContainsRune(name, '/') {
			return fmt.Errorf("%w: invalid workspace path", ErrUnsafeStaging)
		}
		*count = *count + 1
		if *count > maxFiles {
			return fmt.Errorf("%w: file count exceeds %d", ErrStagingLimit, maxFiles)
		}
		var stat unix.Stat_t
		if err := unix.Fstatat(dirFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return fmt.Errorf("%w: inspect workspace entry", ErrUnsafeStaging)
		}
		if uint64(stat.Dev) != device || stat.Uid != owner || stat.Mode&(unix.S_ISUID|unix.S_ISGID|unix.S_ISVTX) != 0 {
			return fmt.Errorf("%w: workspace entry has unsafe device, ownership, or mode", ErrUnsafeStaging)
		}
		switch stat.Mode & unix.S_IFMT {
		case unix.S_IFDIR:
			next, err := unix.Openat(dirFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if err != nil {
				return fmt.Errorf("%w: workspace directory changed", ErrUnsafeStaging)
			}
			err = summarizeDirectory(next, depth+1, device, owner, maxFiles, maxDepth, maxBytes, count, total)
			unix.Close(next)
			if err != nil {
				return err
			}
		case unix.S_IFREG:
			if stat.Nlink != 1 {
				return fmt.Errorf("%w: workspace contains a hard link", ErrUnsafeStaging)
			}
			if stat.Size < 0 || stat.Size > maxBytes-*total {
				return fmt.Errorf("%w: workspace bytes exceed limit", ErrStagingLimit)
			}
			*total += stat.Size
		default:
			return fmt.Errorf("%w: workspace contains a symlink or special file", ErrUnsafeStaging)
		}
	}
	return nil
}

// buildChanges 生成路径排序的完整新增、修改和删除清单。
func buildChanges(beforeRoot, afterRoot *os.File, before, after fileTree, maxDiff int64) ([]Change, error) {
	paths := make([]string, 0, len(before.entries)+len(after.entries))
	seen := make(map[string]struct{}, len(before.entries)+len(after.entries))
	for name := range before.entries {
		seen[name] = struct{}{}
		paths = append(paths, name)
	}
	for name := range after.entries {
		if _, ok := seen[name]; !ok {
			paths = append(paths, name)
		}
	}
	sort.Strings(paths)
	changes := make([]Change, 0)
	for _, name := range paths {
		oldEntry, oldOK := before.entries[name]
		newEntry, newOK := after.entries[name]
		if oldOK && newOK && sameState(oldEntry.state, newEntry.state) {
			continue
		}
		change := Change{Path: name}
		switch {
		case !oldOK:
			change.Operation = Added
			state := newEntry.state
			change.After = &state
		case !newOK:
			change.Operation = Deleted
			state := oldEntry.state
			change.Before = &state
		default:
			change.Operation = Modified
			oldState, newState := oldEntry.state, newEntry.state
			change.Before, change.After = &oldState, &newState
		}
		oldFile := oldOK && !oldEntry.state.IsDirectory
		newFile := newOK && !newEntry.state.IsDirectory
		if (oldFile || newFile) && (oldEntry.binary || newEntry.binary) {
			change.Binary = true
		} else if oldFile || newFile {
			oldData, oldTruncated, err := readPreview(beforeRoot, name, oldFile, maxDiff)
			if err != nil {
				return nil, err
			}
			newData, newTruncated, err := readPreview(afterRoot, name, newFile, maxDiff)
			if err != nil {
				return nil, err
			}
			change.TextDiff, change.DiffTruncated = formatTextDiff(oldData, newData, maxDiff, oldTruncated, newTruncated)
		}
		changes = append(changes, change)
	}
	return changes, nil
}

// sameState 比较文件内容类型、模式、大小和哈希。
func sameState(a, b FileState) bool {
	return a.SHA256 == b.SHA256 && a.Mode == b.Mode && a.IsDirectory == b.IsDirectory && a.Size == b.Size
}

// readPreview 只读取单文件差异上限内的文本前缀。
func readPreview(root *os.File, relative string, exists bool, limit int64) ([]byte, bool, error) {
	if !exists {
		return nil, false, nil
	}
	fd, err := openRelativeFile(root, relative)
	if err != nil {
		return nil, false, fmt.Errorf("%w: read preview %q: %v", ErrUnsafeStaging, relative, err)
	}
	file := os.NewFile(uintptr(fd), relative)
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, false, fmt.Errorf("%w: read preview %q: %v", ErrUnsafeStaging, relative, err)
	}
	truncated := int64(len(data)) > limit
	if truncated {
		data = data[:limit]
	}
	return data, truncated, nil
}

// openRelativeFile 沿每个目录段 no-follow 打开工作区文件。
func openRelativeFile(root *os.File, relative string) (int, error) {
	if !fs.ValidPath(relative) || relative == "." {
		return -1, unix.EINVAL
	}
	parts := strings.Split(relative, "/")
	current, err := unix.Openat(int(root.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	for _, component := range parts[:len(parts)-1] {
		next, openErr := unix.Openat(current, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(current)
		if openErr != nil {
			return -1, openErr
		}
		current = next
	}
	fd, err := unix.Openat(current, parts[len(parts)-1], unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	unix.Close(current)
	if err != nil {
		return -1, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG {
		unix.Close(fd)
		return -1, errors.New("not a regular file")
	}
	return fd, nil
}
