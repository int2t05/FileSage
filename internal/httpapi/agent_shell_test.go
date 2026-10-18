// 本文件验证 Agent Shell HTTP 契约、请求边界与缓存策略。
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"filesage/internal/agent"
	"filesage/internal/store"
)

type shellHTTPStub struct {
	page          store.ShellChangePage
	preview       agent.ShellChangePreview
	plan          store.PlanRecord
	err           error
	listCalls     int
	previewCalls  int
	approvalCalls int
	planID        string
	changeID      string
	decision      string
	version       int
	limit         int
	offset        int
}

func (stub *shellHTTPStub) Plan(context.Context, string) (store.PlanRecord, error) {
	return stub.plan, stub.err
}
func (stub *shellHTTPStub) LatestPlan(context.Context, string) (store.PlanRecord, error) {
	return stub.plan, stub.err
}
func (stub *shellHTTPStub) RevisePlan(context.Context, string, int, []agent.Proposal) (store.PlanRecord, error) {
	return stub.plan, stub.err
}
func (stub *shellHTTPStub) Decide(context.Context, string, string, string, ...int) (store.PlanRecord, error) {
	return stub.plan, stub.err
}
func (stub *shellHTTPStub) Execute(context.Context, string, ...int) (store.PlanRecord, error) {
	return stub.plan, stub.err
}
func (stub *shellHTTPStub) StopExecution(context.Context, string, int) (store.PlanRecord, error) {
	return stub.plan, stub.err
}
func (stub *shellHTTPStub) ShellChanges(_ context.Context, planID string, limit, offset int) (store.ShellChangePage, error) {
	stub.listCalls++
	stub.planID, stub.limit, stub.offset = planID, limit, offset
	return stub.page, stub.err
}
func (stub *shellHTTPStub) PreviewShellChange(_ context.Context, planID, changeID string) (agent.ShellChangePreview, error) {
	stub.previewCalls++
	stub.planID, stub.changeID = planID, changeID
	return stub.preview, stub.err
}
func (stub *shellHTTPStub) DecideShellChange(_ context.Context, planID, changeID, decision string, version int) (store.PlanRecord, error) {
	stub.approvalCalls++
	stub.planID, stub.changeID, stub.decision, stub.version = planID, changeID, decision, version
	return stub.plan, stub.err
}

