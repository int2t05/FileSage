//go:build linux

// 本文件验证 Agent 普通文件移动的审批、冲突和恢复状态。
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"filesage/internal/fsroot"
	"filesage/internal/store"
)

func TestMoveFileRequiresApprovalAndMovesWithinOneMount(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootPath, "source"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rootPath, "destination"), 0o755); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(rootPath, "source", "report.txt")
	targetPath := filepath.Join(rootPath, "destination", "report.txt")
	if err := os.WriteFile(sourcePath, []byte("approved move"), 0o640); err != nil {
		t.Fatal(err)
	}
	metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()

	plan := proposeFileMove(t, ctx, service, "source/report.txt", "destination/report.txt")
	if plan.Operations[0].Reason != "按用户确认的目标路径移动文件" {
		t.Fatalf("move reason = %q", plan.Operations[0].Reason)
	}
	var parentSnapshot fsroot.Fingerprint
	if err := json.Unmarshal([]byte(plan.Operations[0].TargetStateJSON), &parentSnapshot); err != nil || parentSnapshot.Kind != fsroot.KindDirectory {
		t.Fatalf("target parent snapshot = (%#v, %v)", parentSnapshot, err)
	}
	if _, err := service.Execute(ctx, plan.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("unapproved Execute() error = %v, want ErrConflict", err)
	}
	if _, err := service.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(targetPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("approval moved the file: %v", err)
	}
	completed, err := service.Execute(ctx, plan.ID)
	if err != nil || completed.Status != store.PlanCompleted || completed.Operations[0].Status != store.OperationSucceeded {
		t.Fatalf("Execute() = (%#v, %v)", completed, err)
	}
	if completed.Operations[0].ResultMessage != "文件已移动" {
		t.Fatalf("move result message = %q, want cross-directory move wording", completed.Operations[0].ResultMessage)
	}
	if _, err := os.Stat(sourcePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("move kept source: %v", err)
	}
	if content, err := os.ReadFile(targetPath); err != nil || string(content) != "approved move" {
		t.Fatalf("moved content = (%q, %v)", content, err)
	}
}

func TestMoveFileWithinDirectoryRecordsRenameWording(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	parent := filepath.Join(rootPath, "documents")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "draft.txt"), []byte("approved rename"), 0o640); err != nil {
		t.Fatal(err)
	}
	metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()

	plan := proposeFileMove(t, ctx, service, "documents/draft.txt", "documents/final.txt")
	if plan.Operations[0].Reason != "按用户确认的目标路径重命名文件" {
		t.Fatalf("rename reason = %q", plan.Operations[0].Reason)
	}
	if _, err := service.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve"); err != nil {
		t.Fatal(err)
	}
	completed, err := service.Execute(ctx, plan.ID)
	if err != nil || completed.Status != store.PlanCompleted || completed.Operations[0].Status != store.OperationSucceeded {
		t.Fatalf("Execute() = (%#v, %v)", completed, err)
	}
	if completed.Operations[0].ResultMessage != "文件已重命名" {
		t.Fatalf("rename result message = %q", completed.Operations[0].ResultMessage)
	}
	events, err := metadata.AuditForTask(ctx, plan.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.EventType == "operation_finished" && event.OperationID == plan.Operations[0].ID {
			if event.Message != "文件已重命名" {
				t.Fatalf("rename audit message = %q", event.Message)
			}
			return
		}
	}
	t.Fatal("rename operation completion audit not found")
}

