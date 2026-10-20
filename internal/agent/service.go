// Package agent 保存并执行经过人工批准的文件建议。
package agent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"filesage/internal/coordination"
	"filesage/internal/fsroot"
	"filesage/internal/store"
	trashsvc "filesage/internal/trash"
)

var (
	ErrInvalid     = errors.New("invalid agent proposal")
	ErrConflict    = errors.New("agent plan state conflict")
	ErrUnsupported = errors.New("agent action is not supported")
)

const (
	agentArchiveMaxEntries   = 50000
	agentArchiveMaxBytes     = 20 << 30
	agentArchiveMaxPathBytes = 4 << 20
)

// agentArchiveSnapshotLimits 使预检、方案和执行复核采用相同上限。
func agentArchiveSnapshotLimits() fsroot.TreeSnapshotLimits {
	return fsroot.TreeSnapshotLimits{
		MaxEntries: agentArchiveMaxEntries, MaxBytes: agentArchiveMaxBytes, MaxPathBytes: agentArchiveMaxPathBytes,
	}
}

type Proposal struct {
	RequestText                   string    `json:"requestText"`
	ScopePath                     string    `json:"scopePath"`
	Action                        string    `json:"action"`
	SourcePath                    string    `json:"sourcePath"`
	TargetPath                    string    `json:"targetPath"`
	Reason                        string    `json:"reason"`
	Evidence                      string    `json:"evidence"`
	Mode                          *uint32   `json:"mode"`
	OwnerUID                      *uint32   `json:"ownerUid"`
	GroupGID                      *uint32   `json:"groupGid"`
	ConversationID                string    `json:"-"`
	ConversationReply             string    `json:"-"`
	ModelProvider                 string    `json:"-"`
	ModelName                     string    `json:"-"`
	ArchiveRootPath               string    `json:"-"`
	PhotoCapturedAt               time.Time `json:"-"`
	PhotoIndexGeneration          int64     `json:"-"`
	ExpectedSourceStateJSON       string    `json:"-"`
	ExpectedTargetParentStateJSON string    `json:"-"`
	DuplicateScanID               string    `json:"-"`
	DuplicateGroupID              int64     `json:"-"`
	ProposedContent               []byte    `json:"-"`
	Classification                bool      `json:"-"`
}

type directoryCopyState struct {
	RootPath string `json:"rootPath"`
	Digest   string `json:"digest"`
}

type archiveSourceState struct {
	Fingerprint    fsroot.Fingerprint `json:"fingerprint"`
	TreeDigest     string             `json:"treeDigest,omitempty"`
	ManifestDigest string             `json:"manifestDigest,omitempty"`
}

type archiveTargetState struct {
	ParentPath     string             `json:"parentPath"`
	Parent         fsroot.Fingerprint `json:"parent"`
	ManifestDigest string             `json:"manifestDigest,omitempty"`
	PlannedPaths   []string           `json:"plannedPaths"`
}

type duplicateCleanupState struct {
	ScanID     string             `json:"scanId"`
	GroupID    int64              `json:"groupId"`
	SourcePath string             `json:"sourcePath"`
	KeeperPath string             `json:"keeperPath"`
	SHA256     string             `json:"sha256"`
	Source     fsroot.Fingerprint `json:"source"`
	Keeper     fsroot.Fingerprint `json:"keeper"`
}

type trashOperationState struct {
	Fingerprint           fsroot.Fingerprint `json:"fingerprint"`
	TreeDigest            string             `json:"treeDigest,omitempty"`
	Symlink               bool               `json:"symlink,omitempty"`
	LinkDevice            uint64             `json:"linkDevice,omitempty"`
	LinkInode             uint64             `json:"linkInode,omitempty"`
	LinkLinks             uint64             `json:"linkLinks,omitempty"`
	LinkIdentityAvailable bool               `json:"linkIdentityAvailable,omitempty"`
	LinkTarget            string             `json:"linkTarget,omitempty"`
}

type photoArchiveTargetState struct {
	ArchiveRootPath string             `json:"archiveRootPath"`
	CapturedDate    string             `json:"capturedDate"`
	Generation      int64              `json:"generation"`
	Root            fsroot.Fingerprint `json:"root"`
	YearPath        string             `json:"yearPath"`
	Year            fsroot.Fingerprint `json:"year"`
	MonthPath       string             `json:"monthPath"`
	Month           fsroot.Fingerprint `json:"month"`
}

type Service struct {
	store         *store.Store
	root          *fsroot.Root
	locks         *coordination.Locker
	trash         *trashsvc.Manager
	textPayloads  *textPayloadStore
	shellPayloads *shellPayloadStore
}

// New 创建共享文件协调锁的 Agent 服务。
func New(metadata *store.Store, root *fsroot.Root, locks *coordination.Locker, trashManagers ...*trashsvc.Manager) *Service {
	if locks == nil {
		locks = coordination.New()
	}
	var trashManager *trashsvc.Manager
	if len(trashManagers) > 0 {
		trashManager = trashManagers[0]
	}
	return &Service{store: metadata, root: root, locks: locks, trash: trashManager}
}

// NewWithTextStaging 启用 Agent 文本候选的私有暂存。
func NewWithTextStaging(metadata *store.Store, root *fsroot.Root, locks *coordination.Locker, stagingDir string, trashManagers ...*trashsvc.Manager) (*Service, error) {
	service := New(metadata, root, locks, trashManagers...)
	staging, err := openTextPayloadStore(stagingDir)
	if err != nil {
		return nil, err
	}
	service.textPayloads = staging
	return service, nil
}

// NewWithStaging 同时启用文本候选与 Shell 候选的私有暂存。
func NewWithStaging(metadata *store.Store, root *fsroot.Root, locks *coordination.Locker, textDir, shellDir string, trashManagers ...*trashsvc.Manager) (*Service, error) {
	service, err := NewWithTextStaging(metadata, root, locks, textDir, trashManagers...)
	if err != nil {
		return nil, err
	}
	shellPayloads, err := openShellPayloadStore(shellDir)
	if err != nil {
		return nil, err
	}
	service.shellPayloads = shellPayloads
	return service, nil
}

// Propose 校验并保存对话或调用方提交的类型化建议。
func (s *Service) Propose(ctx context.Context, proposal Proposal) (store.PlanRecord, error) {
	return s.ProposeMany(ctx, []Proposal{proposal})
}

// ProposeMany 在全组快照校验通过后原子保存有序方案。
func (s *Service) ProposeMany(ctx context.Context, proposals []Proposal) (store.PlanRecord, error) {
	operations, release, err := s.prepareOperations(ctx, proposals)
	if err != nil {
		return store.PlanRecord{}, err
	}
	defer release()
	stagedID := ""
	if isTextFileWriteAction(proposals[0].Action) {
		operation := &operations[0]
		digest, size, stageErr := s.textPayloads.stage(operation.ID, proposals[0].ProposedContent)
		if stageErr != nil {
			return store.PlanRecord{}, stageErr
		}
		operation.ProposedContentSHA256, operation.ProposedContentSize = digest, size
		stagedID = operation.ID
	}
	stageCommitted := false
	defer func() {
		if stagedID != "" && !stageCommitted {
			_ = s.textPayloads.remove(stagedID)
		}
	}()
	first := proposals[0]
	now := operations[0].CreatedAt
	taskID, err := randomID()
	if err != nil {
		return store.PlanRecord{}, err
	}
	planID, err := randomID()
	if err != nil {
		return store.PlanRecord{}, err
	}
	task := store.TaskRecord{
		ID: taskID, Kind: "agent", Status: store.TaskAwaitingApproval,
		RequestText: first.RequestText, ScopePath: first.ScopePath,
		ModelProvider: first.ModelProvider, ModelName: first.ModelName,
		CreatedAt: now, UpdatedAt: now,
	}
	plan := store.PlanRecord{
		ID: planID, TaskID: taskID, Version: 1, Status: store.PlanAwaitingApproval,
		ScopePath: first.ScopePath, ModelProvider: first.ModelProvider, ModelName: first.ModelName,
		DataScopeJSON: `{"proposalSource":"caller"}`,
		CreatedAt:     now, UpdatedAt: now,
		Operations: operations,
	}
	audit := store.AuditRecord{
		TaskID: taskID, PlanID: planID, OperationID: operations[0].ID,
		EventType: "plan_created", Actor: "user", Outcome: "awaiting_approval",
		Message: "调用方提交的操作建议已保存，尚未执行", SourcePath: operations[0].SourcePath,
		TargetPath: operations[0].TargetPath, CreatedAt: now,
	}
	if first.ConversationID != "" {
		plan.DataScopeJSON = `{"proposalSource":"conversation"}`
		allClassified := true
		modelAnalyzed := false
		for _, proposal := range proposals {
			if proposal.Action == "rename" || isTextFileWriteAction(proposal.Action) {
				modelAnalyzed = true
			}
			allClassified = allClassified && proposal.Classification
		}
		if hasClassificationProposal(proposals) && !allClassified {
			return store.PlanRecord{}, ErrUnsupported
		}
		if modelAnalyzed && len(proposals) > 1 {
			return store.PlanRecord{}, ErrUnsupported
		}
		if modelAnalyzed || allClassified {
			plan.DataScopeJSON = `{"proposalSource":"model-analysis"}`
		}
		audit.Actor = "agent"
		audit.Message = "对话确认后的文件操作建议已保存，尚未执行"
		reply := first.ConversationReply
		if reply == "" {
			reply = "已生成待审批方案。"
		}
		if err := s.store.CreateConversationTaskPlan(ctx, first.ConversationID, task, plan, audit, reply); err != nil {
			return store.PlanRecord{}, err
		}
	} else if err := s.store.CreateAgentTaskPlan(ctx, task, plan, audit); err != nil {
		return store.PlanRecord{}, err
	}
	stageCommitted = true
	return s.store.Plan(ctx, planID)
}

// prepareOperations 校验操作组并绑定当前文件快照。
func (s *Service) prepareOperations(ctx context.Context, proposals []Proposal) (_ []store.OperationRecord, release func(), retErr error) {
	if s.store == nil || s.root == nil || s.locks == nil {
		return nil, nil, errors.New("agent service is not configured")
	}
	if len(proposals) < 1 || len(proposals) > maxProposalOperations(proposals) {
		return nil, nil, ErrInvalid
	}
	first := proposals[0]
	analysisOperations := make([]store.AnalysisOperation, 0, len(proposals))
	keys := make([]string, 0, len(proposals)*4)
	metadataEntries := make(map[int]fsroot.Entry)
	lockedSourceEntries := make(map[int]fsroot.Entry)
	lockedKeeperEntries := make(map[int]fsroot.Entry)
	for i, proposal := range proposals {
		if proposal.RequestText != first.RequestText || proposal.ScopePath != first.ScopePath || proposal.ConversationID != first.ConversationID ||
			proposal.ModelProvider != first.ModelProvider || proposal.ModelName != first.ModelName || proposal.Classification != first.Classification {
			return nil, nil, ErrInvalid
		}
		if proposal.Action == "trash_duplicate" && (proposal.DuplicateScanID != first.DuplicateScanID || proposal.DuplicateGroupID != first.DuplicateGroupID || proposal.TargetPath != first.TargetPath) {
			return nil, nil, ErrInvalid
		}
		if err := validateProposal(proposal); err != nil {
			return nil, nil, err
		}
		if len(proposals) > 1 && (proposal.Action == "rename" || isTextFileWriteAction(proposal.Action)) {
			return nil, nil, ErrUnsupported
		}
		analysisOperations = append(analysisOperations, proposalToAnalysisOperation(proposal))
		if proposal.Action == "trash" || proposal.Action == "permanent_delete" {
			keys = append(keys, coordination.PathKeys(proposal.SourcePath)...)
		} else {
			keys = append(keys, coordination.PathKeys(proposal.SourcePath, proposal.TargetPath)...)
		}
		if proposal.Action == "archive_photo" {
			dateDirectory := path.Dir(proposal.TargetPath)
			keys = append(keys, coordination.PathKeys(proposal.ArchiveRootPath, dateDirectory)...)
		}
		if needsSourceInodeLock(proposal.Action) {
			entry, err := s.root.LstatEntry(proposal.SourcePath)
			if err != nil {
				return nil, nil, ErrConflict
			}
			if proposal.Action == "trash" || proposal.Action == "permanent_delete" {
				if entry.Symlink {
					if !entry.LinkIdentityAvailable || entry.LinkInode == 0 {
						return nil, nil, ErrConflict
					}
				} else if !entry.IdentityAvailable || entry.Inode == 0 || !entry.ChangeTimeAvailable {
					return nil, nil, ErrConflict
				}
			} else if entry.Symlink || !entry.IdentityAvailable {
				return nil, nil, ErrConflict
			}
			key := agentInodeKey(entry)
			if key == "" {
				return nil, nil, ErrConflict
			}
			if proposal.Action == "trash" || proposal.Action == "permanent_delete" || entry.Kind != fsroot.KindDirectory || isMetadataAction(proposal.Action) {
				keys = append(keys, key)
				lockedSourceEntries[i] = entry
			}
			if isMetadataAction(proposal.Action) {
				if key == "" {
					return nil, nil, ErrConflict
				}
				metadataEntries[i] = entry
			}
		}
		if proposal.Action == "trash_duplicate" {
			entry, err := s.root.LstatEntry(proposal.TargetPath)
			if err != nil || entry.Symlink || entry.Kind != fsroot.KindFile || !entry.IdentityAvailable {
				return nil, nil, ErrConflict
			}
			key := agentInodeKey(entry)
			if key == "" {
				return nil, nil, ErrConflict
			}
			keys = append(keys, key)
			lockedKeeperEntries[i] = entry
		}
	}
	if len(proposals) > 1 {
		if err := validateIndependentAnalysisOperations(analysisOperations); err != nil {
			return nil, nil, err
		}
		seenInodes := make(map[[2]uint64]struct{})
		for _, entry := range metadataEntries {
			key := [2]uint64{entry.Device, entry.Inode}
			if _, exists := seenInodes[key]; exists {
				return nil, nil, ErrConflict
			}
			seenInodes[key] = struct{}{}
		}
	}
	unlock, err := s.locks.Lock(ctx, keys...)
	if err != nil {
		return nil, nil, err
	}
	locked := true
	defer func() {
		if locked {
			unlock()
		}
	}()
	for index, before := range lockedSourceEntries {
		current, statErr := s.root.LstatEntry(proposals[index].SourcePath)
		if statErr != nil || agentInodeKey(current) != agentInodeKey(before) {
			return nil, nil, ErrConflict
		}
	}
	for index, before := range lockedKeeperEntries {
		current, statErr := s.root.LstatEntry(proposals[index].TargetPath)
		if statErr != nil || agentInodeKey(current) != agentInodeKey(before) {
			return nil, nil, ErrConflict
		}
	}
	operations := make([]store.OperationRecord, 0, len(proposals))
	cleanupKeepers := make(map[string]duplicateCleanupState)
	now := time.Now().UTC()
	for i, proposal := range proposals {
		var expectedSource *string
		if proposal.ExpectedSourceStateJSON != "" {
			expectedSource = &proposal.ExpectedSourceStateJSON
		}
		var source, targetParent fsroot.Entry
		var archivePhotoTarget *photoArchiveTargetState
		var archiveInputState *archiveSourceState
		var archiveOutputs *archiveTargetState
		var directorySnapshot *fsroot.TreeSnapshot
		var cleanupState *duplicateCleanupState
		var trashState *trashOperationState
		if proposal.Action == "trash_duplicate" {
			var state duplicateCleanupState
			state, source, targetParent, err = s.snapshotDuplicateCleanup(ctx, proposal, nil)
			if err == nil {
				if previous, exists := cleanupKeepers[proposal.TargetPath]; exists &&
					(!sameFileFingerprint(previous.Keeper, state.Keeper) || previous.SHA256 != state.SHA256) {
					return nil, nil, ErrConflict
				}
				cleanupKeepers[proposal.TargetPath] = state
				cleanupState = &state
			}
		} else if proposal.Action == "trash" || proposal.Action == "permanent_delete" {
			var state trashOperationState
			state, source, err = s.snapshotTrash(ctx, proposal, nil)
			if err == nil {
				trashState = &state
			}
		} else if proposal.Action == "edit_text" || proposal.Action == "overwrite_file" {
			source, err = s.validateTextEditSnapshot(proposal, expectedSource)
		} else if proposal.Action == "create_file" {
			source, err = s.validateCreateTextFileSnapshot(proposal, expectedSource, nil)
		} else if proposal.Action == "archive_photo" {
			var snapshot photoArchiveTargetState
			source, snapshot, err = s.snapshotArchivePhoto(proposal, expectedSource, nil)
			if err == nil {
				archivePhotoTarget = &snapshot
			}
		} else if proposal.Action == "create_symlink" {
			var expectedTargetParent *string
			if proposal.ExpectedTargetParentStateJSON != "" {
				expectedTargetParent = &proposal.ExpectedTargetParentStateJSON
			}
			source, targetParent, err = s.validateSymlinkSnapshot(proposal, expectedSource, expectedTargetParent)
		} else if isArchiveAction(proposal.Action) {
			var input archiveSourceState
			var outputs archiveTargetState
			source, input, outputs, err = s.snapshotArchiveOperation(ctx, proposal, nil, nil, true)
			if err == nil {
				archiveInputState, archiveOutputs = &input, &outputs
			}
		} else if isFileTargetAction(proposal.Action) {
			var expectedTargetParent *string
			if proposal.ExpectedTargetParentStateJSON != "" {
				expectedTargetParent = &proposal.ExpectedTargetParentStateJSON
			}
			source, targetParent, err = s.validateFileTargetSnapshot(proposal, expectedSource, expectedTargetParent)
		} else if proposal.Action == "copy_directory" {
			var expectedTargetParent *string
			if proposal.ExpectedTargetParentStateJSON != "" {
				expectedTargetParent = &proposal.ExpectedTargetParentStateJSON
			}
			var snapshot fsroot.TreeSnapshot
			snapshot, targetParent, err = s.validateCopyDirectorySnapshot(ctx, proposal, expectedSource, expectedTargetParent)
			if err == nil {
				directorySnapshot = &snapshot
				source = snapshot.Entries[0].Entry
			}
		} else {
			source, err = s.validateSnapshot(proposal, expectedSource, nil)
		}
		if err != nil {
			return nil, nil, err
		}
		if metadataEntry, ok := metadataEntries[i]; ok && (source.Device != metadataEntry.Device || source.Inode != metadataEntry.Inode) {
			return nil, nil, ErrConflict
		}
		var targetState any = fsroot.Fingerprint{Exists: false}
		if isFileTargetAction(proposal.Action) || proposal.Action == "copy_directory" || proposal.Action == "create_symlink" {
			targetState = targetParent.Fingerprint()
		}
		var sourceState any = source.Fingerprint()
		if directorySnapshot != nil {
			sourceState = directoryCopyState{RootPath: directorySnapshot.RootPath, Digest: directorySnapshot.Digest}
		}
		if cleanupState != nil {
			sourceState = cleanupState
			targetState = targetParent.Fingerprint()
		}
		if trashState != nil {
			sourceState = *trashState
		}
		if archivePhotoTarget != nil {
			targetState = *archivePhotoTarget
		}
		if archiveInputState != nil {
			sourceState = *archiveInputState
			targetState = *archiveOutputs
		}
		sourceJSON, err := json.Marshal(sourceState)
		if err != nil {
			return nil, nil, err
		}
		targetJSON, err := json.Marshal(targetState)
		if err != nil {
			return nil, nil, err
		}
		opID, err := randomID()
		if err != nil {
			return nil, nil, err
		}
		operations = append(operations, store.OperationRecord{
			ID: opID, Ordinal: i, Action: proposal.Action, SourcePath: proposal.SourcePath,
			TargetPath: proposal.TargetPath, Reason: proposal.Reason, Evidence: proposal.Evidence,
			Mode: proposal.Mode, OwnerUID: proposal.OwnerUID, GroupGID: proposal.GroupGID,
			SourceStateJSON: string(sourceJSON), TargetStateJSON: string(targetJSON),
			TargetConflictPolicy: "fail", Approval: store.ApprovalPending,
			Status: store.OperationPending, CreatedAt: now, UpdatedAt: now,
		})
	}
	locked = false
	return operations, unlock, nil
}

func proposalToAnalysisOperation(proposal Proposal) store.AnalysisOperation {
	filePath, targetPath := proposal.SourcePath, proposal.TargetPath
	if proposal.Action == "create_directory" {
		filePath = ""
	}
	if proposal.Action == "create_file" {
		filePath, targetPath = proposal.TargetPath, ""
	}
	if isMetadataAction(proposal.Action) || proposal.Action == "edit_text" || proposal.Action == "overwrite_file" || proposal.Action == "create_file" {
		targetPath = ""
	}
	return store.AnalysisOperation{
		Action: proposal.Action, FilePath: filePath, TargetPath: targetPath,
		RequestedMode: proposal.Mode, RequestedOwnerUID: proposal.OwnerUID, RequestedGroupGID: proposal.GroupGID,
	}
}

// Plan 返回持久方案。
func (s *Service) Plan(ctx context.Context, planID string) (store.PlanRecord, error) {
	return s.store.Plan(ctx, planID)
}

