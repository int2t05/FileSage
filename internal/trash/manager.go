// Package trash 协调回收站文件移动、恢复、清理与审计。
package trash

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strconv"
	"strings"
	"time"

	"filesage/internal/fsroot"
	"filesage/internal/store"
)

const pageSize = 100

// ErrNeedsAttention 表示回收项需人工核查。
var ErrNeedsAttention = errors.New("trash item needs manual review")

// ErrInvalidCursor 表示分页游标格式错误或快照已过期。
var ErrInvalidCursor = errors.New("invalid trash cursor")

type Root interface {
	List(string) ([]fsroot.Entry, error)
	LstatEntry(string) (fsroot.Entry, error)
	LstatTrashEntry(string) (fsroot.Entry, error)
	MoveToTrash(string, string, uint64) error
	RestoreFromTrash(string, string, uint64) error
	Remove(string) error
	RemoveEntryIfUnchanged(fsroot.Entry) error
	TrashArea(string) (string, string, uint64, error)
	VerifyTrashArea(string, uint64) error
	MountID(string) (uint64, error)
	CheckTreeMounts(string, uint64) error
	RemoveTreeNoFollow(string, uint64, uint64, uint64) error
}

type Metadata interface {
	BeginTrash(context.Context, store.TrashRecord) (int64, error)
	SetTrashDestination(context.Context, int64, string) error
	SetTrashStatus(context.Context, int64, store.TrashStatus, store.TrashStatus, string, string, string, string) error
	Trash(context.Context, int64) (store.TrashRecord, error)
	ListTrash(context.Context, int, int64, int64) (store.TrashPage, error)
	ListTrashForPurge(context.Context, int64, int64, int) ([]store.TrashRecord, error)
	ListPendingTrash(context.Context, int64, int) ([]store.TrashRecord, error)
	SetTrashStatusAtSeq(context.Context, int64, store.TrashStatus, int64, store.TrashStatus, string, string, string, string) error
	AppendAudit(context.Context, store.AuditRecord) (int64, error)
}

type Manager struct {
	root Root
	data Metadata
}

type Item struct {
	ID           string
	OriginalPath string
	Name         string
	Kind         string
	Size         int64
	DeletedAt    time.Time
	Status       string
	Message      string
}

type Page struct {
	Items      []Item
	Limit      int
	HasMore    bool
	Snapshot   int64
	NextCursor string
}

type PurgeError struct {
	ID      string `json:"id"`
	Message string `json:"message"`
}

type PurgeResult struct {
	Purged         int          `json:"purged"`
	NeedsAttention int          `json:"needsAttention"`
	Failed         int          `json:"failed"`
	Snapshot       int64        `json:"snapshot"`
	Errors         []PurgeError `json:"errors"`
}

// Open 创建管理器并在启动时核对中断的文件操作。
func Open(ctx context.Context, root Root, data Metadata) (*Manager, error) {
	if root == nil || data == nil {
		return nil, errors.New("trash root and metadata store are required")
	}
	manager := &Manager{root: root, data: data}
	if err := manager.Recover(ctx); err != nil {
		return nil, err
	}
	return manager, nil
}

// List 返回按状态审计序号约束的降序游标页。
func (m *Manager) List(ctx context.Context, limit int, cursor string) (Page, error) {
	snapshot, beforeID := int64(-1), int64(0)
	if cursor != "" {
		var err error
		snapshot, beforeID, err = decodeCursor(cursor)
		if err != nil {
			return Page{}, err
		}
	}
	page, err := m.data.ListTrash(ctx, limit, snapshot, beforeID)
	if err != nil {
		return Page{}, err
	}
	response := Page{Items: make([]Item, 0, len(page.Items)), Limit: page.Limit, HasMore: page.HasMore, Snapshot: page.Snapshot}
	for _, record := range page.Items {
		response.Items = append(response.Items, publicItem(record))
	}
	if page.HasMore && len(page.Items) != 0 {
		response.NextCursor = encodeCursor(page.Snapshot, page.Items[len(page.Items)-1].ID)
	}
	return response, nil
}

// Get 返回指定回收项的公开摘要。
func (m *Manager) Get(ctx context.Context, id string) (Item, error) {
	record, err := m.record(ctx, id)
	if err != nil {
		return Item{}, err
	}
	return publicItem(record), nil
}