func TestMoveFileAcrossMounts(t *testing.T) {
	sourceDir, err := os.MkdirTemp("/tmp", "filesage-agent-move-source-")
	if err != nil {
		t.Skipf("temporary source mount unavailable: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sourceDir) })
	targetDir, err := os.MkdirTemp("/dev/shm", "filesage-agent-move-target-")
	if err != nil {
		t.Skipf("temporary destination mount unavailable: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(targetDir) })
	sourcePath := filepath.Join(sourceDir, "source.bin")
	targetPath := filepath.Join(targetDir, "moved.bin")
	if err := os.WriteFile(sourcePath, []byte(strings.Repeat("cross-mount payload ", 2048)), 0o640); err != nil {
		t.Fatal(err)
	}
	inspectionRoot, err := fsroot.Open("/")
	if err != nil {
		if errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM) {
			t.Skipf("host denies inspection of an unrelated system mount: %v", err)
		}
		t.Fatal(err)
	}
	_ = inspectionRoot.Close()
	metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), "/")
	defer root.Close()
	defer metadata.Close()
	sourceRelative := strings.TrimPrefix(filepath.ToSlash(sourcePath), "/")
	targetRelative := strings.TrimPrefix(filepath.ToSlash(targetPath), "/")
	sourceMount, err := root.MountID(sourceRelative)
	if err != nil {
		t.Skipf("source mount identity unavailable: %v", err)
	}
	targetMount, err := root.MountID(strings.TrimPrefix(filepath.ToSlash(targetDir), "/"))
	if err != nil {
		t.Skipf("destination mount identity unavailable: %v", err)
	}
	if sourceMount == targetMount {
		t.Skip("/tmp and /dev/shm share a mount in this environment")
	}

	plan := proposeFileMove(t, context.Background(), service, sourceRelative, targetRelative)
	if _, err := service.Decide(context.Background(), plan.ID, plan.Operations[0].ID, "approve"); err != nil {
		t.Fatal(err)
	}
	completed, err := service.Execute(context.Background(), plan.ID)
	if errors.Is(err, fsroot.ErrAtomicWriteUnsupported) {
		t.Skipf("destination mount does not support safe file publication: %v", err)
	}
	if err != nil || completed.Operations[0].Status != store.OperationSucceeded {
		t.Fatalf("cross-mount Execute() = (%#v, %v)", completed, err)
	}
	if _, err := os.Stat(sourcePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cross-mount move kept source: %v", err)
	}
	if content, err := os.ReadFile(targetPath); err != nil || string(content) != strings.Repeat("cross-mount payload ", 2048) {
		t.Fatalf("cross-mount content = (%d bytes, %v)", len(content), err)
	}
}

func TestMoveFileRejectsExternalSourceChangeAfterApproval(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootPath, "source"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rootPath, "destination"), 0o755); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(rootPath, "source", "file.txt")
	targetPath := filepath.Join(rootPath, "destination", "file.txt")
	if err := os.WriteFile(sourcePath, []byte("approved source content"), 0o640); err != nil {
		t.Fatal(err)
	}
	metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()
	assertMoveRejectsChangedSourceAfterApproval(t, ctx, service, metadata, sourcePath, targetPath, "source/file.txt", "destination/file.txt")
}

