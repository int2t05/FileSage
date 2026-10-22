//go:build linux

// 本文件验证 Agent 文件与目录复制的审批闭环。
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filesage/internal/fsroot"
	"filesage/internal/store"
	"golang.org/x/sys/unix"
)

func TestCopyFileRequiresApprovalAndPreservesSource(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootPath, "destination"), 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(rootPath, "report.txt")
	if err := os.WriteFile(source, []byte("approved copy"), 0o640); err != nil {
		t.Fatal(err)
	}
	metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()

	plan, err := service.Propose(ctx, copyFileProposal("report.txt", "destination/report.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var targetParent fsroot.Fingerprint
	if err := json.Unmarshal([]byte(plan.Operations[0].TargetStateJSON), &targetParent); err != nil || targetParent.Kind != fsroot.KindDirectory {
		t.Fatalf("target parent snapshot = (%#v, %v)", targetParent, err)
	}
	if _, err := service.Execute(ctx, plan.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("unapproved Execute() error = %v, want ErrConflict", err)
	}
	if _, err := os.Stat(filepath.Join(rootPath, "destination", "report.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unapproved copy created target: %v", err)
	}
	if _, err := service.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve"); err != nil {
		t.Fatal(err)
	}
	completed, err := service.Execute(ctx, plan.ID)
	if err != nil || completed.Status != store.PlanCompleted || completed.Operations[0].Status != store.OperationSucceeded {
		t.Fatalf("Execute() = (%#v, %v)", completed, err)
	}
	for _, file := range []string{source, filepath.Join(rootPath, "destination", "report.txt")} {
		content, readErr := os.ReadFile(file)
		if readErr != nil || string(content) != "approved copy" {
			t.Fatalf("copy content at %q = (%q, %v)", file, content, readErr)
		}
	}
	if !strings.Contains(completed.Operations[0].ResultMessage, "内容校验") {
		t.Fatalf("copy result message = %q", completed.Operations[0].ResultMessage)
	}
}

