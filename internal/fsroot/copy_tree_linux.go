//go:build linux

// 本文件通过目录句柄复制并原子发布目录树。
package fsroot

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const treeCopyTempPrefix = ".filesage-tree-copy-"
const treeCopyMarkerVersion = "filesage-tree-copy-v1"

// treeCopyTemp 持有暂存目录与进程锁，直到发布或安全回收。
type treeCopyTemp struct {
	name      string
	directory *os.File
	stat      unix.Statx_t
	marker    string
	lock      *os.File
	lockStat  unix.Stat_t
}

// platformCopyTreeNoReplace 在 Linux 上安全暂存并原子发布目录副本。
func platformCopyTreeNoReplace(ctx context.Context, root *Root, source, destination string, expected TreeSnapshot) (result TreeCopyResult, returnErr error) {
	result.FileDigests = make(map[string]string)
	expected, err := prepareTreeSnapshot(expected)
	if err != nil {
		return TreeCopyResult{}, err
	}
	entries, children, err := treeCopyIndex(expected)
	if err != nil {
		return TreeCopyResult{}, err
	}

	var sourceParent *os.File
	var sourceRootNode *snapshotNode
	var sourceName string
	var sourceDirectory *os.File
	if source == "" {
		sourceDirectory, err = root.root.Open(".")
		if err != nil {
			return TreeCopyResult{}, fmt.Errorf("open copy source root: %w", err)
		}
		defer sourceDirectory.Close()
		if err := compareDirectoryFD(sourceDirectory, entries[source]); err != nil {
			return TreeCopyResult{}, err
		}
	} else {
		sourceParentPath := parentPathForOperation(source)
		sourceParent, err = openRootDirectoryNoFollow(root.root, sourceParentPath)
		if err != nil {
			return TreeCopyResult{}, copyTreeConflict(err)
		}
		defer sourceParent.Close()
		if err := verifySnapshotParent(root, sourceParentPath, sourceParent); err != nil {
			return TreeCopyResult{}, copyTreeConflict(err)
		}
		sourceName = path.Base(source)
		node, openErr := snapshotNodeAt(ctx, sourceParent, sourceName, source)
		if openErr != nil {
			return TreeCopyResult{}, copyTreeConflict(openErr)
		}
		sourceRootNode = &node
		defer node.identity.Close()
		if !sameTreeSnapshotItem(entries[source], snapshotItem(node.entry, node.stat.Mnt_id)) {
			return TreeCopyResult{}, ErrConflict
		}
		sourceDirectory, err = openWalkDirectory(sourceParent, sourceName)
		if err != nil {
			return TreeCopyResult{}, copyTreeConflict(err)
		}
		defer sourceDirectory.Close()
		opened, statErr := snapshotStatxFD(int(sourceDirectory.Fd()))
		if statErr != nil {
			return TreeCopyResult{}, statErr
		}
		if !sameSnapshotStat(node.stat, opened) {
			return TreeCopyResult{}, ErrConflict
		}
	}

	destinationParentPath := parentPathForOperation(destination)
	destinationParent, err := openRootDirectoryNoFollow(root.root, destinationParentPath)
	if err != nil {
		return TreeCopyResult{}, fmt.Errorf("open copy destination parent: %w", err)
	}
	defer destinationParent.Close()
	if err := verifySnapshotParent(root, destinationParentPath, destinationParent); err != nil {
		return TreeCopyResult{}, copyTreeConflict(err)
	}
	if err := cleanupTreeCopyTemps(destinationParent); err != nil {
		return TreeCopyResult{}, fmt.Errorf("clean interrupted tree-copy staging: %w", err)
	}
	if err := requireCopyDestinationAbsent(destinationParent, destination); err != nil {
		return TreeCopyResult{}, err
	}
	if err := root.VerifyTreeSnapshot(ctx, expected); err != nil {
		return TreeCopyResult{}, copyTreeConflict(err)
	}
	if err := verifyCopySourceRoot(root, source, sourceParent, sourceName, sourceRootNode, sourceDirectory, entries[source]); err != nil {
		return TreeCopyResult{}, err
	}
	if err := verifySnapshotParent(root, destinationParentPath, destinationParent); err != nil {
		return TreeCopyResult{}, copyTreeConflict(err)
	}
	if err := ctx.Err(); err != nil {
		return TreeCopyResult{}, err
	}

	temp, err := createTreeCopyTemp(destinationParent)
	if err != nil {
		return TreeCopyResult{}, fmt.Errorf("create tree-copy staging directory: %w", err)
	}
	tempCreated := true
	published := false
	defer func() {
		if tempCreated && !published && temp.directory != nil {
			if resetErr := resetTreeCopyDirectories(temp.directory); resetErr != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("restore tree-copy staging permissions: %w", resetErr))
			}
		}
		if temp.directory != nil {
			if closeErr := temp.directory.Close(); closeErr != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("close tree-copy staging directory: %w", closeErr))
			}
			temp.directory = nil
		}
		if tempCreated && !published {
			cleanupErr := removeTreeAt(int(destinationParent.Fd()), temp.name, temp.stat.Mnt_id, &trashIdentity{device: statxDevice(temp.stat), inode: temp.stat.Ino})
			if cleanupErr != nil && !errors.Is(cleanupErr, unix.ENOENT) {
				returnErr = errors.Join(returnErr, fmt.Errorf("remove tree-copy staging directory: %w", cleanupErr))
			} else {
				tempCreated = false
			}
		}
		if !tempCreated || published {
			if markerErr := removeTreeCopyMarker(destinationParent, temp.marker, temp.lockStat); markerErr != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("remove tree-copy staging marker: %w", markerErr))
			}
		}
		if temp.lock != nil {
			if closeErr := temp.lock.Close(); closeErr != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("close tree-copy staging lock: %w", closeErr))
			}
		}
	}()

	if err := copyTreeDirectory(ctx, sourceDirectory, sourceParent, sourceName, sourceRootNode, temp.directory, source, entries, children, &result); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := root.VerifyTreeSnapshot(ctx, expected); err != nil {
		return result, copyTreeConflict(err)
	}
	if err := verifyCopySourceRoot(root, source, sourceParent, sourceName, sourceRootNode, sourceDirectory, entries[source]); err != nil {
		return result, err
	}
	if err := verifySnapshotParent(root, destinationParentPath, destinationParent); err != nil {
		return result, copyTreeConflict(err)
	}
	if err := verifyTreeCopyTemp(destinationParent, temp.name, temp.directory, temp.stat); err != nil {
		return result, err
	}

	// 暂存目录权限保持为私有；发布前再还原来源权限。
	if err := temp.directory.Chmod(entries[source].Entry.Mode.Perm()); err != nil {
		return result, fmt.Errorf("set copied root permissions: %w", err)
	}
	if err := temp.directory.Sync(); err != nil {
		return result, fmt.Errorf("sync copied root directory: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := root.VerifyTreeSnapshot(ctx, expected); err != nil {
		return result, copyTreeConflict(err)
	}
	if err := verifyCopySourceRoot(root, source, sourceParent, sourceName, sourceRootNode, sourceDirectory, entries[source]); err != nil {
		return result, err
	}
	if err := verifySnapshotParent(root, destinationParentPath, destinationParent); err != nil {
		return result, copyTreeConflict(err)
	}
	if err := verifyTreeCopyTemp(destinationParent, temp.name, temp.directory, temp.stat); err != nil {
		return result, err
	}
	if err := unix.Renameat2(int(destinationParent.Fd()), temp.name, int(destinationParent.Fd()), path.Base(destination), unix.RENAME_NOREPLACE); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return result, fmt.Errorf("publish tree copy: %w", ErrConflict)
		}
		if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EOPNOTSUPP) {
			return result, fmt.Errorf("publish tree copy: %w: %w", ErrNoReplaceUnsupported, err)
		}
		return result, fmt.Errorf("publish tree copy: %w", err)
	}
	tempCreated = false
	published = true
	result.Published = true
	if err := destinationParent.Sync(); err != nil {
		return result, fmt.Errorf("%w: %w", ErrPublishedNotDurable, err)
	}
	return result, nil
}

