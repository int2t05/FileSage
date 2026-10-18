// 本文件持久化 Agent 对话状态，并原子关联审批方案。
package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var (
	ErrConversationNotFound = errors.New("agent conversation not found")
	ErrConversationConflict = errors.New("agent conversation state conflict")
	ErrConversationLimit    = errors.New("agent conversation limit reached")
)

type ConversationState string

const (
	ConversationProcessing           ConversationState = "processing"
	ConversationClarifying           ConversationState = "clarifying"
	ConversationAwaitingConfirmation ConversationState = "awaiting_confirmation"
	ConversationAnalyzing            ConversationState = "analyzing"
	ConversationAwaitingApproval     ConversationState = "awaiting_approval"
	ConversationAnalysisComplete     ConversationState = "analysis_complete"
	ConversationUnsupported          ConversationState = "unsupported"
	ConversationFailed               ConversationState = "failed"
	maxConversationMessages                            = 100
)

const (
	MaxAnalysisTextFileBytes     int64 = 64 << 10
	MaxAnalysisTextTotalBytes    int64 = 256 << 10
	MaxAnalysisDocumentFileBytes int64 = 16 << 20
	MaxAnalysisSourceTotalBytes  int64 = 64 << 20
)

type ConversationMessage struct {
	Role      string
	Content   string
	CreatedAt time.Time
}

type AnalysisCard struct {
	Operations        []AnalysisOperation
	Action            string
	Goal              string
	FilePath          string
	TargetPath        string
	RequestedMode     *uint32
	RequestedOwnerUID *uint32
	RequestedGroupGID *uint32
	DataTypes         []string
	Provider          string
	Model             string
	Destination       string
}

