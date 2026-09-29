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
	"sort"
	"strings"
	"time"
)

// Profile defines an ordered chain of model targets.
type Profile struct {
	Chain []Target
}

// Target is a single provider+model in a chain.
type Target struct {
	Provider Provider
	Model    string
}

// Router manages named profiles and executes completions with failover.
type Router struct {
	profiles map[string]Profile
}

// ChainError records every (provider, error) attempt across a failed chain.
type ChainError struct {
	Legs []ChainLeg
}

// ChainLeg is one attempt in a chain.
type ChainLeg struct {
	Provider string
	Model    string
	Err      error
}

func (e *ChainError) Error() string {
	parts := make([]string, 0, len(e.Legs))
	for _, l := range e.Legs {
		parts = append(parts, fmt.Sprintf("%s(%s): %v", l.Provider, l.Model, l.Err))
	}
	return "all chain targets failed: [" + strings.Join(parts, "; ") + "]"
}

// NewRouter creates a router with a default "chat" profile.
func NewRouter(def Profile) *Router {
	return &Router{
		profiles: map[string]Profile{
			"chat": def,
		},
	}
}

// SetProfile adds or replaces a named profile.
func (r *Router) SetProfile(name string, p Profile) {
	r.profiles[name] = p
}

// Probe returns the capabilities of the profile's FIRST target (the primary).
// The context window that governs compaction is the primary's; if the
// primary is down the compaction trigger simply doesn't fire (conservative).
func (r *Router) Probe(ctx context.Context, profile string) (Caps, error) {
	p, ok := r.profiles[profile]
	if !ok {
		return Caps{}, fmt.Errorf("unknown profile: %s", profile)
	}
	if len(p.Chain) == 0 {
		return Caps{}, fmt.Errorf("empty profile: %s", profile)
	}
	return p.Chain[0].Provider.Capabilities(ctx)
}

// Complete walks the profile's chain in order, retrying retryable errors
// and advancing on non-retryable ones. Returns *ChainError only if the
// entire chain fails.
func (r *Router) Complete(ctx context.Context, profile string, msgs []Message, opts Options) (*Completion, error) {
	p, ok := r.profiles[profile]
	if !ok {
		return nil, fmt.Errorf("unknown profile: %s", profile)
	}

	return r.executeChain(ctx, p.Chain, msgs, opts, false)
}

// CompleteStream is like Complete but with streaming.
// Fallback on same chain rules BEFORE first delta arrives.
// After first delta, error is returned immediately.
func (r *Router) CompleteStream(ctx context.Context, profile string, msgs []Message, opts Options, onDelta StreamHandler) (*Completion, error) {
	p, ok := r.profiles[profile]
	if !ok {
		return nil, fmt.Errorf("unknown profile: %s", profile)
	}

	return r.executeChain(ctx, p.Chain, msgs, opts, true, onDelta)
}

func (r *Router) executeChain(ctx context.Context, chain []Target, msgs []Message, opts Options, stream bool, handlers ...StreamHandler) (*Completion, error) {
	var legs []ChainLeg

	for ti, target := range chain {
		providerName := target.Provider.Name()
		model := target.Model
		if model == "" {
			model = opts.Model
		}

		// Retry loop for this target (max 3 attempts).
		for attempt := 0; attempt < 3; attempt++ {
			if attempt > 0 {
				// Check if caller cancelled before retrying.
				select {
				case <-ctx.Done():
					// Caller cancellation — abort immediately, no failover.
					legs = append(legs, ChainLeg{Provider: providerName, Model: model, Err: ctx.Err()})
					return nil, &ChainError{Legs: legs}
				default:
				}

				// Sleep before retry.
				delay := time.Duration(500*attempt) * time.Millisecond
				timer := time.NewTimer(delay)
				select {
				case <-ctx.Done():
					timer.Stop()
					legs = append(legs, ChainLeg{Provider: providerName, Model: model, Err: ctx.Err()})
					return nil, &ChainError{Legs: legs}
				case <-timer.C:
				}
			}

			var (
				c   *Completion
				err error
			)

			if stream {
				c, err = r.streamTarget(ctx, target, msgs, opts, handlers[0])
			} else {
				c, err = target.Provider.Complete(ctx, msgs, opts)
			}

			if err == nil {
				return c, nil
			}

			leg := ChainLeg{Provider: providerName, Model: model, Err: err}

			// Check if caller cancelled — abort immediately.
			if ctx.Err() != nil {
				legs = append(legs, leg)
				return nil, &ChainError{Legs: legs}
			}

			if !isRetryable(err) {
				// Non-retryable: advance immediately without more retries.
				legs = append(legs, leg)
				break
			}

			// Retryable: record and try again (or advance if last attempt).
			legs = append(legs, leg)

			// If this was the last attempt for this target, advance.
			if attempt == 2 {
				break
			}
		}

		// Don't advance past the last target; we'll return the chain error.
		if ti == len(chain)-1 {
			break
		}
	}

	return nil, &ChainError{Legs: legs}
}

func (r *Router) streamTarget(ctx context.Context, target Target, msgs []Message, opts Options, onDelta StreamHandler) (*Completion, error) {
	model := target.Model
	if model == "" {
		model = opts.Model
	}

	s, ok := target.Provider.(Streamer)
	if !ok {
		// Degrade: whole text as a single delta.
		comp, err := target.Provider.Complete(ctx, msgs, opts)
		if err != nil {
			return nil, err
		}
		if comp.Text != "" {
			if err := onDelta(comp.Text); err != nil {
				return nil, err
			}
		}
		return comp, nil
	}

	return s.StreamComplete(ctx, msgs, opts, model, onDelta)
}

