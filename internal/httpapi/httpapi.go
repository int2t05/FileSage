// Package httpapi 提供受管理根目录约束的文件 HTTP API。
package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"filesage/internal/agent"
	"filesage/internal/coordination"
	"filesage/internal/fsroot"
	"filesage/internal/store"
	trashsvc "filesage/internal/trash"
)

type fileSystem interface {
	List(relativePath string) ([]fsroot.Entry, error)
	OpenRead(relativePath string) (io.ReadCloser, fsroot.Entry, error)
	Mkdir(relativePath string, perm fs.FileMode) error
	RenameNoReplace(oldPath, newPath string) error
	Remove(relativePath string) error
}

type indexedFileSystem interface {
	LstatEntry(relativePath string) (fsroot.Entry, error)
}

type metadataFileSystem interface {
	UpdateMetadata(string, fsroot.Fingerprint, fsroot.MetadataUpdate) (fsroot.Entry, error)
}

type fileIndexer interface {
	Status(context.Context) (store.FileIndexStatus, error)
	Query(context.Context, store.FileIndexQuery) ([]store.FileIndexRecord, error)
	Refresh(context.Context)
}

type archiveFileSystem interface {
	WriteArchive(context.Context, []string, io.Writer, fsroot.ArchiveLimits) error
}

type archiveFormatFileSystem interface {
	WriteArchiveFormat(context.Context, []string, io.Writer, fsroot.ArchiveLimits, fsroot.ArchiveFormat) error
}

type taskReader interface {
	ListTasks(context.Context, store.TaskListQuery) (store.TaskListPage, error)
	Task(context.Context, string) (store.TaskRecord, error)
	AuditForTaskPage(context.Context, string, int, int) (store.TaskAuditPage, error)
}

type auditRepository interface {
	BeginManualAudit(context.Context, store.AuditRecord) (int64, error)
	FinishManualAudit(context.Context, int64, string, string) error
	AuditHistoryPage(context.Context, int64, int) (store.AuditHistoryPage, error)
}

type agentWorkflow interface {
	Plan(context.Context, string) (store.PlanRecord, error)
	LatestPlan(context.Context, string) (store.PlanRecord, error)
	RevisePlan(context.Context, string, int, []agent.Proposal) (store.PlanRecord, error)
	Decide(context.Context, string, string, string, ...int) (store.PlanRecord, error)
	Execute(context.Context, string, ...int) (store.PlanRecord, error)
	StopExecution(context.Context, string, int) (store.PlanRecord, error)
}

type agentExecutionWorkflow interface {
	StartPlan(context.Context, string, int, string, bool) (store.PlanRecord, error)
	ExecuteStep(context.Context, string, string, int) (store.PlanRecord, error)
}

type agentPlanRejectWorkflow interface {
	RejectPlan(context.Context, string, int) (store.PlanRecord, error)
}

type agentSettingsRepository interface {
	AgentSettings(context.Context) (store.AgentSettings, error)
	SetAgentBypassPermission(context.Context, bool, time.Time) error
}

type agentTextWorkflow interface {
	PreviewTextEdit(context.Context, string, string, int) (agent.TextEditPreview, error)
	ReviseTextCandidate(context.Context, string, string, int, []byte) (store.PlanRecord, error)
}

type agentShellPlanReader interface {
	ShellRun(context.Context, string) (store.ShellRunRecord, error)
}

type agentConversationWorkflow interface {
	Status() agent.ModelStatus
	List(context.Context, int, int) ([]store.ConversationSummary, bool, error)
	Create(context.Context, string) (store.AgentConversation, error)
	Get(context.Context, string) (store.AgentConversation, error)
	AddMessage(context.Context, string, string) (store.AgentConversation, error)
	ConfirmAnalysis(context.Context, string, ...bool) (store.AgentConversation, error)
}

type trashService interface {
	List(context.Context, int, string) (trashsvc.Page, error)
	Get(context.Context, string) (trashsvc.Item, error)
	LockIdentity(context.Context, string) (fsroot.Entry, error)
	MoveToTrash(context.Context, fsroot.Entry) (trashsvc.Item, error)
	PermanentRemove(context.Context, fsroot.Entry) error
	Restore(context.Context, string) (string, error)
	Purge(context.Context, string) error
	PurgeAtSnapshot(context.Context, string, int64) error
	PurgeCandidates(context.Context, int64, int64, int) ([]trashsvc.Item, error)
}

// API 将文件系统能力、安全检查和请求处理组合为 HTTP 接口。
type API struct {
	files            fileSystem
	tasks            taskReader
	audits           auditRepository
	trash            trashService
	agent            agentWorkflow
	conversations    agentConversationWorkflow
	agentSettings    agentSettingsRepository
	index            fileIndexer
	locks            *coordination.Locker
	readiness        func(context.Context) error
	archiveSlots     chan struct{}
	archiveReadSlots chan struct{}
}

type listingResponse struct {
	Path       string      `json:"path"`
	Entries    []fileEntry `json:"entries"`
	Limit      int         `json:"limit"`
	HasMore    bool        `json:"hasMore"`
	NextCursor string      `json:"nextCursor"`
	IndexState string      `json:"indexState"`
}

type indexStatusResponse struct {
	ActiveGeneration   int64  `json:"activeGeneration"`
	BuildingGeneration int64  `json:"buildingGeneration"`
	State              string `json:"state"`
	Scanned            int64  `json:"scanned"`
	StartedAt          string `json:"startedAt,omitempty"`
	CompletedAt        string `json:"completedAt,omitempty"`
	LastError          string `json:"lastError,omitempty"`
}

type fileEntry struct {
	Name               string            `json:"name"`
	Path               string            `json:"path"`
	Kind               fsroot.Kind       `json:"kind"`
	Size               int64             `json:"size"`
	ModifiedAt         string            `json:"modifiedAt"`
	CapturedAt         *string           `json:"capturedAt,omitempty"`
	Mode               uint32            `json:"mode"`
	OwnerUID           uint32            `json:"ownerUid"`
	GroupGID           uint32            `json:"groupGid"`
	OwnershipAvailable bool              `json:"ownershipAvailable"`
	Symlink            bool              `json:"symlink"`
	LinkStatus         fsroot.LinkStatus `json:"linkStatus,omitempty"`
	MountUnavailable   bool              `json:"mountUnavailable,omitempty"`
	ReadOnly           bool              `json:"readOnly,omitempty"`
	Version            string            `json:"version"`
}

type pathResponse struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

type taskListResponse struct {
	Tasks   []taskSummary `json:"tasks"`
	Limit   int           `json:"limit"`
	Offset  int           `json:"offset"`
	HasMore bool          `json:"hasMore"`
}

type taskSummary struct {
	ID        string           `json:"id"`
	Kind      string           `json:"kind"`
	Status    store.TaskStatus `json:"status"`
	ScopePath string           `json:"scopePath"`
	CreatedAt string           `json:"createdAt"`
	UpdatedAt string           `json:"updatedAt"`
}

type taskAuditResponse struct {
	Events  []taskAuditEvent `json:"events"`
	Limit   int              `json:"limit"`
	Offset  int              `json:"offset"`
	HasMore bool             `json:"hasMore"`
}

type taskAuditEvent struct {
	ID         int64  `json:"id"`
	EventType  string `json:"eventType"`
	Actor      string `json:"actor"`
	Outcome    string `json:"outcome"`
	Message    string `json:"message,omitempty"`
	SourcePath string `json:"sourcePath,omitempty"`
	TargetPath string `json:"targetPath,omitempty"`
	CreatedAt  string `json:"createdAt"`
}

type trashListResponse struct {
	Items      []trashItemResponse `json:"items"`
	Limit      int                 `json:"limit"`
	NextCursor string              `json:"nextCursor"`
	HasMore    bool                `json:"hasMore"`
	Snapshot   int64               `json:"snapshot"`
}

type trashItemResponse struct {
	ID           string `json:"id"`
	OriginalPath string `json:"originalPath"`
	Name         string `json:"name"`
	Kind         string `json:"kind"`
	Size         int64  `json:"size"`
	DeletedAt    string `json:"deletedAt"`
	Status       string `json:"status"`
	Message      string `json:"message,omitempty"`
}

type restoreResponse struct {
	Path string `json:"path"`
}

const (
	maxAuditLabelRunes   = 128
	maxAuditMessageRunes = 1024
)

// New 创建使用独立文件协调锁的 HTTP API。
func New(files fileSystem, readiness func(context.Context) error) http.Handler {
	return NewWithLocker(files, readiness, nil)
}

// NewWithLocker 允许文件 API 与其他文件写入入口共享协调锁。
func NewWithLocker(files fileSystem, readiness func(context.Context) error, locks *coordination.Locker) http.Handler {
	return NewWithTaskStore(files, nil, readiness, locks)
}