// TestMoveFileRejectsOpenHandleChangeAfterApproval 验证原地改写会使移动审批失效。
func TestMoveFileRejectsOpenHandleChangeAfterApproval(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootPath, "source"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rootPath, "destination"), 0o755); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(rootPath, "source", "file.txt")
	targetPath := filepath.Join(rootPath, "destination", "file.txt")
	if err := os.WriteFile(sourcePath, []byte("approved source content"), 0o640); err != nil {
		t.Fatal(err)
	}
	metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()
	plan, err := service.Propose(ctx, moveFileProposal("source/file.txt", "destination/file.txt"))
	if err != nil {
		t.Fatalf("Propose() = %v", err)
	}
	if _, err := service.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve"); err != nil {
		t.Fatalf("approve move plan: %v", err)
	}
	approvedInfo, err := os.Stat(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	approvedEntry, err := root.LstatEntry("source/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	handle, externalData := rewriteThroughOpenHandlePreservingIdentityAndMtime(t, sourcePath, approvedInfo)
	defer handle.Close()
	currentEntry, err := root.LstatEntry("source/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !currentEntry.ChangeTimeAvailable || currentEntry.ChangedAt.Equal(approvedEntry.ChangedAt) {
		t.Fatalf("test did not change ctime after preserving visible file metadata: before=%#v after=%#v", approvedEntry, currentEntry)
	}

	updated, err := service.Execute(ctx, plan.ID)
	if !errors.Is(err, ErrConflict) || updated.Status != store.PlanInvalidated || updated.Operations[0].Approval != store.ApprovalInvalidated || updated.Operations[0].Status != store.OperationSkipped {
		t.Fatalf("move after open-handle change = (%#v, %v), want invalidated conflict", updated, err)
	}
	if content, err := os.ReadFile(sourcePath); err != nil || string(content) != externalData {
		t.Fatalf("external source content changed: %q (%v)", content, err)
	}
	if _, err := os.Stat(targetPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("changed source was published to target: %v", err)
	}
	events, err := metadata.AuditForTask(ctx, plan.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	var staleAudit bool
	for _, event := range events {
		if event.PlanID == plan.ID && event.OperationID == plan.Operations[0].ID && event.EventType == "operation_invalidated" && event.Outcome == "stale" {
			staleAudit = true
			break
		}
	}
	if !staleAudit {
		t.Fatalf("open-handle conflict has no stale audit record: %#v", events)
	}
}

// TestRenameRejectsExternalProcessChangeBetweenDigestAndCommit 验证提交前的进程外改写会使重命名冲突。
func TestRenameRejectsExternalProcessChangeBetweenDigestAndCommit(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	parent := filepath.Join(rootPath, "documents")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(parent, "draft.txt")
	targetPath := filepath.Join(parent, "final.txt")
	if err := os.WriteFile(sourcePath, []byte("approved rename"), 0o640); err != nil {
		t.Fatal(err)
	}
	metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()
	plan, err := service.Propose(ctx, Proposal{
		RequestText: "重命名文件", ScopePath: "documents", Action: "rename",
		SourcePath: "documents/draft.txt", TargetPath: "documents/final.txt", Reason: "按用户确认的名称修改",
	})
	if err != nil {
		t.Fatalf("Propose() = %v", err)
	}
	approved, err := service.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve", plan.Version)
	if err != nil {
		t.Fatalf("approve rename: %v", err)
	}
	if err := metadata.ClaimAgentPlanExecution(ctx, plan.ID, approved.Version, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := metadata.StartAgentOperation(ctx, plan.ID, plan.Operations[0].ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	approvedInfo, err := os.Stat(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	// 第三次取消检查位于 Linux 同卷移动已校验打开文件描述符之后、原子改名前。
	externalData := strings.Repeat("x", int(approvedInfo.Size()))
	mutation := &externalWriterMutationContext{Context: ctx, path: sourcePath, triggerAt: 3, done: make(chan struct{})}
	tool := agentTools()["rename"]
	updated, err := tool.execute(service, mutation, plan.ID, approved.ScopePath, approved.Operations[0])
	if mutation.triggeredAt.Load() != mutation.triggerAt {
		t.Fatalf("race setup was not applied at commit boundary: call=%d", mutation.triggeredAt.Load())
	}
	select {
	case <-mutation.done:
	case <-time.After(5 * time.Second):
		t.Fatal("external writer did not finish")
	}
	triggered, setupErr := mutation.result()
	if !triggered || setupErr != nil {
		t.Fatalf("external writer setup failed: triggered=%t err=%v", triggered, setupErr)
	}
	if !errors.Is(err, fsroot.ErrConflict) || updated.Status != store.PlanFailed || updated.Operations[0].Status != store.OperationFailed {
		t.Fatalf("rename after external process change = (%#v, %v), want failed conflict", updated, err)
	}
	if content, err := os.ReadFile(sourcePath); err != nil || string(content) != externalData {
		t.Fatalf("external source content changed: %q (%v)", content, err)
	}
	currentInfo, err := os.Stat(sourcePath)
	if err != nil || !os.SameFile(approvedInfo, currentInfo) {
		t.Fatalf("changed source was not restored to its original directory entry: info=%#v err=%v", currentInfo, err)
	}
	if _, err := os.Stat(targetPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale source was renamed: %v", err)
	}
	final, err := service.Plan(ctx, plan.ID)
	if err != nil || final.Status != store.PlanFailed {
		t.Fatalf("failed rename plan = (%#v, %v)", final, err)
	}
}

// externalWriterMutationContext 在指定提交检查处启动独立进程改写源文件。
type externalWriterMutationContext struct {
	context.Context
	path        string
	triggerAt   int32
	calls       atomic.Int32
	triggeredAt atomic.Int32
	once        sync.Once
	mu          sync.Mutex
	triggered   bool
	err         error
	done        chan struct{}
}

// Err 在指定检查处启动独立写入进程，计数需支持 database/sql 并发调用。
func (ctx *externalWriterMutationContext) Err() error {
	call := ctx.calls.Add(1)
	if call == ctx.triggerAt {
		ctx.triggeredAt.Store(call)
		ctx.once.Do(func() {
			defer close(ctx.done)
			processContext, cancel := context.WithTimeout(ctx.Context, 10*time.Second)
			defer cancel()
			command := exec.CommandContext(processContext, os.Args[0], "-test.run=^TestExternalWriterProcessHelper$")
			command.Env = externalWriterHelperEnvironment(ctx.path)
			var setupErr error
			if output, err := command.CombinedOutput(); err != nil {
				setupErr = fmt.Errorf("external writer process: %w: %s", err, strings.TrimSpace(string(output)))
			}
			ctx.mu.Lock()
			ctx.triggered = true
			ctx.err = setupErr
			ctx.mu.Unlock()
		})
	}
	return ctx.Context.Err()
}

// result 在子进程完成后安全读取测试结果。
func (ctx *externalWriterMutationContext) result() (bool, error) {
	ctx.mu.Lock()
	defer ctx.mu.Unlock()
	return ctx.triggered, ctx.err
}

// TestExternalWriterProcessHelper 由竞态测试子进程调用，模拟应用锁之外的写入者。
func TestExternalWriterProcessHelper(t *testing.T) {
	if os.Getenv("FILESAGE_TEST_EXTERNAL_WRITER_HELPER") != "1" {
		return
	}
	path := os.Getenv("FILESAGE_TEST_EXTERNAL_WRITER_PATH")
	if path == "" {
		t.Fatal("external writer path is required")
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	opened, err := handle.Stat()
	if err != nil || !os.SameFile(before, opened) {
		t.Fatalf("external writer opened a different file: info=%v err=%v", opened, err)
	}
	data := strings.Repeat("x", int(before.Size()))
	if _, err := handle.WriteAt([]byte(data), 0); err != nil {
		t.Fatal(err)
	}
	if err := handle.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
}

// externalWriterHelperEnvironment 仅向辅助进程传递本次测试参数。
func externalWriterHelperEnvironment(path string) []string {
	const prefix = "FILESAGE_TEST_EXTERNAL_WRITER_"
	environment := make([]string, 0, len(os.Environ())+2)
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, prefix) {
			environment = append(environment, value)
		}
	}
	return append(environment,
		"FILESAGE_TEST_EXTERNAL_WRITER_HELPER=1",
		"FILESAGE_TEST_EXTERNAL_WRITER_PATH="+path,
	)
}

// rewriteThroughOpenHandlePreservingIdentityAndMtime 原地改写并保留文件身份、大小和修改时间。
func rewriteThroughOpenHandlePreservingIdentityAndMtime(t *testing.T, path string, approved os.FileInfo) (*os.File, string) {
	t.Helper()
	handle, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	externalData := strings.Repeat("x", int(approved.Size()))
	if _, err := handle.WriteAt([]byte(externalData), 0); err != nil {
		_ = handle.Close()
		t.Fatal(err)
	}
	if err := handle.Sync(); err != nil {
		_ = handle.Close()
		t.Fatal(err)
	}
	if err := os.Chtimes(path, approved.ModTime(), approved.ModTime()); err != nil {
		_ = handle.Close()
		t.Fatal(err)
	}
	current, err := os.Stat(path)
	if err != nil {
		_ = handle.Close()
		t.Fatal(err)
	}
	if !os.SameFile(approved, current) || current.Size() != approved.Size() || !current.ModTime().Equal(approved.ModTime()) {
		_ = handle.Close()
		t.Fatalf("test did not preserve file identity, size and modification time: before=%#v after=%#v", approved, current)
	}
	return handle, externalData
}

func assertMoveRejectsChangedSourceAfterApproval(t *testing.T, ctx context.Context, service *Service, metadata *store.Store, sourcePath, targetPath, sourceRelative, targetRelative string) {
	t.Helper()
	plan, err := service.Propose(ctx, moveFileProposal(sourceRelative, targetRelative))
	if err != nil {
		t.Fatalf("Propose() = %v", err)
	}
	if _, err := service.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve"); err != nil {
		t.Fatalf("approve move plan: %v", err)
	}
	approvedInfo, err := os.Stat(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	externalData := strings.Repeat("x", int(approvedInfo.Size()))
	if err := os.WriteFile(sourcePath, []byte(externalData), approvedInfo.Mode().Perm()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(sourcePath, approvedInfo.ModTime(), approvedInfo.ModTime()); err != nil {
		t.Fatal(err)
	}
	updated, err := service.Execute(ctx, plan.ID)
	if !errors.Is(err, ErrConflict) || updated.Status != store.PlanInvalidated || updated.Operations[0].Approval != store.ApprovalInvalidated || updated.Operations[0].Status != store.OperationSkipped {
		t.Fatalf("move after source change = (%#v, %v), want invalidated conflict", updated, err)
	}
	if content, err := os.ReadFile(sourcePath); err != nil || string(content) != externalData {
		t.Fatalf("external source content changed: %q (%v)", content, err)
	}
	if _, err := os.Stat(targetPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("changed source was published to target: %v", err)
	}
	events, err := metadata.AuditForTask(ctx, plan.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	invalidationAudit := false
	for _, event := range events {
		if event.OperationID == plan.Operations[0].ID && event.EventType == "operation_invalidated" && event.Outcome == "stale" {
			invalidationAudit = true
		}
	}
	if !invalidationAudit {
		t.Fatalf("changed source invalidation was not audited: %#v", events)
	}
}

func TestMoveFileRejectsTargetAndParentConflicts(t *testing.T) {
	ctx := context.Background()
	t.Run("occupied target at proposal", func(t *testing.T) {
		rootPath := t.TempDir()
		if err := os.MkdirAll(filepath.Join(rootPath, "source"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(rootPath, "destination"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(rootPath, "source", "file.txt"), []byte("source"), 0o600); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(rootPath, "destination", "file.txt")
		if err := os.WriteFile(target, []byte("preserve"), 0o600); err != nil {
			t.Fatal(err)
		}
		metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
		defer root.Close()
		defer metadata.Close()
		if _, err := service.Propose(ctx, moveFileProposal("source/file.txt", "destination/file.txt")); !errors.Is(err, ErrConflict) {
			t.Fatalf("Propose() error = %v, want ErrConflict", err)
		}
		content, err := os.ReadFile(target)
		if err != nil || string(content) != "preserve" {
			t.Fatalf("occupied target changed: %q (%v)", content, err)
		}
	})

	t.Run("missing or unsafe parent", func(t *testing.T) {
		rootPath := t.TempDir()
		if err := os.Mkdir(filepath.Join(rootPath, "real"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(rootPath, "real"), filepath.Join(rootPath, "link")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(rootPath, "source.txt"), []byte("source"), 0o600); err != nil {
			t.Fatal(err)
		}
		metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
		defer root.Close()
		defer metadata.Close()
		for _, target := range []string{"missing/file.txt", "link/file.txt"} {
			if _, err := service.Propose(ctx, moveFileProposal("source.txt", target)); err == nil {
				t.Fatalf("Propose() with target %q succeeded, want rejection", target)
			}
		}
	})

	t.Run("parent identity changes before approval", func(t *testing.T) {
		rootPath := t.TempDir()
		if err := os.MkdirAll(filepath.Join(rootPath, "source"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(rootPath, "destination"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(rootPath, "source", "file.txt"), []byte("source"), 0o600); err != nil {
			t.Fatal(err)
		}
		metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
		defer root.Close()
		defer metadata.Close()
		plan := proposeFileMove(t, ctx, service, "source/file.txt", "destination/file.txt")
		if err := os.Rename(filepath.Join(rootPath, "destination"), filepath.Join(rootPath, "old-destination")); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(rootPath, "destination"), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := service.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve"); !errors.Is(err, ErrConflict) {
			t.Fatalf("Decide() error = %v, want ErrConflict", err)
		}
		if _, err := os.Stat(filepath.Join(rootPath, "source", "file.txt")); err != nil {
			t.Fatalf("conflict moved source: %v", err)
		}
	})

	t.Run("parent identity changes after approval", func(t *testing.T) {
		rootPath := t.TempDir()
		if err := os.MkdirAll(filepath.Join(rootPath, "source"), 0o755); err != nil {
			t.Fatal(err)
		}
		destination := filepath.Join(rootPath, "destination")
		if err := os.Mkdir(destination, 0o755); err != nil {
			t.Fatal(err)
		}
		source := filepath.Join(rootPath, "source", "file.txt")
		if err := os.WriteFile(source, []byte("source"), 0o600); err != nil {
			t.Fatal(err)
		}
		metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
		defer root.Close()
		defer metadata.Close()
		plan := proposeFileMove(t, ctx, service, "source/file.txt", "destination/file.txt")
		if _, err := service.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve"); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(destination, filepath.Join(rootPath, "old-destination")); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(destination, 0o755); err != nil {
			t.Fatal(err)
		}
		updated, err := service.Execute(ctx, plan.ID)
		if !errors.Is(err, ErrConflict) || updated.Status != store.PlanInvalidated {
			t.Fatalf("Execute() = (%#v, %v), want invalidated conflict", updated, err)
		}
		if _, err := os.Stat(source); err != nil {
			t.Fatalf("parent conflict moved source: %v", err)
		}
		if _, err := os.Stat(filepath.Join(destination, "file.txt")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("parent conflict created target: %v", err)
		}
	})

	t.Run("target appears after approval", func(t *testing.T) {
		rootPath := t.TempDir()
		if err := os.MkdirAll(filepath.Join(rootPath, "source"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(rootPath, "destination"), 0o755); err != nil {
			t.Fatal(err)
		}
		source := filepath.Join(rootPath, "source", "file.txt")
		if err := os.WriteFile(source, []byte("source"), 0o600); err != nil {
			t.Fatal(err)
		}
		metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
		defer root.Close()
		defer metadata.Close()
		plan := proposeFileMove(t, ctx, service, "source/file.txt", "destination/file.txt")
		if _, err := service.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve"); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(rootPath, "destination", "file.txt")
		if err := os.WriteFile(target, []byte("preserve"), 0o600); err != nil {
			t.Fatal(err)
		}
		updated, err := service.Execute(ctx, plan.ID)
		if !errors.Is(err, ErrConflict) || updated.Status != store.PlanInvalidated {
			t.Fatalf("Execute() = (%#v, %v), want invalidated conflict", updated, err)
		}
		if content, err := os.ReadFile(target); err != nil || string(content) != "preserve" {
			t.Fatalf("target was overwritten: %q (%v)", content, err)
		}
		if _, err := os.Stat(source); err != nil {
			t.Fatalf("source was moved after conflict: %v", err)
		}
	})
}

func TestMoveFileRecordsPublishedButUnremovedAsPartial(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootPath, "source"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rootPath, "destination"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "source", "file.txt"), []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()
	plan := proposeFileMove(t, ctx, service, "source/file.txt", "destination/file.txt")
	if _, err := service.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve"); err != nil {
		t.Fatal(err)
	}
	if err := metadata.StartAgentOperation(ctx, plan.ID, plan.Operations[0].ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	retainedPath := "source/.filesage-move-quarantine-test/source"
	updated, err := service.recordMoveFileOutcome(ctx, plan.ID, plan.Operations[0], fsroot.FileMoveResult{Published: true, SourceRetained: retainedPath}, "", errors.New("source removal conflict"))
	if err == nil || updated.Operations[0].Status != store.OperationPartial || !strings.Contains(updated.Operations[0].ResultMessage, retainedPath) {
		t.Fatalf("partial result = (%#v, %v)", updated, err)
	}
	tasks, err := metadata.ListTasks(ctx, store.TaskListQuery{Limit: 10})
	if err != nil || len(tasks.Tasks) != 1 || tasks.Tasks[0].Status != store.TaskPartial {
		t.Fatalf("partial task state = (%#v, %v)", tasks.Tasks, err)
	}
}

func TestMoveFileRejectsChangedTargetContent(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootPath, "source"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rootPath, "destination"), 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(rootPath, "source", "file.txt")
	target := filepath.Join(rootPath, "destination", "file.txt")
	if err := os.WriteFile(source, []byte("approved"), 0o600); err != nil {
		t.Fatal(err)
	}
	sourceInfo, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()
	plan := proposeFileMove(t, ctx, service, "source/file.txt", "destination/file.txt")
	op := plan.Operations[0]
	if _, err := service.Decide(ctx, plan.ID, op.ID, "approve", plan.Version); err != nil {
		t.Fatal(err)
	}
	if err := metadata.StartAgentOperation(ctx, plan.ID, op.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	digest, err := service.hashApprovedFile(op)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(source, target); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(target, sourceInfo.ModTime(), sourceInfo.ModTime()); err != nil {
		t.Fatal(err)
	}
	updated, err := service.recordMoveFileOutcome(ctx, plan.ID, op, fsroot.FileMoveResult{Published: true, SourceRemoved: true}, digest, nil)
	if !errors.Is(err, ErrConflict) || updated.Status != store.PlanFailed || updated.Operations[0].Status != store.OperationPartial {
		t.Fatalf("tampered move result = (%#v, %v), want manual review", updated, err)
	}
}

func TestCrossVolumeMoveRejectsChangedTargetPermissions(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootPath, "source"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rootPath, "destination"), 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(rootPath, "source", "file.txt")
	target := filepath.Join(rootPath, "destination", "file.txt")
	if err := os.WriteFile(source, []byte("approved"), 0o640); err != nil {
		t.Fatal(err)
	}
	metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()
	plan := proposeFileMove(t, ctx, service, "source/file.txt", "destination/file.txt")
	op := plan.Operations[0]
	if _, err := service.Decide(ctx, plan.ID, op.ID, "approve", plan.Version); err != nil {
		t.Fatal(err)
	}
	if err := metadata.StartAgentOperation(ctx, plan.ID, op.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	digest, err := service.hashApprovedFile(op)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("approved"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	updated, err := service.recordMoveFileOutcome(ctx, plan.ID, op, fsroot.FileMoveResult{
		Published: true, SourceRemoved: true, SHA256: digest,
	}, digest, nil)
	if !errors.Is(err, ErrConflict) || updated.Status != store.PlanFailed || updated.Operations[0].Status != store.OperationPartial {
		t.Fatalf("changed target permissions = (%#v, %v), want manual review", updated, err)
	}
}

func TestMoveFileRecoveryDoesNotReplayAndRequiresCertainPreconditions(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name          string
		sourcePath    string
		targetPath    string
		changeTarget  bool
		wantOperation store.OperationStatus
		wantMessage   string
	}{
		{name: "source unchanged and target empty", sourcePath: "source/file.txt", targetPath: "destination/file.txt", wantOperation: store.OperationPending},
		{name: "move target present is uncertain", sourcePath: "source/file.txt", targetPath: "destination/file.txt", changeTarget: true, wantOperation: store.OperationPartial, wantMessage: "重启后无法证明文件移动结果，源与目标需人工核查"},
		{name: "rename target present is uncertain", sourcePath: "documents/draft.txt", targetPath: "documents/final.txt", changeTarget: true, wantOperation: store.OperationPartial, wantMessage: "重启后无法证明文件重命名结果，源与目标需人工核查"},
	} {
		t.Run(test.name, func(t *testing.T) {
			rootPath := t.TempDir()
			if err := os.MkdirAll(filepath.Dir(filepath.Join(rootPath, test.sourcePath)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(filepath.Join(rootPath, test.targetPath)), 0o755); err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(rootPath, filepath.FromSlash(test.sourcePath))
			target := filepath.Join(rootPath, filepath.FromSlash(test.targetPath))
			if err := os.WriteFile(source, []byte("source"), 0o600); err != nil {
				t.Fatal(err)
			}
			metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
			defer root.Close()
			defer metadata.Close()
			plan := proposeFileMove(t, ctx, service, test.sourcePath, test.targetPath)
			if _, err := service.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve"); err != nil {
				t.Fatal(err)
			}
			if err := metadata.StartAgentOperation(ctx, plan.ID, plan.Operations[0].ID, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			if test.changeTarget {
				if err := os.WriteFile(target, []byte("uncertain"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := service.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			recovered, err := service.Plan(ctx, plan.ID)
			if err != nil || recovered.Operations[0].Status != test.wantOperation {
				t.Fatalf("recovered plan = (%#v, %v), want operation %q", recovered, err, test.wantOperation)
			}
			if test.wantMessage != "" && recovered.Operations[0].ResultMessage != test.wantMessage {
				t.Fatalf("recovery message = %q, want %q", recovered.Operations[0].ResultMessage, test.wantMessage)
			}
			if _, err := os.Stat(source); err != nil {
				t.Fatalf("recovery moved or removed source: %v", err)
			}
			if test.changeTarget {
				if content, err := os.ReadFile(target); err != nil || string(content) != "uncertain" {
					t.Fatalf("recovery changed target: %q (%v)", content, err)
				}
			} else if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("recovery replayed the move: %v", err)
			}
		})
	}
}

func proposeFileMove(t *testing.T, ctx context.Context, service *Service, source, target string) store.PlanRecord {
	t.Helper()
	plan, err := service.Propose(ctx, moveFileProposal(source, target))
	if err != nil {
		t.Fatalf("Propose() error = %v", err)
	}
	return plan
}

func moveFileProposal(source, target string) Proposal {
	return Proposal{
		RequestText: "将文件移动到目标文件夹", ScopePath: "", Action: "move_file",
		SourcePath: source, TargetPath: target, Reason: fileMoveReason(source, target),
	}
}