// treeCopyIndex 建立快照路径索引，并生成每个目录的直接子项清单。
func treeCopyIndex(snapshot TreeSnapshot) (map[string]TreeSnapshotEntry, map[string][]string, error) {
	entries := make(map[string]TreeSnapshotEntry, len(snapshot.Entries))
	children := make(map[string][]string, len(snapshot.Entries))
	for _, item := range snapshot.Entries {
		entries[item.Entry.Path] = item
		if item.Entry.Path == snapshot.RootPath {
			continue
		}
		parent := path.Dir(item.Entry.Path)
		if parent == "." {
			parent = ""
		}
		children[parent] = append(children[parent], path.Base(item.Entry.Path))
	}
	return entries, children, nil
}

// compareDirectoryFD 将已打开目录与审批快照中的单项比较。
func compareDirectoryFD(directory *os.File, expected TreeSnapshotEntry) error {
	stat, err := snapshotStatxFD(int(directory.Fd()))
	if err != nil {
		return err
	}
	info, err := directory.Stat()
	if err != nil {
		return err
	}
	entry, err := snapshotEntry(expected.Entry.Name, expected.Entry.Path, info, stat)
	if err != nil {
		return err
	}
	if !sameTreeSnapshotItem(expected, snapshotItem(entry, stat.Mnt_id)) {
		return ErrConflict
	}
	return nil
}

