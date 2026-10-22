// 本文件验证模型请求格式、连接边界和错误脱敏。
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"filesage/internal/config"
)

func TestChatClientUsesOpenAICompatibleJSONAndKeepsKeyPrivate(t *testing.T) {
	const secret = "test-secret-never-returned"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+secret {
			t.Errorf("authorization header = %q", got)
		}
		var request struct {
			Model          string        `json:"model"`
			Messages       []ChatMessage `json:"messages"`
			Stream         bool          `json:"stream"`
			Temperature    float64       `json:"temperature"`
			ResponseFormat struct {
				Type string `json:"type"`
			} `json:"response_format"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if request.Model != "local-model" || len(request.Messages) != 2 || request.Stream || request.Temperature != 0 || request.ResponseFormat.Type != "json_object" {
			t.Errorf("request shape = %#v", request)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"state\":\"clarifying\",\"reply\":\"请补充路径\",\"goal\":\"\",\"filePath\":\"\"}"}}]}`))
	}))
	defer server.Close()
	client := newChatClient(config.AgentModelConfig{BaseURL: server.URL + "/v1", Model: "local-model", Provider: "ollama", APIKey: secret})
	if !client.Status().Available || client.Status().Destination != "local" {
		t.Fatalf("status = %#v", client.Status())
	}
	response, err := client.complete(context.Background(), []ChatMessage{{Role: "system", Content: "strict JSON"}, {Role: "user", Content: "rename"}})
	if err != nil || !strings.Contains(response, `"clarifying"`) {
		t.Fatalf("complete() = (%q, %v)", response, err)
	}
	status, err := json.Marshal(client.Status())
	if err != nil || strings.Contains(string(status), secret) || strings.Contains(string(status), server.URL) {
		t.Fatalf("status exposed private configuration: %s (%v)", status, err)
	}
}

func TestChatClientDoesNotFollowRedirectOrExposeProviderBody(t *testing.T) {
	const secret = "provider-secret"
	var redirected bool
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected = true }))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", destination.URL)
		w.WriteHeader(http.StatusFound)
		_, _ = fmt.Fprintf(w, "upstream echoed %s", secret)
	}))
	defer server.Close()
	client := newChatClient(config.AgentModelConfig{BaseURL: server.URL, Model: "model", APIKey: secret})
	_, err := client.complete(context.Background(), []ChatMessage{{Role: "user", Content: "test"}})
	if !errors.Is(err, ErrModelRequest) || strings.Contains(err.Error(), secret) || redirected {
		t.Fatalf("complete error = %v, redirected = %t", err, redirected)
	}
	class, status := modelFailureLogDetails(err)
	if class != "http_status" || status != http.StatusFound {
		t.Fatalf("failure details = (%q, %d), want redirect status without provider body", class, status)
	}
}

func TestChatClientClassifiesTransportFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := server.URL
	server.Close()
	client := newChatClient(config.AgentModelConfig{BaseURL: url, Model: "model"})
	_, err := client.complete(context.Background(), []ChatMessage{{Role: "user", Content: "test"}})
	if !errors.Is(err, ErrModelRequest) {
		t.Fatalf("complete error = %v, want model request failure", err)
	}
	class, status := modelFailureLogDetails(err)
	if class != "transport" || status != 0 {
		t.Fatalf("failure details = (%q, %d), want transport failure without HTTP status", class, status)
	}
}

func TestChatEndpointRequiresHTTPSForRemoteHosts(t *testing.T) {
	for _, test := range []struct {
		url string
		ok  bool
	}{
		{"http://127.0.0.1:11434/v1", true},
		{"http://localhost:11434/v1", true},
		{"http://example.com/v1", false},
		{"https://example.com/v1", true},
		{"https://user:pass@example.com/v1", false},
		{"https://example.com/v1?token=secret", false},
		{"ftp://127.0.0.1/v1", false},
		{"https://example.com/a/../v1", false},
	} {
		t.Run(test.url, func(t *testing.T) {
			_, _, ok := chatEndpoint(test.url)
			if ok != test.ok {
				t.Fatalf("chatEndpoint(%q) accepted = %t, want %t", test.url, ok, test.ok)
			}
		})
	}
}

func TestChatClientBoundsProviderResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"` + strings.Repeat("x", maxModelResponseBytes) + `"}}]}`))
	}))
	defer server.Close()
	client := newChatClient(config.AgentModelConfig{BaseURL: server.URL, Model: "model"})
	_, err := client.complete(context.Background(), []ChatMessage{{Role: "user", Content: "test"}})
	if !errors.Is(err, ErrModelResponse) {
		t.Fatalf("oversized response error = %v", err)
	}
}

func TestTextEditChatUsesSeparateBoundedModelResponseLimit(t *testing.T) {
	candidate := strings.Repeat("x", 20<<10)
	modelContent, err := json.Marshal(textEditAnalysisResult{Supported: true, ProposedContent: candidate})
	if err != nil {
		t.Fatal(err)
	}
	providerBody, err := json.Marshal(struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}{Choices: []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}{{Message: struct {
		Content string `json:"content"`
	}{Content: string(modelContent)}}}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(providerBody)
	}))
	defer server.Close()
	client := newChatClient(config.AgentModelConfig{BaseURL: server.URL, Model: "model"})
	messages := []ChatMessage{{Role: "user", Content: "generate a text candidate"}}
	if _, err := client.complete(context.Background(), messages); !errors.Is(err, ErrModelResponse) {
		t.Fatalf("general model response error = %v, want bounded rejection", err)
	}
	content, err := client.completeTextEdit(context.Background(), messages)
	if err != nil || len(content) <= maxModelContentBytes {
		t.Fatalf("text edit response length=%d error=%v", len(content), err)
	}
}

func TestValidTextEditCandidateBounds(t *testing.T) {
	for _, test := range []struct {
		name    string
		content []byte
		valid   bool
	}{
		{name: "empty"},
		{name: "within limit", content: []byte(strings.Repeat("中", maxTextEditBytes/3)), valid: true},
		{name: "exact limit", content: []byte(strings.Repeat("x", maxTextEditBytes)), valid: true},
		{name: "over limit", content: []byte(strings.Repeat("x", maxTextEditBytes+1))},
		{name: "invalid utf8", content: []byte{0xff}},
		{name: "nul", content: []byte("a\x00b")},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := validTextEditCandidate(test.content); got != test.valid {
				t.Fatalf("validTextEditCandidate() = %t, want %t", got, test.valid)
			}
		})
	}
}

func TestValidateProposalAcceptsSamePathEditText(t *testing.T) {
	proposal := Proposal{
		RequestText: "润色这份笔记", Action: "edit_text", SourcePath: "notes.md", TargetPath: "notes.md",
		Reason: "按用户确认的目标编辑正文", ProposedContent: []byte("更新后的正文"),
	}
	if err := validateProposal(proposal); err != nil {
		t.Fatalf("validateProposal(edit_text) = %v", err)
	}

	for _, invalid := range []Proposal{
		{RequestText: proposal.RequestText, Action: "edit_text", SourcePath: "notes.md", TargetPath: "other.md", Reason: proposal.Reason, ProposedContent: proposal.ProposedContent},
		{RequestText: proposal.RequestText, ScopePath: "documents", Action: "edit_text", SourcePath: "notes.md", TargetPath: "notes.md", Reason: proposal.Reason, ProposedContent: proposal.ProposedContent},
		{RequestText: proposal.RequestText, Action: "edit_text", SourcePath: "notes.pdf", TargetPath: "notes.pdf", Reason: proposal.Reason, ProposedContent: proposal.ProposedContent},
	} {
		if err := validateProposal(invalid); !errors.Is(err, ErrInvalid) {
			t.Fatalf("validateProposal(invalid edit_text) = %v, want ErrInvalid", err)
		}
	}
}

func TestStrictModelJSONRejectsUnknownAndTrailingData(t *testing.T) {
	for _, raw := range []string{
		`{"state":"clarifying","reply":"补充路径","goal":"","filePath":""}`,
		`{"state":"clarifying","reply":"补充路径","goal":"","filePath":"","unexpected":true}`,
		`{"state":"clarifying","state":"ready","reply":"补充路径","goal":"","filePath":""}`,
		`{"state":"clarifying","reply":"补充路径","goal":"","filePath":""}{}`,
	} {
		var result conversationTurnResult
		err := decodeStrictModelJSON(raw, &result)
		if strings.Contains(raw, "unexpected") || strings.Contains(raw, `"state":"clarifying","state"`) || strings.HasSuffix(raw, "{}") {
			if err == nil {
				t.Fatalf("accepted invalid model JSON: %s", raw)
			}
		} else if err != nil {
			t.Fatalf("rejected valid model JSON: %v", err)
		}
	}
}