// NewWithTaskStore 创建同时提供文件和任务历史查询的 HTTP API。
func NewWithTaskStore(files fileSystem, tasks taskReader, readiness func(context.Context) error, locks *coordination.Locker) http.Handler {
	return NewWithTrash(files, tasks, nil, readiness, locks)
}

// NewWithTrash 创建提供文件、任务历史和回收站接口的 HTTP API。
func NewWithTrash(files fileSystem, tasks taskReader, trash trashService, readiness func(context.Context) error, locks *coordination.Locker) http.Handler {
	return NewWithAgent(files, tasks, trash, nil, readiness, locks)
}

// NewWithAgent 创建带有人工审批执行接口的服务。
func NewWithAgent(files fileSystem, tasks taskReader, trash trashService, workflow agentWorkflow, readiness func(context.Context) error, locks *coordination.Locker) http.Handler {
	return NewWithAgentAndIndex(files, tasks, trash, workflow, nil, readiness, locks)
}

// NewWithAgentAndIndex 创建带索引查询和人工审批执行接口的服务。
func NewWithAgentAndIndex(files fileSystem, tasks taskReader, trash trashService, workflow agentWorkflow, index fileIndexer, readiness func(context.Context) error, locks *coordination.Locker) http.Handler {
	return NewWithAgentAndIndexAndConversation(files, tasks, trash, workflow, index, nil, readiness, locks)
}

// NewWithAgentAndIndexAndConversation 增加持久化 Agent 对话接口。
func NewWithAgentAndIndexAndConversation(files fileSystem, tasks taskReader, trash trashService, workflow agentWorkflow, index fileIndexer, conversations agentConversationWorkflow, readiness func(context.Context) error, locks *coordination.Locker) http.Handler {
	cleanupArchiveTemps()
	if locks == nil {
		locks = coordination.New()
	}
	api := &API{
		files: files, tasks: tasks, trash: trash, agent: workflow, conversations: conversations,
		index: index, locks: locks, readiness: readiness, archiveSlots: make(chan struct{}, 1),
		archiveReadSlots: make(chan struct{}, maxConcurrentArchiveReads),
	}
	api.agentSettings, _ = tasks.(agentSettingsRepository)
	api.audits, _ = tasks.(auditRepository)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", api.health)
	mux.HandleFunc("GET /api/v1/index/status", api.indexStatus)
	mux.HandleFunc("GET /api/v1/files", api.listFiles)
	mux.HandleFunc("POST /api/v1/files/archive", api.downloadArchive)
	mux.HandleFunc("POST /api/v1/files/archive/create", api.createArchive)
	mux.HandleFunc("GET /api/v1/files/archive/entries", api.archiveEntries)
	mux.HandleFunc("GET /api/v1/files/archive/content", api.archiveEntryContent)
	mux.HandleFunc("POST /api/v1/files/archive/extract", api.extractArchive)
	mux.HandleFunc("GET /api/v1/files/content", api.readFile)
	mux.HandleFunc("PUT /api/v1/files/content", api.writeTextFile)
	mux.HandleFunc("PATCH /api/v1/files/metadata", api.updateFileMetadata)
	mux.HandleFunc("GET /api/v1/files/preview", api.previewFile)
	mux.HandleFunc("GET /api/v1/files/office/preview", api.officePreview)
	mux.HandleFunc("GET /api/v1/tasks", api.listTasks)
	mux.HandleFunc("GET /api/v1/tasks/{id}/audit", api.taskAudit)
	mux.HandleFunc("GET /api/v1/audit", api.auditHistory)
	mux.HandleFunc("GET /api/v1/agent/status", api.agentStatus)
	mux.HandleFunc("GET /api/v1/settings/agent", api.getAgentSettings)
	mux.HandleFunc("PUT /api/v1/settings/agent", api.updateAgentSettings)
	mux.HandleFunc("GET /api/v1/agent/conversations", api.listAgentConversations)
	mux.HandleFunc("POST /api/v1/agent/conversations", api.createAgentConversation)
	mux.HandleFunc("GET /api/v1/agent/conversations/{id}", api.getAgentConversation)
	mux.HandleFunc("POST /api/v1/agent/conversations/{id}/messages", api.addAgentConversationMessage)
	mux.HandleFunc("POST /api/v1/agent/conversations/{id}/confirm-analysis", api.confirmAgentConversationAnalysis)
	mux.HandleFunc("GET /api/v1/agent/duplicate-scans/{id}", api.getDuplicateScan)
	mux.HandleFunc("POST /api/v1/agent/duplicate-scans/{id}/cancel", api.cancelDuplicateScan)
	mux.HandleFunc("GET /api/v1/agent/duplicate-scans/{id}/groups", api.listDuplicateGroups)
	mux.HandleFunc("GET /api/v1/agent/duplicate-scans/{id}/groups/{groupId}/files", api.listDuplicateFiles)
	mux.HandleFunc("POST /api/v1/agent/duplicate-scans/{id}/cleanup-plan", api.createDuplicateCleanupPlan)
	mux.HandleFunc("GET /api/v1/agent/tasks/{taskId}", api.getAgentTask)
	mux.HandleFunc("GET /api/v1/agent/plans/{planId}", api.getAgentPlan)
	mux.HandleFunc("GET /api/v1/agent/plans/{planId}/shell-changes", api.listAgentShellChanges)
	mux.HandleFunc("GET /api/v1/agent/plans/{planId}/shell-changes/{changeId}/preview", api.previewAgentShellChange)
	mux.HandleFunc("GET /api/v1/agent/plans/{planId}/operations/{operationId}/preview", api.previewAgentTextEdit)
	mux.HandleFunc("PUT /api/v1/agent/plans/{planId}/operations/{operationId}/content", api.reviseAgentTextCandidate)
	mux.HandleFunc("PUT /api/v1/agent/plans/{planId}/revision", api.reviseAgentPlan)
	mux.HandleFunc("POST /api/v1/agent/plans/{planId}/reject", api.rejectAgentPlan)
	mux.HandleFunc("POST /api/v1/agent/plans/{planId}/start", api.startAgentPlan)
	mux.HandleFunc("POST /api/v1/agent/plans/{planId}/steps/{operationId}/execute", api.executeAgentStep)
	mux.HandleFunc("POST /api/v1/agent/plans/{planId}/execute", api.executeAgentPlan)
	mux.HandleFunc("POST /api/v1/agent/plans/{planId}/stop", api.stopAgentPlanExecution)
	mux.HandleFunc("POST /api/v1/files/directories", api.createDirectory)
	mux.HandleFunc("POST /api/v1/files/symlinks", api.createSymlink)
	mux.HandleFunc("POST /api/v1/files/copy", api.copyFile)
	mux.HandleFunc("POST /api/v1/files/copy-directory", api.copyDirectory)
	mux.HandleFunc("POST /api/v1/files/move-directory", api.moveDirectory)
	mux.HandleFunc("POST /api/v1/files/move", api.moveFile)
	mux.HandleFunc("POST /api/v1/files/rename", api.renameFile)
	mux.HandleFunc("POST /api/v1/files/batch-rename", api.batchRename)
	mux.HandleFunc("POST /api/v1/files/batch-copy", api.batchCopy)
	mux.HandleFunc("POST /api/v1/files/batch-move", api.batchMove)
	mux.HandleFunc("POST /api/v1/files/batch-trash", api.batchTrash)
	mux.HandleFunc("DELETE /api/v1/files", api.deleteFile)
	mux.HandleFunc("GET /api/v1/trash", api.listTrash)
	mux.HandleFunc("POST /api/v1/trash/{id}/restore", api.restoreTrash)
	mux.HandleFunc("DELETE /api/v1/trash/{id}", api.purgeTrashItem)
	mux.HandleFunc("DELETE /api/v1/trash", api.purgeTrashSnapshot)
	return securityHeaders(refreshIndexAfterMutation(mux, index))
}

type agentPlanResponse struct {
	TaskID         string                   `json:"taskId"`
	PlanID         string                   `json:"planId"`
	PlanVersion    int                      `json:"planVersion"`
	Status         store.PlanStatus         `json:"status"`
	ExecutionMode  string                   `json:"executionMode,omitempty"`
	ReviewedAt     *string                  `json:"reviewedAt,omitempty"`
	StopRequested  bool                     `json:"stopRequested"`
	ProposalSource string                   `json:"proposalSource"`
	Operations     []agentOperationResponse `json:"operations"`
}