// LockIdentity 返回回收区内已校验条目的当前身份，供调用方协调硬链接别名。
func (m *Manager) LockIdentity(ctx context.Context, id string) (fsroot.Entry, error) {
	record, err := m.record(ctx, id)
	if err != nil {
		return fsroot.Entry{}, err
	}
	if record.Status != store.TrashTrashed {
		return fsroot.Entry{}, fsroot.ErrConflict
	}
	return m.storedItemEntry(record, false)
}

// MoveToTrash 将单个目录项原子移动到同一挂载点的隐藏回收区。
func (m *Manager) MoveToTrash(ctx context.Context, expected fsroot.Entry) (Item, error) {
	return m.moveToTrash(ctx, expected, store.AuditRecord{Actor: "user"}, nil)
}

// MoveToTrashForAgent 保留 Agent 任务关联的回收审计。
func (m *Manager) MoveToTrashForAgent(ctx context.Context, expected fsroot.Entry, taskID, planID, operationID string, verifyBeforeMove func() error) (Item, error) {
	if taskID == "" || planID == "" || operationID == "" {
		return Item{}, errors.New("agent trash audit identifiers are required")
	}
	return m.moveToTrash(ctx, expected, store.AuditRecord{
		Actor: "agent", TaskID: taskID, PlanID: planID, OperationID: operationID,
	}, verifyBeforeMove)
}

func (m *Manager) moveToTrash(ctx context.Context, expected fsroot.Entry, audit store.AuditRecord, verifyBeforeMove func() error) (Item, error) {
	if audit.Actor == "agent" {
		if audit.TaskID == "" || audit.PlanID == "" || audit.OperationID == "" {
			return Item{}, errors.New("agent trash audit identifiers are required")
		}
		if _, ok := m.data.(agentAuditMetadata); !ok {
			return Item{}, errors.New("trash metadata does not support Agent audit links")
		}
	}
	current, err := m.root.LstatEntry(expected.Path)
	if err != nil {
		return Item{}, err
	}
	if !sameEntry(expected, current) {
		return Item{}, fsroot.ErrConflict
	}
	kind := itemKind(current)
	if kind == "" {
		return Item{}, errors.New("special filesystem entries cannot be moved to trash")
	}
	area, mountRoot, mountID, err := m.root.TrashArea(current.Path)
	if err != nil {
		return Item{}, err
	}
	record := recordFromEntry(current, mountRoot)
	record.Actor = audit.Actor
	record.AuditTaskID, record.AuditPlanID, record.AuditOperationID = audit.TaskID, audit.PlanID, audit.OperationID
	id, err := m.data.BeginTrash(ctx, record)
	if err != nil {
		return Item{}, err
	}
	destination := path.Join(area, strconv.FormatInt(id, 10))
	if err := m.setTrashDestination(ctx, id, destination, audit); err != nil {
		m.markMoveAttention(ctx, id, store.TrashMoving, "回收目标登记失败，需核查", audit)
		return Item{}, err
	}
	current, err = m.root.LstatEntry(expected.Path)
	if err != nil || !sameEntry(expected, current) {
		m.markMoveAttention(ctx, id, store.TrashMoving, "移动前文件状态变化，需核查", audit)
		return Item{}, fsroot.ErrConflict
	}
	if current.Kind == fsroot.KindDirectory && !current.Symlink {
		if err := m.root.CheckTreeMounts(current.Path, mountID); err != nil {
			m.markMoveAttention(ctx, id, store.TrashMoving, "目录包含挂载或状态变化，需核查", audit)
			return Item{}, err
		}
	}
	if verifyBeforeMove != nil {
		if err := verifyBeforeMove(); err != nil {
			m.markMoveAttention(ctx, id, store.TrashMoving, "移动前条目状态变化，需核查", audit)
			return Item{}, err
		}
	}
	if err := m.root.MoveToTrash(current.Path, destination, mountID); err != nil {
		m.markMoveAttention(ctx, id, store.TrashMoving, "移动未完成，需核查", audit)
		return Item{}, err
	}
	moved, err := m.root.LstatTrashEntry(destination)
	if err != nil || !matchesRecord(record, moved) {
		m.markMoveAttention(ctx, id, store.TrashMoving, "移动后文件身份不一致，需核查", audit)
		return Item{}, fsroot.ErrConflict
	}
	if err := m.setMoveTrashed(ctx, id, audit); err != nil {
		return Item{}, err
	}
	stored, err := m.data.Trash(ctx, id)
	if err != nil {
		return Item{}, err
	}
	return publicItem(stored), nil
}

