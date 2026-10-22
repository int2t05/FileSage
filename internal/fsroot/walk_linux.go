//go:build linux

// 本文件通过目录句柄递归遍历，不跟随软链接。
package fsroot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"golang.org/x/sys/unix"
)

const walkReadBatchSize = 128

// Walk 回调根目录内的条目；软链接只报告自身，不遍历目标。
// 任意错误可能发生在部分回调之后，调用方必须丢弃本轮扫描结果。
func (r *Root) Walk(ctx context.Context, visit func(Entry) error) error {
	return r.walkFrom(ctx, "", "", visit)
}

// WalkDirectory 仅遍历指定的管理根相对目录；打开路径的每一级都拒绝软链接。
func (r *Root) WalkDirectory(ctx context.Context, relativePath string, visit func(Entry) error) error {
	if err := validateRelativePath(relativePath); err != nil || relativePath == "" || hasTrashComponent(relativePath) {
		return ErrInvalidPath
	}
	return r.walkFrom(ctx, relativePath, "", visit)
}

// WalkExcluding 在安全遍历中跳过一个目录子树，不读取或报告该子树中的项目。
func (r *Root) WalkExcluding(ctx context.Context, scopePath, excludedPath string, visit func(Entry) error) error {
	if err := validateRelativePath(scopePath); err != nil || hasTrashComponent(scopePath) {
		return ErrInvalidPath
	}
	if err := validateRelativePath(excludedPath); err != nil || excludedPath == "" || hasTrashComponent(excludedPath) || excludedPath == scopePath {
		return ErrInvalidPath
	}
	if scopePath != "" && excludedPath != scopePath && !strings.HasPrefix(excludedPath, scopePath+"/") {
		return r.walkFrom(ctx, scopePath, "", visit)
	}
	return r.walkFrom(ctx, scopePath, excludedPath, visit)
}

// WalkDirectoryExcluding 在指定目录内跳过一个目录子树。
func (r *Root) WalkDirectoryExcluding(ctx context.Context, scopePath, excludedPath string, visit func(Entry) error) error {
	if err := validateRelativePath(scopePath); err != nil || scopePath == "" {
		return ErrInvalidPath
	}
	if excludedPath == "" {
		return r.WalkDirectory(ctx, scopePath, visit)
	}
	return r.WalkExcluding(ctx, scopePath, excludedPath, visit)
}

func (r *Root) walkFrom(ctx context.Context, scopePath, excludedPath string, visit func(Entry) error) error {
	if ctx == nil {
		return errors.New("walk requires a context")
	}
	if visit == nil {
		return errors.New("walk requires a visitor")
	}
	if r == nil || r.root == nil {
		return errors.New("walk requires an open root")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := r.verifyRootPath(); err != nil {
		return fmt.Errorf("verify walk root: %w", err)
	}
	if err := r.checkVolumeAvailability(scopePath); err != nil {
		return err
	}

	directory, err := openWalkPath(r.root, scopePath)
	if err != nil {
		return fmt.Errorf("open walk scope %q: %w", scopePath, err)
	}
	initial, err := directory.Stat()
	if err != nil || !initial.IsDir() {
		_ = directory.Close()
		return fmt.Errorf("inspect walk scope %q: %w", scopePath, ErrNotDirectory)
	}
	walkErr := r.walkDirectory(ctx, directory, scopePath, excludedPath, visit)
	closeErr := directory.Close()
	if walkErr != nil {
		return walkErr
	}
	if closeErr != nil {
		return fmt.Errorf("close walk root: %w", closeErr)
	}
	if err := r.verifyRootPath(); err != nil {
		return fmt.Errorf("verify walk root after scan: %w", err)
	}
	current, err := openWalkPath(r.root, scopePath)
	if err != nil {
		return fmt.Errorf("recheck walk scope %q: %w", scopePath, ErrPathChanged)
	}
	currentInfo, statErr := current.Stat()
	_ = current.Close()
	if statErr != nil || !os.SameFile(initial, currentInfo) {
		return fmt.Errorf("walk scope %q: %w", scopePath, ErrPathChanged)
	}
	return ctx.Err()
}

func openWalkPath(root *os.Root, scopePath string) (*os.File, error) {
	if scopePath == "" {
		return root.Open(".")
	}
	directory, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	for _, component := range strings.Split(scopePath, "/") {
		next, openErr := openWalkDirectory(directory, component)
		_ = directory.Close()
		if openErr != nil {
			return nil, openErr
		}
		directory = next
	}
	return directory, nil
}

func (r *Root) walkDirectory(ctx context.Context, directory *os.File, relativePath, excludedPath string, visit func(Entry) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := r.verifyWalkVolume(directory, relativePath); err != nil {
		return err
	}
	initial, err := directory.Stat()
	if err != nil {
		return fmt.Errorf("inspect directory %q: %w", relativePath, err)
	}
	if !initial.IsDir() {
		return fmt.Errorf("inspect directory %q: %w", relativePath, ErrNotDirectory)
	}

	seen := make(map[string]struct{})
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		children, readErr := directory.ReadDir(walkReadBatchSize)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return fmt.Errorf("read directory %q: %w", relativePath, readErr)
		}
		for _, child := range children {
			if err := ctx.Err(); err != nil {
				return err
			}
			name := child.Name()
			if name == TrashDirectoryName || isArchiveStageComponent(name) {
				continue
			}
			childPath := path.Join(relativePath, name)
			seen[childPath] = struct{}{}
			if err := r.walkEntry(ctx, directory, name, childPath, excludedPath, visit); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) || len(children) == 0 {
			break
		}
	}
	for _, mount := range r.registeredMounts {
		if relativeParent(mount.path) != relativePath || mount.path == excludedPath {
			continue
		}
		if _, exists := seen[mount.path]; exists {
			continue
		}
		if err := r.checkRegisteredMount(mount); errors.Is(err, ErrVolumeUnavailable) {
			if err := visitWalkEntry(ctx, visit, unavailableVolumeEntry(mount.path)); err != nil {
				return err
			}
		} else if err != nil {
			return fmt.Errorf("inspect mounted volume %q: %w", mount.path, err)
		}
	}

	if err := ctx.Err(); err != nil {
		return err
	}
	final, err := directory.Stat()
	if err != nil {
		return fmt.Errorf("recheck directory %q: %w", relativePath, err)
	}
	if !sameWalkDirectorySnapshot(initial, final) {
		return fmt.Errorf("directory %q: %w", relativePath, ErrPathChanged)
	}
	return nil
}