type agentOperationResponse struct {
	ID                string                `json:"id"`
	Ordinal           int                   `json:"ordinal"`
	Action            string                `json:"action"`
	SourcePath        string                `json:"sourcePath"`
	TargetPath        string                `json:"targetPath"`
	Reason            string                `json:"reason"`
	Evidence          string                `json:"evidence"`
	BeforeMode        *uint32               `json:"beforeMode"`
	Mode              *uint32               `json:"mode"`
	RequestedMode     *uint32               `json:"requestedMode,omitempty"`
	BeforeOwnerUID    *uint32               `json:"beforeOwnerUid"`
	OwnerUID          *uint32               `json:"ownerUid"`
	RequestedOwnerUID *uint32               `json:"requestedOwnerUid,omitempty"`
	BeforeGroupGID    *uint32               `json:"beforeGroupGid"`
	GroupGID          *uint32               `json:"groupGid"`
	RequestedGroupGID *uint32               `json:"requestedGroupGid,omitempty"`
	Approval          store.ApprovalState   `json:"approval"`
	ExecutionStatus   store.OperationStatus `json:"executionStatus"`
	PlannedPaths      []string              `json:"plannedPaths,omitempty"`
	ResultMessage     string                `json:"resultMessage,omitempty"`
	Command           string                `json:"command,omitempty"`
	WorkingDir        string                `json:"workingDir,omitempty"`
	ExitCode          *int                  `json:"exitCode,omitempty"`
	OutputDiscarded   bool                  `json:"outputDiscarded,omitempty"`
}

type agentPlanRevisionRequest struct {
	ExpectedPlanVersion int                          `json:"expectedPlanVersion"`
	Operations          []agentPlanRevisionOperation `json:"operations"`
}

type agentPlanRevisionOperation struct {
	Action     string  `json:"action"`
	SourcePath string  `json:"sourcePath"`
	TargetPath string  `json:"targetPath"`
	Reason     string  `json:"reason"`
	Evidence   string  `json:"evidence"`
	Mode       *uint32 `json:"mode"`
	OwnerUID   *uint32 `json:"ownerUid"`
	GroupGID   *uint32 `json:"groupGid"`
}

func (api *API) getAgentTask(w http.ResponseWriter, r *http.Request) {
	if api.agent == nil {
		writeError(w, http.StatusServiceUnavailable, "Agent service is not configured")
		return
	}
	taskID := r.PathValue("taskId")
	if !validTaskID(taskID) {
		writeError(w, http.StatusBadRequest, "invalid task identifier")
		return
	}
	plan, err := api.agent.LatestPlan(r.Context(), taskID)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	api.writeAgentPlanResponse(w, r, plan)
}

func (api *API) getAgentPlan(w http.ResponseWriter, r *http.Request) {
	if api.agent == nil {
		writeError(w, http.StatusServiceUnavailable, "Agent service is not configured")
		return
	}
	planID := r.PathValue("planId")
	if !validTaskID(planID) {
		writeError(w, http.StatusBadRequest, "invalid plan identifier")
		return
	}
	plan, err := api.agent.Plan(r.Context(), planID)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	api.writeAgentPlanResponse(w, r, plan)
}

type agentTextEditPreviewResponse struct {
	PlanID          string `json:"planId"`
	OperationID     string `json:"operationId"`
	Path            string `json:"path"`
	SourceContent   string `json:"sourceContent"`
	ProposedContent string `json:"proposedContent"`
}

type agentTextEditRevisionRequest struct {
	ExpectedPlanVersion int    `json:"expectedPlanVersion"`
	ProposedContent     string `json:"proposedContent"`
}

func (api *API) previewAgentTextEdit(w http.ResponseWriter, r *http.Request) {
	setNoStore(w)
	workflow, ok := api.agent.(agentTextWorkflow)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "Agent text editing is not configured")
		return
	}
	planID, operationID := r.PathValue("planId"), r.PathValue("operationId")
	version, err := strconv.Atoi(r.URL.Query().Get("expectedPlanVersion"))
	if !validTaskID(planID) || !validTaskID(operationID) || err != nil || version < 1 {
		writeError(w, http.StatusBadRequest, "invalid plan, operation, or expected version")
		return
	}
	preview, err := workflow.PreviewTextEdit(r.Context(), planID, operationID, version)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, agentTextEditPreviewResponse{
		PlanID: preview.PlanID, OperationID: preview.OperationID, Path: preview.Path,
		SourceContent: preview.SourceContent, ProposedContent: preview.ProposedContent,
	})
}

func (api *API) reviseAgentTextCandidate(w http.ResponseWriter, r *http.Request) {
	setNoStore(w)
	if !requireSameOrigin(w, r) {
		return
	}
	workflow, ok := api.agent.(agentTextWorkflow)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "Agent text editing is not configured")
		return
	}
	var request agentTextEditRevisionRequest
	if !decodeRequestJSONLimit(w, r, &request, 256<<10) {
		return
	}
	planID, operationID := r.PathValue("planId"), r.PathValue("operationId")
	if !validTaskID(planID) || !validTaskID(operationID) || request.ExpectedPlanVersion < 1 {
		writeError(w, http.StatusBadRequest, "invalid plan, operation, or expected version")
		return
	}
	plan, err := workflow.ReviseTextCandidate(r.Context(), planID, operationID, request.ExpectedPlanVersion, []byte(request.ProposedContent))
	if err != nil {
		writeAgentError(w, err)
		return
	}
	api.writeAgentPlanResponse(w, r, plan)
}

func setNoStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
}

func decodeRequestJSONLimit(w http.ResponseWriter, r *http.Request, value any, maximum int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maximum)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body is too large")
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid JSON request")
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "request must contain one JSON object")
		return false
	}
	return true
}

func (api *API) reviseAgentPlan(w http.ResponseWriter, r *http.Request) {
	if !requireSameOrigin(w, r) {
		return
	}
	if api.agent == nil {
		writeError(w, http.StatusServiceUnavailable, "Agent service is not configured")
		return
	}
	var request agentPlanRevisionRequest
	if !decodeRequestJSONLimit(w, r, &request, 4<<20) {
		return
	}
	planID := r.PathValue("planId")
	if !validTaskID(planID) || request.ExpectedPlanVersion < 1 || len(request.Operations) < 1 || len(request.Operations) > store.MaxAgentPlanOperations {
		writeError(w, http.StatusBadRequest, "invalid plan revision")
		return
	}
	proposals := make([]agent.Proposal, 0, len(request.Operations))
	for _, operation := range request.Operations {
		proposals = append(proposals, agent.Proposal{
			Action: operation.Action, SourcePath: operation.SourcePath, TargetPath: operation.TargetPath,
			Reason: operation.Reason, Evidence: operation.Evidence, Mode: operation.Mode,
			OwnerUID: operation.OwnerUID, GroupGID: operation.GroupGID,
		})
	}
	plan, err := api.agent.RevisePlan(r.Context(), planID, request.ExpectedPlanVersion, proposals)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	api.writeAgentPlanResponse(w, r, plan)
}

func (api *API) executeAgentPlan(w http.ResponseWriter, r *http.Request) {
	if !requireSameOrigin(w, r) {
		return
	}
	if api.agent == nil {
		writeError(w, http.StatusServiceUnavailable, "Agent service is not configured")
		return
	}
	var request struct {
		ExpectedPlanVersion int `json:"expectedPlanVersion"`
	}
	if !decodeRequestJSON(w, r, &request) {
		return
	}
	planID := r.PathValue("planId")
	if !validTaskID(planID) {
		writeError(w, http.StatusBadRequest, "invalid plan identifier")
		return
	}
	if request.ExpectedPlanVersion < 1 {
		writeError(w, http.StatusBadRequest, "invalid expected plan version")
		return
	}
	current, err := api.agent.Plan(r.Context(), planID)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	if current.Version != request.ExpectedPlanVersion || current.Status != store.PlanApproved ||
		current.ReviewedAt == nil || current.ExecutionMode != "bypass" {
		writeAgentError(w, agent.ErrConflict)
		return
	}
	plan, err := api.agent.Execute(r.Context(), planID, request.ExpectedPlanVersion)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	api.writeAgentPlanResponse(w, r, plan)
}

func (api *API) stopAgentPlanExecution(w http.ResponseWriter, r *http.Request) {
	if !requireSameOrigin(w, r) {
		return
	}
	if api.agent == nil {
		writeError(w, http.StatusServiceUnavailable, "Agent service is not configured")
		return
	}
	var request struct {
		ExpectedPlanVersion int `json:"expectedPlanVersion"`
	}
	if !decodeRequestJSON(w, r, &request) {
		return
	}
	planID := r.PathValue("planId")
	if !validTaskID(planID) {
		writeError(w, http.StatusBadRequest, "invalid plan identifier")
		return
	}
	if request.ExpectedPlanVersion < 1 {
		writeError(w, http.StatusBadRequest, "invalid expected plan version")
		return
	}
	plan, err := api.agent.StopExecution(r.Context(), planID, request.ExpectedPlanVersion)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	api.writeAgentPlanResponse(w, r, plan)
}

