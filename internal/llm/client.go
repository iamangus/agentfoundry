package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// Default base URL — OpenRouter is the default for backward compatibility.
const defaultBaseURL = "https://openrouter.ai/api/v1"

// Client is the interface for LLM providers.
type Client interface {
	ChatCompletion(ctx context.Context, req *ChatRequest) (*ChatResponse, error)
}

// ClientConfig holds configuration for an OpenAI-compatible LLM client.
type ClientConfig struct {
	// BaseURL is the API base URL (e.g. "https://openrouter.ai/api/v1",
	// "https://api.openai.com/v1"). The client appends "/chat/completions".
	BaseURL string

	// APIKey is the bearer token for authentication.
	APIKey string

	// DefaultModel is used when a request doesn't specify a model.
	DefaultModel string

	// Headers are extra HTTP headers sent with every request.
	// Useful for provider-specific headers like OpenRouter's HTTP-Referer / X-Title.
	Headers map[string]string
}

// OpenAIClient implements Client for any OpenAI-compatible API.
type OpenAIClient struct {
	baseURL      string
	apiKey       string
	defaultModel string
	headers      map[string]string
	httpClient   *http.Client
}

// NewClient creates a new OpenAI-compatible LLM client.
func NewClient(cfg ClientConfig) *OpenAIClient {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &OpenAIClient{
		baseURL:      baseURL,
		apiKey:       cfg.APIKey,
		defaultModel: cfg.DefaultModel,
		headers:      cfg.Headers,
		httpClient:   &http.Client{Timeout: 120 * time.Second},
	}
}

// NewOpenRouterClient creates a client pre-configured for OpenRouter.
// Kept for backward compatibility.
func NewOpenRouterClient(apiKey, defaultModel string) *OpenAIClient {
	return NewClient(ClientConfig{
		BaseURL:      "https://openrouter.ai/api/v1",
		APIKey:       apiKey,
		DefaultModel: defaultModel,
		Headers: map[string]string{
			"HTTP-Referer": "https://github.com/angoo/agentfile",
			"X-Title":      "agentfile",
		},
	})
}

// ResponseFormat instructs the model to produce output in a specific format.
type ResponseFormat struct {
	Type       string      `json:"type"`
	JSONSchema *JSONSchema `json:"json_schema,omitempty"`
}

// JSONSchema is the json_schema block within a ResponseFormat.
// It mirrors the OpenAI structured outputs format exactly.
type JSONSchema struct {
	Name   string          `json:"name"`
	Schema json.RawMessage `json:"schema"`
	Strict bool            `json:"strict"`
}

// ChatRequest represents a chat completion request.
type ChatRequest struct {
	Model          string          `json:"model"`
	Messages       []Message       `json:"messages"`
	Tools          []ToolDef       `json:"tools,omitempty"`
	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`
}

// Message represents a chat message.
type Message struct {
	Role       string     `json:"role"`
	Content    any        `json:"content,omitempty"` // string or nil
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolDef represents a tool definition for the LLM.
type ToolDef struct {
	Type     string      `json:"type"`
	Function FunctionDef `json:"function"`
}

// FunctionDef represents a function definition within a tool.
type FunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ToolCall represents a tool call made by the LLM.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall represents the function portion of a tool call.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ChatResponse represents a chat completion response.
type ChatResponse struct {
	ID      string   `json:"id"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
}

// Choice represents a response choice.
type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

// Usage represents token usage.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

func isEmptyResponse(resp *ChatResponse) bool {
	if len(resp.Choices) == 0 {
		return true
	}
	for _, c := range resp.Choices {
		if c.FinishReason == "length" {
			return true
		}
		if len(c.Message.ToolCalls) > 0 {
			return false
		}
		if c.Message.Content != nil {
			if s, ok := c.Message.Content.(string); ok && s != "" {
				return false
			}
		}
	}
	return true
}

func isRetryableHTTP(statusCode int) bool {
	switch statusCode {
	case http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	}
	return false
}

// ChatCompletion sends a chat completion request to the configured API endpoint.
// Transient errors (429, 500, 502, 503, 504) and network errors are retried
// with exponential backoff: 1s, 3s, 7s, then every 10s up to maxRetries.
func (c *OpenAIClient) ChatCompletion(ctx context.Context, req *ChatRequest) (*ChatResponse, error) {
	if req.Model == "" {
		req.Model = c.defaultModel
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	backoffSchedule := []time.Duration{
		1 * time.Second,
		3 * time.Second,
		7 * time.Second,
	}
	const defaultBackoff = 10 * time.Second
	const maxRetries = 10

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			var wait time.Duration
			if attempt-1 < len(backoffSchedule) {
				wait = backoffSchedule[attempt-1]
			} else {
				wait = defaultBackoff
			}
			slog.Warn("LLM request failed, retrying",
				"attempt", attempt,
				"wait", wait,
				"error", lastErr,
			)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
		}

		httpReq, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}

		httpReq.Header.Set("Content-Type", "application/json")
		if c.apiKey != "" {
			httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
		}
		for k, v := range c.headers {
			httpReq.Header.Set(k, v)
		}

		resp, err := c.httpClient.Do(httpReq)
		if err != nil {
			lastErr = fmt.Errorf("http request: %w", err)
			continue
		}

		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("read response: %w", err)
			continue
		}

		if resp.StatusCode == http.StatusOK {
			var chatResp ChatResponse
			if err := json.Unmarshal(respBody, &chatResp); err != nil {
				return nil, fmt.Errorf("parse response: %w", err)
			}
			if isEmptyResponse(&chatResp) {
				lastErr = fmt.Errorf("LLM returned empty response (no content, no tool calls, or finish_reason=length)")
				continue
			}
			return &chatResp, nil
		}

		lastErr = fmt.Errorf("LLM API error %d: %s", resp.StatusCode, string(respBody))
		if !isRetryableHTTP(resp.StatusCode) {
			return nil, lastErr
		}
	}

	return nil, fmt.Errorf("LLM request failed after %d retries: %w", maxRetries, lastErr)
}