// RevisePlan 校验修改后的清单，并用新方案版本替代尚未审批的旧版。
func (s *Service) RevisePlan(ctx context.Context, planID string, expectedVersion int, proposals []Proposal) (store.PlanRecord, error) {
	if s.store == nil || s.root == nil || s.locks == nil {
		return store.PlanRecord{}, errors.New("agent service is not configured")
	}
	if expectedVersion < 1 {
		return store.PlanRecord{}, ErrConflict
	}
	previous, err := s.store.Plan(ctx, planID)
	if err != nil {
		return store.PlanRecord{}, err
	}
	if len(proposals) < 1 || len(proposals) > maxProposalOperations(proposals) {
		return store.PlanRecord{}, ErrInvalid
	}
	if previous.Version != expectedVersion || previous.Status != store.PlanAwaitingApproval {
		return store.PlanRecord{}, ErrConflict
	}
	task, err := s.store.Task(ctx, previous.TaskID)
	if err != nil {
		return store.PlanRecord{}, err
	}
	if task.Kind != "agent" || task.Status != store.TaskAwaitingApproval {
		return store.PlanRecord{}, ErrConflict
	}
	for index := range proposals {
		proposal := &proposals[index]
		proposal.RequestText = task.RequestText
		proposal.ScopePath = previous.ScopePath
		proposal.ConversationID = ""
		proposal.ModelProvider = ""
		proposal.ModelName = ""
		proposal.ExpectedSourceStateJSON = ""
		proposal.ExpectedTargetParentStateJSON = ""
		if isMetadataAction(proposal.Action) && proposal.TargetPath == "" {
			proposal.TargetPath = proposal.SourcePath
		}
	}
	operations, release, err := s.prepareOperations(ctx, proposals)
	if err != nil {
		return store.PlanRecord{}, err
	}
	defer release()
	now := time.Now().UTC()
	newID, err := randomID()
	if err != nil {
		return store.PlanRecord{}, err
	}
	plan := store.PlanRecord{
		ID: newID, TaskID: previous.TaskID, Version: expectedVersion + 1, Status: store.PlanAwaitingApproval,
		ScopePath: previous.ScopePath, ModelProvider: previous.ModelProvider, ModelName: previous.ModelName,
		DataScopeJSON: fmt.Sprintf(`{"proposalSource":"user-edit","previousPlanId":%q}`, previous.ID),
		CreatedAt:     now, UpdatedAt: now, Operations: operations,
	}
	if err := s.store.ReviseAgentPlan(ctx, previous.ID, expectedVersion, plan, now); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return store.PlanRecord{}, ErrConflict
		}
		return store.PlanRecord{}, err
	}
	return s.store.Plan(ctx, plan.ID)
}

// ReviseTextCandidate 用新的候选正文创建待审批方案版本。
func (s *Service) ReviseTextCandidate(ctx context.Context, planID, operationID string, expectedVersion int, content []byte) (store.PlanRecord, error) {
	if s.store == nil || s.root == nil || s.textPayloads == nil || !validTextFileCandidate(content) || expectedVersion < 1 {
		return store.PlanRecord{}, ErrInvalid
	}
	previous, err := s.store.Plan(ctx, planID)
	if err != nil {
		return store.PlanRecord{}, err
	}
	if previous.Version != expectedVersion || previous.Status != store.PlanAwaitingApproval || len(previous.Operations) != 1 {
		return store.PlanRecord{}, ErrConflict
	}
	old := previous.Operations[0]
	if old.ID != operationID || !isTextFileWriteAction(old.Action) || old.Approval != store.ApprovalPending || old.Status != store.OperationPending || old.Action == "edit_text" && len(content) == 0 {
		return store.PlanRecord{}, ErrConflict
	}
	task, err := s.store.Task(ctx, previous.TaskID)
	if err != nil {
		return store.PlanRecord{}, err
	}
	if task.Kind != "agent" || task.Status != store.TaskAwaitingApproval {
		return store.PlanRecord{}, ErrConflict
	}
	proposal := Proposal{
		RequestText: task.RequestText, ScopePath: previous.ScopePath, Action: old.Action,
		SourcePath: old.SourcePath, TargetPath: old.TargetPath, Reason: old.Reason,
		Evidence: old.Evidence, ExpectedSourceStateJSON: old.SourceStateJSON, ProposedContent: content,
	}
	operations, release, err := s.prepareOperations(ctx, []Proposal{proposal})
	if err != nil {
		return store.PlanRecord{}, err
	}
	defer release()
	operation := &operations[0]
	digest, size, err := s.textPayloads.stage(operation.ID, content)
	if err != nil {
		return store.PlanRecord{}, err
	}
	stageCommitted := false
	defer func() {
		if !stageCommitted {
			_ = s.textPayloads.remove(operation.ID)
		}
	}()
	operation.ProposedContentSHA256, operation.ProposedContentSize = digest, size
	now := time.Now().UTC()
	newID, err := randomID()
	if err != nil {
		return store.PlanRecord{}, err
	}
	plan := store.PlanRecord{
		ID: newID, TaskID: previous.TaskID, Version: expectedVersion + 1, Status: store.PlanAwaitingApproval,
		ScopePath: previous.ScopePath, ModelProvider: previous.ModelProvider, ModelName: previous.ModelName,
		DataScopeJSON: fmt.Sprintf(`{"proposalSource":"user-edit","previousPlanId":%q}`, previous.ID),
		CreatedAt:     now, UpdatedAt: now, Operations: operations,
	}
	if err := s.store.ReviseAgentPlan(ctx, previous.ID, expectedVersion, plan, now); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return store.PlanRecord{}, ErrConflict
		}
		return store.PlanRecord{}, err
	}
	stageCommitted = true
	if err := s.textPayloads.remove(old.ID); err != nil {
		return store.PlanRecord{}, fmt.Errorf("remove superseded Agent text candidate: %w", err)
	}
	return s.store.Plan(ctx, plan.ID)
}

// LatestPlan 返回任务的最新方案。
func (s *Service) LatestPlan(ctx context.Context, taskID string) (store.PlanRecord, error) {
	return s.store.LatestPlanForTask(ctx, taskID)
}

// Decide 逐项记录用户审批，并在批准时复核文件快照。
func (s *Service) Decide(ctx context.Context, planID, operationID, decision string, expectedVersion ...int) (store.PlanRecord, error) {
	plan, err := s.store.Plan(ctx, planID)
	if err != nil {
		return store.PlanRecord{}, err
	}
	if plan.ReviewedAt != nil {
		return store.PlanRecord{}, ErrConflict
	}
	if len(expectedVersion) > 1 || len(expectedVersion) == 1 && expectedVersion[0] != plan.Version {
		return store.PlanRecord{}, ErrConflict
	}
	if isShellParent(plan) {
		if decision != "reject" {
			return store.PlanRecord{}, ErrInvalid
		}
		changes, err := s.shellPlanChanges(ctx, planID)
		if err != nil {
			return store.PlanRecord{}, err
		}
		for _, change := range changes {
			if change.Approval == store.ApprovalPending && change.Status == store.OperationPending {
				return s.DecideShellChange(ctx, planID, change.ID, decision, plan.Version)
			}
		}
		return store.PlanRecord{}, ErrConflict
	}
	op, err := singleOperation(plan, operationID)
	if err != nil {
		return store.PlanRecord{}, err
	}
	if decision == "approve" {
		release, err := s.lockOperation(ctx, op)
		if err != nil {
			return store.PlanRecord{}, err
		}
		defer release()
		if err := s.validateStoredSnapshot(ctx, plan.ScopePath, op); err != nil {
			if invalidateErr := s.store.InvalidateAgentOperation(ctx, plan.ID, op.ID, "文件状态已变化，方案失效", time.Now().UTC()); invalidateErr != nil {
				return store.PlanRecord{}, fmt.Errorf("record invalidated approval: %w", invalidateErr)
			}
			_ = s.removeTextPayload(op)
			updated, readErr := s.store.Plan(ctx, plan.ID)
			if readErr != nil {
				return store.PlanRecord{}, readErr
			}
			return updated, fmt.Errorf("approval invalidated: %w (%v)", ErrConflict, err)
		}
	} else if decision != "reject" {
		return store.PlanRecord{}, ErrInvalid
	}
	state := store.ApprovalApproved
	if decision == "reject" {
		state = store.ApprovalRejected
	}
	if err := s.store.SetAgentApproval(ctx, plan.ID, op.ID, state, time.Now().UTC(), plan.Version); err != nil {
		return store.PlanRecord{}, err
	}
	updated, err := s.store.Plan(ctx, plan.ID)
	if err != nil {
		return store.PlanRecord{}, err
	}
	if decision == "reject" {
		if err := s.removeTextPayload(op); err != nil {
			return updated, err
		}
	}
	return updated, nil
}

// RejectPlan 拒绝整份尚未执行的计划，不暴露逐项审批入口。
func (s *Service) RejectPlan(ctx context.Context, planID string, expectedVersion int) (store.PlanRecord, error) {
	if expectedVersion < 1 {
		return store.PlanRecord{}, ErrConflict
	}
	plan, err := s.store.Plan(ctx, planID)
	if err != nil {
		return store.PlanRecord{}, err
	}
	if plan.Version != expectedVersion || plan.Status != store.PlanAwaitingApproval || len(plan.Operations) == 0 {
		return store.PlanRecord{}, ErrConflict
	}
	return s.Decide(ctx, planID, plan.Operations[0].ID, "reject", expectedVersion)
}

// StartPlan 固定审核时的权限模式，并验证不可恢复删除的明确确认。
func (s *Service) StartPlan(ctx context.Context, planID string, expectedVersion int, expectedMode string, confirmPermanentDelete bool) (store.PlanRecord, error) {
	if expectedVersion < 1 || expectedMode != "bypass" && expectedMode != "manual" {
		return store.PlanRecord{}, ErrConflict
	}
	plan, err := s.store.Plan(ctx, planID)
	if err != nil {
		return store.PlanRecord{}, err
	}
	if plan.Version != expectedVersion || plan.Status != store.PlanAwaitingApproval || plan.ReviewedAt != nil {
		return store.PlanRecord{}, ErrConflict
	}
	if planContainsPermanentDelete(plan) != confirmPermanentDelete {
		return store.PlanRecord{}, ErrConflict
	}
	at := time.Now().UTC()
	var mode string
	if isShellParent(plan) {
		mode, err = s.reviewShellPlan(ctx, plan, expectedVersion, expectedMode, at)
		if err != nil {
			return store.PlanRecord{}, err
		}
	} else {
		if len(plan.Operations) == 0 || len(plan.Operations) > maxStoredPlanOperations(plan.Operations) {
			return store.PlanRecord{}, ErrUnsupported
		}
		for _, operation := range plan.Operations {
			if !supportedAction(operation.Action) || operation.Approval != store.ApprovalPending || operation.Status != store.OperationPending {
				return store.PlanRecord{}, ErrConflict
			}
			release, lockErr := s.lockOperation(ctx, operation)
			if lockErr != nil {
				return store.PlanRecord{}, lockErr
			}
			snapshotErr := s.validateStoredSnapshot(ctx, plan.ScopePath, operation)
			release()
			if snapshotErr != nil {
				if invalidateErr := s.store.InvalidateAgentOperation(ctx, plan.ID, operation.ID, "审核时文件状态已变化，方案失效", at); invalidateErr != nil {
					return store.PlanRecord{}, fmt.Errorf("record invalidated plan review: %w", invalidateErr)
				}
				updated, readErr := s.store.Plan(ctx, plan.ID)
				if readErr != nil {
					return store.PlanRecord{}, readErr
				}
				return updated, fmt.Errorf("plan review invalidated: %w (%v)", ErrConflict, snapshotErr)
			}
		}
		mode, err = s.store.ReviewAgentPlan(ctx, planID, expectedVersion, expectedMode, at, confirmPermanentDelete)
		if err != nil {
			return store.PlanRecord{}, err
		}
	}
	reviewed, err := s.store.Plan(ctx, planID)
	if err != nil || mode != "bypass" {
		return reviewed, err
	}
	if isShellParent(reviewed) {
		return s.executeOperation(ctx, reviewed.ID, reviewed.ScopePath, reviewed.Operations[0])
	}
	return s.Execute(ctx, planID, reviewed.Version)
}

// planContainsPermanentDelete 检查计划是否含不可恢复删除。
func planContainsPermanentDelete(plan store.PlanRecord) bool {
	for _, operation := range plan.Operations {
		if operation.Action == "permanent_delete" {
			return true
		}
	}
	return false
}

// reviewShellPlan 锁定并校验快照；返回后释放文件锁，再进入工具执行器。
func (s *Service) reviewShellPlan(ctx context.Context, plan store.PlanRecord, expectedVersion int, expectedMode string, at time.Time) (string, error) {
	if s.shellPayloads == nil || s.trash == nil {
		return "", ErrUnsupported
	}
	changes, err := s.shellPlanChanges(ctx, plan.ID)
	if err != nil {
		return "", err
	}
	if len(changes) == 0 || len(changes) > store.MaxShellChanges {
		return "", ErrConflict
	}
	release, err := s.locks.Lock(ctx, shellChangeLockKeys(plan.ScopePath, changes)...)
	if err != nil {
		return "", err
	}
	defer release()
	for _, change := range changes {
		state, stateErr := decodeShellOriginalState(change)
		if stateErr != nil {
			return "", stateErr
		}
		if err := s.verifyShellOriginal(ctx, plan.ScopePath, change, state); err != nil {
			return "", fmt.Errorf("Shell plan changed before review: %w", ErrConflict)
		}
		if change.Kind != "deleted" && change.CandidateSHA256 != "" {
			payload, openErr := s.shellPayloads.open(change.PayloadID, change.CandidateSHA256, change.CandidateSize)
			if openErr != nil {
				return "", ErrConflict
			}
			if closeErr := payload.Close(); closeErr != nil {
				return "", closeErr
			}
		}
	}
	return s.store.ReviewShellPlan(ctx, plan.ID, expectedVersion, expectedMode, at)
}

// ExecuteStep 按用户确认逐次执行已审核方案中的下一步。
func (s *Service) ExecuteStep(ctx context.Context, planID, operationID string, expectedVersion int) (store.PlanRecord, error) {
	if expectedVersion < 1 || operationID == "" {
		return store.PlanRecord{}, ErrConflict
	}
	plan, err := s.store.Plan(ctx, planID)
	if err != nil {
		return store.PlanRecord{}, err
	}
	if plan.Version != expectedVersion || plan.ExecutionMode != "manual" || plan.ReviewedAt == nil || plan.Status != store.PlanApproved {
		return store.PlanRecord{}, ErrConflict
	}
	if isShellParent(plan) {
		if len(plan.Operations) != 1 || plan.Operations[0].ID != operationID {
			return store.PlanRecord{}, ErrConflict
		}
		return s.executeOperation(ctx, plan.ID, plan.ScopePath, plan.Operations[0])
	}
	operation, err := singleOperation(plan, operationID)
	if err != nil || operation.Approval != store.ApprovalPending && operation.Approval != store.ApprovalApproved || operation.Status != store.OperationPending {
		return store.PlanRecord{}, ErrConflict
	}
	release, err := s.lockOperation(ctx, operation)
	if err != nil {
		return store.PlanRecord{}, err
	}
	defer release()
	if err := s.validateStoredSnapshot(ctx, plan.ScopePath, operation); err != nil {
		if invalidateErr := s.store.InvalidateAgentOperation(ctx, plan.ID, operation.ID, "执行前文件状态已变化，方案失效", time.Now().UTC()); invalidateErr != nil {
			return store.PlanRecord{}, fmt.Errorf("record invalidated step: %w", invalidateErr)
		}
		updated, readErr := s.store.Plan(ctx, plan.ID)
		if readErr != nil {
			return store.PlanRecord{}, readErr
		}
		return updated, fmt.Errorf("step invalidated: %w (%v)", ErrConflict, err)
	}
	if err := s.store.StartReviewedAgentOperation(ctx, plan.ID, operation.ID, plan.Version, time.Now().UTC()); err != nil {
		return store.PlanRecord{}, err
	}
	latest, err := s.store.Plan(ctx, plan.ID)
	if err != nil {
		return store.PlanRecord{}, err
	}
	current, err := singleOperation(latest, operation.ID)
	if err != nil {
		return store.PlanRecord{}, err
	}
	if err := s.validateStoredSnapshot(ctx, latest.ScopePath, current); err != nil {
		if invalidateErr := s.store.InvalidateAgentOperation(ctx, latest.ID, current.ID, "执行占用后文件状态变化，方案失效", time.Now().UTC()); invalidateErr != nil {
			return store.PlanRecord{}, fmt.Errorf("record invalidated running step: %w", invalidateErr)
		}
		updated, readErr := s.store.Plan(ctx, latest.ID)
		if readErr != nil {
			return store.PlanRecord{}, readErr
		}
		return updated, fmt.Errorf("step invalidated after claim: %w (%v)", ErrConflict, err)
	}
	return s.executeOperation(ctx, latest.ID, latest.ScopePath, current)
}

// Execute 串行执行全部已批准操作，首个失败后取消后续项。
func (s *Service) Execute(ctx context.Context, planID string, expectedVersion ...int) (store.PlanRecord, error) {
	plan, err := s.store.Plan(ctx, planID)
	if err != nil {
		return store.PlanRecord{}, err
	}
	if len(expectedVersion) > 1 || len(expectedVersion) == 1 && expectedVersion[0] != plan.Version {
		return store.PlanRecord{}, ErrConflict
	}
	if isShellParent(plan) {
		if plan.ReviewedAt != nil && plan.ExecutionMode == "manual" {
			return store.PlanRecord{}, ErrConflict
		}
		return s.executeShellPlan(ctx, plan, expectedVersion)
	}
	if plan.ReviewedAt != nil && plan.ExecutionMode == "manual" {
		return store.PlanRecord{}, ErrConflict
	}
	if plan.Status != store.PlanApproved || len(plan.Operations) < 1 || len(plan.Operations) > maxStoredPlanOperations(plan.Operations) {
		return store.PlanRecord{}, ErrConflict
	}
	for _, operation := range plan.Operations {
		if !supportedAction(operation.Action) || operation.Approval != store.ApprovalApproved ||
			operation.Status != store.OperationPending && operation.Status != store.OperationSucceeded {
			return store.PlanRecord{}, ErrConflict
		}
	}
	if err := s.store.ClaimAgentPlanExecution(ctx, plan.ID, plan.Version, time.Now().UTC()); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return store.PlanRecord{}, ErrConflict
		}
		return store.PlanRecord{}, err
	}
	plan, err = s.store.Plan(ctx, planID)
	if err != nil {
		return store.PlanRecord{}, err
	}
	for _, operation := range plan.Operations {
		if operation.Status == store.OperationSucceeded {
			continue
		}
		latest, readErr := s.store.Plan(ctx, planID)
		if readErr != nil {
			return store.PlanRecord{}, readErr
		}
		if latest.Status == store.PlanCancelled {
			if err := s.removeCancelledTextPayloads(latest); err != nil {
				return latest, err
			}
			return latest, nil
		}
		if latest.Version != plan.Version || latest.Status != store.PlanExecuting {
			return latest, ErrConflict
		}
		plan = latest
		release, lockErr := s.lockOperation(ctx, operation)
		if lockErr != nil {
			return store.PlanRecord{}, lockErr
		}
		latest, readErr = s.store.Plan(ctx, planID)
		if readErr != nil {
			release()
			return store.PlanRecord{}, readErr
		}
		if latest.Version != plan.Version || latest.Status != store.PlanExecuting {
			release()
			if latest.Status == store.PlanCancelled {
				if err := s.removeCancelledTextPayloads(latest); err != nil {
					return latest, err
				}
				return latest, nil
			}
			return latest, ErrConflict
		}
		current, findErr := singleOperation(latest, operation.ID)
		if findErr != nil || current.Approval != store.ApprovalApproved || current.Status != store.OperationPending {
			release()
			return latest, ErrConflict
		}
		if snapshotErr := s.validateStoredSnapshot(ctx, latest.ScopePath, current); snapshotErr != nil {
			invalidateErr := s.store.InvalidateAgentOperation(ctx, latest.ID, current.ID, "执行前文件状态已变化，方案失效，后续操作取消", time.Now().UTC())
			release()
			if invalidateErr != nil {
				return store.PlanRecord{}, fmt.Errorf("record invalidated plan: %w", invalidateErr)
			}
			_ = s.removeTextPayload(current)
			updated, readErr := s.store.Plan(ctx, plan.ID)
			if readErr != nil {
				return store.PlanRecord{}, readErr
			}
			return updated, fmt.Errorf("execution invalidated: %w (%v)", ErrConflict, snapshotErr)
		}
		if err := s.store.StartAgentOperation(ctx, latest.ID, current.ID, time.Now().UTC()); err != nil {
			release()
			if errors.Is(err, store.ErrConflict) {
				updated, readErr := s.store.Plan(ctx, planID)
				if readErr != nil {
					return store.PlanRecord{}, readErr
				}
				if updated.Status == store.PlanCancelled {
					return updated, nil
				}
			}
			return store.PlanRecord{}, err
		}
		if snapshotErr := s.validateStoredSnapshot(ctx, latest.ScopePath, current); snapshotErr != nil {
			invalidateErr := s.store.InvalidateAgentOperation(ctx, latest.ID, current.ID, "执行占用后文件状态变化，方案失效，后续操作取消", time.Now().UTC())
			release()
			if invalidateErr != nil {
				return store.PlanRecord{}, fmt.Errorf("record invalidated running plan: %w", invalidateErr)
			}
			_ = s.removeTextPayload(current)
			updated, readErr := s.store.Plan(ctx, plan.ID)
			if readErr != nil {
				return store.PlanRecord{}, readErr
			}
			return updated, fmt.Errorf("execution invalidated after claim: %w (%v)", ErrConflict, snapshotErr)
		}
		updated, operationErr := s.executeOperation(ctx, latest.ID, latest.ScopePath, current)
		release()
		if operationErr != nil {
			if updated.Status == store.PlanExecuting {
				if cancelErr := s.store.CancelUnstartedAgentOperations(ctx, latest.ID, current.ID, "前序操作失败，后续操作未执行", time.Now().UTC()); cancelErr != nil {
					return updated, errors.Join(operationErr, cancelErr)
				}
				if finalizeErr := s.store.FinalizeAgentPlan(ctx, latest.ID, time.Now().UTC()); finalizeErr != nil {
					return updated, errors.Join(operationErr, finalizeErr)
				}
				updated, readErr = s.store.Plan(ctx, latest.ID)
				if readErr != nil {
					return store.PlanRecord{}, readErr
				}
			}
			return updated, operationErr
		}
		plan = updated
	}
	return s.store.Plan(ctx, planID)
}

