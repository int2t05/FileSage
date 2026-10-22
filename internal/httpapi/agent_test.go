// 本文件验证 Agent 审批路由的契约与同源保护。
package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"filesage/internal/agent"
	"filesage/internal/fsroot"
	"filesage/internal/store"
)

type agentServiceStub struct {
	plan              store.PlanRecord
	err               error
	decisions         int
	rejections        int
	executions        int
	stopExecutions    int
	revisions         int
	version           int
	proposals         []agent.Proposal
	preview           agent.TextEditPreview
	previewCalls      int
	textRevisionCalls int
	textCandidate     []byte
}

type agentExecutionServiceStub struct {
	*agentServiceStub
	starts                 int
	steps                  int
	stepID                 string
	version                int
	mode                   string
	confirmPermanentDelete bool
}

func (stub *agentExecutionServiceStub) StartPlan(_ context.Context, _ string, version int, mode string, confirmPermanentDelete bool) (store.PlanRecord, error) {
	stub.starts++
	stub.version = version
	stub.mode = mode
	stub.confirmPermanentDelete = confirmPermanentDelete
	return stub.plan, stub.err
}

func (stub *agentExecutionServiceStub) ExecuteStep(_ context.Context, _, operationID string, version int) (store.PlanRecord, error) {
	stub.steps++
	stub.stepID = operationID
	stub.version = version
	return stub.plan, stub.err
}

func (stub *agentServiceStub) Plan(context.Context, string) (store.PlanRecord, error) {
	return stub.plan, stub.err
}
func (stub *agentServiceStub) LatestPlan(context.Context, string) (store.PlanRecord, error) {
	return stub.plan, stub.err
}
func (stub *agentServiceStub) RevisePlan(_ context.Context, _ string, version int, proposals []agent.Proposal) (store.PlanRecord, error) {
	stub.revisions++
	stub.version = version
	stub.proposals = proposals
	return stub.plan, stub.err
}
func (stub *agentServiceStub) Decide(_ context.Context, _, _, _ string, version ...int) (store.PlanRecord, error) {
	stub.decisions++
	if len(version) == 1 {
		stub.version = version[0]
	}
	return stub.plan, stub.err
}
func (stub *agentServiceStub) RejectPlan(_ context.Context, _ string, version int) (store.PlanRecord, error) {
	stub.rejections++
	stub.version = version
	return stub.plan, stub.err
}
func (stub *agentServiceStub) Execute(_ context.Context, _ string, version ...int) (store.PlanRecord, error) {
	stub.executions++
	if len(version) == 1 {
		stub.version = version[0]
	}
	return stub.plan, stub.err
}
func (stub *agentServiceStub) StopExecution(_ context.Context, _ string, version int) (store.PlanRecord, error) {
	stub.stopExecutions++
	stub.version = version
	return stub.plan, stub.err
}
func (stub *agentServiceStub) PreviewTextEdit(_ context.Context, _, _ string, version int) (agent.TextEditPreview, error) {
	stub.previewCalls++
	stub.version = version
	return stub.preview, stub.err
}
func (stub *agentServiceStub) ReviseTextCandidate(_ context.Context, _, _ string, version int, content []byte) (store.PlanRecord, error) {
	stub.textRevisionCalls++
	stub.version = version
	stub.textCandidate = append([]byte(nil), content...)
	return stub.plan, stub.err
}

func TestManualAgentProposalEndpointIsRemoved(t *testing.T) {
	stub := &agentServiceStub{}
	handler := NewWithAgent(nil, nil, nil, stub, nil, nil)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/agent/tasks", strings.NewReader(`{"requestText":"rename"}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("manual proposal endpoint status=%d, want 404", response.Code)
	}
}

func TestAgentTaskRouteReturnsNotFoundWithoutPlan(t *testing.T) {
	stub := &agentServiceStub{err: store.ErrNotFound}
	handler := NewWithAgent(nil, nil, nil, stub, nil, nil)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/agent/tasks/missing", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("task lookup status = %d, want 404", response.Code)
	}
}

func TestAgentExecuteRequiresExpectedPlanVersion(t *testing.T) {
	stub := &agentServiceStub{plan: store.PlanRecord{ID: "plan-1", TaskID: "task-1", Status: store.PlanApproved}}
	handler := NewWithAgent(nil, nil, nil, stub, nil, nil)
	request := httptest.NewRequest(http.MethodPost, "http://filesage.local/api/v1/agent/plans/plan-1/execute", strings.NewReader("{}"))
	request.Header.Set("Origin", "http://filesage.local")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || stub.executions != 0 {
		t.Fatalf("chunked execute body status=%d executions=%d", response.Code, stub.executions)
	}
}

