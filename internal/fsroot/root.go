// Package fsroot 提供受根目录约束的文件浏览、读取与受控写入。
package fsroot

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

var (
	// ErrInvalidPath 表示路径不是有效的根目录相对路径。
	ErrInvalidPath = errors.New("invalid root-relative path")
	// ErrOutsideRoot 表示路径或软链接指向管理根目录之外。
	ErrOutsideRoot = errors.New("path escapes the configured root")
	// ErrDanglingLink 表示软链接目标不存在。
	ErrDanglingLink = errors.New("dangling symbolic link")
	// ErrLinkCycle 表示软链接形成循环或链接层级过深。
	ErrLinkCycle = errors.New("symbolic-link cycle")
	// ErrNotDirectory 表示目标路径不是目录。
	ErrNotDirectory = errors.New("not a directory")
	// ErrNotRegularFile 表示读取目标不是普通文件。
	ErrNotRegularFile = errors.New("not a regular file")
	// ErrPathChanged 表示打开期间路径对应的对象发生变化。
	ErrPathChanged = errors.New("path changed while opening")
	// ErrEntryLimit 表示目录直接子项超过调用方提供的安全上限。
	ErrEntryLimit = errors.New("directory entry limit exceeded")
	// ErrConflict 表示目标已存在或文件状态与快照不符。
	ErrConflict = errors.New("filesystem state conflict")
	// ErrSymlinkPath 要求写入调用方明确使用根内真实路径。
	ErrSymlinkPath = errors.New("write path traverses a symbolic link")
	// ErrHardLinked 拒绝会破坏硬链接身份的内容替换。
	ErrHardLinked = errors.New("content write refused for multiply linked file")
	// ErrIdentityUnavailable 表示无法校验文件身份。
	ErrIdentityUnavailable = errors.New("file identity unavailable")
	// ErrMetadataUnsupported 表示平台无法安全地更新文件属主或权限。
	ErrMetadataUnsupported = errors.New("safe metadata update unsupported")
	// ErrMetadataNeedsReview 表示元数据修改已执行，但结果无法完整复核。
	ErrMetadataNeedsReview = errors.New("metadata update needs review")
	// ErrNoReplaceUnsupported 表示平台或文件系统不支持安全的禁止覆盖重命名。
	ErrNoReplaceUnsupported = errors.New("no-replace rename unsupported")
	// ErrAtomicWriteUnsupported 表示平台不能提供原子替换语义。
	ErrAtomicWriteUnsupported = errors.New("atomic write unsupported")
	// ErrMoveRecoveryUnavailable 表示跨卷移动缺少持久恢复日志。
	ErrMoveRecoveryUnavailable = errors.New("move recovery journal unavailable")
	// ErrSizeMismatch 表示流的实际长度与声明长度不符。
	ErrSizeMismatch = errors.New("stream size does not match expected size")
	// ErrPublishedNotDurable 表示目标已发布，但父目录同步失败。
	ErrPublishedNotDurable = errors.New("file published but parent directory sync failed")
	// ErrTrashUnsupported 表示平台无法验证回收站所需的挂载边界。
	ErrTrashUnsupported = errors.New("safe trash operation unsupported")
	// ErrNestedMount 表示目录树内包含其他挂载点。
	ErrNestedMount = errors.New("directory contains a nested mount")
	// ErrMountRoot 表示不能移动或清理挂载根目录本身。
	ErrMountRoot = errors.New("mount root cannot be moved or purged")
	// ErrVolumeUnavailable 表示启动时登记的子卷当前不可用。
	ErrVolumeUnavailable = errors.New("registered volume is unavailable")
	// ErrWatchUnsupported 表示当前平台没有安全目录监听实现。
	ErrWatchUnsupported = errors.New("safe filesystem watch unsupported")
)

// Kind 表示目录条目的解析类型；不安全或无法解析的链接为 KindOther。
type Kind string

const (
	KindDirectory Kind = "directory"
	KindFile      Kind = "file"
	KindOther     Kind = "other"
)

// TrashDirectoryName 是文件接口隐藏的内部回收目录名。
const TrashDirectoryName = ".filesage-trash"

