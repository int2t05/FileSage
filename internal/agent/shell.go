// 本文件把隔离 Shell 的变化保存为一个受控 Agent 步骤。
package agent

import (
	"context"
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
	"strings"
	"time"
	"unicode/utf8"

	"filesage/internal/coordination"
	"filesage/internal/fsroot"
	"filesage/internal/shellstage"
	"filesage/internal/store"
)

const maxShellInputBytes = 480 << 20

type ShellProposal struct {
	RequestText      string
	ScopePath        string
	Command          string
	WorkingDirectory string
	ConversationID   string
	ModelProvider    string
	ModelName        string
}

type shellOriginalState struct {
	Fingerprint fsroot.Fingerprint `json:"fingerprint"`
	SHA256      string             `json:"sha256"`
	Mode        uint32             `json:"mode"`
	IsDirectory bool               `json:"isDirectory"`
	Size        int64              `json:"size"`
}

type ShellChangePreview struct {
	Change store.ShellChangeRecord
	Diff   string
}

type shellTreeItem struct {
	entry fsroot.Entry
	path  string
}

// ProposeShell 在私有副本运行命令，并只保存候选文件与变更元数据。
func (s *Service) ProposeShell(ctx context.Context, proposal ShellProposal) (store.PlanRecord, error) {
	if s == nil || s.store == nil || s.root == nil || s.shellPayloads == nil || ctx == nil ||
		!validConversationID(proposal.ConversationID) || strings.TrimSpace(proposal.RequestText) == "" ||
		len(proposal.RequestText) > 2048 || len(proposal.Command) == 0 || len(proposal.Command) > 64<<10 ||
		!cleanPath(proposal.ScopePath, true) || !validShellWorkingDirectory(proposal.WorkingDirectory) {
		return store.PlanRecord{}, ErrInvalid
	}
	if proposal.WorkingDirectory == "" {
		proposal.WorkingDirectory = "."
	}
	if !s.shellDirectoryExists(proposal.ScopePath, proposal.WorkingDirectory) {
		return store.PlanRecord{}, ErrUnsupported
	}
	if err := shellstage.CheckAvailability(ctx); err != nil {
		return store.PlanRecord{}, fmt.Errorf("Shell 沙箱当前不可用: %w", ErrUnsupported)
	}
	stageRoot, err := s.shellPayloads.newRunDirectory()
	if err != nil {
		return store.PlanRecord{}, err
	}
	defer func() { _ = removeShellRunDirectory(stageRoot) }()
	baselineDir, workspaceDir := filepath.Join(stageRoot, "baseline"), filepath.Join(stageRoot, "workspace")
	if err := os.Mkdir(baselineDir, 0o700); err != nil {
		return store.PlanRecord{}, err
	}
	if err := os.Mkdir(workspaceDir, 0o700); err != nil {
		return store.PlanRecord{}, err
	}
	snapshot, items, err := s.copyShellInput(ctx, proposal.ScopePath, baselineDir, workspaceDir)
	if err != nil {
		return store.PlanRecord{}, err
	}
	if err := s.root.VerifyTreeSnapshot(ctx, snapshot); err != nil {
		return store.PlanRecord{}, fmt.Errorf("Shell 输入在暂存期间发生变化: %w", ErrConflict)
	}
	result, err := shellstage.Execute(ctx, shellstage.Request{
		BaselineDir: baselineDir, WorkspaceDir: workspaceDir,
		WorkingDir: proposal.WorkingDirectory, Command: proposal.Command, Limits: shellstage.DefaultLimits(),
	})
	if err != nil {
		return store.PlanRecord{}, fmt.Errorf("Shell 沙箱未完成，原文件未修改: %w", errors.Join(ErrUnsupported, err))
	}
	if result.ExitCode != 0 {
		return store.PlanRecord{}, fmt.Errorf("Shell 命令返回非零状态: %w", ErrUnsupported)
	}
	if err := s.root.VerifyTreeSnapshot(ctx, snapshot); err != nil {
		return store.PlanRecord{}, fmt.Errorf("Shell 运行期间管理目录发生变化，请重新分析: %w", ErrConflict)
	}
	return s.saveShellPlan(ctx, proposal, result, items, workspaceDir, stageRoot)
}

func validShellWorkingDirectory(value string) bool {
	return value == "" || value == "." || cleanPath(value, false)
}

func (s *Service) shellDirectoryExists(scope, workingDirectory string) bool {
	if scope != "" {
		entry, err := s.root.LstatEntry(scope)
		if err != nil || entry.Symlink || entry.Kind != fsroot.KindDirectory {
			return false
		}
	}
	if workingDirectory == "" || workingDirectory == "." {
		return true
	}
	relative := workingDirectory
	if scope != "" {
		relative = path.Join(scope, workingDirectory)
	}
	entry, err := s.root.LstatEntry(relative)
	return err == nil && !entry.Symlink && entry.Kind == fsroot.KindDirectory
}

