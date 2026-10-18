// 本文件管理 Shell 候选文件的私有暂存与完整性校验。
package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type shellPayloadStore struct {
	dir string
}

var errShellPayloadSizeMismatch = errors.New("Shell 候选长度与预期不符")

func openShellPayloadStore(dir string) (*shellPayloadStore, error) {
	if dir == "" {
		return nil, ErrUnsupported
	}
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(absolute)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, errors.New("Shell 候选暂存路径必须是真实目录")
	}
	if err := os.Chmod(absolute, 0o700); err != nil {
		return nil, err
	}
	return &shellPayloadStore{dir: absolute}, nil
}

func (s *shellPayloadStore) stage(id, sourcePath, expectedSHA256 string, expectedSize int64) error {
	if s == nil || !validConversationID(id) || sourcePath == "" || !validSHA256(expectedSHA256) || expectedSize < 0 {
		return ErrInvalid
	}
	info, err := os.Lstat(sourcePath)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() != expectedSize {
		return ErrConflict
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer source.Close()
	opened, err := source.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return ErrConflict
	}
	target, err := os.OpenFile(filepath.Join(s.dir, id), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	removeTarget := true
	defer func() {
		if removeTarget {
			_ = os.Remove(target.Name())
		}
	}()
	digest := sha256.New()
	written, copyErr := io.CopyN(io.MultiWriter(target, digest), source, expectedSize)
	if copyErr != nil || written != expectedSize {
		_ = target.Close()
		if copyErr != nil {
			return copyErr
		}
		return io.ErrShortWrite
	}
	if err := requireShellEOF(source); err != nil {
		_ = target.Close()
		return err
	}
	if hex.EncodeToString(digest.Sum(nil)) != strings.ToLower(expectedSHA256) {
		_ = target.Close()
		return ErrConflict
	}
	if err := target.Sync(); err != nil {
		_ = target.Close()
		return err
	}
	if err := target.Close(); err != nil {
		return err
	}
	parent, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	if err := parent.Sync(); err != nil {
		_ = parent.Close()
		return err
	}
	if err := parent.Close(); err != nil {
		return err
	}
	removeTarget = false
	return nil
}

func (s *shellPayloadStore) open(id, expectedSHA256 string, expectedSize int64) (*os.File, error) {
	if s == nil || !validConversationID(id) || !validSHA256(expectedSHA256) || expectedSize < 0 {
		return nil, ErrConflict
	}
	name := filepath.Join(s.dir, id)
	info, err := os.Lstat(name)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() != expectedSize {
		return nil, ErrConflict
	}
	file, err := os.Open(name)
	if err != nil {
		return nil, ErrConflict
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		_ = file.Close()
		return nil, ErrConflict
	}
	digest := sha256.New()
	written, err := io.CopyN(digest, file, expectedSize)
	if err != nil || written != expectedSize {
		_ = file.Close()
		return nil, ErrConflict
	}
	if err := requireShellEOF(file); err != nil || hex.EncodeToString(digest.Sum(nil)) != strings.ToLower(expectedSHA256) {
		_ = file.Close()
		return nil, ErrConflict
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = file.Close()
		return nil, ErrConflict
	}
	return file, nil
}

func (s *shellPayloadStore) remove(id string) error {
	if s == nil || !validConversationID(id) {
		return nil
	}
	err := os.Remove(filepath.Join(s.dir, id))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func (s *shellPayloadStore) removeOrphans(referenced map[string]struct{}) error {
	if s == nil {
		return nil
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		id := entry.Name()
		if strings.HasPrefix(id, "stage-") {
			if entry.IsDir() {
				if err := removeShellRunDirectory(filepath.Join(s.dir, id)); err != nil {
					return err
				}
			}
			continue
		}
		if !validConversationID(id) {
			continue
		}
		if _, keep := referenced[id]; keep {
			continue
		}
		if err := s.remove(id); err != nil {
			return err
		}
	}
	return nil
}

func removeShellRunDirectory(root string) error {
	info, err := os.Lstat(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return os.Remove(root)
	}
	if err := filepath.WalkDir(root, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return os.Chmod(current, 0o700)
		}
		return nil
	}); err != nil {
		return err
	}
	return os.RemoveAll(root)
}

func (s *shellPayloadStore) newRunDirectory() (string, error) {
	if s == nil {
		return "", ErrUnsupported
	}
	name, err := os.MkdirTemp(s.dir, "stage-")
	if err != nil {
		return "", fmt.Errorf("create Shell staging directory: %w", err)
	}
	if err := os.Chmod(name, 0o700); err != nil {
		_ = os.RemoveAll(name)
		return "", err
	}
	return name, nil
}

func requireShellEOF(reader io.Reader) error {
	var extra [1]byte
	for emptyReads := 0; emptyReads < 100; emptyReads++ {
		n, err := reader.Read(extra[:])
		if n > 0 {
			return errShellPayloadSizeMismatch
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	return io.ErrNoProgress
}