// StopExecution 请求当前操作安全结束后停止后续项。
func (s *Service) StopExecution(ctx context.Context, planID string, expectedVersion int) (store.PlanRecord, error) {
	if err := s.store.RequestAgentPlanStop(ctx, planID, expectedVersion, time.Now().UTC()); err != nil {
		return store.PlanRecord{}, err
	}
	plan, err := s.store.Plan(ctx, planID)
	if err != nil {
		return store.PlanRecord{}, err
	}
	if err := s.removeCancelledTextPayloads(plan); err != nil {
		return plan, err
	}
	return plan, nil
}

// removeCancelledTextPayloads 清理已取消方案中的候选正文。
func (s *Service) removeCancelledTextPayloads(plan store.PlanRecord) error {
	if plan.Status != store.PlanCancelled {
		return nil
	}
	for _, operation := range plan.Operations {
		if err := s.removeTextPayload(operation); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) executeOperation(ctx context.Context, planID, scope string, op store.OperationRecord) (store.PlanRecord, error) {
	tool, ok := agentTools()[op.Action]
	if !ok || tool.execute == nil {
		return store.PlanRecord{}, ErrUnsupported
	}
	return tool.execute(s, ctx, planID, scope, op)
}

func (s *Service) executeArchiveCreate(ctx context.Context, planID, scope string, op store.OperationRecord) (store.PlanRecord, error) {
	if err := s.validateStoredSnapshot(ctx, scope, op); err != nil {
		return s.invalidateArchiveOperation(ctx, planID, op, "归档来源或目标状态已变化，方案失效，请重新审批", err)
	}
	format, ok := archiveCreateFormat(op.TargetPath)
	if !ok {
		return s.finishArchiveOperation(ctx, planID, op, store.OperationFailed, "压缩包格式不受支持，未创建文件", ErrUnsupported)
	}
	tempDir, err := os.MkdirTemp("", ".filesage-agent-archive-")
	if err != nil {
		return s.finishArchiveOperation(ctx, planID, op, store.OperationFailed, "无法创建安全暂存空间，未写入目标目录", err)
	}
	defer os.RemoveAll(tempDir)
	if err := os.Chmod(tempDir, 0o700); err != nil {
		return s.finishArchiveOperation(ctx, planID, op, store.OperationFailed, "无法保护归档暂存空间，未写入目标目录", err)
	}
	temporary, err := os.OpenFile(filepath.Join(tempDir, "archive"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return s.finishArchiveOperation(ctx, planID, op, store.OperationFailed, "无法创建归档暂存文件，未写入目标目录", err)
	}
	defer temporary.Close()
	if err := s.root.WriteArchiveFormat(ctx, []string{op.SourcePath}, temporary, fsroot.ArchiveLimits{
		MaxEntries: agentArchiveMaxEntries, MaxBytes: agentArchiveMaxBytes,
	}, format); err != nil {
		if isArchiveSnapshotConflict(err) {
			return s.invalidateArchiveOperation(ctx, planID, op, "归档来源已变化，方案失效，请重新审批", err)
		}
		return s.finishArchiveOperation(ctx, planID, op, store.OperationFailed, "压缩包生成失败，目标文件未发布", err)
	}
	if err := s.validateStoredSnapshot(ctx, scope, op); err != nil {
		return s.invalidateArchiveOperation(ctx, planID, op, "归档来源或目标状态已变化，方案失效，请重新审批", err)
	}
	if err := temporary.Sync(); err != nil {
		return s.finishArchiveOperation(ctx, planID, op, store.OperationFailed, "压缩包暂存未能持久化，目标文件未发布", err)
	}
	info, err := temporary.Stat()
	if err != nil || info.Size() <= 0 {
		if err == nil {
			err = fsroot.ErrArchiveCorrupt
		}
		return s.finishArchiveOperation(ctx, planID, op, store.OperationFailed, "压缩包暂存内容无效，目标文件未发布", err)
	}
	if _, err := temporary.Seek(0, io.SeekStart); err != nil {
		return s.finishArchiveOperation(ctx, planID, op, store.OperationFailed, "无法读取归档暂存文件，目标文件未发布", err)
	}
	digest, publishErr := s.root.WriteNewAtomicFrom(op.TargetPath, temporary, info.Size(), 0o644)
	if digest == "" {
		if isArchiveSnapshotConflict(publishErr) {
			return s.invalidateArchiveOperation(ctx, planID, op, "目标路径在执行期间被占用，方案失效，请重新审批", publishErr)
		}
		return s.finishArchiveOperation(ctx, planID, op, store.OperationFailed, "压缩包发布失败，未覆盖目标文件", publishErr)
	}
	created, statErr := s.root.LstatEntry(op.TargetPath)
	actualDigest, actual, hashErr := s.hashFile(op.TargetPath)
	if statErr != nil || hashErr != nil || created.Symlink || created.Kind != fsroot.KindFile || created.Size != info.Size() || actualDigest != digest || actual.Size != info.Size() {
		return s.finishArchiveOperation(ctx, planID, op, store.OperationPartial, "压缩包已发布，但结果无法完整核验，需人工检查", errors.Join(publishErr, statErr, hashErr, ErrConflict))
	}
	if publishErr != nil {
		return s.finishArchiveOperation(ctx, planID, op, store.OperationPartial, "压缩包已发布，但持久化状态无法确认，需人工检查", publishErr)
	}
	return s.finishArchiveOperation(ctx, planID, op, store.OperationSucceeded, "压缩包已创建："+op.TargetPath, nil)
}

func (s *Service) executeArchiveExtract(ctx context.Context, planID, scope string, op store.OperationRecord) (store.PlanRecord, error) {
	if err := s.validateStoredSnapshot(ctx, scope, op); err != nil {
		return s.invalidateArchiveOperation(ctx, planID, op, "归档来源或解压目标已变化，方案失效，请重新审批", err)
	}
	var source archiveSourceState
	var target archiveTargetState
	if json.Unmarshal([]byte(op.SourceStateJSON), &source) != nil || json.Unmarshal([]byte(op.TargetStateJSON), &target) != nil {
		return s.finishArchiveOperation(ctx, planID, op, store.OperationPartial, "审批清单无法读取，未确认解压结果，需人工核查", ErrConflict)
	}
	manifest := fsroot.ArchiveManifest{Fingerprint: source.Fingerprint, Digest: source.ManifestDigest}
	result, extractErr := s.root.ExtractArchiveApproved(ctx, op.SourcePath, op.TargetPath, manifest, target.Parent, target.PlannedPaths, agentArchiveReadLimits())
	if len(result.Paths) > 0 {
		if extractErr != nil || !sameAgentPaths(result.Paths, target.PlannedPaths) || len(result.Errors) > 0 {
			message := "解压内容已部分发布，剩余结果需人工核查；不会自动重放"
			if errors.Is(extractErr, fsroot.ErrPublishedNotDurable) && sameAgentPaths(result.Paths, target.PlannedPaths) {
				message = "解压内容已发布，但持久性未确认，需人工核查；不会自动重放"
			}
			if err := s.store.FinishAgentOperation(ctx, planID, op.ID, store.OperationPartial, message, time.Now().UTC()); err != nil {
				return store.PlanRecord{}, errors.Join(extractErr, err)
			}
			plan, err := s.store.Plan(ctx, planID)
			return plan, errors.Join(extractErr, err, ErrConflict)
		}
		return s.finishArchiveOperation(ctx, planID, op, store.OperationSucceeded, fmt.Sprintf("已解压 %d 项", len(result.Paths)), nil)
	}
	if isArchiveSnapshotConflict(extractErr) {
		return s.invalidateArchiveOperation(ctx, planID, op, "归档内容或目标路径已变化，方案失效，请重新审批", extractErr)
	}
	if extractErr != nil {
		return s.finishArchiveOperation(ctx, planID, op, store.OperationFailed, "解压失败，未发布内容", extractErr)
	}
	return s.finishArchiveOperation(ctx, planID, op, store.OperationPartial, "解压未返回可核验结果，需人工核查", ErrConflict)
}

func (s *Service) finishArchiveOperation(ctx context.Context, planID string, op store.OperationRecord, status store.OperationStatus, message string, resultErr error) (store.PlanRecord, error) {
	if err := s.store.FinishAgentOperation(ctx, planID, op.ID, status, message, time.Now().UTC()); err != nil {
		return store.PlanRecord{}, errors.Join(resultErr, err)
	}
	plan, err := s.store.Plan(ctx, planID)
	if status != store.OperationSucceeded && resultErr == nil {
		resultErr = ErrConflict
	}
	return plan, errors.Join(resultErr, err)
}

func (s *Service) invalidateArchiveOperation(ctx context.Context, planID string, op store.OperationRecord, message string, cause error) (store.PlanRecord, error) {
	if err := s.store.InvalidateAgentOperation(ctx, planID, op.ID, message, time.Now().UTC()); err != nil {
		return store.PlanRecord{}, errors.Join(cause, err)
	}
	plan, err := s.store.Plan(ctx, planID)
	return plan, errors.Join(ErrConflict, cause, err)
}

func isArchiveSnapshotConflict(err error) bool {
	return errors.Is(err, ErrConflict) || errors.Is(err, fsroot.ErrConflict) || errors.Is(err, fsroot.ErrPathChanged) || errors.Is(err, fs.ErrExist)
}

func (s *Service) executeArchivePhoto(ctx context.Context, planID, scope string, op store.OperationRecord) (store.PlanRecord, error) {
	if snapshotErr := s.validateStoredSnapshot(ctx, scope, op); snapshotErr != nil {
		return s.finishArchivePhoto(ctx, planID, op, store.OperationFailed, "照片状态已变化，未创建归档目录", snapshotErr)
	}
	created, mkdirErr := s.root.MkdirAllNoFollow(path.Dir(op.TargetPath), 0o755)
	if mkdirErr != nil {
		status, message := store.OperationFailed, "归档年月目录创建失败，照片未移动"
		if len(created) > 0 {
			status, message = store.OperationPartial, "年月目录已部分创建但照片未移动，需人工核查"
		}
		if err := s.store.FinishAgentOperation(ctx, planID, op.ID, status, message, time.Now().UTC()); err != nil {
			return store.PlanRecord{}, errors.Join(mkdirErr, err)
		}
		plan, err := s.store.Plan(ctx, planID)
		return plan, errors.Join(mkdirErr, err)
	}
	if snapshotErr := s.validateStoredSnapshot(ctx, scope, op); snapshotErr != nil {
		status, message := store.OperationFailed, "归档目录校验失败，照片未移动"
		if len(created) > 0 {
			status, message = store.OperationPartial, "年月目录已创建但照片未移动，需人工核查"
		}
		return s.finishArchivePhoto(ctx, planID, op, status, message, snapshotErr)
	}
	var expected fsroot.Fingerprint
	if json.Unmarshal([]byte(op.SourceStateJSON), &expected) != nil {
		return s.finishArchivePhoto(ctx, planID, op, store.OperationPartial, "归档目录已创建但来源快照无效，需人工核查", ErrConflict)
	}
	outcome, moveErr := s.root.MoveFileNoReplace(ctx, op.SourcePath, op.TargetPath, expected)
	status, message, resultErr := store.OperationSucceeded, "照片已按拍摄日期归档", moveErr
	switch {
	case outcome.Published && outcome.SourceRemoved && moveErr == nil:
		target, statErr := s.root.LstatEntry(op.TargetPath)
		if statErr != nil || target.Symlink || target.Kind != fsroot.KindFile || target.Size != expected.Size || target.Mode.Perm() != expected.Mode.Perm() {
			status, message, resultErr = store.OperationPartial, "照片已移动但目标状态无法确认，需人工核查", ErrConflict
		} else if outcome.SHA256 == "" && !matchesRenameResult(target.Fingerprint(), op.SourceStateJSON) || outcome.SHA256 != "" && len(outcome.SHA256) != 64 {
			status, message, resultErr = store.OperationPartial, "照片移动结果无法核验，需人工核查", ErrConflict
		}
	case outcome.Published || outcome.SourceRemoved:
		status, message = store.OperationPartial, "照片移动结果不完整，需核查源与目标"
		if resultErr == nil {
			resultErr = ErrConflict
		}
	case moveErr != nil && len(created) > 0:
		status, message = store.OperationPartial, "年月目录已创建但照片未移动，需人工核查"
	case moveErr != nil:
		status, message = store.OperationFailed, "照片归档失败，未覆盖目标文件"
	default:
		status, message, resultErr = store.OperationPartial, "无法确认照片归档结果，需人工核查", ErrConflict
	}
	return s.finishArchivePhoto(ctx, planID, op, status, message, resultErr)
}

func (s *Service) finishArchivePhoto(ctx context.Context, planID string, op store.OperationRecord, status store.OperationStatus, message string, resultErr error) (store.PlanRecord, error) {
	if err := s.store.FinishAgentOperation(ctx, planID, op.ID, status, message, time.Now().UTC()); err != nil {
		return store.PlanRecord{}, errors.Join(resultErr, err)
	}
	plan, err := s.store.Plan(ctx, planID)
	if err != nil {
		return store.PlanRecord{}, err
	}
	return plan, resultErr
}

// executeSymlink 创建已审批软链接，并核验来源与链接文本后记录审计结果。
func (s *Service) executeSymlink(ctx context.Context, planID, scope string, op store.OperationRecord) (store.PlanRecord, error) {
	if err := s.validateStoredSnapshot(ctx, scope, op); err != nil {
		return s.finishSymlink(ctx, planID, op, store.OperationFailed, "软链接来源或目标目录状态已变化，未创建链接", err)
	}
	if err := s.root.SymlinkNoReplace(op.SourcePath, op.TargetPath); err != nil {
		return s.finishSymlink(ctx, planID, op, store.OperationFailed, "软链接创建失败，未覆盖已有路径", err)
	}
	if err := s.verifyCreatedSymlink(op); err != nil {
		return s.finishSymlink(ctx, planID, op, store.OperationPartial, "软链接已创建，但结果无法完整核验，需人工检查", err)
	}
	return s.finishSymlink(ctx, planID, op, store.OperationSucceeded, "软链接已创建", nil)
}

func (s *Service) finishSymlink(ctx context.Context, planID string, op store.OperationRecord, status store.OperationStatus, message string, resultErr error) (store.PlanRecord, error) {
	if err := s.store.FinishAgentOperation(ctx, planID, op.ID, status, message, time.Now().UTC()); err != nil {
		return store.PlanRecord{}, errors.Join(resultErr, err)
	}
	plan, err := s.store.Plan(ctx, planID)
	if err != nil {
		return store.PlanRecord{}, err
	}
	return plan, resultErr
}

// verifyCreatedSymlink 确认链接文本、来源对象和目标父目录仍与审批快照一致。
func (s *Service) verifyCreatedSymlink(op store.OperationRecord) error {
	source, sourceErr := s.root.LstatEntry(op.SourcePath)
	parent, parentErr := s.directoryParentEntry(parentPath(op.TargetPath))
	link, linkErr := s.root.LstatEntry(op.TargetPath)
	if sourceErr != nil || source.Symlink || !matchesFingerprint(source.Fingerprint(), op.SourceStateJSON) ||
		parentErr != nil || !matchesTargetDirectoryParent(parentPath(op.TargetPath), parent, op.TargetStateJSON) ||
		linkErr != nil || !link.Symlink || link.LinkStatus != fsroot.LinkStatusInternal || link.Kind != source.Kind ||
		link.LinkTarget != relativeLinkTarget(parentPath(op.TargetPath), op.SourcePath) {
		return ErrConflict
	}
	return nil
}

func relativeLinkTarget(parentPath, targetPath string) string {
	parentParts, targetParts := strings.Split(parentPath, "/"), strings.Split(targetPath, "/")
	if parentPath == "" {
		parentParts = nil
	}
	common := 0
	for common < len(parentParts) && common < len(targetParts) && parentParts[common] == targetParts[common] {
		common++
	}
	parts := make([]string, 0, len(parentParts)-common+len(targetParts)-common)
	for range len(parentParts) - common {
		parts = append(parts, "..")
	}
	parts = append(parts, targetParts[common:]...)
	if len(parts) == 0 {
		return "."
	}
	return strings.Join(parts, "/")
}

func (s *Service) executeTextEdit(ctx context.Context, planID string, op store.OperationRecord) (store.PlanRecord, error) {
	content, err := s.textPayloads.read(op.ID, op.ProposedContentSHA256, op.ProposedContentSize)
	if err != nil {
		if finishErr := s.store.FinishAgentOperation(ctx, planID, op.ID, store.OperationFailed, "候选内容缺失或校验失败，文件未写入", time.Now().UTC()); finishErr != nil {
			return store.PlanRecord{}, errors.Join(err, finishErr)
		}
		if removeErr := s.removeTextPayload(op); removeErr != nil {
			return store.PlanRecord{}, errors.Join(err, removeErr)
		}
		plan, readErr := s.store.Plan(ctx, planID)
		if readErr != nil {
			return store.PlanRecord{}, errors.Join(err, readErr)
		}
		return plan, errors.Join(ErrConflict, err)
	}
	var expected fsroot.Fingerprint
	targetPath, mode := op.SourcePath, fs.FileMode(0o644)
	if op.Action == "create_file" {
		targetPath = op.TargetPath
	} else {
		if json.Unmarshal([]byte(op.SourceStateJSON), &expected) != nil || !expected.Exists || expected.Kind != fsroot.KindFile || expected.Links != 1 {
			return store.PlanRecord{}, ErrConflict
		}
		mode = expected.Mode.Perm()
	}
	writeErr := s.root.WriteAtomicIfUnchanged(targetPath, expected, content, mode)
	status, message, resultErr := store.OperationSucceeded, textWriteSuccessMessage(op.Action), writeErr
	verifyErr := s.verifyTextEditResult(op)
	var recovery *fsroot.WriteRecoveryError
	switch {
	case writeErr == nil && verifyErr == nil:
		resultErr = nil
	case errors.As(writeErr, &recovery):
		status, message = store.OperationPartial, fmt.Sprintf("待核查文件保留在 %q", recovery.Path)
	case errors.Is(writeErr, fsroot.ErrPublishedNotDurable):
		status, message = store.OperationPartial, "文件已写入，但目录状态未能持久确认；需人工核查"
	case writeErr == nil:
		status, message, resultErr = store.OperationPartial, "写入后文件状态发生变化；需人工核查", verifyErr
	default:
		status, message = store.OperationFailed, "文件状态已变化，未完成写入；请刷新后重新审阅"
		resultErr = writeErr
	}
	if status != store.OperationSucceeded && verifyErr != nil && resultErr == nil {
		resultErr = verifyErr
	}
	if status == store.OperationSucceeded {
		resultErr = nil
	}
	if err := s.store.FinishAgentOperation(ctx, planID, op.ID, status, message, time.Now().UTC()); err != nil {
		return store.PlanRecord{}, fmt.Errorf("record text edit result: %w", err)
	}
	if err := s.removeTextPayload(op); err != nil {
		return store.PlanRecord{}, err
	}
	plan, err := s.store.Plan(ctx, planID)
	if err != nil {
		return store.PlanRecord{}, err
	}
	if status != store.OperationSucceeded {
		return plan, resultErr
	}
	return plan, nil
}

func (s *Service) verifyTextEditResult(op store.OperationRecord) error {
	entry, err := s.root.LstatEntry(textWriteTargetPath(op))
	if err != nil || entry.Symlink || entry.Kind != fsroot.KindFile || entry.Links != 1 || entry.Size != op.ProposedContentSize {
		return ErrConflict
	}
	digest, fingerprint, err := s.hashFile(textWriteTargetPath(op))
	if err != nil || !sameFileFingerprint(entry.Fingerprint(), fingerprint) || digest != op.ProposedContentSHA256 {
		return ErrConflict
	}
	return nil
}

func (s *Service) removeTextPayload(op store.OperationRecord) error {
	if !isTextFileWriteAction(op.Action) {
		return nil
	}
	return s.textPayloads.remove(op.ID)
}

func textWriteTargetPath(op store.OperationRecord) string {
	if op.Action == "create_file" {
		return op.TargetPath
	}
	return op.SourcePath
}

func textWriteSuccessMessage(action string) string {
	switch action {
	case "create_file":
		return "已按批准的候选创建文本文件"
	case "overwrite_file":
		return "已按批准的候选替换文件内容"
	default:
		return "文件内容已按批准的候选更新"
	}
}

func (s *Service) recordRenameOutcome(ctx context.Context, planID string, op store.OperationRecord, sourceDigest string, renameErr error) (store.PlanRecord, error) {
	status, message, resultErr := store.OperationSucceeded, "文件已重命名", renameErr
	switch {
	case sourceDigest != "" && s.verifyRenamedFile(op, sourceDigest) == nil:
		resultErr = nil
	case renameErr == nil:
		status, message, resultErr = store.OperationPartial, "重命名结果与批准的源文件状态不符，需人工核查", ErrConflict
	case s.verifyRenameNotApplied(op) == nil:
		status, message = store.OperationFailed, "重命名失败，文件未发生变化"
	default:
		status, message, resultErr = store.OperationPartial, "重命名结果无法确认，需人工核查", ErrConflict
	}
	if err := s.store.FinishAgentOperation(ctx, planID, op.ID, status, message, time.Now().UTC()); err != nil {
		return store.PlanRecord{}, fmt.Errorf("record rename outcome: %w", err)
	}
	plan, err := s.store.Plan(ctx, planID)
	if err != nil {
		return store.PlanRecord{}, err
	}
	return plan, resultErr
}

func (s *Service) verifyRenamedFile(op store.OperationRecord, sourceDigest string) error {
	target, err := s.root.LstatEntry(op.TargetPath)
	if err != nil || target.Symlink || target.Kind != fsroot.KindFile || !matchesRenameResult(target.Fingerprint(), op.SourceStateJSON) {
		return fmt.Errorf("rename target does not match approved source: %w", ErrConflict)
	}
	digest, fingerprint, err := s.hashFile(op.TargetPath)
	if err != nil || !matchesRenameResult(fingerprint, op.SourceStateJSON) || digest != sourceDigest {
		return fmt.Errorf("rename target content does not match approved source: %w", ErrConflict)
	}
	if _, err := s.root.LstatEntry(op.SourcePath); !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("rename source remains or cannot be checked: %w", ErrConflict)
	}
	return nil
}

func (s *Service) verifyRenameNotApplied(op store.OperationRecord) error {
	source, err := s.root.LstatEntry(op.SourcePath)
	if err != nil || source.Symlink || source.Kind != fsroot.KindFile || !matchesFingerprint(source.Fingerprint(), op.SourceStateJSON) {
		return ErrConflict
	}
	if _, err := s.root.LstatEntry(op.TargetPath); !errors.Is(err, fs.ErrNotExist) {
		return ErrConflict
	}
	return nil
}

// executeMoveFile 使用已审批的源快照执行不覆盖移动。
func (s *Service) executeMoveFile(ctx context.Context, planID string, op store.OperationRecord) (store.PlanRecord, error) {
	var sourceState fsroot.Fingerprint
	if err := json.Unmarshal([]byte(op.SourceStateJSON), &sourceState); err != nil {
		return store.PlanRecord{}, fmt.Errorf("decode approved move source snapshot: %w", err)
	}
	sourceDigest, hashErr := s.hashApprovedFile(op)
	if hashErr != nil {
		return s.recordMoveFileOutcome(ctx, planID, op, fsroot.FileMoveResult{}, "", hashErr)
	}
	outcome, moveErr := s.root.MoveFileNoReplace(ctx, op.SourcePath, op.TargetPath, sourceState)
	return s.recordMoveFileOutcome(ctx, planID, op, outcome, sourceDigest, moveErr)
}

// executeCopyFile 按已审批的文件快照复制并校验副本。
func (s *Service) executeCopyFile(ctx context.Context, planID string, op store.OperationRecord) (store.PlanRecord, error) {
	var sourceState fsroot.Fingerprint
	if err := json.Unmarshal([]byte(op.SourceStateJSON), &sourceState); err != nil {
		return s.recordCopyFileOutcome(ctx, planID, op, fsroot.FileCopyResult{}, fmt.Errorf("decode approved copy source snapshot: %w", err))
	}
	outcome, copyErr := s.root.CopyFileNoReplace(ctx, op.SourcePath, op.TargetPath, sourceState)
	return s.recordCopyFileOutcome(ctx, planID, op, outcome, copyErr)
}

// recordCopyFileOutcome 保存复制结果；已发布但无法确认的副本标记为部分完成。
func (s *Service) recordCopyFileOutcome(ctx context.Context, planID string, op store.OperationRecord, outcome fsroot.FileCopyResult, copyErr error) (store.PlanRecord, error) {
	status, message, resultErr := store.OperationFailed, "文件复制失败，未覆盖目标文件", copyErr
	switch {
	case outcome.Published && copyErr == nil:
		var verifyErr error
		if verifyErr = s.verifyCopiedFile(op, outcome.SHA256); verifyErr == nil {
			status, message, resultErr = store.OperationSucceeded, "文件已复制并通过内容校验", nil
		} else {
			status, message, resultErr = store.OperationPartial, "文件副本已发布，但无法确认最终状态，需人工核查", verifyErr
		}
	case outcome.Published:
		status, message = store.OperationPartial, "文件副本已发布，但持久化或状态校验未完成，需人工核查"
		if resultErr == nil {
			resultErr = ErrConflict
		}
	case copyErr == nil:
		status, message, resultErr = store.OperationPartial, "无法确认文件副本是否发布，需人工核查", ErrConflict
	}
	if err := s.store.FinishAgentOperation(ctx, planID, op.ID, status, message, time.Now().UTC()); err != nil {
		return store.PlanRecord{}, fmt.Errorf("record copy file outcome: %w", err)
	}
	plan, err := s.store.Plan(ctx, planID)
	if err != nil {
		return store.PlanRecord{}, err
	}
	return plan, resultErr
}

// verifyCopiedFile 确认副本位于批准的目录且内容、长度和权限与来源一致。
func (s *Service) verifyCopiedFile(op store.OperationRecord, expectedDigest string) error {
	targetParent, err := s.directoryParentEntry(parentPath(op.TargetPath))
	if err != nil || !matchesDirectoryIdentity(targetParent.Fingerprint(), op.TargetStateJSON) {
		return fmt.Errorf("copy target parent identity changed: %w", ErrConflict)
	}
	target, err := s.root.LstatEntry(op.TargetPath)
	if err != nil || target.Symlink || target.Kind != fsroot.KindFile || !target.IdentityAvailable {
		return fmt.Errorf("copy target cannot be verified: %w", ErrConflict)
	}
	var expected fsroot.Fingerprint
	if json.Unmarshal([]byte(op.SourceStateJSON), &expected) != nil || target.Size != expected.Size || target.Mode.Perm() != expected.Mode.Perm() {
		return fmt.Errorf("copy target metadata does not match approved source: %w", ErrConflict)
	}
	digest, opened, err := s.hashFile(op.TargetPath)
	if err != nil || !sameFileFingerprint(target.Fingerprint(), opened) || digest != expectedDigest {
		return fmt.Errorf("copy target content does not match copied source: %w", ErrConflict)
	}
	return nil
}

// executeCopyDirectory 按审批时记录的树摘要安全复制目录。
func (s *Service) executeCopyDirectory(ctx context.Context, planID string, op store.OperationRecord) (store.PlanRecord, error) {
	snapshot, targetParent, err := s.validateCopyDirectorySnapshot(ctx, Proposal{
		ScopePath: "", Action: op.Action, SourcePath: op.SourcePath, TargetPath: op.TargetPath,
	}, &op.SourceStateJSON, &op.TargetStateJSON)
	if err != nil {
		return s.recordCopyDirectoryOutcome(ctx, planID, op, fsroot.TreeSnapshot{}, fsroot.Entry{}, fsroot.TreeCopyResult{}, err)
	}
	outcome, copyErr := s.root.CopyTreeNoReplace(ctx, op.SourcePath, op.TargetPath, snapshot)
	return s.recordCopyDirectoryOutcome(ctx, planID, op, snapshot, targetParent, outcome, copyErr)
}

// recordCopyDirectoryOutcome 记录目录复制结果，不保存文件正文。
func (s *Service) recordCopyDirectoryOutcome(ctx context.Context, planID string, op store.OperationRecord, snapshot fsroot.TreeSnapshot, targetParent fsroot.Entry, outcome fsroot.TreeCopyResult, copyErr error) (store.PlanRecord, error) {
	status, message, resultErr := store.OperationFailed, "文件夹复制失败，未覆盖目标路径", copyErr
	switch {
	case outcome.Published && copyErr == nil:
		if err := s.verifyCopiedDirectory(ctx, op, snapshot, targetParent, outcome); err != nil {
			status, message, resultErr = store.OperationPartial, "文件夹副本已发布，但无法确认最终状态，需人工核查", err
		} else {
			status = store.OperationSucceeded
			message = fmt.Sprintf("文件夹已复制：%d 个文件，%d 字节", outcome.FilesCopied, outcome.BytesCopied)
			resultErr = nil
		}
	case outcome.Published:
		status, message = store.OperationPartial, "文件夹副本已发布，但持久化或状态校验未完成，需人工核查"
		if resultErr == nil {
			resultErr = ErrConflict
		}
	case copyErr == nil:
		status, message, resultErr = store.OperationPartial, "无法确认文件夹副本是否发布，需人工核查", ErrConflict
	}
	if err := s.store.FinishAgentOperation(ctx, planID, op.ID, status, message, time.Now().UTC()); err != nil {
		return store.PlanRecord{}, fmt.Errorf("record copy directory outcome: %w", err)
	}
	plan, err := s.store.Plan(ctx, planID)
	if err != nil {
		return store.PlanRecord{}, err
	}
	return plan, resultErr
}

// verifyCopiedDirectory 确认目录副本已发布到批准的目标目录。
func (s *Service) verifyCopiedDirectory(ctx context.Context, op store.OperationRecord, snapshot fsroot.TreeSnapshot, targetParent fsroot.Entry, outcome fsroot.TreeCopyResult) error {
	currentParent, err := s.directoryParentEntry(parentPath(op.TargetPath))
	if err != nil || !matchesDirectoryIdentity(currentParent.Fingerprint(), op.TargetStateJSON) || !matchesDirectoryIdentity(targetParent.Fingerprint(), op.TargetStateJSON) {
		return fmt.Errorf("copy directory target parent identity changed: %w", ErrConflict)
	}
	target, err := s.root.LstatEntry(op.TargetPath)
	if err != nil || target.Symlink || target.Kind != fsroot.KindDirectory {
		return fmt.Errorf("copied directory cannot be verified: %w", ErrConflict)
	}
	targetSnapshot, err := s.root.SnapshotTree(ctx, op.TargetPath)
	if err != nil || validateAgentCopyTree(targetSnapshot) != nil || len(targetSnapshot.Entries) != len(snapshot.Entries) {
		return fmt.Errorf("copied directory tree cannot be verified: %w", ErrConflict)
	}
	for index, sourceItem := range snapshot.Entries {
		copiedItem := targetSnapshot.Entries[index]
		relative := strings.TrimPrefix(sourceItem.Entry.Path, snapshot.RootPath)
		if copiedItem.Entry.Path != op.TargetPath+relative || copiedItem.Entry.Kind != sourceItem.Entry.Kind ||
			copiedItem.Entry.Size != sourceItem.Entry.Size || copiedItem.Entry.Mode.Perm() != sourceItem.Entry.Mode.Perm() {
			return fmt.Errorf("copied directory entry does not match source snapshot: %w", ErrConflict)
		}
		if sourceItem.Entry.Kind == fsroot.KindFile {
			expectedDigest, ok := outcome.FileDigests[sourceItem.Entry.Path]
			if !ok {
				return fmt.Errorf("copied directory file digest is missing: %w", ErrConflict)
			}
			actualDigest, _, hashErr := s.hashFile(copiedItem.Entry.Path)
			if hashErr != nil || actualDigest != expectedDigest {
				return fmt.Errorf("copied directory file content does not match: %w", ErrConflict)
			}
		}
	}
	files, bytes := treeCopyTotals(snapshot)
	if outcome.FilesCopied != files || outcome.BytesCopied != bytes || len(outcome.FileDigests) != files {
		return fmt.Errorf("copied directory totals do not match approved source: %w", ErrConflict)
	}
	finalSnapshot, err := s.root.SnapshotTree(ctx, op.TargetPath)
	if err != nil || finalSnapshot.Digest != targetSnapshot.Digest {
		return fmt.Errorf("copied directory changed during final verification: %w", ErrConflict)
	}
	return nil
}

func treeCopyTotals(snapshot fsroot.TreeSnapshot) (int, int64) {
	var files int
	var bytes int64
	for _, item := range snapshot.Entries {
		if item.Entry.Kind == fsroot.KindFile {
			files++
			bytes += item.Entry.Size
		}
	}
	return files, bytes
}

func (s *Service) executeMetadata(ctx context.Context, planID string, op store.OperationRecord) (store.PlanRecord, error) {
	var expected fsroot.Fingerprint
	if err := json.Unmarshal([]byte(op.SourceStateJSON), &expected); err != nil {
		return store.PlanRecord{}, fmt.Errorf("decode approved metadata snapshot: %w", err)
	}
	update := fsroot.MetadataUpdate{}
	if op.Action == "set_permissions" {
		mode := fs.FileMode(*op.Mode)
		update.Mode = &mode
	} else {
		update.OwnerUID, update.GroupGID = op.OwnerUID, op.GroupGID
	}
	updated, updateErr := s.root.UpdateMetadata(op.SourcePath, expected, update)
	if updateErr == nil {
		var verifyErr error
		updated, verifyErr = s.root.LstatEntry(op.SourcePath)
		if verifyErr != nil || !matchesMetadataTarget(updated.Fingerprint(), expected, op) {
			updateErr = fmt.Errorf("metadata result could not be verified: %w", ErrConflict)
		}
	}
	if updateErr != nil {
		status, message := store.OperationFailed, "元数据修改失败，文件状态未确认变化"
		if errors.Is(updateErr, fsroot.ErrMetadataNeedsReview) {
			status, message = store.OperationPartial, "元数据可能已修改，无法确认最终状态，需人工核查"
		} else if errors.Is(updateErr, ErrConflict) || errors.Is(updateErr, fsroot.ErrConflict) || errors.Is(updateErr, fsroot.ErrPathChanged) {
			status, message = store.OperationPartial, "元数据操作结果无法确认，需人工核查文件状态"
		}
		if finishErr := s.store.FinishAgentOperation(ctx, planID, op.ID, status, message, time.Now().UTC()); finishErr != nil {
			return store.PlanRecord{}, fmt.Errorf("record Agent metadata failure: %w", finishErr)
		}
		plan, err := s.store.Plan(ctx, planID)
		if err != nil {
			return store.PlanRecord{}, err
		}
		return plan, updateErr
	}
	message := metadataResultMessage(expected, updated.Fingerprint(), op)
	if err := s.store.FinishAgentOperation(ctx, planID, op.ID, store.OperationSucceeded, message, time.Now().UTC()); err != nil {
		return store.PlanRecord{}, fmt.Errorf("metadata updated but result persistence failed: %w", err)
	}
	return s.store.Plan(ctx, planID)
}

func metadataResultMessage(before, after fsroot.Fingerprint, op store.OperationRecord) string {
	if op.Action == "set_permissions" {
		return fmt.Sprintf("权限 mode：%04o → %04o", before.Mode.Perm(), after.Mode.Perm())
	}
	changes := make([]string, 0, 2)
	if op.OwnerUID != nil {
		changes = append(changes, fmt.Sprintf("UID：%d → %d", before.OwnerUID, after.OwnerUID))
	}
	if op.GroupGID != nil {
		changes = append(changes, fmt.Sprintf("GID：%d → %d", before.GroupGID, after.GroupGID))
	}
	return "属主 " + strings.Join(changes, "，")
}

func matchesMetadataTarget(actual, expected fsroot.Fingerprint, op store.OperationRecord) bool {
	if actual.Exists != expected.Exists || actual.Kind != expected.Kind || actual.Size != expected.Size ||
		!actual.ModifiedAt.Equal(expected.ModifiedAt) || actual.Device != expected.Device || actual.Inode != expected.Inode ||
		actual.Links != expected.Links || !actual.IdentityAvailable || !actual.OwnershipAvailable ||
		!actual.ChangeTimeAvailable || actual.IdentityAvailable != expected.IdentityAvailable ||
		actual.OwnershipAvailable != expected.OwnershipAvailable || actual.ChangeTimeAvailable != expected.ChangeTimeAvailable ||
		hasSpecialModeBits(actual.Mode) {
		return false
	}
	if op.Action == "set_permissions" {
		if op.Mode == nil {
			return false
		}
		targetMode := expected.Mode&fs.ModeType | fs.FileMode(*op.Mode)
		return actual.Mode == targetMode &&
			actual.OwnerUID == expected.OwnerUID && actual.GroupGID == expected.GroupGID
	}
	modeUnchanged := actual.Mode == expected.Mode
	uid := expected.OwnerUID
	if op.OwnerUID != nil {
		uid = *op.OwnerUID
	}
	gid := expected.GroupGID
	if op.GroupGID != nil {
		gid = *op.GroupGID
	}
	return modeUnchanged && actual.OwnerUID == uid && actual.GroupGID == gid
}

func (s *Service) lockOperation(ctx context.Context, op store.OperationRecord) (func(), error) {
	keys := coordination.PathKeys(op.SourcePath, op.TargetPath)
	if isArchiveAction(op.Action) {
		var snapshot archiveSourceState
		if json.Unmarshal([]byte(op.SourceStateJSON), &snapshot) != nil || !snapshot.Fingerprint.Exists || !snapshot.Fingerprint.IdentityAvailable || snapshot.Fingerprint.Inode == 0 {
			return nil, ErrInvalid
		}
		if snapshot.Fingerprint.Kind != fsroot.KindDirectory {
			keys = append(keys, coordination.InodeKey(snapshot.Fingerprint.Device, snapshot.Fingerprint.Inode))
		}
		return s.locks.Lock(ctx, keys...)
	}
	if op.Action == "trash" || op.Action == "permanent_delete" {
		keys = coordination.PathKeys(op.SourcePath)
		var snapshot trashOperationState
		if json.Unmarshal([]byte(op.SourceStateJSON), &snapshot) != nil {
			return nil, ErrInvalid
		}
		if snapshot.Symlink {
			if !snapshot.LinkIdentityAvailable || snapshot.LinkInode == 0 {
				return nil, ErrInvalid
			}
			keys = append(keys, coordination.InodeKey(snapshot.LinkDevice, snapshot.LinkInode))
		} else {
			if !snapshot.Fingerprint.IdentityAvailable || snapshot.Fingerprint.Inode == 0 {
				return nil, ErrInvalid
			}
			keys = append(keys, coordination.InodeKey(snapshot.Fingerprint.Device, snapshot.Fingerprint.Inode))
		}
		return s.locks.Lock(ctx, keys...)
	}
	if op.Action == "trash_duplicate" {
		var snapshot duplicateCleanupState
		if json.Unmarshal([]byte(op.SourceStateJSON), &snapshot) != nil ||
			snapshot.SourcePath != op.SourcePath || snapshot.KeeperPath != op.TargetPath ||
			!snapshot.Source.IdentityAvailable || snapshot.Source.Inode == 0 ||
			!snapshot.Keeper.IdentityAvailable || snapshot.Keeper.Inode == 0 {
			return nil, ErrInvalid
		}
		keys = append(keys,
			coordination.InodeKey(snapshot.Source.Device, snapshot.Source.Inode),
			coordination.InodeKey(snapshot.Keeper.Device, snapshot.Keeper.Inode),
		)
		return s.locks.Lock(ctx, keys...)
	}
	if needsSourceInodeLock(op.Action) {
		var snapshot fsroot.Fingerprint
		if json.Unmarshal([]byte(op.SourceStateJSON), &snapshot) != nil || !snapshot.IdentityAvailable || snapshot.Inode == 0 {
			return nil, ErrInvalid
		}
		if snapshot.Kind != fsroot.KindDirectory || isMetadataAction(op.Action) {
			keys = append(keys, coordination.InodeKey(snapshot.Device, snapshot.Inode))
		}
	}
	return s.locks.Lock(ctx, keys...)
}

func needsSourceInodeLock(action string) bool {
	return action == "rename" || action == "edit_text" || action == "overwrite_file" || isFileTargetAction(action) || action == "create_symlink" || isMetadataAction(action) || action == "trash_duplicate" || action == "archive_photo" || action == "create_archive" || action == "extract_archive" || action == "trash" || action == "permanent_delete"
}

func agentInodeKey(entry fsroot.Entry) string {
	if entry.Symlink {
		if entry.LinkIdentityAvailable && entry.LinkInode != 0 {
			return coordination.InodeKey(entry.LinkDevice, entry.LinkInode)
		}
		return ""
	}
	if entry.IdentityAvailable && entry.Inode != 0 {
		return coordination.InodeKey(entry.Device, entry.Inode)
	}
	return ""
}

func isFileTargetAction(action string) bool {
	return action == "move_file" || action == "copy_file"
}

// fileMoveWording 按源和目标父目录区分重命名与移动文案。
func fileMoveWording(sourcePath, targetPath string) (string, string) {
	if parentPath(sourcePath) == parentPath(targetPath) {
		return "重命名", "文件已重命名"
	}
	return "移动", "文件已移动"
}

// fileMoveReason 让方案理由与同目录重命名或跨目录移动保持一致。
func fileMoveReason(sourcePath, targetPath string) string {
	action, _ := fileMoveWording(sourcePath, targetPath)
	return "按用户确认的目标路径" + action + "文件"
}

func isArchiveAction(action string) bool {
	return action == "create_archive" || action == "extract_archive"
}

func isMetadataAction(action string) bool {
	return action == "set_permissions" || action == "set_owner"
}

// hasSpecialModeBits 检查 mode 中不会被审批目标覆盖的特殊位。
func hasSpecialModeBits(mode fs.FileMode) bool {
	return mode&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) != 0
}