// PermanentRemove 永久删除单个文件、软链接或空目录。
func (m *Manager) PermanentRemove(ctx context.Context, expected fsroot.Entry) error {
	current, err := m.root.LstatEntry(expected.Path)
	if err != nil {
		return err
	}
	if !sameEntry(expected, current) {
		return fsroot.ErrConflict
	}
	if itemKind(current) == "" {
		return errors.New("special filesystem entries cannot be permanently deleted")
	}
	mountID, err := m.root.MountID(current.Path)
	if err != nil {
		return err
	}
	if current.Kind == fsroot.KindDirectory && !current.Symlink {
		parentID, err := m.root.MountID(parentPath(current.Path))
		if err != nil {
			return err
		}
		if parentID != mountID {
			return fsroot.ErrMountRoot
		}
		if err := m.root.CheckTreeMounts(current.Path, mountID); err != nil {
			return err
		}
		children, err := m.root.List(current.Path)
		if err != nil {
			return err
		}
		if len(children) != 0 {
			return fsroot.ErrConflict
		}
	}
	if _, err := m.data.AppendAudit(ctx, store.AuditRecord{
		EventType: "permanent_delete_started", Actor: "user", Outcome: "started",
		Message: "已确认永久删除", SourcePath: current.Path,
	}); err != nil {
		return err
	}
	if err := m.root.RemoveEntryIfUnchanged(current); err != nil {
		if errors.Is(err, fsroot.ErrPublishedNotDurable) {
			_, auditErr := m.data.AppendAudit(ctx, store.AuditRecord{
				EventType: "permanent_delete_completed", Actor: "user", Outcome: "needs_review",
				Message: "永久删除已执行，但目录持久化未确认", SourcePath: current.Path,
			})
			return errors.Join(err, auditErr)
		}
		message := "永久删除未完成"
		var recovery *fsroot.EntryRemovalRecoveryError
		if errors.As(err, &recovery) {
			message = fmt.Sprintf("永久删除未完成，需核查路径：%s", recovery.Path)
		}
		_, _ = m.data.AppendAudit(ctx, store.AuditRecord{EventType: "permanent_delete_failed", Actor: "user", Outcome: "failed", Message: message, SourcePath: current.Path})
		return err
	}
	if _, err := m.data.AppendAudit(ctx, store.AuditRecord{EventType: "permanent_delete_completed", Actor: "user", Outcome: "succeeded", Message: "永久删除已完成", SourcePath: current.Path}); err != nil {
		return fmt.Errorf("file was removed but audit completion could not be saved: %w", err)
	}
	return nil
}

// Restore 恢复单项；目标重名时采用编号名称，不覆盖现有内容。
func (m *Manager) Restore(ctx context.Context, id string) (string, error) {
	record, err := m.record(ctx, id)
	if err != nil {
		return "", err
	}
	if record.Status != store.TrashTrashed {
		return "", ErrNeedsAttention
	}
	if err := m.verifyStoredItem(record); err != nil {
		m.markAttention(ctx, record.ID, store.TrashTrashed, "回收项身份或挂载状态变化，需核查")
		return "", ErrNeedsAttention
	}
	target := record.OriginalPath
	for suffix := 1; suffix <= 10000; suffix++ {
		if suffix > 1 {
			var nameErr error
			target, nameErr = numberedPath(record.OriginalPath, suffix)
			if nameErr != nil {
				return "", nameErr
			}
		}
		if err := m.data.SetTrashStatus(ctx, record.ID, store.TrashTrashed, store.TrashRestoring, target, "正在恢复回收项", "user", "trash_restore_started"); err != nil {
			return "", err
		}
		mountID, mountErr := m.root.MountID(record.TrashPath)
		if mountErr != nil {
			_ = m.data.SetTrashStatus(ctx, record.ID, store.TrashRestoring, store.TrashTrashed, "", "挂载状态变化，需核查", "system", "trash_restore_failed")
			return "", mountErr
		}
		if err := m.root.RestoreFromTrash(record.TrashPath, target, mountID); err != nil {
			if errors.Is(err, fsroot.ErrConflict) {
				_ = m.data.SetTrashStatus(ctx, record.ID, store.TrashRestoring, store.TrashTrashed, "", "恢复目标已占用，尝试编号名称", "system", "trash_restore_conflict")
				continue
			}
			_ = m.data.SetTrashStatus(ctx, record.ID, store.TrashRestoring, store.TrashTrashed, "", "恢复未完成，回收项仍保留", "system", "trash_restore_failed")
			return "", err
		}
		restored, err := m.root.LstatEntry(target)
		if err != nil || !matchesRecord(record, restored) {
			m.markAttention(ctx, record.ID, store.TrashRestoring, "恢复后身份不一致，需核查")
			return "", fsroot.ErrConflict
		}
		if err := m.data.SetTrashStatus(ctx, record.ID, store.TrashRestoring, store.TrashRestored, target, "回收项已恢复", "user", "trash_restored"); err != nil {
			return "", err
		}
		return target, nil
	}
	return "", fsroot.ErrConflict
}