// LinkStatus 表示软链接的解析状态，普通条目为空。
type LinkStatus string

const (
	LinkStatusInternal    LinkStatus = "internal"
	LinkStatusDangling    LinkStatus = "dangling"
	LinkStatusOutsideRoot LinkStatus = "outside_root"
	LinkStatusCycle       LinkStatus = "cycle"
	LinkStatusUnavailable LinkStatus = "unavailable"
)

// Entry 描述根目录下的条目；可解析链接的主要属性描述其目标，Link* 属性描述链接本身。
type Entry struct {
	Name       string      `json:"name"`
	Path       string      `json:"path"`
	Kind       Kind        `json:"kind"`
	Size       int64       `json:"size"`
	ModifiedAt time.Time   `json:"modifiedAt"`
	Mode       fs.FileMode `json:"mode"`
	Symlink    bool        `json:"symlink"`
	LinkStatus LinkStatus  `json:"linkStatus,omitempty"`
	LinkTarget string      `json:"linkTarget,omitempty"`
	// MountUnavailable 标记已登记但当前缺失的卷入口。
	MountUnavailable bool `json:"mountUnavailable,omitempty"`
	// ReadOnly 禁止在当前条目上执行文件变更。
	ReadOnly bool `json:"readOnly,omitempty"`

	Device             uint64 `json:"device,omitempty"`
	Inode              uint64 `json:"inode,omitempty"`
	Links              uint64 `json:"links,omitempty"`
	IdentityAvailable  bool   `json:"identityAvailable"`
	OwnerUID           uint32 `json:"ownerUid"`
	GroupGID           uint32 `json:"groupGid"`
	OwnershipAvailable bool   `json:"ownershipAvailable"`
	// ChangedAt 是文件状态最后变更时间，仅供服务端审批复核。
	ChangedAt time.Time `json:"-"`
	// ChangeTimeAvailable 表示是否读取到可靠的状态变更时间。
	ChangeTimeAvailable bool `json:"-"`

	LinkDevice            uint64 `json:"linkDevice,omitempty"`
	LinkInode             uint64 `json:"linkInode,omitempty"`
	LinkLinks             uint64 `json:"linkLinks,omitempty"`
	LinkIdentityAvailable bool   `json:"linkIdentityAvailable,omitempty"`
}

// Fingerprint 是用于拒绝过期写入的文件状态快照。
type Fingerprint struct {
	Exists              bool        `json:"exists"`
	Kind                Kind        `json:"kind,omitempty"`
	Size                int64       `json:"size,omitempty"`
	ModifiedAt          time.Time   `json:"modifiedAt,omitempty"`
	Mode                fs.FileMode `json:"mode,omitempty"`
	Device              uint64      `json:"device,omitempty"`
	Inode               uint64      `json:"inode,omitempty"`
	Links               uint64      `json:"links,omitempty"`
	IdentityAvailable   bool        `json:"identityAvailable,omitempty"`
	OwnerUID            uint32      `json:"ownerUid,omitempty"`
	GroupGID            uint32      `json:"groupGid,omitempty"`
	OwnershipAvailable  bool        `json:"ownershipAvailable,omitempty"`
	ChangeTime          time.Time   `json:"changeTime,omitempty"`
	ChangeTimeAvailable bool        `json:"changeTimeAvailable,omitempty"`
}

// Fingerprint 返回条目的状态快照。
func (e Entry) Fingerprint() Fingerprint {
	return Fingerprint{
		Exists:              true,
		Kind:                e.Kind,
		Size:                e.Size,
		ModifiedAt:          e.ModifiedAt,
		Mode:                e.Mode,
		Device:              e.Device,
		Inode:               e.Inode,
		Links:               e.Links,
		IdentityAvailable:   e.IdentityAvailable,
		OwnerUID:            e.OwnerUID,
		GroupGID:            e.GroupGID,
		OwnershipAvailable:  e.OwnershipAvailable,
		ChangeTime:          e.ChangedAt,
		ChangeTimeAvailable: e.ChangeTimeAvailable,
	}
}