// AnalysisFile 保存候选路径、大小及不含正文的版本令牌。
type AnalysisFile struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"sizeBytes"`
	Version   string `json:"version"`
}

// AnalysisCategory 保存分类根目录下一个真实类别目录的稳定编号。
type AnalysisCategory struct {
	ID   int    `json:"id"`
	Path string `json:"path"`
}

// AnalysisOperation 保存一项不含文件正文的待确认操作。
type AnalysisOperation struct {
	Action            string             `json:"action"`
	FilePath          string             `json:"filePath"`
	TargetPath        string             `json:"targetPath"`
	ShellCommand      string             `json:"shellCommand,omitempty"`
	WorkingDir        string             `json:"workingDir,omitempty"`
	EnableOCR         bool               `json:"enableOCR,omitempty"`
	RequestedMode     *uint32            `json:"requestedMode,omitempty"`
	RequestedOwnerUID *uint32            `json:"requestedOwnerUid,omitempty"`
	RequestedGroupGID *uint32            `json:"requestedGroupGid,omitempty"`
	Files             []AnalysisFile     `json:"files,omitempty"`
	Categories        []AnalysisCategory `json:"categories,omitempty"`
}

// ValidAnalysisOperations 校验分析卡中的操作数量与目标参数。
func ValidAnalysisOperations(operations []AnalysisOperation) bool {
	if len(operations) < 1 || len(operations) > 20 {
		return false
	}
	if len(operations) > 1 {
		for _, operation := range operations {
			if operation.Action == "edit_text" || operation.Action == "create_file" || operation.Action == "overwrite_file" || operation.Action == "create_symlink" || operation.Action == "create_archive" || operation.Action == "extract_archive" || operation.Action == "run_shell" {
				return false
			}
		}
	}
	for _, operation := range operations {
		if operation.Action != "run_shell" && (operation.ShellCommand != "" || operation.WorkingDir != "") {
			return false
		}
		if operation.Action != "classify_documents" && operation.EnableOCR {
			return false
		}
		if operation.Action != "classify_documents" && (len(operation.Files) != 0 || len(operation.Categories) != 0) {
			return false
		}
		switch operation.Action {
		case "classify_documents":
			if !validDuplicateScopePath(operation.FilePath) || !validAnalysisRelativePath(operation.TargetPath) ||
				operation.RequestedMode != nil || operation.RequestedOwnerUID != nil || operation.RequestedGroupGID != nil ||
				len(operation.Files) < 1 || len(operation.Files) > 20 || len(operation.Categories) < 1 || len(operation.Categories) > 50 {
				return false
			}
			if operation.FilePath != "" && operation.TargetPath == operation.FilePath {
				return false
			}
			var sourceBytes, textBytes int64
			seenFiles := make(map[string]struct{}, len(operation.Files))
			for _, file := range operation.Files {
				fileLimit := AnalysisFileSizeLimit(file.Path)
				if !validAnalysisRelativePath(file.Path) || !conversationWithinScope(operation.FilePath, file.Path) ||
					fileLimit == 0 || file.SizeBytes < 0 || file.SizeBytes > fileLimit ||
					!validAnalysisFileVersion(file.Version) {
					return false
				}
				if _, exists := seenFiles[file.Path]; exists {
					return false
				}
				seenFiles[file.Path] = struct{}{}
				sourceBytes += file.SizeBytes
				if validAnalysisTextExtension(file.Path) {
					textBytes += file.SizeBytes
				}
			}
			if sourceBytes > MaxAnalysisSourceTotalBytes || textBytes > MaxAnalysisTextTotalBytes {
				return false
			}
			seenCategories := make(map[string]struct{}, len(operation.Categories))
			for index, category := range operation.Categories {
				if category.ID != index || !validAnalysisRelativePath(category.Path) ||
					path.Dir(category.Path) != operation.TargetPath || path.Base(category.Path) == "." {
					return false
				}
				if _, exists := seenCategories[category.Path]; exists {
					return false
				}
				seenCategories[category.Path] = struct{}{}
			}
		case "rename":
			if operation.FilePath == "" || operation.TargetPath != "" || operation.RequestedMode != nil || operation.RequestedOwnerUID != nil || operation.RequestedGroupGID != nil {
				return false
			}
		case "edit_text":
			if operation.FilePath == "" || operation.TargetPath != "" || operation.RequestedMode != nil || operation.RequestedOwnerUID != nil || operation.RequestedGroupGID != nil {
				return false
			}
		case "create_file", "overwrite_file":
			if !validAnalysisRelativePath(operation.FilePath) || operation.TargetPath != "" || operation.RequestedMode != nil || operation.RequestedOwnerUID != nil || operation.RequestedGroupGID != nil || !validAnalysisTextExtension(operation.FilePath) {
				return false
			}
		case "trash":
			if !validAnalysisRelativePath(operation.FilePath) || operation.TargetPath != "" || operation.RequestedMode != nil || operation.RequestedOwnerUID != nil || operation.RequestedGroupGID != nil {
				return false
			}
		case "permanent_delete":
			if !validAnalysisRelativePath(operation.FilePath) || operation.TargetPath != "" || operation.RequestedMode != nil || operation.RequestedOwnerUID != nil || operation.RequestedGroupGID != nil {
				return false
			}
		case "create_directory":
			if operation.FilePath != "" || operation.TargetPath == "" || operation.RequestedMode != nil || operation.RequestedOwnerUID != nil || operation.RequestedGroupGID != nil {
				return false
			}
		case "move_file":
			if operation.FilePath == "" || operation.TargetPath == "" || operation.FilePath == operation.TargetPath || operation.RequestedMode != nil || operation.RequestedOwnerUID != nil || operation.RequestedGroupGID != nil {
				return false
			}
		case "create_symlink":
			if operation.FilePath == "" || operation.TargetPath == "" || operation.FilePath == operation.TargetPath || operation.RequestedMode != nil || operation.RequestedOwnerUID != nil || operation.RequestedGroupGID != nil {
				return false
			}
		case "copy_file", "copy_directory":
			if operation.FilePath == "" || operation.TargetPath == "" || operation.FilePath == operation.TargetPath || operation.RequestedMode != nil || operation.RequestedOwnerUID != nil || operation.RequestedGroupGID != nil {
				return false
			}
		case "create_archive":
			if !validAnalysisRelativePath(operation.FilePath) || !validAnalysisRelativePath(operation.TargetPath) || operation.FilePath == operation.TargetPath ||
				operation.RequestedMode != nil || operation.RequestedOwnerUID != nil || operation.RequestedGroupGID != nil {
				return false
			}
		case "extract_archive":
			if !validAnalysisRelativePath(operation.FilePath) || operation.TargetPath != "" && !validAnalysisRelativePath(operation.TargetPath) ||
				operation.FilePath == operation.TargetPath || operation.RequestedMode != nil || operation.RequestedOwnerUID != nil || operation.RequestedGroupGID != nil {
				return false
			}
		case "find_duplicates":
			if !validDuplicateScopePath(operation.FilePath) || operation.TargetPath != "" || operation.RequestedMode != nil || operation.RequestedOwnerUID != nil || operation.RequestedGroupGID != nil {
				return false
			}
		case "archive_photos":
			if !validDuplicateScopePath(operation.FilePath) || operation.TargetPath == "" || operation.RequestedMode != nil || operation.RequestedOwnerUID != nil || operation.RequestedGroupGID != nil {
				return false
			}
		case "set_permissions":
			if operation.FilePath == "" || operation.TargetPath != "" || operation.RequestedMode == nil || *operation.RequestedMode > 0o777 || operation.RequestedOwnerUID != nil || operation.RequestedGroupGID != nil {
				return false
			}
		case "set_owner":
			if operation.FilePath == "" || operation.TargetPath != "" || operation.RequestedMode != nil || operation.RequestedOwnerUID == nil && operation.RequestedGroupGID == nil {
				return false
			}
		case "run_shell":
			if !validShellScopePath(operation.FilePath) || operation.TargetPath != "" || operation.RequestedMode != nil || operation.RequestedOwnerUID != nil || operation.RequestedGroupGID != nil ||
				strings.TrimSpace(operation.ShellCommand) == "" || len(operation.ShellCommand) > maxShellCommandBytes || !utf8.ValidString(operation.ShellCommand) ||
				operation.WorkingDir != "." && !validShellRelativePath(operation.WorkingDir) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// AnalysisDataTypes 返回单项操作会使用的数据类型说明。
func AnalysisDataTypes(action string) []string {
	switch action {
	case "rename":
		return []string{"UTF-8 文件正文全文"}
	case "edit_text":
		return []string{"用户确认后读取的 UTF-8 文件正文全文会发送给模型"}
	case "create_file":
		return []string{"根据用户确认的任务生成 UTF-8 文本候选；目标须不存在"}
	case "overwrite_file":
		return []string{"用户确认后读取目标 UTF-8 文本并生成完整替换候选；原内容不会保留"}
	case "trash":
		return []string{"任务描述与待回收路径；不读取文件正文，移入可恢复回收站"}
	case "permanent_delete":
		return []string{"任务描述与待删除路径；不读取文件正文，永久删除且不可恢复"}
	case "create_directory":
		return []string{"任务描述与目标路径"}
	case "move_file":
		return []string{"任务描述、源路径与目标路径"}
	case "create_symlink":
		return []string{"任务描述、现有目标路径与软链接路径；不读取文件正文"}
	case "copy_file":
		return []string{"任务描述、源路径与目标路径；文件内容仅在本机复制，不发送给模型"}
	case "copy_directory":
		return []string{"任务描述、源路径与目标路径；目录内容仅在本机复制，不发送给模型"}
	case "create_archive":
		return []string{"任务描述、来源路径与压缩包路径；文件仅在本机读取并打包，不发送给模型"}
	case "extract_archive":
		return []string{"任务描述、压缩包路径与解压目标；内容仅在本机解压，不发送给模型"}
	case "set_permissions":
		return []string{"任务描述、文件路径与目标权限"}
	case "set_owner":
		return []string{"任务描述、文件路径与目标属主"}
	case "run_shell":
		return []string{"经确认的命令与工作目录；命令在隔离暂存区执行，仅提交逐项审批后的文件变化"}
	case "find_duplicates":
		return []string{"任务描述与扫描范围会发送给模型；文件内容仅在服务端本机读取并计算 SHA-256，不发送给模型"}
	case "archive_photos":
		return []string{"索引中的照片路径与拍摄日期；不读取或发送图片正文"}
	case "classify_documents":
		return []string{"确认后提取候选正文并发送给模型；中英文 OCR 仅在确认卡单独启用时处理整份无文字层文档"}
	default:
		return []string{"任务描述、操作路径与目标值"}
	}
}

type AgentConversation struct {
	ID              string
	State           ConversationState
	Messages        []ConversationMessage
	Analysis        *AnalysisCard
	TaskID          string
	PlanID          string
	DuplicateScanID string
	Error           string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type ConversationSummary struct {
	ID              string
	State           ConversationState
	Title           string
	TaskID          string
	PlanID          string
	DuplicateScanID string
	UpdatedAt       time.Time
}

// CreateConversation 保存首条用户消息，尚不触碰文件系统。
func (s *Store) CreateConversation(ctx context.Context, id, message string, at time.Time) error {
	if id == "" || len(message) == 0 || len(message) > 4096 {
		return errors.New("invalid conversation identifier or message")
	}
	at = conversationTime(at)
	atNS := at.UnixNano()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin agent conversation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_conversations (id, state, created_at_ns, updated_at_ns) VALUES (?, 'processing', ?, ?)`, id, atNS, atNS); err != nil {
		return fmt.Errorf("create agent conversation: %w", err)
	}
	if err := s.insertConversationMessage(ctx, tx, id, 1, "user", message, at); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit agent conversation: %w", err)
	}
	return nil
}