// recordMoveFileOutcome 将移动结果写入操作状态和审计记录。
func (s *Service) recordMoveFileOutcome(ctx context.Context, planID string, op store.OperationRecord, outcome fsroot.FileMoveResult, sourceDigest string, moveErr error) (store.PlanRecord, error) {
	action, successMessage := fileMoveWording(op.SourcePath, op.TargetPath)
	status, message, resultErr := store.OperationSucceeded, successMessage, moveErr
	switch {
	case outcome.Published && outcome.SourceRemoved && moveErr == nil:
		if err := s.verifyMovedFile(op, sourceDigest, outcome.SHA256); err != nil {
			status, message, resultErr = store.OperationPartial, successMessage+"，但目标路径身份无法确认；需人工核查", err
		}
	case outcome.Published || outcome.SourceRemoved:
		status, message = store.OperationPartial, action+"结果不完整；请核查源文件和目标文件"
		if resultErr == nil {
			resultErr = ErrConflict
		}
	case moveErr != nil:
		status, message = store.OperationFailed, "文件"+action+"失败，未覆盖目标文件"
	default:
		status, message, resultErr = store.OperationPartial, "无法确认文件"+action+"结果；需人工核查", ErrConflict
	}
	if finishErr := s.store.FinishAgentOperation(ctx, planID, op.ID, status, message, time.Now().UTC()); finishErr != nil {
		return store.PlanRecord{}, fmt.Errorf("record move file outcome: %w", finishErr)
	}
	plan, err := s.store.Plan(ctx, planID)
	if err != nil {
		return store.PlanRecord{}, err
	}
	return plan, resultErr
}