func TestCopyFileRejectsChangedSourceAndOccupiedTarget(t *testing.T) {
	ctx := context.Background()
	t.Run("changed source invalidates approval", func(t *testing.T) {
		rootPath := t.TempDir()
		if err := os.Mkdir(filepath.Join(rootPath, "destination"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(rootPath, "source.txt"), []byte("before"), 0o600); err != nil {
			t.Fatal(err)
		}
		metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
		defer root.Close()
		defer metadata.Close()
		plan, err := service.Propose(ctx, copyFileProposal("source.txt", "destination/copy.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(rootPath, "source.txt"), []byte("changed"), 0o600); err != nil {
			t.Fatal(err)
		}
		updated, err := service.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve")
		if !errors.Is(err, ErrConflict) || updated.Operations[0].Approval != store.ApprovalInvalidated {
			t.Fatalf("Decide() = (%#v, %v), want invalidated conflict", updated, err)
		}
		if _, err := os.Stat(filepath.Join(rootPath, "destination", "copy.txt")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stale source was copied: %v", err)
		}
	})

	t.Run("open handle after approval invalidates execution", func(t *testing.T) {
		rootPath := t.TempDir()
		if err := os.Mkdir(filepath.Join(rootPath, "destination"), 0o755); err != nil {
			t.Fatal(err)
		}
		sourcePath := filepath.Join(rootPath, "source.txt")
		if err := os.WriteFile(sourcePath, []byte("approved"), 0o600); err != nil {
			t.Fatal(err)
		}
		metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
		defer root.Close()
		defer metadata.Close()
		plan, err := service.Propose(ctx, copyFileProposal("source.txt", "destination/copy.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve"); err != nil {
			t.Fatal(err)
		}
		approvedInfo, err := os.Stat(sourcePath)
		if err != nil {
			t.Fatal(err)
		}
		approvedEntry, err := root.LstatEntry("source.txt")
		if err != nil {
			t.Fatal(err)
		}
		handle, externalData := rewriteThroughOpenHandlePreservingIdentityAndMtime(t, sourcePath, approvedInfo)
		defer handle.Close()
		currentEntry, err := root.LstatEntry("source.txt")
		if err != nil || !currentEntry.ChangeTimeAvailable || currentEntry.ChangedAt.Equal(approvedEntry.ChangedAt) {
			t.Fatalf("same-inode rewrite did not change ctime: entry=%#v err=%v", currentEntry, err)
		}

		updated, err := service.Execute(ctx, plan.ID)
		if !errors.Is(err, ErrConflict) || updated.Status != store.PlanInvalidated || updated.Operations[0].Approval != store.ApprovalInvalidated {
			t.Fatalf("copy after open-handle change = (%#v, %v), want invalidated conflict", updated, err)
		}
		if content, err := os.ReadFile(sourcePath); err != nil || string(content) != externalData {
			t.Fatalf("external source changed: content=%q err=%v", content, err)
		}
		if _, err := os.Stat(filepath.Join(rootPath, "destination", "copy.txt")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stale source was copied: %v", err)
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
	})

	t.Run("occupied target is preserved", func(t *testing.T) {
		rootPath := t.TempDir()
		if err := os.Mkdir(filepath.Join(rootPath, "destination"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(rootPath, "source.txt"), []byte("source"), 0o600); err != nil {
			t.Fatal(err)
		}
		metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
		defer root.Close()
		defer metadata.Close()
		plan, err := service.Propose(ctx, copyFileProposal("source.txt", "destination/copy.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(rootPath, "destination", "copy.txt"), []byte("preserve"), 0o600); err != nil {
			t.Fatal(err)
		}
		updated, err := service.Execute(ctx, plan.ID)
		if !errors.Is(err, ErrConflict) || updated.Status != store.PlanInvalidated {
			t.Fatalf("Execute() = (%#v, %v), want invalidated conflict", updated, err)
		}
		content, err := os.ReadFile(filepath.Join(rootPath, "destination", "copy.txt"))
		if err != nil || string(content) != "preserve" {
			t.Fatalf("occupied target = (%q, %v)", content, err)
		}
	})
}

func TestCopyDirectoryRequiresApprovalAndCopiesTree(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootPath, "source", "nested"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(rootPath, "destination"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "source", "one.txt"), []byte("one"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "source", "nested", "two.txt"), []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()

	plan, err := service.Propose(ctx, copyDirectoryProposal("source", "destination/clone"))
	if err != nil {
		t.Fatal(err)
	}
	var approved directoryCopyState
	if err := json.Unmarshal([]byte(plan.Operations[0].SourceStateJSON), &approved); err != nil || approved.RootPath != "source" || len(approved.Digest) != 64 {
		t.Fatalf("directory source snapshot = (%#v, %v)", approved, err)
	}
	if _, err := service.Execute(ctx, plan.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("unapproved Execute() error = %v, want ErrConflict", err)
	}
	if _, err := service.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve"); err != nil {
		t.Fatal(err)
	}
	completed, err := service.Execute(ctx, plan.ID)
	if err != nil || completed.Status != store.PlanCompleted || completed.Operations[0].Status != store.OperationSucceeded {
		t.Fatalf("Execute() = (%#v, %v)", completed, err)
	}
	for relative, expected := range map[string]string{
		"source/one.txt":                   "one",
		"source/nested/two.txt":            "two",
		"destination/clone/one.txt":        "one",
		"destination/clone/nested/two.txt": "two",
	} {
		content, readErr := os.ReadFile(filepath.Join(rootPath, filepath.FromSlash(relative)))
		if readErr != nil || string(content) != expected {
			t.Fatalf("file %q = (%q, %v)", relative, content, readErr)
		}
	}
	if !strings.Contains(completed.Operations[0].ResultMessage, "2 个文件") {
		t.Fatalf("copy result message = %q", completed.Operations[0].ResultMessage)
	}
}

func TestCopyDirectoryDetectsSameSizeContentChangeAfterPublish(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "source"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(rootPath, "destination"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "source", "file.txt"), []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()
	plan, err := service.Propose(ctx, copyDirectoryProposal("source", "destination/clone"))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := root.SnapshotTree(ctx, "source")
	if err != nil {
		t.Fatal(err)
	}
	targetParent, err := root.LstatEntry("destination")
	if err != nil {
		t.Fatal(err)
	}
	result, err := root.CopyTreeNoReplace(ctx, "source", "destination/clone", snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "destination", "clone", "file.txt"), []byte("tamper"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := service.verifyCopiedDirectory(ctx, plan.Operations[0], snapshot, targetParent, result); !errors.Is(err, ErrConflict) {
		t.Fatalf("verifyCopiedDirectory() error = %v, want same-size content conflict", err)
	}
}

func TestCopyDirectoryRecoveryCleansOrphanAndDoesNotReplay(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "source"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "source", "file.txt"), []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(rootPath, "destination"), 0o755); err != nil {
		t.Fatal(err)
	}
	metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()
	plan, err := service.Propose(ctx, copyDirectoryProposal("source", "destination/clone"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve"); err != nil {
		t.Fatal(err)
	}
	if err := metadata.StartAgentOperation(ctx, plan.ID, plan.Operations[0].ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	nonce := strings.Repeat("d", 24)
	stageName := ".filesage-tree-copy-" + nonce
	stagePath := filepath.Join(rootPath, "destination", stageName)
	if err := os.Mkdir(stagePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stagePath, "partial"), []byte("interrupted"), 0o600); err != nil {
		t.Fatal(err)
	}
	stat := unix.Statx_t{}
	if err := unix.Statx(unix.AT_FDCWD, stagePath, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_BASIC_STATS|unix.STATX_MNT_ID, &stat); err != nil {
		t.Fatal(err)
	}
	marker := fmt.Sprintf("filesage-tree-copy-v1\nactive\n%s\n%d\n%d\n%d\n", nonce, unix.Mkdev(stat.Dev_major, stat.Dev_minor), stat.Ino, stat.Mnt_id)
	if err := os.WriteFile(stagePath+".lock", []byte(marker), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := service.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	recovered, err := service.Plan(ctx, plan.ID)
	if err != nil || recovered.Operations[0].Status != store.OperationPending {
		t.Fatalf("recovered plan = (%#v, %v), want pending retry", recovered, err)
	}
	if _, err := os.Stat(stagePath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("recovery left interrupted staging data: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rootPath, "destination", "clone")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("recovery replayed copy or created target: %v", err)
	}
}

func TestCopyDirectoryRecoveryKeepsActiveOperationRunning(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "source"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "source", "file.txt"), []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(rootPath, "destination"), 0o755); err != nil {
		t.Fatal(err)
	}
	metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()
	plan, err := service.Propose(ctx, copyDirectoryProposal("source", "destination/clone"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve"); err != nil {
		t.Fatal(err)
	}
	if err := metadata.StartAgentOperation(ctx, plan.ID, plan.Operations[0].ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	nonce := strings.Repeat("e", 24)
	stageName := ".filesage-tree-copy-" + nonce
	stagePath := filepath.Join(rootPath, "destination", stageName)
	if err := os.Mkdir(stagePath, 0o700); err != nil {
		t.Fatal(err)
	}
	stat := unix.Statx_t{}
	if err := unix.Statx(unix.AT_FDCWD, stagePath, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_BASIC_STATS|unix.STATX_MNT_ID, &stat); err != nil {
		t.Fatal(err)
	}
	markerPath := stagePath + ".lock"
	marker := fmt.Sprintf("filesage-tree-copy-v1\nactive\n%s\n%d\n%d\n%d\n", nonce, unix.Mkdev(stat.Dev_major, stat.Dev_minor), stat.Ino, stat.Mnt_id)
	if err := os.WriteFile(markerPath, []byte(marker), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(markerPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	if err := service.Recover(ctx); !errors.Is(err, fsroot.ErrTreeCopyInProgress) {
		t.Fatalf("Recover() error = %v, want active-copy error", err)
	}
	recovered, err := service.Plan(ctx, plan.ID)
	if err != nil || recovered.Operations[0].Status != store.OperationRunning {
		t.Fatalf("recovered plan = (%#v, %v), want operation to remain running", recovered, err)
	}
	if _, err := os.Stat(stagePath); err != nil {
		t.Errorf("active staging directory was removed: %v", err)
	}
}

func TestCopyDirectoryRejectsNestedTargetsAndUnsafeEntries(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootPath, "source"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "source", "file.txt"), []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()
	if _, err := service.Propose(ctx, copyDirectoryProposal("source", "source/clone")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nested target Propose() error = %v, want ErrInvalid", err)
	}
	if err := os.Symlink("file.txt", filepath.Join(rootPath, "source", "link.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Propose(ctx, copyDirectoryProposal("source", "clone")); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("symlink tree Propose() error = %v, want ErrUnsupported", err)
	}
	if _, err := os.Lstat(filepath.Join(rootPath, "clone")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsafe tree created target: %v", err)
	}
}

func TestConversationCopyCreatesPlanOnlyAfterUserConfirmation(t *testing.T) {
	for _, test := range []struct {
		action string
		source string
		target string
	}{
		{action: "copy_file", source: "source/report.txt", target: "destination/report.txt"},
		{action: "copy_directory", source: "source/project", target: "destination/project"},
	} {
		t.Run(test.action, func(t *testing.T) {
			ctx := context.Background()
			rootPath := t.TempDir()
			if err := os.MkdirAll(filepath.Join(rootPath, "destination"), 0o755); err != nil {
				t.Fatal(err)
			}
			if test.action == "copy_file" {
				if err := os.Mkdir(filepath.Join(rootPath, "source"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(rootPath, filepath.FromSlash(test.source)), []byte("local only"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.MkdirAll(filepath.Join(rootPath, filepath.FromSlash(test.source)), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(rootPath, filepath.FromSlash(test.source), "readme.md"), []byte("local only"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			metadata, root, plans := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
			defer root.Close()
			defer metadata.Close()
			model := &scriptedConversationModel{
				status:  ModelStatus{Available: true, Provider: "test-provider", Model: "test-model", Destination: "local"},
				replies: []string{`{"state":"ready","reply":"将按本机路径复制，不会发送文件正文，请确认。","goal":"复制一份文件","operations":[{"action":"` + test.action + `","filePath":"` + test.source + `","targetPath":"` + test.target + `"}]}`},
			}
			conversationService := &ConversationService{store: metadata, root: root, plans: plans, model: model}
			conversation, err := conversationService.Create(ctx, "复制一份文件")
			if err != nil || conversation.State != store.ConversationAwaitingConfirmation || conversation.Analysis == nil {
				t.Fatalf("Create() = (%#v, %v)", conversation, err)
			}
			if strings.Contains(strings.Join(conversation.Analysis.DataTypes, " "), "正文全文") {
				t.Fatalf("copy confirmation unexpectedly discloses file body: %#v", conversation.Analysis.DataTypes)
			}
			model.mu.Lock()
			request := append([]ChatMessage(nil), model.requests[0]...)
			model.mu.Unlock()
			if strings.Contains(marshalMessages(t, request), "local only") {
				t.Fatal("file body was sent to the model for a copy operation")
			}
			confirmed, err := conversationService.ConfirmAnalysis(ctx, conversation.ID)
			if err != nil || confirmed.State != store.ConversationAwaitingApproval || confirmed.PlanID == "" || model.callCount() != 1 {
				t.Fatalf("ConfirmAnalysis() = (%#v, %v), model calls = %d", confirmed, err, model.callCount())
			}
			plan, err := plans.Plan(ctx, confirmed.PlanID)
			if err != nil || len(plan.Operations) != 1 || plan.Operations[0].Action != test.action {
				t.Fatalf("copy plan = (%#v, %v)", plan, err)
			}
			if _, err := os.Stat(filepath.Join(rootPath, filepath.FromSlash(test.target))); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("confirmation copied before approval: %v", err)
			}
		})
	}
}

func copyFileProposal(source, target string) Proposal {
	return Proposal{RequestText: "复制文件", Action: "copy_file", SourcePath: source, TargetPath: target, Reason: "按用户确认的目标路径复制文件"}
}

func copyDirectoryProposal(source, target string) Proposal {
	return Proposal{RequestText: "复制文件夹", Action: "copy_directory", SourcePath: source, TargetPath: target, Reason: "按用户确认的目标路径复制文件夹"}
}