// StartConversationTurn 仅允许未生成方案的终态继续澄清，并解除旧扫描关联。
func (s *Store) StartConversationTurn(ctx context.Context, id, message string, at time.Time) error {
	if len(message) == 0 || len(message) > 4096 {
		return errors.New("invalid conversation message")
	}
	at = conversationTime(at)
	atNS := at.UnixNano()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin agent conversation turn: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	result, err := tx.ExecContext(ctx, `
		UPDATE agent_conversations SET state = 'processing', analysis_action = 'rename', goal = '', file_path = '',
			target_path = '', mode = NULL, owner_uid = NULL, group_gid = NULL,
			operations_json = '[]', provider = '', model = '', destination = '', duplicate_scan_id = NULL, error_code = '', updated_at_ns = ?
		WHERE id = ? AND plan_id IS NULL AND state IN ('clarifying', 'awaiting_confirmation', 'analysis_complete', 'unsupported', 'failed')`, atNS, id)
	if err != nil {
		return fmt.Errorf("start agent conversation turn: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check agent conversation turn: %w", err)
	}
	if affected != 1 {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM agent_conversations WHERE id = ?`, id).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
			return ErrConversationNotFound
		} else if err != nil {
			return fmt.Errorf("check agent conversation: %w", err)
		}
		return ErrConversationConflict
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_conversation_messages WHERE conversation_id = ?`, id).Scan(&count); err != nil {
		return fmt.Errorf("count agent conversation messages: %w", err)
	}
	if count >= maxConversationMessages {
		return ErrConversationLimit
	}
	if err := s.insertConversationMessage(ctx, tx, id, count+1, "user", message, at); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit agent conversation turn: %w", err)
	}
	return nil
}