// Purge 永久清理单个状态正常的回收项。
func (m *Manager) Purge(ctx context.Context, id string) error {
	record, err := m.record(ctx, id)
	if err != nil {
		return err
	}
	if record.Status != store.TrashTrashed {
		return ErrNeedsAttention
	}
	return m.purgeRecord(ctx, record, record.StateSeq)
}

// PurgeForAgent 仅清理由同一已审批操作刚移入回收站的项目。
func (m *Manager) PurgeForAgent(ctx context.Context, id, taskID, planID, operationID string) error {
	if taskID == "" || planID == "" || operationID == "" {
		return errors.New("agent trash audit identifiers are required")
	}
	record, err := m.record(ctx, id)
	if err != nil {
		return err
	}
	if record.Status != store.TrashTrashed {
		return ErrNeedsAttention
	}
	metadata, ok := m.data.(agentPurgeMetadata)
	if !ok {
		return errors.New("trash metadata does not support Agent purge audit links")
	}
	if !agentOwnsTrashItem(ctx, metadata, record, taskID, planID, operationID) {
		return ErrNeedsAttention
	}
	return m.purgeRecordForAgent(ctx, record, metadata, taskID, planID, operationID)
}

// PurgeAtSnapshot 仅清理状态仍属于指定快照的回收项。
func (m *Manager) PurgeAtSnapshot(ctx context.Context, id string, snapshot int64) error {
	if snapshot < 0 {
		return ErrInvalidCursor
	}
	if _, err := m.data.ListTrashForPurge(ctx, snapshot, 0, 1); err != nil {
		return err
	}
	record, err := m.record(ctx, id)
	if err != nil {
		return err
	}
	if record.Status == store.TrashNeedsAttention {
		return ErrNeedsAttention
	}
	if record.Status != store.TrashTrashed || record.StateSeq > snapshot {
		return store.ErrConflict
	}
	return m.purgeRecord(ctx, record, record.StateSeq)
}

// PurgeSnapshot 仅清理指定快照内正常状态的记录。
func (m *Manager) PurgeSnapshot(ctx context.Context, snapshot int64) (PurgeResult, error) {
	if snapshot < 0 {
		return PurgeResult{}, errors.New("invalid trash snapshot")
	}
	result := PurgeResult{Snapshot: snapshot, Errors: make([]PurgeError, 0)}
	var after int64
	for {
		items, err := m.data.ListTrashForPurge(ctx, snapshot, after, pageSize)
		if err != nil {
			return result, err
		}
		if len(items) == 0 {
			return result, nil
		}
		for _, record := range items {
			after = record.ID
			if record.Status == store.TrashNeedsAttention {
				result.NeedsAttention++
				continue
			}
			if record.StateSeq > snapshot {
				continue
			}
			if err := m.purgeRecord(ctx, record, record.StateSeq); err != nil {
				result.Failed++
				result.NeedsAttention++
				result.Errors = append(result.Errors, PurgeError{ID: strconv.FormatInt(record.ID, 10), Message: "清理未完成，已保留并标记待核查"})
				continue
			}
			result.Purged++
		}
	}
}

