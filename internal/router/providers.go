package router

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
	"os"
	"strings"
	"time"
)

// ProviderSpec configures an HTTP provider adapter.
type ProviderSpec struct {
	Name         string
	Type         string // openai | ollama | mockllm | anthropic | gemini
	BaseURL      string
	APIKey       string
	Timeout      time.Duration
	DefaultModel string
	Query        map[string]string
	Headers      map[string]string
	// ExtraBody fields are added to OpenAI-compatible request bodies unless the
	// request already sets them (e.g. reasoning_effort: none for local models).
	ExtraBody map[string]any
}

// NewProvider builds the adapter for spec.Type.
func NewProvider(spec ProviderSpec, client *http.Client) (Provider, error) {
	if client == nil {
		client = DefaultHTTPClient()
	}
	if spec.Timeout <= 0 {
		spec.Timeout = 60 * time.Second
	}
	base := &httpProvider{spec: spec, client: client}
	switch spec.Type {
	case "openai", "ollama", "mockllm":
		return &openAIProvider{base}, nil
	case "anthropic":
		return &anthropicProvider{base}, nil
	case "gemini":
		return &geminiProvider{base}, nil
	}
	return nil, fmt.Errorf("unknown provider type %q", spec.Type)
}

// NewProviderFromEnv resolves the API key from apiKeyEnv.
func NewProviderFromEnv(spec ProviderSpec, apiKeyEnv string, client *http.Client) (Provider, error) {
	if apiKeyEnv != "" && spec.APIKey == "" {
		spec.APIKey = os.Getenv(apiKeyEnv)
	}
	return NewProvider(spec, client)
}

// DefaultHTTPClient is tuned for high-concurrency upstream fan-out.
func DefaultHTTPClient() *http.Client {
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          2048,
		MaxIdleConnsPerHost:   1024,
		MaxConnsPerHost:       0,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &http.Client{Transport: tr}
}

type httpProvider struct {
	spec   ProviderSpec
	client *http.Client
}

func (p *httpProvider) Name() string { return p.spec.Name }
func (p *httpProvider) Type() string { return p.spec.Type }

func (p *httpProvider) model(req *ChatRequest) string {
	if req.Model != "" {
		return req.Model
	}
	return p.spec.DefaultModel
}

func (p *httpProvider) endpoint(path string, extra url.Values) string {
	u := strings.TrimRight(p.spec.BaseURL, "/") + path
	q := url.Values{}
	for k, v := range p.spec.Query {
		q.Set(k, v)
	}
	for k, vs := range extra {
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	if len(q) > 0 {
		if strings.Contains(u, "?") {
			u += "&" + q.Encode()
		} else {
			u += "?" + q.Encode()
		}
	}
	return u
}

// do sends the request. For streams the returned context cancel must be
// called when the body is closed; for non-streams it is applied internally.
func (p *httpProvider) do(ctx context.Context, url string, body any, hdr http.Header, opts CallOptions) (*http.Response, context.CancelFunc, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, p.spec.Timeout)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		cancel()
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header[k] = v
	}
	for k, v := range p.spec.Headers {
		req.Header.Set(k, v)
	}
	for k, v := range opts.Headers {
		req.Header[k] = v
	}
	resp, err := p.client.Do(req)
	if err != nil {
		cancel()
		return nil, nil, classify(p.spec.Name, err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		cancel()
		return nil, nil, &UpstreamError{Provider: p.spec.Name, Status: resp.StatusCode, Body: string(b)}
	}
	return resp, cancel, nil
}

// ---------------------------------------------------------------- OpenAI ---

type openAIProvider struct{ *httpProvider }