// streamComplete handles SSE streaming for openAI-compatible providers.
func (o *openAI) streamComplete(ctx context.Context, msgs []Message, opts Options, model string, onDelta StreamHandler) (*Completion, error) {
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
		Model:         model,
		Messages:      reqMsgs,
		MaxTokens:     opts.MaxTokens,
		Temperature:   opts.Temperature,
		Tools:         reqTools,
		Stream:        true,
		StreamOptions: &streamOptions{IncludeUsage: true},
	}
	if o.disableThink {
		body.ChatTemplateKwargs = map[string]any{"enable_thinking": false}
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", o.baseURL+"/chat/completions", bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if o.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.apiKey)
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

	// Accumulate completion.
	c := &Completion{
		Model: model,
	}

	// Tool calls are accumulated by index, merging fragments.
	// Each SSE chunk may contain partial tool_calls.
	type toolCallAccum struct {
		ID       string
		Name     string
		ArgsJSON string
	}
	var toolCallMap map[int]*toolCallAccum

	scanner := bufio.NewScanner(resp.Body)
	// Increase buffer for large SSE lines.
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	firstDelta := false
	for scanner.Scan() {
		line := scanner.Text()

		// Skip empty lines.
		if line == "" {
			continue
		}

		// [DONE] terminator.
		if line == "data: [DONE]" {
			break
		}

		// Must start with "data: ".
		if !strings.HasPrefix(line, "data: ") {
			// Malformed SSE line — skip, not fatal.
			continue
		}

		data := strings.TrimPrefix(line, "data: ")

		var chunk openAIStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			// Malformed JSON — skip.
			continue
		}

		if c.Model == "" && chunk.Model != "" {
			c.Model = chunk.Model
		}

		// Final usage chunk (stream_options.include_usage).
		if chunk.Usage != nil {
			c.InTokens = chunk.Usage.PromptTokens
			c.OutTokens = chunk.Usage.CompletionTokens
			c.Probed = true
		}

		for _, choice := range chunk.Choices {
			// Forward text delta.
			if choice.Delta.Content != "" {
				if !firstDelta {
					firstDelta = true
				}
				c.Text += choice.Delta.Content
				if onDelta != nil {
					if err := onDelta(choice.Delta.Content); err != nil {
						return c, fmt.Errorf("stream handler error: %w", err)
					}
				}
			}

			// Accumulate tool calls by index.
			for _, tc := range choice.Delta.ToolCalls {
				idx := tc.Index
				if idx < 0 {
					idx = 0
				}
				if toolCallMap == nil {
					toolCallMap = make(map[int]*toolCallAccum)
				}
				acc, exists := toolCallMap[idx]
				if !exists {
					acc = &toolCallAccum{}
					toolCallMap[idx] = acc
				}
				if tc.ID != "" {
					acc.ID = tc.ID
				}
				if tc.Function.Name != "" {
					acc.Name = tc.Function.Name
				}
				acc.ArgsJSON += tc.Function.Args
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return c, fmt.Errorf("scan error: %w", err)
	}

	// Convert accumulated tool calls in index order.
	indices := make([]int, 0, len(toolCallMap))
	for i := range toolCallMap {
		indices = append(indices, i)
	}
	sort.Ints(indices)
	for _, i := range indices {
		acc := toolCallMap[i]
		c.ToolCalls = append(c.ToolCalls, ToolCall{
			ID:       acc.ID,
			Name:     acc.Name,
			ArgsJSON: acc.ArgsJSON,
		})
	}

	// Fallback estimate only if the server sent no usage chunk.
	if !c.Probed {
		c.OutTokens = len(c.Text) / 4
	}

	return c, nil
}

// openAIStreamChunk is a single SSE chunk from the streaming API.
type openAIStreamChunk struct {
	Model   string         `json:"model"`
	Choices []streamChoice `json:"choices"`
	Usage   *openAIUsage   `json:"usage"`
}

type streamChoice struct {
	Index        int         `json:"index"`
	Delta        streamDelta `json:"delta"`
	FinishReason string      `json:"finish_reason"`
}

type streamDelta struct {
	Role      string           `json:"role"`
	Content   string           `json:"content,omitempty"`
	ToolCalls []streamToolCall `json:"tool_calls,omitempty"`
}

type streamToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name string `json:"name"`
		Args string `json:"arguments"`
	} `json:"function"`
}

type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// isRetryable classifies an error for failover decisions.
// Retryable: 429, 5xx, network dial error, ctx deadline (NOT caller cancellation).
// Non-retryable: 4xx except 429.
func isRetryable(err error) bool {
	if err == nil {
		return false
	}

	// Context deadline exceeded from the operation itself is retryable;
	// context.Canceled from the caller is not.
	msg := err.Error()

	// Check for 429 or 5xx in HTTP error messages.
	if strings.Contains(msg, "HTTP 429") {
		return true
	}
	if strings.Contains(msg, "HTTP 5") {
		return true
	}

	// 4xx (except 429) is non-retryable.
	if strings.Contains(msg, "HTTP 4") {
		return false
	}

	// Network dial errors.
	if strings.Contains(msg, "dial") || strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "connection reset") || strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "i/o timeout") {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}

	// Context deadline exceeded (not cancellation).
	if strings.Contains(msg, "context deadline exceeded") {
		return true
	}
	// Context canceled = caller abort, not retryable.
	if strings.Contains(msg, "context canceled") {
		return false
	}

	return false
}