// PurgeCandidates 返回清空快照内下一批记录，供调用方逐路径加锁。
func (m *Manager) PurgeCandidates(ctx context.Context, snapshot, after int64, limit int) ([]Item, error) {
	if limit < 1 || limit > pageSize {
		return nil, errors.New("invalid trash purge page")
	}
	records, err := m.data.ListTrashForPurge(ctx, snapshot, after, limit)
	if err != nil {
		return nil, err
	}
	items := make([]Item, 0, len(records))
	for _, record := range records {
		items = append(items, publicItem(record))
	}
	return items, nil
}

func (m *Manager) purgeRecord(ctx context.Context, record store.TrashRecord, expectedSeq int64) error {
	if record.Status != store.TrashTrashed {
		return ErrNeedsAttention
	}
	if err := m.verifyStoredItem(record); err != nil {
		_ = m.data.SetTrashStatusAtSeq(ctx, record.ID, store.TrashTrashed, expectedSeq, store.TrashNeedsAttention, "", "回收项身份或挂载状态变化，需核查", "system", "trash_attention")
		return ErrNeedsAttention
	}
	if err := m.data.SetTrashStatusAtSeq(ctx, record.ID, store.TrashTrashed, expectedSeq, store.TrashPurging, "", "正在永久清理回收项", "user", "trash_purge_started"); err != nil {
		return err
	}
	mountID, err := m.root.MountID(record.TrashPath)
	if err == nil {
		device, inode, ok := storedTrashIdentity(record)
		if !ok {
			err = fsroot.ErrIdentityUnavailable
		} else {
			err = m.root.RemoveTreeNoFollow(record.TrashPath, mountID, device, inode)
		}
	}
	if err != nil {
		_ = m.data.SetTrashStatus(ctx, record.ID, store.TrashPurging, store.TrashNeedsAttention, "", "清理未完成，需核查", "system", "trash_purge_failed")
		return err
	}
	return m.data.SetTrashStatus(ctx, record.ID, store.TrashPurging, store.TrashPurged, "", "永久清理已完成", "user", "trash_purged")
}

// purgeRecordForAgent 按回收记录序号清理，并将阶段审计关联到已批准操作。
func (m *Manager) purgeRecordForAgent(ctx context.Context, record store.TrashRecord, metadata agentPurgeMetadata, taskID, planID, operationID string) error {
	if record.Status != store.TrashTrashed {
		return ErrNeedsAttention
	}
	if err := m.verifyStoredItem(record); err != nil {
		_ = metadata.SetTrashStatusForAgentAtSeq(ctx, record.ID, store.TrashTrashed, record.StateSeq, store.TrashNeedsAttention, "", "回收项身份或挂载状态变化，需核查", taskID, planID, operationID, "trash_attention")
		return ErrNeedsAttention
	}
	if err := metadata.SetTrashStatusForAgentAtSeq(ctx, record.ID, store.TrashTrashed, record.StateSeq, store.TrashPurging, "", "正在永久删除已审批项目", taskID, planID, operationID, "trash_purge_started"); err != nil {
		return err
	}
	mountID, err := m.root.MountID(record.TrashPath)
	if err == nil {
		device, inode, ok := storedTrashIdentity(record)
		if !ok {
			err = fsroot.ErrIdentityUnavailable
		} else {
			err = m.root.RemoveTreeNoFollow(record.TrashPath, mountID, device, inode)
		}
	}
	if err != nil {
		_ = metadata.SetTrashStatusForAgent(ctx, record.ID, store.TrashPurging, store.TrashNeedsAttention, "", "永久删除未完成，需人工核查回收项", taskID, planID, operationID, "trash_purge_failed")
		return err
	}
	return metadata.SetTrashStatusForAgent(ctx, record.ID, store.TrashPurging, store.TrashPurged, "", "永久删除已完成", taskID, planID, operationID, "trash_purged")
}

func storedTrashIdentity(record store.TrashRecord) (device, inode uint64, ok bool) {
	if record.Symlink {
		if !record.LinkIdentityAvailable || record.LinkDevice == nil || record.LinkInode == nil {
			return 0, 0, false
		}
		return *record.LinkDevice, *record.LinkInode, true
	}
	if !record.IdentityAvailable || record.Device == nil || record.Inode == nil {
		return 0, 0, false
	}
	return *record.Device, *record.Inode, true
}