// verifyMovedFile 确认移动目标内容、身份和源路径状态。
func (s *Service) verifyMovedFile(op store.OperationRecord, sourceDigest, copiedDigest string) error {
	targetParent, err := s.directoryParentEntry(parentPath(op.TargetPath))
	if err != nil || !matchesDirectoryIdentity(targetParent.Fingerprint(), op.TargetStateJSON) {
		return fmt.Errorf("move target parent identity changed: %w", ErrConflict)
	}
	target, err := s.root.LstatEntry(op.TargetPath)
	if err != nil || target.Symlink || target.Kind != fsroot.KindFile || !target.IdentityAvailable {
		return fmt.Errorf("move target cannot be verified: %w", ErrConflict)
	}
	var expected fsroot.Fingerprint
	if json.Unmarshal([]byte(op.SourceStateJSON), &expected) != nil || target.Size != expected.Size {
		return fmt.Errorf("move target size does not match approved source: %w", ErrConflict)
	}
	sameFile := matchesRenameResult(target.Fingerprint(), op.SourceStateJSON)
	if !sameFile && (copiedDigest == "" || copiedDigest != sourceDigest) {
		return fmt.Errorf("cross-volume copy digest does not match approved source: %w", ErrConflict)
	}
	if !sameFile && target.Mode.Perm() != expected.Mode.Perm() {
		return fmt.Errorf("cross-volume target permissions do not match approved source: %w", ErrConflict)
	}
	digest, openedFingerprint, err := s.hashFile(op.TargetPath)
	if err != nil || !sameFileFingerprint(target.Fingerprint(), openedFingerprint) || digest != sourceDigest {
		return fmt.Errorf("move target content could not be verified: %w", ErrConflict)
	}
	if _, err := s.root.LstatEntry(op.SourcePath); !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("move source remains or cannot be checked: %w", ErrConflict)
	}
	return nil
}

// hashApprovedFile 仅在源文件仍匹配审批快照时返回内容摘要。
func (s *Service) hashApprovedFile(op store.OperationRecord) (string, error) {
	digest, fingerprint, err := s.hashFile(op.SourcePath)
	if err != nil || !matchesFingerprint(fingerprint, op.SourceStateJSON) {
		return "", fmt.Errorf("approved source changed before content verification: %w", ErrConflict)
	}
	return digest, nil
}

// snapshotDuplicateCleanup 复核成功扫描组，并绑定保留文件与副本的当前状态。
func (s *Service) snapshotDuplicateCleanup(ctx context.Context, proposal Proposal, expected *duplicateCleanupState) (duplicateCleanupState, fsroot.Entry, fsroot.Entry, error) {
	if s.store == nil || s.trash == nil || proposal.Action != "trash_duplicate" {
		return duplicateCleanupState{}, fsroot.Entry{}, fsroot.Entry{}, ErrUnsupported
	}
	scan, err := s.store.DuplicateScan(ctx, proposal.DuplicateScanID)
	if err != nil || scan.Status != store.DuplicateScanSucceeded || scan.ScopePath != proposal.ScopePath {
		return duplicateCleanupState{}, fsroot.Entry{}, fsroot.Entry{}, ErrConflict
	}
	if scan.ScopePath != "" {
		scope, scopeErr := s.root.LstatEntry(scan.ScopePath)
		if scopeErr != nil || scope.Symlink || scope.Kind != fsroot.KindDirectory {
			return duplicateCleanupState{}, fsroot.Entry{}, fsroot.Entry{}, ErrConflict
		}
	}
	files, err := s.store.DuplicateScanFilesByPaths(ctx, proposal.DuplicateScanID, proposal.DuplicateGroupID, []string{proposal.SourcePath, proposal.TargetPath})
	if err != nil || len(files) != 2 {
		return duplicateCleanupState{}, fsroot.Entry{}, fsroot.Entry{}, ErrConflict
	}
	byPath := make(map[string]store.DuplicateScanFile, len(files))
	for _, file := range files {
		byPath[file.Path] = file
	}
	sourceScan, sourceExists := byPath[proposal.SourcePath]
	keeperScan, keeperExists := byPath[proposal.TargetPath]
	if !sourceExists || !keeperExists {
		return duplicateCleanupState{}, fsroot.Entry{}, fsroot.Entry{}, ErrConflict
	}
	source, err := s.root.LstatEntry(proposal.SourcePath)
	if err != nil || source.Symlink || source.Kind != fsroot.KindFile || !source.IdentityAvailable || !source.ChangeTimeAvailable {
		return duplicateCleanupState{}, fsroot.Entry{}, fsroot.Entry{}, ErrConflict
	}
	keeper, err := s.root.LstatEntry(proposal.TargetPath)
	if err != nil || keeper.Symlink || keeper.Kind != fsroot.KindFile || !keeper.IdentityAvailable || !keeper.ChangeTimeAvailable || agentInodeKey(source) == agentInodeKey(keeper) {
		return duplicateCleanupState{}, fsroot.Entry{}, fsroot.Entry{}, ErrConflict
	}
	if source.Size != sourceScan.SizeBytes || !source.ModifiedAt.Equal(sourceScan.ModifiedAt) ||
		keeper.Size != keeperScan.SizeBytes || !keeper.ModifiedAt.Equal(keeperScan.ModifiedAt) {
		return duplicateCleanupState{}, fsroot.Entry{}, fsroot.Entry{}, ErrConflict
	}
	state := duplicateCleanupState{
		ScanID: proposal.DuplicateScanID, GroupID: proposal.DuplicateGroupID,
		SourcePath: proposal.SourcePath, KeeperPath: proposal.TargetPath,
		Source: source.Fingerprint(), Keeper: keeper.Fingerprint(),
	}
	if expected != nil {
		if expected.ScanID != state.ScanID || expected.GroupID != state.GroupID || expected.SourcePath != state.SourcePath ||
			expected.KeeperPath != state.KeeperPath || len(expected.SHA256) != 64 ||
			!sameFileFingerprint(state.Source, expected.Source) || !sameFileFingerprint(state.Keeper, expected.Keeper) {
			return duplicateCleanupState{}, fsroot.Entry{}, fsroot.Entry{}, ErrConflict
		}
		if _, err := hex.DecodeString(expected.SHA256); err != nil {
			return duplicateCleanupState{}, fsroot.Entry{}, fsroot.Entry{}, ErrConflict
		}
		state.SHA256 = expected.SHA256
		return state, source, keeper, nil
	}
	sourceDigest, sourceFingerprint, err := s.hashFile(proposal.SourcePath)
	if err != nil || !sameFileFingerprint(state.Source, sourceFingerprint) {
		return duplicateCleanupState{}, fsroot.Entry{}, fsroot.Entry{}, ErrConflict
	}
	keeperDigest, keeperFingerprint, err := s.hashFile(proposal.TargetPath)
	if err != nil || !sameFileFingerprint(state.Keeper, keeperFingerprint) || sourceDigest != keeperDigest {
		return duplicateCleanupState{}, fsroot.Entry{}, fsroot.Entry{}, ErrConflict
	}
	state.SHA256 = sourceDigest
	return state, source, keeper, nil
}

// snapshotTrash 只绑定条目元数据；目录额外绑定整棵树的状态摘要。
func (s *Service) snapshotTrash(ctx context.Context, proposal Proposal, expected *trashOperationState) (trashOperationState, fsroot.Entry, error) {
	if s.trash == nil || (proposal.Action != "trash" && proposal.Action != "permanent_delete") || !withinScope(proposal.ScopePath, proposal.SourcePath) || reservedPath(proposal.SourcePath) {
		return trashOperationState{}, fsroot.Entry{}, ErrUnsupported
	}
	if proposal.ScopePath != "" {
		scope, err := s.root.LstatEntry(proposal.ScopePath)
		if err != nil || scope.Symlink || scope.Kind != fsroot.KindDirectory {
			return trashOperationState{}, fsroot.Entry{}, ErrConflict
		}
	}
	entry, err := s.root.LstatEntry(proposal.SourcePath)
	if err != nil {
		return trashOperationState{}, fsroot.Entry{}, ErrConflict
	}
	state := trashOperationState{}
	if entry.Symlink {
		if !entry.LinkIdentityAvailable || entry.LinkInode == 0 || entry.LinkTarget == "" {
			return trashOperationState{}, fsroot.Entry{}, ErrConflict
		}
		state.Symlink = true
		state.LinkDevice, state.LinkInode, state.LinkLinks = entry.LinkDevice, entry.LinkInode, entry.LinkLinks
		state.LinkIdentityAvailable, state.LinkTarget = entry.LinkIdentityAvailable, entry.LinkTarget
	} else {
		if (entry.Kind != fsroot.KindFile && entry.Kind != fsroot.KindDirectory) || !entry.IdentityAvailable || entry.Inode == 0 || !entry.ChangeTimeAvailable {
			return trashOperationState{}, fsroot.Entry{}, ErrConflict
		}
		state.Fingerprint = entry.Fingerprint()
		if entry.Kind == fsroot.KindDirectory {
			mountID, mountErr := s.root.MountID(proposal.SourcePath)
			if mountErr != nil || s.root.CheckTreeMounts(proposal.SourcePath, mountID) != nil {
				return trashOperationState{}, fsroot.Entry{}, ErrConflict
			}
			tree, snapshotErr := s.root.SnapshotTree(ctx, proposal.SourcePath)
			if snapshotErr != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return trashOperationState{}, fsroot.Entry{}, ctxErr
				}
				return trashOperationState{}, fsroot.Entry{}, fmt.Errorf("snapshot trash directory: %w", ErrConflict)
			}
			state.TreeDigest = tree.Digest
		}
	}
	if expected != nil && !sameTrashOperationState(state, *expected) {
		return trashOperationState{}, fsroot.Entry{}, ErrConflict
	}
	return state, entry, nil
}

func sameTrashOperationState(actual, expected trashOperationState) bool {
	if actual.Symlink != expected.Symlink {
		return false
	}
	if expected.Symlink {
		return actual.LinkIdentityAvailable && expected.LinkIdentityAvailable && actual.LinkDevice == expected.LinkDevice &&
			actual.LinkInode == expected.LinkInode && actual.LinkLinks == expected.LinkLinks && actual.LinkTarget == expected.LinkTarget
	}
	if !matchesFingerprint(actual.Fingerprint, mustJSON(expected.Fingerprint)) || actual.TreeDigest != expected.TreeDigest {
		return false
	}
	return actual.Fingerprint.Kind != fsroot.KindDirectory || len(expected.TreeDigest) == 64
}

// executeTrash 将已批准条目移入可恢复回收站并关联任务审计。
func (s *Service) executeTrash(ctx context.Context, planID string, op store.OperationRecord) (store.PlanRecord, error) {
	var expected trashOperationState
	if json.Unmarshal([]byte(op.SourceStateJSON), &expected) != nil {
		return store.PlanRecord{}, ErrConflict
	}
	plan, err := s.store.Plan(ctx, planID)
	if err != nil || plan.TaskID == "" {
		return store.PlanRecord{}, ErrConflict
	}
	proposal := Proposal{ScopePath: plan.ScopePath, Action: op.Action, SourcePath: op.SourcePath, Reason: op.Reason, Evidence: op.Evidence}
	state, source, err := s.snapshotTrash(ctx, proposal, &expected)
	if err != nil || !sameTrashOperationState(state, expected) {
		return s.finishAgentTrash(ctx, planID, op, store.OperationFailed, "原项目状态已变化，未移入回收站", ErrConflict)
	}
	verifyBeforeMove := func() error {
		_, _, verifyErr := s.snapshotTrash(ctx, proposal, &expected)
		return verifyErr
	}
	item, moveErr := s.trash.MoveToTrashForAgent(ctx, source, plan.TaskID, planID, op.ID, verifyBeforeMove)
	if moveErr != nil {
		_, _, checkErr := s.snapshotTrash(ctx, proposal, &expected)
		if checkErr == nil {
			return s.finishAgentTrash(ctx, planID, op, store.OperationFailed, "移入回收站失败，原项目仍在", moveErr)
		}
		return s.finishAgentTrash(ctx, planID, op, store.OperationPartial, "回收站操作结果不完整，需人工核查原路径与回收站", moveErr)
	}
	_, sourceErr := s.root.LstatEntry(op.SourcePath)
	if item.OriginalPath != op.SourcePath || item.Status != string(store.TrashTrashed) || !errors.Is(sourceErr, fs.ErrNotExist) {
		return s.finishAgentTrash(ctx, planID, op, store.OperationPartial, "项目已移入回收站，但结果需人工核查", ErrConflict)
	}
	return s.finishAgentTrash(ctx, planID, op, store.OperationSucceeded, "项目已移入可恢复回收站", nil)
}

// executePermanentDelete 先登记到回收区，再 no-follow 永久清理并关联完整审计。
func (s *Service) executePermanentDelete(ctx context.Context, planID string, op store.OperationRecord) (store.PlanRecord, error) {
	var expected trashOperationState
	if json.Unmarshal([]byte(op.SourceStateJSON), &expected) != nil {
		return store.PlanRecord{}, ErrConflict
	}
	plan, err := s.store.Plan(ctx, planID)
	if err != nil || plan.TaskID == "" {
		return store.PlanRecord{}, ErrConflict
	}
	proposal := Proposal{ScopePath: plan.ScopePath, Action: op.Action, SourcePath: op.SourcePath, Reason: op.Reason, Evidence: op.Evidence}
	state, source, err := s.snapshotTrash(ctx, proposal, &expected)
	if err != nil || !sameTrashOperationState(state, expected) {
		return s.finishAgentTrash(ctx, planID, op, store.OperationFailed, "原项目状态已变化，未执行永久删除", ErrConflict)
	}
	verifyBeforeMove := func() error {
		_, _, verifyErr := s.snapshotTrash(ctx, proposal, &expected)
		return verifyErr
	}
	item, moveErr := s.trash.MoveToTrashForAgent(ctx, source, plan.TaskID, planID, op.ID, verifyBeforeMove)
	if moveErr != nil {
		_, _, checkErr := s.snapshotTrash(ctx, proposal, &expected)
		if checkErr == nil {
			return s.finishAgentTrash(ctx, planID, op, store.OperationFailed, "永久删除未开始，原项目仍在", moveErr)
		}
		return s.finishAgentTrash(ctx, planID, op, store.OperationPartial, "永久删除状态不确定，需核查原路径与回收区", moveErr)
	}
	_, sourceErr := s.root.LstatEntry(op.SourcePath)
	if item.Status != string(store.TrashTrashed) || item.OriginalPath != op.SourcePath || !errors.Is(sourceErr, fs.ErrNotExist) {
		return s.finishAgentTrash(ctx, planID, op, store.OperationPartial, "项目已移出原路径，但永久删除前状态需核查", ErrConflict)
	}
	if err := s.trash.PurgeForAgent(ctx, item.ID, plan.TaskID, planID, op.ID); err != nil {
		return s.finishAgentTrash(ctx, planID, op, store.OperationPartial, "永久删除结果不确定，需人工核查回收区", err)
	}
	if _, sourceErr := s.root.LstatEntry(op.SourcePath); !errors.Is(sourceErr, fs.ErrNotExist) {
		return s.finishAgentTrash(ctx, planID, op, store.OperationPartial, "永久删除后原路径状态无法确认，需人工核查", ErrConflict)
	}
	return s.finishAgentTrash(ctx, planID, op, store.OperationSucceeded, "项目已永久删除，无法恢复", nil)
}

func (s *Service) finishAgentTrash(ctx context.Context, planID string, op store.OperationRecord, status store.OperationStatus, message string, operationErr error) (store.PlanRecord, error) {
	if err := s.store.FinishAgentOperation(ctx, planID, op.ID, status, message, time.Now().UTC()); err != nil {
		return store.PlanRecord{}, fmt.Errorf("record agent trash outcome: %w", err)
	}
	plan, err := s.store.Plan(ctx, planID)
	if err != nil {
		return store.PlanRecord{}, err
	}
	return plan, operationErr
}

// executeDuplicateTrash 执行单项已审批的可恢复清理。
func (s *Service) executeDuplicateTrash(ctx context.Context, planID string, op store.OperationRecord) (store.PlanRecord, error) {
	var state duplicateCleanupState
	if json.Unmarshal([]byte(op.SourceStateJSON), &state) != nil {
		return store.PlanRecord{}, ErrConflict
	}
	plan, err := s.store.Plan(ctx, planID)
	if err != nil || plan.TaskID == "" {
		return store.PlanRecord{}, ErrConflict
	}
	source, err := s.root.LstatEntry(op.SourcePath)
	if err != nil || !sameFileFingerprint(source.Fingerprint(), state.Source) {
		return s.finishDuplicateTrash(ctx, planID, op, store.OperationFailed, "原文件状态已变化，未移入回收站", ErrConflict)
	}
	item, moveErr := s.trash.MoveToTrashForAgent(ctx, source, plan.TaskID, planID, op.ID, nil)
	if moveErr != nil {
		sourceAfter, sourceErr := s.root.LstatEntry(op.SourcePath)
		keeperAfter, keeperErr := s.root.LstatEntry(op.TargetPath)
		if sourceErr != nil || keeperErr != nil || !sameFileFingerprint(sourceAfter.Fingerprint(), state.Source) || !sameFileFingerprint(keeperAfter.Fingerprint(), state.Keeper) {
			return s.finishDuplicateTrash(ctx, planID, op, store.OperationPartial, "回收站操作结果不完整，需核查源文件与回收站", moveErr)
		}
		return s.finishDuplicateTrash(ctx, planID, op, store.OperationFailed, "移入回收站失败，原文件仍在", moveErr)
	}
	keeperAfter, keeperErr := s.root.LstatEntry(op.TargetPath)
	_, sourceErr := s.root.LstatEntry(op.SourcePath)
	if item.OriginalPath != op.SourcePath || item.Status != string(store.TrashTrashed) || keeperErr != nil ||
		!sameFileFingerprint(keeperAfter.Fingerprint(), state.Keeper) || !errors.Is(sourceErr, fs.ErrNotExist) {
		return s.finishDuplicateTrash(ctx, planID, op, store.OperationPartial, "文件已移入回收站，但保留文件状态需人工核查", ErrConflict)
	}
	return s.finishDuplicateTrash(ctx, planID, op, store.OperationSucceeded, "副本已移入回收站，可在回收站恢复", nil)
}

func (s *Service) finishDuplicateTrash(ctx context.Context, planID string, op store.OperationRecord, status store.OperationStatus, message string, operationErr error) (store.PlanRecord, error) {
	if err := s.store.FinishAgentOperation(ctx, planID, op.ID, status, message, time.Now().UTC()); err != nil {
		return store.PlanRecord{}, fmt.Errorf("record duplicate cleanup outcome: %w", err)
	}
	plan, err := s.store.Plan(ctx, planID)
	if err != nil {
		return store.PlanRecord{}, err
	}
	return plan, operationErr
}

