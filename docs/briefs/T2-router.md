# Task Brief T2 — `internal/router`: model-agnostic provider layer

Spec: docs/ARCHITECTURE.md §2.1 + BUILD-PROCESS Phase 2 ("Detection, not assumption").
Module root: github.com/lararium-app/lararium. Go 1.24. **Stdlib only.**
Do NOT touch internal/penatus/ or anything outside internal/router/.

EXACT DESIGN (do not deviate):

## Files (all in internal/router/)

1. `provider.go`
   - `type Role string` constants RoleSystem/User/Assistant/Tool.
   - `type Message struct { Role Role; Content string; ToolCallID string; ToolCalls []ToolCall }`
   - `type ToolCall struct { ID, Name, ArgsJSON string }`
   - `type Completion struct { Text string; ToolCalls []ToolCall; Model string; InTokens, OutTokens int; Probed bool }` — Probed=true when token counts came from provider usage fields (else locally estimated).
   - `type Options struct { Model string; MaxTokens int; Temperature float64; Tools []ToolSpec }`
   - `type ToolSpec struct { Name, Description, ParamsJSONSchema string }`
   - `type Provider interface { Complete(ctx, []Message, Options) (*Completion, error); Capabilities(ctx) (Caps, error); Name() string }`
   - `type Caps struct { ContextLength int; SupportsVision, SupportsTools bool; Source string }` — Source ∈ {"probed","config"}.
   - `type StreamHandler func(delta string) error`.
2. `openai.go` — `NewOpenAI(baseURL, apiKey, defaultModel string) Provider`.
   - POST {base}/chat/completions, OpenAI schema; maps Tools into `tools` array
     (type function, parameters from ParamsJSONSchema raw). Response tool_calls →
     ToolCall (ArgsJSON = raw arguments string). usage.prompt_tokens/completion_tokens
     → In/OutTokens, Probed=true when usage object present, else estimate
     len(text)/4 and Probed=false. HTTP non-2xx → error including status + body
     capped at 400 bytes. Timeout via ctx. Authorization: Bearer (omit header when
     apiKey empty — local llama.cpp).
3. `capabilities.go`
   - `ProbeOpenAI(ctx, baseURL, apiKey string) (Caps, error)`:
     GET {base}/models → pick entry matching defaultModel (or first). llama.cpp
     extension fields parsed when present: `trained_tokens`, `n_ctx` /
     `projector` presence → SupportsVision. Anthropic-style `/v1/models` id-match
     best-effort. If GET fails → error; callers then fall back to config Caps with
     Source="config". NEVER invent numbers: absent field = 0/false.
4. `router.go`
   - `type Router struct` with: profiles map[string]Profile; default profile "chat".
   - `type Profile struct { Chain []Target }`, `type Target struct { Provider Provider; Model string }`.
   - `NewRouter(def Profile) *Router`, `(*Router) SetProfile(name string, p Profile)`,
     `(*Router) Complete(ctx, profile string, msgs []Message, opts Options) (*Completion, error)`
     — walks chain IN ORDER: try target; classify error (see below); on retryable
     (429, 5xx, network dial error, ctx deadline NOT from caller cancellation)
     sleep 500ms*attempt (max 3 attempts per target), then advance to next target;
     on non-retryable (4xx except 429) advance immediately without retry. Only if
     entire chain fails does Complete return an error — the error is a
     `*ChainError` listing every (provider, err) leg.
   - context.Canceled from the CALLER must abort immediately, no failover.
   - `CompleteStream(ctx, profile, msgs, opts, onDelta) (*Completion, error)` —
     `stream: true`, SSE parsing (`data: ` lines, [DONE] terminator), deltas
     forwarded, accumulated text + tool_calls (index-merged per OpenAI spec:
     fragments arrive split by index) returned. Fallback on same chain rules
     BEFORE first delta arrives; after first delta, error is returned (no silent
     restart mid-stream).
5. `router_test.go` + `openai_test.go` — use net/http/httptest servers ONLY:
   happy path with usage fields; missing usage → estimate + Probed=false; 500 then
   200 failover within chain; 400 advances with zero retries (count requests
   server-side); all-fail → ChainError with legs; caller ctx cancel aborts without
   hitting second target; SSE stream reassembles split tool_call argument
   fragments across ≥3 chunks; [DONE] respected; malformed SSE line skipped, not fatal.

HARD RULES:
- NO third-party imports, NO TODO bodies. Full implementations.
- go vet ./... && go test ./... must pass; paste real output when done.