// Root 持有根目录句柄，并将文件操作限制在其目录树内。
type Root struct {
	root             *os.Root
	moveRecoveryRoot *os.Root
	rootPath         string
	registeredMounts []registeredMount
	rootReadOnly     bool
}

type registeredMount struct {
	path     string
	id       uint64
	readOnly bool
}

const maxSymlinkHops = 128

// Open 打开管理根目录；根路径本身可以是软链接。
func Open(rootPath string) (*Root, error) {
	if rootPath == "" {
		return nil, fmt.Errorf("open filesystem root: %w", fs.ErrInvalid)
	}
	if info, err := os.Stat(rootPath); err == nil && !info.IsDir() {
		return nil, fmt.Errorf("filesystem root %q: %w", rootPath, ErrNotDirectory)
	}

	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, fmt.Errorf("open filesystem root: %w", err)
	}
	info, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("inspect filesystem root: %w", err)
	}
	if !info.IsDir() {
		_ = root.Close()
		return nil, fmt.Errorf("filesystem root %q: %w", rootPath, ErrNotDirectory)
	}
	absolute, err := filepath.Abs(rootPath)
	if err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("resolve filesystem root path: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("resolve filesystem root path: %w", err)
	}
	canonicalRoot := filepath.Clean(resolved)
	registeredMounts, err := platformRegisterMounts(canonicalRoot)
	if err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("inspect mounted volumes: %w", err)
	}
	rootReadOnly, err := platformRootReadOnly(canonicalRoot)
	if err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("inspect filesystem root permissions: %w", err)
	}
	return &Root{root: root, rootPath: canonicalRoot, registeredMounts: registeredMounts, rootReadOnly: rootReadOnly}, nil
}

// Close 释放根目录句柄。
func (r *Root) Close() error {
	if r == nil || r.root == nil {
		return nil
	}
	var closeErr error
	if r.moveRecoveryRoot != nil {
		closeErr = errors.Join(closeErr, r.moveRecoveryRoot.Close())
	}
	return errors.Join(closeErr, r.root.Close())
}

// List 返回 rel 的直接子项；空路径表示根目录，越界软链接不会被跟随。
func (r *Root) List(rel string) ([]Entry, error) {
	return r.list(rel, 0)
}

// ListLimited 返回不超过 limit 个直接子项；超限时不返回部分结果。
func (r *Root) ListLimited(rel string, limit int) ([]Entry, error) {
	if limit < 1 {
		return nil, ErrEntryLimit
	}
	return r.list(rel, limit)
}

