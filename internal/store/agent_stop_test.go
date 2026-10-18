// 本文件验证 Agent 执行停止、并发边界与审计状态。
package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func approveAndClaimPlan(t *testing.T, operationCount int) (*Store, PlanRecord) {
	t.Helper()
	ctx := context.Background()
	metadata, plan := revisionFixture(t, operationCount)
	for _, operation := range plan.Operations {
		if err := metadata.SetAgentApproval(ctx, plan.ID, operation.ID, ApprovalApproved, time.Now().UTC(), plan.Version); err != nil {
			t.Fatal(err)
		}
	}
	if err := metadata.ClaimAgentPlanExecution(ctx, plan.ID, plan.Version, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	return metadata, plan
}

func TestAgentPlanStopFinishesCurrentOperationAndCancelsLaterItems(t *testing.T) {
	ctx := context.Background()
	metadata, plan := approveAndClaimPlan(t, 2)
	first, second := plan.Operations[0], plan.Operations[1]
	if err := metadata.StartAgentOperation(ctx, plan.ID, first.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := metadata.RequestAgentPlanStop(ctx, plan.ID, plan.Version, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	requested, err := metadata.Plan(ctx, plan.ID)
	if err != nil || requested.Status != PlanExecuting || !requested.StopRequested || requested.Operations[0].Status != OperationRunning || requested.Operations[1].Status != OperationPending {
		t.Fatalf("plan after stop request = (%#v, %v)", requested, err)
	}
	if err := metadata.StartAgentOperation(ctx, plan.ID, second.ID, time.Now().UTC()); !errors.Is(err, ErrConflict) {
		t.Fatalf("later operation start = %v, want conflict", err)
	}
	if err := metadata.FinishAgentOperation(ctx, plan.ID, first.ID, OperationSucceeded, "目录已创建", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	stopped, err := metadata.Plan(ctx, plan.ID)
	if err != nil || stopped.Status != PlanCancelled || !stopped.StopRequested || stopped.Operations[0].Status != OperationSucceeded || stopped.Operations[1].Status != OperationCancelled {
		t.Fatalf("stopped plan = (%#v, %v)", stopped, err)
	}
	tasks, err := metadata.ListTasks(ctx, TaskListQuery{Kind: "agent", Limit: 10})
	if err != nil || len(tasks.Tasks) != 1 || tasks.Tasks[0].Status != TaskCancelled {
		t.Fatalf("stopped task = (%#v, %v)", tasks.Tasks, err)
	}
	events, err := metadata.AuditForTask(ctx, plan.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	var stopRequested, finished, operationCancelled, planStopped int
	lastFinish, firstCancelled, stoppedEvent := -1, -1, -1
	for index, event := range events {
		switch event.EventType {
		case "plan_stop_requested":
			stopRequested++
		case "operation_finished":
			if event.OperationID == first.ID {
				finished++
				lastFinish = index
			}
		case "operation_cancelled":
			if event.OperationID == second.ID {
				operationCancelled++
				firstCancelled = index
			}
		case "plan_execution_stopped":
			planStopped++
			stoppedEvent = index
		}
	}
	if stopRequested != 1 || finished != 1 || operationCancelled != 1 || planStopped != 1 || !(lastFinish < firstCancelled && firstCancelled < stoppedEvent) {
		t.Fatalf("stop audit order/counts: stop=%d finished=%d cancelled=%d stopped=%d indexes=%d/%d/%d events=%#v", stopRequested, finished, operationCancelled, planStopped, lastFinish, firstCancelled, stoppedEvent, events)
	}
}

func TestAgentPlanStopBeforeFirstItemIsAtomic(t *testing.T) {
	ctx := context.Background()
	metadata, plan := approveAndClaimPlan(t, 2)
	if err := metadata.RequestAgentPlanStop(ctx, plan.ID, plan.Version, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	stopped, err := metadata.Plan(ctx, plan.ID)
	if err != nil || stopped.Status != PlanCancelled || !stopped.StopRequested {
		t.Fatalf("plan stopped before first item = (%#v, %v)", stopped, err)
	}
	for _, operation := range stopped.Operations {
		if operation.Status != OperationCancelled {
			t.Fatalf("operation started despite stop: %#v", operation)
		}
	}
	if err := metadata.StartAgentOperation(ctx, plan.ID, plan.Operations[0].ID, time.Now().UTC()); !errors.Is(err, ErrConflict) {
		t.Fatalf("start after stop = %v, want conflict", err)
	}
}

func TestAgentPlanStopRaceWithNextOperationStart(t *testing.T) {
	ctx := context.Background()
	for attempt := 0; attempt < 8; attempt++ {
		metadata, plan := approveAndClaimPlan(t, 2)
		var wait sync.WaitGroup
		wait.Add(2)
		startResult := make(chan error, 1)
		stopResult := make(chan error, 1)
		go func() {
			defer wait.Done()
			startResult <- metadata.StartAgentOperation(ctx, plan.ID, plan.Operations[0].ID, time.Now().UTC())
		}()
		go func() {
			defer wait.Done()
			stopResult <- metadata.RequestAgentPlanStop(ctx, plan.ID, plan.Version, time.Now().UTC())
		}()
		wait.Wait()
		startErr, stopErr := <-startResult, <-stopResult
		if stopErr != nil {
			t.Fatalf("attempt %d stop error = %v", attempt, stopErr)
		}
		current, err := metadata.Plan(ctx, plan.ID)
		if err != nil {
			t.Fatal(err)
		}
		if startErr == nil {
			if current.Status != PlanExecuting || !current.StopRequested || current.Operations[0].Status != OperationRunning {
				t.Fatalf("attempt %d start won with inconsistent state: %#v", attempt, current)
			}
			if err := metadata.FinishAgentOperation(ctx, plan.ID, plan.Operations[0].ID, OperationSucceeded, "目录已创建", time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
		} else if !errors.Is(startErr, ErrConflict) {
			t.Fatalf("attempt %d start error = %v, want conflict or success", attempt, startErr)
		}
		current, err = metadata.Plan(ctx, plan.ID)
		if err != nil || current.Status != PlanCancelled || current.Operations[1].Status != OperationCancelled {
			t.Fatalf("attempt %d final plan = (%#v, %v)", attempt, current, err)
		}
	}
}

func TestAgentPlanStopRecoveryPreservesOutcome(t *testing.T) {
	for _, test := range []struct {
		name       string
		resolution string
		plan       PlanStatus
		first      OperationStatus
		task       TaskStatus
	}{
		{name: "unchanged", resolution: "retry", plan: PlanCancelled, first: OperationCancelled, task: TaskCancelled},
		{name: "completed", resolution: "succeeded", plan: PlanCancelled, first: OperationSucceeded, task: TaskCancelled},
		{name: "uncertain", resolution: "partial", plan: PlanFailed, first: OperationPartial, task: TaskPartial},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			metadata, plan := approveAndClaimPlan(t, 2)
			first := plan.Operations[0]
			if err := metadata.StartAgentOperation(ctx, plan.ID, first.ID, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			if err := metadata.RequestAgentPlanStop(ctx, plan.ID, plan.Version, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			if err := metadata.RecoverAgentOperation(ctx, plan.ID, first.ID, test.resolution, "恢复核查", time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			finished, err := metadata.Plan(ctx, plan.ID)
			if err != nil || finished.Status != test.plan || finished.Operations[0].Status != test.first || finished.Operations[1].Status != OperationCancelled {
				t.Fatalf("recovered plan = (%#v, %v)", finished, err)
			}
			tasks, err := metadata.ListTasks(ctx, TaskListQuery{Kind: "agent", Limit: 10})
			if err != nil || len(tasks.Tasks) != 1 || tasks.Tasks[0].Status != test.task {
				t.Fatalf("recovered task = (%#v, %v)", tasks.Tasks, err)
			}
		})
	}
}

func TestAgentPlanStopAfterFinalItemCompletesNormally(t *testing.T) {
	ctx := context.Background()
	metadata, plan := approveAndClaimPlan(t, 1)
	operation := plan.Operations[0]
	if err := metadata.StartAgentOperation(ctx, plan.ID, operation.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := metadata.RequestAgentPlanStop(ctx, plan.ID, plan.Version, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := metadata.FinishAgentOperation(ctx, plan.ID, operation.ID, OperationSucceeded, "已完成", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	finished, err := metadata.Plan(ctx, plan.ID)
	if err != nil || finished.Status != PlanCompleted || finished.Operations[0].Status != OperationSucceeded {
		t.Fatalf("final item stop race = (%#v, %v)", finished, err)
	}
}
