package router

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// openAI implements Provider for OpenAI-compatible APIs.
type openAI struct {
	baseURL      string
	apiKey       string
	defaultModel string
}

// NewOpenAI creates an OpenAI-compatible provider.
// Pass an empty apiKey for local llama.cpp (no Authorization header).
func NewOpenAI(baseURL, apiKey, defaultModel string) Provider {
	return &openAI{
		baseURL:      strings.TrimRight(baseURL, "/"),
		apiKey:       apiKey,
		defaultModel: defaultModel,
	}
}

func (o *openAI) Name() string {
	return "openai"
}

func (o *openAI) Capabilities(ctx context.Context) (Caps, error) {
	return ProbeOpenAI(ctx, o.baseURL, o.apiKey, o.defaultModel)
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
}

func (o *openAI) Complete(ctx context.Context, msgs []Message, opts Options) (*Completion, error) {
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

	var oresp openAIResponse
	if err := json.NewDecoder(resp.Body).Decode(&oresp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
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