func (r *Root) list(rel string, limit int) ([]Entry, error) {
	if err := validateRelativePath(rel); err != nil {
		return nil, err
	}
	if hasTrashComponent(rel) {
		return nil, ErrInvalidPath
	}

	resolved, err := r.resolvePath(rel)
	if err != nil {
		return nil, fmt.Errorf("resolve directory %q: %w", rel, err)
	}
	if !resolved.info.IsDir() {
		return nil, fmt.Errorf("list %q: %w", rel, ErrNotDirectory)
	}

	file, err := r.root.OpenFile(rootName(rel), os.O_RDONLY|nonblockingOpenFlag()|directoryOpenFlag(), 0)
	if err != nil {
		return nil, fmt.Errorf("open directory %q: %w", rel, mapRootError(err, resolved.hadLink, resolved.linkPath, resolved.linkTarget))
	}
	defer file.Close()

	openedInfo, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat directory %q: %w", rel, err)
	}
	if !openedInfo.IsDir() {
		return nil, fmt.Errorf("list %q: %w", rel, ErrNotDirectory)
	}
	if !os.SameFile(resolved.info, openedInfo) {
		return nil, fmt.Errorf("list %q: %w", rel, ErrPathChanged)
	}
	if err := r.checkVolumeAvailability(rel); err != nil {
		return nil, err
	}

	readLimit := -1
	if limit > 0 {
		readLimit = limit + 1
	}
	children, err := file.ReadDir(readLimit)
	if err != nil {
		return nil, fmt.Errorf("read directory %q: %w", rel, err)
	}
	if limit > 0 && len(children) > limit {
		return nil, ErrEntryLimit
	}
	sort.Slice(children, func(i, j int) bool { return children[i].Name() < children[j].Name() })

	entries := make([]Entry, 0, len(children))
	seen := make(map[string]struct{}, len(children))
	for _, child := range children {
		name := child.Name()
		if name == TrashDirectoryName || isArchiveStageComponent(name) {
			continue
		}
		childPath := path.Join(rel, name)
		seen[childPath] = struct{}{}
		info, err := r.root.Lstat(childPath)
		if err != nil {
			return nil, fmt.Errorf("inspect entry %q: %w", childPath, err)
		}
		entry := r.entry(name, childPath, info)
		if mount, exists := r.registeredMountAt(childPath); exists {
			if err := r.checkRegisteredMount(mount); errors.Is(err, ErrVolumeUnavailable) {
				markUnavailableVolume(&entry)
			} else if err != nil {
				return nil, fmt.Errorf("inspect mounted volume %q: %w", childPath, err)
			}
		}
		entries = append(entries, entry)
	}
	for _, mount := range r.registeredMounts {
		if relativeParent(mount.path) != rel {
			continue
		}
		if _, exists := seen[mount.path]; exists {
			continue
		}
		if err := r.checkRegisteredMount(mount); errors.Is(err, ErrVolumeUnavailable) {
			entries = append(entries, unavailableVolumeEntry(mount.path))
		} else if err != nil {
			return nil, fmt.Errorf("inspect mounted volume %q: %w", mount.path, err)
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, nil
}

// LstatEntry 返回末级目录项元数据，不跟随末级软链接。
func (r *Root) LstatEntry(rel string) (Entry, error) {
	if err := validateMutationPath(rel); err != nil {
		return Entry{}, err
	}
	if hasTrashComponent(rel) {
		return Entry{}, ErrInvalidPath
	}
	if mount, exact := r.registeredMountAt(rel); exact {
		if err := r.checkRegisteredMount(mount); errors.Is(err, ErrVolumeUnavailable) {
			info, statErr := r.root.Lstat(rel)
			if statErr != nil {
				return unavailableVolumeEntry(rel), nil
			}
			entry := r.entry(path.Base(rel), rel, info)
			markUnavailableVolume(&entry)
			return entry, nil
		} else if err != nil {
			return Entry{}, err
		}
	}
	if err := r.rejectSymlinkParents(rel); err != nil {
		return Entry{}, err
	}
	info, err := r.root.Lstat(rel)
	if err != nil {
		return Entry{}, err
	}
	return r.entry(path.Base(rel), rel, info), nil
}

// LstatTrashEntry 仅供回收站协调器读取隐藏目录项。
func (r *Root) LstatTrashEntry(rel string) (Entry, error) {
	if err := validateMutationPath(rel); err != nil || !hasTrashComponent(rel) {
		return Entry{}, ErrInvalidPath
	}
	if err := r.rejectSymlinkParents(rel); err != nil {
		return Entry{}, err
	}
	info, err := r.root.Lstat(rel)
	if err != nil {
		return Entry{}, err
	}
	return r.entry(path.Base(rel), rel, info), nil
}

// OpenRead 打开根目录下的普通文件；仅允许指向根目录内部的软链接。
func (r *Root) OpenRead(rel string) (io.ReadCloser, Entry, error) {
	if err := validateRelativePath(rel); err != nil {
		return nil, Entry{}, err
	}
	if hasTrashComponent(rel) {
		return nil, Entry{}, ErrInvalidPath
	}
	if err := r.checkVolumeAvailability(rel); err != nil {
		return nil, Entry{}, err
	}
	if rel == "" {
		return nil, Entry{}, fmt.Errorf("open read %q: %w", rel, ErrNotRegularFile)
	}

	linkInfo, err := r.root.Lstat(rel)
	if err != nil {
		return nil, Entry{}, fmt.Errorf("inspect file %q: %w", rel, err)
	}
	resolved, err := r.resolvePath(rel)
	if err != nil {
		return nil, Entry{}, fmt.Errorf("resolve file %q: %w", rel, err)
	}
	if !resolved.info.Mode().IsRegular() {
		return nil, Entry{}, fmt.Errorf("open read %q: %w", rel, ErrNotRegularFile)
	}

	file, err := r.root.OpenFile(rel, os.O_RDONLY|nonblockingOpenFlag(), 0)
	if err != nil {
		return nil, Entry{}, fmt.Errorf("open read %q: %w", rel, mapRootError(err, resolved.hadLink, resolved.linkPath, resolved.linkTarget))
	}
	openedInfo, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, Entry{}, fmt.Errorf("stat opened file %q: %w", rel, err)
	}
	if !openedInfo.Mode().IsRegular() {
		_ = file.Close()
		return nil, Entry{}, fmt.Errorf("open read %q: %w", rel, ErrNotRegularFile)
	}
	if !os.SameFile(resolved.info, openedInfo) {
		_ = file.Close()
		return nil, Entry{}, fmt.Errorf("open read %q: %w", rel, ErrPathChanged)
	}

	entry := entryFromInfo(path.Base(rel), rel, openedInfo)
	r.applyReadOnly(&entry, rel)
	if linkInfo.Mode()&os.ModeSymlink != 0 {
		target, targetErr := r.root.Readlink(rel)
		if targetErr != nil {
			_ = file.Close()
			return nil, Entry{}, fmt.Errorf("read link %q: %w", rel, targetErr)
		}
		entry.Symlink = true
		entry.LinkStatus = LinkStatusInternal
		entry.LinkTarget = target
		setLinkIdentity(&entry, linkInfo)
	}
	return file, entry, nil
}

