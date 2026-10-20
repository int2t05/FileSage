// Package duplicates 按完整文件内容查找重复文件。
package duplicates

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"filesage/internal/fsroot"
)

var (
	ErrInvalidScope  = errors.New("invalid duplicate scan scope")
	ErrLimitExceeded = errors.New("duplicate scan limit exceeded")
	ErrFileChanged   = errors.New("file changed during duplicate scan")
	ErrUnavailable   = errors.New("duplicate scan requires stable file identity")
)

const (
	DefaultMaxEntries = 500_000
	DefaultMaxFiles   = 100_000
	DefaultMaxBytes   = int64(1 << 40)
	DefaultWorkers    = 2
	readBufferSize    = 128 * 1024
)

// FileSystem 是安全遍历与读取所需的最小文件系统接口。
type FileSystem interface {
	Walk(context.Context, func(fsroot.Entry) error) error
	LstatEntry(string) (fsroot.Entry, error)
	OpenRead(string) (io.ReadCloser, fsroot.Entry, error)
}

// Options 限制扫描规模和并发读盘数量。
type Options struct {
	MaxEntries int
	MaxFiles   int
	MaxBytes   int64
	Workers    int
}

// Progress 描述目录枚举与候选内容读取进度。
type Progress struct {
	Phase          string
	EntriesVisited int
	CandidateFiles int
	FilesHashed    int
	BytesHashed    int64
	TotalBytes     int64
}

// File 描述一个内容校验通过的路径。
type File struct {
	Path       string    `json:"path"`
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modifiedAt"`
	Device     uint64    `json:"device"`
	Inode      uint64    `json:"inode"`
}

// Group 包含 SHA-256 与完整内容一致的一组独立文件。
type Group struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Files  []File `json:"files"`
}

type candidate struct {
	entry fsroot.Entry
	hash  string
}

type hashResult struct {
	candidate candidate
	err       error
}

