// Package router proxies OpenAI-compatible chat completions to multiple
// upstream providers (OpenAI, Anthropic, Gemini, Ollama, MockLLM), translating
// wire formats, streaming via SSE, and failing over between providers behind
// per-provider circuit breakers.
package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// ChatMessage is an OpenAI-style message. Content accepts either a string or
// an array of {type:"text", text:"..."} parts, flattened to a string.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	Name    string `json:"name,omitempty"`
}

// UnmarshalJSON accepts string or multi-part content.
func (m *ChatMessage) UnmarshalJSON(b []byte) error {
	var raw struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
		Name    string          `json:"name"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	m.Role, m.Name = raw.Role, raw.Name
	trimmed := bytes.TrimSpace(raw.Content)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		m.Content = ""
		return nil
	}
	if trimmed[0] == '"' {
		return json.Unmarshal(trimmed, &m.Content)
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(trimmed, &parts); err != nil {
		return fmt.Errorf("message content must be a string or array of text parts: %w", err)
	}
	var sb strings.Builder
	for _, p := range parts {
		if p.Type == "text" || p.Type == "" {
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(p.Text)
		}
	}
	m.Content = sb.String()
	return nil
}

// ChatRequest is the OpenAI /v1/chat/completions request body.
type ChatRequest struct {
	Model       string        `json:"model"`
	Messages    []ChatMessage `json:"messages"`
	Stream      bool          `json:"stream,omitempty"`
	Temperature *float64      `json:"temperature,omitempty"`
	TopP        *float64      `json:"top_p,omitempty"`
	MaxTokens   *int          `json:"max_tokens,omitempty"`
	Stop        []string      `json:"stop,omitempty"`
	User        string        `json:"user,omitempty"`
}

// Usage is token accounting.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Choice is a non-streaming completion choice.
type Choice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

// ChatResponse is the OpenAI chat.completion response.
type ChatResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
}

// StreamDelta is the delta inside a streaming chunk.
type StreamDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

// StreamChoice is a streaming choice.
type StreamChoice struct {
	Index        int         `json:"index"`
	Delta        StreamDelta `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
}

// StreamChunk is an OpenAI chat.completion.chunk.
type StreamChunk struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []StreamChoice `json:"choices"`
	Usage   *Usage         `json:"usage,omitempty"`
}

// Completion is a provider-neutral non-streaming result.
type Completion struct {
	Content      string
	FinishReason string
	Model        string
	Usage        Usage
}

// Delta is a provider-neutral streaming event.
type Delta struct {
	Content      string
	FinishReason string
	Usage        *Usage
}

// Stream yields Deltas until io.EOF.
type Stream interface {
	Recv() (Delta, error)
	Close() error
}

// CallOptions carries per-call extras.
type CallOptions struct {
	// Headers are forwarded verbatim to the upstream (e.g. X-Mock-Fail for
	// chaos testing against the mock upstream).
	Headers http.Header
}

// Provider is an upstream LLM API adapter.
type Provider interface {
	Name() string
	Type() string
	Complete(ctx context.Context, req *ChatRequest, opts CallOptions) (*Completion, error)
	Stream(ctx context.Context, req *ChatRequest, opts CallOptions) (Stream, error)
}

// UpstreamError describes a failed upstream call.
type UpstreamError struct {
	Provider string
	Status   int // 0 for transport errors
	Body     string
	Timeout  bool
	Err      error
}

func (e *UpstreamError) Error() string {
	switch {
	case e.Timeout:
		return fmt.Sprintf("%s: upstream timeout", e.Provider)
	case e.Status > 0:
		return fmt.Sprintf("%s: upstream status %d: %s", e.Provider, e.Status, truncate(e.Body, 200))
	default:
		return fmt.Sprintf("%s: upstream transport error: %v", e.Provider, e.Err)
	}
}

func (e *UpstreamError) Unwrap() error { return e.Err }

// Retryable reports whether the error should trigger failover: 5xx, 429,
// timeouts, and transport failures. Other 4xx are client errors.
func (e *UpstreamError) Retryable() bool {
	return e.Status == 0 || e.Status >= 500 || e.Status == http.StatusTooManyRequests || e.Timeout
}

// IsRetryable reports whether err should trigger failover.
func IsRetryable(err error) bool {
	var ue *UpstreamError
	if errors.As(err, &ue) {
		return ue.Retryable()
	}
	if errors.Is(err, ErrCircuitOpen) {
		return true
	}
	return !errors.Is(err, context.Canceled)
}

func classify(provider string, err error) error {
	if err == nil {
		return nil
	}
	var ue *UpstreamError
	if errors.As(err, &ue) {
		return err
	}
	var ne net.Error
	timeout := errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
	return &UpstreamError{Provider: provider, Timeout: timeout, Err: err}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// EstimateTokens is a fallback when an upstream omits usage.
func EstimateTokens(s string) int {
	if s == "" {
		return 0
	}
	return (len(s) + 3) / 4
}

func nowUnix() int64 { return time.Now().Unix() }