type pathResolution struct {
	info       fs.FileInfo
	hadLink    bool
	linkPath   string
	linkTarget string
}

// resolvePath 逐段解析软链接，仅用于分类和读取元数据。
func (r *Root) resolvePath(rel string) (pathResolution, error) {
	if err := r.checkVolumeAvailability(rel); err != nil {
		return pathResolution{}, err
	}
	candidate := rel
	seenLinks := make(map[string]struct{})
	hadLink := false
	linkPath := ""
	linkTarget := ""
	requiredTargetComponents := 0

	for range maxSymlinkHops {
		if err := r.checkVolumeAvailability(candidate); err != nil {
			return pathResolution{}, err
		}
		components := splitRelativeComponents(candidate)
		if len(components) == 0 {
			info, err := r.root.Stat(".")
			if err != nil {
				return pathResolution{}, err
			}
			return pathResolution{info: info, hadLink: hadLink, linkPath: linkPath, linkTarget: linkTarget}, nil
		}

		currentComponents := make([]string, 0, len(components))
		restarted := false
		for index, component := range components {
			currentComponents = append(currentComponents, component)
			prefix := strings.Join(currentComponents, "/")
			info, err := r.root.Lstat(prefix)
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) && index < requiredTargetComponents {
					return pathResolution{}, fmt.Errorf("%w: link %q -> %q", ErrDanglingLink, linkPath, linkTarget)
				}
				return pathResolution{}, err
			}

			if info.Mode()&os.ModeSymlink == 0 {
				if index < len(components)-1 && !info.IsDir() {
					return pathResolution{}, fmt.Errorf("path component %q: %w", prefix, ErrNotDirectory)
				}
				if index == len(components)-1 {
					return pathResolution{info: info, hadLink: hadLink, linkPath: linkPath, linkTarget: linkTarget}, nil
				}
				continue
			}

			if _, exists := seenLinks[prefix]; exists {
				return pathResolution{}, fmt.Errorf("%w: %q", ErrLinkCycle, prefix)
			}
			seenLinks[prefix] = struct{}{}
			target, err := r.root.Readlink(prefix)
			if err != nil {
				return pathResolution{}, err
			}
			if !hadLink {
				hadLink = true
				linkPath = prefix
				linkTarget = target
			}

			targetPath, targetErr := normalizeLinkTarget(path.Dir(prefix), target)
			if targetErr != nil {
				return pathResolution{}, fmt.Errorf("%w: link %q -> %q", ErrOutsideRoot, prefix, target)
			}
			targetComponents := splitRelativeComponents(targetPath)
			suffix := strings.Join(components[index+1:], "/")
			candidate = joinRelativePath(targetPath, suffix)
			requiredTargetComponents = len(targetComponents)
			restarted = true
			break
		}
		if !restarted {
			return pathResolution{}, fmt.Errorf("unable to resolve root-relative path %q", rel)
		}
	}
	return pathResolution{}, fmt.Errorf("%w: too many symbolic links in %q", ErrLinkCycle, rel)
}