// requireCopyDestinationAbsent 拒绝已有目标，最终发布仍由 no-replace 保底。
func requireCopyDestinationAbsent(parent *os.File, destination string) error {
	var existing unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), path.Base(destination), &existing, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		return fmt.Errorf("copy target %q: %w", destination, ErrConflict)
	} else if !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("inspect copy target %q: %w", destination, err)
	}
	return nil
}

// createTreeCopyTemp 创建带进程锁和身份标记的私有暂存目录。
func createTreeCopyTemp(parent *os.File) (treeCopyTemp, error) {
	parentStat, err := snapshotStatxFD(int(parent.Fd()))
	if err != nil {
		return treeCopyTemp{}, err
	}
	for range 8 {
		suffix := make([]byte, 12)
		if _, err := rand.Read(suffix); err != nil {
			return treeCopyTemp{}, err
		}
		nonce := hex.EncodeToString(suffix)
		name := treeCopyTempPrefix + nonce
		marker := name + ".lock"
		lockFD, err := unix.Openat(int(parent.Fd()), marker, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		if err != nil {
			if errors.Is(err, unix.EEXIST) {
				continue
			}
			return treeCopyTemp{}, err
		}
		lock := os.NewFile(uintptr(lockFD), marker)
		var lockStat unix.Stat_t
		if err := unix.Fstat(lockFD, &lockStat); err != nil {
			_ = lock.Close()
			return treeCopyTemp{}, err
		}
		if err := unix.Flock(lockFD, unix.LOCK_EX|unix.LOCK_NB); err != nil {
			_ = removeTreeCopyMarker(parent, marker, lockStat)
			_ = lock.Close()
			return treeCopyTemp{}, err
		}
		if _, err := lock.WriteString(treeCopyPendingMarker(nonce)); err != nil {
			_ = removeTreeCopyMarker(parent, marker, lockStat)
			_ = lock.Close()
			return treeCopyTemp{}, err
		}
		if err := lock.Sync(); err != nil {
			_ = removeTreeCopyMarker(parent, marker, lockStat)
			_ = lock.Close()
			return treeCopyTemp{}, err
		}
		if err := unix.Mkdirat(int(parent.Fd()), name, 0o700); err != nil {
			_ = removeTreeCopyMarker(parent, marker, lockStat)
			_ = lock.Close()
			if errors.Is(err, unix.EEXIST) {
				continue
			}
			return treeCopyTemp{}, err
		}
		fd, openErr := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if openErr != nil {
			cleanupErr := removeUnopenedTreeCopyTemp(parent, name, parentStat.Mnt_id)
			_ = removeTreeCopyMarker(parent, marker, lockStat)
			_ = lock.Close()
			return treeCopyTemp{}, errors.Join(openErr, cleanupErr)
		}
		directory := os.NewFile(uintptr(fd), name)
		stat, statErr := snapshotStatxFD(int(directory.Fd()))
		if statErr != nil {
			_ = directory.Close()
			cleanupErr := removeUnopenedTreeCopyTemp(parent, name, parentStat.Mnt_id)
			_ = removeTreeCopyMarker(parent, marker, lockStat)
			_ = lock.Close()
			return treeCopyTemp{}, errors.Join(statErr, cleanupErr)
		}
		if stat.Mnt_id != parentStat.Mnt_id {
			_ = directory.Close()
			cleanupErr := removeUnopenedTreeCopyTemp(parent, name, parentStat.Mnt_id)
			_ = removeTreeCopyMarker(parent, marker, lockStat)
			_ = lock.Close()
			return treeCopyTemp{}, errors.Join(ErrNestedMount, cleanupErr)
		}
		activeMarker := treeCopyActiveMarker(nonce, stat)
		if err := lock.Truncate(0); err != nil {
			_ = directory.Close()
			cleanupErr := removeUnopenedTreeCopyTemp(parent, name, parentStat.Mnt_id)
			_ = removeTreeCopyMarker(parent, marker, lockStat)
			_ = lock.Close()
			return treeCopyTemp{}, errors.Join(err, cleanupErr)
		}
		if _, err := lock.Seek(0, io.SeekStart); err != nil {
			_ = directory.Close()
			cleanupErr := removeUnopenedTreeCopyTemp(parent, name, parentStat.Mnt_id)
			_ = removeTreeCopyMarker(parent, marker, lockStat)
			_ = lock.Close()
			return treeCopyTemp{}, errors.Join(err, cleanupErr)
		}
		if _, err := lock.WriteString(activeMarker); err != nil {
			_ = directory.Close()
			cleanupErr := removeUnopenedTreeCopyTemp(parent, name, parentStat.Mnt_id)
			_ = removeTreeCopyMarker(parent, marker, lockStat)
			_ = lock.Close()
			return treeCopyTemp{}, errors.Join(err, cleanupErr)
		}
		if err := lock.Sync(); err != nil {
			_ = directory.Close()
			cleanupErr := removeUnopenedTreeCopyTemp(parent, name, parentStat.Mnt_id)
			_ = removeTreeCopyMarker(parent, marker, lockStat)
			_ = lock.Close()
			return treeCopyTemp{}, errors.Join(err, cleanupErr)
		}
		return treeCopyTemp{name: name, directory: directory, stat: stat, marker: marker, lock: lock, lockStat: lockStat}, nil
	}
	return treeCopyTemp{}, fs.ErrExist
}

// platformCleanupTreeCopyStaging 只清理已验证且没有活动进程持锁的暂存目录。
func platformCleanupTreeCopyStaging(root *Root, relativeParent string) error {
	parent, err := openRootDirectoryNoFollow(root.root, relativeParent)
	if err != nil {
		return err
	}
	defer parent.Close()
	if err := verifySnapshotParent(root, relativeParent, parent); err != nil {
		return err
	}
	return cleanupTreeCopyTemps(parent)
}

// cleanupTreeCopyTemps 按随机标记、文件锁和目录身份回收中断暂存项。
func cleanupTreeCopyTemps(parent *os.File) error {
	parentStat, err := snapshotStatxFD(int(parent.Fd()))
	if err != nil {
		return err
	}
	entries, err := parent.ReadDir(-1)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		marker := entry.Name()
		if !strings.HasSuffix(marker, ".lock") {
			continue
		}
		name := strings.TrimSuffix(marker, ".lock")
		nonce, ok := treeCopyTempNonce(name)
		if !ok {
			continue
		}
		fd, err := unix.Openat(int(parent.Fd()), marker, unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			return err
		}
		lock := os.NewFile(uintptr(fd), marker)
		var lockStat unix.Stat_t
		if err := unix.Fstat(fd, &lockStat); err != nil {
			_ = lock.Close()
			return err
		}
		if lockStat.Mode&unix.S_IFMT != unix.S_IFREG || lockStat.Uid != uint32(os.Geteuid()) || lockStat.Mode&0o777 != 0o600 || lockStat.Size > 256 {
			_ = lock.Close()
			continue
		}
		if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
			_ = lock.Close()
			if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
				return fmt.Errorf("active tree-copy staging %q: %w", name, ErrTreeCopyInProgress)
			}
			return err
		}
		if _, err := lock.Seek(0, io.SeekStart); err != nil {
			_ = lock.Close()
			return err
		}
		markerData, err := io.ReadAll(io.LimitReader(lock, 257))
		if err != nil {
			_ = lock.Close()
			return err
		}
		stageStat, stageErr := snapshotStatxAt(int(parent.Fd()), name, unix.AT_SYMLINK_NOFOLLOW)
		if errors.Is(stageErr, unix.ENOENT) {
			if validTreeCopyMarker(string(markerData), nonce) {
				removeErr := removeTreeCopyMarker(parent, marker, lockStat)
				_ = lock.Close()
				if removeErr != nil {
					return removeErr
				}
				continue
			}
			_ = lock.Close()
			continue
		}
		if stageErr != nil {
			_ = lock.Close()
			return stageErr
		}
		if stageStat.Mode&unix.S_IFMT != unix.S_IFDIR || stageStat.Mnt_id != parentStat.Mnt_id || stageStat.Uid != uint32(os.Geteuid()) {
			_ = lock.Close()
			continue
		}
		if !validTreeCopyStageMarker(string(markerData), nonce, stageStat) {
			if stageStat.Mode&0o777 == 0o700 && strings.HasPrefix(string(markerData), treeCopyMarkerVersion+"\nactive\n"+nonce+"\n") {
				if err := removeEmptyTreeCopyTemp(parent, name, marker, lock, lockStat, stageStat); err != nil {
					_ = lock.Close()
					return err
				}
				continue
			}
			_ = lock.Close()
			continue
		}
		if stageStat.Mode&0o777 == 0o700 && strings.HasPrefix(string(markerData), treeCopyMarkerVersion+"\npending\n") {
			stageFD, openErr := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if openErr != nil {
				_ = lock.Close()
				return openErr
			}
			stage := os.NewFile(uintptr(stageFD), name)
			children, readErr := stage.ReadDir(1)
			closeErr := stage.Close()
			if readErr != nil || closeErr != nil || len(children) != 0 {
				_ = lock.Close()
				return errors.Join(readErr, closeErr, ErrPathChanged)
			}
		}
		pathFD, err := unix.Openat(int(parent.Fd()), name, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			_ = lock.Close()
			return err
		}
		pathDirectory := os.NewFile(uintptr(pathFD), name)
		openedStageStat, statErr := snapshotStatxFD(pathFD)
		if statErr != nil || !sameSnapshotObject(stageStat, openedStageStat) {
			_ = pathDirectory.Close()
			_ = lock.Close()
			return errors.Join(statErr, ErrPathChanged)
		}
		chmodErr := unix.Fchmodat(pathFD, "", 0o700, unix.AT_EMPTY_PATH)
		closePathErr := pathDirectory.Close()
		if chmodErr != nil || closePathErr != nil {
			_ = lock.Close()
			return errors.Join(chmodErr, closePathErr)
		}
		currentStageStat, err := snapshotStatxAt(int(parent.Fd()), name, unix.AT_SYMLINK_NOFOLLOW)
		if err != nil || !sameSnapshotObject(stageStat, currentStageStat) {
			_ = lock.Close()
			return errors.Join(err, ErrPathChanged)
		}
		stageFD, openErr := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if openErr != nil {
			_ = lock.Close()
			return openErr
		}
		stage := os.NewFile(uintptr(stageFD), name)
		cleanupErr := resetTreeCopyDirectories(stage)
		if cleanupErr == nil {
			cleanupErr = removeTreeAt(int(parent.Fd()), name, parentStat.Mnt_id, &trashIdentity{device: statxDevice(stageStat), inode: stageStat.Ino})
		}
		closeErr := stage.Close()
		if cleanupErr == nil {
			cleanupErr = closeErr
		} else if closeErr != nil {
			cleanupErr = errors.Join(cleanupErr, closeErr)
		}
		if cleanupErr != nil {
			_ = lock.Close()
			return cleanupErr
		}
		if err := removeTreeCopyMarker(parent, marker, lockStat); err != nil {
			_ = lock.Close()
			return err
		}
		if err := parent.Sync(); err != nil {
			_ = lock.Close()
			return err
		}
		if err := lock.Close(); err != nil {
			return err
		}
	}
	return nil
}