func TestListAgentShellChangesSerializesReviewContractAndDisablesCache(t *testing.T) {
	mode := uint32(0o640)
	stub := &shellHTTPStub{page: store.ShellChangePage{
		Changes: []store.ShellChangeRecord{{
			ID: "change-1", Path: "docs/readme.md", Kind: "modified",
			OriginalStateJSON: `{"fingerprint":{"exists":true},"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","mode":420,"isDirectory":false,"size":10}`,
			CandidateSHA256:   "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", CandidateSize: 14,
			CandidateMode: &mode, Binary: false, Truncated: true, Approval: store.ApprovalPending,
			Status: store.OperationPending,
		}}, Total: 2, Limit: 1, Offset: 1, HasMore: false,
	}}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/agent/plans/plan-1/shell-changes?limit=1&offset=1", nil)
	request.SetPathValue("planId", "plan-1")
	response := httptest.NewRecorder()
	(&API{agent: stub}).listAgentShellChanges(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" {
		t.Fatalf("response=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	if stub.listCalls != 1 || stub.planID != "plan-1" || stub.limit != 1 || stub.offset != 1 {
		t.Fatalf("unexpected list call: %#v", stub)
	}
	var body struct {
		Changes []map[string]json.RawMessage `json:"changes"`
		Offset  int                          `json:"offset"`
		Limit   int                          `json:"limit"`
		Total   int                          `json:"total"`
		HasMore bool                         `json:"hasMore"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Offset != 1 || body.Limit != 1 || body.Total != 2 || body.HasMore || len(body.Changes) != 1 {
		t.Fatalf("unexpected page: %#v", body)
	}
	change := body.Changes[0]
	for _, name := range []string{"id", "path", "kind", "approval", "status", "binary", "diffTruncated", "before", "after"} {
		if _, ok := change[name]; !ok {
			t.Fatalf("change is missing %q: %#v", name, change)
		}
	}
	if _, ok := change["command"]; ok {
		t.Fatalf("change leaked command data: %#v", change)
	}
	var before, after map[string]any
	if err := json.Unmarshal(change["before"], &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(change["after"], &after); err != nil {
		t.Fatal(err)
	}
	if before["sha256"] != strings.Repeat("a", 64) || before["mode"] != float64(420) || before["size"] != float64(10) || before["isDirectory"] != false {
		t.Fatalf("wrong before state: %#v", before)
	}
	if after["sha256"] != strings.Repeat("b", 64) || after["mode"] != float64(0o640) || after["size"] != float64(14) || after["isDirectory"] != false {
		t.Fatalf("wrong after state: %#v", after)
	}
}

func TestAgentShellListRejectsInvalidQueryBeforeCallingService(t *testing.T) {
	for _, query := range []string{"?limit=101", "?offset=-1", "?unknown=1", "?limit=1&limit=2", "?offset="} {
		t.Run(query, func(t *testing.T) {
			stub := &shellHTTPStub{}
			request := httptest.NewRequest(http.MethodGet, "/api/v1/agent/plans/plan-1/shell-changes"+query, nil)
			request.SetPathValue("planId", "plan-1")
			response := httptest.NewRecorder()
			(&API{agent: stub}).listAgentShellChanges(response, request)
			if response.Code != http.StatusBadRequest || response.Header().Get("Cache-Control") != "no-store" || stub.listCalls != 0 {
				t.Fatalf("response=%d calls=%d headers=%v body=%s", response.Code, stub.listCalls, response.Header(), response.Body.String())
			}
		})
	}
}

func TestPreviewAgentShellChangeIsNoStoreAndReturnsTextOnlyOnDemand(t *testing.T) {
	mode := uint32(0o644)
	stub := &shellHTTPStub{preview: agent.ShellChangePreview{
		Change: store.ShellChangeRecord{
			ID: "change-1", Path: "note.txt", Kind: "added",
			OriginalStateJSON: `{"fingerprint":{"exists":false},"sha256":"","mode":0,"isDirectory":false,"size":0}`,
			CandidateSHA256:   "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", CandidateSize: 5,
			CandidateMode: &mode, Approval: store.ApprovalPending, Status: store.OperationPending,
		}, Diff: "+hello",
	}}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/agent/plans/plan-1/shell-changes/change-1/preview", nil)
	request.SetPathValue("planId", "plan-1")
	request.SetPathValue("changeId", "change-1")
	response := httptest.NewRecorder()
	(&API{agent: stub}).previewAgentShellChange(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" || stub.previewCalls != 1 {
		t.Fatalf("response=%d calls=%d headers=%v body=%s", response.Code, stub.previewCalls, response.Header(), response.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if string(body["textDiff"]) != `"+hello"` || len(body["change"]) == 0 || strings.Contains(response.Body.String(), "command output") {
		t.Fatalf("unexpected preview payload: %s", response.Body.String())
	}
}

func TestAgentShellApprovalValidatesRequestAndReturnsStandardPlan(t *testing.T) {
	stub := &shellHTTPStub{plan: store.PlanRecord{
		ID: "plan-1", TaskID: "task-1", Version: 4, Status: store.PlanAwaitingApproval,
		Operations: []store.OperationRecord{},
	}}
	request := httptest.NewRequest(http.MethodPost, "http://filesage.local/api/v1/agent/plans/plan-1/shell-changes/change-1/approval", strings.NewReader(`{"decision":"approve","expectedPlanVersion":3}`))
	request.Header.Set("Origin", "http://filesage.local")
	request.SetPathValue("planId", "plan-1")
	request.SetPathValue("changeId", "change-1")
	response := httptest.NewRecorder()
	(&API{agent: stub}).decideAgentShellChange(response, request)
	if response.Code != http.StatusOK || stub.approvalCalls != 1 || stub.decision != "approve" || stub.version != 3 {
		t.Fatalf("response=%d calls=%d decision=%q version=%d body=%s", response.Code, stub.approvalCalls, stub.decision, stub.version, response.Body.String())
	}
	var plan map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"taskId", "planId", "planVersion", "status", "stopRequested", "proposalSource", "operations"} {
		if _, ok := plan[name]; !ok {
			t.Fatalf("standard plan response missing %q: %s", name, response.Body.String())
		}
	}
}

func TestAgentShellApprovalRejectsInvalidBodiesAndCrossOrigin(t *testing.T) {
	for _, test := range []struct {
		name   string
		body   string
		origin string
		status int
	}{
		{name: "decision", body: `{"decision":"maybe","expectedPlanVersion":1}`, status: http.StatusBadRequest},
		{name: "version", body: `{"decision":"approve","expectedPlanVersion":0}`, status: http.StatusBadRequest},
		{name: "unknown-field", body: `{"decision":"approve","expectedPlanVersion":1,"actor":"admin"}`, status: http.StatusBadRequest},
		{name: "cross-origin", body: `{"decision":"approve","expectedPlanVersion":1}`, origin: "https://elsewhere.example", status: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			stub := &shellHTTPStub{}
			request := httptest.NewRequest(http.MethodPost, "http://filesage.local/api/v1/agent/plans/plan-1/shell-changes/change-1/approval", strings.NewReader(test.body))
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			request.SetPathValue("planId", "plan-1")
			request.SetPathValue("changeId", "change-1")
			response := httptest.NewRecorder()
			(&API{agent: stub}).decideAgentShellChange(response, request)
			if response.Code != test.status || stub.approvalCalls != 0 {
				t.Fatalf("response=%d calls=%d body=%s", response.Code, stub.approvalCalls, response.Body.String())
			}
		})
	}
}

func TestAgentShellServiceErrorsUseAgentErrorMapping(t *testing.T) {
	stub := &shellHTTPStub{err: store.ErrNotFound}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/agent/plans/plan-1/shell-changes", nil)
	request.SetPathValue("planId", "plan-1")
	response := httptest.NewRecorder()
	(&API{agent: stub}).listAgentShellChanges(response, request)
	if response.Code != http.StatusNotFound || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("response=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	stub.err = errors.New("hidden storage failure")
	request = httptest.NewRequest(http.MethodGet, "/api/v1/agent/plans/plan-1/shell-changes/change-1/preview", nil)
	request.SetPathValue("planId", "plan-1")
	request.SetPathValue("changeId", "change-1")
	response = httptest.NewRecorder()
	(&API{agent: stub}).previewAgentShellChange(response, request)
	if response.Code != http.StatusInternalServerError || response.Header().Get("Cache-Control") != "no-store" || strings.Contains(response.Body.String(), "hidden storage failure") {
		t.Fatalf("response=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
}