func (p *openAIProvider) body(req *ChatRequest, stream bool) map[string]any {
	b := map[string]any{"model": p.model(req), "messages": req.Messages}
	if stream {
		b["stream"] = true
		b["stream_options"] = map[string]any{"include_usage": true}
	}
	if req.Temperature != nil {
		b["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		b["top_p"] = *req.TopP
	}
	if req.MaxTokens != nil {
		b["max_tokens"] = *req.MaxTokens
	}
	if len(req.Stop) > 0 {
		b["stop"] = req.Stop
	}
	for k, v := range p.spec.ExtraBody {
		if _, set := b[k]; !set {
			b[k] = v
		}
	}
	return b
}

func (p *openAIProvider) authHeader() http.Header {
	h := http.Header{}
	if p.spec.APIKey != "" {
		h.Set("Authorization", "Bearer "+p.spec.APIKey)
	}
	return h
}

func (p *openAIProvider) Complete(ctx context.Context, req *ChatRequest, opts CallOptions) (*Completion, error) {
	resp, cancel, err := p.do(ctx, p.endpoint("/v1/chat/completions", nil), p.body(req, false), p.authHeader(), opts)
	if err != nil {
		return nil, err
	}
	defer cancel()
	defer resp.Body.Close()
	var out ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, classify(p.spec.Name, fmt.Errorf("decode response: %w", err))
	}
	if len(out.Choices) == 0 {
		return nil, &UpstreamError{Provider: p.spec.Name, Status: http.StatusBadGateway, Body: "no choices in response"}
	}
	c := &Completion{Content: out.Choices[0].Message.Content, FinishReason: out.Choices[0].FinishReason, Model: out.Model, Usage: out.Usage}
	fillUsage(c, req)
	return c, nil
}

func (p *openAIProvider) Stream(ctx context.Context, req *ChatRequest, opts CallOptions) (Stream, error) {
	resp, cancel, err := p.do(ctx, p.endpoint("/v1/chat/completions", nil), p.body(req, true), p.authHeader(), opts)
	if err != nil {
		return nil, err
	}
	return &sseStream{provider: p.spec.Name, body: resp.Body, cancel: cancel, r: bufio.NewReaderSize(resp.Body, 32<<10), parse: parseOpenAIEvent}, nil
}

func parseOpenAIEvent(_ string, data []byte) (Delta, bool, error) {
	if string(data) == "[DONE]" {
		return Delta{}, true, nil
	}
	var c StreamChunk
	if err := json.Unmarshal(data, &c); err != nil {
		return Delta{}, false, err
	}
	var d Delta
	if len(c.Choices) > 0 {
		d.Content = c.Choices[0].Delta.Content
		if c.Choices[0].FinishReason != nil {
			d.FinishReason = *c.Choices[0].FinishReason
		}
	}
	d.Usage = c.Usage
	return d, false, nil
}

// ------------------------------------------------------------- Anthropic ---

type anthropicProvider struct{ *httpProvider }

