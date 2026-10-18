//go:build linux

// 本文件验证 Agent 元数据操作的审批、锁和恢复语义。
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"filesage/internal/fsroot"
	"filesage/internal/store"
)

func TestMetadataPlanRequiresApprovalAndExplicitExecution(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	filePath := filepath.Join(rootPath, "notes.md")
	if err := os.WriteFile(filePath, []byte("metadata test"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()

	mode := uint32(0o640)
	plan := proposeMetadata(t, ctx, service, "set_permissions", "notes.md", &mode, nil, nil)
	op := plan.Operations[0]
	if op.Action != "set_permissions" || op.SourcePath != op.TargetPath || op.Mode == nil || *op.Mode != mode || op.OwnerUID != nil || op.GroupGID != nil {
		t.Fatalf("metadata operation = %#v", op)
	}
	if got := fileMode(t, filePath); got != 0o600 {
		t.Fatalf("proposal changed mode to %#o before approval", got)
	}
	if _, err := service.Decide(ctx, plan.ID, op.ID, "approve"); err != nil {
		t.Fatal(err)
	}
	if got := fileMode(t, filePath); got != 0o600 {
		t.Fatalf("approval changed mode to %#o before execution", got)
	}
	completed, err := service.Execute(ctx, plan.ID)
	if err != nil || completed.Status != store.PlanCompleted || completed.Operations[0].Status != store.OperationSucceeded {
		t.Fatalf("Execute() = (%#v, %v)", completed, err)
	}
	if got := fileMode(t, filePath); got != 0o640 {
		t.Fatalf("executed mode = %#o, want 0640", got)
	}
	if completed.Operations[0].ResultMessage == "" {
		t.Fatal("metadata result was not included in the audit outcome")
	}
}

func TestOwnerPlanPersistsOnlyRequestedFieldsAndExecutes(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	filePath := filepath.Join(rootPath, "owned.txt")
	if err := os.WriteFile(filePath, []byte("owner test"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()
	entry, err := root.LstatEntry("owned.txt")
	if err != nil {
		t.Fatal(err)
	}
	uid := entry.OwnerUID
	plan := proposeMetadata(t, ctx, service, "set_owner", "owned.txt", nil, &uid, nil)
	op := plan.Operations[0]
	if op.Action != "set_owner" || op.Mode != nil || op.OwnerUID == nil || *op.OwnerUID != uid || op.GroupGID != nil {
		t.Fatalf("owner operation = %#v", op)
	}
	if _, err := service.Decide(ctx, plan.ID, op.ID, "approve"); err != nil {
		t.Fatal(err)
	}
	completed, err := service.Execute(ctx, plan.ID)
	if err != nil || completed.Status != store.PlanCompleted || completed.Operations[0].Status != store.OperationSucceeded {
		t.Fatalf("Execute() = (%#v, %v)", completed, err)
	}
	after, err := root.LstatEntry("owned.txt")
	if err != nil || after.OwnerUID != uid || after.GroupGID != entry.GroupGID {
		t.Fatalf("owner after operation = (%#v, %v)", after, err)
	}
}

func TestOwnerPlanPersistsZeroIDsWithoutExecuting(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	filePath := filepath.Join(rootPath, "zero-owner.txt")
	if err := os.WriteFile(filePath, []byte("owner persistence"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()
	before, err := root.LstatEntry("zero-owner.txt")
	if err != nil {
		t.Fatal(err)
	}
	uid, gid := uint32(0), uint32(0)
	plan := proposeMetadata(t, ctx, service, "set_owner", "zero-owner.txt", nil, &uid, &gid)
	reloaded, err := service.Plan(ctx, plan.ID)
	if err != nil || len(reloaded.Operations) != 1 {
		t.Fatalf("reloaded owner plan = (%#v, %v)", reloaded, err)
	}
	op := reloaded.Operations[0]
	if op.OwnerUID == nil || *op.OwnerUID != 0 || op.GroupGID == nil || *op.GroupGID != 0 {
		t.Fatalf("zero owner IDs did not round-trip as non-nil pointers: %#v", op)
	}
	after, err := root.LstatEntry("zero-owner.txt")
	if err != nil || after.OwnerUID != before.OwnerUID || after.GroupGID != before.GroupGID {
		t.Fatalf("reading the persisted plan changed ownership: before=%#v after=%#v err=%v", before, after, err)
	}
}

func TestMetadataPlanCanChangeDirectoryPermissions(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	directoryPath := filepath.Join(rootPath, "documents")
	if err := os.Mkdir(directoryPath, 0o700); err != nil {
		t.Fatal(err)
	}
	metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()
	mode := uint32(0o750)
	plan := proposeMetadata(t, ctx, service, "set_permissions", "documents", &mode, nil, nil)
	if _, err := service.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve"); err != nil {
		t.Fatal(err)
	}
	completed, err := service.Execute(ctx, plan.ID)
	if err != nil || completed.Status != store.PlanCompleted {
		t.Fatalf("Execute() = (%#v, %v)", completed, err)
	}
	entry, err := root.LstatEntry("documents")
	if err != nil || entry.Kind != fsroot.KindDirectory || entry.Mode.Perm() != 0o750 {
		t.Fatalf("directory metadata = (%#v, %v)", entry, err)
	}
}

func TestMetadataApprovalInvalidatesChangedFingerprint(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	filePath := filepath.Join(rootPath, "stale.txt")
	if err := os.WriteFile(filePath, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()
	mode := uint32(0o640)
	plan := proposeMetadata(t, ctx, service, "set_permissions", "stale.txt", &mode, nil, nil)
	if err := os.WriteFile(filePath, []byte("changed after proposal"), 0o600); err != nil {
		t.Fatal(err)
	}
	updated, err := service.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve")
	if !errors.Is(err, ErrConflict) || updated.Status != store.PlanInvalidated || updated.Operations[0].Approval != store.ApprovalInvalidated {
		t.Fatalf("Decide() = (%#v, %v), want invalidated snapshot", updated, err)
	}
	if got := fileMode(t, filePath); got != 0o600 {
		t.Fatalf("stale approval changed mode to %#o", got)
	}
}

func TestMetadataResultVerificationRejectsInjectedSpecialModeBits(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	filePath := filepath.Join(rootPath, "result.txt")
	if err := os.WriteFile(filePath, []byte("metadata result"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()
	mode := uint32(0o640)
	plan := proposeMetadata(t, ctx, service, "set_permissions", "result.txt", &mode, nil, nil)
	op := plan.Operations[0]
	var expected fsroot.Fingerprint
	if err := json.Unmarshal([]byte(op.SourceStateJSON), &expected); err != nil {
		t.Fatal(err)
	}
	targetMode := fs.FileMode(mode)
	updated, err := root.UpdateMetadata("result.txt", expected, fsroot.MetadataUpdate{Mode: &targetMode})
	if err != nil {
		t.Fatal(err)
	}
	for _, bit := range []fs.FileMode{fs.ModeSetuid, fs.ModeSetgid, fs.ModeSticky} {
		injected := updated.Fingerprint()
		injected.Mode |= bit
		if matchesMetadataTarget(injected, expected, op) {
			t.Fatalf("metadata result with injected mode bit %v was accepted", bit)
		}
	}
}

func TestHardLinkMetadataPlansSerializeOnInode(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	firstPath := filepath.Join(rootPath, "first.txt")
	secondPath := filepath.Join(rootPath, "second.txt")
	if err := os.WriteFile(firstPath, []byte("linked"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(firstPath, secondPath); err != nil {
		t.Fatal(err)
	}
	metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()
	firstMode, secondMode := uint32(0o640), uint32(0o644)
	first := proposeMetadata(t, ctx, service, "set_permissions", "first.txt", &firstMode, nil, nil)
	second := proposeMetadata(t, ctx, service, "set_permissions", "second.txt", &secondMode, nil, nil)
	for _, plan := range []store.PlanRecord{first, second} {
		if _, err := service.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve"); err != nil {
			t.Fatal(err)
		}
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for _, plan := range []store.PlanRecord{first, second} {
		workers.Add(1)
		go func(planID string) {
			defer workers.Done()
			<-start
			_, err := service.Execute(ctx, planID)
			results <- err
		}(plan.ID)
	}
	close(start)
	workers.Wait()
	close(results)
	succeeded, conflicted := 0, 0
	for err := range results {
		if err == nil {
			succeeded++
		} else if errors.Is(err, ErrConflict) {
			conflicted++
		} else {
			t.Fatalf("Execute() error = %v", err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("same-inode executions: succeeded=%d conflicted=%d, want 1 each", succeeded, conflicted)
	}
	firstEntry, err := root.LstatEntry("first.txt")
	if err != nil || firstEntry.Mode.Perm() != 0o640 && firstEntry.Mode.Perm() != 0o644 {
		t.Fatalf("hard-linked target mode = (%#o, %v)", firstEntry.Mode.Perm(), err)
	}
}

func TestRecoverMetadataUsesOriginalTargetAndManualReviewStates(t *testing.T) {
	for _, test := range []struct {
		name       string
		state      string
		specialBit fs.FileMode
		wantStatus store.OperationStatus
	}{
		{name: "unchanged", wantStatus: store.OperationPending},
		{name: "approved value reached", state: "target", wantStatus: store.OperationSucceeded},
		{name: "different value", state: "other", wantStatus: store.OperationPartial},
		{name: "unapproved setuid", state: "special", specialBit: fs.ModeSetuid, wantStatus: store.OperationPartial},
		{name: "unapproved setgid", state: "special", specialBit: fs.ModeSetgid, wantStatus: store.OperationPartial},
		{name: "unapproved sticky bit", state: "special", specialBit: fs.ModeSticky, wantStatus: store.OperationPartial},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			rootPath := t.TempDir()
			filePath := filepath.Join(rootPath, "recover.txt")
			if err := os.WriteFile(filePath, []byte("recover"), 0o600); err != nil {
				t.Fatal(err)
			}
			metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
			defer root.Close()
			defer metadata.Close()
			mode := uint32(0o640)
			plan := proposeMetadata(t, ctx, service, "set_permissions", "recover.txt", &mode, nil, nil)
			op := plan.Operations[0]
			if _, err := service.Decide(ctx, plan.ID, op.ID, "approve"); err != nil {
				t.Fatal(err)
			}
			if err := metadata.StartAgentOperation(ctx, plan.ID, op.ID, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			if test.state != "" {
				targetMode := fs.FileMode(mode)
				if test.state == "other" {
					targetMode = 0o644
				}
				if test.state == "special" {
					if err := os.Chmod(filePath, targetMode|test.specialBit); err != nil {
						t.Fatal(err)
					}
					info, err := os.Lstat(filePath)
					if err != nil {
						t.Fatal(err)
					}
					if info.Mode()&test.specialBit == 0 {
						t.Skipf("filesystem did not retain special mode bit %v", test.specialBit)
					}
				} else {
					current, err := root.LstatEntry("recover.txt")
					if err != nil {
						t.Fatal(err)
					}
					if _, err := root.UpdateMetadata("recover.txt", current.Fingerprint(), fsroot.MetadataUpdate{Mode: &targetMode}); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := service.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			recovered, err := service.Plan(ctx, plan.ID)
			if err != nil || recovered.Operations[0].Status != test.wantStatus {
				t.Fatalf("recovered operation = (%#v, %v), want %q", recovered.Operations[0], err, test.wantStatus)
			}
			if test.state == "target" && !strings.Contains(recovered.Operations[0].ResultMessage, "权限 mode：0600 → 0640") {
				t.Fatalf("recovered success audit lacks observed mode transition: %q", recovered.Operations[0].ResultMessage)
			}
			if test.state == "target" {
				audit, err := metadata.AuditForTask(ctx, plan.TaskID)
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, record := range audit {
					if record.EventType == "operation_recovered" {
						found = strings.Contains(record.Message, "权限 mode：0600 → 0640")
					}
				}
				if !found {
					t.Fatalf("recovery audit does not record observed mode transition: %#v", audit)
				}
			}
			if test.state == "" && fileMode(t, filePath) != 0o600 {
				t.Fatal("recovery automatically replayed an unchanged metadata action")
			}
		})
	}
}

func TestMetadataProposalRejectsSymlinksAndMixedParameters(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "target.txt"), []byte("target"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target.txt", filepath.Join(rootPath, "alias.txt")); err != nil {
		t.Fatal(err)
	}
	metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()
	mode := uint32(0o640)
	if _, err := service.Propose(ctx, Proposal{
		RequestText: "更新权限", Action: "set_permissions", SourcePath: "alias.txt", TargetPath: "alias.txt", Reason: "按用户目标修改",
		Mode: &mode,
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("symlink proposal error = %v, want conflict", err)
	}
	uid := uint32(1)
	if err := validateOperation(Proposal{
		Action: "set_permissions", SourcePath: "target.txt", TargetPath: "target.txt", Reason: "权限",
		Mode: &mode, OwnerUID: &uid,
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("mixed metadata proposal error = %v, want invalid", err)
	}
}

func TestMetadataProposalRejectsSpecialModeBits(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name string
		path string
		mode fs.FileMode
		bit  fs.FileMode
		kind string
	}{
		{name: "setuid", path: "setuid.txt", mode: fs.ModeSetuid | 0o755, bit: fs.ModeSetuid, kind: "file"},
		{name: "setgid", path: "setgid.txt", mode: fs.ModeSetgid | 0o755, bit: fs.ModeSetgid, kind: "file"},
		{name: "sticky", path: "sticky", mode: fs.ModeSticky | 0o777, bit: fs.ModeSticky, kind: "directory"},
	} {
		t.Run(test.name, func(t *testing.T) {
			rootPath := t.TempDir()
			absolute := filepath.Join(rootPath, test.path)
			if test.kind == "directory" {
				if err := os.Mkdir(absolute, 0o700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(absolute, []byte("metadata test"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(absolute, test.mode); err != nil {
				t.Fatal(err)
			}
			info, err := os.Lstat(absolute)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode()&test.bit == 0 {
				t.Skipf("filesystem did not retain requested special mode %#o", test.mode)
			}

			metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
			defer root.Close()
			defer metadata.Close()
			mode := uint32(0o640)
			if _, err := service.Propose(ctx, Proposal{
				RequestText: "更新元数据", Action: "set_permissions", SourcePath: test.path, TargetPath: test.path, Reason: "按用户目标修改",
				Mode: &mode,
			}); !errors.Is(err, ErrUnsupported) {
				t.Fatalf("set_permissions proposal error = %v, want unsupported", err)
			}
			uid := uint32(1)
			if _, err := service.Propose(ctx, Proposal{
				RequestText: "更新元数据", Action: "set_owner", SourcePath: test.path, TargetPath: test.path, Reason: "按用户目标修改",
				OwnerUID: &uid,
			}); !errors.Is(err, ErrUnsupported) {
				t.Fatalf("set_owner proposal error = %v, want unsupported", err)
			}
		})
	}
}

func proposeMetadata(t *testing.T, ctx context.Context, service *Service, action, relativePath string, mode, ownerUID, groupGID *uint32) store.PlanRecord {
	t.Helper()
	plan, err := service.Propose(ctx, Proposal{
		RequestText: "设置文件元数据", Action: action, SourcePath: relativePath, TargetPath: relativePath,
		Reason: "按用户确认的目标元数据执行", Mode: mode, OwnerUID: ownerUID, GroupGID: groupGID,
	})
	if err != nil {
		t.Fatalf("Propose(%s) error = %v", action, err)
	}
	return plan
}

func fileMode(t *testing.T, path string) fs.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}