// FinishConversationTurn 原子保存模型回复和其确认状态。
func (s *Store) FinishConversationTurn(ctx context.Context, id string, state ConversationState, reply string, card AnalysisCard, errorCode string, at time.Time) error {
	if !validConversationState(state) || state == ConversationProcessing || state == ConversationAnalyzing || state == ConversationAwaitingApproval {
		return errors.New("invalid finished conversation state")
	}
	if len(reply) == 0 || len(reply) > 4096 {
		return errors.New("invalid conversation reply")
	}
	at = conversationTime(at)
	atNS := at.UnixNano()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin conversation response: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_conversation_messages WHERE conversation_id = ?`, id).Scan(&count); err != nil {
		return fmt.Errorf("count conversation response messages: %w", err)
	}
	if count >= maxConversationMessages {
		return ErrConversationLimit
	}
	var action, goal, filePath, targetPath, mode, ownerUID, groupGID, provider, model, destination, operationsJSON any = "rename", "", "", "", nil, nil, nil, "", "", "", "[]"
	if state == ConversationAwaitingConfirmation {
		operations := card.Operations
		if len(operations) == 0 {
			cardAction := card.Action
			if cardAction == "" {
				cardAction = "rename"
			}
			operations = []AnalysisOperation{{
				Action: cardAction, FilePath: card.FilePath, TargetPath: card.TargetPath,
				RequestedMode: card.RequestedMode, RequestedOwnerUID: card.RequestedOwnerUID, RequestedGroupGID: card.RequestedGroupGID,
			}}
		}
		if card.Goal == "" || !ValidAnalysisOperations(operations) || card.Provider == "" || card.Model == "" || card.Destination == "" {
			return errors.New("analysis confirmation card is incomplete")
		}
		encoded, err := json.Marshal(operations)
		if err != nil {
			return fmt.Errorf("encode analysis operations: %w", err)
		}
		operationsJSON = string(encoded)
		if len(operations) == 1 {
			first := operations[0]
			action, filePath, targetPath = first.Action, first.FilePath, first.TargetPath
			mode, ownerUID, groupGID = nullableUint32(first.RequestedMode), nullableUint32(first.RequestedOwnerUID), nullableUint32(first.RequestedGroupGID)
		}
		goal, provider, model, destination = card.Goal, card.Provider, card.Model, card.Destination
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE agent_conversations SET state = ?, analysis_action = ?, goal = ?, file_path = ?, target_path = ?,
			mode = ?, owner_uid = ?, group_gid = ?,
			operations_json = ?, provider = ?, model = ?, destination = ?, error_code = ?, updated_at_ns = ?
		WHERE id = ? AND state = 'processing'`,
		state, action, goal, filePath, targetPath, mode, ownerUID, groupGID, operationsJSON, provider, model, destination, errorCode, atNS, id)
	if err != nil {
		return fmt.Errorf("finish agent conversation turn: %w", err)
	}
	if err := requireOneRow(result, "agent conversation"); err != nil {
		return ErrConversationConflict
	}
	if err := s.insertConversationMessage(ctx, tx, id, count+1, "assistant", reply, at); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit agent conversation response: %w", err)
	}
	return nil
}

// BeginConversationAnalysis 用条件更新保证每个待确认卡只领取一次。
func (s *Store) BeginConversationAnalysis(ctx context.Context, id string, at time.Time, enableOCR ...bool) (bool, error) {
	if len(enableOCR) > 1 {
		return false, errors.New("invalid conversation analysis options")
	}
	useOCR := len(enableOCR) == 1 && enableOCR[0]
	atNS := timestampOrNow(at)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin conversation analysis claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var operationsJSON string
	if err := tx.QueryRowContext(ctx, `SELECT operations_json FROM agent_conversations WHERE id = ? AND state = 'awaiting_confirmation' AND plan_id IS NULL`, id).Scan(&operationsJSON); errors.Is(err, sql.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("read conversation analysis operations: %w", err)
	}
	if useOCR {
		var operations []AnalysisOperation
		if err := json.Unmarshal([]byte(operationsJSON), &operations); err != nil || len(operations) != 1 || operations[0].Action != "classify_documents" {
			return false, errors.New("OCR is only supported for document classification")
		}
		operations[0].EnableOCR = true
		if !ValidAnalysisOperations(operations) {
			return false, errors.New("invalid OCR analysis operation")
		}
		encoded, err := json.Marshal(operations)
		if err != nil {
			return false, fmt.Errorf("encode OCR analysis operation: %w", err)
		}
		operationsJSON = string(encoded)
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE agent_conversations SET state = 'analyzing', operations_json = ?, error_code = '', updated_at_ns = ?
		WHERE id = ? AND state = 'awaiting_confirmation' AND plan_id IS NULL`, operationsJSON, atNS, id)
	if err != nil {
		return false, fmt.Errorf("claim conversation analysis: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("check conversation analysis claim: %w", err)
	}
	if count != 1 {
		return false, nil
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit conversation analysis claim: %w", err)
	}
	return true, nil
}

// FailConversationAnalysis 保留用户确认卡，供人工查看失败原因。
func (s *Store) FailConversationAnalysis(ctx context.Context, id, errorCode, reply string, at time.Time) error {
	at = conversationTime(at)
	atNS := at.UnixNano()
	if reply == "" || len(reply) > 4096 {
		return errors.New("invalid analysis failure reply")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin failed conversation analysis: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_conversation_messages WHERE conversation_id = ?`, id).Scan(&count); err != nil {
		return fmt.Errorf("count failed analysis messages: %w", err)
	}
	if count >= maxConversationMessages {
		return ErrConversationLimit
	}
	result, err := tx.ExecContext(ctx, `UPDATE agent_conversations SET state = 'failed', error_code = ?, updated_at_ns = ? WHERE id = ? AND state = 'analyzing' AND plan_id IS NULL`, errorCode, atNS, id)
	if err != nil {
		return fmt.Errorf("fail conversation analysis: %w", err)
	}
	if err := requireOneRow(result, "agent conversation"); err != nil {
		return ErrConversationConflict
	}
	if err := s.insertConversationMessage(ctx, tx, id, count+1, "assistant", reply, at); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit failed conversation analysis: %w", err)
	}
	return nil
}