// removeEmptyTreeCopyTemp 清理写入身份标记前中断留下的空目录。
func removeEmptyTreeCopyTemp(parent *os.File, name, marker string, lock *os.File, lockStat unix.Stat_t, expected unix.Statx_t) error {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	directory := os.NewFile(uintptr(fd), name)
	children, readErr := directory.ReadDir(1)
	closeErr := directory.Close()
	if readErr != nil || closeErr != nil {
		return errors.Join(readErr, closeErr)
	}
	if len(children) != 0 {
		return ErrPathChanged
	}
	if err := removeTreeAt(int(parent.Fd()), name, expected.Mnt_id, &trashIdentity{device: statxDevice(expected), inode: expected.Ino}); err != nil {
		return err
	}
	if err := removeTreeCopyMarker(parent, marker, lockStat); err != nil {
		return err
	}
	if err := parent.Sync(); err != nil {
		return err
	}
	return lock.Close()
}

// resetTreeCopyDirectories 还原暂存目录权限，确保失败清理能遍历整棵树。
func resetTreeCopyDirectories(directory *os.File) error {
	if err := directory.Chmod(0o700); err != nil {
		return err
	}
	entries, err := directory.ReadDir(-1)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		pathFD, err := unix.Openat(int(directory.Fd()), entry.Name(), unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			if errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
				continue
			}
			return err
		}
		pathDirectory := os.NewFile(uintptr(pathFD), entry.Name())
		pathStat, err := snapshotStatxFD(pathFD)
		if err != nil {
			_ = pathDirectory.Close()
			return err
		}
		if err := unix.Fchmodat(pathFD, "", 0o700, unix.AT_EMPTY_PATH); err != nil {
			_ = pathDirectory.Close()
			return err
		}
		if err := pathDirectory.Close(); err != nil {
			return err
		}
		fd, err := unix.Openat(int(directory.Fd()), entry.Name(), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		child := os.NewFile(uintptr(fd), entry.Name())
		childStat, err := snapshotStatxFD(fd)
		if err != nil || !sameSnapshotObject(pathStat, childStat) {
			_ = child.Close()
			return errors.Join(err, ErrPathChanged)
		}
		resetErr := resetTreeCopyDirectories(child)
		closeErr := child.Close()
		if resetErr != nil || closeErr != nil {
			return errors.Join(resetErr, closeErr)
		}
	}
	return nil
}