// verifyWalkVolume 在遍历目录前同时核验路径与已打开句柄的挂载实例。
func (r *Root) verifyWalkVolume(directory *os.File, relativePath string) error {
	volume, isMountRoot := r.registeredMountAt(relativePath)
	if !isMountRoot {
		return r.checkVolumeAvailability(relativePath)
	}
	if err := r.checkRegisteredMount(volume); err != nil {
		return err
	}
	currentID, err := platformMountIDForFD(directory.Fd())
	if err != nil || currentID != volume.id {
		return fmt.Errorf("volume %q: %w", relativePath, ErrVolumeUnavailable)
	}
	return nil
}

func (r *Root) walkEntry(ctx context.Context, parent *os.File, name, relativePath, excludedPath string, visit func(Entry) error) error {
	if relativePath == excludedPath {
		return nil
	}
	volume, isMountRoot := r.registeredMountAt(relativePath)
	if isMountRoot {
		if err := r.checkRegisteredMount(volume); errors.Is(err, ErrVolumeUnavailable) {
			return visitWalkEntry(ctx, visit, unavailableVolumeEntry(relativePath))
		} else if err != nil {
			return fmt.Errorf("inspect mounted volume %q: %w", relativePath, err)
		}
	}
	identityFile, err := openWalkIdentity(parent, name)
	if err != nil {
		return fmt.Errorf("open entry %q: %w", relativePath, err)
	}
	if isMountRoot {
		currentID, err := platformMountIDForFD(identityFile.Fd())
		if err != nil || currentID != volume.id {
			_ = identityFile.Close()
			return visitWalkEntry(ctx, visit, unavailableVolumeEntry(relativePath))
		}
	}
	info, err := identityFile.Stat()
	if err != nil {
		_ = identityFile.Close()
		return fmt.Errorf("inspect entry %q: %w", relativePath, err)
	}

	entry := entryFromInfo(name, relativePath, info)
	r.applyReadOnly(&entry, relativePath)
	if info.Mode()&os.ModeSymlink != 0 {
		entry.Symlink = true
		setLinkIdentity(&entry, info)
		if err := identityFile.Close(); err != nil {
			return fmt.Errorf("close entry %q: %w", relativePath, err)
		}
		if err := visitWalkEntry(ctx, visit, entry); err != nil {
			return err
		}
		return verifyWalkIdentity(parent, name, info, relativePath)
	}

	if !info.IsDir() {
		if err := identityFile.Close(); err != nil {
			return fmt.Errorf("close entry %q: %w", relativePath, err)
		}
		if err := visitWalkEntry(ctx, visit, entry); err != nil {
			return err
		}
		return verifyWalkIdentity(parent, name, info, relativePath)
	}

	directory, err := openWalkDirectory(parent, name)
	if err != nil {
		_ = identityFile.Close()
		return fmt.Errorf("open directory %q: %w", relativePath, err)
	}
	openedInfo, err := directory.Stat()
	if err != nil {
		_ = directory.Close()
		_ = identityFile.Close()
		return fmt.Errorf("inspect opened directory %q: %w", relativePath, err)
	}
	if !openedInfo.IsDir() || !os.SameFile(info, openedInfo) {
		_ = directory.Close()
		_ = identityFile.Close()
		return fmt.Errorf("directory %q: %w", relativePath, ErrPathChanged)
	}
	if err := verifyWalkIdentity(parent, name, info, relativePath); err != nil {
		_ = directory.Close()
		_ = identityFile.Close()
		return err
	}
	if err := identityFile.Close(); err != nil {
		_ = directory.Close()
		return fmt.Errorf("close entry %q: %w", relativePath, err)
	}

	entry = entryFromInfo(name, relativePath, openedInfo)
	r.applyReadOnly(&entry, relativePath)
	if err := visitWalkEntry(ctx, visit, entry); err != nil {
		_ = directory.Close()
		return err
	}
	if err := verifyWalkIdentity(parent, name, openedInfo, relativePath); err != nil {
		_ = directory.Close()
		return err
	}

	walkErr := r.walkDirectory(ctx, directory, relativePath, excludedPath, visit)
	closeErr := directory.Close()
	if walkErr != nil {
		return walkErr
	}
	if closeErr != nil {
		return fmt.Errorf("close directory %q: %w", relativePath, closeErr)
	}
	return verifyWalkIdentity(parent, name, openedInfo, relativePath)
}