// CompleteConversationAnalysis 保存无需文件操作的分析结果。
func (s *Store) CompleteConversationAnalysis(ctx context.Context, id, reply string, at time.Time) error {
	if reply == "" || len(reply) > 4096 {
		return errors.New("invalid analysis completion reply")
	}
	at = conversationTime(at)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin completed conversation analysis: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_conversation_messages WHERE conversation_id = ?`, id).Scan(&count); err != nil {
		return err
	}
	if count >= maxConversationMessages {
		return ErrConversationLimit
	}
	result, err := tx.ExecContext(ctx, `UPDATE agent_conversations SET state = 'analysis_complete', error_code = '', updated_at_ns = ? WHERE id = ? AND state = 'analyzing' AND plan_id IS NULL`, at.UnixNano(), id)
	if err != nil {
		return fmt.Errorf("complete conversation analysis: %w", err)
	}
	if err := requireOneRow(result, "agent conversation"); err != nil {
		return ErrConversationConflict
	}
	if err := s.insertConversationMessage(ctx, tx, id, count+1, "assistant", reply, at); err != nil {
		return err
	}
	return tx.Commit()
}

// RecoverConversations 标记重启前未完成的模型调用，绝不自动重放。
func (s *Store) RecoverConversations(ctx context.Context, at time.Time) error {
	atNS := timestampOrNow(at)
	_, err := s.db.ExecContext(ctx, `UPDATE agent_conversations SET state = 'failed', error_code = 'service_restarted', updated_at_ns = ?
		WHERE state IN ('processing', 'analyzing') AND plan_id IS NULL AND NOT EXISTS (
			SELECT 1 FROM duplicate_scan_runs
			WHERE duplicate_scan_runs.id = agent_conversations.duplicate_scan_id
				AND duplicate_scan_runs.status IN ('queued', 'running')
		)`, atNS)
	if err != nil {
		return fmt.Errorf("recover interrupted conversations: %w", err)
	}
	return nil
}

// Conversation 返回持久化消息和当前分析卡，不读取文件。
func (s *Store) Conversation(ctx context.Context, id string) (AgentConversation, error) {
	return loadConversation(ctx, s.db, id)
}

// ListConversations 返回不含消息正文的有界会话摘要。
func (s *Store) ListConversations(ctx context.Context, limit, offset int) ([]ConversationSummary, bool, error) {
	if limit < 1 || limit > 100 || offset < 0 || offset > 100000 {
		return nil, false, errors.New("invalid conversation list range")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id, c.state, COALESCE(NULLIF(substr(c.goal, 1, 48), ''), CASE c.state
			WHEN 'awaiting_confirmation' THEN '待确认分析'
			WHEN 'awaiting_approval' THEN '待审批方案'
			WHEN 'unsupported' THEN '暂不支持的任务'
			WHEN 'failed' THEN '需要处理的任务'
			ELSE 'Agent 对话' END),
		COALESCE(c.task_id, ''), COALESCE(c.plan_id, ''), COALESCE(c.duplicate_scan_id, ''), c.updated_at_ns
		FROM agent_conversations c ORDER BY c.updated_at_ns DESC, c.id DESC LIMIT ? OFFSET ?`, limit+1, offset)
	if err != nil {
		return nil, false, fmt.Errorf("list agent conversations: %w", err)
	}
	defer rows.Close()
	summaries := make([]ConversationSummary, 0, limit+1)
	for rows.Next() {
		var item ConversationSummary
		var updated int64
		if err := rows.Scan(&item.ID, &item.State, &item.Title, &item.TaskID, &item.PlanID, &item.DuplicateScanID, &updated); err != nil {
			return nil, false, fmt.Errorf("scan conversation summary: %w", err)
		}
		item.Title = strings.NewReplacer("\r", " ", "\n", " ", "\t", " ").Replace(item.Title)
		item.Title = strings.TrimSpace(item.Title)
		item.UpdatedAt = time.Unix(0, updated).UTC()
		summaries = append(summaries, item)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("iterate conversation summaries: %w", err)
	}
	hasMore := len(summaries) > limit
	if hasMore {
		summaries = summaries[:limit]
	}
	return summaries, hasMore, nil
}

