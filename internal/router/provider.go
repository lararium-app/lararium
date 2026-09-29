package router

import (
	"context"
)

// Role identifies the sender of a message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is a single turn in a conversation.
type Message struct {
	Role       Role
	Content    string
	ToolCallID string
	ToolCalls  []ToolCall
}

// ToolCall represents a single tool invocation requested by the model.
type ToolCall struct {
	ID       string
	Name     string
	ArgsJSON string
}

// Completion is the result of a model completion request.
type Completion struct {
	Text      string
	ToolCalls []ToolCall
	Model     string
	InTokens  int
	OutTokens int
	Probed    bool
}

// Options configures a completion request.
type Options struct {
	Model       string
	MaxTokens   int
	Temperature float64
	Tools       []ToolSpec
}

// ToolSpec describes a tool available to the model.
type ToolSpec struct {
	Name             string
	Description      string
	ParamsJSONSchema string
}

// Provider is the model-agnostic interface for LLM backends.
type Provider interface {
	Complete(ctx context.Context, msgs []Message, opts Options) (*Completion, error)
	Capabilities(ctx context.Context) (Caps, error)
	Name() string
}

// Streamer is an optional interface for providers that support token
// streaming. The router degrades non-streamers to a single whole-text
// delta rather than failing the chain (a chain may legitimately mix
// streaming and non-streaming targets).
type Streamer interface {
	Provider
	StreamComplete(ctx context.Context, msgs []Message, opts Options, model string, onDelta StreamHandler) (*Completion, error)
}

// Caps describes the capabilities of a model/provider.
type Caps struct {
	ContextLength  int
	SupportsVision bool
	SupportsTools  bool
	Source         string // "probed" or "config"
}

// StreamHandler receives streaming text deltas.
type StreamHandler func(delta string) error
