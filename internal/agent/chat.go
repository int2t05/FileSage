// 本文件封装受限的 OpenAI-compatible Chat Completions 客户端。
package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"filesage/internal/config"
)

const (
	maxModelRequestBytes  = 512 << 10
	maxModelResponseBytes = 128 << 10
	maxModelContentBytes  = 16 << 10
	// modelRequestTimeout 为较慢的模型 JSON 生成留出时间。
	modelRequestTimeout = 90 * time.Second
)

var (
	ErrModelUnavailable = errors.New("agent model is unavailable")
	ErrModelRequest     = errors.New("agent model request failed")
	ErrModelResponse    = errors.New("agent model response is invalid")
	ErrMessageTooLarge  = errors.New("agent conversation message is too large")
)

// modelRequestFailure 只记录安全的失败类别和 HTTP 状态码。
type modelRequestFailure struct {
	class  string
	status int
}

func (e *modelRequestFailure) Error() string { return ErrModelRequest.Error() }

func (e *modelRequestFailure) Unwrap() error { return ErrModelRequest }

func modelFailureLogDetails(err error) (string, int) {
	var requestFailure *modelRequestFailure
	if errors.As(err, &requestFailure) {
		return requestFailure.class, requestFailure.status
	}
	if errors.Is(err, ErrModelResponse) {
		return "invalid_response", 0
	}
	if errors.Is(err, ErrModelUnavailable) {
		return "model_unavailable", 0
	}
	return "unknown", 0
}