// registeredMountForPath 返回覆盖路径的最深登记卷。
func registeredMountForPath(mounts []registeredMount, rel string) (registeredMount, bool) {
	var matched registeredMount
	found := false
	for _, mount := range mounts {
		if rel == mount.path || strings.HasPrefix(rel, mount.path+"/") {
			if !found || len(mount.path) > len(matched.path) {
				matched = mount
				found = true
			}
		}
	}
	return matched, found
}

// registeredMountAt 匹配恰好位于已登记卷根的路径。
func (r *Root) registeredMountAt(rel string) (registeredMount, bool) {
	for _, mount := range r.registeredMounts {
		if rel == mount.path {
			return mount, true
		}
	}
	return registeredMount{}, false
}

// checkVolumeAvailability 对登记卷按挂载实例 ID 复核，避免访问到底层空目录。
func (r *Root) checkVolumeAvailability(rel string) error {
	mount, ok := registeredMountForPath(r.registeredMounts, rel)
	if !ok {
		return nil
	}
	return r.checkRegisteredMount(mount)
}

func (r *Root) checkRegisteredMount(mount registeredMount) error {
	currentID, err := platformMountIDAt(r.rootPath, mount.path)
	if err != nil || currentID != mount.id {
		return fmt.Errorf("volume %q: %w", mount.path, ErrVolumeUnavailable)
	}
	return nil
}

// applyReadOnly 标记当前路径所在文件系统是否只读。
func (r *Root) applyReadOnly(entry *Entry, rel string) {
	if mount, ok := registeredMountForPath(r.registeredMounts, rel); ok {
		entry.ReadOnly = mount.readOnly
		return
	}
	entry.ReadOnly = r.rootReadOnly
}

func markUnavailableVolume(entry *Entry) {
	entry.MountUnavailable = true
	entry.ReadOnly = true
}

func unavailableVolumeEntry(rel string) Entry {
	entry := Entry{Name: path.Base(rel), Path: rel, Kind: KindDirectory}
	markUnavailableVolume(&entry)
	return entry
}

func relativeParent(rel string) string {
	parent := path.Dir(rel)
	if parent == "." {
		return ""
	}
	return parent
}

func (r *Root) entry(name, rel string, info fs.FileInfo) Entry {
	entry := entryFromInfo(name, rel, info)
	r.applyReadOnly(&entry, rel)
	if info.Mode()&os.ModeSymlink == 0 {
		return entry
	}

	entry.Symlink = true
	setLinkIdentity(&entry, info)
	if target, err := r.root.Readlink(rel); err == nil {
		entry.LinkTarget = target
	} else {
		entry.LinkStatus = LinkStatusUnavailable
		return entry
	}

	resolved, err := r.resolvePath(rel)
	if err != nil {
		entry.LinkStatus = linkStatusForError(err)
		return entry
	}
	entry = entryFromInfo(name, rel, resolved.info)
	r.applyReadOnly(&entry, rel)
	entry.Symlink = true
	entry.LinkStatus = LinkStatusInternal
	entry.LinkTarget = resolved.linkTarget
	if entry.LinkTarget == "" {
		entry.LinkTarget, _ = r.root.Readlink(rel)
	}
	setLinkIdentity(&entry, info)
	return entry
}

func entryFromInfo(name, rel string, info fs.FileInfo) Entry {
	changedAt, changeTimeAvailable := fileChangeTime(info)
	ownerUID, groupGID, ownershipAvailable := fileOwnership(info)
	entry := Entry{
		Name:                name,
		Path:                rel,
		Kind:                kindFromInfo(info),
		Size:                info.Size(),
		ModifiedAt:          info.ModTime(),
		Mode:                info.Mode(),
		ChangedAt:           changedAt,
		ChangeTimeAvailable: changeTimeAvailable,
		OwnerUID:            ownerUID,
		GroupGID:            groupGID,
		OwnershipAvailable:  ownershipAvailable,
	}
	entry.Device, entry.Inode, entry.Links, entry.IdentityAvailable = fileIdentity(info)
	return entry
}

