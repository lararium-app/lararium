package custos

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"sort"
	"strings"
)

// Frozen worker-lane error codes per CUSTOS-SPEC §5.1a (closed enum).
const (
	ErrCodeDenied           = "denied"
	ErrCodeLocked           = "locked"
	ErrCodeUnknownConnector = "unknown_connector"
	ErrCodeUnknownTool      = "unknown_tool"
	ErrCodeBadArgs          = "bad_args"
	ErrCodeUpstream         = "upstream"
	ErrCodeRateLimited      = "rate_limited"
	ErrCodeTimeout          = "timeout"
	ErrCodeTransport        = "transport"
	ErrCodeInternal         = "internal"
)

// Frozen detail templates per §5.1a: closed set, no URL, no upstream body,
// no wrapped error chain.
const (
	detailDeniedToken      = "denied: invalid cell token"
	detailDeniedPolicy     = "denied: policy"
	detailDeniedApproval   = "denied: approval"
	detailDeniedRevoked    = "denied: revoked"
	detailDeniedCredGone   = "denied: credential unavailable" //nolint:gosec // frozen detail template, not a secret
	detailLocked           = "custody locked"
	detailUnknownConnector = "unknown connector"
	detailUnknownTool      = "unknown tool"
	detailBadArgs          = "invalid arguments"
	detailUpstream5xx      = "upstream returned 5xx"
	detailUpstream4xx      = "upstream returned 4xx"
	detailRateLimited      = "upstream rate limited"
	detailTimeout          = "operation deadline exceeded"
	detailTransport        = "connection failed before response"
	detailInternal         = "internal error"
)

// ValidWorkerErrorCode reports membership in the frozen §5.1a enum.
func ValidWorkerErrorCode(code string) bool {
	switch code {
	case ErrCodeDenied, ErrCodeLocked, ErrCodeUnknownConnector, ErrCodeUnknownTool,
		ErrCodeBadArgs, ErrCodeUpstream, ErrCodeRateLimited, ErrCodeTimeout,
		ErrCodeTransport, ErrCodeInternal:
		return true
	default:
		return false
	}
}

// workerError carries a §5.1a code with a template-frozen detail. Its error
// text never contains a URL, an upstream body, or a wrapped cause chain.
type workerError struct {
	code   string
	detail string
}

func (e *workerError) Error() string { return e.code + ": " + e.detail }

func wErr(code, detail string) *workerError { return &workerError{code: code, detail: detail} }

// WorkerRequest is one NDJSON worker request per §5.1.
type WorkerRequest struct {
	Op        string          `json:"op"`
	Connector string          `json:"connector"`
	Tool      string          `json:"tool"`
	Args      json.RawMessage `json:"args"`
	CellToken string          `json:"cell_token"`
}

