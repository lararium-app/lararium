package router

import (
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
)

// ErrNoKey is the sentinel for a provider that resolved to no key
// while pointing at a remote host (KEYS-SPEC K6 local-fail rule: a
// removed key must never produce a keyless request to a remote
// provider). Doors render their own sentence from this text.
var ErrNoKey = errors.New("has no key — hearthd keys set")

// openAI implements Provider for OpenAI-compatible APIs.
type openAI struct {
	baseURL      string
	keyFn        func(ctx context.Context) string
	defaultModel string
	// disableThink sends chat_template_kwargs{enable_thinking:false} to
	// llama.cpp-style servers (measured 2.5–15× faster non-reasoning turns).
	disableThink bool
	// enforceKey applies the K6 local-fail rule: empty key + non-loopback
	// host => error before the request. Set only by NewOpenAIKeyed;
	// static-key constructors keep the legacy keyless behavior.
	enforceKey bool
}

// NewOpenAI creates an OpenAI-compatible provider.
// Pass an empty apiKey for local llama.cpp (no Authorization header).
func NewOpenAI(baseURL, apiKey, defaultModel string) Provider {
	return NewOpenAIWith(baseURL, apiKey, defaultModel, false)
}

// NewOpenAIWith additionally disables model-side reasoning chains
// (Qwen3-style thinking) when supported by the server template.
// Static key: legacy behavior preserved — an empty key sends no
// Authorization header without the K6 local-fail check.
func NewOpenAIWith(baseURL, apiKey, defaultModel string, disableThink bool) Provider {
	return &openAI{
		baseURL:      strings.TrimRight(baseURL, "/"),
		keyFn:        func(context.Context) string { return apiKey },
		defaultModel: defaultModel,
		disableThink: disableThink,
	}
}

// NewOpenAIKeyed creates a provider whose key is resolved per call
// from keyFn (turn-pinned snapshot first, registry fallback — the
// caller owns that ordering). Enforces the K6 local-fail rule: an
// empty key against a non-loopback host fails before any request.
func NewOpenAIKeyed(baseURL string, keyFn func(context.Context) string, defaultModel string, disableThink bool) Provider {
	return &openAI{
		baseURL:      strings.TrimRight(baseURL, "/"),
		keyFn:        keyFn,
		defaultModel: defaultModel,
		disableThink: disableThink,
		enforceKey:   true,
	}
}

// resolveKey applies the K6 local-fail rule and returns the key for
// this call. Empty key on a loopback host is allowed (local llama.cpp
// needs no Authorization header).
func (o *openAI) resolveKey(ctx context.Context) (string, error) {
	key := o.keyFn(ctx)
	if key == "" && o.enforceKey && !isLoopbackURL(o.baseURL) {
		return "", fmt.Errorf("provider %w %s", ErrNoKey, o.baseURLHost())
	}
	return key, nil
}

// baseURLHost returns the URL host for error messages (raw on parse
// failure — the request path will surface the real error).
func (o *openAI) baseURLHost() string {
	u, err := url.Parse(o.baseURL)
	if err != nil {
		return o.baseURL
	}
	return u.Host
}

// isLoopbackURL reports whether the URL host is loopback: 127.0.0.0/8,
// ::1, "localhost", or an empty/unparseable host (unix sockets).
func isLoopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return true // unparseable: treat as local, never worse than today
	}
	host := u.Hostname()
	if host == "" || host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false // remote hostname
	}
	return ip.IsLoopback() || ip.Equal(net.IPv4zero) || ip4In127(ip)
}

// ip4In127 covers 127.0.0.0/8 explicitly (IsLoopback already does on
// Linux; belt-and-braces for IPv4-in-IPv6 mapped forms).
func ip4In127(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		return v4[0] == 127
	}
	return false
}

// Name returns the provider id.
func (o *openAI) Name() string {
	return "openai"
}

// Capabilities probes the backend (GET /models) and reports what it supports.
func (o *openAI) Capabilities(ctx context.Context) (Caps, error) {
	key, err := o.resolveKey(ctx)
	if err != nil {
		return Caps{}, err
	}
	return ProbeOpenAI(ctx, o.baseURL, key, o.defaultModel)
}

// schemaOrNull returns raw JSON for a tool schema, defaulting to an empty object.
func schemaOrNull(s string) json.RawMessage {
	if strings.TrimSpace(s) == "" {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(s)
}

type openAIRequest struct {
	Model       string          `json:"model"`
	Messages    []openAIMessage `json:"messages"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
	Temperature float64         `json:"temperature,omitempty"`
	Tools       []openAITool    `json:"tools,omitempty"`
	Stream      bool            `json:"stream,omitempty"`
	// Ask for a final usage chunk on streams (OpenAI-compatible; llama.cpp
	// supports it). Without it, streaming turns report 0 tokens.
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
	// llama.cpp extension; nil for providers that reject unknown fields.
	ChatTemplateKwargs map[string]any `json:"chat_template_kwargs,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
}

type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name string `json:"name"`
		Args string `json:"arguments"`
	} `json:"function"`
}