func setLinkIdentity(entry *Entry, info fs.FileInfo) {
	entry.LinkDevice, entry.LinkInode, entry.LinkLinks, entry.LinkIdentityAvailable = fileIdentity(info)
}

func kindFromInfo(info fs.FileInfo) Kind {
	if info.IsDir() {
		return KindDirectory
	}
	if info.Mode().IsRegular() {
		return KindFile
	}
	return KindOther
}

func validateRelativePath(rel string) error {
	if rel == "" {
		return nil
	}
	if strings.IndexByte(rel, 0) >= 0 || strings.Contains(rel, `\`) || strings.HasPrefix(rel, "/") || filepath.IsAbs(rel) || filepath.VolumeName(rel) != "" || hasDriveVolume(rel) {
		return fmt.Errorf("%q: %w", rel, ErrInvalidPath)
	}
	for _, component := range strings.Split(rel, "/") {
		if component == "" || component == "." || component == ".." {
			return fmt.Errorf("%q: %w", rel, ErrInvalidPath)
		}
		if isArchiveStageComponent(component) {
			return fmt.Errorf("%q: %w", rel, ErrInvalidPath)
		}
	}
	return nil
}

func isArchiveStageComponent(component string) bool {
	return strings.HasPrefix(component, ".filesage-archive-stage-")
}

func hasTrashComponent(rel string) bool {
	for _, component := range strings.Split(rel, "/") {
		if component == TrashDirectoryName {
			return true
		}
	}
	return false
}

func hasDriveVolume(rel string) bool {
	return len(rel) >= 2 && ((rel[0] >= 'a' && rel[0] <= 'z') || (rel[0] >= 'A' && rel[0] <= 'Z')) && rel[1] == ':'
}

func rootName(rel string) string {
	if rel == "" {
		return "."
	}
	return rel
}

func splitRelativeComponents(rel string) []string {
	if rel == "" || rel == "." {
		return nil
	}
	return strings.Split(rel, "/")
}

func joinRelativePath(first, second string) string {
	switch {
	case first == "":
		return second
	case second == "":
		return first
	default:
		return first + "/" + second
	}
}

func normalizeLinkTarget(parent, target string) (string, error) {
	if target == "" || filepath.IsAbs(target) || filepath.VolumeName(target) != "" {
		return "", ErrOutsideRoot
	}
	if runtime.GOOS == "windows" {
		target = filepath.ToSlash(target)
	}
	if strings.HasPrefix(target, "/") || strings.IndexByte(target, 0) >= 0 {
		return "", ErrOutsideRoot
	}

	components := splitRelativeComponents(parent)
	for _, component := range strings.Split(target, "/") {
		switch component {
		case "", ".":
			continue
		case "..":
			if len(components) == 0 {
				return "", ErrOutsideRoot
			}
			components = components[:len(components)-1]
		default:
			components = append(components, component)
		}
	}
	return strings.Join(components, "/"), nil
}

func mapRootError(err error, hadLink bool, linkPath, linkTarget string) error {
	if !hadLink {
		return err
	}
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: link %q -> %q: %w", ErrDanglingLink, linkPath, linkTarget, err)
	}
	if strings.Contains(strings.ToLower(err.Error()), "path escapes from parent") {
		return fmt.Errorf("%w: link %q -> %q: %w", ErrOutsideRoot, linkPath, linkTarget, err)
	}
	return err
}

func linkStatusForError(err error) LinkStatus {
	switch {
	case errors.Is(err, ErrOutsideRoot):
		return LinkStatusOutsideRoot
	case errors.Is(err, ErrDanglingLink), errors.Is(err, fs.ErrNotExist):
		return LinkStatusDangling
	case errors.Is(err, ErrLinkCycle):
		return LinkStatusCycle
	default:
		return LinkStatusUnavailable
	}
}