// treeCopyTempNonce 只接受 FileSage 生成的 96 位随机暂存名称。
func treeCopyTempNonce(name string) (string, bool) {
	if !strings.HasPrefix(name, treeCopyTempPrefix) {
		return "", false
	}
	nonce := strings.TrimPrefix(name, treeCopyTempPrefix)
	if len(nonce) != 24 {
		return "", false
	}
	if _, err := hex.DecodeString(nonce); err != nil || strings.ToLower(nonce) != nonce {
		return "", false
	}
	return nonce, true
}

// treeCopyPendingMarker 记录目录创建前的可恢复标记。
func treeCopyPendingMarker(nonce string) string {
	return treeCopyMarkerVersion + "\npending\n" + nonce + "\n"
}

// treeCopyActiveMarker 将随机标记绑定到暂存目录身份。
func treeCopyActiveMarker(nonce string, stat unix.Statx_t) string {
	return fmt.Sprintf("%s\nactive\n%s\n%d\n%d\n%d\n", treeCopyMarkerVersion, nonce, statxDevice(stat), stat.Ino, stat.Mnt_id)
}

// validTreeCopyMarker 检查暂存目录缺失时遗留标记的格式。
func validTreeCopyMarker(marker, nonce string) bool {
	return marker == treeCopyPendingMarker(nonce) || strings.HasPrefix(marker, treeCopyMarkerVersion+"\nactive\n"+nonce+"\n")
}