func encodeCursor(snapshot, beforeID int64) string {
	var value [16]byte
	binary.BigEndian.PutUint64(value[:8], uint64(snapshot))
	binary.BigEndian.PutUint64(value[8:], uint64(beforeID))
	return base64.RawURLEncoding.EncodeToString(value[:])
}

func decodeCursor(cursor string) (int64, int64, error) {
	if len(cursor) > 64 {
		return 0, 0, ErrInvalidCursor
	}
	value, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || len(value) != 16 {
		return 0, 0, ErrInvalidCursor
	}
	snapshot := binary.BigEndian.Uint64(value[:8])
	beforeID := binary.BigEndian.Uint64(value[8:])
	if snapshot > uint64(^uint64(0)>>1) || beforeID == 0 || beforeID > uint64(^uint64(0)>>1) {
		return 0, 0, ErrInvalidCursor
	}
	return int64(snapshot), int64(beforeID), nil
}

// Recover 核对崩溃前未完成的文件移动阶段。
func (m *Manager) Recover(ctx context.Context) error {
	var after int64
	for {
		items, err := m.data.ListPendingTrash(ctx, after, pageSize)
		if err != nil {
			return err
		}
		if len(items) == 0 {
			return nil
		}
		for _, record := range items {
			after = record.ID
			if err := m.recoverRecord(ctx, record); err != nil {
				return err
			}
		}
	}
}

func (m *Manager) recoverRecord(ctx context.Context, record store.TrashRecord) error {
	if record.TrashPath == "pending" || !validStoredPath(record.TrashPath) {
		return m.data.SetTrashStatus(ctx, record.ID, record.Status, store.TrashNeedsAttention, "", "操作目标不完整，需核查", "system", "trash_recovery_attention")
	}
	_, originalErr := m.root.LstatEntry(record.OriginalPath)
	trashed, trashErr := m.root.LstatTrashEntry(record.TrashPath)
	trashOK := trashErr == nil && matchesRecord(record, trashed)
	originalMissing := errors.Is(originalErr, fs.ErrNotExist)
	trashMissing := errors.Is(trashErr, fs.ErrNotExist)
	switch record.Status {
	case store.TrashMoving:
		if trashOK && originalMissing {
			return m.data.SetTrashStatus(ctx, record.ID, store.TrashMoving, store.TrashTrashed, "", "启动核对确认回收移动已完成", "system", "trash_recovered")
		}
	case store.TrashRestoring:
		if record.RestorePath != "" {
			restored, err := m.root.LstatEntry(record.RestorePath)
			if err == nil && matchesRecord(record, restored) && trashMissing {
				return m.data.SetTrashStatus(ctx, record.ID, store.TrashRestoring, store.TrashRestored, record.RestorePath, "启动核对确认恢复已完成", "system", "trash_recovered")
			}
		}
		if trashOK && record.RestorePath != "" {
			if _, err := m.root.LstatEntry(record.RestorePath); errors.Is(err, fs.ErrNotExist) {
				return m.data.SetTrashStatus(ctx, record.ID, store.TrashRestoring, store.TrashTrashed, "", "启动核对确认回收项仍在回收区", "system", "trash_recovered")
			}
		}
	case store.TrashPurging:
		if trashMissing {
			return m.data.SetTrashStatus(ctx, record.ID, store.TrashPurging, store.TrashPurged, "", "启动核对确认清理已完成", "system", "trash_recovered")
		}
	}
	return m.data.SetTrashStatus(ctx, record.ID, record.Status, store.TrashNeedsAttention, "", "启动核对发现状态不明确，需人工检查", "system", "trash_recovery_attention")
}

func (m *Manager) verifyStoredItem(record store.TrashRecord) error {
	_, err := m.storedItemEntry(record, true)
	return err
}