type ModelStatus struct {
	Available   bool   `json:"available"`
	Provider    string `json:"provider,omitempty"`
	Model       string `json:"model,omitempty"`
	Destination string `json:"destination,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatClient struct {
	status   ModelStatus
	endpoint string
	apiKey   string
	http     *http.Client
}

// newChatClient 验证配置并建立不可重定向的短超时客户端。
func newChatClient(cfg config.AgentModelConfig) *chatClient {
	status := ModelStatus{Reason: "not_configured"}
	baseURL, model := strings.TrimSpace(cfg.BaseURL), strings.TrimSpace(cfg.Model)
	if baseURL == "" && model == "" {
		return &chatClient{status: status}
	}
	if baseURL == "" || model == "" {
		status.Reason = "incomplete_configuration"
		return &chatClient{status: status}
	}
	provider := strings.TrimSpace(cfg.Provider)
	if provider == "" {
		provider = "openai-compatible"
	}
	if !validModelText(provider, 128) || !validModelText(model, 256) || len(cfg.APIKey) > 8192 {
		status.Reason = "invalid_configuration"
		return &chatClient{status: status}
	}
	endpoint, destination, ok := chatEndpoint(baseURL)
	if !ok {
		status.Reason = "invalid_configuration"
		return &chatClient{status: status}
	}
	return &chatClient{
		status:   ModelStatus{Available: true, Provider: provider, Model: model, Destination: destination},
		endpoint: endpoint,
		apiKey:   cfg.APIKey,
		http: &http.Client{
			Timeout:       modelRequestTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

func chatEndpoint(raw string) (string, string, bool) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Opaque != "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.ForceQuery {
		return "", "", false
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return "", "", false
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" || parsed.Port() == "0" || strings.Contains(parsed.Host, "\\") {
		return "", "", false
	}
	destination := "remote"
	local := host == "localhost"
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		local = true
	}
	if parsed.Scheme == "http" && !local {
		return "", "", false
	}
	if local {
		destination = "local"
	}
	for _, segment := range strings.Split(parsed.Path, "/") {
		if segment == ".." || segment == "." {
			return "", "", false
		}
	}
	basePath := strings.TrimRight(parsed.Path, "/")
	if strings.HasSuffix(basePath, "/chat/completions") {
		return "", "", false
	}
	parsed.Path = basePath + "/chat/completions"
	parsed.RawPath = ""
	return parsed.String(), destination, true
}

func (c *chatClient) Status() ModelStatus {
	return c.status
}

func (c *chatClient) complete(ctx context.Context, messages []ChatMessage) (string, error) {
	return c.completeWithLimits(ctx, messages, maxModelRequestBytes, maxModelResponseBytes, maxModelContentBytes, 64<<10)
}

// completeTextEdit 为正文分析单独扩大传输边界，不改变其他模型请求上限。
func (c *chatClient) completeTextEdit(ctx context.Context, messages []ChatMessage) (string, error) {
	return c.completeWithLimits(ctx, messages, 512<<10, 512<<10, 256<<10, 256<<10)
}

// completeStream 以 OpenAI SSE 协议读取对话响应，回调只接收模型内容片段。
func (c *chatClient) completeStream(ctx context.Context, messages []ChatMessage, onContent func(string)) (string, error) {
	if c == nil || !c.status.Available {
		return "", ErrModelUnavailable
	}
	if len(messages) == 0 {
		return "", ErrModelResponse
	}
	for _, message := range messages {
		if (message.Role != "system" && message.Role != "user" && message.Role != "assistant") || message.Content == "" || len(message.Content) > 64<<10 {
			return "", ErrModelResponse
		}
	}
	requestBody, err := json.Marshal(struct {
		Model          string        `json:"model"`
		Messages       []ChatMessage `json:"messages"`
		Stream         bool          `json:"stream"`
		Temperature    float64       `json:"temperature"`
		ResponseFormat struct {
			Type string `json:"type"`
		} `json:"response_format"`
	}{Model: c.status.Model, Messages: messages, Stream: true, Temperature: 0, ResponseFormat: struct {
		Type string `json:"type"`
	}{Type: "json_object"}})
	if err != nil || len(requestBody) > maxModelRequestBytes {
		return "", ErrModelResponse
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(requestBody))
	if err != nil {
		return "", &modelRequestFailure{class: "request_build"}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	if c.apiKey != "" {
		request.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return "", &modelRequestFailure{class: "transport"}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", &modelRequestFailure{class: "http_status", status: response.StatusCode}
	}
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 4096), maxModelResponseBytes+1)
	var content strings.Builder
	responseBytes := 0
	for scanner.Scan() {
		line := scanner.Bytes()
		responseBytes += len(line) + 1
		if responseBytes > maxModelResponseBytes {
			return "", ErrModelResponse
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(line[len("data:"):])
		if bytes.Equal(data, []byte("[DONE]")) {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(data, &chunk); err != nil {
			return "", ErrModelResponse
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		piece := chunk.Choices[0].Delta.Content
		if len(content.String())+len(piece) > maxModelContentBytes {
			return "", ErrModelResponse
		}
		content.WriteString(piece)
		if piece != "" && onContent != nil {
			onContent(piece)
		}
	}
	if err := scanner.Err(); err != nil || content.Len() == 0 {
		return "", ErrModelResponse
	}
	return content.String(), nil
}

// completeClassification 单独放宽传输包络，正文总量仍由分类流程限制在 256 KiB。
func (c *chatClient) completeClassification(ctx context.Context, messages []ChatMessage) (string, error) {
	return c.completeWithLimits(ctx, messages, 2<<20, 128<<10, 16<<10, 2<<20)
}

func (c *chatClient) completeWithLimits(ctx context.Context, messages []ChatMessage, maxRequest, maxResponse, maxContent, maxMessage int) (string, error) {
	if c == nil || !c.status.Available {
		return "", ErrModelUnavailable
	}
	if len(messages) == 0 {
		return "", ErrModelResponse
	}
	for _, message := range messages {
		if (message.Role != "system" && message.Role != "user" && message.Role != "assistant") || message.Content == "" || len(message.Content) > maxMessage {
			return "", ErrModelResponse
		}
	}
	requestBody, err := json.Marshal(struct {
		Model          string        `json:"model"`
		Messages       []ChatMessage `json:"messages"`
		Stream         bool          `json:"stream"`
		Temperature    float64       `json:"temperature"`
		ResponseFormat struct {
			Type string `json:"type"`
		} `json:"response_format"`
	}{Model: c.status.Model, Messages: messages, Stream: false, Temperature: 0, ResponseFormat: struct {
		Type string `json:"type"`
	}{Type: "json_object"}})
	if err != nil || len(requestBody) > maxRequest {
		return "", ErrModelResponse
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(requestBody))
	if err != nil {
		return "", &modelRequestFailure{class: "request_build"}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		request.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return "", &modelRequestFailure{class: "transport"}
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, int64(maxResponse)+1))
	if err != nil || len(body) > maxResponse {
		return "", ErrModelResponse
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", &modelRequestFailure{class: "http_status", status: response.StatusCode}
	}
	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &result); err != nil || len(result.Choices) == 0 {
		return "", ErrModelResponse
	}
	content := result.Choices[0].Message.Content
	if content == "" || len(content) > maxContent {
		return "", ErrModelResponse
	}
	return content, nil
}

func (s ModelStatus) String() string {
	if !s.Available {
		return fmt.Sprintf("unavailable: %s", s.Reason)
	}
	return s.Provider + "/" + s.Model
}

func validModelText(value string, maximum int) bool {
	if len(value) == 0 || len(value) > maximum || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