// CreateConversationTaskPlan 原子保存模型方案，并将会话固定到该方案。
func (s *Store) CreateConversationTaskPlan(ctx context.Context, conversationID string, task TaskRecord, plan PlanRecord, audit AuditRecord, reply string) error {
	if conversationID == "" || task.ID == "" || plan.TaskID != task.ID || task.ScopePath != plan.ScopePath || audit.TaskID != task.ID || audit.PlanID != plan.ID {
		return errors.New("conversation task, plan, and audit identifiers do not match")
	}
	if task.Kind != "agent" || task.Status != TaskAwaitingApproval || task.ModelProvider == "" || task.ModelName == "" {
		return errors.New("conversation Agent task must identify its model and await approval")
	}
	if plan.Status != PlanAwaitingApproval || !validInitialAgentOperations(plan.Operations) || plan.ModelProvider != task.ModelProvider || plan.ModelName != task.ModelName {
		return errors.New("conversation Agent plan must contain supported model-proposed operations")
	}
	if audit.OperationID != plan.Operations[0].ID || audit.EventType != "plan_created" || len(reply) == 0 || len(reply) > 4096 {
		return errors.New("conversation plan audit or response is invalid")
	}
	audit.Actor = "agent"
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin conversation plan transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	expectedOperations, valid := conversationConfirmationOperations(ctx, tx, conversationID)
	if !valid {
		return ErrConversationConflict
	}
	if len(expectedOperations) == 1 && expectedOperations[0].Action == "archive_photos" {
		if !conversationArchivePlanMatches(expectedOperations[0], plan.Operations) {
			return ErrConversationConflict
		}
	} else if len(expectedOperations) == 1 && expectedOperations[0].Action == "classify_documents" {
		if !conversationClassificationPlanMatches(expectedOperations[0], plan.Operations) {
			return ErrConversationConflict
		}
	} else {
		if len(expectedOperations) != len(plan.Operations) {
			return ErrConversationConflict
		}
		for _, operation := range plan.Operations {
			if operation.Ordinal < 0 || operation.Ordinal >= len(expectedOperations) || !conversationOperationMatches(expectedOperations[operation.Ordinal], operation) {
				return ErrConversationConflict
			}
		}
	}
	if err := insertTaskRecord(ctx, tx, task); err != nil {
		return err
	}
	if err := createPlanRecord(ctx, tx, plan); err != nil {
		return err
	}
	if _, err := appendAuditRecord(ctx, tx, audit); err != nil {
		return err
	}
	for _, operation := range plan.Operations[1:] {
		if _, err := appendAuditRecord(ctx, tx, operationPlanCreatedAudit(audit, operation)); err != nil {
			return err
		}
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_conversation_messages WHERE conversation_id = ?`, conversationID).Scan(&count); err != nil {
		return fmt.Errorf("count conversation plan messages: %w", err)
	}
	if count >= maxConversationMessages {
		return ErrConversationLimit
	}
	result, err := tx.ExecContext(ctx, `UPDATE agent_conversations SET state = 'awaiting_approval', task_id = ?, plan_id = ?, error_code = '', updated_at_ns = ? WHERE id = ? AND state = 'analyzing' AND task_id IS NULL AND plan_id IS NULL`, task.ID, plan.ID, timestampOrNow(task.UpdatedAt), conversationID)
	if err != nil {
		return fmt.Errorf("link conversation to plan: %w", err)
	}
	if err := requireOneRow(result, "agent conversation"); err != nil {
		return ErrConversationConflict
	}
	if err := s.insertConversationMessage(ctx, tx, conversationID, count+1, "assistant", reply, task.UpdatedAt); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit conversation plan transaction: %w", err)
	}
	return nil
}

func conversationClassificationPlanMatches(expected AnalysisOperation, operations []OperationRecord) bool {
	if expected.Action != "classify_documents" || !ValidAnalysisOperations([]AnalysisOperation{expected}) || len(operations) == 0 || len(operations) > len(expected.Files) {
		return false
	}
	files, categories := make(map[string]struct{}, len(expected.Files)), make(map[string]struct{}, len(expected.Categories))
	for _, file := range expected.Files {
		files[file.Path] = struct{}{}
	}
	for _, category := range expected.Categories {
		categories[category.Path] = struct{}{}
	}
	sources, targets := make(map[string]struct{}, len(operations)), make(map[string]struct{}, len(operations))
	for index, operation := range operations {
		if operation.Ordinal != index || operation.Action != "move_file" || operation.SourcePath == "" || operation.TargetPath == "" ||
			!conversationWithinScope(expected.FilePath, operation.SourcePath) {
			return false
		}
		if _, exists := files[operation.SourcePath]; !exists {
			return false
		}
		if _, exists := categories[path.Dir(operation.TargetPath)]; !exists || !classificationTargetName(path.Base(operation.SourcePath), path.Base(operation.TargetPath)) {
			return false
		}
		if _, exists := sources[operation.SourcePath]; exists {
			return false
		}
		if _, exists := targets[operation.TargetPath]; exists {
			return false
		}
		sources[operation.SourcePath], targets[operation.TargetPath] = struct{}{}, struct{}{}
	}
	return true
}

func classificationTargetName(original, target string) bool {
	if original == target {
		return true
	}
	extension := path.Ext(original)
	stem := strings.TrimSuffix(original, extension)
	if !strings.HasPrefix(target, stem+" (") || !strings.HasSuffix(target, ")"+extension) {
		return false
	}
	index := strings.TrimSuffix(strings.TrimPrefix(target, stem+" ("), ")"+extension)
	n, err := strconv.Atoi(index)
	return err == nil && n > 0 && strconv.Itoa(n) == index
}

func validAnalysisRelativePath(value string) bool {
	if value == "" || len(value) > 4096 || !utf8.ValidString(value) || strings.ContainsAny(value, "\\\x00") || path.IsAbs(value) || path.Clean(value) != value {
		return false
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || component == "." || component == ".." || component == ".filesage-trash" || len(component) > 255 {
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

func validShellScopePath(value string) bool {
	return value == "" || validShellRelativePath(value)
}

func validShellRelativePath(value string) bool {
	return validAnalysisRelativePath(value) && validateIndexPath(value, false) == nil
}

func validAnalysisTextExtension(filePath string) bool {
	switch strings.ToLower(path.Ext(filePath)) {
	case ".txt", ".md", ".markdown", ".mdown":
		return true
	default:
		return false
	}
}

// AnalysisFileSizeLimit 返回 Agent 分类允许的单文件源大小上限。
func AnalysisFileSizeLimit(filePath string) int64 {
	if validAnalysisTextExtension(filePath) {
		return MaxAnalysisTextFileBytes
	}
	switch strings.ToLower(path.Ext(filePath)) {
	case ".pdf", ".docx", ".pptx":
		return MaxAnalysisDocumentFileBytes
	default:
		return 0
	}
}

func validAnalysisFileVersion(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func conversationArchivePlanMatches(expected AnalysisOperation, operations []OperationRecord) bool {
	if expected.Action != "archive_photos" || !conversationArchiveScopeTargetValid(expected.FilePath, expected.TargetPath) || len(operations) == 0 || len(operations) > MaxAgentPlanOperations {
		return false
	}
	sources, targets := make(map[string]struct{}, len(operations)), make(map[string]struct{}, len(operations))
	for index, operation := range operations {
		if operation.Ordinal != index || operation.Action != "archive_photo" || operation.SourcePath == "" || operation.TargetPath == "" ||
			!conversationWithinScope(expected.FilePath, operation.SourcePath) || !conversationArchiveTarget(expected.TargetPath, operation.SourcePath, operation.TargetPath) {
			return false
		}
		if _, exists := sources[operation.SourcePath]; exists {
			return false
		}
		if _, exists := targets[operation.TargetPath]; exists {
			return false
		}
		sources[operation.SourcePath], targets[operation.TargetPath] = struct{}{}, struct{}{}
	}
	return true
}

func conversationArchiveScopeTargetValid(scope, target string) bool {
	if target == "" {
		return false
	}
	return scope == "" || target != scope && !strings.HasPrefix(scope, target+"/")
}

func conversationWithinScope(scope, value string) bool {
	return scope == "" || value == scope || strings.HasPrefix(value, scope+"/")
}

func conversationArchiveTarget(root, source, target string) bool {
	prefix := root + "/"
	if !strings.HasPrefix(target, prefix) {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(target, prefix), "/")
	if len(parts) != 3 || len(parts[0]) != 4 {
		return false
	}
	for _, digit := range parts[0] {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	month, err := strconv.Atoi(parts[1])
	if err != nil || month < 1 || month > 12 {
		return false
	}
	name, original := parts[2], path.Base(source)
	if name == original {
		return true
	}
	extension := path.Ext(original)
	stem := strings.TrimSuffix(original, extension)
	if !strings.HasPrefix(name, stem+" (") || !strings.HasSuffix(name, ")"+extension) {
		return false
	}
	index := strings.TrimSuffix(strings.TrimPrefix(name, stem+" ("), ")"+extension)
	n, err := strconv.Atoi(index)
	return err == nil && n > 0
}

func conversationConfirmationOperations(ctx context.Context, tx *sql.Tx, conversationID string) ([]AnalysisOperation, bool) {
	var operationsJSON, action, filePath, targetPath string
	var mode, ownerUID, groupGID sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT operations_json, analysis_action, file_path, target_path, mode, owner_uid, group_gid
		FROM agent_conversations WHERE id = ? AND state = 'analyzing' AND plan_id IS NULL`, conversationID).
		Scan(&operationsJSON, &action, &filePath, &targetPath, &mode, &ownerUID, &groupGID)
	if err != nil {
		return nil, false
	}
	var expected []AnalysisOperation
	if operationsJSON != "" && operationsJSON != "[]" {
		if json.Unmarshal([]byte(operationsJSON), &expected) != nil || !ValidAnalysisOperations(expected) {
			return nil, false
		}
	} else {
		requestedMode, modeValid := optionalUint32(mode, 0o777)
		requestedUID, uidValid := optionalUint32(ownerUID, uint64(^uint32(0)))
		requestedGID, gidValid := optionalUint32(groupGID, uint64(^uint32(0)))
		if !modeValid || !uidValid || !gidValid {
			return nil, false
		}
		expected = []AnalysisOperation{{Action: action, FilePath: filePath, TargetPath: targetPath,
			RequestedMode: requestedMode, RequestedOwnerUID: requestedUID, RequestedGroupGID: requestedGID}}
	}
	return expected, ValidAnalysisOperations(expected)
}