func (m *Manager) storedItemEntry(record store.TrashRecord, checkTree bool) (fsroot.Entry, error) {
	if !validStoredPath(record.TrashPath) || record.TrashPath != trashPath(record.MountRoot, record.ID) {
		return fsroot.Entry{}, fsroot.ErrInvalidPath
	}
	area := path.Dir(record.TrashPath)
	mountID, err := m.root.MountID(record.TrashPath)
	if err != nil {
		return fsroot.Entry{}, err
	}
	if err := m.root.VerifyTrashArea(area, mountID); err != nil {
		return fsroot.Entry{}, err
	}
	entry, err := m.root.LstatTrashEntry(record.TrashPath)
	if err != nil {
		return fsroot.Entry{}, err
	}
	if !matchesRecord(record, entry) {
		return fsroot.Entry{}, fsroot.ErrConflict
	}
	if checkTree && record.Kind == "directory" {
		if err := m.root.CheckTreeMounts(record.TrashPath, mountID); err != nil {
			return fsroot.Entry{}, err
		}
	}
	return entry, nil
}

func (m *Manager) markAttention(ctx context.Context, id int64, expected store.TrashStatus, message string) {
	_ = m.data.SetTrashStatus(ctx, id, expected, store.TrashNeedsAttention, "", message, "system", "trash_attention")
}

type agentAuditMetadata interface {
	SetTrashDestinationForAgent(context.Context, int64, string, string, string, string) error
	SetTrashStatusForAgent(context.Context, int64, store.TrashStatus, store.TrashStatus, string, string, string, string, string, string) error
}

type agentPurgeMetadata interface {
	agentAuditMetadata
	SetTrashStatusForAgentAtSeq(context.Context, int64, store.TrashStatus, int64, store.TrashStatus, string, string, string, string, string, string) error
	AuditForTask(context.Context, string) ([]store.AuditRecord, error)
}

// agentOwnsTrashItem 用不可变移动审计确认回收项属于当前操作。
func agentOwnsTrashItem(ctx context.Context, metadata agentPurgeMetadata, record store.TrashRecord, taskID, planID, operationID string) bool {
	events, err := metadata.AuditForTask(ctx, taskID)
	if err != nil {
		return false
	}
	for _, event := range events {
		if event.Actor == "agent" && event.PlanID == planID && event.OperationID == operationID && event.EventType == "trash_trashed" &&
			event.SourcePath == record.OriginalPath && event.TargetPath == record.TrashPath {
			return true
		}
	}
	return false
}

func (m *Manager) setTrashDestination(ctx context.Context, id int64, destination string, audit store.AuditRecord) error {
	if audit.Actor != "agent" {
		return m.data.SetTrashDestination(ctx, id, destination)
	}
	metadata, ok := m.data.(agentAuditMetadata)
	if !ok {
		return errors.New("trash metadata does not support Agent audit links")
	}
	return metadata.SetTrashDestinationForAgent(ctx, id, destination, audit.TaskID, audit.PlanID, audit.OperationID)
}

func (m *Manager) setMoveTrashed(ctx context.Context, id int64, audit store.AuditRecord) error {
	if audit.Actor != "agent" {
		return m.data.SetTrashStatus(ctx, id, store.TrashMoving, store.TrashTrashed, "", "", audit.Actor, "trash_trashed")
	}
	metadata, ok := m.data.(agentAuditMetadata)
	if !ok {
		return errors.New("trash metadata does not support Agent audit links")
	}
	return metadata.SetTrashStatusForAgent(ctx, id, store.TrashMoving, store.TrashTrashed, "", "", audit.TaskID, audit.PlanID, audit.OperationID, "trash_trashed")
}

func (m *Manager) markMoveAttention(ctx context.Context, id int64, expected store.TrashStatus, message string, audit store.AuditRecord) {
	if audit.Actor != "agent" {
		m.markAttention(ctx, id, expected, message)
		return
	}
	metadata, ok := m.data.(agentAuditMetadata)
	if !ok {
		return
	}
	_ = metadata.SetTrashStatusForAgent(ctx, id, expected, store.TrashNeedsAttention, "", message, audit.TaskID, audit.PlanID, audit.OperationID, "trash_attention")
}

func (m *Manager) record(ctx context.Context, value string) (store.TrashRecord, error) {
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id < 1 || strconv.FormatInt(id, 10) != value {
		return store.TrashRecord{}, store.ErrNotFound
	}
	return m.data.Trash(ctx, id)
}

func publicItem(record store.TrashRecord) Item {
	name := path.Base(record.OriginalPath)
	status := string(record.Status)
	if record.Status != store.TrashNeedsAttention {
		status = string(store.TrashTrashed)
	}
	kind := record.Kind
	return Item{ID: strconv.FormatInt(record.ID, 10), OriginalPath: record.OriginalPath, Name: name, Kind: kind, Size: record.SizeBytes, DeletedAt: record.DeletedAt, Status: status, Message: record.Message}
}