func TestAgentPlanRevisionSupportsMaximumPlanSize(t *testing.T) {
	stub := &agentServiceStub{plan: store.PlanRecord{ID: "plan-1", TaskID: "task-1", Version: 2, Status: store.PlanAwaitingApproval}}
	handler := NewWithAgent(nil, nil, nil, stub, nil, nil)
	operations := make([]agentPlanRevisionOperation, store.MaxAgentPlanOperations)
	for index := range operations {
		operations[index] = agentPlanRevisionOperation{
			Action: "create_directory", SourcePath: "", TargetPath: "整理/目录", Reason: "按用户目标创建目录",
			Evidence: strings.Repeat("e", 2048),
		}
	}
	body, err := json.Marshal(agentPlanRevisionRequest{ExpectedPlanVersion: 1, Operations: operations})
	if err != nil {
		t.Fatal(err)
	}
	if len(body) <= 16<<10 {
		t.Fatalf("request body = %d bytes, want to exceed the former 16 KiB limit", len(body))
	}
	request := httptest.NewRequest(http.MethodPut, "http://filesage.local/api/v1/agent/plans/plan-1/revision", strings.NewReader(string(body)))
	request.Header.Set("Origin", "http://filesage.local")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || stub.revisions != 1 || len(stub.proposals) != store.MaxAgentPlanOperations {
		t.Fatalf("revision status=%d calls=%d proposals=%d body=%s", response.Code, stub.revisions, len(stub.proposals), response.Body.String())
	}

	operations = append(operations, operations[0])
	body, err = json.Marshal(agentPlanRevisionRequest{ExpectedPlanVersion: 1, Operations: operations})
	if err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodPut, "http://filesage.local/api/v1/agent/plans/plan-1/revision", strings.NewReader(string(body)))
	request.Header.Set("Origin", "http://filesage.local")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || stub.revisions != 1 {
		t.Fatalf("oversized revision status=%d calls=%d body=%s", response.Code, stub.revisions, response.Body.String())
	}
}

func TestAgentEveryWriteRouteChecksOrigin(t *testing.T) {
	stub := &agentServiceStub{plan: store.PlanRecord{
		ID: "plan-1", TaskID: "task-1", Version: 1, Status: store.PlanApproved,
		Operations: []store.OperationRecord{{ID: "op-1", Action: "rename", Approval: store.ApprovalApproved, Status: store.OperationPending}},
	}}
	handler := NewWithAgent(nil, nil, nil, stub, nil, nil)
	for _, endpoint := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/api/v1/agent/plans/plan-1/reject", `{"expectedPlanVersion":1}`},
		{http.MethodPost, "/api/v1/agent/plans/plan-1/execute", `{"expectedPlanVersion":1}`},
		{http.MethodPost, "/api/v1/agent/plans/plan-1/stop", `{"expectedPlanVersion":1}`},
		{http.MethodPut, "/api/v1/agent/plans/plan-1/revision", `{"expectedPlanVersion":1,"operations":[{"action":"move_file","sourcePath":"a","targetPath":"b","reason":"r","evidence":""}]}`},
		{http.MethodPut, "/api/v1/agent/plans/plan-1/operations/op-1/content", `{"expectedPlanVersion":1,"proposedContent":"new"}`},
	} {
		request := httptest.NewRequest(endpoint.method, endpoint.path, strings.NewReader(endpoint.body))
		request.Header.Set("Origin", "http://attacker.local")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Errorf("%s %s status = %d, want 403", endpoint.method, endpoint.path, response.Code)
		}
	}
	if stub.decisions != 0 || stub.rejections != 0 || stub.executions != 0 || stub.stopExecutions != 0 || stub.revisions != 0 || stub.textRevisionCalls != 0 {
		t.Fatalf("cross-origin request reached Agent service: decide=%d reject=%d execute=%d stop=%d revise=%d text-revise=%d", stub.decisions, stub.rejections, stub.executions, stub.stopExecutions, stub.revisions, stub.textRevisionCalls)
	}
}

func TestAgentPerOperationApprovalRouteIsRemoved(t *testing.T) {
	stub := &agentServiceStub{}
	handler := NewWithAgent(nil, nil, nil, stub, nil, nil)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/agent/plans/plan-1/operations/op-1/approval", strings.NewReader(`{"decision":"approve","expectedPlanVersion":1}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound || stub.decisions != 0 {
		t.Fatalf("legacy approval route status=%d decisions=%d body=%s", response.Code, stub.decisions, response.Body.String())
	}
}

