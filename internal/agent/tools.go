// 本文件集中登记受限 Agent 工具及其输入、执行策略和处理函数。
package agent

import (
	"context"
	"errors"
	"io/fs"
	"sort"
	"sync"
	"time"

	"filesage/internal/fsroot"
	"filesage/internal/store"
)

// AgentToolCapability 描述可供 Agent 规划的服务端工具能力。
type AgentToolCapability struct {
	ID              string `json:"id"`
	Description     string `json:"description"`
	InputContract   string `json:"inputContract"`
	ExecutionPolicy string `json:"executionPolicy"`
}

type agentTool struct {
	AgentToolCapability
	validate func(Proposal) error
	execute  func(*Service, context.Context, string, string, store.OperationRecord) (store.PlanRecord, error)
}

var (
	agentToolRegistry     map[string]agentTool
	agentToolRegistryOnce sync.Once
)

func agentTools() map[string]agentTool {
	agentToolRegistryOnce.Do(func() { agentToolRegistry = buildAgentToolRegistry() })
	return agentToolRegistry
}

// AgentToolCatalog 返回静态白名单，供后续提示词和界面扩展使用。
func AgentToolCatalog() []AgentToolCapability {
	registry := agentTools()
	tools := make([]AgentToolCapability, 0, len(registry))
	for _, tool := range registry {
		tools = append(tools, tool.AgentToolCapability)
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].ID < tools[j].ID })
	return tools
}

func buildAgentToolRegistry() map[string]agentTool {
	registry := make(map[string]agentTool)
	register := func(id, description, input, policy string,
		validate func(Proposal) error,
		execute func(*Service, context.Context, string, string, store.OperationRecord) (store.PlanRecord, error),
	) {
		registry[id] = agentTool{
			AgentToolCapability: AgentToolCapability{ID: id, Description: description, InputContract: input, ExecutionPolicy: policy},
			validate:            validate,
			execute:             execute,
		}
	}
	proposal := func(id string) func(Proposal) error {
		return func(value Proposal) error {
			if value.Action != id {
				return ErrInvalid
			}
			return validateOperationContract(value)
		}
	}
	register("rename", "在同一目录安全重命名文件", "scopePath, sourcePath, targetPath, reason", "root_scoped_snapshot_write", proposal("rename"),
		executeMoveTool)
	register("edit_text", "编辑单个 UTF-8 文本文件", "scopePath, sourcePath, reason, stagedContent", "root_scoped_snapshot_write", proposal("edit_text"), executeTextTool)
	register("create_file", "创建单个 UTF-8 文本文件", "scopePath, targetPath, reason, stagedContent", "root_scoped_snapshot_write", proposal("create_file"), executeTextTool)
	register("overwrite_file", "覆盖单个 UTF-8 文本文件", "scopePath, sourcePath, reason, stagedContent", "root_scoped_snapshot_write", proposal("overwrite_file"), executeTextTool)
	register("create_directory", "在管理根目录内创建文件夹", "scopePath, targetPath, reason", "root_scoped_snapshot_write", proposal("create_directory"), executeCreateDirectoryTool)
	register("move_file", "移动普通文件到管理范围内的目录", "scopePath, sourcePath, targetPath, reason", "root_scoped_snapshot_write", proposal("move_file"), executeMoveTool)
	register("create_symlink", "创建指向管理根目录内对象的软链接", "scopePath, sourcePath, targetPath, reason", "root_scoped_snapshot_write", proposal("create_symlink"), executeSymlinkTool)
	register("copy_file", "复制普通文件", "scopePath, sourcePath, targetPath, reason", "root_scoped_snapshot_write", proposal("copy_file"), executeCopyFileTool)
	register("copy_directory", "复制不含软链接或特殊文件的目录", "scopePath, sourcePath, targetPath, reason", "root_scoped_tree_snapshot_write", proposal("copy_directory"), executeCopyDirectoryTool)
	register("create_archive", "创建受限格式的压缩包", "scopePath, sourcePath, targetPath, reason", "root_scoped_archive_write", proposal("create_archive"), executeCreateArchiveTool)
	register("extract_archive", "将单个压缩包解压到现有目录", "scopePath, sourcePath, targetPath, reason", "root_scoped_archive_write", proposal("extract_archive"), executeExtractArchiveTool)
	register("set_permissions", "修改文件或文件夹权限", "scopePath, sourcePath, mode, reason", "root_scoped_metadata_write", proposal("set_permissions"), executeMetadataTool)
	register("set_owner", "设置文件或文件夹的数字 UID/GID", "scopePath, sourcePath, ownerUid, groupGid, reason", "root_scoped_metadata_write", proposal("set_owner"), executeMetadataTool)
	register("trash_duplicate", "将重复文件移入可恢复回收站", "scopePath, sourcePath, keeperPath, reason", "root_scoped_trash_write", proposal("trash_duplicate"), executeDuplicateTrashTool)
	register("archive_photo", "按固定索引中的拍摄日期归档照片", "scopePath, sourcePath, targetPath, capturedAt, reason", "root_scoped_archive_write", proposal("archive_photo"), executePhotoArchiveTool)
	register("trash", "将文件或文件夹移入可恢复回收站", "scopePath, sourcePath, reason", "root_scoped_trash_write", proposal("trash"), executeTrashTool)
	register("permanent_delete", "永久删除指定文件或文件夹", "scopePath, sourcePath, reason", "root_scoped_irreversible_write", proposal("permanent_delete"), executePermanentDeleteTool)
	register("run_shell", "在隔离副本中运行 Shell 并提交已审核差异", "scopePath, workingDir, shellCommand", "isolated_shell_overlay_commit", nil, executeShellTool)
	return registry
}