func conversationOperationMatches(expected AnalysisOperation, operation OperationRecord) bool {
	if expected.Action != operation.Action {
		return false
	}
	if expected.Action == "rename" {
		return expected.FilePath == operation.SourcePath && expected.TargetPath == ""
	}
	if expected.Action == "create_directory" {
		return expected.TargetPath == operation.TargetPath && expected.FilePath == ""
	}
	if expected.Action == "trash" || expected.Action == "permanent_delete" {
		return expected.FilePath == operation.SourcePath && operation.TargetPath == ""
	}
	if expected.Action == "move_file" || expected.Action == "create_symlink" || expected.Action == "copy_file" || expected.Action == "copy_directory" {
		return expected.FilePath == operation.SourcePath && expected.TargetPath == operation.TargetPath
	}
	if expected.Action == "create_archive" || expected.Action == "extract_archive" {
		return expected.FilePath == operation.SourcePath && expected.TargetPath == operation.TargetPath
	}
	if expected.Action == "edit_text" {
		return expected.FilePath == operation.SourcePath && operation.SourcePath == operation.TargetPath
	}
	if expected.Action == "create_file" {
		parent := path.Dir(expected.FilePath)
		if parent == "." {
			parent = ""
		}
		return expected.FilePath == operation.TargetPath && parent == operation.SourcePath
	}
	if expected.Action == "overwrite_file" {
		return expected.FilePath == operation.SourcePath && operation.SourcePath == operation.TargetPath
	}
	if expected.FilePath != operation.SourcePath || operation.SourcePath != operation.TargetPath {
		return false
	}
	if expected.Action == "set_permissions" {
		return expected.RequestedMode != nil && operation.Mode != nil && *expected.RequestedMode == *operation.Mode && expected.RequestedOwnerUID == nil && expected.RequestedGroupGID == nil
	}
	if expected.Action == "set_owner" {
		return equalUint32Pointers(expected.RequestedOwnerUID, operation.OwnerUID) && equalUint32Pointers(expected.RequestedGroupGID, operation.GroupGID) && (operation.OwnerUID != nil || operation.GroupGID != nil)
	}
	return false
}

func equalUint32Pointers(left, right *uint32) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

func equalNullableUint32(value sql.NullInt64, expected *uint32) bool {
	if !value.Valid {
		return expected == nil
	}
	return expected != nil && value.Int64 >= 0 && uint64(value.Int64) <= uint64(^uint32(0)) && uint32(value.Int64) == *expected
}