func TestAgentStopExecutionRequiresVersionAndReturnsProgress(t *testing.T) {
	stub := &agentServiceStub{plan: store.PlanRecord{
		ID: "plan-1", TaskID: "task-1", Version: 4, Status: store.PlanExecuting, StopRequested: true,
		Operations: []store.OperationRecord{{ID: "op-1", Action: "create_directory", Approval: store.ApprovalApproved, Status: store.OperationRunning}},
	}}
	handler := NewWithAgent(nil, nil, nil, stub, nil, nil)
	request := httptest.NewRequest(http.MethodPost, "http://filesage.local/api/v1/agent/plans/plan-1/stop", strings.NewReader(`{}`))
	request.Header.Set("Origin", "http://filesage.local")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || stub.stopExecutions != 0 {
		t.Fatalf("missing version status=%d calls=%d", response.Code, stub.stopExecutions)
	}

	request = httptest.NewRequest(http.MethodPost, "http://filesage.local/api/v1/agent/plans/plan-1/stop", strings.NewReader(`{"expectedPlanVersion":4}`))
	request.Header.Set("Origin", "http://filesage.local")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var body struct {
		Status        store.PlanStatus `json:"status"`
		StopRequested bool             `json:"stopRequested"`
		Operations    []struct {
			ExecutionStatus store.OperationStatus `json:"executionStatus"`
		} `json:"operations"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || stub.stopExecutions != 1 || stub.version != 4 || body.Status != store.PlanExecuting || !body.StopRequested || len(body.Operations) != 1 || body.Operations[0].ExecutionStatus != store.OperationRunning {
		t.Fatalf("stop response status=%d calls=%d version=%d body=%#v", response.Code, stub.stopExecutions, stub.version, body)
	}
}

func TestAgentStopExecutionConflictDoesNotRetry(t *testing.T) {
	stub := &agentServiceStub{err: store.ErrConflict}
	handler := NewWithAgent(nil, nil, nil, stub, nil, nil)
	request := httptest.NewRequest(http.MethodPost, "http://filesage.local/api/v1/agent/plans/plan-1/stop", strings.NewReader(`{"expectedPlanVersion":4}`))
	request.Header.Set("Origin", "http://filesage.local")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict || stub.stopExecutions != 1 {
		t.Fatalf("stale stop status=%d calls=%d", response.Code, stub.stopExecutions)
	}
}

func TestAgentTextEditPreviewAndCandidateRevisionContracts(t *testing.T) {
	stub := &agentServiceStub{
		plan: store.PlanRecord{ID: "plan-1", TaskID: "task-1", Version: 8, Status: store.PlanAwaitingApproval,
			Operations: []store.OperationRecord{{ID: "op-1", Action: "edit_text", SourcePath: "notes.md", TargetPath: "notes.md", ProposedContentSHA256: strings.Repeat("a", 64), ProposedContentSize: 3}}},
		preview: agent.TextEditPreview{PlanID: "plan-1", OperationID: "op-1", Path: "notes.md", SourceContent: "original-secret", ProposedContent: "candidate-secret"},
	}
	handler := NewWithAgent(nil, nil, nil, stub, nil, nil)

	request := httptest.NewRequest(http.MethodGet, "/api/v1/agent/plans/plan-1/operations/op-1/preview?expectedPlanVersion=8", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var previewResponse struct {
		PlanID          string `json:"planId"`
		OperationID     string `json:"operationId"`
		Path            string `json:"path"`
		SourceContent   string `json:"sourceContent"`
		ProposedContent string `json:"proposedContent"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &previewResponse); err != nil {
		t.Fatalf("decode preview response: %v", err)
	}
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" ||
		previewResponse.PlanID != "plan-1" || previewResponse.OperationID != "op-1" || previewResponse.Path != "notes.md" ||
		previewResponse.SourceContent != "original-secret" || previewResponse.ProposedContent != "candidate-secret" || stub.previewCalls != 1 || stub.version != 8 {
		t.Fatalf("preview response status=%d headers=%v calls=%d version=%d body=%s", response.Code, response.Header(), stub.previewCalls, stub.version, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/api/v1/agent/plans/plan-1", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "original-secret") || strings.Contains(response.Body.String(), "candidate-secret") {
		t.Fatalf("regular plan exposed text content: status=%d body=%s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodPut, "http://filesage.local/api/v1/agent/plans/plan-1/operations/op-1/content", strings.NewReader(`{"expectedPlanVersion":8,"proposedContent":"human revised"}`))
	request.Header.Set("Origin", "http://filesage.local")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || stub.textRevisionCalls != 1 || stub.version != 8 || string(stub.textCandidate) != "human revised" || strings.Contains(response.Body.String(), "candidate-secret") {
		t.Fatalf("candidate revision status=%d calls=%d version=%d content=%q body=%s", response.Code, stub.textRevisionCalls, stub.version, stub.textCandidate, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodPut, "http://filesage.local/api/v1/agent/plans/plan-1/operations/op-1/content", strings.NewReader(`{"expectedPlanVersion":8,"proposedContent":"`+strings.Repeat("x", 260<<10)+`"}`))
	request.Header.Set("Origin", "http://filesage.local")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge || stub.textRevisionCalls != 1 {
		t.Fatalf("oversized candidate status=%d revision calls=%d", response.Code, stub.textRevisionCalls)
	}
}

func TestAgentPlanRevisionRequiresVersionAndPassesTypedOperations(t *testing.T) {
	stub := &agentServiceStub{plan: store.PlanRecord{ID: "new-plan", TaskID: "task-1", Version: 2, Status: store.PlanAwaitingApproval}}
	handler := NewWithAgent(nil, nil, nil, stub, nil, nil)
	for _, body := range []string{
		`{"operations":[{"action":"move_file","sourcePath":"a","targetPath":"b","reason":"r","evidence":""}]}`,
		`{"expectedPlanVersion":1,"operations":[{"action":"move_file","sourcePath":"a","targetPath":"b","reason":"r","evidence":"","unexpected":true}]}`,
	} {
		request := httptest.NewRequest(http.MethodPut, "http://filesage.local/api/v1/agent/plans/plan-1/revision", strings.NewReader(body))
		request.Header.Set("Origin", "http://filesage.local")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest || stub.revisions != 0 {
			t.Fatalf("invalid revision status=%d revisions=%d body=%s", response.Code, stub.revisions, response.Body.String())
		}
	}
	request := httptest.NewRequest(http.MethodPut, "http://filesage.local/api/v1/agent/plans/plan-1/revision", strings.NewReader(`{"expectedPlanVersion":1,"operations":[{"action":"set_owner","sourcePath":"file.txt","targetPath":"","reason":"按用户要求调整","evidence":"","ownerUid":1001}]}`))
	request.Header.Set("Origin", "http://filesage.local")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || stub.revisions != 1 || stub.version != 1 || len(stub.proposals) != 1 || stub.proposals[0].OwnerUID == nil || *stub.proposals[0].OwnerUID != 1001 {
		t.Fatalf("revision status=%d revisions=%d version=%d proposals=%#v body=%s", response.Code, stub.revisions, stub.version, stub.proposals, response.Body.String())
	}
}

func TestAgentPlanRevisionReportsUnsupportedAndOutOfStateErrors(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want int
	}{
		{name: "unsupported operation", err: agent.ErrUnsupported, want: http.StatusUnprocessableEntity},
		{name: "approval already began", err: agent.ErrConflict, want: http.StatusConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			stub := &agentServiceStub{err: test.err}
			handler := NewWithAgent(nil, nil, nil, stub, nil, nil)
			request := httptest.NewRequest(http.MethodPut, "http://filesage.local/api/v1/agent/plans/plan-1/revision", strings.NewReader(`{"expectedPlanVersion":1,"operations":[{"action":"move_file","sourcePath":"a","targetPath":"b","reason":"r","evidence":""}]}`))
			request.Header.Set("Origin", "http://filesage.local")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.want || stub.revisions != 1 {
				t.Fatalf("revision status=%d revisions=%d body=%s, want %d", response.Code, stub.revisions, response.Body.String(), test.want)
			}
		})
	}
}

func TestAgentPlanRejectionRequiresAndForwardsExpectedPlanVersion(t *testing.T) {
	stub := &agentServiceStub{plan: store.PlanRecord{
		ID: "plan-1", TaskID: "task-1", Version: 3, Status: store.PlanRejected,
		Operations: []store.OperationRecord{{ID: "op-1", Ordinal: 0, Action: "rename", Approval: store.ApprovalRejected, Status: store.OperationPending}},
	}}
	handler := NewWithAgent(nil, nil, nil, stub, nil, nil)

	request := httptest.NewRequest(http.MethodPost, "/api/v1/agent/plans/plan-1/reject", strings.NewReader(`{}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || stub.rejections != 0 {
		t.Fatalf("missing version status=%d rejections=%d body=%s", response.Code, stub.rejections, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodPost, "/api/v1/agent/plans/plan-1/reject", strings.NewReader(`{"expectedPlanVersion":3}`))
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var approvalBody struct {
		PlanVersion int `json:"planVersion"`
		Operations  []struct {
			Ordinal int `json:"ordinal"`
		} `json:"operations"`
	}
	if response.Code != http.StatusOK || stub.rejections != 1 || stub.version != 3 {
		t.Fatalf("reject status=%d rejections=%d version=%d body=%s", response.Code, stub.rejections, stub.version, response.Body.String())
	}
	if err := json.Unmarshal(response.Body.Bytes(), &approvalBody); err != nil || approvalBody.PlanVersion != 3 || len(approvalBody.Operations) != 1 || approvalBody.Operations[0].Ordinal != 0 {
		t.Fatalf("reject response=%s err=%v", response.Body.String(), err)
	}

	reviewedAt := time.Now().UTC()
	stub.plan.Status = store.PlanApproved
	stub.plan.ExecutionMode = "bypass"
	stub.plan.ReviewedAt = &reviewedAt
	request = httptest.NewRequest(http.MethodPost, "/api/v1/agent/plans/plan-1/execute", strings.NewReader(`{"expectedPlanVersion":3}`))
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || stub.executions != 1 || stub.version != 3 {
		t.Fatalf("execute status=%d executions=%d version=%d body=%s", response.Code, stub.executions, stub.version, response.Body.String())
	}
}

func TestAgentPlanStartAndStepRoutesForwardVersionAndReturnReviewMode(t *testing.T) {
	reviewedAt := "2026-10-18T12:00:00Z"
	stub := &agentExecutionServiceStub{agentServiceStub: &agentServiceStub{plan: store.PlanRecord{
		ID: "plan-1", TaskID: "task-1", Version: 3, Status: store.PlanApproved,
		ExecutionMode: "manual", ReviewedAt: func() *time.Time { value, _ := time.Parse(time.RFC3339, reviewedAt); return &value }(),
		Operations: []store.OperationRecord{{ID: "op-1", Ordinal: 0, Action: "permanent_delete", SourcePath: "archive/old.txt", Approval: store.ApprovalPending, Status: store.OperationPending}},
	}}}
	handler := NewWithAgent(nil, nil, nil, stub, nil, nil)

	request := httptest.NewRequest(http.MethodPost, "http://filesage.local/api/v1/agent/plans/plan-1/start", strings.NewReader(`{"expectedPlanVersion":3,"expectedExecutionMode":"manual"}`))
	request.Header.Set("Origin", "http://filesage.local")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || stub.starts != 0 {
		t.Fatalf("missing irreversible-delete confirmation status=%d starts=%d body=%s", response.Code, stub.starts, response.Body.String())
	}

	for _, test := range []struct {
		path string
		body string
	}{
		{path: "/api/v1/agent/plans/plan-1/start", body: `{"expectedPlanVersion":3,"expectedExecutionMode":"manual","confirmPermanentDelete":true}`},
		{path: "/api/v1/agent/plans/plan-1/steps/op-1/execute", body: `{"expectedPlanVersion":3}`},
	} {
		request := httptest.NewRequest(http.MethodPost, "http://filesage.local"+test.path, strings.NewReader(test.body))
		request.Header.Set("Origin", "http://filesage.local")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("route %s status=%d body=%s", test.path, response.Code, response.Body.String())
		}
		var body struct {
			ExecutionMode string  `json:"executionMode"`
			ReviewedAt    *string `json:"reviewedAt"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.ExecutionMode != "manual" || body.ReviewedAt == nil || *body.ReviewedAt != reviewedAt {
			t.Fatalf("route %s response=%s err=%v", test.path, response.Body.String(), err)
		}
	}
	if stub.starts != 1 || stub.steps != 1 || stub.version != 3 || stub.stepID != "op-1" || stub.mode != "manual" || !stub.confirmPermanentDelete {
		t.Fatalf("execution calls=(%d,%d), step=%q version=%d mode=%q irreversible confirmation=%t", stub.starts, stub.steps, stub.stepID, stub.version, stub.mode, stub.confirmPermanentDelete)
	}
}

func TestAgentPlanExposesMetadataDiffWithoutSnapshot(t *testing.T) {
	before := fsroot.Fingerprint{
		Exists: true, Kind: fsroot.KindFile, Mode: 0o644, Device: 21, Inode: 34, Links: 1,
		IdentityAvailable: true, OwnerUID: 1001, GroupGID: 1002, OwnershipAvailable: true, ChangeTimeAvailable: true,
	}
	snapshot, err := json.Marshal(before)
	if err != nil {
		t.Fatal(err)
	}
	mode := uint32(0o400)
	stub := &agentServiceStub{plan: store.PlanRecord{
		ID: "plan-1", TaskID: "task-1", Version: 1, Status: store.PlanAwaitingApproval,
		Operations: []store.OperationRecord{{
			ID: "op-1", Action: "set_permissions", SourcePath: "memo.txt", TargetPath: "memo.txt",
			Mode: &mode, SourceStateJSON: string(snapshot), Approval: store.ApprovalPending, Status: store.OperationPending,
		}},
	}}
	handler := NewWithAgent(nil, nil, nil, stub, nil, nil)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/agent/plans/plan-1", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("plan response status=%d body=%s", response.Code, response.Body.String())
	}
	var body struct {
		Operations []map[string]json.RawMessage `json:"operations"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || len(body.Operations) != 1 {
		t.Fatalf("plan response = (%s, %v)", response.Body.String(), err)
	}
	operation := body.Operations[0]
	if string(operation["beforeMode"]) != "420" || string(operation["mode"]) != "256" {
		t.Fatalf("permission diff = before %s after %s", operation["beforeMode"], operation["mode"])
	}
	if strings.Contains(response.Body.String(), "sourceStateJSON") || strings.Contains(response.Body.String(), "device") || strings.Contains(response.Body.String(), "inode") {
		t.Fatalf("plan response exposed the stored snapshot: %s", response.Body.String())
	}

	uid := uint32(1004)
	stub.plan.Operations[0] = store.OperationRecord{
		ID: "op-2", Action: "set_owner", SourcePath: "memo.txt", TargetPath: "memo.txt",
		OwnerUID: &uid, SourceStateJSON: string(snapshot), Approval: store.ApprovalPending, Status: store.OperationPending,
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("owner plan response status=%d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"beforeOwnerUid":1001`) || !strings.Contains(response.Body.String(), `"ownerUid":1004`) ||
		!strings.Contains(response.Body.String(), `"beforeGroupGid":1002`) || !strings.Contains(response.Body.String(), `"groupGid":1002`) {
		t.Fatalf("owner diff omitted unchanged group values: %s", response.Body.String())
	}
	if strings.Contains(response.Body.String(), "sourceStateJSON") || strings.Contains(response.Body.String(), "device") || strings.Contains(response.Body.String(), "inode") {
		t.Fatalf("owner plan response exposed the stored snapshot: %s", response.Body.String())
	}
}

func TestAgentPlanExposesApprovedArchiveOutputPaths(t *testing.T) {
	stub := &agentServiceStub{plan: store.PlanRecord{
		ID: "plan-archive", TaskID: "task-archive", Version: 2, Status: store.PlanAwaitingApproval,
		Operations: []store.OperationRecord{{
			ID: "op-extract", Ordinal: 0, Action: "extract_archive", SourcePath: "bundle.zip", TargetPath: "unpacked",
			TargetStateJSON: `{"plannedPaths":["unpacked/docs","unpacked/docs/readme.txt"]}`,
			Approval:        store.ApprovalPending, Status: store.OperationPending,
		}},
	}}
	handler := NewWithAgent(nil, nil, nil, stub, nil, nil)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/agent/plans/plan-archive", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("plan response status=%d body=%s", response.Code, response.Body.String())
	}
	var body struct {
		Operations []struct {
			PlannedPaths []string `json:"plannedPaths"`
		} `json:"operations"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || len(body.Operations) != 1 {
		t.Fatalf("plan response = (%s, %v)", response.Body.String(), err)
	}
	want := []string{"unpacked/docs", "unpacked/docs/readme.txt"}
	if len(body.Operations[0].PlannedPaths) != len(want) || body.Operations[0].PlannedPaths[0] != want[0] || body.Operations[0].PlannedPaths[1] != want[1] {
		t.Fatalf("planned paths = %#v, want %#v", body.Operations[0].PlannedPaths, want)
	}
	if strings.Contains(response.Body.String(), "targetStateJSON") {
		t.Fatalf("plan response exposed stored state: %s", response.Body.String())
	}
}