// hashFile 通过根目录句柄读取文件，并确认读取期间路径身份未变化。
func (s *Service) hashFile(relativePath string) (string, fsroot.Fingerprint, error) {
	before, err := s.root.LstatEntry(relativePath)
	if err != nil || before.Symlink || before.Kind != fsroot.KindFile || !before.IdentityAvailable {
		return "", fsroot.Fingerprint{}, fmt.Errorf("file cannot be safely hashed: %w", ErrConflict)
	}
	reader, opened, err := s.root.OpenRead(relativePath)
	if err != nil {
		return "", fsroot.Fingerprint{}, err
	}
	openedFingerprint := opened.Fingerprint()
	if opened.Symlink || opened.Kind != fsroot.KindFile || !sameFileFingerprint(before.Fingerprint(), openedFingerprint) {
		_ = reader.Close()
		return "", fsroot.Fingerprint{}, fmt.Errorf("file changed before hashing: %w", ErrConflict)
	}
	hasher := sha256.New()
	count, readErr := io.Copy(hasher, reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || count != opened.Size {
		return "", fsroot.Fingerprint{}, fmt.Errorf("file content could not be read completely: %w", ErrConflict)
	}
	after, err := s.root.LstatEntry(relativePath)
	if err != nil || after.Symlink || after.Kind != fsroot.KindFile || !sameFileFingerprint(openedFingerprint, after.Fingerprint()) {
		return "", fsroot.Fingerprint{}, fmt.Errorf("file changed while hashing: %w", ErrConflict)
	}
	return hex.EncodeToString(hasher.Sum(nil)), openedFingerprint, nil
}

// sameFileFingerprint 比较读取期间文件的完整状态。
func sameFileFingerprint(left, right fsroot.Fingerprint) bool {
	return left.Exists == right.Exists && left.Kind == right.Kind && left.Size == right.Size &&
		left.ModifiedAt.Equal(right.ModifiedAt) && left.Mode == right.Mode && left.Device == right.Device &&
		left.Inode == right.Inode && left.Links == right.Links && left.IdentityAvailable == right.IdentityAvailable &&
		left.OwnerUID == right.OwnerUID && left.GroupGID == right.GroupGID && left.OwnershipAvailable == right.OwnershipAvailable &&
		left.ChangeTimeAvailable == right.ChangeTimeAvailable && left.ChangeTime.Equal(right.ChangeTime)
}

// Recover 检查中断的文件操作，不会自动重放不确定操作。
func (s *Service) Recover(ctx context.Context) error {
	plans, err := s.store.RunningAgentPlans(ctx)
	if err != nil {
		return err
	}
	for _, plan := range plans {
		if len(plan.Operations) < 1 || len(plan.Operations) > maxStoredPlanOperations(plan.Operations) {
			continue
		}
		var running *store.OperationRecord
		for index := range plan.Operations {
			if plan.Operations[index].Status == store.OperationRunning {
				if running != nil {
					return fmt.Errorf("plan %s has multiple running operations: %w", plan.ID, ErrConflict)
				}
				running = &plan.Operations[index]
			}
		}
		if running == nil {
			if err := s.store.RecoverIdleAgentPlan(ctx, plan.ID, time.Now().UTC()); err != nil {
				return err
			}
			continue
		}
		op := *running
		if op.Action == "run_shell" {
			if err := s.recoverShellPlan(ctx, plan); err != nil {
				return err
			}
			continue
		}
		if !supportedAction(op.Action) {
			return fmt.Errorf("unsupported running operation %s: %w", op.ID, ErrUnsupported)
		}
		release, err := s.lockOperation(ctx, op)
		if err != nil {
			return err
		}
		resolution, message := "partial", "重启后无法确定操作结果，需人工核查"
		if op.Action == "trash" || op.Action == "permanent_delete" {
			if snapshotErr := s.validateStoredSnapshot(ctx, plan.ScopePath, op); snapshotErr == nil {
				resolution, message = "retry", "重启核查确认原项目仍与审批一致；不会自动重放，可重新确认后执行"
			} else {
				message = "重启后无法确认删除结果，需人工核查原路径与回收区"
			}
		} else if op.Action == "create_symlink" {
			source, sourceErr := s.root.LstatEntry(op.SourcePath)
			parent, parentErr := s.directoryParentEntry(parentPath(op.TargetPath))
			_, linkErr := s.root.LstatEntry(op.TargetPath)
			if sourceErr == nil && !source.Symlink && matchesFingerprint(source.Fingerprint(), op.SourceStateJSON) &&
				parentErr == nil && matchesTargetDirectoryParent(parentPath(op.TargetPath), parent, op.TargetStateJSON) {
				if errors.Is(linkErr, fs.ErrNotExist) {
					proposal := Proposal{ScopePath: plan.ScopePath, Action: op.Action, SourcePath: op.SourcePath, TargetPath: op.TargetPath, Reason: op.Reason}
					if _, _, snapshotErr := s.validateSymlinkSnapshot(proposal, &op.SourceStateJSON, &op.TargetStateJSON); snapshotErr == nil {
						resolution, message = "retry", "重启核查确认来源未变且目标为空；不会自动重放，请重新确认后执行"
					}
				} else if linkErr == nil && s.verifyCreatedSymlink(op) == nil {
					resolution, message = "succeeded", "重启核查确认软链接文本、来源对象和目标目录均与审批一致"
				} else {
					message = "重启后软链接结果无法与审批快照匹配，需人工核查"
				}
			} else {
				message = "重启后软链接来源或目标目录已变化，需人工核查"
			}
		} else if isTextFileWriteAction(op.Action) {
			if _, stageErr := s.textPayloads.read(op.ID, op.ProposedContentSHA256, op.ProposedContentSize); stageErr == nil {
				if s.verifyTextEditResult(op) == nil {
					resolution, message = "succeeded", "重启核查确认文件内容与批准的候选一致"
				} else if op.Action == "create_file" {
					_, snapshotErr := s.validateCreateTextFileSnapshot(Proposal{Action: op.Action, SourcePath: op.SourcePath, TargetPath: op.TargetPath}, &op.SourceStateJSON, &op.TargetStateJSON)
					if snapshotErr == nil {
						resolution, message = "retry", "重启核查确认目标仍空缺且父目录未变；不会自动重放，请重新执行"
					} else {
						message = "重启后无法确认新文件创建结果，需人工核查"
					}
				} else {
					if source, sourceErr := s.root.LstatEntry(op.SourcePath); sourceErr == nil && !source.Symlink && matchesFingerprint(source.Fingerprint(), op.SourceStateJSON) {
						resolution, message = "retry", "重启核查确认原文件仍在；不会自动重放，请重新执行"
					} else {
						message = "重启后无法确认文件写入结果，需人工核查"
					}
				}
			} else {
				message = "重启后候选内容缺失或校验失败，需人工核查"
			}
		} else if op.Action == "trash_duplicate" {
			var expected duplicateCleanupState
			if json.Unmarshal([]byte(op.SourceStateJSON), &expected) == nil {
				proposal := Proposal{
					ScopePath: plan.ScopePath, Action: op.Action, SourcePath: op.SourcePath, TargetPath: op.TargetPath,
					Reason: op.Reason, Evidence: op.Evidence,
					DuplicateScanID: expected.ScanID, DuplicateGroupID: expected.GroupID,
				}
				if _, _, _, snapshotErr := s.snapshotDuplicateCleanup(ctx, proposal, &expected); snapshotErr == nil {
					resolution, message = "retry", "重启核查确认原文件与保留文件未变；可重新执行"
				}
			}
		} else if op.Action == "create_directory" {
			parent, parentErr := s.directoryParentEntry(op.SourcePath)
			_, targetErr := s.root.LstatEntry(op.TargetPath)
			if parentErr == nil && matchesDirectoryIdentity(parent.Fingerprint(), op.SourceStateJSON) && errors.Is(targetErr, fs.ErrNotExist) {
				resolution, message = "retry", "重启核查确认目标仍空缺；不会自动重放，请重新执行"
			} else {
				message = "重启后无法证明目录创建结果，需人工核查"
			}
		} else if op.Action == "move_file" {
			action, _ := fileMoveWording(op.SourcePath, op.TargetPath)
			source, sourceErr := s.root.LstatEntry(op.SourcePath)
			parent, parentErr := s.directoryParentEntry(parentPath(op.TargetPath))
			_, targetErr := s.root.LstatEntry(op.TargetPath)
			if sourceErr == nil && !source.Symlink && source.Kind == fsroot.KindFile && matchesFingerprint(source.Fingerprint(), op.SourceStateJSON) &&
				parentErr == nil && matchesDirectoryIdentity(parent.Fingerprint(), op.TargetStateJSON) && errors.Is(targetErr, fs.ErrNotExist) {
				resolution, message = "retry", "重启核查确认源文件未变且目标空缺；不会自动重放，请重新执行"
			} else {
				message = "重启后无法证明文件" + action + "结果，源与目标需人工核查"
			}
		} else if op.Action == "copy_file" {
			source, sourceErr := s.root.LstatEntry(op.SourcePath)
			parent, parentErr := s.directoryParentEntry(parentPath(op.TargetPath))
			_, targetErr := s.root.LstatEntry(op.TargetPath)
			if sourceErr == nil && !source.Symlink && source.Kind == fsroot.KindFile && matchesFingerprint(source.Fingerprint(), op.SourceStateJSON) &&
				parentErr == nil && matchesDirectoryIdentity(parent.Fingerprint(), op.TargetStateJSON) {
				if errors.Is(targetErr, fs.ErrNotExist) {
					resolution, message = "retry", "重启核查确认源文件未变且目标空缺；不会自动重放，请重新执行"
				} else if targetErr == nil {
					digest, hashErr := s.hashApprovedFile(op)
					if hashErr == nil && s.verifyCopiedFile(op, digest) == nil {
						resolution, message = "succeeded", "重启核查确认文件副本内容与批准的来源一致"
					} else {
						message = "重启后文件副本与来源状态无法匹配，需人工核查"
					}
				}
			}
		} else if op.Action == "copy_directory" {
			parent, parentErr := s.directoryParentEntry(parentPath(op.TargetPath))
			if parentErr == nil && matchesDirectoryIdentity(parent.Fingerprint(), op.TargetStateJSON) {
				if cleanupErr := s.root.CleanupTreeCopyStaging(parentPath(op.TargetPath)); cleanupErr != nil {
					if errors.Is(cleanupErr, fsroot.ErrTreeCopyInProgress) {
						release()
						return fmt.Errorf("recover active directory copy %s: %w", op.ID, cleanupErr)
					}
					message = "重启后无法清理中断的文件夹暂存副本，需人工核查"
				} else {
					snapshot, snapshotErr := s.root.SnapshotTree(ctx, op.SourcePath)
					_, targetErr := s.root.LstatEntry(op.TargetPath)
					if snapshotErr == nil && validateAgentCopyTree(snapshot) == nil && matchesDirectoryCopyState(snapshot, op.SourceStateJSON) && errors.Is(targetErr, fs.ErrNotExist) {
						resolution, message = "retry", "重启核查确认来源未变、目标空缺且暂存副本已清理；不会自动重放，请重新执行"
					}
				}
			} else {
				message = "重启后无法证明文件夹复制结果，来源与目标需人工核查"
			}
		} else if isMetadataAction(op.Action) {
			var snapshot fsroot.Fingerprint
			if json.Unmarshal([]byte(op.SourceStateJSON), &snapshot) == nil {
				entry, readErr := s.root.LstatEntry(op.SourcePath)
				if readErr == nil && matchesFingerprint(entry.Fingerprint(), op.SourceStateJSON) {
					resolution, message = "retry", "重启核查确认文件仍为原状态；不会自动重放，请重新执行"
				} else if readErr == nil && matchesMetadataTarget(entry.Fingerprint(), snapshot, op) {
					resolution = "succeeded"
					message = "重启核查确认文件已达到批准的元数据目标；" + metadataResultMessage(snapshot, entry.Fingerprint(), op)
				} else {
					message = "重启后文件状态与原快照及批准目标均不一致，需人工核查"
				}
			} else {
				message = "重启后无法读取已批准的元数据快照，需人工核查"
			}
		} else if isArchiveAction(op.Action) {
			var target archiveTargetState
			if json.Unmarshal([]byte(op.TargetStateJSON), &target) == nil && len(target.PlannedPaths) > 0 {
				published := false
				for _, targetPath := range target.PlannedPaths {
					if _, statErr := s.root.LstatEntry(targetPath); statErr == nil || !errors.Is(statErr, fs.ErrNotExist) {
						published = true
						break
					}
				}
				if !published && s.validateStoredSnapshot(ctx, plan.ScopePath, op) == nil {
					resolution, message = "retry", "重启核查未发现已发布目标；不会自动重放，可重新确认后执行"
				} else {
					message = "重启后归档结果不确定或已有部分输出，需人工核查；不会自动重放"
				}
			}
		} else if op.Action == "archive_photo" {
			if snapshotErr := s.validateStoredSnapshot(ctx, plan.ScopePath, op); snapshotErr == nil {
				resolution, message = "retry", "重启核查确认照片源未变且归档目标空缺；不会自动重放，可重新执行"
			} else {
				message = "重启后无法确认照片归档结果，需人工核查源、目标和年月目录"
			}
		} else {
			source, sourceErr := s.root.LstatEntry(op.SourcePath)
			_, targetErr := s.root.LstatEntry(op.TargetPath)
			message = "重启后无法确定重命名结果，需人工核查"
			if sourceErr == nil && targetErr != nil && errors.Is(targetErr, fs.ErrNotExist) && matchesFingerprint(source.Fingerprint(), op.SourceStateJSON) {
				resolution, message = "retry", "重启核查确认源文件仍在且目标空缺；不会自动重放，请重新执行"
			} else if errors.Is(sourceErr, fs.ErrNotExist) && targetErr == nil {
				message = "重启后未保存可验证的内容摘要，无法确认目标文件，需人工核查"
			}
		}
		release()
		if err := s.store.RecoverAgentOperation(ctx, plan.ID, op.ID, resolution, message, time.Now().UTC()); err != nil {
			return err
		}
		if isTextFileWriteAction(op.Action) && resolution != "retry" {
			if err := s.removeTextPayload(op); err != nil {
				return err
			}
		}
	}
	activeTextPlans, err := s.store.ActiveAgentTextPlans(ctx)
	if err != nil {
		return err
	}
	for _, plan := range activeTextPlans {
		for _, op := range plan.Operations {
			if !isTextFileWriteAction(op.Action) || op.Status != store.OperationPending {
				continue
			}
			release, err := s.lockOperation(ctx, op)
			if err != nil {
				return err
			}
			latest, err := s.store.Plan(ctx, plan.ID)
			if err != nil {
				release()
				return err
			}
			current, err := singleOperation(latest, op.ID)
			if err != nil || latest.Status != store.PlanAwaitingApproval && latest.Status != store.PlanApproved || current.Status != store.OperationPending {
				release()
				continue
			}
			if snapshotErr := s.validateStoredSnapshot(ctx, latest.ScopePath, current); snapshotErr != nil {
				message := "重启恢复时候选缺失或文件状态变化，方案已失效"
				if err := s.store.InvalidateAgentOperation(ctx, latest.ID, current.ID, message, time.Now().UTC()); err != nil {
					release()
					return err
				}
				if err := s.removeTextPayload(current); err != nil {
					release()
					return err
				}
				release()
				break
			}
			release()
		}
	}
	if s.textPayloads != nil {
		referenced, err := s.store.ActiveAgentTextPayloadIDs(ctx)
		if err != nil {
			return err
		}
		if err := s.textPayloads.removeOrphans(referenced); err != nil {
			return fmt.Errorf("remove orphaned Agent text candidates: %w", err)
		}
	}
	if s.shellPayloads != nil {
		referenced, err := s.store.ActiveShellPayloadIDs(ctx)
		if err != nil {
			return err
		}
		if err := s.shellPayloads.removeOrphans(referenced); err != nil {
			return fmt.Errorf("remove orphaned Agent Shell candidates: %w", err)
		}
	}
	return nil
}

func (s *Service) validateStoredSnapshot(ctx context.Context, scope string, op store.OperationRecord) error {
	proposal := Proposal{
		ScopePath: scope, Action: op.Action, SourcePath: op.SourcePath, TargetPath: op.TargetPath,
		Reason: op.Reason, Evidence: op.Evidence, Mode: op.Mode, OwnerUID: op.OwnerUID, GroupGID: op.GroupGID,
	}
	if isArchiveAction(op.Action) {
		_, _, _, err := s.snapshotArchiveOperation(ctx, proposal, &op.SourceStateJSON, &op.TargetStateJSON, false)
		return err
	}
	if op.Action == "archive_photo" {
		var expected photoArchiveTargetState
		if json.Unmarshal([]byte(op.TargetStateJSON), &expected) != nil {
			return ErrConflict
		}
		capturedAt, err := time.Parse("2006-01-02", expected.CapturedDate)
		if err != nil || expected.Generation <= 0 {
			return ErrConflict
		}
		proposal.ArchiveRootPath, proposal.PhotoCapturedAt, proposal.PhotoIndexGeneration = expected.ArchiveRootPath, capturedAt.UTC(), expected.Generation
		_, _, err = s.snapshotArchivePhoto(proposal, &op.SourceStateJSON, &op.TargetStateJSON)
		return err
	}
	if op.Action == "trash" || op.Action == "permanent_delete" {
		if err := validateOperation(proposal); err != nil {
			return err
		}
		var expected trashOperationState
		if json.Unmarshal([]byte(op.SourceStateJSON), &expected) != nil {
			return ErrConflict
		}
		_, _, err := s.snapshotTrash(ctx, proposal, &expected)
		return err
	}
	if err := validateOperation(proposal); err != nil {
		return err
	}
	if op.Action == "create_symlink" {
		_, _, err := s.validateSymlinkSnapshot(proposal, &op.SourceStateJSON, &op.TargetStateJSON)
		return err
	}
	if isTextFileWriteAction(op.Action) {
		if _, err := s.textPayloads.read(op.ID, op.ProposedContentSHA256, op.ProposedContentSize); err != nil {
			return err
		}
		if op.Action == "create_file" {
			_, err := s.validateCreateTextFileSnapshot(proposal, &op.SourceStateJSON, &op.TargetStateJSON)
			return err
		}
		_, err := s.validateTextEditSnapshot(proposal, &op.SourceStateJSON)
		return err
	}
	if isFileTargetAction(op.Action) {
		_, _, err := s.validateFileTargetSnapshot(proposal, &op.SourceStateJSON, &op.TargetStateJSON)
		return err
	}
	if op.Action == "copy_directory" {
		_, _, err := s.validateCopyDirectorySnapshot(ctx, proposal, &op.SourceStateJSON, &op.TargetStateJSON)
		return err
	}
	if op.Action == "trash_duplicate" {
		var expected duplicateCleanupState
		if json.Unmarshal([]byte(op.SourceStateJSON), &expected) != nil {
			return ErrConflict
		}
		proposal.DuplicateScanID = expected.ScanID
		proposal.DuplicateGroupID = expected.GroupID
		_, _, _, err := s.snapshotDuplicateCleanup(ctx, proposal, &expected)
		return err
	}
	_, err := s.validateSnapshot(proposal, &op.SourceStateJSON, &op.TargetStateJSON)
	return err
}

func (s *Service) snapshotArchiveOperation(ctx context.Context, proposal Proposal, sourceJSON, targetJSON *string, inspectArchive bool) (fsroot.Entry, archiveSourceState, archiveTargetState, error) {
	var source fsroot.Entry
	var input archiveSourceState
	var outputs archiveTargetState
	if proposal.ScopePath != "" {
		scope, err := s.root.LstatEntry(proposal.ScopePath)
		if err != nil || scope.Symlink || scope.Kind != fsroot.KindDirectory {
			return source, input, outputs, ErrConflict
		}
	}

	parentPath := ""
	if proposal.Action == "create_archive" {
		snapshot, err := s.root.SnapshotTreeWithLimits(ctx, proposal.SourcePath, agentArchiveSnapshotLimits())
		if err != nil || len(snapshot.Entries) == 0 {
			return source, input, outputs, ErrConflict
		}
		if err := validateAgentArchiveTree(snapshot); err != nil {
			return source, input, outputs, err
		}
		source = snapshot.Entries[0].Entry
		input = archiveSourceState{Fingerprint: source.Fingerprint(), TreeDigest: snapshot.Digest}
		parentPath = parentPathFor(proposal.TargetPath)
		outputs.PlannedPaths = []string{proposal.TargetPath}
	} else {
		entry, err := s.root.LstatEntry(proposal.SourcePath)
		if err != nil || entry.Symlink || entry.Kind != fsroot.KindFile || !entry.Mode.IsRegular() || !entry.IdentityAvailable || !entry.ChangeTimeAvailable {
			return source, input, outputs, ErrConflict
		}
		source = entry
		input.Fingerprint = entry.Fingerprint()
		parentPath = proposal.TargetPath
		if inspectArchive {
			manifest, inspectErr := s.root.InspectArchive(ctx, proposal.SourcePath, agentArchiveReadLimits())
			if inspectErr != nil || !sameFileFingerprint(manifest.Fingerprint, entry.Fingerprint()) || len(manifest.Entries) == 0 {
				return source, input, outputs, ErrConflict
			}
			input.ManifestDigest = manifest.Digest
			outputs.ManifestDigest = manifest.Digest
			outputs.PlannedPaths = archiveOutputPaths(proposal.TargetPath, manifest.Entries)
			if len(outputs.PlannedPaths) > agentArchiveMaxEntries || archivePathsSize(outputs.PlannedPaths) > agentArchiveMaxPathBytes {
				return source, input, outputs, ErrUnsupported
			}
		}
	}

	if sourceJSON != nil {
		var expected archiveSourceState
		if json.Unmarshal([]byte(*sourceJSON), &expected) != nil || !sameFileFingerprint(expected.Fingerprint, input.Fingerprint) ||
			proposal.Action == "create_archive" && expected.TreeDigest != input.TreeDigest {
			return source, input, outputs, ErrConflict
		}
		if proposal.Action == "extract_archive" {
			input.ManifestDigest = expected.ManifestDigest
		}
	}
	if proposal.Action == "extract_archive" && !inspectArchive {
		if targetJSON == nil || outputs.PlannedPaths != nil {
			return source, input, outputs, ErrConflict
		}
		if err := json.Unmarshal([]byte(*targetJSON), &outputs); err != nil || outputs.ManifestDigest == "" || outputs.ManifestDigest != input.ManifestDigest {
			return source, input, outputs, ErrConflict
		}
	}

	parent, err := s.directoryParentEntry(parentPath)
	if err != nil {
		return source, input, outputs, err
	}
	if targetJSON != nil {
		var expected archiveTargetState
		if json.Unmarshal([]byte(*targetJSON), &expected) != nil || expected.ParentPath != parentPath ||
			!sameDirectoryFingerprintIdentity(parent.Fingerprint(), expected.Parent) || expected.ManifestDigest != outputs.ManifestDigest ||
			!sameAgentPaths(expected.PlannedPaths, outputs.PlannedPaths) {
			return source, input, outputs, ErrConflict
		}
	}
	outputs.ParentPath, outputs.Parent = parentPath, parent.Fingerprint()
	for _, output := range outputs.PlannedPaths {
		if _, err := s.root.LstatEntry(output); err == nil {
			return source, input, outputs, ErrConflict
		} else if !errors.Is(err, fs.ErrNotExist) {
			return source, input, outputs, ErrConflict
		}
	}
	return source, input, outputs, nil
}

func archivePathsSize(paths []string) int {
	total := 0
	for _, value := range paths {
		total += len(value)
		if total > agentArchiveMaxPathBytes {
			return total
		}
	}
	return total
}

func sameDirectoryFingerprintIdentity(actual, expected fsroot.Fingerprint) bool {
	if actual.Kind != fsroot.KindDirectory || expected.Kind != fsroot.KindDirectory || actual.Exists != expected.Exists ||
		actual.IdentityAvailable != expected.IdentityAvailable {
		return false
	}
	if !actual.Exists {
		return true
	}
	return !actual.IdentityAvailable || actual.Device == expected.Device && actual.Inode == expected.Inode
}

func validateAgentArchiveTree(snapshot fsroot.TreeSnapshot) error {
	if len(snapshot.Entries) == 0 || len(snapshot.Entries) > agentArchiveMaxEntries || snapshot.RootPath == "" {
		return ErrUnsupported
	}
	var totalBytes int64
	var totalPathBytes int
	for _, item := range snapshot.Entries {
		entry := item.Entry
		if len(entry.Path) > agentArchiveMaxPathBytes-totalPathBytes {
			return ErrUnsupported
		}
		totalPathBytes += len(entry.Path)
		if entry.Symlink || !entry.IdentityAvailable || !entry.ChangeTimeAvailable || !item.ChangeTimeAvailable {
			return ErrUnsupported
		}
		switch entry.Kind {
		case fsroot.KindDirectory:
			if !entry.Mode.IsDir() {
				return ErrUnsupported
			}
		case fsroot.KindFile:
			if !entry.Mode.IsRegular() || entry.Size < 0 || entry.Size > agentArchiveMaxBytes-totalBytes {
				return ErrUnsupported
			}
			totalBytes += entry.Size
		default:
			return ErrUnsupported
		}
	}
	return nil
}

func sameAgentPaths(actual, expected []string) bool {
	if len(actual) != len(expected) {
		return false
	}
	for index := range actual {
		if actual[index] != expected[index] {
			return false
		}
	}
	return true
}

func parentPathFor(relativePath string) string {
	parent := path.Dir(relativePath)
	if parent == "." {
		return ""
	}
	return parent
}

func archiveOutputPaths(destination string, entries []fsroot.ArchiveEntry) []string {
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		output := entry.Path
		if destination != "" {
			output = path.Join(destination, output)
		}
		paths = append(paths, output)
	}
	sort.Strings(paths)
	return paths
}