func visitWalkEntry(ctx context.Context, visit func(Entry) error, entry Entry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := visit(entry); err != nil {
		return fmt.Errorf("visit %q: %w", entry.Path, err)
	}
	return ctx.Err()
}

func openWalkIdentity(parent *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

func openWalkDirectory(parent *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			return nil, ErrPathChanged
		}
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

// OpenReadNoFollow 通过逐段 no-follow 目录句柄打开普通文件。
func (r *Root) OpenReadNoFollow(relativePath string) (io.ReadCloser, Entry, error) {
	if err := validateRelativePath(relativePath); err != nil || relativePath == "" || hasTrashComponent(relativePath) {
		return nil, Entry{}, ErrInvalidPath
	}
	if err := r.verifyRootPath(); err != nil {
		return nil, Entry{}, err
	}
	if err := r.checkVolumeAvailability(relativePath); err != nil {
		return nil, Entry{}, err
	}
	components := strings.Split(relativePath, "/")
	parentPath := strings.Join(components[:len(components)-1], "/")
	parent, err := openWalkPath(r.root, parentPath)
	if err != nil {
		return nil, Entry{}, err
	}
	defer parent.Close()
	name := components[len(components)-1]
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, Entry{}, err
	}
	file := os.NewFile(uintptr(fd), relativePath)
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, Entry{}, ErrNotRegularFile
	}
	if err := verifyWalkIdentity(parent, name, info, relativePath); err != nil {
		_ = file.Close()
		return nil, Entry{}, err
	}
	entry := entryFromInfo(name, relativePath, info)
	r.applyReadOnly(&entry, relativePath)
	return file, entry, nil
}

func verifyWalkIdentity(parent *os.File, name string, expected os.FileInfo, relativePath string) error {
	current, err := openWalkIdentity(parent, name)
	if err != nil {
		return fmt.Errorf("recheck entry %q: %w", relativePath, ErrPathChanged)
	}
	defer current.Close()
	currentInfo, err := current.Stat()
	if err != nil {
		return fmt.Errorf("recheck entry %q: %w", relativePath, err)
	}
	if !os.SameFile(expected, currentInfo) {
		return fmt.Errorf("recheck entry %q: %w", relativePath, ErrPathChanged)
	}
	return nil
}

func sameWalkDirectorySnapshot(before, after os.FileInfo) bool {
	if !os.SameFile(before, after) || !before.IsDir() || !after.IsDir() || before.Mode() != after.Mode() || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return false
	}
	beforeChange, beforeAvailable := fileChangeTime(before)
	afterChange, afterAvailable := fileChangeTime(after)
	return beforeAvailable == afterAvailable && (!beforeAvailable || beforeChange.Equal(afterChange))
}