// Scan 对范围内目录条目做首尾复核；发现变化时丢弃整次结果。
func Scan(ctx context.Context, filesystem FileSystem, scopes []string, options Options, onProgress func(Progress)) ([]Group, error) {
	if ctx == nil || filesystem == nil {
		return nil, errors.New("duplicate scan requires a context and filesystem")
	}
	options = normalizeOptions(options)
	if options.MaxEntries < 1 || options.MaxFiles < 1 || options.MaxBytes < 1 || options.Workers < 1 || options.Workers > 16 {
		return nil, errors.New("invalid duplicate scan limits")
	}
	normalizedScopes, err := validateScopes(filesystem, scopes)
	if err != nil {
		return nil, err
	}

	bySize := make(map[int64][]fsroot.Entry)
	filesByInode := make(map[[2]uint64]fsroot.Entry)
	treeSnapshot := make(map[string]fsroot.Entry)
	progress := Progress{Phase: "enumerating"}
	reportProgress := func() {
		if onProgress == nil {
			return
		}
		reported := progress
		if reported.Phase == "enumerating" {
			reported.CandidateFiles = 0
		}
		onProgress(reported)
	}
	err = filesystem.Walk(ctx, func(entry fsroot.Entry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		progress.EntriesVisited++
		if progress.EntriesVisited%256 == 0 {
			reportProgress()
		}
		if progress.EntriesVisited > options.MaxEntries {
			return ErrLimitExceeded
		}
		if !inScope(entry.Path, normalizedScopes) || entry.Symlink || entry.Kind != fsroot.KindFile {
			if inScope(entry.Path, normalizedScopes) {
				treeSnapshot[entry.Path] = entry
			}
			return nil
		}
		treeSnapshot[entry.Path] = entry
		if entry.Size < 0 {
			return ErrFileChanged
		}
		if !entry.IdentityAvailable {
			return ErrUnavailable
		}
		identity := [2]uint64{entry.Device, entry.Inode}
		if previous, exists := filesByInode[identity]; exists {
			if entry.Path < previous.Path {
				filesByInode[identity] = entry
			}
			return nil
		}
		progress.CandidateFiles++
		if progress.CandidateFiles > options.MaxFiles {
			return ErrLimitExceeded
		}
		filesByInode[identity] = entry
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := validateScopeSnapshot(filesystem, normalizedScopes, treeSnapshot); err != nil {
		return nil, err
	}
	for _, entry := range filesByInode {
		bySize[entry.Size] = append(bySize[entry.Size], entry)
	}

	var candidates []candidate
	for _, entries := range bySize {
		if len(entries) < 2 {
			continue
		}
		for _, entry := range entries {
			if entry.Size > options.MaxBytes-progress.TotalBytes {
				return nil, ErrLimitExceeded
			}
			progress.TotalBytes += entry.Size
			candidates = append(candidates, candidate{entry: entry})
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].entry.Path < candidates[j].entry.Path })
	progress.CandidateFiles = len(candidates)
	progress.Phase = "hashing"
	reportProgress()
	var results []candidate
	if len(candidates) >= 2 {
		results, err = hashCandidates(ctx, filesystem, candidates, options.Workers, &progress, onProgress)
		if err != nil {
			return nil, err
		}
	}
	if err := verifyTreeSnapshot(ctx, filesystem, normalizedScopes, treeSnapshot, options.MaxEntries); err != nil {
		return nil, err
	}
	if len(candidates) < 2 {
		progress.Phase = "complete"
		reportProgress()
		return []Group{}, nil
	}
	byHash := make(map[string][]fsroot.Entry)
	for _, result := range results {
		byHash[result.hash] = append(byHash[result.hash], result.entry)
	}

	groups := make([]Group, 0)
	for digest, entries := range byHash {
		if len(entries) < 2 {
			continue
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
		files := make([]File, 0, len(entries))
		for _, entry := range entries {
			files = append(files, File{Path: entry.Path, Size: entry.Size, ModifiedAt: entry.ModifiedAt, Device: entry.Device, Inode: entry.Inode})
		}
		groups = append(groups, Group{SHA256: digest, Size: entries[0].Size, Files: files})
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Files[0].Path < groups[j].Files[0].Path })
	progress.Phase = "complete"
	reportProgress()
	return groups, nil
}

func validateScopeSnapshot(filesystem FileSystem, scopes []string, snapshot map[string]fsroot.Entry) error {
	for _, scope := range scopes {
		if scope == "" {
			continue
		}
		observed, exists := snapshot[scope]
		if !exists || observed.Symlink || observed.Kind != fsroot.KindDirectory && observed.Kind != fsroot.KindFile {
			return ErrFileChanged
		}
		current, err := filesystem.LstatEntry(scope)
		if err != nil || !sameEntrySnapshot(observed, current) {
			return ErrFileChanged
		}
	}
	return nil
}

func verifyTreeSnapshot(ctx context.Context, filesystem FileSystem, scopes []string, expected map[string]fsroot.Entry, maxEntries int) error {
	current := make(map[string]fsroot.Entry, len(expected))
	visited := 0
	err := filesystem.Walk(ctx, func(entry fsroot.Entry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		visited++
		if visited > maxEntries {
			return ErrLimitExceeded
		}
		if inScope(entry.Path, scopes) {
			current[entry.Path] = entry
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrLimitExceeded) {
			return err
		}
		return fmt.Errorf("verify duplicate scan tree: %w", ErrFileChanged)
	}
	if len(current) != len(expected) {
		return ErrFileChanged
	}
	for name, previous := range expected {
		latest, exists := current[name]
		if !exists || !sameEntrySnapshot(previous, latest) {
			return ErrFileChanged
		}
	}
	return validateScopeSnapshot(filesystem, scopes, current)
}

func sameEntrySnapshot(previous, current fsroot.Entry) bool {
	if previous.Path != current.Path || previous.Symlink != current.Symlink {
		return false
	}
	left, right := previous.Fingerprint(), current.Fingerprint()
	return left.Exists == right.Exists && left.Kind == right.Kind && left.Size == right.Size &&
		left.ModifiedAt.Equal(right.ModifiedAt) && left.Mode == right.Mode && left.Device == right.Device &&
		left.Inode == right.Inode && left.Links == right.Links && left.IdentityAvailable == right.IdentityAvailable &&
		left.OwnerUID == right.OwnerUID && left.GroupGID == right.GroupGID && left.OwnershipAvailable == right.OwnershipAvailable &&
		left.ChangeTime.Equal(right.ChangeTime) && left.ChangeTimeAvailable == right.ChangeTimeAvailable
}

func normalizeOptions(options Options) Options {
	if options.MaxEntries == 0 {
		options.MaxEntries = DefaultMaxEntries
	}
	if options.MaxFiles == 0 {
		options.MaxFiles = DefaultMaxFiles
	}
	if options.MaxBytes == 0 {
		options.MaxBytes = DefaultMaxBytes
	}
	if options.Workers == 0 {
		options.Workers = DefaultWorkers
	}
	return options
}

func validateScopes(filesystem FileSystem, scopes []string) ([]string, error) {
	if len(scopes) == 0 {
		return []string{""}, nil
	}
	cleaned := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		if scope == "" {
			if len(scopes) == 1 {
				return []string{""}, nil
			}
			return nil, ErrInvalidScope
		}
		if path.IsAbs(scope) || strings.Contains(scope, "\\") || path.Clean(scope) != scope || scope == "." || strings.HasPrefix(scope, "../") {
			return nil, ErrInvalidScope
		}
		entry, err := filesystem.LstatEntry(scope)
		if err != nil || entry.Symlink || entry.Kind != fsroot.KindDirectory && entry.Kind != fsroot.KindFile {
			return nil, ErrInvalidScope
		}
		cleaned = append(cleaned, scope)
	}
	sort.Strings(cleaned)
	result := make([]string, 0, len(cleaned))
	for _, scope := range cleaned {
		if len(result) == 0 || !pathContains(result[len(result)-1], scope) {
			result = append(result, scope)
		}
	}
	return result, nil
}

func pathContains(parent, candidate string) bool {
	return parent == "" || candidate == parent || strings.HasPrefix(candidate, parent+"/")
}

func inScope(candidate string, scopes []string) bool {
	for _, scope := range scopes {
		if pathContains(scope, candidate) {
			return true
		}
	}
	return false
}

func hashCandidates(ctx context.Context, filesystem FileSystem, candidates []candidate, workers int, progress *Progress, onProgress func(Progress)) ([]candidate, error) {
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan candidate)
	results := make(chan hashResult, len(candidates))
	var bytesHashed atomic.Int64
	var filesHashed atomic.Int64
	var reportMu sync.Mutex
	var firstErr error
	var errorMu sync.Mutex

	workerCount := min(workers, len(candidates))
	var workerGroup sync.WaitGroup
	workerGroup.Add(workerCount)
	for range workerCount {
		go func() {
			defer workerGroup.Done()
			for item := range jobs {
				if workCtx.Err() != nil {
					continue
				}
				digest, count, err := hashCandidate(workCtx, filesystem, item.entry)
				if err == nil {
					item.hash = digest
					bytesHashed.Add(count)
					filesHashed.Add(1)
					if onProgress != nil {
						reportMu.Lock()
						progress.BytesHashed = bytesHashed.Load()
						progress.FilesHashed = int(filesHashed.Load())
						onProgress(*progress)
						reportMu.Unlock()
					}
				} else {
					errorMu.Lock()
					if firstErr == nil {
						firstErr = err
						cancel()
					}
					errorMu.Unlock()
				}
				results <- hashResult{candidate: item, err: err}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, item := range candidates {
			select {
			case <-workCtx.Done():
				return
			case jobs <- item:
			}
		}
	}()
	go func() {
		workerGroup.Wait()
		close(results)
	}()

	hashed := make([]candidate, 0, len(candidates))
	for result := range results {
		if result.err == nil {
			hashed = append(hashed, result.candidate)
		}
	}
	errorMu.Lock()
	scanErr := firstErr
	errorMu.Unlock()
	if scanErr != nil {
		return nil, scanErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(hashed) != len(candidates) {
		return nil, ErrFileChanged
	}
	return hashed, nil
}

func hashCandidate(ctx context.Context, filesystem FileSystem, expected fsroot.Entry) (string, int64, error) {
	reader, opened, err := filesystem.OpenRead(expected.Path)
	if err != nil {
		return "", 0, fmt.Errorf("open %q: %w", expected.Path, err)
	}
	defer reader.Close()
	if opened.Symlink || opened.Kind != fsroot.KindFile || !opened.IdentityAvailable || !sameEntrySnapshot(opened, expected) {
		return "", 0, fmt.Errorf("verify opened %q: %w", expected.Path, ErrFileChanged)
	}

	hasher := sha256.New()
	buffer := make([]byte, readBufferSize)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return "", total, err
		}
		read, readErr := reader.Read(buffer)
		if read > 0 {
			total += int64(read)
			if total > expected.Size {
				return "", total, ErrFileChanged
			}
			if _, err := hasher.Write(buffer[:read]); err != nil {
				return "", total, err
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return "", total, readErr
			}
			break
		}
		if read == 0 {
			return "", total, io.ErrNoProgress
		}
	}
	if total != expected.Size {
		return "", total, ErrFileChanged
	}
	current, err := filesystem.LstatEntry(expected.Path)
	if err != nil || current.Symlink || !sameEntrySnapshot(current, expected) {
		return "", total, fmt.Errorf("verify after reading %q: %w", expected.Path, ErrFileChanged)
	}
	return hex.EncodeToString(hasher.Sum(nil)), total, nil
}