// validTreeCopyStageMarker 确认磁盘目录仍是标记对应的暂存项。
func validTreeCopyStageMarker(marker, nonce string, stat unix.Statx_t) bool {
	if marker == treeCopyPendingMarker(nonce) {
		return stat.Mode&0o777 == 0o700
	}
	fields := strings.Split(marker, "\n")
	if len(fields) != 7 || fields[0] != treeCopyMarkerVersion || fields[1] != "active" || fields[2] != nonce || fields[6] != "" {
		return false
	}
	device, deviceErr := strconv.ParseUint(fields[3], 10, 64)
	inode, inodeErr := strconv.ParseUint(fields[4], 10, 64)
	mountID, mountErr := strconv.ParseUint(fields[5], 10, 64)
	return deviceErr == nil && inodeErr == nil && mountErr == nil && device == statxDevice(stat) && inode == stat.Ino && mountID == stat.Mnt_id
}

// removeTreeCopyMarker 仅按已打开文件的 inode 删除标记。
func removeTreeCopyMarker(parent *os.File, marker string, expected unix.Stat_t) error {
	current, err := snapshotStatxAt(int(parent.Fd()), marker, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	if current.Mode&unix.S_IFMT != unix.S_IFREG || current.Ino != expected.Ino || statxDevice(current) != uint64(expected.Dev) {
		return ErrPathChanged
	}
	if err := unix.Unlinkat(int(parent.Fd()), marker, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return nil
}

// removeUnopenedTreeCopyTemp 只按刚创建的随机名称尝试安全清理。
func removeUnopenedTreeCopyTemp(parent *os.File, name string, mountID uint64) error {
	stat, err := snapshotStatxAt(int(parent.Fd()), name, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Mnt_id != mountID {
		return ErrPathChanged
	}
	return removeTreeAt(int(parent.Fd()), name, mountID, &trashIdentity{device: statxDevice(stat), inode: stat.Ino})
}

// copyTreeDirectory 按快照清单递归复制目录内容。
func copyTreeDirectory(ctx context.Context, sourceDirectory, sourceParent *os.File, sourceName string, sourceNode *snapshotNode, destinationDirectory *os.File, relativePath string, entries map[string]TreeSnapshotEntry, children map[string][]string, result *TreeCopyResult) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	expected, ok := entries[relativePath]
	if !ok || expected.Entry.Kind != KindDirectory {
		return ErrInvalidPath
	}
	if err := compareDirectoryFD(sourceDirectory, expected); err != nil {
		return copyTreeConflict(err)
	}
	initialNames, err := snapshotDirectoryNames(sourceDirectory, -1)
	if err != nil {
		return copyTreeConflict(err)
	}
	expectedNames := children[relativePath]
	if !sameNames(initialNames, expectedNames) {
		return ErrConflict
	}
	for _, name := range expectedNames {
		if err := ctx.Err(); err != nil {
			return err
		}
		childPath := path.Join(relativePath, name)
		if relativePath == "" {
			childPath = name
		}
		expectedChild, exists := entries[childPath]
		if !exists {
			return ErrConflict
		}
		node, err := snapshotNodeAt(ctx, sourceDirectory, name, childPath)
		if err != nil {
			return copyTreeConflict(err)
		}
		if !sameTreeSnapshotItem(expectedChild, snapshotItem(node.entry, node.stat.Mnt_id)) {
			_ = node.identity.Close()
			return ErrConflict
		}
		switch expectedChild.Entry.Kind {
		case KindFile:
			err = copyTreeFile(ctx, sourceDirectory, &node, destinationDirectory, expectedChild, result)
		case KindDirectory:
			err = copyTreeSubdirectory(ctx, sourceDirectory, &node, destinationDirectory, expectedChild, entries, children, result)
		default:
			err = ErrTreeCopyUnsafeEntry
		}
		closeErr := node.identity.Close()
		if err != nil || closeErr != nil {
			return copyTreeConflict(errors.Join(err, closeErr))
		}
	}
	finalNames, err := snapshotDirectoryNames(sourceDirectory, -1)
	if err != nil {
		return copyTreeConflict(err)
	}
	if !sameNames(initialNames, finalNames) {
		return ErrConflict
	}
	if err := compareDirectoryFD(sourceDirectory, expected); err != nil {
		return copyTreeConflict(err)
	}
	if sourceNode != nil {
		if err := verifySnapshotNode(sourceParent, sourceName, relativePath, sourceNode); err != nil {
			return copyTreeConflict(err)
		}
	} else if relativePath == "" && sourceParent == nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if err := destinationDirectory.Sync(); err != nil {
		return fmt.Errorf("sync copied directory %q: %w", relativePath, err)
	}
	return ctx.Err()
}

// copyTreeSubdirectory 创建子目录后递归复制，并保留其权限位。
func copyTreeSubdirectory(ctx context.Context, sourceParent *os.File, sourceNode *snapshotNode, destinationParent *os.File, expected TreeSnapshotEntry, entries map[string]TreeSnapshotEntry, children map[string][]string, result *TreeCopyResult) error {
	name := expected.Entry.Name
	fd, err := unix.Openat(int(sourceParent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return copyTreeConflict(err)
	}
	sourceDirectory := os.NewFile(uintptr(fd), name)
	defer sourceDirectory.Close()
	openedStat, err := snapshotStatxFD(int(sourceDirectory.Fd()))
	if err != nil {
		return err
	}
	if !sameSnapshotStat(sourceNode.stat, openedStat) {
		return ErrConflict
	}
	if err := unix.Mkdirat(int(destinationParent.Fd()), name, 0o700); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return ErrConflict
		}
		return fmt.Errorf("create copied directory %q: %w", expected.Entry.Path, err)
	}
	destinationFD, err := unix.Openat(int(destinationParent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open copied directory %q: %w", expected.Entry.Path, err)
	}
	destinationDirectory := os.NewFile(uintptr(destinationFD), name)
	defer destinationDirectory.Close()
	if err := copyTreeDirectory(ctx, sourceDirectory, sourceParent, name, sourceNode, destinationDirectory, expected.Entry.Path, entries, children, result); err != nil {
		return err
	}
	if err := destinationDirectory.Chmod(expected.Entry.Mode.Perm()); err != nil {
		return fmt.Errorf("set copied directory %q permissions: %w", expected.Entry.Path, err)
	}
	if err := destinationDirectory.Sync(); err != nil {
		return fmt.Errorf("sync copied directory %q: %w", expected.Entry.Path, err)
	}
	return verifySnapshotNode(sourceParent, name, expected.Entry.Path, sourceNode)
}

// copyTreeFile 流式复制普通文件并分别核验源与目标 SHA-256。
func copyTreeFile(ctx context.Context, sourceParent *os.File, sourceNode *snapshotNode, destinationParent *os.File, expected TreeSnapshotEntry, result *TreeCopyResult) (returnErr error) {
	name := expected.Entry.Name
	sourceFD, err := unix.Openat(int(sourceParent.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return copyTreeConflict(err)
	}
	sourceFile := os.NewFile(uintptr(sourceFD), name)
	defer func() {
		if closeErr := sourceFile.Close(); closeErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close copy source %q: %w", expected.Entry.Path, closeErr))
		}
	}()
	sourceStat, err := snapshotStatxFD(int(sourceFile.Fd()))
	if err != nil {
		return err
	}
	if sourceStat.Mode&unix.S_IFMT != unix.S_IFREG || !sameSnapshotStat(sourceNode.stat, sourceStat) {
		return ErrConflict
	}

	destinationFD, err := unix.Openat(int(destinationParent.Fd()), name, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		if errors.Is(err, unix.EEXIST) {
			return ErrConflict
		}
		return fmt.Errorf("create copied file %q: %w", expected.Entry.Path, err)
	}
	destinationFile := os.NewFile(uintptr(destinationFD), name)
	defer func() {
		if closeErr := destinationFile.Close(); closeErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close copied file %q: %w", expected.Entry.Path, closeErr))
		}
	}()

	sourceDigest := sha256.New()
	written, err := io.CopyN(io.MultiWriter(destinationFile, sourceDigest), contextReader{ctx: ctx, reader: sourceFile}, expected.Entry.Size)
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return fmt.Errorf("source file %q changed during copy: %w", expected.Entry.Path, ErrConflict)
		}
		return err
	}
	if written != expected.Entry.Size {
		return ErrConflict
	}
	if err := requireStreamEOF(contextReader{ctx: ctx, reader: sourceFile}); err != nil {
		if errors.Is(err, ErrSizeMismatch) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return fmt.Errorf("source file %q changed during copy: %w", expected.Entry.Path, ErrConflict)
		}
		return err
	}
	if err := destinationFile.Chmod(expected.Entry.Mode.Perm()); err != nil {
		return fmt.Errorf("set copied file %q permissions: %w", expected.Entry.Path, err)
	}
	if err := destinationFile.Sync(); err != nil {
		return fmt.Errorf("sync copied file %q: %w", expected.Entry.Path, err)
	}
	if _, err := destinationFile.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind copied file %q: %w", expected.Entry.Path, err)
	}
	destinationDigest := sha256.New()
	verifiedBytes, err := io.Copy(destinationDigest, contextReader{ctx: ctx, reader: destinationFile})
	if err != nil {
		return fmt.Errorf("verify copied file %q: %w", expected.Entry.Path, err)
	}
	if verifiedBytes != expected.Entry.Size || subtle.ConstantTimeCompare(sourceDigest.Sum(nil), destinationDigest.Sum(nil)) != 1 {
		return fmt.Errorf("copied file %q failed content verification: %w", expected.Entry.Path, ErrConflict)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	finalSourceStat, err := snapshotStatxFD(int(sourceFile.Fd()))
	if err != nil {
		return err
	}
	if !sameSnapshotStat(sourceNode.stat, finalSourceStat) {
		return ErrConflict
	}
	if err := verifySnapshotNode(sourceParent, name, expected.Entry.Path, sourceNode); err != nil {
		return copyTreeConflict(err)
	}
	result.FilesCopied++
	result.BytesCopied += written
	result.FileDigests[expected.Entry.Path] = hex.EncodeToString(sourceDigest.Sum(nil))
	return nil
}