func (s *Service) copyShellInput(ctx context.Context, scope, baselineDir, workspaceDir string) (fsroot.TreeSnapshot, map[string]fsroot.Entry, error) {
	limits := fsroot.TreeSnapshotLimits{MaxEntries: 20_000, MaxBytes: maxShellInputBytes, MaxPathBytes: 4 << 20}
	snapshot, err := s.root.SnapshotTreeWithLimits(ctx, scope, limits)
	if err != nil {
		return fsroot.TreeSnapshot{}, nil, fmt.Errorf("Shell 范围无法安全暂存: %w", ErrUnsupported)
	}
	items := make(map[string]fsroot.Entry, len(snapshot.Entries))
	directories := make([]string, 0)
	for _, snapshotItem := range snapshot.Entries {
		entry := snapshotItem.Entry
		relative := entry.Path
		if scope != "" {
			switch {
			case entry.Path == scope:
				relative = ""
			case strings.HasPrefix(entry.Path, scope+"/"):
				relative = strings.TrimPrefix(entry.Path, scope+"/")
			default:
				return fsroot.TreeSnapshot{}, nil, ErrUnsupported
			}
		}
		if relative == "" {
			continue
		}
		if !safeShellTreePath(relative) || entry.Symlink || entry.Kind != fsroot.KindFile && entry.Kind != fsroot.KindDirectory {
			return fsroot.TreeSnapshot{}, nil, fmt.Errorf("Shell 范围包含不支持的路径或文件类型: %w", ErrUnsupported)
		}
		if !entry.IdentityAvailable || !entry.ChangeTimeAvailable || entry.Kind == fsroot.KindFile && entry.Links != 1 {
			return fsroot.TreeSnapshot{}, nil, ErrUnsupported
		}
		items[relative] = entry
		if entry.Kind == fsroot.KindDirectory {
			directories = append(directories, relative)
		}
	}
	sort.Slice(directories, func(i, j int) bool {
		if pathDepth(directories[i]) != pathDepth(directories[j]) {
			return pathDepth(directories[i]) < pathDepth(directories[j])
		}
		return directories[i] < directories[j]
	})
	for _, relative := range directories {
		for _, base := range []string{baselineDir, workspaceDir} {
			if err := os.MkdirAll(filepath.Join(base, filepath.FromSlash(relative)), 0o700); err != nil {
				return fsroot.TreeSnapshot{}, nil, err
			}
		}
	}
	files := make([]string, 0, len(items))
	for relative, entry := range items {
		if entry.Kind == fsroot.KindFile {
			files = append(files, relative)
		}
	}
	sort.Strings(files)
	for _, relative := range files {
		entry := items[relative]
		reader, observed, err := s.root.OpenReadNoFollow(entry.Path)
		if err != nil || !sameFingerprint(observed.Fingerprint(), entry.Fingerprint()) {
			if reader != nil {
				_ = reader.Close()
			}
			return fsroot.TreeSnapshot{}, nil, ErrConflict
		}
		basePath := filepath.Join(baselineDir, filepath.FromSlash(relative))
		base, err := os.OpenFile(basePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			_ = reader.Close()
			return fsroot.TreeSnapshot{}, nil, err
		}
		written, copyErr := io.CopyN(base, reader, entry.Size)
		closeReadErr, closeBaseErr := reader.Close(), base.Close()
		if copyErr != nil || written != entry.Size || closeReadErr != nil || closeBaseErr != nil {
			return fsroot.TreeSnapshot{}, nil, errors.Join(copyErr, closeReadErr, closeBaseErr, io.ErrShortWrite)
		}
		if err := os.Chmod(basePath, entry.Mode.Perm()); err != nil {
			return fsroot.TreeSnapshot{}, nil, err
		}
		if err := cloneStagedFile(basePath, filepath.Join(workspaceDir, filepath.FromSlash(relative)), entry.Size, entry.Mode.Perm()); err != nil {
			return fsroot.TreeSnapshot{}, nil, err
		}
	}
	for index := len(directories) - 1; index >= 0; index-- {
		relative := directories[index]
		mode := items[relative].Mode.Perm()
		for _, base := range []string{baselineDir, workspaceDir} {
			if err := os.Chmod(filepath.Join(base, filepath.FromSlash(relative)), mode); err != nil {
				return fsroot.TreeSnapshot{}, nil, err
			}
		}
	}
	return snapshot, items, nil
}

func cloneStagedFile(sourcePath, targetPath string, size int64, mode fs.FileMode) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer source.Close()
	target, err := os.OpenFile(targetPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	written, copyErr := io.CopyN(target, source, size)
	closeErr := target.Close()
	if copyErr != nil || written != size || closeErr != nil {
		return errors.Join(copyErr, closeErr, io.ErrShortWrite)
	}
	return os.Chmod(targetPath, mode)
}

func safeShellTreePath(value string) bool {
	return utf8.ValidString(value) && cleanPath(value, false) && !reservedPath(value)
}

func pathDepth(value string) int {
	return strings.Count(value, "/")
}