func (api *API) writeAgentPlanResponse(w http.ResponseWriter, r *http.Request, plan store.PlanRecord) {
	response := toAgentPlanResponse(plan)
	for _, operation := range plan.Operations {
		if operation.Action != "run_shell" {
			continue
		}
		reader, ok := api.agent.(agentShellPlanReader)
		if !ok {
			writeError(w, http.StatusServiceUnavailable, "Agent Shell review is not configured")
			return
		}
		run, err := reader.ShellRun(r.Context(), plan.ID)
		if err != nil || run.OperationID != operation.ID {
			writeAgentError(w, errors.Join(err, agent.ErrConflict))
			return
		}
		for index := range response.Operations {
			if response.Operations[index].ID == operation.ID {
				response.Operations[index].Command = run.Command
				response.Operations[index].WorkingDir = run.WorkingDirectory
				response.Operations[index].ExitCode = &run.ExitCode
				response.Operations[index].OutputDiscarded = run.OutputDiscarded
				break
			}
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func toAgentPlanResponse(plan store.PlanRecord) agentPlanResponse {
	proposalSource := "unknown"
	var planMetadata struct {
		ProposalSource string `json:"proposalSource"`
	}
	if json.Unmarshal([]byte(plan.DataScopeJSON), &planMetadata) == nil && planMetadata.ProposalSource != "" {
		proposalSource = planMetadata.ProposalSource
	}
	response := agentPlanResponse{
		TaskID: plan.TaskID, PlanID: plan.ID, PlanVersion: plan.Version,
		Status: plan.Status, ExecutionMode: plan.ExecutionMode, StopRequested: plan.StopRequested, ProposalSource: proposalSource,
		Operations: make([]agentOperationResponse, 0, len(plan.Operations)),
	}
	if plan.ReviewedAt != nil {
		value := plan.ReviewedAt.UTC().Format(time.RFC3339Nano)
		response.ReviewedAt = &value
	}
	for _, operation := range plan.Operations {
		item := agentOperationResponse{
			ID: operation.ID, Ordinal: operation.Ordinal, Action: operation.Action, SourcePath: operation.SourcePath,
			TargetPath: operation.TargetPath, Reason: operation.Reason, Evidence: operation.Evidence,
			RequestedMode: operation.Mode, RequestedOwnerUID: operation.OwnerUID, RequestedGroupGID: operation.GroupGID,
			Approval: operation.Approval, ExecutionStatus: operation.Status, ResultMessage: operation.ResultMessage,
		}
		if operation.Action == "extract_archive" {
			var archiveTarget struct {
				PlannedPaths []string `json:"plannedPaths"`
			}
			if json.Unmarshal([]byte(operation.TargetStateJSON), &archiveTarget) == nil {
				item.PlannedPaths = archiveTarget.PlannedPaths
			}
		}
		var source fsroot.Fingerprint
		if (operation.Action == "set_permissions" || operation.Action == "set_owner") && json.Unmarshal([]byte(operation.SourceStateJSON), &source) == nil {
			if operation.Action == "set_permissions" && operation.Mode != nil && source.IdentityAvailable {
				beforeMode := uint32(source.Mode.Perm())
				item.BeforeMode, item.Mode = &beforeMode, operation.Mode
			}
			if operation.Action == "set_owner" && source.IdentityAvailable && source.OwnershipAvailable {
				beforeUID, beforeGID := source.OwnerUID, source.GroupGID
				ownerUID, groupGID := beforeUID, beforeGID
				if operation.OwnerUID != nil {
					ownerUID = *operation.OwnerUID
				}
				if operation.GroupGID != nil {
					groupGID = *operation.GroupGID
				}
				item.BeforeOwnerUID, item.OwnerUID = &beforeUID, &ownerUID
				item.BeforeGroupGID, item.GroupGID = &beforeGID, &groupGID
			}
		}
		response.Operations = append(response.Operations, item)
	}
	return response
}

func writeAgentError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "Agent plan not found")
	case errors.Is(err, agent.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid Agent proposal or decision")
	case errors.Is(err, agent.ErrUnsupported):
		writeError(w, http.StatusUnprocessableEntity, "Agent operation is not supported for this file or filesystem")
	case errors.Is(err, agent.ErrConflict), errors.Is(err, store.ErrConflict), errors.Is(err, fsroot.ErrConflict):
		writeError(w, http.StatusConflict, "Agent plan is stale or already changed")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusRequestTimeout, "Agent operation was cancelled")
	default:
		writeError(w, http.StatusInternalServerError, "Agent operation failed")
	}
}

func (api *API) health(w http.ResponseWriter, r *http.Request) {
	if api.readiness != nil && api.readiness(r.Context()) != nil {
		writeError(w, http.StatusServiceUnavailable, "service is not ready")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (api *API) listTasks(w http.ResponseWriter, r *http.Request) {
	if api.tasks == nil {
		writeError(w, http.StatusServiceUnavailable, "task history is not configured")
		return
	}
	query, ok := parseTaskListQuery(w, r)
	if !ok {
		return
	}
	page, err := api.tasks.ListTasks(r.Context(), query)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "task history is unavailable")
		return
	}
	response := taskListResponse{
		Tasks:   make([]taskSummary, 0, len(page.Tasks)),
		Limit:   page.Limit,
		Offset:  page.Offset,
		HasMore: page.HasMore,
	}
	for _, task := range page.Tasks {
		response.Tasks = append(response.Tasks, taskSummary{
			ID:        task.ID,
			Kind:      task.Kind,
			Status:    task.Status,
			ScopePath: safeTaskPath(task.ScopePath),
			CreatedAt: formatAPITime(task.CreatedAt),
			UpdatedAt: formatAPITime(task.UpdatedAt),
		})
	}
	writeJSON(w, http.StatusOK, response)
}

func (api *API) taskAudit(w http.ResponseWriter, r *http.Request) {
	if api.tasks == nil {
		writeError(w, http.StatusServiceUnavailable, "task history is not configured")
		return
	}
	taskID := r.PathValue("id")
	if !validTaskID(taskID) {
		writeError(w, http.StatusBadRequest, "invalid task ID")
		return
	}
	limit, offset, ok := parseTaskPage(w, r, false)
	if !ok {
		return
	}
	task, err := api.tasks.Task(r.Context(), taskID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "task audit history is unavailable")
		return
	}
	page, err := api.tasks.AuditForTaskPage(r.Context(), taskID, limit, offset)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "task audit history is unavailable")
		return
	}
	response := taskAuditResponse{
		Events:  make([]taskAuditEvent, 0, len(page.Records)),
		Limit:   page.Limit,
		Offset:  page.Offset,
		HasMore: page.HasMore,
	}
	for _, record := range page.Records {
		response.Events = append(response.Events, taskAuditEvent{
			ID:         record.ID,
			EventType:  safeAuditLabel(record.EventType, task.RequestText),
			Actor:      safeAuditLabel(record.Actor, task.RequestText),
			Outcome:    safeAuditLabel(record.Outcome, task.RequestText),
			Message:    safeAuditMessage(record.Message, task.RequestText),
			SourcePath: safeTaskPath(record.SourcePath),
			TargetPath: safeTaskPath(record.TargetPath),
			CreatedAt:  formatAPITime(record.CreatedAt),
		})
	}
	writeJSON(w, http.StatusOK, response)
}

func parseTaskListQuery(w http.ResponseWriter, r *http.Request) (store.TaskListQuery, bool) {
	limit, offset, values, ok := parseTaskPageValues(w, r, []string{"limit", "offset", "kind", "status"})
	if !ok {
		return store.TaskListQuery{}, false
	}
	query := store.TaskListQuery{Limit: limit, Offset: offset}
	if value := values["kind"]; len(value) == 1 {
		if value[0] == "" || len(value[0]) > 128 || !utf8.ValidString(value[0]) || hasControlRune(value[0]) {
			writeError(w, http.StatusBadRequest, "invalid task kind filter")
			return store.TaskListQuery{}, false
		}
		query.Kind = value[0]
	}
	if value := values["status"]; len(value) == 1 {
		status := store.TaskStatus(value[0])
		if !validPublicTaskStatus(status) {
			writeError(w, http.StatusBadRequest, "invalid task status filter")
			return store.TaskListQuery{}, false
		}
		query.Status = status
	}
	return query, true
}

func parseTaskPage(w http.ResponseWriter, r *http.Request, allowFilters bool) (int, int, bool) {
	keys := []string{"limit", "offset"}
	if allowFilters {
		keys = append(keys, "kind", "status")
	}
	limit, offset, _, ok := parseTaskPageValues(w, r, keys)
	return limit, offset, ok
}

func parseTaskPageValues(w http.ResponseWriter, r *http.Request, allowedKeys []string) (int, int, url.Values, bool) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid task query")
		return 0, 0, nil, false
	}
	allowed := make(map[string]struct{}, len(allowedKeys))
	for _, key := range allowedKeys {
		allowed[key] = struct{}{}
	}
	for key, value := range values {
		if _, ok := allowed[key]; !ok || len(value) != 1 {
			writeError(w, http.StatusBadRequest, "invalid or repeated task query parameter")
			return 0, 0, nil, false
		}
	}
	limit, offset := 50, 0
	if raw, ok := values["limit"]; ok {
		limit, err = strconv.Atoi(raw[0])
		if err != nil || limit < 1 || limit > 100 {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and 100")
			return 0, 0, nil, false
		}
	}
	if raw, ok := values["offset"]; ok {
		offset, err = strconv.Atoi(raw[0])
		if err != nil || offset < 0 || offset > 1000000 {
			writeError(w, http.StatusBadRequest, "offset must be between 0 and 1000000")
			return 0, 0, nil, false
		}
	}
	return limit, offset, values, true
}