func (p *anthropicProvider) body(req *ChatRequest, stream bool) map[string]any {
	var system []string
	msgs := make([]map[string]string, 0, len(req.Messages))
	for _, m := range req.Messages {
		if m.Role == "system" {
			system = append(system, m.Content)
			continue
		}
		role := m.Role
		if role != "assistant" {
			role = "user"
		}
		msgs = append(msgs, map[string]string{"role": role, "content": m.Content})
	}
	maxTokens := 1024
	if req.MaxTokens != nil {
		maxTokens = *req.MaxTokens
	}
	b := map[string]any{"model": p.model(req), "messages": msgs, "max_tokens": maxTokens}
	if len(system) > 0 {
		b["system"] = strings.Join(system, "\n\n")
	}
	if stream {
		b["stream"] = true
	}
	if req.Temperature != nil {
		b["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		b["top_p"] = *req.TopP
	}
	if len(req.Stop) > 0 {
		b["stop_sequences"] = req.Stop
	}
	return b
}

func (p *anthropicProvider) headers() http.Header {
	h := http.Header{}
	h.Set("anthropic-version", "2023-06-01")
	if p.spec.APIKey != "" {
		h.Set("x-api-key", p.spec.APIKey)
	}
	return h
}

func anthropicStop(s string) string {
	switch s {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "":
		return ""
	}
	return s
}

func (p *anthropicProvider) Complete(ctx context.Context, req *ChatRequest, opts CallOptions) (*Completion, error) {
	resp, cancel, err := p.do(ctx, p.endpoint("/v1/messages", nil), p.body(req, false), p.headers(), opts)
	if err != nil {
		return nil, err
	}
	defer cancel()
	defer resp.Body.Close()
	var out struct {
		Model   string `json:"model"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, classify(p.spec.Name, fmt.Errorf("decode response: %w", err))
	}
	var sb strings.Builder
	for _, c := range out.Content {
		if c.Type == "text" {
			sb.WriteString(c.Text)
		}
	}
	c := &Completion{
		Content: sb.String(), FinishReason: anthropicStop(out.StopReason), Model: out.Model,
		Usage: Usage{PromptTokens: out.Usage.InputTokens, CompletionTokens: out.Usage.OutputTokens, TotalTokens: out.Usage.InputTokens + out.Usage.OutputTokens},
	}
	fillUsage(c, req)
	return c, nil
}

func (p *anthropicProvider) Stream(ctx context.Context, req *ChatRequest, opts CallOptions) (Stream, error) {
	resp, cancel, err := p.do(ctx, p.endpoint("/v1/messages", nil), p.body(req, true), p.headers(), opts)
	if err != nil {
		return nil, err
	}
	st := &anthropicState{}
	return &sseStream{provider: p.spec.Name, body: resp.Body, cancel: cancel, r: bufio.NewReaderSize(resp.Body, 32<<10), parse: st.parse}, nil
}

type anthropicState struct{ inputTokens int }

func (s *anthropicState) parse(event string, data []byte) (Delta, bool, error) {
	var ev struct {
		Type    string `json:"type"`
		Message struct {
			Usage struct {
				InputTokens int `json:"input_tokens"`
			} `json:"usage"`
		} `json:"message"`
		Delta struct {
			Type       string `json:"type"`
			Text       string `json:"text"`
			StopReason string `json:"stop_reason"`
		} `json:"delta"`
		Usage struct {
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return Delta{}, false, err
	}
	if ev.Type == "" {
		ev.Type = event
	}
	switch ev.Type {
	case "message_start":
		s.inputTokens = ev.Message.Usage.InputTokens
	case "content_block_delta":
		return Delta{Content: ev.Delta.Text}, false, nil
	case "message_delta":
		u := &Usage{PromptTokens: s.inputTokens, CompletionTokens: ev.Usage.OutputTokens, TotalTokens: s.inputTokens + ev.Usage.OutputTokens}
		return Delta{FinishReason: anthropicStop(ev.Delta.StopReason), Usage: u}, false, nil
	case "message_stop":
		return Delta{}, true, nil
	case "error":
		return Delta{}, false, errors.New("anthropic stream error: " + ev.Error.Message)
	}
	return Delta{}, false, nil
}

// ---------------------------------------------------------------- Gemini ---

type geminiProvider struct{ *httpProvider }

func (p *geminiProvider) body(req *ChatRequest) map[string]any {
	type part struct {
		Text string `json:"text"`
	}
	type content struct {
		Role  string `json:"role,omitempty"`
		Parts []part `json:"parts"`
	}
	var system []part
	contents := make([]content, 0, len(req.Messages))
	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			system = append(system, part{m.Content})
		case "assistant":
			contents = append(contents, content{Role: "model", Parts: []part{{m.Content}}})
		default:
			contents = append(contents, content{Role: "user", Parts: []part{{m.Content}}})
		}
	}
	b := map[string]any{"contents": contents}
	if len(system) > 0 {
		b["systemInstruction"] = content{Parts: system}
	}
	gc := map[string]any{}
	if req.Temperature != nil {
		gc["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		gc["topP"] = *req.TopP
	}
	if req.MaxTokens != nil {
		gc["maxOutputTokens"] = *req.MaxTokens
	}
	if len(req.Stop) > 0 {
		gc["stopSequences"] = req.Stop
	}
	if len(gc) > 0 {
		b["generationConfig"] = gc
	}
	return b
}

func (p *geminiProvider) headers() http.Header {
	h := http.Header{}
	if p.spec.APIKey != "" {
		h.Set("x-goog-api-key", p.spec.APIKey)
	}
	return h
}

type geminiResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata *struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
		TotalTokenCount      int `json:"totalTokenCount"`
	} `json:"usageMetadata"`
	ModelVersion string `json:"modelVersion"`
}

func (g geminiResponse) text() string {
	var sb strings.Builder
	for _, c := range g.Candidates {
		for _, p := range c.Content.Parts {
			sb.WriteString(p.Text)
		}
		break
	}
	return sb.String()
}

func geminiStop(s string) string {
	switch s {
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT":
		return "content_filter"
	}
	return strings.ToLower(s)
}

func (g geminiResponse) usage() *Usage {
	if g.UsageMetadata == nil {
		return nil
	}
	return &Usage{PromptTokens: g.UsageMetadata.PromptTokenCount, CompletionTokens: g.UsageMetadata.CandidatesTokenCount, TotalTokens: g.UsageMetadata.TotalTokenCount}
}

func (p *geminiProvider) Complete(ctx context.Context, req *ChatRequest, opts CallOptions) (*Completion, error) {
	model := p.model(req)
	resp, cancel, err := p.do(ctx, p.endpoint("/v1beta/models/"+url.PathEscape(model)+":generateContent", nil), p.body(req), p.headers(), opts)
	if err != nil {
		return nil, err
	}
	defer cancel()
	defer resp.Body.Close()
	var out geminiResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, classify(p.spec.Name, fmt.Errorf("decode response: %w", err))
	}
	if len(out.Candidates) == 0 {
		return nil, &UpstreamError{Provider: p.spec.Name, Status: http.StatusBadGateway, Body: "no candidates in response"}
	}
	c := &Completion{Content: out.text(), FinishReason: geminiStop(out.Candidates[0].FinishReason), Model: model}
	if u := out.usage(); u != nil {
		c.Usage = *u
	}
	fillUsage(c, req)
	return c, nil
}

func (p *geminiProvider) Stream(ctx context.Context, req *ChatRequest, opts CallOptions) (Stream, error) {
	model := p.model(req)
	resp, cancel, err := p.do(ctx, p.endpoint("/v1beta/models/"+url.PathEscape(model)+":streamGenerateContent", url.Values{"alt": {"sse"}}), p.body(req), p.headers(), opts)
	if err != nil {
		return nil, err
	}
	return &sseStream{provider: p.spec.Name, body: resp.Body, cancel: cancel, r: bufio.NewReaderSize(resp.Body, 32<<10), parse: parseGeminiEvent}, nil
}

func parseGeminiEvent(_ string, data []byte) (Delta, bool, error) {
	var g geminiResponse
	if err := json.Unmarshal(data, &g); err != nil {
		return Delta{}, false, err
	}
	d := Delta{Content: g.text(), Usage: g.usage()}
	if len(g.Candidates) > 0 && g.Candidates[0].FinishReason != "" {
		d.FinishReason = geminiStop(g.Candidates[0].FinishReason)
	}
	return d, false, nil
}

// ------------------------------------------------------------ SSE reader ---

// sseStream parses a text/event-stream body, delegating each data payload to
// a provider-specific parser. parse returns done=true on a terminal event.
type sseStream struct {
	provider string
	body     io.ReadCloser
	cancel   context.CancelFunc
	r        *bufio.Reader
	parse    func(event string, data []byte) (Delta, bool, error)
	done     bool
}

func (s *sseStream) Recv() (Delta, error) {
	if s.done {
		return Delta{}, io.EOF
	}
	var event string
	var data bytes.Buffer
	for {
		line, err := s.r.ReadBytes('\n')
		if len(line) == 0 && err != nil {
			if errors.Is(err, io.EOF) {
				s.done = true
				if data.Len() > 0 {
					d, _, perr := s.parse(event, data.Bytes())
					if perr != nil {
						return Delta{}, classify(s.provider, perr)
					}
					return d, nil
				}
				return Delta{}, io.EOF
			}
			return Delta{}, classify(s.provider, err)
		}
		line = bytes.TrimRight(line, "\r\n")
		switch {
		case len(line) == 0: // dispatch
			if data.Len() == 0 {
				event = ""
				continue
			}
			d, done, perr := s.parse(event, data.Bytes())
			if perr != nil {
				return Delta{}, classify(s.provider, perr)
			}
			if done {
				s.done = true
				if d.Content == "" && d.FinishReason == "" && d.Usage == nil {
					return Delta{}, io.EOF
				}
				return d, nil
			}
			if d.Content == "" && d.FinishReason == "" && d.Usage == nil {
				event = ""
				data.Reset()
				continue
			}
			return d, nil
		case bytes.HasPrefix(line, []byte("event:")):
			event = strings.TrimSpace(string(line[6:]))
		case bytes.HasPrefix(line, []byte("data:")):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.Write(bytes.TrimPrefix(line[5:], []byte(" ")))
		}
	}
}

func (s *sseStream) Close() error {
	err := s.body.Close()
	s.cancel()
	return err
}

func fillUsage(c *Completion, req *ChatRequest) {
	if c.Usage.PromptTokens == 0 {
		n := 0
		for _, m := range req.Messages {
			n += EstimateTokens(m.Content)
		}
		c.Usage.PromptTokens = n
	}
	if c.Usage.CompletionTokens == 0 {
		c.Usage.CompletionTokens = EstimateTokens(c.Content)
	}
	c.Usage.TotalTokens = c.Usage.PromptTokens + c.Usage.CompletionTokens
}