func agentArchiveReadLimits() fsroot.ArchiveReadLimits {
	return fsroot.ArchiveReadLimits{
		MaxArchiveBytes: 20 << 30, MaxExternalArchiveBytes: 1 << 30, MaxEntries: 50000,
		MaxEntryBytes: 2 << 30, MaxTotalBytes: 20 << 30, MaxPreviewBytes: 32 << 20, MaxRatio: 1000,
	}
}

func maxProposalOperations(proposals []Proposal) int {
	if len(proposals) == 0 {
		return store.MaxAgentPlanOperations
	}
	for _, proposal := range proposals {
		if isArchiveAction(proposal.Action) {
			return 1
		}
		if proposal.Action == "archive_photo" && proposal.ConversationID == "" {
			return 20
		}
	}
	return store.MaxAgentPlanOperations
}

func hasClassificationProposal(proposals []Proposal) bool {
	for _, proposal := range proposals {
		if proposal.Classification {
			return true
		}
	}
	return false
}

func (s *Service) snapshotArchivePhoto(proposal Proposal, sourceJSON, targetStateJSON *string) (fsroot.Entry, photoArchiveTargetState, error) {
	if err := validateOperation(proposal); err != nil {
		return fsroot.Entry{}, photoArchiveTargetState{}, err
	}
	if proposal.ScopePath != "" {
		scope, err := s.root.LstatEntry(proposal.ScopePath)
		if err != nil || scope.Symlink || scope.Kind != fsroot.KindDirectory {
			return fsroot.Entry{}, photoArchiveTargetState{}, ErrConflict
		}
	}
	source, err := s.root.LstatEntry(proposal.SourcePath)
	if err != nil || source.Symlink || source.Kind != fsroot.KindFile || !source.IdentityAvailable || !source.ChangeTimeAvailable {
		return fsroot.Entry{}, photoArchiveTargetState{}, ErrConflict
	}
	if sourceJSON != nil && !matchesFingerprint(source.Fingerprint(), *sourceJSON) {
		return fsroot.Entry{}, photoArchiveTargetState{}, ErrConflict
	}
	date := proposal.PhotoCapturedAt.UTC().Format("2006-01-02")
	root, err := s.root.LstatEntry(proposal.ArchiveRootPath)
	if err != nil || root.Symlink || root.Kind != fsroot.KindDirectory || !root.IdentityAvailable {
		return fsroot.Entry{}, photoArchiveTargetState{}, ErrConflict
	}
	yearPath := path.Join(proposal.ArchiveRootPath, date[:4])
	monthPath := path.Join(yearPath, date[5:7])
	year, err := archiveDirectory(s.root, yearPath)
	if err != nil {
		return fsroot.Entry{}, photoArchiveTargetState{}, err
	}
	month := fsroot.Fingerprint{Exists: false}
	if year.Exists {
		month, err = archiveDirectory(s.root, monthPath)
		if err != nil {
			return fsroot.Entry{}, photoArchiveTargetState{}, err
		}
	}
	if _, err := s.root.LstatEntry(proposal.TargetPath); err == nil {
		return fsroot.Entry{}, photoArchiveTargetState{}, ErrConflict
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fsroot.Entry{}, photoArchiveTargetState{}, ErrConflict
	}
	state := photoArchiveTargetState{
		ArchiveRootPath: proposal.ArchiveRootPath, CapturedDate: date, Generation: proposal.PhotoIndexGeneration,
		Root: root.Fingerprint(), YearPath: yearPath, Year: year, MonthPath: monthPath, Month: month,
	}
	if targetStateJSON != nil {
		var expected photoArchiveTargetState
		if json.Unmarshal([]byte(*targetStateJSON), &expected) != nil || expected.ArchiveRootPath != state.ArchiveRootPath ||
			expected.CapturedDate != state.CapturedDate || expected.Generation != state.Generation ||
			expected.YearPath != state.YearPath || expected.MonthPath != state.MonthPath ||
			!matchesDirectoryIdentity(state.Root, mustJSON(expected.Root)) ||
			!archiveDirectoryMatches(state.Year, expected.Year) || !archiveDirectoryMatches(state.Month, expected.Month) {
			return fsroot.Entry{}, photoArchiveTargetState{}, ErrConflict
		}
	}
	return source, state, nil
}

func archiveDirectory(root *fsroot.Root, relativePath string) (fsroot.Fingerprint, error) {
	entry, err := root.LstatEntry(relativePath)
	if errors.Is(err, fs.ErrNotExist) {
		return fsroot.Fingerprint{Exists: false}, nil
	}
	if err != nil || entry.Symlink || entry.Kind != fsroot.KindDirectory || !entry.IdentityAvailable {
		return fsroot.Fingerprint{}, ErrConflict
	}
	return entry.Fingerprint(), nil
}

func archiveDirectoryMatches(actual, expected fsroot.Fingerprint) bool {
	if !expected.Exists {
		return !actual.Exists || actual.Kind == fsroot.KindDirectory && actual.IdentityAvailable
	}
	return matchesDirectoryIdentity(actual, mustJSON(expected))
}

func mustJSON(value any) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

// TextEditPreview 仅用于审批界面临时展示原文和候选正文。
type TextEditPreview struct {
	PlanID          string
	OperationID     string
	Path            string
	SourceContent   string
	ProposedContent string
}

// PreviewTextEdit 只在方案版本与源文件指纹均匹配时返回正文。
func (s *Service) PreviewTextEdit(ctx context.Context, planID, operationID string, expectedVersion int) (TextEditPreview, error) {
	if expectedVersion < 1 {
		return TextEditPreview{}, ErrConflict
	}
	plan, err := s.store.Plan(ctx, planID)
	if err != nil {
		return TextEditPreview{}, err
	}
	if plan.Version != expectedVersion || plan.Status != store.PlanAwaitingApproval && plan.Status != store.PlanApproved {
		return TextEditPreview{}, ErrConflict
	}
	op, err := singleOperation(plan, operationID)
	if err != nil || !isTextFileWriteAction(op.Action) || op.Status != store.OperationPending {
		return TextEditPreview{}, ErrConflict
	}
	release, err := s.lockOperation(ctx, op)
	if err != nil {
		return TextEditPreview{}, err
	}
	defer release()
	latest, err := s.store.Plan(ctx, planID)
	if err != nil || latest.Version != expectedVersion || latest.Status != plan.Status {
		return TextEditPreview{}, ErrConflict
	}
	current, err := singleOperation(latest, operationID)
	if err != nil || s.validateStoredSnapshot(ctx, latest.ScopePath, current) != nil {
		return TextEditPreview{}, ErrConflict
	}
	var content []byte
	if current.Action != "create_file" {
		content, err = s.readTextEditSource(current)
		if err != nil {
			return TextEditPreview{}, ErrConflict
		}
	}
	proposed, err := s.textPayloads.read(current.ID, current.ProposedContentSHA256, current.ProposedContentSize)
	if err != nil {
		return TextEditPreview{}, ErrConflict
	}
	return TextEditPreview{
		PlanID: latest.ID, OperationID: current.ID, Path: textWriteTargetPath(current),
		SourceContent: string(content), ProposedContent: string(proposed),
	}, nil
}

func (s *Service) validateTextEditSnapshot(proposal Proposal, sourceJSON *string) (fsroot.Entry, error) {
	if !supportedTextExtension(proposal.SourcePath) || !withinScope(proposal.ScopePath, proposal.SourcePath) || reservedPath(proposal.SourcePath) {
		return fsroot.Entry{}, ErrInvalid
	}
	entry, err := s.root.LstatEntry(proposal.SourcePath)
	if err != nil || entry.Symlink || entry.Kind != fsroot.KindFile || !entry.IdentityAvailable || !entry.ChangeTimeAvailable || entry.Links != 1 || entry.Size > maxTextEditBytes {
		return fsroot.Entry{}, ErrUnsupported
	}
	if sourceJSON != nil && !matchesFingerprint(entry.Fingerprint(), *sourceJSON) {
		return fsroot.Entry{}, ErrConflict
	}
	return entry, nil
}

func (s *Service) validateCreateTextFileSnapshot(proposal Proposal, parentJSON, targetJSON *string) (fsroot.Entry, error) {
	if proposal.Action != "create_file" || !supportedTextExtension(proposal.TargetPath) || !withinScope(proposal.ScopePath, proposal.TargetPath) || reservedPath(proposal.TargetPath) {
		return fsroot.Entry{}, ErrInvalid
	}
	parent, err := s.directoryParentEntry(proposal.SourcePath)
	if err != nil {
		return fsroot.Entry{}, err
	}
	if _, err := s.root.LstatEntry(proposal.TargetPath); err == nil {
		return fsroot.Entry{}, fmt.Errorf("agent text file target is already occupied: %w", ErrConflict)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fsroot.Entry{}, fmt.Errorf("agent text file target cannot be checked: %w", ErrConflict)
	}
	if parentJSON != nil && !matchesDirectoryIdentity(parent.Fingerprint(), *parentJSON) {
		return fsroot.Entry{}, fmt.Errorf("agent text file parent changed: %w", ErrConflict)
	}
	if targetJSON != nil && !matchesFingerprint(fsroot.Fingerprint{Exists: false}, *targetJSON) {
		return fsroot.Entry{}, fmt.Errorf("agent text file target snapshot changed: %w", ErrConflict)
	}
	return parent, nil
}

func (s *Service) readTextEditSource(op store.OperationRecord) ([]byte, error) {
	proposal := Proposal{Action: op.Action, ScopePath: "", SourcePath: op.SourcePath, TargetPath: op.TargetPath}
	entry, err := s.validateTextEditSnapshot(proposal, &op.SourceStateJSON)
	if err != nil {
		return nil, err
	}
	reader, opened, err := s.root.OpenRead(op.SourcePath)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	if opened.Symlink || !sameFingerprint(opened.Fingerprint(), entry.Fingerprint()) {
		return nil, ErrConflict
	}
	content, err := io.ReadAll(io.LimitReader(reader, maxTextEditBytes+1))
	if err != nil || len(content) > maxTextEditBytes || !utf8.Valid(content) || strings.ContainsRune(string(content), '\x00') {
		return nil, ErrUnsupported
	}
	current, err := s.root.LstatEntry(op.SourcePath)
	if err != nil || !sameFingerprint(current.Fingerprint(), entry.Fingerprint()) {
		return nil, ErrConflict
	}
	return content, nil
}

// validateFileTargetSnapshot 校验文件来源、目标父目录身份和目标空缺状态。
func (s *Service) validateFileTargetSnapshot(proposal Proposal, sourceJSON, targetParentJSON *string) (fsroot.Entry, fsroot.Entry, error) {
	if proposal.ScopePath != "" {
		scope, err := s.root.LstatEntry(proposal.ScopePath)
		if err != nil || scope.Symlink || scope.Kind != fsroot.KindDirectory {
			return fsroot.Entry{}, fsroot.Entry{}, ErrInvalid
		}
	}
	source, err := s.root.LstatEntry(proposal.SourcePath)
	if err != nil || source.Symlink || source.Kind != fsroot.KindFile || !source.IdentityAvailable || !source.ChangeTimeAvailable {
		return fsroot.Entry{}, fsroot.Entry{}, ErrConflict
	}
	targetParent, err := s.directoryParentEntry(parentPath(proposal.TargetPath))
	if err != nil {
		return fsroot.Entry{}, fsroot.Entry{}, err
	}
	if _, err := s.root.LstatEntry(proposal.TargetPath); err == nil {
		return fsroot.Entry{}, fsroot.Entry{}, fmt.Errorf("agent target is already occupied: %w", ErrConflict)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fsroot.Entry{}, fsroot.Entry{}, fmt.Errorf("agent target cannot be checked: %w", ErrConflict)
	}
	if sourceJSON != nil && !matchesFingerprint(source.Fingerprint(), *sourceJSON) {
		return fsroot.Entry{}, fsroot.Entry{}, fmt.Errorf("agent source changed: %w", ErrConflict)
	}
	if targetParentJSON != nil && !matchesTargetDirectoryParent(parentPath(proposal.TargetPath), targetParent, *targetParentJSON) {
		return fsroot.Entry{}, fsroot.Entry{}, fmt.Errorf("agent target parent changed: %w", ErrConflict)
	}
	return source, targetParent, nil
}

// validateSymlinkSnapshot 绑定现有普通对象、目标父目录和空闲链接路径。
func (s *Service) validateSymlinkSnapshot(proposal Proposal, sourceJSON, targetParentJSON *string) (fsroot.Entry, fsroot.Entry, error) {
	if proposal.ScopePath != "" {
		scope, err := s.root.LstatEntry(proposal.ScopePath)
		if err != nil || scope.Symlink || scope.Kind != fsroot.KindDirectory {
			return fsroot.Entry{}, fsroot.Entry{}, ErrInvalid
		}
	}
	source, err := s.root.LstatEntry(proposal.SourcePath)
	if err != nil || source.Symlink || source.Kind != fsroot.KindFile && source.Kind != fsroot.KindDirectory ||
		!source.IdentityAvailable || !source.ChangeTimeAvailable {
		return fsroot.Entry{}, fsroot.Entry{}, ErrConflict
	}
	if source.Kind == fsroot.KindDirectory && (parentPath(proposal.TargetPath) == proposal.SourcePath || strings.HasPrefix(parentPath(proposal.TargetPath), proposal.SourcePath+"/")) {
		return fsroot.Entry{}, fsroot.Entry{}, ErrInvalid
	}
	targetParent, err := s.directoryParentEntry(parentPath(proposal.TargetPath))
	if err != nil {
		return fsroot.Entry{}, fsroot.Entry{}, err
	}
	if _, err := s.root.LstatEntry(proposal.TargetPath); err == nil {
		return fsroot.Entry{}, fsroot.Entry{}, fmt.Errorf("agent symlink target is already occupied: %w", ErrConflict)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fsroot.Entry{}, fsroot.Entry{}, fmt.Errorf("agent symlink target cannot be checked: %w", ErrConflict)
	}
	if sourceJSON != nil && !matchesFingerprint(source.Fingerprint(), *sourceJSON) {
		return fsroot.Entry{}, fsroot.Entry{}, fmt.Errorf("agent symlink source changed: %w", ErrConflict)
	}
	if targetParentJSON != nil && !matchesTargetDirectoryParent(parentPath(proposal.TargetPath), targetParent, *targetParentJSON) {
		return fsroot.Entry{}, fsroot.Entry{}, fmt.Errorf("agent symlink parent changed: %w", ErrConflict)
	}
	return source, targetParent, nil
}

func matchesTargetDirectoryParent(relativePath string, actual fsroot.Entry, expectedJSON string) bool {
	if relativePath != "" {
		return matchesDirectoryIdentity(actual.Fingerprint(), expectedJSON)
	}
	var expected fsroot.Fingerprint
	return json.Unmarshal([]byte(expectedJSON), &expected) == nil && sameFileFingerprint(expected, actual.Fingerprint())
}

// validateCopyDirectorySnapshot 校验目录树快照与复制目标，快照仅保存摘要到审批记录。
func (s *Service) validateCopyDirectorySnapshot(ctx context.Context, proposal Proposal, sourceJSON, targetParentJSON *string) (fsroot.TreeSnapshot, fsroot.Entry, error) {
	if proposal.ScopePath != "" {
		scope, err := s.root.LstatEntry(proposal.ScopePath)
		if err != nil || scope.Symlink || scope.Kind != fsroot.KindDirectory {
			return fsroot.TreeSnapshot{}, fsroot.Entry{}, ErrInvalid
		}
	}
	if proposal.TargetPath == proposal.SourcePath || strings.HasPrefix(proposal.TargetPath, proposal.SourcePath+"/") {
		return fsroot.TreeSnapshot{}, fsroot.Entry{}, ErrInvalid
	}
	snapshot, err := s.root.SnapshotTree(ctx, proposal.SourcePath)
	if err != nil {
		return fsroot.TreeSnapshot{}, fsroot.Entry{}, err
	}
	if err := validateAgentCopyTree(snapshot); err != nil {
		return fsroot.TreeSnapshot{}, fsroot.Entry{}, err
	}
	targetParent, err := s.directoryParentEntry(parentPath(proposal.TargetPath))
	if err != nil {
		return fsroot.TreeSnapshot{}, fsroot.Entry{}, err
	}
	if _, err := s.root.LstatEntry(proposal.TargetPath); err == nil {
		return fsroot.TreeSnapshot{}, fsroot.Entry{}, fmt.Errorf("agent directory target is already occupied: %w", ErrConflict)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fsroot.TreeSnapshot{}, fsroot.Entry{}, fmt.Errorf("agent directory target cannot be checked: %w", ErrConflict)
	}
	if sourceJSON != nil && !matchesDirectoryCopyState(snapshot, *sourceJSON) {
		return fsroot.TreeSnapshot{}, fsroot.Entry{}, fmt.Errorf("agent directory source changed: %w", ErrConflict)
	}
	if targetParentJSON != nil && !matchesDirectoryIdentity(targetParent.Fingerprint(), *targetParentJSON) {
		return fsroot.TreeSnapshot{}, fsroot.Entry{}, fmt.Errorf("agent directory target parent changed: %w", ErrConflict)
	}
	return snapshot, targetParent, nil
}

// validateAgentCopyTree 限定复制树只包含可复核的普通文件和目录。
func validateAgentCopyTree(snapshot fsroot.TreeSnapshot) error {
	if snapshot.RootPath == "" || len(snapshot.Entries) == 0 || snapshot.Entries[0].Entry.Kind != fsroot.KindDirectory {
		return ErrUnsupported
	}
	for _, item := range snapshot.Entries {
		entry := item.Entry
		if entry.Symlink || !entry.IdentityAvailable || !entry.ChangeTimeAvailable || !item.ChangeTimeAvailable {
			return ErrUnsupported
		}
		switch entry.Kind {
		case fsroot.KindDirectory:
			if !entry.Mode.IsDir() {
				return ErrUnsupported
			}
		case fsroot.KindFile:
			if !entry.Mode.IsRegular() {
				return ErrUnsupported
			}
		default:
			return ErrUnsupported
		}
	}
	return nil
}

func matchesDirectoryCopyState(snapshot fsroot.TreeSnapshot, expectedJSON string) bool {
	var expected directoryCopyState
	if json.Unmarshal([]byte(expectedJSON), &expected) != nil || expected.RootPath == "" || len(expected.Digest) != sha256.Size*2 {
		return false
	}
	if _, err := hex.DecodeString(expected.Digest); err != nil {
		return false
	}
	return snapshot.RootPath == expected.RootPath && snapshot.Digest == expected.Digest
}