func validPublicTaskStatus(status store.TaskStatus) bool {
	switch status {
	case store.TaskQueued, store.TaskAnalyzing, store.TaskAwaitingApproval,
		store.TaskRunning, store.TaskPaused, store.TaskSucceeded,
		store.TaskPartial, store.TaskFailed, store.TaskCancelled:
		return true
	default:
		return false
	}
}

func validTaskID(id string) bool {
	if id == "" || len(id) > 128 || !utf8.ValidString(id) || strings.ContainsAny(id, "/\\") {
		return false
	}
	return !hasControlRune(id)
}

func formatAPITime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.999999999Z07:00")
}

var (
	auditSecretAssignment = regexp.MustCompile(`(?i)(api[_-]?key|access[_-]?token|refresh[_-]?token|password|passwd|secret|authorization)(\s*[:=]\s*)[^\s,;]+`)
	auditBearerToken      = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/-]+=*`)
	auditPrefixedKey      = regexp.MustCompile(`\b(?:sk|pk)-[A-Za-z0-9_-]{12,}\b`)
)

func safeTaskPath(value string) string {
	if value == "" || !validRelativePath(value, true) {
		return ""
	}
	return value
}

func safeAuditMessage(value, requestText string) string {
	return safeAuditText(value, requestText, maxAuditMessageRunes)
}

func safeAuditLabel(value, requestText string) string {
	return safeAuditText(value, requestText, maxAuditLabelRunes)
}

func safeAuditText(value, requestText string, maxRunes int) string {
	if requestText != "" {
		value = strings.ReplaceAll(value, requestText, "[任务请求正文已隐藏]")
	}
	value = strings.Map(func(character rune) rune {
		if unicode.IsControl(character) {
			return ' '
		}
		return character
	}, value)
	value = auditSecretAssignment.ReplaceAllString(value, "$1$2[已隐藏]")
	value = auditBearerToken.ReplaceAllString(value, "Bearer [已隐藏]")
	value = auditPrefixedKey.ReplaceAllString(value, "[密钥已隐藏]")
	runes := []rune(strings.TrimSpace(value))
	if len(runes) > maxRunes {
		runes = append(runes[:maxRunes-1], '…')
	}
	return string(runes)
}

func (api *API) listFiles(w http.ResponseWriter, r *http.Request) {
	api.listFilePage(w, r)
}

// createDirectory 创建唯一名称的真实目录。
func (api *API) createDirectory(w http.ResponseWriter, r *http.Request) {
	if !requireSameOrigin(w, r) {
		return
	}
	var request struct {
		Parent *string `json:"parent"`
		Name   string  `json:"name"`
	}
	if !decodeRequestJSON(w, r, &request) {
		return
	}
	if request.Parent == nil || !validRelativePath(*request.Parent, true) || isReservedPath(*request.Parent) {
		writeError(w, http.StatusBadRequest, "parent must be a clean path relative to the managed root")
		return
	}
	if !validEntryName(request.Name) || request.Name == fsroot.TrashDirectoryName {
		writeError(w, http.StatusBadRequest, "name must be a single valid file name")
		return
	}

	parent := *request.Parent
	wantedPath := joinPath(parent, request.Name)
	release, ok := api.lockMutationPaths(w, r, wantedPath)
	if !ok {
		return
	}
	defer release()

	entries, err := api.files.List(parent)
	if err != nil {
		writeFileError(w, err)
		return
	}
	occupiedNames := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		occupiedNames[entry.Name] = struct{}{}
	}
	for suffix := 1; suffix <= 10000; suffix++ {
		name := request.Name
		if suffix > 1 {
			name = fmt.Sprintf("%s (%d)", request.Name, suffix)
		}
		if _, occupied := occupiedNames[name]; occupied {
			continue
		}
		createdPath := joinPath(parent, name)
		auditID, ok := beginFileAudit(api, w, r, "manual_create_directory", "", createdPath)
		if !ok {
			return
		}
		if err := api.files.Mkdir(createdPath, 0o755); err != nil {
			if !finishFileAudit(api, w, r, auditID, "failed", "目录创建未完成。") {
				return
			}
			if errors.Is(err, fsroot.ErrConflict) {
				occupiedNames[name] = struct{}{}
				continue
			}
			writeFileError(w, err)
			return
		}
		if !finishFileAudit(api, w, r, auditID, "succeeded", "目录已创建。") {
			return
		}
		writeJSON(w, http.StatusCreated, pathResponse{Path: createdPath, Name: name})
		return
	}
	writeError(w, http.StatusConflict, "no available name could be reserved")
}

// renameFile 按目录快照重命名单个条目，不覆盖目标。
func (api *API) renameFile(w http.ResponseWriter, r *http.Request) {
	if !requireSameOrigin(w, r) {
		return
	}
	var request struct {
		Path    string `json:"path"`
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if !decodeRequestJSON(w, r, &request) {
		return
	}
	if !validRelativePath(request.Path, false) || isReservedPath(request.Path) {
		writeError(w, http.StatusBadRequest, "path must identify an entry within the managed root")
		return
	}
	if !validEntryName(request.Name) || request.Name == fsroot.TrashDirectoryName {
		writeError(w, http.StatusBadRequest, "name must be a single valid file name")
		return
	}
	if request.Version == "" {
		writeError(w, http.StatusBadRequest, "version is required")
		return
	}

	parent := parentPath(request.Path)
	newPath := joinPath(parent, request.Name)
	release, ok := api.lockMutationPaths(w, r, request.Path, newPath)
	if !ok {
		return
	}
	defer release()

	if _, err := api.verifyEntryVersion(request.Path, request.Version); err != nil {
		writeFileError(w, err)
		return
	}
	auditID, ok := beginFileAudit(api, w, r, "manual_rename", request.Path, newPath)
	if !ok {
		return
	}
	// 审计写入后重新核对版本，避免等待期间的路径替换继续执行。
	if _, err := api.verifyEntryVersion(request.Path, request.Version); err != nil {
		if !finishFileAudit(api, w, r, auditID, "failed", "源文件状态已变化，未执行重命名。") {
			return
		}
		writeFileError(w, err)
		return
	}
	if err := api.files.RenameNoReplace(request.Path, newPath); err != nil {
		if !finishFileAudit(api, w, r, auditID, "failed", "重命名未完成。") {
			return
		}
		writeFileError(w, err)
		return
	}
	if !finishFileAudit(api, w, r, auditID, "succeeded", "重命名完成。") {
		return
	}
	writeJSON(w, http.StatusOK, pathResponse{Path: newPath, Name: request.Name})
}

// deleteFile 默认移入回收站，仅在明确确认后永久删除单项。
func (api *API) deleteFile(w http.ResponseWriter, r *http.Request) {
	if !requireSameOrigin(w, r) {
		return
	}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid path or version")
		return
	}
	for key, value := range values {
		if (key != "path" && key != "version" && key != "confirmPermanent") || len(value) != 1 {
			writeError(w, http.StatusBadRequest, "invalid or repeated delete query parameter")
			return
		}
	}
	confirmations, hasConfirmation := values["confirmPermanent"]
	if hasConfirmation && (len(confirmations) != 1 || (confirmations[0] != "true" && confirmations[0] != "false")) {
		writeError(w, http.StatusBadRequest, "confirmPermanent must be true or false")
		return
	}
	permanent := hasConfirmation && confirmations[0] == "true"
	paths := values["path"]
	versions := values["version"]
	if len(paths) != 1 || !validRelativePath(paths[0], false) || isReservedPath(paths[0]) || len(versions) != 1 || versions[0] == "" {
		writeError(w, http.StatusBadRequest, "a valid path and version are required")
		return
	}
	if !permanent && api.trash == nil {
		writeError(w, http.StatusServiceUnavailable, "trash service is not configured")
		return
	}
	relativePath := paths[0]
	release, ok := api.lockMutationPaths(w, r, relativePath)
	if !ok {
		return
	}
	defer release()

	entry, err := api.verifyEntryVersion(relativePath, versions[0])
	if err != nil {
		writeFileError(w, err)
		return
	}
	if entry.Kind == fsroot.KindOther && !entry.Symlink {
		writeError(w, http.StatusBadRequest, "special filesystem entries cannot be deleted through this API")
		return
	}
	if !permanent {
		item, err := api.trash.MoveToTrash(r.Context(), entry)
		if err != nil {
			writeFileError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, toTrashItemResponse(item))
		return
	}
	if api.trash != nil {
		if err := api.trash.PermanentRemove(r.Context(), entry); err != nil {
			writeFileError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if entry.Kind == fsroot.KindDirectory && !entry.Symlink {
		children, err := api.files.List(relativePath)
		if err != nil {
			writeFileError(w, err)
			return
		}
		if len(children) != 0 {
			writeError(w, http.StatusConflict, "directory is not empty")
			return
		}
	}
	if err := api.files.Remove(relativePath); err != nil {
		writeFileError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (api *API) listTrash(w http.ResponseWriter, r *http.Request) {
	if api.trash == nil {
		writeError(w, http.StatusServiceUnavailable, "trash service is not configured")
		return
	}
	limit, cursor, ok := parseTrashPage(w, r)
	if !ok {
		return
	}
	page, err := api.trash.List(r.Context(), limit, cursor)
	if err != nil {
		if errors.Is(err, trashsvc.ErrInvalidCursor) || errors.Is(err, store.ErrConflict) {
			writeError(w, http.StatusBadRequest, "trash cursor is invalid or expired")
			return
		}
		writeError(w, http.StatusInternalServerError, "trash list is unavailable")
		return
	}
	response := trashListResponse{Items: make([]trashItemResponse, 0, len(page.Items)), Limit: page.Limit, NextCursor: page.NextCursor, HasMore: page.HasMore, Snapshot: page.Snapshot}
	for _, item := range page.Items {
		response.Items = append(response.Items, toTrashItemResponse(item))
	}
	writeJSON(w, http.StatusOK, response)
}

func (api *API) restoreTrash(w http.ResponseWriter, r *http.Request) {
	if !requireSameOrigin(w, r) {
		return
	}
	if api.trash == nil {
		writeError(w, http.StatusServiceUnavailable, "trash service is not configured")
		return
	}
	id := r.PathValue("id")
	item, err := api.trash.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "trash item not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "trash item is unavailable")
		return
	}
	release, ok := api.lockTrashMutation(w, r, item.ID, item.OriginalPath)
	if !ok {
		return
	}
	defer release()
	restoredPath, err := api.trash.Restore(r.Context(), id)
	if err != nil {
		writeFileError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, restoreResponse{Path: restoredPath})
}

func (api *API) purgeTrashItem(w http.ResponseWriter, r *http.Request) {
	if !requireSameOrigin(w, r) {
		return
	}
	if api.trash == nil {
		writeError(w, http.StatusServiceUnavailable, "trash service is not configured")
		return
	}
	values, queryErr := url.ParseQuery(r.URL.RawQuery)
	if queryErr != nil || len(values) != 1 {
		writeError(w, http.StatusBadRequest, "confirmPermanent=true is required")
		return
	}
	if !confirmPermanentQuery(w, r) {
		return
	}
	id := r.PathValue("id")
	item, err := api.trash.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "trash item not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "trash item is unavailable")
		return
	}
	release, ok := api.lockTrashMutation(w, r, item.ID, item.OriginalPath)
	if !ok {
		return
	}
	defer release()
	if err := api.trash.Purge(r.Context(), id); err != nil {
		writeFileError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (api *API) purgeTrashSnapshot(w http.ResponseWriter, r *http.Request) {
	if !requireSameOrigin(w, r) {
		return
	}
	if api.trash == nil {
		writeError(w, http.StatusServiceUnavailable, "trash service is not configured")
		return
	}
	if !confirmPermanentQuery(w, r) {
		return
	}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(values) != 2 || len(values["snapshot"]) != 1 {
		writeError(w, http.StatusBadRequest, "a valid snapshot is required")
		return
	}
	snapshot, err := strconv.ParseInt(values.Get("snapshot"), 10, 64)
	if err != nil || snapshot < 0 || strconv.FormatInt(snapshot, 10) != values.Get("snapshot") {
		writeError(w, http.StatusBadRequest, "a valid snapshot is required")
		return
	}
	result := trashsvc.PurgeResult{Snapshot: snapshot, Errors: make([]trashsvc.PurgeError, 0)}
	var after int64
	for {
		items, err := api.trash.PurgeCandidates(r.Context(), snapshot, after, 100)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "trash cleanup list is unavailable")
			return
		}
		if len(items) == 0 {
			break
		}
		for _, item := range items {
			id, _ := strconv.ParseInt(item.ID, 10, 64)
			after = id
			if item.Status == string(store.TrashNeedsAttention) {
				result.NeedsAttention++
				continue
			}
			release, ok := api.lockTrashMutation(w, r, item.ID, item.OriginalPath)
			if !ok {
				return
			}
			err := api.trash.PurgeAtSnapshot(r.Context(), item.ID, snapshot)
			release()
			if err != nil {
				result.Failed++
				result.Errors = append(result.Errors, trashsvc.PurgeError{ID: item.ID, Message: "清理未完成，已保留并标记待核查"})
				if current, getErr := api.trash.Get(r.Context(), item.ID); getErr == nil && current.Status == string(store.TrashNeedsAttention) {
					result.NeedsAttention++
				}
				continue
			}
			result.Purged++
		}
	}
	status := http.StatusOK
	if result.Failed != 0 {
		status = http.StatusMultiStatus
	}
	writeJSON(w, status, result)
}

func (api *API) readFile(w http.ResponseWriter, r *http.Request) {
	relativePath, ok := requestPath(w, r)
	if !ok {
		return
	}
	if relativePath == "" {
		writeError(w, http.StatusBadRequest, "a file path is required")
		return
	}

	file, entry, err := api.files.OpenRead(relativePath)
	if err != nil {
		writeFileError(w, err)
		return
	}
	defer file.Close()
	if entry.Kind != fsroot.Kind("file") {
		writeError(w, http.StatusBadRequest, "only regular files can be read")
		return
	}

	contentDisposition := mime.FormatMediaType("attachment", map[string]string{"filename": entry.Name})
	if contentDisposition == "" {
		contentDisposition = "attachment"
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", contentDisposition)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-FileSage-Version", versionForEntry(entry))
	seeker, ok := file.(io.ReadSeeker)
	if !ok {
		writeError(w, http.StatusInternalServerError, "file streaming is unavailable")
		return
	}
	http.ServeContent(w, r, entry.Name, entry.ModifiedAt, seeker)
}

func (api *API) previewFile(w http.ResponseWriter, r *http.Request) {
	relativePath, ok := requestPath(w, r)
	if !ok {
		return
	}
	if relativePath == "" {
		writeError(w, http.StatusBadRequest, "a file path is required")
		return
	}

	contentType, ok := previewContentType(relativePath)
	if !ok {
		writeError(w, http.StatusUnsupportedMediaType, "this file type cannot be previewed inline")
		return
	}
	file, entry, err := api.files.OpenRead(relativePath)
	if err != nil {
		writeFileError(w, err)
		return
	}
	defer file.Close()
	if entry.Kind != fsroot.KindFile {
		writeError(w, http.StatusBadRequest, "only regular files can be previewed")
		return
	}
	seeker, ok := file.(io.ReadSeeker)
	if !ok {
		writeError(w, http.StatusInternalServerError, "file streaming is unavailable")
		return
	}

	contentDisposition := mime.FormatMediaType("inline", map[string]string{"filename": entry.Name})
	if contentDisposition == "" {
		contentDisposition = "inline"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", contentDisposition)
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, entry.Name, entry.ModifiedAt, seeker)
}

func previewContentType(relativePath string) (string, bool) {
	switch strings.ToLower(filepath.Ext(relativePath)) {
	case ".pdf":
		return "application/pdf", true
	case ".avif":
		return "image/avif", true
	case ".bmp":
		return "image/bmp", true
	case ".gif":
		return "image/gif", true
	case ".heic":
		return "image/heic", true
	case ".heif":
		return "image/heif", true
	case ".jpeg", ".jpg":
		return "image/jpeg", true
	case ".png":
		return "image/png", true
	case ".webp":
		return "image/webp", true
	case ".aac":
		return "audio/aac", true
	case ".flac":
		return "audio/flac", true
	case ".m4a":
		return "audio/mp4", true
	case ".mp3":
		return "audio/mpeg", true
	case ".oga", ".ogg", ".opus":
		return "audio/ogg", true
	case ".wav":
		return "audio/wav", true
	case ".avi":
		return "video/x-msvideo", true
	case ".m4v", ".mp4":
		return "video/mp4", true
	case ".mkv":
		return "video/x-matroska", true
	case ".mov":
		return "video/quicktime", true
	case ".ogv":
		return "video/ogg", true
	case ".webm":
		return "video/webm", true
	default:
		return "", false
	}
}

func requestPath(w http.ResponseWriter, r *http.Request) (string, bool) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid path")
		return "", false
	}
	relativePath := values.Get("path")
	if relativePath == "" {
		return "", true
	}
	if !validRelativePath(relativePath, true) || isReservedPath(relativePath) {
		writeError(w, http.StatusBadRequest, "path must be a clean path relative to the managed root")
		return "", false
	}
	return relativePath, true
}

func parseTrashPage(w http.ResponseWriter, r *http.Request) (int, string, bool) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid trash query")
		return 0, "", false
	}
	for key, value := range values {
		if (key != "limit" && key != "cursor") || len(value) != 1 {
			writeError(w, http.StatusBadRequest, "invalid or repeated trash query parameter")
			return 0, "", false
		}
	}
	limit := 50
	if value := values.Get("limit"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 100 {
			writeError(w, http.StatusBadRequest, "trash limit must be between 1 and 100")
			return 0, "", false
		}
		limit = parsed
	}
	cursor := values.Get("cursor")
	if len(cursor) > 64 {
		writeError(w, http.StatusBadRequest, "trash cursor is too long")
		return 0, "", false
	}
	return limit, cursor, true
}

func confirmPermanentQuery(w http.ResponseWriter, r *http.Request) bool {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid confirmation query")
		return false
	}
	confirmation := values["confirmPermanent"]
	if len(confirmation) != 1 || confirmation[0] != "true" {
		writeError(w, http.StatusBadRequest, "permanent cleanup is irreversible; set confirmPermanent=true")
		return false
	}
	return true
}

func isReservedPath(relativePath string) bool {
	for _, component := range strings.Split(relativePath, "/") {
		if component == fsroot.TrashDirectoryName || strings.HasPrefix(component, ".filesage-archive-stage-") {
			return true
		}
	}
	return false
}

func toTrashItemResponse(item trashsvc.Item) trashItemResponse {
	return trashItemResponse{
		ID: item.ID, OriginalPath: item.OriginalPath, Name: item.Name, Kind: item.Kind,
		Size: item.Size, DeletedAt: formatAPITime(item.DeletedAt), Status: item.Status, Message: item.Message,
	}
}

// lockMutationPaths 同时锁定条目及祖先目录，避免父子操作交错。
func (api *API) lockMutationPaths(w http.ResponseWriter, r *http.Request, paths ...string) (func(), bool) {
	return api.lockMutationPathsWithKeys(w, r, nil, nil, paths...)
}

type mutationPathSnapshot struct {
	path    string
	exists  bool
	version string
}

// lockMutationPathsWithInodePaths 同时锁定路径意图和已有对象身份。
func (api *API) lockMutationPathsWithInodePaths(w http.ResponseWriter, r *http.Request, inodePaths []string, paths ...string) (func(), bool) {
	return api.lockMutationPathsWithKeys(w, r, nil, inodePaths, paths...)
}

func (api *API) lockMutationPathsWithKeys(w http.ResponseWriter, r *http.Request, extraKeys, inodePaths []string, paths ...string) (func(), bool) {
	keys := coordination.PathKeys(paths...)
	keys = append(keys, extraKeys...)
	inspector, indexed := api.files.(indexedFileSystem)
	snapshots := make([]mutationPathSnapshot, 0, len(paths))
	forced := make(map[string]struct{}, len(inodePaths))
	for _, relativePath := range inodePaths {
		forced[relativePath] = struct{}{}
	}
	if indexed {
		seen := make(map[string]struct{}, len(paths))
		for _, relativePath := range paths {
			if relativePath == "" {
				continue
			}
			if _, exists := seen[relativePath]; exists {
				continue
			}
			seen[relativePath] = struct{}{}
			entry, err := inspector.LstatEntry(relativePath)
			if errors.Is(err, fs.ErrNotExist) {
				snapshots = append(snapshots, mutationPathSnapshot{path: relativePath})
				continue
			}
			if err != nil {
				writeFileError(w, err)
				return nil, false
			}
			snapshots = append(snapshots, mutationPathSnapshot{path: relativePath, exists: true, version: versionForEntry(entry)})
			_, forceInode := forced[relativePath]
			if forceInode || entry.Symlink || entry.Kind != fsroot.KindDirectory {
				if key := mutationInodeKey(entry); key != "" {
					keys = append(keys, key)
				}
			}
		}
	}

	release, err := api.locks.Lock(r.Context(), keys...)
	if err != nil {
		writeError(w, http.StatusRequestTimeout, "file operation was cancelled while waiting for another change")
		return nil, false
	}
	if indexed {
		for _, snapshot := range snapshots {
			entry, statErr := inspector.LstatEntry(snapshot.path)
			if !snapshot.exists {
				if errors.Is(statErr, fs.ErrNotExist) {
					continue
				}
				release()
				if statErr != nil {
					writeFileError(w, statErr)
				} else {
					writeFileError(w, fsroot.ErrConflict)
				}
				return nil, false
			}
			if statErr != nil || versionForEntry(entry) != snapshot.version {
				release()
				writeFileError(w, fsroot.ErrConflict)
				return nil, false
			}
		}
	}
	return release, true
}

// lockTrashMutation 以回收区内实体 inode 协调恢复和清理。
func (api *API) lockTrashMutation(w http.ResponseWriter, r *http.Request, id, originalPath string) (func(), bool) {
	entry, err := api.trash.LockIdentity(r.Context(), id)
	if err != nil {
		writeTrashIdentityError(w, err)
		return nil, false
	}
	key := mutationInodeKey(entry)
	if key == "" {
		writeFileError(w, fsroot.ErrIdentityUnavailable)
		return nil, false
	}
	release, ok := api.lockMutationPathsWithKeys(w, r, []string{key}, nil, originalPath)
	if !ok {
		return nil, false
	}
	current, err := api.trash.LockIdentity(r.Context(), id)
	if err != nil || mutationInodeKey(current) != key {
		release()
		if err != nil {
			writeTrashIdentityError(w, err)
		} else {
			writeFileError(w, fsroot.ErrConflict)
		}
		return nil, false
	}
	return release, true
}

func writeTrashIdentityError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "trash item not found")
	case errors.Is(err, fsroot.ErrConflict), errors.Is(err, store.ErrConflict):
		writeFileError(w, fsroot.ErrConflict)
	default:
		writeFileError(w, err)
	}
}

// mutationInodeKey 按操作对象语义锁定软链接本身或真实文件。
func mutationInodeKey(entry fsroot.Entry) string {
	if entry.Symlink {
		if !entry.LinkIdentityAvailable || entry.LinkInode == 0 {
			return ""
		}
		return coordination.InodeKey(entry.LinkDevice, entry.LinkInode)
	}
	if !entry.IdentityAvailable || entry.Inode == 0 {
		return ""
	}
	return coordination.InodeKey(entry.Device, entry.Inode)
}

// verifyEntryVersion 复核列表令牌对应的当前目录项。
func (api *API) verifyEntryVersion(relativePath, expected string) (fsroot.Entry, error) {
	entries, err := api.files.List(parentPath(relativePath))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fsroot.Entry{}, fsroot.ErrConflict
		}
		return fsroot.Entry{}, err
	}
	for _, entry := range entries {
		if entry.Path != relativePath {
			continue
		}
		if entry.Symlink {
			if !entry.LinkIdentityAvailable {
				return fsroot.Entry{}, fsroot.ErrIdentityUnavailable
			}
		} else if !entry.IdentityAvailable {
			return fsroot.Entry{}, fsroot.ErrIdentityUnavailable
		}
		if versionForEntry(entry) != expected {
			return fsroot.Entry{}, fsroot.ErrConflict
		}
		return entry, nil
	}
	return fsroot.Entry{}, fsroot.ErrConflict
}

func decodeRequestJSON(w http.ResponseWriter, r *http.Request, value any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body is too large")
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid JSON request")
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "request must contain one JSON object")
		return false
	}
	return true
}

func requireEmptyRequestBody(w http.ResponseWriter, r *http.Request) bool {
	var probe [1]byte
	n, err := r.Body.Read(probe[:])
	if n == 0 && errors.Is(err, io.EOF) {
		return true
	}
	writeError(w, http.StatusBadRequest, "request body must be empty")
	return false
}

func requireSameOrigin(w http.ResponseWriter, r *http.Request) bool {
	// 浏览器来源元数据可在 Origin 缺失时拦截跨站写请求。
	fetchSite := r.Header.Get("Sec-Fetch-Site")
	if fetchSite != "" && fetchSite != "same-origin" && fetchSite != "none" {
		writeError(w, http.StatusForbidden, "cross-origin file changes are not allowed")
		return false
	}
	origins := r.Header.Values("Origin")
	if len(origins) == 0 {
		return true
	}
	if len(origins) != 1 || !sameOrigin(origins[0], requestScheme(r), r.Host) {
		writeError(w, http.StatusForbidden, "cross-origin file changes are not allowed")
		return false
	}
	return true
}

// sameOrigin 校验协议、主机与有效端口完全一致。
func sameOrigin(origin, requestScheme, requestHost string) bool {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Opaque != "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.ForceQuery || strings.ContainsAny(origin, "?#") || (parsed.Path != "" && parsed.Path != "/") {
		return false
	}
	scheme := strings.ToLower(parsed.Scheme)
	requestScheme = strings.ToLower(requestScheme)
	if (scheme != "http" && scheme != "https") || scheme != requestScheme {
		return false
	}
	originHost, originPort, ok := normalizedAuthority(parsed.Host, scheme)
	if !ok {
		return false
	}
	requestHost, requestPort, ok := normalizedAuthority(requestHost, requestScheme)
	return ok && strings.EqualFold(originHost, requestHost) && originPort == requestPort
}

func requestScheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	if r.URL.Scheme != "" {
		return strings.ToLower(r.URL.Scheme)
	}
	return "http"
}

func normalizedAuthority(authority, scheme string) (host, port string, ok bool) {
	parsed, err := url.Parse("//" + authority)
	if err != nil || parsed.Host != authority || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", "", false
	}
	host = strings.ToLower(parsed.Hostname())
	if host == "" {
		return "", "", false
	}
	port = parsed.Port()
	if port == "" {
		if scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return host, port, true
}

func validRelativePath(relativePath string, allowRoot bool) bool {
	if relativePath == "" {
		return allowRoot
	}
	if len(relativePath) > 4096 || !utf8.ValidString(relativePath) || strings.ContainsRune(relativePath, '\\') || path.IsAbs(relativePath) || filepath.IsAbs(relativePath) || filepath.VolumeName(relativePath) != "" || path.Clean(relativePath) != relativePath {
		return false
	}
	if hasDrivePrefix(relativePath) {
		return false
	}
	for _, component := range strings.Split(relativePath, "/") {
		if component == "" || component == "." || component == ".." || len(component) > 255 || hasControlRune(component) {
			return false
		}
	}
	return true
}

func validEntryName(name string) bool {
	return name != "" && len(name) <= 255 && utf8.ValidString(name) && name != "." && name != ".." && !strings.ContainsAny(name, "/\\") && !hasControlRune(name) && !hasDrivePrefix(name)
}

func hasControlRune(value string) bool {
	for _, character := range value {
		if unicode.IsControl(character) {
			return true
		}
	}
	return false
}

func hasDrivePrefix(value string) bool {
	return len(value) >= 2 && ((value[0] >= 'a' && value[0] <= 'z') || (value[0] >= 'A' && value[0] <= 'Z')) && value[1] == ':'
}

func parentPath(relativePath string) string {
	parent := path.Dir(relativePath)
	if parent == "." {
		return ""
	}
	return parent
}

func joinPath(parent, name string) string {
	if parent == "" {
		return name
	}
	return parent + "/" + name
}

type versionFields struct {
	Name                  string            `json:"name"`
	Path                  string            `json:"path"`
	Kind                  fsroot.Kind       `json:"kind"`
	Size                  int64             `json:"size"`
	ModifiedUnix          int64             `json:"modifiedUnix"`
	ModifiedNanosecond    int               `json:"modifiedNanosecond"`
	ChangedUnix           int64             `json:"changedUnix"`
	ChangedNanosecond     int               `json:"changedNanosecond"`
	ChangeTimeAvailable   bool              `json:"changeTimeAvailable"`
	Mode                  uint32            `json:"mode"`
	OwnerUID              uint32            `json:"ownerUid"`
	GroupGID              uint32            `json:"groupGid"`
	OwnershipAvailable    bool              `json:"ownershipAvailable"`
	Symlink               bool              `json:"symlink"`
	LinkStatus            fsroot.LinkStatus `json:"linkStatus"`
	LinkTarget            string            `json:"linkTarget"`
	MountUnavailable      bool              `json:"mountUnavailable"`
	ReadOnly              bool              `json:"readOnly"`
	Device                uint64            `json:"device"`
	Inode                 uint64            `json:"inode"`
	Links                 uint64            `json:"links"`
	IdentityAvailable     bool              `json:"identityAvailable"`
	LinkDevice            uint64            `json:"linkDevice"`
	LinkInode             uint64            `json:"linkInode"`
	LinkLinks             uint64            `json:"linkLinks"`
	LinkIdentityAvailable bool              `json:"linkIdentityAvailable"`
}

// versionForEntry 用条目状态生成不可读的并发校验令牌。
func versionForEntry(entry fsroot.Entry) string {
	fields := versionFields{
		Name:                  entry.Name,
		Path:                  entry.Path,
		Kind:                  entry.Kind,
		Size:                  entry.Size,
		ModifiedUnix:          entry.ModifiedAt.Unix(),
		ModifiedNanosecond:    entry.ModifiedAt.Nanosecond(),
		ChangedUnix:           entry.ChangedAt.Unix(),
		ChangedNanosecond:     entry.ChangedAt.Nanosecond(),
		ChangeTimeAvailable:   entry.ChangeTimeAvailable,
		Mode:                  uint32(entry.Mode),
		OwnerUID:              entry.OwnerUID,
		GroupGID:              entry.GroupGID,
		OwnershipAvailable:    entry.OwnershipAvailable,
		Symlink:               entry.Symlink,
		LinkStatus:            entry.LinkStatus,
		LinkTarget:            entry.LinkTarget,
		MountUnavailable:      entry.MountUnavailable,
		ReadOnly:              entry.ReadOnly,
		Device:                entry.Device,
		Inode:                 entry.Inode,
		Links:                 entry.Links,
		IdentityAvailable:     entry.IdentityAvailable,
		LinkDevice:            entry.LinkDevice,
		LinkInode:             entry.LinkInode,
		LinkLinks:             entry.LinkLinks,
		LinkIdentityAvailable: entry.LinkIdentityAvailable,
	}
	serialized, _ := json.Marshal(fields)
	digest := sha256.Sum256(serialized)
	return "v1." + base64.RawURLEncoding.EncodeToString(digest[:])
}

func writeFileError(w http.ResponseWriter, err error) {
	var recovery *fsroot.WriteRecoveryError
	if errors.As(err, &recovery) {
		writeError(w, http.StatusConflict, fmt.Sprintf("文件状态冲突；待核查文件保留在管理目录内的 %q。", recovery.Path))
		return
	}
	switch {
	case errors.Is(err, syscall.EROFS):
		writeError(w, http.StatusForbidden, "管理卷为只读状态，无法修改文件")
	case errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EDQUOT):
		writeError(w, http.StatusInsufficientStorage, "管理卷空间不足，无法完成文件操作")
	case errors.Is(err, fsroot.ErrVolumeUnavailable):
		writeError(w, http.StatusServiceUnavailable, "管理卷暂不可用，请确认磁盘已挂载后重试")
	case errors.Is(err, fs.ErrNotExist):
		writeError(w, http.StatusNotFound, "file or directory not found")
	case errors.Is(err, fs.ErrPermission):
		writeError(w, http.StatusForbidden, "file access is not permitted")
	case errors.Is(err, fs.ErrInvalid), errors.Is(err, fsroot.ErrInvalidPath), errors.Is(err, fsroot.ErrOutsideRoot), errors.Is(err, fsroot.ErrDanglingLink), errors.Is(err, fsroot.ErrLinkCycle), errors.Is(err, fsroot.ErrNotDirectory), errors.Is(err, fsroot.ErrNotRegularFile):
		writeError(w, http.StatusBadRequest, "invalid file path")
	case errors.Is(err, fsroot.ErrSymlinkPath):
		writeError(w, http.StatusBadRequest, "write through symbolic links is not supported; use the resolved path")
	case errors.Is(err, fs.ErrExist), errors.Is(err, fsroot.ErrConflict), errors.Is(err, fsroot.ErrHardLinked), errors.Is(err, fsroot.ErrIdentityUnavailable):
		writeError(w, http.StatusConflict, "file state changed or cannot be safely modified")
	case errors.Is(err, fsroot.ErrNoReplaceUnsupported), errors.Is(err, fsroot.ErrAtomicWriteUnsupported), errors.Is(err, fsroot.ErrWalkUnsupported), errors.Is(err, fsroot.ErrMetadataUnsupported):
		writeError(w, http.StatusNotImplemented, "this filesystem operation is not supported on this platform or volume")
	case errors.Is(err, fsroot.ErrMetadataNeedsReview):
		writeError(w, http.StatusConflict, "metadata may have changed; refresh the entry and verify its current values")
	case errors.Is(err, fsroot.ErrPathChanged):
		writeError(w, http.StatusConflict, "file changed while it was being opened")
	case errors.Is(err, fsroot.ErrTreeCopyUnsafeEntry):
		writeError(w, http.StatusBadRequest, "directory operations do not support symbolic links or special files")
	case errors.Is(err, fsroot.ErrTreeCopyIntoSource):
		writeError(w, http.StatusBadRequest, "destination cannot be inside the source directory")
	case errors.Is(err, fsroot.ErrNestedMount):
		writeError(w, http.StatusBadRequest, "directory operations do not support nested mount points")
	case errors.Is(err, fsroot.ErrMountRoot):
		writeError(w, http.StatusBadRequest, "a filesystem mount root cannot be moved")
	default:
		writeError(w, http.StatusInternalServerError, "file operation failed")
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		return
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN") // PDF 阅读器可同源嵌入，跨源仍拒绝。
		next.ServeHTTP(w, r)
	})
}