type openAITool struct {
	Type     string     `json:"type"`
	Function openAIFunc `json:"function"`
}

type openAIFunc struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// openAIResponse is the JSON body from /chat/completions.
type openAIResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int           `json:"index"`
		Message      openAIMessage `json:"message"`
		FinishReason string        `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *inBandError `json:"error"`
}

// inBandError models providers (OpenRouter free tier, notably) that answer
// HTTP 200 but carry the real failure — overloaded, rate-limited — in the
// JSON body. Treating those as completions silently swallows the failure.
type inBandError struct {
	Message string      `json:"message"`
	Code    interface{} `json:"code"`
}

func (e *inBandError) Error() string {
	// Format the code so isRetryable's "HTTP 5xx"/"HTTP 429" classification
	// works on in-band errors exactly as on real HTTP statuses.
	switch c := e.Code.(type) {
	case float64:
		return fmt.Sprintf("HTTP %d: %s (in-band)", int(c), e.Message)
	case string:
		return fmt.Sprintf("HTTP %s: %s (in-band)", c, e.Message)
	default:
		return fmt.Sprintf("in-band error: %s", e.Message)
	}
}

// Complete issues one non-streaming chat completion.
func (o *openAI) Complete(ctx context.Context, msgs []Message, opts Options) (*Completion, error) {
	key, err := o.resolveKey(ctx)
	if err != nil {
		return nil, err
	}
	model := opts.Model
	if model == "" {
		model = o.defaultModel
	}

	reqMsgs := make([]openAIMessage, 0, len(msgs))
	for _, m := range msgs {
		om := openAIMessage{Role: string(m.Role), Content: m.Content}
		if m.ToolCallID != "" {
			om.ToolCallID = m.ToolCallID
		}
		for _, tc := range m.ToolCalls {
			var otc openAIToolCall
			otc.ID = tc.ID
			otc.Type = "function"
			otc.Function.Name = tc.Name
			otc.Function.Args = tc.ArgsJSON
			om.ToolCalls = append(om.ToolCalls, otc)
		}
		reqMsgs = append(reqMsgs, om)
	}

	var reqTools []openAITool
	for _, t := range opts.Tools {
		reqTools = append(reqTools, openAITool{
			Type: "function",
			Function: openAIFunc{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  schemaOrNull(t.ParamsJSONSchema),
			},
		})
	}

	body := openAIRequest{
		Model:       model,
		Messages:    reqMsgs,
		MaxTokens:   opts.MaxTokens,
		Temperature: opts.Temperature,
		Tools:       reqTools,
	}
	if o.disableThink {
		body.ChatTemplateKwargs = map[string]any{"enable_thinking": false}
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/chat/completions", bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		limit := int64(400)
		b, _ := io.ReadAll(io.LimitReader(resp.Body, limit))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	var oresp openAIResponse
	if err := json.NewDecoder(resp.Body).Decode(&oresp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	// HTTP 200 with an error body: some hosts hide upstream failures
	// in-band. Raise them so the chain failover sees and classifies.
	if oresp.Error != nil && oresp.Error.Message != "" {
		return nil, oresp.Error
	}

	if len(oresp.Choices) == 0 {
		return nil, fmt.Errorf("no choices in response")
	}

	choice := oresp.Choices[0]
	c := &Completion{
		Model:  oresp.Model,
		Text:   choice.Message.Content,
		Probed: oresp.Usage != nil,
	}

	for _, tc := range choice.Message.ToolCalls {
		c.ToolCalls = append(c.ToolCalls, ToolCall{
			ID:       tc.ID,
			Name:     tc.Function.Name,
			ArgsJSON: tc.Function.Args,
		})
	}

	if oresp.Usage != nil {
		c.InTokens = oresp.Usage.PromptTokens
		c.OutTokens = oresp.Usage.CompletionTokens
	} else {
		c.InTokens = 0
		c.OutTokens = len(choice.Message.Content) / 4
	}

	return c, nil
}

// StreamComplete implements Streamer.
func (o *openAI) StreamComplete(ctx context.Context, msgs []Message, opts Options, model string, onDelta StreamHandler) (*Completion, error) {
	return o.streamComplete(ctx, msgs, opts, model, onDelta)
}