func (s *Store) insertConversationMessage(ctx context.Context, execer storeExecer, id string, sequence int, role, content string, at time.Time) error {
	if (role != "user" && role != "assistant") || content == "" || len(content) > 4096 {
		return errors.New("invalid conversation message")
	}
	_, err := execer.ExecContext(ctx, `INSERT INTO agent_conversation_messages (conversation_id, sequence, role, content, created_at_ns) VALUES (?, ?, ?, ?, ?)`, id, sequence, role, content, timestampOrNow(at))
	if err != nil {
		return fmt.Errorf("save agent conversation message: %w", err)
	}
	return nil
}

func loadConversation(ctx context.Context, queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, id string) (AgentConversation, error) {
	var conversation AgentConversation
	var createdAt, updatedAt int64
	var action, goal, filePath, targetPath, operationsJSON, provider, model, destination string
	var mode, ownerUID, groupGID sql.NullInt64
	var taskID, planID, duplicateScanID sql.NullString
	err := queryer.QueryRowContext(ctx, `SELECT id, state, analysis_action, goal, file_path, target_path, mode, owner_uid, group_gid, operations_json, provider, model, destination, task_id, plan_id, duplicate_scan_id, error_code, created_at_ns, updated_at_ns FROM agent_conversations WHERE id = ?`, id).Scan(
		&conversation.ID, &conversation.State, &action, &goal, &filePath, &targetPath, &mode, &ownerUID, &groupGID, &operationsJSON, &provider, &model, &destination,
		&taskID, &planID, &duplicateScanID, &conversation.Error, &createdAt, &updatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentConversation{}, ErrConversationNotFound
	}
	if err != nil {
		return AgentConversation{}, fmt.Errorf("read agent conversation: %w", err)
	}
	conversation.CreatedAt = time.Unix(0, createdAt).UTC()
	conversation.UpdatedAt = time.Unix(0, updatedAt).UTC()
	if taskID.Valid {
		conversation.TaskID = taskID.String
	}
	if planID.Valid {
		conversation.PlanID = planID.String
	}
	if duplicateScanID.Valid {
		conversation.DuplicateScanID = duplicateScanID.String
	}
	canRestoreAnalysis := conversation.State == ConversationAwaitingConfirmation || conversation.DuplicateScanID == "" &&
		(conversation.State == ConversationAnalyzing || conversation.State == ConversationFailed || conversation.State == ConversationAwaitingApproval)
	if canRestoreAnalysis && goal != "" {
		operations := make([]AnalysisOperation, 0, 1)
		if operationsJSON != "" && operationsJSON != "[]" {
			if err := json.Unmarshal([]byte(operationsJSON), &operations); err != nil || !ValidAnalysisOperations(operations) {
				return AgentConversation{}, fmt.Errorf("decode agent analysis operations: invalid stored value")
			}
		} else {
			if action == "" {
				action = "rename"
			}
			requestedMode, modeValid := optionalUint32(mode, 0o777)
			requestedUID, uidValid := optionalUint32(ownerUID, uint64(^uint32(0)))
			requestedGID, gidValid := optionalUint32(groupGID, uint64(^uint32(0)))
			if modeValid && uidValid && gidValid {
				operations = append(operations, AnalysisOperation{
					Action: action, FilePath: filePath, TargetPath: targetPath,
					RequestedMode: requestedMode, RequestedOwnerUID: requestedUID, RequestedGroupGID: requestedGID,
				})
			}
		}
		if ValidAnalysisOperations(operations) {
			card := &AnalysisCard{
				Operations: operations, Goal: goal, DataTypes: []string{"任务描述、操作路径与目标值"},
				Provider: provider, Model: model, Destination: destination,
			}
			if len(operations) == 1 {
				operation := operations[0]
				card.Action, card.FilePath, card.TargetPath = operation.Action, operation.FilePath, operation.TargetPath
				card.RequestedMode, card.RequestedOwnerUID, card.RequestedGroupGID = operation.RequestedMode, operation.RequestedOwnerUID, operation.RequestedGroupGID
				card.DataTypes = AnalysisDataTypes(operation.Action)
			}
			conversation.Analysis = card
		}
	}
	rows, err := queryer.QueryContext(ctx, `SELECT role, content, created_at_ns FROM agent_conversation_messages WHERE conversation_id = ? ORDER BY sequence`, id)
	if err != nil {
		return AgentConversation{}, fmt.Errorf("read agent conversation messages: %w", err)
	}
	defer rows.Close()
	conversation.Messages = make([]ConversationMessage, 0)
	for rows.Next() {
		var message ConversationMessage
		var created int64
		if err := rows.Scan(&message.Role, &message.Content, &created); err != nil {
			return AgentConversation{}, fmt.Errorf("scan agent conversation message: %w", err)
		}
		message.CreatedAt = time.Unix(0, created).UTC()
		conversation.Messages = append(conversation.Messages, message)
	}
	if err := rows.Err(); err != nil {
		return AgentConversation{}, fmt.Errorf("iterate agent conversation messages: %w", err)
	}
	return conversation, nil
}

func validConversationState(state ConversationState) bool {
	switch state {
	case ConversationProcessing, ConversationClarifying, ConversationAwaitingConfirmation, ConversationAnalyzing, ConversationAwaitingApproval, ConversationAnalysisComplete, ConversationUnsupported, ConversationFailed:
		return true
	default:
		return false
	}
}

func conversationTime(at time.Time) time.Time {
	if at.IsZero() {
		return time.Now().UTC()
	}
	return at.UTC()
}