func (s *Service) saveShellPlan(ctx context.Context, proposal ShellProposal, result shellstage.Result, original map[string]fsroot.Entry, workspaceDir, stageRoot string) (store.PlanRecord, error) {
	changes := result.Changes
	if len(changes) > 20_000 {
		return store.PlanRecord{}, ErrUnsupported
	}
	now := time.Now().UTC()
	taskID, err := randomID()
	if err != nil {
		return store.PlanRecord{}, err
	}
	planID, err := randomID()
	if err != nil {
		return store.PlanRecord{}, err
	}
	operationID, err := randomID()
	if err != nil {
		return store.PlanRecord{}, err
	}
	runID, err := randomID()
	if err != nil {
		return store.PlanRecord{}, err
	}
	planStatus := store.PlanAwaitingApproval
	taskStatus := store.TaskAwaitingApproval
	operationStatus := store.OperationPending
	runStatus := store.ShellRunAwaitingApproval
	if len(changes) == 0 {
		planStatus, taskStatus, operationStatus, runStatus = store.PlanCompleted, store.TaskSucceeded, store.OperationSucceeded, store.ShellRunCompleted
	}
	parent := store.OperationRecord{
		ID: operationID, PlanID: planID, Ordinal: 0, Action: "run_shell", SourcePath: proposal.ScopePath,
		Reason: strings.TrimSpace(proposal.RequestText), TargetConflictPolicy: "fail",
		Approval: store.ApprovalPending, Status: operationStatus, ResultMessage: "", CreatedAt: now, UpdatedAt: now,
	}
	if len(changes) == 0 {
		parent.ResultMessage = "隔离运行完成，无文件变化"
	}
	plan := store.PlanRecord{
		ID: planID, TaskID: taskID, Version: 1, Status: planStatus, ScopePath: proposal.ScopePath,
		ModelProvider: proposal.ModelProvider, ModelName: proposal.ModelName,
		DataScopeJSON: `{"proposalSource":"conversation","shell":true}`,
		CreatedAt:     now, UpdatedAt: now, Operations: []store.OperationRecord{parent},
	}
	task := store.TaskRecord{
		ID: taskID, Kind: "agent", Status: taskStatus, RequestText: proposal.RequestText,
		ScopePath: proposal.ScopePath, ModelProvider: proposal.ModelProvider, ModelName: proposal.ModelName,
		CreatedAt: now, UpdatedAt: now,
	}
	stdout, stdoutTruncated := boundedShellOutput(result.Stdout)
	stderr, stderrTruncated := boundedShellOutput(result.Stderr)
	run := store.ShellRunRecord{
		ID: runID, PlanID: planID, OperationID: operationID, Command: proposal.Command,
		WorkingDirectory: proposal.WorkingDirectory, Stdout: stdout, Stderr: stderr,
		OutputTruncated: stdoutTruncated || stderrTruncated, ExitCode: result.ExitCode,
		StageID: runID, Status: runStatus,
		CreatedAt: now, UpdatedAt: now,
	}
	storedChanges := make([]store.ShellChangeRecord, 0, len(changes))
	stagedPayloads := make([]string, 0, len(changes))
	committed := false
	defer func() {
		if !committed {
			for _, id := range stagedPayloads {
				_ = s.shellPayloads.remove(id)
			}
		}
	}()
	for ordinal, change := range changes {
		if !safeShellTreePath(change.Path) || change.Path == "." || change.Operation != shellstage.Added && change.Operation != shellstage.Modified && change.Operation != shellstage.Deleted {
			return store.PlanRecord{}, ErrUnsupported
		}
		if change.Before != nil && change.After != nil && change.Before.IsDirectory != change.After.IsDirectory {
			return store.PlanRecord{}, fmt.Errorf("Shell 不支持同一路径的文件与目录类型替换: %w", ErrUnsupported)
		}
		id, err := randomID()
		if err != nil {
			return store.PlanRecord{}, err
		}
		before, hadBefore := original[change.Path]
		state := shellOriginalState{Fingerprint: fsroot.Fingerprint{Exists: false}}
		if change.Before != nil {
			if !hadBefore || before.Symlink || before.Kind == fsroot.KindOther {
				return store.PlanRecord{}, ErrConflict
			}
			state = shellOriginalState{
				Fingerprint: before.Fingerprint(), SHA256: change.Before.SHA256,
				Mode: change.Before.Mode, IsDirectory: change.Before.IsDirectory, Size: change.Before.Size,
			}
		} else if hadBefore {
			return store.PlanRecord{}, ErrConflict
		}
		stateJSON, err := json.Marshal(state)
		if err != nil {
			return store.PlanRecord{}, err
		}
		record := store.ShellChangeRecord{
			ID: id, PlanID: planID, Ordinal: ordinal, Path: change.Path, Kind: string(change.Operation),
			OriginalStateJSON: string(stateJSON), Binary: change.Binary, Truncated: change.DiffTruncated,
			Approval: store.ApprovalPending, Status: store.OperationPending, CreatedAt: now, UpdatedAt: now,
		}
		if change.After != nil {
			record.CandidateSHA256 = change.After.SHA256
			record.CandidateSize = change.After.Size
			mode := change.After.Mode
			record.CandidateMode = &mode
			if !change.After.IsDirectory {
				payloadID, idErr := randomID()
				if idErr != nil {
					return store.PlanRecord{}, idErr
				}
				candidatePath := filepath.Join(workspaceDir, filepath.FromSlash(change.Path))
				if err := s.shellPayloads.stage(payloadID, candidatePath, change.After.SHA256, change.After.Size); err != nil {
					return store.PlanRecord{}, err
				}
				record.PayloadID = payloadID
				stagedPayloads = append(stagedPayloads, payloadID)
			}
		}
		storedChanges = append(storedChanges, record)
	}
	if err := removeShellRunDirectory(stageRoot); err != nil {
		return store.PlanRecord{}, fmt.Errorf("清理 Shell 隔离副本失败，方案未保存: %w", err)
	}
	auditOutcome := "awaiting_approval"
	if len(changes) == 0 {
		auditOutcome = "completed"
	}
	audit := store.AuditRecord{
		TaskID: taskID, PlanID: planID, OperationID: operationID, EventType: "shell_plan_created",
		Actor: "agent", Outcome: auditOutcome, Message: fmt.Sprintf("隔离 Shell 分析完成，共发现 %d 项文件变化", len(changes)),
		SourcePath: proposal.ScopePath, CreatedAt: now,
	}
	if err := s.store.CreateConversationShellTaskPlan(ctx, proposal.ConversationID, task, plan, run, storedChanges, audit, ""); err != nil {
		return store.PlanRecord{}, err
	}
	committed = true
	return s.store.Plan(ctx, planID)
}