func recordFromEntry(entry fsroot.Entry, mountRoot string) store.TrashRecord {
	record := store.TrashRecord{
		OriginalPath: entry.Path, MountRoot: mountRoot, Kind: itemKind(entry),
		SizeBytes: max(entry.Size, 0), ModifiedAt: entry.ModifiedAt, Mode: uint32(entry.Mode.Perm()),
		IdentityAvailable: entry.IdentityAvailable, Symlink: entry.Symlink,
		LinkIdentityAvailable: entry.LinkIdentityAvailable,
	}
	if entry.IdentityAvailable {
		record.Device, record.Inode, record.LinkCount = uintPtr(entry.Device), uintPtr(entry.Inode), uintPtr(entry.Links)
	}
	if entry.LinkIdentityAvailable {
		record.LinkDevice, record.LinkInode, record.LinkCountForLink = uintPtr(entry.LinkDevice), uintPtr(entry.LinkInode), uintPtr(entry.LinkLinks)
	}
	return record
}

func sameEntry(first, second fsroot.Entry) bool {
	if first.Path != second.Path || first.Symlink != second.Symlink || first.Kind != second.Kind || first.Mode.Perm() != second.Mode.Perm() || first.Size != second.Size || !first.ModifiedAt.Equal(second.ModifiedAt) {
		return false
	}
	if first.Symlink {
		return first.LinkIdentityAvailable && second.LinkIdentityAvailable && first.LinkDevice == second.LinkDevice && first.LinkInode == second.LinkInode && first.LinkLinks == second.LinkLinks && first.LinkTarget == second.LinkTarget
	}
	return first.IdentityAvailable && second.IdentityAvailable && first.Device == second.Device && first.Inode == second.Inode && first.Links == second.Links
}

func matchesRecord(record store.TrashRecord, entry fsroot.Entry) bool {
	if entry.Symlink != record.Symlink {
		return false
	}
	if record.Symlink {
		return record.LinkIdentityAvailable && entry.LinkIdentityAvailable && record.LinkDevice != nil && record.LinkInode != nil && record.LinkCountForLink != nil && *record.LinkDevice == entry.LinkDevice && *record.LinkInode == entry.LinkInode && *record.LinkCountForLink == entry.LinkLinks
	}
	return record.IdentityAvailable && entry.IdentityAvailable && record.Device != nil && record.Inode != nil && record.LinkCount != nil && *record.Device == entry.Device && *record.Inode == entry.Inode && *record.LinkCount == entry.Links && record.SizeBytes == entry.Size && record.Mode == uint32(entry.Mode.Perm()) && record.ModifiedAt.Equal(entry.ModifiedAt)
}

func itemKind(entry fsroot.Entry) string {
	if entry.Symlink {
		return "symlink"
	}
	switch entry.Kind {
	case fsroot.KindFile:
		return "file"
	case fsroot.KindDirectory:
		return "directory"
	default:
		return ""
	}
}

func trashPath(mountRoot string, id int64) string {
	return path.Join(mountRoot, fsroot.TrashDirectoryName, strconv.FormatInt(id, 10))
}

func validStoredPath(value string) bool {
	if value == "" || strings.HasPrefix(value, "/") || strings.Contains(value, `\`) {
		return false
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || component == "." || component == ".." {
			return false
		}
	}
	return true
}

func numberedPath(original string, suffix int) (string, error) {
	parent, name := path.Dir(original), path.Base(original)
	extension := path.Ext(name)
	if strings.HasPrefix(name, ".") && strings.Count(name, ".") == 1 {
		extension = ""
	}
	base := strings.TrimSuffix(name, extension)
	newName := fmt.Sprintf("%s (%d)%s", base, suffix, extension)
	if len(newName) > 255 {
		return "", errors.New("cannot create a safe numbered restore name")
	}
	if parent == "." {
		return newName, nil
	}
	return path.Join(parent, newName), nil
}

func parentPath(value string) string {
	parent := path.Dir(value)
	if parent == "." {
		return ""
	}
	return parent
}

func uintPtr(value uint64) *uint64 { return &value }
