//go:build linux

// 本文件通过安全打开的目录句柄递归注册 inotify。
package fsroot

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path"

	"golang.org/x/sys/unix"
)

const (
	watchReadBatchSize  = 128
	watchReadBufferSize = 64 << 10
	watchPollInterval   = 250
	watchChangeMask     = unix.IN_ATTRIB | unix.IN_CLOSE_WRITE | unix.IN_CREATE | unix.IN_DELETE | unix.IN_MOVED_FROM | unix.IN_MOVED_TO | unix.IN_DELETE_SELF | unix.IN_MOVE_SELF | unix.IN_UNMOUNT
	watchDirectoryMask  = unix.IN_CREATE | unix.IN_DELETE | unix.IN_MOVED_FROM | unix.IN_MOVED_TO
	watchDirectoryEvent = unix.IN_DELETE_SELF | unix.IN_MOVE_SELF | unix.IN_UNMOUNT | unix.IN_IGNORED
)

// Watch 阻塞监听目录变化；事件只作提示，调用方负责校准索引。
func (r *Root) Watch(ctx context.Context, changed func()) error {
	if ctx == nil {
		return errors.New("watch requires a context")
	}
	if changed == nil {
		return errors.New("watch requires a change callback")
	}
	if r == nil || r.root == nil {
		return errors.New("watch requires an open root")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	watchFD, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return fmt.Errorf("create inotify instance: %w", err)
	}
	defer unix.Close(watchFD)

	active := make(map[int]struct{})
	ignored := make(map[int]int)
	active, err = r.rebuildWatches(ctx, watchFD, active, ignored)
	if err != nil {
		return err
	}
	changed()

	buffer := make([]byte, watchReadBufferSize)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		poll := []unix.PollFd{{Fd: int32(watchFD), Events: unix.POLLIN}}
		_, err := unix.Poll(poll, watchPollInterval)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return fmt.Errorf("wait for filesystem changes: %w", err)
		}
		if poll[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
			return fmt.Errorf("inotify poll failed: events %#x", poll[0].Revents)
		}
		if poll[0].Revents&unix.POLLIN == 0 {
			continue
		}

		n, readErr := unix.Read(watchFD, buffer)
		if errors.Is(readErr, unix.EINTR) || errors.Is(readErr, unix.EAGAIN) {
			continue
		}
		if readErr != nil {
			return fmt.Errorf("read filesystem changes: %w", readErr)
		}
		if n == 0 {
			return io.ErrUnexpectedEOF
		}
		batch, err := parseInotifyEvents(buffer[:n], ignored)
		if err != nil {
			return err
		}
		if batch.overflow {
			clear(ignored)
		}
		if err := dispatchInotifyBatch(batch, func() error {
			active, err = r.rebuildWatches(ctx, watchFD, active, ignored)
			return err
		}, changed); err != nil {
			return err
		}
	}
}

type inotifyBatch struct {
	changed  bool
	rebuild  bool
	overflow bool
}

func dispatchInotifyBatch(batch inotifyBatch, rebuild func() error, changed func()) error {
	if batch.rebuild {
		if err := rebuild(); err != nil {
			return err
		}
	}
	if batch.changed {
		changed()
	}
	return nil
}

func parseInotifyEvents(data []byte, ignored map[int]int) (inotifyBatch, error) {
	var batch inotifyBatch
	for offset := 0; offset < len(data); {
		if len(data)-offset < unix.SizeofInotifyEvent {
			return inotifyBatch{}, errors.New("truncated inotify event header")
		}
		header := data[offset : offset+unix.SizeofInotifyEvent]
		watchID := int(int32(binary.NativeEndian.Uint32(header[0:4])))
		mask := binary.NativeEndian.Uint32(header[4:8])
		nameLength := binary.NativeEndian.Uint32(header[12:16])
		remaining := len(data) - offset - unix.SizeofInotifyEvent
		if uint64(nameLength) > uint64(remaining) {
			return inotifyBatch{}, errors.New("truncated inotify event name")
		}
		offset += unix.SizeofInotifyEvent + int(nameLength)

		if mask&unix.IN_Q_OVERFLOW != 0 {
			batch.changed = true
			batch.rebuild = true
			batch.overflow = true
			continue
		}
		if mask&unix.IN_IGNORED != 0 && ignored[watchID] > 0 {
			ignored[watchID]--
			if ignored[watchID] == 0 {
				delete(ignored, watchID)
			}
			continue
		}
		if mask&watchChangeMask != 0 {
			batch.changed = true
		}
		if mask&watchDirectoryMask != 0 && mask&unix.IN_ISDIR != 0 || mask&watchDirectoryEvent != 0 {
			batch.changed = true
			batch.rebuild = true
		}
	}
	return batch, nil
}