// WorkerResponse is one NDJSON worker response per §5.1:
// {"ok":true,"result":…} or {"error_code":"…","detail":"…"}.
type WorkerResponse struct {
	OK        bool            `json:"ok,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	ErrorCode string          `json:"error_code,omitempty"`
	Detail    string          `json:"detail,omitempty"`
}

func okResponse(result json.RawMessage) WorkerResponse {
	return WorkerResponse{OK: true, Result: result}
}

func errResponse(code, detail string) WorkerResponse {
	return WorkerResponse{ErrorCode: code, Detail: detail}
}

// Dispatch runs the §5.1 order: locked → unknown_connector → unknown_tool →
// policy worker-lane Decide → ask card (review fields) → execute.
// Every response detail passes the §5.1a whole-set scrub.
func (ws *WorkerServer) Dispatch(cw *cellWorker, req *WorkerRequest) (resp WorkerResponse) {
	host := ""
	connector := ""
	tool := ""

	defer func() {
		if resp.Detail != "" {
			resp.Detail = ws.ScrubDetail(resp.Detail)
		}
	}()

	// Protocol gate: op must be "call" or "list".
	if req.Op != "call" && req.Op != "list" {
		resp = errResponse(ErrCodeBadArgs, detailBadArgs)
		ws.auditDenied(cw, connector, host, tool, "bad_op")
		return resp
	}

	// cell_token identity (§5.1).
	if !cw.verifyCellToken(req.CellToken) {
		resp = errResponse(ErrCodeDenied, detailDeniedToken)
		ws.auditDenied(cw, connector, host, tool, "cell_token")
		return resp
	}

	// 1. custody locked — only when the vault exists and is actually
	// locked; a nil vault is a server wiring fault, never "locked"
	// (a cell must never be told custody is sealed when none is
	// loaded). A nil vault has no audit logger; the denial response
	// itself is the signal.
	if ws.vault == nil {
		return errResponse(ErrCodeInternal, detailInternal)
	}
	if !ws.vault.IsUnlocked() {
		resp = errResponse(ErrCodeLocked, detailLocked)
		ws.auditDenied(cw, connector, host, tool, "locked")
		return resp
	}

	// Op "list" enumeration per MCP-SPEC A1 / CUSTOS-SPEC §5.1:
	// rail snapshots ∪ non-rail ToolLister results, connector order
	// names-sorted (no first-party privilege), server order within
	// connector.
	if req.Op == "list" {
		var tools []ToolInfo
		ws.mu.Lock()
		names := make([]string, 0, len(ws.connectors))
		for name := range ws.connectors {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if tl, ok := ws.connectors[name].(ToolLister); ok {
				tools = append(tools, tl.ListTools()...)
			}
		}
		ws.mu.Unlock()

		tools = TruncateToolList(tools, 2<<20)

		raw, err := json.Marshal(struct {
			Tools []ToolInfo `json:"tools"`
		}{
			Tools: tools,
		})
		if err != nil {
			// v13: §5.1a's error enum is closed — an unreachable
			// marshal fault maps to `internal`, never a coined code.
			return WorkerResponse{
				ErrorCode: ErrCodeInternal,
				Detail:    detailInternal,
			}
		}

		return WorkerResponse{
			OK:     true,
			Result: raw,
		}
	}

	// 2. unknown connector.
	conn, known := ws.connectorByName(req.Connector)
	if !known {
		resp = errResponse(ErrCodeUnknownConnector, detailUnknownConnector)
		ws.auditDenied(cw, req.Connector, host, tool, "unknown_connector")
		return resp
	}
	connector = conn.Name()

	// 3. unknown tool.
	if !conn.HasTool(req.Tool) {
		resp = errResponse(ErrCodeUnknownTool, detailUnknownTool)
		ws.auditDenied(cw, connector, host, req.Tool, "unknown_tool")
		return resp
	}
	tool = req.Tool
	host = conn.Host()

	// 4. typed args schema (bad shape ⇒ bad_args).
	if err := conn.ValidateArgs(tool, req.Args); err != nil {
		resp = errResponse(ErrCodeBadArgs, detailBadArgs)
		ws.auditDenied(cw, connector, host, tool, "bad_args")
		return resp
	}

	// 5. policy worker-lane Decide (§6.1/§6.2 tool-qualified patterns).
	verdict, _ := ws.vault.Policy().Decide(DecideInput{
		Lane:       "worker",
		Credential: conn.Credential(),
		Tool:       tool,
		Host:       host,
	})

	switch verdict {
	case VerdictDeny:
		resp = errResponse(ErrCodeDenied, detailDeniedPolicy)
		// v13: record the declared policy-deny reason. The matched
		// pattern is unbounded vocabulary; §8.1's reason set is closed.
		ws.auditDenied(cw, connector, host, tool, "verdict")
		return resp

	case VerdictAsk:
		if ok, r := ws.askCard(cw, conn, tool, req.Args); !ok {
			resp = r
			return resp
		}
		// §7 doctrine (mirror of bearer policy-during-park): the engine is
		// re-consulted after the hold settles — a policy rm/flip committed
		// during the hold kills the flow before any upstream byte.
		// §6.2 doctrine: if the policy flipped to deny while the card
		// was held (via any surface), the approval never executes.
		// An unchanged ask is the very thing the human just answered.
		if v2, _ := ws.vault.Policy().Decide(DecideInput{
			Lane:       "worker",
			Credential: conn.Credential(),
			Tool:       tool,
			Host:       host,
		}); v2 == VerdictDeny {
			resp = errResponse(ErrCodeDenied, detailDeniedPolicy)
			ws.auditDenied(cw, connector, host, tool, "policy_changed_during_hold")
			return resp
		}

	case VerdictAuto:
		// fall through to execution

	default:
		// Unknown verdict is impossible from the engine; refuse closed.
		resp = errResponse(ErrCodeDenied, detailDeniedPolicy)
		ws.auditDenied(cw, connector, host, tool, "verdict")
		return resp
	}

	// Execution-time re-validation (§7 doctrine: policy rm / revoke during a
	// hold kills the flow at the dial; the credential must still exist).
	cred, ok := ws.vault.GetCredential(conn.Credential())
	if !ok {
		resp = errResponse(ErrCodeDenied, detailDeniedCredGone)
		ws.auditDenied(cw, connector, host, tool, "credential_gone")
		return resp
	}

	ctx, cancel := context.WithTimeout(context.Background(), ws.cfg.WorkerExecTimeout)
	defer cancel()

	result, execErr := conn.Execute(ctx, tool, req.Args, cred)
	if execErr != nil {
		we := classifyWorkerError(execErr)
		resp = errResponse(we.code, we.detail)
		ws.auditDenied(cw, connector, host, tool, we.code)
		return resp
	}

	ws.auditAllowed(cw, connector, host, tool)
	return okResponse(result)
}

// connectorByName looks a connector up under the dispatch lock.
func (ws *WorkerServer) connectorByName(name string) (Connector, bool) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	c, ok := ws.connectors[name]
	return c, ok
}

// auditAllowed appends worker_call_allowed (§8.1) with the verified actor.
// A nil vault has no audit logger: the call is a no-op (the response frame
// is the only signal; nil vault is a wiring fault, never a panic).
func (ws *WorkerServer) auditAllowed(cw *cellWorker, cred, host, tool string) {
	if ws.vault == nil {
		return
	}
	_ = ws.vault.Audit().Append(AuditRecord{
		Kind:    AuditKindWorkerCallAllowed,
		Cred:    cred,
		Actor:   cw.cellID,
		Host:    host,
		Tool:    tool,
		Verdict: "allow",
	})
}

// auditDenied appends worker_call_denied (§8.1) with the verified actor.
// A nil vault has no audit logger: no-op, never a panic.
func (ws *WorkerServer) auditDenied(cw *cellWorker, cred, host, tool, reason string) {
	if ws.vault == nil {
		return
	}
	_ = ws.vault.Audit().Append(AuditRecord{
		Kind:    AuditKindWorkerCallDenied,
		Cred:    cred,
		Actor:   cw.cellID,
		Host:    host,
		Tool:    tool,
		Verdict: "deny",
		Reason:  reason,
	})
}

// classifyWorkerError maps execution failures onto the frozen §5.1a enum.
// Output is code + frozen template only: never a URL, upstream body, or
// wrapped chain (§5.1a).
func classifyWorkerError(err error) *workerError {
	var we *workerError
	if errors.As(err, &we) {
		return we
	}
	// Token-endpoint non-2xx contributes its status class only (§5.1a).
	var tse *tokenStatusError
	if errors.As(err, &tse) {
		return upstreamStatusError(tse.status)
	}
	if errors.Is(err, ErrCredentialGone) {
		return wErr(ErrCodeDenied, detailDeniedCredGone)
	}
	if isDeadlineErr(err) {
		return wErr(ErrCodeTimeout, detailTimeout)
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		// DNS / refused / no-status: connection failed before a response.
		return wErr(ErrCodeTransport, detailTransport)
	}
	// Bare resolver/socket failures (connector dials outside http.Client
	// wrappers too): any net.Error — DNSError, OpError — is transport.
	var ne net.Error
	if errors.As(err, &ne) {
		return wErr(ErrCodeTransport, detailTransport)
	}
	return wErr(ErrCodeInternal, detailInternal)
}

// V13TestError is the exported view of a classified worker error for the
// doctrine tests (classification itself stays package-internal).
type V13TestError struct {
	Code   string
	Detail string
}

// ClassifyWorkerErrorForTest exposes classifyWorkerError to the V-suite.
func ClassifyWorkerErrorForTest(err error) V13TestError {
	we := classifyWorkerError(err)
	return V13TestError{Code: we.code, Detail: we.detail}
}

// ScrubDetail applies the §5.1a whole-set scrub: every loaded credential
// field individually (api_key secret; oauth2 client_secret, refresh_token,
// access_token separately) and every loaded surrogate token, replaced
// left-to-right non-overlapping with [redacted]; values < 8 chars are never
// replaced (KEYS-SPEC floor).
func (ws *WorkerServer) ScrubDetail(detail string) string {
	pairs := ws.scrubNeedles()
	if len(pairs) == 0 {
		return detail
	}
	return strings.NewReplacer(pairs...).Replace(detail)
}

// scrubNeedles collects replacement pairs from the loaded vault state:
// every credential field individually, every surrogate token. A nil vault
// has nothing loaded: no needles, never a panic (the nil-vault doctrine
// branch in Dispatch must stay reachable).
func (ws *WorkerServer) scrubNeedles() []string {
	if ws.vault == nil {
		return nil
	}
	var vals []string
	add := func(v string) {
		if len(v) >= scrubMinLength {
			vals = append(vals, v)
		}
	}

	for _, c := range ws.vault.LoadedCredentials() {
		add(c.Secret)
		add(c.ClientSecret)
		add(c.RefreshToken)
		add(c.AccessToken)
	}
	if ws.vault.Surrogates() != nil {
		for _, s := range ws.vault.Surrogates().List() {
			add(s.Token)
		}
	}

	sort.Slice(vals, func(i, j int) bool { return len(vals[i]) > len(vals[j]) })
	pairs := make([]string, 0, 2*len(vals))
	for _, v := range vals {
		pairs = append(pairs, v, redactedPlaceholder)
	}
	return pairs
}