func executeTextTool(s *Service, ctx context.Context, planID, scope string, op store.OperationRecord) (store.PlanRecord, error) {
	return s.executeTextEdit(ctx, planID, op)
}

func executeMoveTool(s *Service, ctx context.Context, planID, scope string, op store.OperationRecord) (store.PlanRecord, error) {
	return s.executeMoveFile(ctx, planID, op)
}

func executeSymlinkTool(s *Service, ctx context.Context, planID, scope string, op store.OperationRecord) (store.PlanRecord, error) {
	return s.executeSymlink(ctx, planID, scope, op)
}

func executeCopyFileTool(s *Service, ctx context.Context, planID, scope string, op store.OperationRecord) (store.PlanRecord, error) {
	return s.executeCopyFile(ctx, planID, op)
}

func executeCopyDirectoryTool(s *Service, ctx context.Context, planID, scope string, op store.OperationRecord) (store.PlanRecord, error) {
	return s.executeCopyDirectory(ctx, planID, op)
}

func executeCreateArchiveTool(s *Service, ctx context.Context, planID, scope string, op store.OperationRecord) (store.PlanRecord, error) {
	return s.executeArchiveCreate(ctx, planID, scope, op)
}

func executeExtractArchiveTool(s *Service, ctx context.Context, planID, scope string, op store.OperationRecord) (store.PlanRecord, error) {
	return s.executeArchiveExtract(ctx, planID, scope, op)
}

func executeMetadataTool(s *Service, ctx context.Context, planID, scope string, op store.OperationRecord) (store.PlanRecord, error) {
	return s.executeMetadata(ctx, planID, op)
}

func executeDuplicateTrashTool(s *Service, ctx context.Context, planID, scope string, op store.OperationRecord) (store.PlanRecord, error) {
	return s.executeDuplicateTrash(ctx, planID, op)
}

func executePhotoArchiveTool(s *Service, ctx context.Context, planID, scope string, op store.OperationRecord) (store.PlanRecord, error) {
	return s.executeArchivePhoto(ctx, planID, scope, op)
}

func executeTrashTool(s *Service, ctx context.Context, planID, scope string, op store.OperationRecord) (store.PlanRecord, error) {
	return s.executeTrash(ctx, planID, op)
}

func executePermanentDeleteTool(s *Service, ctx context.Context, planID, scope string, op store.OperationRecord) (store.PlanRecord, error) {
	return s.executePermanentDelete(ctx, planID, op)
}

func executeShellTool(s *Service, ctx context.Context, planID, _ string, _ store.OperationRecord) (store.PlanRecord, error) {
	plan, err := s.store.Plan(ctx, planID)
	if err != nil {
		return store.PlanRecord{}, err
	}
	return s.executeShellPlan(ctx, plan, []int{plan.Version})
}

func executeCreateDirectoryTool(s *Service, ctx context.Context, planID, scope string, op store.OperationRecord) (store.PlanRecord, error) {
	applyErr := s.createDirectory(op)
	if applyErr != nil {
		message := "目录创建失败，未覆盖已有内容"
		if errors.Is(applyErr, fsroot.ErrConflict) || errors.Is(applyErr, fs.ErrExist) {
			message = "目标路径已被占用，未覆盖已有内容"
		}
		if finishErr := s.store.FinishAgentOperation(ctx, planID, op.ID, store.OperationFailed, message, time.Now().UTC()); finishErr != nil {
			return store.PlanRecord{}, errors.Join(applyErr, finishErr)
		}
		updated, readErr := s.store.Plan(ctx, planID)
		if readErr != nil {
			return store.PlanRecord{}, readErr
		}
		return updated, applyErr
	}
	if err := s.verifyCreatedDirectory(op); err != nil {
		if finishErr := s.store.FinishAgentOperation(ctx, planID, op.ID, store.OperationPartial, "目录创建后无法确认父目录身份，需人工核查", time.Now().UTC()); finishErr != nil {
			return store.PlanRecord{}, errors.Join(err, finishErr)
		}
		updated, readErr := s.store.Plan(ctx, planID)
		if readErr != nil {
			return store.PlanRecord{}, readErr
		}
		return updated, err
	}
	if err := s.store.FinishAgentOperation(ctx, planID, op.ID, store.OperationSucceeded, "目录已创建", time.Now().UTC()); err != nil {
		return store.PlanRecord{}, err
	}
	return s.store.Plan(ctx, planID)
}