func (s *Service) validateSnapshot(proposal Proposal, sourceJSON, targetJSON *string) (fsroot.Entry, error) {
	if isMetadataAction(proposal.Action) {
		if proposal.ScopePath != "" {
			scope, err := s.root.LstatEntry(proposal.ScopePath)
			if err != nil || scope.Symlink || scope.Kind != fsroot.KindDirectory {
				return fsroot.Entry{}, ErrInvalid
			}
		}
		source, err := s.root.LstatEntry(proposal.SourcePath)
		if err != nil || source.Symlink || source.Kind != fsroot.KindFile && source.Kind != fsroot.KindDirectory ||
			!source.IdentityAvailable || !source.OwnershipAvailable || !source.ChangeTimeAvailable {
			return fsroot.Entry{}, ErrConflict
		}
		if hasSpecialModeBits(source.Mode) {
			return fsroot.Entry{}, ErrUnsupported
		}
		if sourceJSON != nil && !matchesFingerprint(source.Fingerprint(), *sourceJSON) {
			return fsroot.Entry{}, fmt.Errorf("agent metadata source snapshot changed: %w", ErrConflict)
		}
		parent := parentPath(proposal.SourcePath)
		if parent != "" {
			parentEntry, parentErr := s.root.LstatEntry(parent)
			if parentErr != nil || parentEntry.Symlink || parentEntry.Kind != fsroot.KindDirectory {
				return fsroot.Entry{}, ErrConflict
			}
		}
		if targetJSON != nil && !matchesFingerprint(fsroot.Fingerprint{Exists: false}, *targetJSON) {
			return fsroot.Entry{}, ErrConflict
		}
		return source, nil
	}
	if proposal.Action == "create_directory" {
		if proposal.ScopePath != "" {
			scope, err := s.root.LstatEntry(proposal.ScopePath)
			if err != nil || scope.Symlink || scope.Kind != fsroot.KindDirectory {
				return fsroot.Entry{}, ErrInvalid
			}
		}
		parent, err := s.directoryParentEntry(proposal.SourcePath)
		if err != nil {
			return fsroot.Entry{}, err
		}
		if _, err := s.root.LstatEntry(proposal.TargetPath); err == nil {
			return fsroot.Entry{}, fmt.Errorf("agent directory target is already occupied: %w", ErrConflict)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fsroot.Entry{}, fmt.Errorf("agent directory target cannot be checked: %w", ErrConflict)
		}
		if sourceJSON != nil && !matchesDirectoryIdentity(parent.Fingerprint(), *sourceJSON) {
			return fsroot.Entry{}, fmt.Errorf("agent directory parent changed: %w", ErrConflict)
		}
		if targetJSON != nil && !matchesFingerprint(fsroot.Fingerprint{Exists: false}, *targetJSON) {
			return fsroot.Entry{}, fmt.Errorf("agent directory target snapshot changed: %w", ErrConflict)
		}
		return parent, nil
	}
	if proposal.ScopePath != "" {
		scope, err := s.root.LstatEntry(proposal.ScopePath)
		if err != nil || scope.Symlink || scope.Kind != fsroot.KindDirectory {
			return fsroot.Entry{}, ErrInvalid
		}
	}
	source, err := s.root.LstatEntry(proposal.SourcePath)
	if err != nil || source.Symlink || source.Kind != fsroot.KindFile || !source.IdentityAvailable || !source.ChangeTimeAvailable {
		return fsroot.Entry{}, ErrConflict
	}
	if proposal.TargetPath != proposal.SourcePath {
		if _, err := s.root.LstatEntry(proposal.TargetPath); err == nil {
			return fsroot.Entry{}, fmt.Errorf("agent target is already occupied: %w", ErrConflict)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fsroot.Entry{}, fmt.Errorf("agent target cannot be checked: %w", ErrConflict)
		}
	}
	if sourceJSON != nil {
		if !matchesFingerprint(source.Fingerprint(), *sourceJSON) {
			return fsroot.Entry{}, fmt.Errorf("agent source snapshot changed: %w", ErrConflict)
		}
	}
	if targetJSON != nil {
		if !matchesFingerprint(fsroot.Fingerprint{Exists: false}, *targetJSON) {
			return fsroot.Entry{}, fmt.Errorf("agent target snapshot changed: %w", ErrConflict)
		}
	}
	parent := path.Dir(proposal.SourcePath)
	if parent == "." {
		parent = ""
	}
	if parent != "" {
		entry, err := s.root.LstatEntry(parent)
		if err != nil || entry.Kind != fsroot.KindDirectory || entry.Symlink {
			return fsroot.Entry{}, ErrConflict
		}
	}
	return source, nil
}

// directoryParentEntry 读取目标父目录快照，并拒绝软链接或不可识别的目录身份。
func (s *Service) directoryParentEntry(parentPath string) (fsroot.Entry, error) {
	if parentPath == "" {
		if _, err := s.root.List(""); err != nil {
			return fsroot.Entry{}, fmt.Errorf("inspect managed root: %w", ErrConflict)
		}
		return fsroot.Entry{Kind: fsroot.KindDirectory}, nil
	}
	parent, err := s.root.LstatEntry(parentPath)
	if err != nil || parent.Symlink || parent.Kind != fsroot.KindDirectory || !parent.IdentityAvailable {
		return fsroot.Entry{}, fmt.Errorf("directory parent is unavailable or changed: %w", ErrConflict)
	}
	return parent, nil
}

// parentPath 将根级条目的父路径规范化为空字符串。
func parentPath(relativePath string) string {
	parent := path.Dir(relativePath)
	if parent == "." {
		return ""
	}
	return parent
}

// applyOperation 执行已审批的文件操作。
func (s *Service) applyOperation(op store.OperationRecord) error {
	switch op.Action {
	case "rename":
		return s.root.RenameNoReplace(op.SourcePath, op.TargetPath)
	case "create_directory":
		return s.root.Mkdir(op.TargetPath, 0o755)
	case "create_symlink":
		return s.root.SymlinkNoReplace(op.SourcePath, op.TargetPath)
	default:
		return ErrUnsupported
	}
}

// verifyCreatedDirectory 确认创建后父目录身份未变且目标仍为真实目录。
func (s *Service) verifyCreatedDirectory(op store.OperationRecord) error {
	parent, err := s.directoryParentEntry(op.SourcePath)
	if err != nil || !matchesDirectoryIdentity(parent.Fingerprint(), op.SourceStateJSON) {
		return fmt.Errorf("directory parent identity changed after creation: %w", ErrConflict)
	}
	target, err := s.root.LstatEntry(op.TargetPath)
	if err != nil || target.Symlink || target.Kind != fsroot.KindDirectory {
		return fmt.Errorf("created directory cannot be verified: %w", ErrConflict)
	}
	return nil
}

func validateProposal(proposal Proposal) error {
	if len(proposal.RequestText) == 0 || len(proposal.RequestText) > 4096 || strings.TrimSpace(proposal.RequestText) == "" {
		return ErrInvalid
	}
	if proposal.ConversationID != "" && (proposal.ModelProvider == "" || proposal.ModelName == "") {
		return ErrInvalid
	}
	if proposal.Action == "trash_duplicate" && (proposal.ConversationID != "" || proposal.ModelProvider != "" || proposal.ModelName != "" ||
		proposal.DuplicateScanID == "" || proposal.DuplicateGroupID < 1 || proposal.ExpectedSourceStateJSON != "" || proposal.ExpectedTargetParentStateJSON != "") {
		return ErrInvalid
	}
	if proposal.Action == "archive_photo" {
		if proposal.ConversationID == "" || proposal.ArchiveRootPath == "" || proposal.PhotoCapturedAt.IsZero() || proposal.PhotoIndexGeneration <= 0 || proposal.ExpectedSourceStateJSON == "" {
			return ErrInvalid
		}
	} else if proposal.ArchiveRootPath != "" || !proposal.PhotoCapturedAt.IsZero() || proposal.PhotoIndexGeneration != 0 {
		return ErrInvalid
	}
	if proposal.ConversationReply != "" && (proposal.Action != "archive_photo" && !proposal.Classification ||
		len(proposal.ConversationReply) > 4096 || strings.TrimSpace(proposal.ConversationReply) == "" || !utf8.ValidString(proposal.ConversationReply)) {
		return ErrInvalid
	}
	if proposal.Action != "trash_duplicate" && (proposal.DuplicateScanID != "" || proposal.DuplicateGroupID != 0) {
		return ErrInvalid
	}
	if isTextFileWriteAction(proposal.Action) {
		if !validTextFileCandidate(proposal.ProposedContent) || proposal.Action == "edit_text" && len(proposal.ProposedContent) == 0 {
			return ErrInvalid
		}
	} else if proposal.ProposedContent != nil {
		return ErrInvalid
	}
	if proposal.Classification && (proposal.ConversationID == "" || proposal.Action != "move_file" || proposal.ExpectedSourceStateJSON == "" || proposal.Evidence != "") {
		return ErrInvalid
	}
	if proposal.ExpectedTargetParentStateJSON != "" && !isFileTargetAction(proposal.Action) && proposal.Action != "copy_directory" && proposal.Action != "create_symlink" {
		return ErrInvalid
	}
	if proposal.ConversationID != "" && (proposal.Action == "rename" || isTextFileWriteAction(proposal.Action) || proposal.Action == "create_symlink") && proposal.ExpectedSourceStateJSON == "" {
		return ErrInvalid
	}
	if proposal.ConversationID == "" && (proposal.ModelProvider != "" || proposal.ModelName != "" ||
		proposal.ExpectedSourceStateJSON != "" && !isTextFileWriteAction(proposal.Action) || proposal.ExpectedTargetParentStateJSON != "") {
		return ErrInvalid
	}
	return validateOperation(proposal)
}

func validateOperation(proposal Proposal) error {
	tool, ok := agentTools()[proposal.Action]
	if !ok || tool.validate == nil {
		return ErrUnsupported
	}
	return tool.validate(proposal)
}

func validateOperationContract(proposal Proposal) error {
	if len(proposal.Reason) == 0 || len(proposal.Reason) > 1024 || strings.TrimSpace(proposal.Reason) == "" || len(proposal.Evidence) > 2048 {
		return ErrInvalid
	}
	if !cleanPath(proposal.ScopePath, true) || !cleanPath(proposal.SourcePath, proposal.Action == "create_directory" || proposal.Action == "create_file") || !cleanPath(proposal.TargetPath, proposal.Action == "trash" || proposal.Action == "permanent_delete" || proposal.Action == "extract_archive") {
		return ErrInvalid
	}
	if proposal.Action == "archive_photo" && (!cleanPath(proposal.ArchiveRootPath, false) || proposal.SourcePath == proposal.TargetPath ||
		!withinScope(proposal.ScopePath, proposal.SourcePath) || !validPhotoArchiveTarget(proposal.ArchiveRootPath, proposal.PhotoCapturedAt, proposal.SourcePath, proposal.TargetPath)) {
		return ErrInvalid
	}
	if proposal.Action == "create_archive" {
		if proposal.SourcePath == "" || proposal.TargetPath == "" || proposal.SourcePath == proposal.TargetPath ||
			!withinScope(proposal.ScopePath, proposal.SourcePath) || !withinScope(proposal.ScopePath, proposal.TargetPath) {
			return ErrInvalid
		}
		format, supported := archiveCreateFormat(proposal.TargetPath)
		if !supported || fsroot.ValidateArchiveFormat(format) != nil {
			return ErrUnsupported
		}
	} else if proposal.Action == "extract_archive" {
		if proposal.SourcePath == "" || !supportedExtractArchivePath(proposal.SourcePath) ||
			!withinScope(proposal.ScopePath, proposal.SourcePath) || !withinScope(proposal.ScopePath, proposal.TargetPath) {
			return ErrInvalid
		}
	}
	if proposal.Action == "edit_text" || proposal.Action == "overwrite_file" {
		if proposal.SourcePath == "" || proposal.SourcePath != proposal.TargetPath || !supportedTextExtension(proposal.SourcePath) {
			return ErrInvalid
		}
	} else if proposal.Action == "create_file" {
		parent := path.Dir(proposal.TargetPath)
		if parent == "." {
			parent = ""
		}
		if proposal.SourcePath != parent || !supportedTextExtension(proposal.TargetPath) || !withinScope(proposal.ScopePath, proposal.TargetPath) {
			return ErrInvalid
		}
	} else if proposal.Action == "set_permissions" {
		if proposal.SourcePath != proposal.TargetPath || proposal.Mode == nil || *proposal.Mode > 0o777 || proposal.OwnerUID != nil || proposal.GroupGID != nil || !withinScope(proposal.ScopePath, proposal.SourcePath) {
			return ErrInvalid
		}
	} else if proposal.Action == "set_owner" {
		if proposal.SourcePath != proposal.TargetPath || proposal.Mode != nil || proposal.OwnerUID == nil && proposal.GroupGID == nil || !withinScope(proposal.ScopePath, proposal.SourcePath) {
			return ErrInvalid
		}
	} else if proposal.Mode != nil || proposal.OwnerUID != nil || proposal.GroupGID != nil {
		return ErrInvalid
	}
	if proposal.Action == "trash" || proposal.Action == "permanent_delete" {
		if proposal.SourcePath == "" || proposal.TargetPath != "" || !withinScope(proposal.ScopePath, proposal.SourcePath) {
			return ErrInvalid
		}
	} else if proposal.Action == "trash_duplicate" {
		if proposal.SourcePath == proposal.TargetPath || !withinScope(proposal.ScopePath, proposal.SourcePath) || !withinScope(proposal.ScopePath, proposal.TargetPath) {
			return ErrInvalid
		}
	} else if proposal.Action == "rename" {
		if proposal.SourcePath == proposal.TargetPath || path.Dir(proposal.SourcePath) != path.Dir(proposal.TargetPath) {
			return ErrInvalid
		}
		if !withinScope(proposal.ScopePath, proposal.SourcePath) || !withinScope(proposal.ScopePath, proposal.TargetPath) {
			return ErrInvalid
		}
	} else if proposal.Action == "create_directory" {
		parent := path.Dir(proposal.TargetPath)
		if parent == "." {
			parent = ""
		}
		if parent != proposal.SourcePath || !withinScope(proposal.ScopePath, proposal.SourcePath) || !withinScope(proposal.ScopePath, proposal.TargetPath) {
			return ErrInvalid
		}
	} else if proposal.Action == "create_archive" {
		if strings.HasPrefix(proposal.TargetPath, proposal.SourcePath+"/") || proposal.SourcePath == proposal.TargetPath {
			return ErrInvalid
		}
	} else if proposal.Action == "extract_archive" {
		if proposal.TargetPath == proposal.SourcePath {
			return ErrInvalid
		}
	} else if proposal.Action == "copy_directory" {
		if strings.HasPrefix(proposal.TargetPath, proposal.SourcePath+"/") || !withinScope(proposal.ScopePath, proposal.SourcePath) || !withinScope(proposal.ScopePath, proposal.TargetPath) {
			return ErrInvalid
		}
	} else if proposal.Action == "edit_text" || proposal.Action == "overwrite_file" {
		if !withinScope(proposal.ScopePath, proposal.SourcePath) {
			return ErrInvalid
		}
	} else if proposal.Action == "create_file" {
		if !withinScope(proposal.ScopePath, proposal.TargetPath) {
			return ErrInvalid
		}
	} else if proposal.Action == "archive_photo" {
		if reservedPath(proposal.SourcePath) || reservedPath(proposal.TargetPath) {
			return ErrInvalid
		}
	} else if isMetadataAction(proposal.Action) {
		if reservedPath(proposal.SourcePath) || !withinScope(proposal.ScopePath, proposal.SourcePath) {
			return ErrInvalid
		}
	} else if proposal.SourcePath == proposal.TargetPath || !withinScope(proposal.ScopePath, proposal.SourcePath) || !withinScope(proposal.ScopePath, proposal.TargetPath) {
		return ErrInvalid
	}
	if reservedPath(proposal.SourcePath) || reservedPath(proposal.TargetPath) {
		return ErrInvalid
	}
	return nil
}

func cleanPath(value string, allowRoot bool) bool {
	if value == "" {
		return allowRoot
	}
	if len(value) > 4096 || strings.ContainsAny(value, "\\\x00") || path.IsAbs(value) || path.Clean(value) != value {
		return false
	}
	if hasDrivePrefix(value) {
		return false
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || component == "." || component == ".." || len(component) > 255 {
			return false
		}
		for _, r := range component {
			if unicode.IsControl(r) {
				return false
			}
		}
	}
	return true
}

func hasDrivePrefix(value string) bool {
	return len(value) >= 2 && ((value[0] >= 'a' && value[0] <= 'z') || (value[0] >= 'A' && value[0] <= 'Z')) && value[1] == ':'
}

func withinScope(scope, value string) bool {
	return scope == "" || value == scope || strings.HasPrefix(value, scope+"/")
}

func reservedPath(value string) bool {
	for _, component := range strings.Split(value, "/") {
		if component == fsroot.TrashDirectoryName {
			return true
		}
	}
	return false
}

func singleOperation(plan store.PlanRecord, operationID string) (store.OperationRecord, error) {
	if len(plan.Operations) == 0 || len(plan.Operations) > maxStoredPlanOperations(plan.Operations) {
		return store.OperationRecord{}, ErrUnsupported
	}
	for _, op := range plan.Operations {
		if operationID != "" && op.ID != operationID {
			continue
		}
		if !supportedAction(op.Action) {
			return store.OperationRecord{}, ErrUnsupported
		}
		return op, nil
	}
	return store.OperationRecord{}, store.ErrNotFound
}

func maxStoredPlanOperations(operations []store.OperationRecord) int {
	if len(operations) == 0 {
		return store.MaxAgentPlanOperations
	}
	for _, operation := range operations {
		if isArchiveAction(operation.Action) {
			return 1
		}
	}
	return store.MaxAgentPlanOperations
}

// supportedAction 返回当前服务可执行的单项操作。
func supportedAction(action string) bool {
	tool, ok := agentTools()[action]
	return ok && tool.execute != nil
}

func isTextFileWriteAction(action string) bool {
	return action == "edit_text" || action == "create_file" || action == "overwrite_file"
}

func archiveCreateFormat(relativePath string) (fsroot.ArchiveFormat, bool) {
	lower := strings.ToLower(relativePath)
	switch {
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		return fsroot.ArchiveFormatTARGZ, true
	case strings.HasSuffix(lower, ".zip"):
		return fsroot.ArchiveFormatZIP, true
	case strings.HasSuffix(lower, ".7z"):
		return fsroot.ArchiveFormat7Z, true
	case strings.HasSuffix(lower, ".tar"):
		return fsroot.ArchiveFormatTAR, true
	default:
		return "", false
	}
}

func supportedExtractArchivePath(relativePath string) bool {
	lower := strings.ToLower(relativePath)
	return strings.HasSuffix(lower, ".zip") || strings.HasSuffix(lower, ".7z") || strings.HasSuffix(lower, ".tar") ||
		strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tgz") || strings.HasSuffix(lower, ".rar")
}

func validPhotoArchiveTarget(root string, capturedAt time.Time, source, target string) bool {
	if capturedAt.IsZero() || !cleanPath(root, false) {
		return false
	}
	date := capturedAt.UTC()
	datePath := path.Join(root, date.Format("2006"), date.Format("01")) + "/"
	if !strings.HasPrefix(target, datePath) || strings.Contains(strings.TrimPrefix(target, datePath), "/") {
		return false
	}
	name, original := strings.TrimPrefix(target, datePath), path.Base(source)
	if name == original {
		return true
	}
	extension := path.Ext(original)
	stem := strings.TrimSuffix(original, extension)
	if !strings.HasPrefix(name, stem+" (") || !strings.HasSuffix(name, ")"+extension) {
		return false
	}
	suffix := strings.TrimSuffix(strings.TrimPrefix(name, stem+" ("), ")"+extension)
	n, err := strconv.Atoi(suffix)
	return err == nil && n > 0
}

func matchesFingerprint(actual fsroot.Fingerprint, expectedJSON string) bool {
	var expected fsroot.Fingerprint
	if json.Unmarshal([]byte(expectedJSON), &expected) != nil {
		return false
	}
	return actual.Exists == expected.Exists && actual.Kind == expected.Kind && actual.Size == expected.Size &&
		actual.ModifiedAt.Equal(expected.ModifiedAt) && actual.Mode == expected.Mode && actual.Device == expected.Device &&
		actual.Inode == expected.Inode && actual.Links == expected.Links && actual.IdentityAvailable == expected.IdentityAvailable &&
		actual.OwnerUID == expected.OwnerUID && actual.GroupGID == expected.GroupGID && actual.OwnershipAvailable == expected.OwnershipAvailable &&
		actual.ChangeTimeAvailable == expected.ChangeTimeAvailable && actual.ChangeTime.Equal(expected.ChangeTime)
}

// matchesDirectoryIdentity 只比较父目录身份，避免同级内容变化误伤审批。
func matchesDirectoryIdentity(actual fsroot.Fingerprint, expectedJSON string) bool {
	var expected fsroot.Fingerprint
	if json.Unmarshal([]byte(expectedJSON), &expected) != nil {
		return false
	}
	if !actual.Exists || !expected.Exists || actual.Kind != fsroot.KindDirectory || expected.Kind != fsroot.KindDirectory ||
		actual.IdentityAvailable != expected.IdentityAvailable {
		return false
	}
	if !actual.IdentityAvailable {
		return actual.Device == 0 && actual.Inode == 0 && expected.Device == 0 && expected.Inode == 0
	}
	return actual.Device == expected.Device && actual.Inode == expected.Inode
}

// matchesRenameResult 忽略重命名会更新的 ctime，其余身份属性须吻合。
func matchesRenameResult(actual fsroot.Fingerprint, expectedJSON string) bool {
	var expected fsroot.Fingerprint
	if json.Unmarshal([]byte(expectedJSON), &expected) != nil {
		return false
	}
	return actual.Exists == expected.Exists && actual.Kind == expected.Kind && actual.Size == expected.Size &&
		actual.ModifiedAt.Equal(expected.ModifiedAt) && actual.Mode == expected.Mode && actual.Device == expected.Device &&
		actual.Inode == expected.Inode && actual.Links == expected.Links && actual.IdentityAvailable == expected.IdentityAvailable &&
		actual.OwnerUID == expected.OwnerUID && actual.GroupGID == expected.GroupGID && actual.OwnershipAvailable == expected.OwnershipAvailable &&
		actual.ChangeTimeAvailable == expected.ChangeTimeAvailable
}

func randomID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate agent identifier: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}