// boundedShellOutput 保留有效 UTF-8，并限制单路持久化输出大小。
func boundedShellOutput(value string) (string, bool) {
	value = strings.ToValidUTF8(value, "�")
	if len(value) <= store.MaxShellOutputBytes {
		return value, false
	}
	value = value[:store.MaxShellOutputBytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value, true
}

func hashShellPath(root *fsroot.Root, relativePath string, expectedSize int64) (string, error) {
	reader, _, err := root.OpenReadNoFollow(relativePath)
	if err != nil {
		return "", err
	}
	defer reader.Close()
	digest := sha256.New()
	written, err := io.CopyN(digest, reader, expectedSize)
	if err != nil || written != expectedSize {
		return "", errors.Join(err, fsroot.ErrSizeMismatch)
	}
	if err := requireShellEOF(reader); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// ShellRun 返回审核界面和续聊所需的限长命令回执。
func (s *Service) ShellRun(ctx context.Context, planID string) (store.ShellRunRecord, error) {
	if s == nil || s.store == nil {
		return store.ShellRunRecord{}, ErrUnsupported
	}
	return s.store.ShellRun(ctx, planID)
}

// ShellChanges 返回方案的固定顺序变更分页。
func (s *Service) ShellChanges(ctx context.Context, planID string, limit, offset int) (store.ShellChangePage, error) {
	if s == nil || s.store == nil {
		return store.ShellChangePage{}, ErrUnsupported
	}
	return s.store.ListShellChanges(ctx, planID, limit, offset)
}

// PreviewShellChange 仅在用户展开预览时读取原文和候选正文。
func (s *Service) PreviewShellChange(ctx context.Context, planID, changeID string) (ShellChangePreview, error) {
	if s == nil || s.store == nil || s.root == nil || s.shellPayloads == nil {
		return ShellChangePreview{}, ErrUnsupported
	}
	plan, err := s.store.Plan(ctx, planID)
	if err != nil {
		return ShellChangePreview{}, err
	}
	change, err := s.shellChange(ctx, planID, changeID)
	if err != nil {
		return ShellChangePreview{}, err
	}
	if change.Binary {
		return ShellChangePreview{}, ErrUnsupported
	}
	if change.Status == store.OperationSucceeded && change.Kind == "added" {
		if err := s.verifyShellCandidate(plan.ScopePath, change); err != nil {
			return ShellChangePreview{}, ErrConflict
		}
		after, err := s.readShellFile(ctx, shellRootPath(plan.ScopePath, change.Path), change.CandidateSize)
		if err != nil {
			return ShellChangePreview{}, ErrConflict
		}
		diff, truncated := shellstage.FormatTextDiff(nil, after, change.Path, 64<<10)
		change.Truncated = change.Truncated || truncated
		return ShellChangePreview{Change: change, Diff: diff}, nil
	}
	if (plan.Status != store.PlanAwaitingApproval && plan.Status != store.PlanApproved) || change.Status != store.OperationPending ||
		change.Approval != store.ApprovalPending && change.Approval != store.ApprovalApproved {
		return ShellChangePreview{}, ErrConflict
	}
	state, err := decodeShellOriginalState(change)
	if err != nil {
		return ShellChangePreview{}, err
	}
	if err := s.verifyShellOriginal(ctx, plan.ScopePath, change, state); err != nil {
		return ShellChangePreview{}, ErrConflict
	}
	if state.IsDirectory || change.CandidateMode != nil && change.CandidateSHA256 == "" {
		return ShellChangePreview{Change: change, Diff: "目录变化不包含正文差异。"}, nil
	}
	const maxPreviewFileBytes = 1 << 20
	if state.Size > maxPreviewFileBytes || change.CandidateSize > maxPreviewFileBytes {
		change.Truncated = true
		return ShellChangePreview{Change: change, Diff: "文件较大，仅显示变化摘要。"}, nil
	}
	var before []byte
	if state.Fingerprint.Exists {
		before, err = s.readShellFile(ctx, shellRootPath(plan.ScopePath, change.Path), state.Size)
		if err != nil {
			return ShellChangePreview{}, ErrConflict
		}
		digest := sha256.Sum256(before)
		entry, statErr := s.root.LstatEntry(shellRootPath(plan.ScopePath, change.Path))
		if hex.EncodeToString(digest[:]) != state.SHA256 || statErr != nil || !sameFileFingerprint(entry.Fingerprint(), state.Fingerprint) {
			return ShellChangePreview{}, ErrConflict
		}
	}
	var after []byte
	if change.CandidateSHA256 != "" {
		payload, openErr := s.shellPayloads.open(change.PayloadID, change.CandidateSHA256, change.CandidateSize)
		if openErr != nil {
			return ShellChangePreview{}, ErrConflict
		}
		after, err = io.ReadAll(io.LimitReader(payload, maxPreviewFileBytes+1))
		closeErr := payload.Close()
		if err != nil || closeErr != nil || int64(len(after)) != change.CandidateSize {
			return ShellChangePreview{}, ErrConflict
		}
	}
	diff, truncated := shellstage.FormatTextDiff(before, after, change.Path, 64<<10)
	change.Truncated = change.Truncated || truncated
	return ShellChangePreview{Change: change, Diff: diff}, nil
}

// DecideShellChange 复核当前文件版本后逐项记录用户审批。
func (s *Service) DecideShellChange(ctx context.Context, planID, changeID, decision string, expectedVersion int) (store.PlanRecord, error) {
	if s == nil || s.store == nil || s.root == nil || s.shellPayloads == nil || expectedVersion < 1 || decision != "approve" && decision != "reject" {
		return store.PlanRecord{}, ErrInvalid
	}
	plan, err := s.store.Plan(ctx, planID)
	if err != nil {
		return store.PlanRecord{}, err
	}
	if plan.Version != expectedVersion || plan.Status != store.PlanAwaitingApproval || len(plan.Operations) != 1 || plan.Operations[0].Action != "run_shell" {
		return store.PlanRecord{}, ErrConflict
	}
	change, err := s.shellChange(ctx, planID, changeID)
	if err != nil {
		return store.PlanRecord{}, err
	}
	if change.Status != store.OperationPending || change.Approval != store.ApprovalPending {
		return store.PlanRecord{}, ErrConflict
	}
	keys := shellChangeLockKeys(plan.ScopePath, []store.ShellChangeRecord{change})
	release, err := s.locks.Lock(ctx, keys...)
	if err != nil {
		return store.PlanRecord{}, err
	}
	defer release()
	latest, err := s.store.Plan(ctx, planID)
	if err != nil {
		return store.PlanRecord{}, err
	}
	if latest.Version != expectedVersion || latest.Status != store.PlanAwaitingApproval {
		return store.PlanRecord{}, ErrConflict
	}
	if decision == "approve" {
		state, stateErr := decodeShellOriginalState(change)
		if stateErr != nil {
			return store.PlanRecord{}, stateErr
		}
		if err := s.verifyShellOriginal(ctx, plan.ScopePath, change, state); err != nil {
			return s.invalidateShellPlan(ctx, plan, change, "审批时文件状态已变化，请重新分析。", err)
		}
	}
	approval := store.ApprovalApproved
	if decision == "reject" {
		approval = store.ApprovalRejected
	}
	if err := s.store.SetShellChangeApproval(ctx, planID, changeID, approval, expectedVersion, time.Now().UTC()); err != nil {
		return store.PlanRecord{}, err
	}
	updated, err := s.store.Plan(ctx, planID)
	if decision == "reject" {
		cleanupErr := s.removeShellPlanPayloads(ctx, planID)
		if err != nil {
			return store.PlanRecord{}, errors.Join(err, cleanupErr)
		}
		return updated, cleanupErr
	}
	return updated, err
}

func (s *Service) shellChange(ctx context.Context, planID, changeID string) (store.ShellChangeRecord, error) {
	for offset := 0; offset < store.MaxShellChanges; offset += store.MaxShellChangePageSize {
		page, err := s.store.ListShellChanges(ctx, planID, store.MaxShellChangePageSize, offset)
		if err != nil {
			return store.ShellChangeRecord{}, err
		}
		for _, change := range page.Changes {
			if change.ID == changeID {
				return change, nil
			}
		}
		if !page.HasMore {
			break
		}
	}
	return store.ShellChangeRecord{}, store.ErrNotFound
}

func (s *Service) shellPlanChanges(ctx context.Context, planID string) ([]store.ShellChangeRecord, error) {
	changes := make([]store.ShellChangeRecord, 0)
	for offset := 0; offset < store.MaxShellChanges; offset += store.MaxShellChangePageSize {
		page, err := s.store.ListShellChanges(ctx, planID, store.MaxShellChangePageSize, offset)
		if err != nil {
			return nil, err
		}
		changes = append(changes, page.Changes...)
		if !page.HasMore {
			return changes, nil
		}
	}
	return nil, ErrUnsupported
}

func decodeShellOriginalState(change store.ShellChangeRecord) (shellOriginalState, error) {
	var state shellOriginalState
	if err := json.Unmarshal([]byte(change.OriginalStateJSON), &state); err != nil || state.Size < 0 || state.Mode > 0o777 || state.Fingerprint.Exists == (change.Kind == "added") {
		return shellOriginalState{}, ErrInvalid
	}
	validOriginal := !state.Fingerprint.Exists || state.IsDirectory && state.SHA256 == "" && state.Size == 0 && state.Fingerprint.Kind == fsroot.KindDirectory ||
		!state.IsDirectory && validSHA256(state.SHA256) && state.Size == state.Fingerprint.Size && state.Fingerprint.Kind == fsroot.KindFile
	if state.Fingerprint.Exists != (change.Kind == "modified" || change.Kind == "deleted") || !validOriginal ||
		!state.Fingerprint.Exists && (state.SHA256 != "" || state.Size != 0 || state.Mode != 0 || state.IsDirectory) {
		return shellOriginalState{}, ErrInvalid
	}
	return state, nil
}

func shellRootPath(scope, relative string) string {
	if scope == "" {
		return relative
	}
	return path.Join(scope, relative)
}

func (s *Service) verifyShellOriginal(ctx context.Context, scope string, change store.ShellChangeRecord, state shellOriginalState) error {
	relative := shellRootPath(scope, change.Path)
	entry, err := s.root.LstatEntry(relative)
	if !state.Fingerprint.Exists {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fsroot.ErrConflict
	}
	if err != nil || entry.Symlink || !sameFileFingerprint(entry.Fingerprint(), state.Fingerprint) {
		return fsroot.ErrConflict
	}
	if state.IsDirectory {
		if entry.Kind != fsroot.KindDirectory || state.SHA256 != "" || state.Size != 0 {
			return fsroot.ErrConflict
		}
		return nil
	}
	if entry.Kind != fsroot.KindFile || entry.Size != state.Size || !validSHA256(state.SHA256) {
		return fsroot.ErrConflict
	}
	digest, err := hashShellEntry(ctx, s.root, relative, entry)
	if err != nil || digest != state.SHA256 {
		return fsroot.ErrConflict
	}
	return nil
}

func hashShellEntry(ctx context.Context, root *fsroot.Root, relative string, expected fsroot.Entry) (string, error) {
	reader, opened, err := root.OpenReadNoFollow(relative)
	if err != nil {
		return "", err
	}
	defer reader.Close()
	if opened.Symlink || opened.Kind != fsroot.KindFile || !sameFileFingerprint(opened.Fingerprint(), expected.Fingerprint()) {
		return "", fsroot.ErrConflict
	}
	digest := sha256.New()
	written, err := io.CopyN(digest, contextReader{ctx: ctx, reader: reader}, expected.Size)
	if err != nil || written != expected.Size {
		return "", errors.Join(err, fsroot.ErrSizeMismatch)
	}
	if err := requireShellEOF(reader); err != nil {
		return "", err
	}
	after, err := root.LstatEntry(relative)
	if err != nil || !sameFileFingerprint(opened.Fingerprint(), after.Fingerprint()) {
		return "", fsroot.ErrConflict
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func (s *Service) readShellFile(ctx context.Context, relative string, expectedSize int64) ([]byte, error) {
	reader, entry, err := s.root.OpenReadNoFollow(relative)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	if entry.Symlink || entry.Kind != fsroot.KindFile || entry.Size != expectedSize {
		return nil, fsroot.ErrConflict
	}
	content, err := io.ReadAll(contextReader{ctx: ctx, reader: io.LimitReader(reader, expectedSize+1)})
	if err != nil || int64(len(content)) != expectedSize {
		return nil, errors.Join(err, fsroot.ErrSizeMismatch)
	}
	return content, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}

func shellChangeLockKeys(scope string, changes []store.ShellChangeRecord) []string {
	paths := make([]string, 0, len(changes))
	keys := make([]string, 0, len(changes))
	for _, change := range changes {
		paths = append(paths, shellRootPath(scope, change.Path))
		state, err := decodeShellOriginalState(change)
		if err == nil && state.Fingerprint.Exists && state.Fingerprint.IdentityAvailable && state.Fingerprint.Inode != 0 {
			keys = append(keys, coordination.InodeKey(state.Fingerprint.Device, state.Fingerprint.Inode))
		}
	}
	keys = append(keys, coordination.PathKeys(paths...)...)
	return keys
}

func (s *Service) invalidateShellPlan(ctx context.Context, plan store.PlanRecord, change store.ShellChangeRecord, message string, cause error) (store.PlanRecord, error) {
	if err := s.store.InvalidateShellPlan(ctx, plan.ID, change.ID, message, time.Now().UTC()); err != nil {
		return store.PlanRecord{}, errors.Join(cause, err)
	}
	cleanupErr := s.removeShellPlanPayloads(ctx, plan.ID)
	updated, readErr := s.store.Plan(ctx, plan.ID)
	if readErr != nil {
		return store.PlanRecord{}, errors.Join(ErrConflict, cause, cleanupErr, readErr)
	}
	return updated, errors.Join(ErrConflict, cause, cleanupErr)
}

func (s *Service) removeShellPlanPayloads(ctx context.Context, planID string) error {
	changes, err := s.shellPlanChanges(ctx, planID)
	if err != nil {
		return err
	}
	var cleanupErr error
	for _, change := range changes {
		if change.PayloadID != "" {
			cleanupErr = errors.Join(cleanupErr, s.shellPayloads.remove(change.PayloadID))
		}
	}
	return cleanupErr
}

func isShellParent(plan store.PlanRecord) bool {
	return len(plan.Operations) == 1 && plan.Operations[0].Action == "run_shell"
}

func (s *Service) executeShellPlan(ctx context.Context, plan store.PlanRecord, expectedVersion []int) (store.PlanRecord, error) {
	if len(expectedVersion) > 1 || len(expectedVersion) == 1 && expectedVersion[0] != plan.Version ||
		plan.Status != store.PlanApproved || len(plan.Operations) != 1 || plan.Operations[0].Approval != store.ApprovalApproved ||
		plan.Operations[0].Status != store.OperationPending || s.shellPayloads == nil || s.trash == nil {
		return store.PlanRecord{}, ErrConflict
	}
	changes, err := s.shellPlanChanges(ctx, plan.ID)
	if err != nil {
		return store.PlanRecord{}, err
	}
	if len(changes) == 0 || len(changes) > store.MaxShellChanges {
		return store.PlanRecord{}, ErrConflict
	}
	for _, change := range changes {
		if change.Approval != store.ApprovalApproved || change.Status != store.OperationPending {
			return store.PlanRecord{}, ErrConflict
		}
	}
	release, err := s.locks.Lock(ctx, shellChangeLockKeys(plan.ScopePath, changes)...)
	if err != nil {
		return store.PlanRecord{}, err
	}
	defer release()
	latest, err := s.store.Plan(ctx, plan.ID)
	if err != nil {
		return store.PlanRecord{}, err
	}
	if latest.Version != plan.Version || latest.Status != store.PlanApproved || !isShellParent(latest) {
		return store.PlanRecord{}, ErrConflict
	}
	for _, change := range changes {
		state, stateErr := decodeShellOriginalState(change)
		if stateErr != nil {
			return store.PlanRecord{}, stateErr
		}
		if err := s.verifyShellOriginal(ctx, latest.ScopePath, change, state); err != nil {
			return s.invalidateShellPlan(ctx, latest, change, "执行前文件状态已变化，方案失效，请重新分析。", err)
		}
		if change.Kind != "deleted" && change.CandidateSHA256 != "" {
			payload, openErr := s.shellPayloads.open(change.PayloadID, change.CandidateSHA256, change.CandidateSize)
			if openErr != nil {
				return store.PlanRecord{}, ErrConflict
			}
			if closeErr := payload.Close(); closeErr != nil {
				return store.PlanRecord{}, closeErr
			}
		}
	}
	if err := s.store.StartShellExecution(ctx, plan.ID, plan.Version, time.Now().UTC()); err != nil {
		return store.PlanRecord{}, err
	}
	ordered := orderShellChanges(latest.ScopePath, changes)
	succeeded := 0
	stagedDirectories := make(map[string]fsroot.Fingerprint)
	for index, change := range ordered {
		current, readErr := s.store.Plan(ctx, plan.ID)
		if readErr != nil {
			return store.PlanRecord{}, readErr
		}
		if current.Status != store.PlanExecuting || current.Version != plan.Version {
			return current, ErrConflict
		}
		if ctx.Err() != nil || index > 0 && current.StopRequested {
			if err := s.finishStagedShellDirectories(ctx, plan.ID, ordered, stagedDirectories, plan.Version); err != nil {
				return store.PlanRecord{}, err
			}
			return s.finishShellPlan(ctx, plan.ID, store.ShellRunCancelled, "已停止后续文件变化。", plan.Version)
		}
		if err := s.store.StartShellChange(ctx, plan.ID, change.ID, plan.Version, time.Now().UTC()); err != nil {
			return store.PlanRecord{}, err
		}
		applyErr := s.applyShellChange(ctx, latest, change)
		if applyErr == nil && isAddedShellDirectory(change) {
			relative := shellRootPath(latest.ScopePath, change.Path)
			entry, statErr := s.root.LstatEntry(relative)
			if statErr != nil || entry.Symlink || entry.Kind != fsroot.KindDirectory || !entry.IdentityAvailable {
				applyErr = errors.Join(ErrConflict, statErr)
			} else {
				stagedDirectories[change.ID] = entry.Fingerprint()
				continue
			}
		}
		changeStatus, message := classifyShellApply(ctx, s, latest, change, applyErr)
		persistCtx, cancel := shellPersistenceContext(ctx)
		finishErr := s.store.FinishShellChange(persistCtx, plan.ID, change.ID, changeStatus, message, plan.Version, time.Now().UTC())
		cancel()
		if finishErr != nil {
			return store.PlanRecord{}, errors.Join(applyErr, finishErr)
		}
		if changeStatus == store.OperationSucceeded {
			succeeded++
			continue
		}
		if err := s.finishStagedShellDirectories(ctx, plan.ID, ordered, stagedDirectories, plan.Version); err != nil {
			return store.PlanRecord{}, errors.Join(applyErr, err)
		}
		runStatus := store.ShellRunFailed
		if succeeded > 0 {
			runStatus = store.ShellRunPartial
		}
		return s.finishShellPlan(ctx, plan.ID, runStatus, "文件变化未能全部提交，已完成部分保持不变，请核查状态后重新分析。", plan.Version)
	}
	for index := len(ordered) - 1; index >= 0; index-- {
		change := ordered[index]
		initial, pending := stagedDirectories[change.ID]
		if !pending {
			continue
		}
		current, readErr := s.store.Plan(ctx, plan.ID)
		if readErr != nil {
			return store.PlanRecord{}, readErr
		}
		if ctx.Err() != nil || current.StopRequested {
			if err := s.finishStagedShellDirectories(ctx, plan.ID, ordered, stagedDirectories, plan.Version); err != nil {
				return store.PlanRecord{}, err
			}
			return s.finishShellPlan(ctx, plan.ID, store.ShellRunCancelled, "已停止后续文件变化。", plan.Version)
		}
		applyErr := s.finalizeAddedShellDirectory(latest, change, initial)
		changeStatus, message := classifyShellApply(ctx, s, latest, change, applyErr)
		persistCtx, cancel := shellPersistenceContext(ctx)
		finishErr := s.store.FinishShellChange(persistCtx, plan.ID, change.ID, changeStatus, message, plan.Version, time.Now().UTC())
		cancel()
		delete(stagedDirectories, change.ID)
		if finishErr != nil {
			return store.PlanRecord{}, errors.Join(applyErr, finishErr)
		}
		if changeStatus == store.OperationSucceeded {
			succeeded++
			continue
		}
		if err := s.finishStagedShellDirectories(ctx, plan.ID, ordered, stagedDirectories, plan.Version); err != nil {
			return store.PlanRecord{}, errors.Join(applyErr, err)
		}
		runStatus := store.ShellRunFailed
		if succeeded > 0 {
			runStatus = store.ShellRunPartial
		}
		return s.finishShellPlan(ctx, plan.ID, runStatus, "目录权限提交未能全部完成，已完成部分保持不变，请核查状态。", plan.Version)
	}
	return s.finishShellPlan(ctx, plan.ID, store.ShellRunCompleted, "已按审批清单提交全部文件变化。", plan.Version)
}

func isAddedShellDirectory(change store.ShellChangeRecord) bool {
	return change.Kind == "added" && change.CandidateSHA256 == "" && change.CandidateMode != nil
}

func (s *Service) finishStagedShellDirectories(ctx context.Context, planID string, changes []store.ShellChangeRecord, staged map[string]fsroot.Fingerprint, version int) error {
	for index := len(changes) - 1; index >= 0; index-- {
		change := changes[index]
		if _, pending := staged[change.ID]; !pending {
			continue
		}
		persistCtx, cancel := shellPersistenceContext(ctx)
		err := s.store.FinishShellChange(persistCtx, planID, change.ID, store.OperationPartial,
			"目录已创建，但最终权限未能确认，请人工核查。", version, time.Now().UTC())
		cancel()
		if err != nil {
			return err
		}
		delete(staged, change.ID)
	}
	return nil
}

func (s *Service) finalizeAddedShellDirectory(plan store.PlanRecord, change store.ShellChangeRecord, initial fsroot.Fingerprint) error {
	relative := shellRootPath(plan.ScopePath, change.Path)
	entry, err := s.root.LstatEntry(relative)
	if err != nil || entry.Symlink || entry.Kind != fsroot.KindDirectory || !entry.IdentityAvailable ||
		entry.Device != initial.Device || entry.Inode != initial.Inode || change.CandidateMode == nil {
		return errors.Join(ErrConflict, err)
	}
	mode := fs.FileMode(*change.CandidateMode)
	if _, err := s.root.UpdateMetadata(relative, entry.Fingerprint(), fsroot.MetadataUpdate{Mode: &mode}); err != nil {
		return err
	}
	return s.verifyShellCandidate(plan.ScopePath, change)
}

func orderShellChanges(scope string, changes []store.ShellChangeRecord) []store.ShellChangeRecord {
	ordered := append([]store.ShellChangeRecord(nil), changes...)
	phase := func(change store.ShellChangeRecord) int {
		state, _ := decodeShellOriginalState(change)
		switch {
		case change.Kind == "added" && change.CandidateSHA256 == "" && change.CandidateMode != nil:
			return 0
		case change.Kind == "added" || change.Kind == "modified" && !state.IsDirectory:
			return 1
		case change.Kind == "deleted" && !state.IsDirectory:
			return 2
		case change.Kind == "modified" && state.IsDirectory:
			return 3
		default:
			return 4
		}
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		left, right := phase(ordered[i]), phase(ordered[j])
		if left != right {
			return left < right
		}
		leftDepth := pathDepth(shellRootPath(scope, ordered[i].Path))
		rightDepth := pathDepth(shellRootPath(scope, ordered[j].Path))
		if left == 0 {
			if leftDepth != rightDepth {
				return leftDepth < rightDepth
			}
		} else if left == 3 || left == 4 {
			if leftDepth != rightDepth {
				return leftDepth > rightDepth
			}
		}
		return ordered[i].Path < ordered[j].Path
	})
	return ordered
}

func (s *Service) applyShellChange(ctx context.Context, plan store.PlanRecord, change store.ShellChangeRecord) error {
	state, err := decodeShellOriginalState(change)
	if err != nil {
		return err
	}
	relative := shellRootPath(plan.ScopePath, change.Path)
	if change.Kind == "deleted" {
		if s.trash == nil {
			return ErrUnsupported
		}
		entry, err := s.root.LstatEntry(relative)
		if err != nil || entry.Symlink {
			return errors.Join(ErrConflict, err)
		}
		if state.IsDirectory {
			if !sameShellDirectoryIdentity(entry, state.Fingerprint) {
				return ErrConflict
			}
			children, listErr := s.root.List(relative)
			if listErr != nil || len(children) != 0 {
				return errors.Join(ErrConflict, listErr)
			}
		} else if !sameFileFingerprint(entry.Fingerprint(), state.Fingerprint) || entry.Kind != fsroot.KindFile {
			return ErrConflict
		} else if verifyErr := s.verifyShellOriginal(ctx, plan.ScopePath, change, state); verifyErr != nil {
			return verifyErr
		}
		_, err = s.trash.MoveToTrashForAgent(ctx, entry, plan.TaskID, plan.ID, plan.Operations[0].ID, func() error {
			current, statErr := s.root.LstatEntry(relative)
			if statErr != nil || current.Symlink {
				return errors.Join(ErrConflict, statErr)
			}
			if state.IsDirectory {
				children, listErr := s.root.List(relative)
				if !sameShellDirectoryIdentity(current, state.Fingerprint) || listErr != nil || len(children) != 0 {
					return errors.Join(ErrConflict, listErr)
				}
				return nil
			}
			return s.verifyShellOriginal(ctx, plan.ScopePath, change, state)
		})
		return err
	}
	if change.CandidateMode == nil {
		return ErrInvalid
	}
	mode := fs.FileMode(*change.CandidateMode)
	if change.Kind == "added" && change.CandidateSHA256 == "" {
		if err := s.verifyShellOriginal(ctx, plan.ScopePath, change, state); err != nil {
			return err
		}
		return s.root.Mkdir(relative, 0o700)
	}
	if state.IsDirectory {
		entry, err := s.root.LstatEntry(relative)
		if err != nil || !sameShellDirectoryIdentity(entry, state.Fingerprint) {
			return errors.Join(ErrConflict, err)
		}
		if entry.Mode.Perm() != state.Fingerprint.Mode.Perm() {
			return ErrConflict
		}
		_, err = s.root.UpdateMetadata(relative, entry.Fingerprint(), fsroot.MetadataUpdate{Mode: &mode})
		return err
	}
	if err := s.verifyShellOriginal(ctx, plan.ScopePath, change, state); err != nil {
		return err
	}
	payload, err := s.shellPayloads.open(change.PayloadID, change.CandidateSHA256, change.CandidateSize)
	if err != nil {
		return err
	}
	defer payload.Close()
	digest, err := s.root.WriteAtomicFrom(relative, state.Fingerprint, contextReader{ctx: ctx, reader: payload}, change.CandidateSize, mode)
	if err != nil {
		return err
	}
	if digest != change.CandidateSHA256 {
		return fsroot.ErrConflict
	}
	return nil
}

func sameShellDirectoryIdentity(entry fsroot.Entry, expected fsroot.Fingerprint) bool {
	return !entry.Symlink && entry.Kind == fsroot.KindDirectory && entry.IdentityAvailable && expected.IdentityAvailable &&
		entry.Device == expected.Device && entry.Inode == expected.Inode && entry.Mode.Perm() == expected.Mode.Perm()
}

func classifyShellApply(ctx context.Context, service *Service, plan store.PlanRecord, change store.ShellChangeRecord, applyErr error) (store.OperationStatus, string) {
	if applyErr == nil {
		if service.verifyShellCandidate(plan.ScopePath, change) == nil {
			return store.OperationSucceeded, "已提交此项文件变化。"
		}
		return store.OperationPartial, "写入后的文件状态无法验证，请人工核查。"
	}
	if service.verifyShellCandidate(plan.ScopePath, change) == nil {
		return store.OperationPartial, "文件已发生变化，但提交结果无法确认，请人工核查。"
	}
	state, stateErr := decodeShellOriginalState(change)
	if stateErr == nil && service.verifyShellOriginal(ctx, plan.ScopePath, change, state) == nil {
		return store.OperationFailed, "文件状态与审批一致，未能提交此项变化。"
	}
	return store.OperationPartial, "文件状态与审批不一致，请人工核查。"
}

func (s *Service) verifyShellCandidate(scope string, change store.ShellChangeRecord) error {
	relative := shellRootPath(scope, change.Path)
	entry, err := s.root.LstatEntry(relative)
	if change.Kind == "deleted" {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fsroot.ErrConflict
	}
	if err != nil || entry.Symlink || change.CandidateMode == nil || entry.Mode.Perm() != fs.FileMode(*change.CandidateMode).Perm() {
		return fsroot.ErrConflict
	}
	if change.CandidateSHA256 == "" {
		if entry.Kind != fsroot.KindDirectory {
			return fsroot.ErrConflict
		}
		return nil
	}
	if entry.Kind != fsroot.KindFile || entry.Size != change.CandidateSize {
		return fsroot.ErrConflict
	}
	digest, err := hashShellEntry(context.Background(), s.root, relative, entry)
	if err != nil || digest != change.CandidateSHA256 {
		return fsroot.ErrConflict
	}
	return nil
}

func shellPersistenceContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
}

func (s *Service) finishShellPlan(ctx context.Context, planID string, status store.ShellRunStatus, message string, version int) (store.PlanRecord, error) {
	persistCtx, cancel := shellPersistenceContext(ctx)
	finishErr := s.store.FinishShellExecution(persistCtx, planID, status, message, version, time.Now().UTC())
	cleanupErr := s.removeShellPlanPayloads(persistCtx, planID)
	plan, readErr := s.store.Plan(persistCtx, planID)
	cancel()
	if finishErr != nil || cleanupErr != nil || readErr != nil {
		return plan, errors.Join(finishErr, cleanupErr, readErr)
	}
	return plan, nil
}

// recoverShellPlan 将重启期间的运行标为需核查，并且不会重放命令或写回。
func (s *Service) recoverShellPlan(ctx context.Context, plan store.PlanRecord) error {
	changes, err := s.shellPlanChanges(ctx, plan.ID)
	if err != nil {
		return err
	}
	release, err := s.locks.Lock(ctx, shellChangeLockKeys(plan.ScopePath, changes)...)
	if err != nil {
		return err
	}
	defer release()
	latest, err := s.store.Plan(ctx, plan.ID)
	if err != nil {
		return err
	}
	if latest.Version != plan.Version || !isShellParent(latest) || latest.Status != store.PlanExecuting {
		return ErrConflict
	}
	var succeeded, running, ambiguousRunning int
	for _, change := range changes {
		switch change.Status {
		case store.OperationSucceeded:
			succeeded++
		case store.OperationRunning:
			running++
			if !isAddedShellDirectory(change) {
				ambiguousRunning++
			}
		}
	}
	if ambiguousRunning > 1 {
		return ErrConflict
	}
	for _, change := range changes {
		if change.Status != store.OperationRunning {
			continue
		}
		persistCtx, cancel := shellPersistenceContext(ctx)
		err := s.store.FinishShellChange(persistCtx, plan.ID, change.ID, store.OperationPartial, "服务重启时此项写回状态不确定，请人工核查。", latest.Version, time.Now().UTC())
		cancel()
		if err != nil {
			return err
		}
	}
	status := store.ShellRunCancelled
	message := "服务重启后已停止后续写回；隔离命令不会自动重跑。"
	if running > 0 {
		status = store.ShellRunFailed
		if succeeded > 0 {
			status = store.ShellRunPartial
		}
		message = "服务重启时写回结果不确定，请人工核查后重新分析。"
	}
	persistCtx, cancel := shellPersistenceContext(ctx)
	finishErr := s.store.FinishShellExecution(persistCtx, plan.ID, status, message, latest.Version, time.Now().UTC())
	cleanupErr := s.removeShellPlanPayloads(persistCtx, plan.ID)
	cancel()
	return errors.Join(finishErr, cleanupErr)
}
