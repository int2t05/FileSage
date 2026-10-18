//go:build linux

// 本文件验证方案编辑的版本、审批与审计边界。
package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"filesage/internal/store"
)

func TestRevisePlanCreatesFreshPendingVersionAndAuditsEdit(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	parent := filepath.Join(rootPath, "parent")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()

	previous := proposeDirectories(t, ctx, service, "parent", "first", "second")
	revised, err := service.RevisePlan(ctx, previous.ID, previous.Version, []Proposal{
		{Action: "create_directory", SourcePath: "parent", TargetPath: "parent/contracts", Reason: "归档合同"},
		{Action: "create_directory", SourcePath: "parent", TargetPath: "parent/photos", Reason: "归档照片"},
	})
	if err != nil {
		t.Fatalf("RevisePlan() error = %v", err)
	}
	if revised.ID == previous.ID || revised.TaskID != previous.TaskID || revised.Version != previous.Version+1 || revised.Status != store.PlanAwaitingApproval || len(revised.Operations) != 2 {
		t.Fatalf("revised plan = %#v", revised)
	}
	if revised.Operations[0].Approval != store.ApprovalPending || revised.Operations[0].Status != store.OperationPending || revised.Operations[0].TargetPath != "parent/contracts" {
		t.Fatalf("edited operation was not reset for fresh review: %#v", revised.Operations[0])
	}
	old, err := service.Plan(ctx, previous.ID)
	if err != nil || old.Status != store.PlanInvalidated {
		t.Fatalf("previous plan = (%#v, %v)", old, err)
	}
	for _, operation := range old.Operations {
		if operation.Approval != store.ApprovalInvalidated || operation.Status != store.OperationCancelled {
			t.Fatalf("previous operation was not invalidated: %#v", operation)
		}
	}
	latest, err := service.LatestPlan(ctx, previous.TaskID)
	if err != nil || latest.ID != revised.ID {
		t.Fatalf("latest plan = (%#v, %v)", latest, err)
	}
	page, err := metadata.AuditForTaskPage(ctx, previous.TaskID, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	var edited, superseded, created int
	for _, event := range page.Records {
		switch event.EventType {
		case "plan_edited":
			edited++
		case "operation_superseded":
			superseded++
		case "plan_created":
			if event.PlanID == revised.ID {
				created++
			}
		}
	}
	if edited != 1 || superseded != len(previous.Operations) || created != len(revised.Operations) {
		t.Fatalf("revision audit counts: edited=%d superseded=%d created=%d events=%#v", edited, superseded, created, page.Records)
	}
}

func TestRevisePlanRejectsStaleApprovalStateTargetConflictsAndLimits(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	parent := filepath.Join(rootPath, "parent")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(parent, "occupied"), 0o755); err != nil {
		t.Fatal(err)
	}
	metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()

	previous := proposeDirectories(t, ctx, service, "parent", "first", "second")
	edit := []Proposal{{Action: "create_directory", SourcePath: "parent", TargetPath: "parent/available", Reason: "测试编辑"}}
	if _, err := service.RevisePlan(ctx, previous.ID, previous.Version+1, edit); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale revision = %v, want conflict", err)
	}
	conflicting := []Proposal{{Action: "create_directory", SourcePath: "parent", TargetPath: "parent/occupied", Reason: "占用目标"}}
	if _, err := service.RevisePlan(ctx, previous.ID, previous.Version, conflicting); !errors.Is(err, ErrConflict) {
		t.Fatalf("occupied target revision = %v, want conflict", err)
	}
	unchanged, err := service.Plan(ctx, previous.ID)
	if err != nil || unchanged.Status != store.PlanAwaitingApproval || unchanged.Operations[0].Approval != store.ApprovalPending {
		t.Fatalf("failed edit changed old plan: (%#v, %v)", unchanged, err)
	}
	tooMany := make([]Proposal, 21)
	for index := range tooMany {
		tooMany[index] = Proposal{Action: "create_directory", SourcePath: "parent", TargetPath: filepath.ToSlash(filepath.Join("parent", fmt.Sprintf("new-%02d", index))), Reason: "数量上限验证"}
	}
	if _, err := service.RevisePlan(ctx, previous.ID, previous.Version, tooMany); !errors.Is(err, ErrInvalid) {
		t.Fatalf("21-operation revision = %v, want invalid", err)
	}
	if _, err := service.Decide(ctx, previous.ID, previous.Operations[0].ID, "approve", previous.Version); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RevisePlan(ctx, previous.ID, previous.Version, edit); !errors.Is(err, ErrConflict) {
		t.Fatalf("revision after approval = %v, want conflict", err)
	}
}

func TestRevisePlanRacesApprovalAndExecutionWithoutChangingApprovedPlan(t *testing.T) {
	t.Run("approval", func(t *testing.T) {
		ctx := context.Background()
		rootPath := t.TempDir()
		if err := os.Mkdir(filepath.Join(rootPath, "parent"), 0o755); err != nil {
			t.Fatal(err)
		}
		metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
		defer root.Close()
		defer metadata.Close()
		previous := proposeDirectories(t, ctx, service, "parent", "first", "second")
		start := make(chan struct{})
		var wait sync.WaitGroup
		wait.Add(2)
		var approveErr, reviseErr error
		go func() {
			defer wait.Done()
			<-start
			_, approveErr = service.Decide(ctx, previous.ID, previous.Operations[0].ID, "approve", previous.Version)
		}()
		go func() {
			defer wait.Done()
			<-start
			_, reviseErr = service.RevisePlan(ctx, previous.ID, previous.Version, []Proposal{
				{Action: "create_directory", SourcePath: "parent", TargetPath: "parent/changed", Reason: "竞态修改"},
			})
		}()
		close(start)
		wait.Wait()
		if (approveErr == nil) == (reviseErr == nil) {
			t.Fatalf("approval/revision must have one winner: approve=%v revise=%v", approveErr, reviseErr)
		}
	})

	t.Run("execution", func(t *testing.T) {
		ctx := context.Background()
		rootPath := t.TempDir()
		if err := os.Mkdir(filepath.Join(rootPath, "parent"), 0o755); err != nil {
			t.Fatal(err)
		}
		metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
		defer root.Close()
		defer metadata.Close()
		plan := proposeDirectories(t, ctx, service, "parent", "run")
		approveAllOperations(t, ctx, service, plan)
		start := make(chan struct{})
		var wait sync.WaitGroup
		wait.Add(2)
		var executeErr, reviseErr error
		go func() {
			defer wait.Done()
			<-start
			_, executeErr = service.Execute(ctx, plan.ID, plan.Version)
		}()
		go func() {
			defer wait.Done()
			<-start
			_, reviseErr = service.RevisePlan(ctx, plan.ID, plan.Version, []Proposal{
				{Action: "create_directory", SourcePath: "parent", TargetPath: "parent/changed", Reason: "执行竞态修改"},
			})
		}()
		close(start)
		wait.Wait()
		if executeErr != nil || !errors.Is(reviseErr, ErrConflict) {
			t.Fatalf("execute/revision race: execute=%v revise=%v", executeErr, reviseErr)
		}
	})
}
