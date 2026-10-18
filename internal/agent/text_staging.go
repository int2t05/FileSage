// 本文件管理 Agent 文本候选的私有暂存与完整性校验。
package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const maxTextEditBytes = 32 << 10

type textPayloadStore struct {
	dir string
}

// openTextPayloadStore 创建仅服务账号可读写的候选正文目录。
func openTextPayloadStore(dir string) (*textPayloadStore, error) {
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
		return nil, errors.New("Agent 候选暂存路径必须是真实目录")
	}
	if err := os.Chmod(absolute, 0o700); err != nil {
		return nil, err
	}
	return &textPayloadStore{dir: absolute}, nil
}

func (s *textPayloadStore) stage(id string, content []byte) (string, int64, error) {
	if s == nil || !validConversationID(id) || !validTextFileCandidate(content) {
		return "", 0, ErrInvalid
	}
	file, err := os.OpenFile(filepath.Join(s.dir, id), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", 0, err
	}
	written, err := file.Write(content)
	if err != nil || written != len(content) {
		_ = file.Close()
		_ = os.Remove(file.Name())
		if err != nil {
			return "", 0, err
		}
		return "", 0, io.ErrShortWrite
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return "", 0, err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(file.Name())
		return "", 0, err
	}
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:]), int64(len(content)), nil
}

func (s *textPayloadStore) read(id, expectedDigest string, expectedSize int64) ([]byte, error) {
	if s == nil || !validConversationID(id) || !validSHA256(expectedDigest) || expectedSize < 0 || expectedSize > maxTextEditBytes {
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
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, maxTextEditBytes+1))
	if err != nil || int64(len(content)) != expectedSize || !validTextFileCandidate(content) {
		return nil, ErrConflict
	}
	digest := sha256.Sum256(content)
	if hex.EncodeToString(digest[:]) != strings.ToLower(expectedDigest) {
		return nil, ErrConflict
	}
	return content, nil
}

func (s *textPayloadStore) remove(id string) error {
	if s == nil || !validConversationID(id) {
		return nil
	}
	err := os.Remove(filepath.Join(s.dir, id))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func (s *textPayloadStore) removeOrphans(referenced map[string]struct{}) error {
	if s == nil {
		return nil
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		id := entry.Name()
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

func validTextEditCandidate(content []byte) bool {
	return len(content) > 0 && validTextFileCandidate(content)
}

func validTextFileCandidate(content []byte) bool {
	if len(content) > maxTextEditBytes || !utf8.Valid(content) || strings.ContainsRune(string(content), '\x00') {
		return false
	}
	return true
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
