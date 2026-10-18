// Package coordination 为调用方指定的资源键提供访问协调。
package coordination

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
)

var (
	errNilContext       = errors.New("context is nil")
	errNoResources      = errors.New("at least one resource key is required")
	errEmptyResourceKey = errors.New("resource key is empty")
)

// Locker 按调用方提供的资源键协调访问。
type Locker struct {
	mu        sync.Mutex
	resources map[string]*resourceLock
}

type resourceLock struct {
	mu             sync.Mutex
	changed        chan struct{}
	readers        int
	writer         bool
	waitingWriters int
	references     int
}

type lockMode uint8

const (
	sharedLock lockMode = iota
	exclusiveLock
)

type resourceReference struct {
	key   string
	mode  lockMode
	entry *resourceLock
}

// New 创建一个独立的资源协调锁。
func New() *Locker {
	return &Locker{resources: make(map[string]*resourceLock)}
}

// PathKeys 为祖先目录生成共享意图键，为目标路径生成独占键。
func PathKeys(paths ...string) []string {
	modes := make(map[string]lockMode, len(paths)*2)
	add := func(relativePath string, mode lockMode) {
		if prior, exists := modes[relativePath]; !exists || mode > prior {
			modes[relativePath] = mode
		}
	}
	for _, relativePath := range paths {
		add(relativePath, exclusiveLock)
		for {
			if relativePath == "" {
				break
			}
			parent := path.Dir(relativePath)
			if parent == "." {
				parent = ""
			}
			add(parent, sharedLock)
			relativePath = parent
		}
	}
	keys := make([]string, 0, len(modes))
	for relativePath, mode := range modes {
		prefix := "path-intent:"
		if mode == exclusiveLock {
			prefix = "path:"
		}
		keys = append(keys, prefix+relativePath)
	}
	sort.Strings(keys)
	return keys
}

// InodeKey 为同一设备上的硬链接生成共享冲突键。
func InodeKey(device, inode uint64) string {
	return fmt.Sprintf("inode:%016x:%016x", device, inode)
}

// Lock 稳定排序后加锁；普通键独占，路径意图键共享，取消时释放已取得的锁。
func (l *Locker) Lock(ctx context.Context, keys ...string) (func(), error) {
	if ctx == nil {
		return nil, errNilContext
	}
	if len(keys) == 0 {
		return nil, errNoResources
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	modes := make(map[string]lockMode, len(keys))
	orderedKeys := make([]string, 0, len(keys))
	for _, requestedKey := range keys {
		if requestedKey == "" {
			return nil, errEmptyResourceKey
		}
		key, mode := normalizeLockKey(requestedKey)
		if prior, exists := modes[key]; exists {
			if prior < mode {
				modes[key] = mode
			}
		} else {
			modes[key] = mode
			orderedKeys = append(orderedKeys, key)
		}
	}
	sort.Strings(orderedKeys)

	references := l.reserve(orderedKeys, modes)
	acquired := make([]resourceReference, 0, len(references))
	for _, reference := range references {
		if err := acquire(ctx, reference); err != nil {
			l.release(acquired, references)
			return nil, err
		}
		acquired = append(acquired, reference)
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			l.release(acquired, references)
		})
	}, nil
}

func normalizeLockKey(key string) (string, lockMode) {
	if strings.HasPrefix(key, "path-intent:") {
		return "path:" + strings.TrimPrefix(key, "path-intent:"), sharedLock
	}
	return key, exclusiveLock
}

func acquire(ctx context.Context, reference resourceReference) error {
	entry := reference.entry
	waitingWriter := false
	for {
		if err := ctx.Err(); err != nil {
			if waitingWriter {
				stopWaitingWriter(entry)
			}
			return err
		}
		entry.mu.Lock()
		if reference.mode == exclusiveLock && !waitingWriter {
			entry.waitingWriters++
			waitingWriter = true
			notifyLocked(entry)
		}
		available := !entry.writer
		if reference.mode == exclusiveLock {
			available = available && entry.readers == 0
		} else {
			available = available && entry.waitingWriters == 0
		}
		if available {
			if reference.mode == sharedLock {
				entry.readers++
			} else {
				entry.writer = true
				entry.waitingWriters--
				waitingWriter = false
			}
			entry.mu.Unlock()
			if err := ctx.Err(); err != nil {
				releaseResource(reference)
				return err
			}
			return nil
		}
		changed := entry.changed
		entry.mu.Unlock()

		select {
		case <-ctx.Done():
			if waitingWriter {
				stopWaitingWriter(entry)
			}
			return ctx.Err()
		case <-changed:
		}
	}
}

func stopWaitingWriter(entry *resourceLock) {
	entry.mu.Lock()
	entry.waitingWriters--
	notifyLocked(entry)
	entry.mu.Unlock()
}

func releaseResource(reference resourceReference) {
	entry := reference.entry
	entry.mu.Lock()
	if reference.mode == sharedLock {
		entry.readers--
	} else {
		entry.writer = false
	}
	notifyLocked(entry)
	entry.mu.Unlock()
}

func notifyLocked(entry *resourceLock) {
	close(entry.changed)
	entry.changed = make(chan struct{})
}

func (l *Locker) reserve(keys []string, modes map[string]lockMode) []resourceReference {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.resources == nil {
		l.resources = make(map[string]*resourceLock)
	}

	references := make([]resourceReference, 0, len(keys))
	for _, key := range keys {
		entry := l.resources[key]
		if entry == nil {
			entry = &resourceLock{changed: make(chan struct{})}
			l.resources[key] = entry
		}
		entry.references++
		references = append(references, resourceReference{key: key, mode: modes[key], entry: entry})
	}
	return references
}

func (l *Locker) release(acquired, references []resourceReference) {
	for index := len(acquired) - 1; index >= 0; index-- {
		releaseResource(acquired[index])
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	for _, reference := range references {
		reference.entry.references--
		if reference.entry.references == 0 && l.resources[reference.key] == reference.entry {
			delete(l.resources, reference.key)
		}
	}
}