func (r *Root) rebuildWatches(ctx context.Context, watchFD int, active map[int]struct{}, ignored map[int]int) (map[int]struct{}, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := r.verifyRootPath(); err != nil {
		return nil, fmt.Errorf("verify watch root: %w", err)
	}
	root, err := r.root.Open(".")
	if err != nil {
		return nil, fmt.Errorf("open watch root: %w", err)
	}
	defer root.Close()

	next := make(map[int]struct{}, len(active))
	if err := r.watchDirectoryTree(ctx, watchFD, root, "", next); err != nil {
		return nil, err
	}
	if err := r.verifyRootPath(); err != nil {
		return nil, fmt.Errorf("verify watch root after registration: %w", err)
	}
	for watchID := range active {
		if _, found := next[watchID]; found {
			continue
		}
		if _, err := unix.InotifyRmWatch(watchFD, uint32(watchID)); err != nil {
			if !errors.Is(err, unix.EINVAL) {
				return nil, fmt.Errorf("remove stale inotify watch %d: %w", watchID, err)
			}
		} else {
			ignored[watchID]++
		}
	}
	return next, nil
}

func (r *Root) watchDirectoryTree(ctx context.Context, watchFD int, directory *os.File, relativePath string, seen map[int]struct{}) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := r.verifyWalkVolume(directory, relativePath); err != nil {
		return err
	}
	info, err := directory.Stat()
	if err != nil {
		return fmt.Errorf("inspect watched directory %q: %w", relativePath, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("watch %q: %w", relativePath, ErrNotDirectory)
	}
	fdPath := fmt.Sprintf("/proc/self/fd/%d", directory.Fd())
	watchID, err := unix.InotifyAddWatch(watchFD, fdPath, watchChangeMask|unix.IN_ONLYDIR)
	if err != nil {
		return fmt.Errorf("watch directory %q: %w", relativePath, err)
	}
	seen[watchID] = struct{}{}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		children, readErr := directory.ReadDir(watchReadBatchSize)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return fmt.Errorf("read watched directory %q: %w", relativePath, readErr)
		}
		for _, child := range children {
			if err := ctx.Err(); err != nil {
				return err
			}
			name := child.Name()
			if name == TrashDirectoryName {
				continue
			}
			childPath := path.Join(relativePath, name)
			if err := r.watchDirectoryEntry(ctx, watchFD, directory, name, childPath, seen); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) || len(children) == 0 {
			return nil
		}
	}
}

func (r *Root) watchDirectoryEntry(ctx context.Context, watchFD int, parent *os.File, name, relativePath string, seen map[int]struct{}) error {
	if err := r.checkVolumeAvailability(relativePath); errors.Is(err, ErrVolumeUnavailable) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect watched volume %q: %w", relativePath, err)
	}
	identity, err := openWalkIdentity(parent, name)
	if err != nil {
		if isWatchPathRace(err) {
			return nil
		}
		return fmt.Errorf("open watched entry %q: %w", relativePath, err)
	}
	if volume, isMountRoot := r.registeredMountAt(relativePath); isMountRoot {
		currentID, err := platformMountIDForFD(identity.Fd())
		if err != nil || currentID != volume.id {
			return closeWatchFile(identity, relativePath)
		}
	}
	info, err := identity.Stat()
	if err != nil {
		_ = identity.Close()
		return fmt.Errorf("inspect watched entry %q: %w", relativePath, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return closeWatchFile(identity, relativePath)
	}

	directory, err := openWalkDirectory(parent, name)
	if err != nil {
		_ = identity.Close()
		if isWatchPathRace(err) {
			return nil
		}
		return fmt.Errorf("open watched directory %q: %w", relativePath, err)
	}
	openedInfo, err := directory.Stat()
	if err != nil {
		_ = directory.Close()
		_ = identity.Close()
		return fmt.Errorf("inspect opened watched directory %q: %w", relativePath, err)
	}
	if !openedInfo.IsDir() || !os.SameFile(info, openedInfo) {
		_ = directory.Close()
		_ = identity.Close()
		return nil
	}
	if err := identity.Close(); err != nil {
		_ = directory.Close()
		return fmt.Errorf("close watched identity %q: %w", relativePath, err)
	}
	if err := verifyWalkIdentity(parent, name, openedInfo, relativePath); err != nil {
		_ = directory.Close()
		if errors.Is(err, ErrPathChanged) {
			return nil
		}
		return err
	}
	watchErr := r.watchDirectoryTree(ctx, watchFD, directory, relativePath, seen)
	closeErr := directory.Close()
	if watchErr != nil {
		return watchErr
	}
	if closeErr != nil {
		return fmt.Errorf("close watched directory %q: %w", relativePath, closeErr)
	}
	return nil
}

func closeWatchFile(file *os.File, relativePath string) error {
	if err := file.Close(); err != nil {
		return fmt.Errorf("close watched entry %q: %w", relativePath, err)
	}
	return nil
}

func isWatchPathRace(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) || errors.Is(err, ErrPathChanged)
}