// verifyCopySourceRoot 重核来源根目录及其父目录仍对应打开的对象。
func verifyCopySourceRoot(root *Root, source string, sourceParent *os.File, sourceName string, sourceNode *snapshotNode, sourceDirectory *os.File, expected TreeSnapshotEntry) error {
	if err := compareDirectoryFD(sourceDirectory, expected); err != nil {
		return copyTreeConflict(err)
	}
	if source == "" {
		if err := root.verifyRootPath(); err != nil {
			return copyTreeConflict(err)
		}
		return nil
	}
	if err := verifySnapshotParent(root, parentPathForOperation(source), sourceParent); err != nil {
		return copyTreeConflict(err)
	}
	if err := verifySnapshotNode(sourceParent, sourceName, source, sourceNode); err != nil {
		return copyTreeConflict(err)
	}
	return nil
}

// verifyTreeCopyTemp 确认暂存路径仍指向本次创建的目录。
func verifyTreeCopyTemp(parent *os.File, name string, directory *os.File, expected unix.Statx_t) error {
	opened, err := snapshotStatxFD(int(directory.Fd()))
	if err != nil {
		return err
	}
	current, err := snapshotStatxAt(int(parent.Fd()), name, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		return ErrPathChanged
	}
	if current.Mode&unix.S_IFMT != unix.S_IFDIR || !sameSnapshotObject(expected, opened) || !sameSnapshotObject(expected, current) || expected.Mnt_id != current.Mnt_id {
		return ErrPathChanged
	}
	return nil
}

// sameTreeSnapshotItem 比较摘要中绑定的单项目元数据。
func sameTreeSnapshotItem(expected, actual TreeSnapshotEntry) bool {
	return digestTreeSnapshot(expected.Entry.Path, []TreeSnapshotEntry{expected}) == digestTreeSnapshot(expected.Entry.Path, []TreeSnapshotEntry{actual})
}

// copyTreeConflict 将来源树竞态统一报告为冲突。
func copyTreeConflict(err error) error {
	if err == nil || errors.Is(err, ErrConflict) {
		return err
	}
	if errors.Is(err, ErrPathChanged) || errors.Is(err, ErrSymlinkPath) || errors.Is(err, ErrNotDirectory) || errors.Is(err, fs.ErrNotExist) || errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
		return errors.Join(ErrConflict, err)
	}
	return err
}
