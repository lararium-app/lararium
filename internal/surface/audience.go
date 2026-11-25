package surface

import (
	"context"
	"errors"
	"slices"
)

// Session audience (SURFACE-SPEC §4b, PENATUS-SPEC §2). A session's
// audience is the list of caller identities that may see it; an empty
// audience means every paired device. Audience is privacy among one
// operator's own paired devices — a glance-guard, not a hostile-tenant
// boundary: the hearth operator can always read the session files.

// ErrNotCreator means a caller other than the session's creator tried to
// change its audience (v1: audience edits are creator-only).
var ErrNotCreator = errors.New("not session creator")

// ErrSharedSession means an audience change was requested on a session
// whose audience is fixed to every device (the main session).
var ErrSharedSession = errors.New("session audience is fixed")

// SessionCreateRequest is one session creation with its audience record.
// Creator is the creating caller identity ("" for none); Private scopes the
// new session to the creator alone.
type SessionCreateRequest struct {
	Title    string
	ModelPin string
	Creator  string
	Private  bool
}

// SessionConfigureRequest is one configure call with an optional audience
// change. Field semantics match SessionSource.Configure; Private nil leaves
// the audience unchanged, true scopes the session to Caller (who must be its
// creator), false shares it with every device again.
type SessionConfigureRequest struct {
	ModelPin         *string
	Private          *bool
	ExpectedRevision *int64
	Caller           string
}

// SessionAudienceSource is implemented by session sources that persist the
// audience record (PenatusSource). Sources without it treat every session
// as shared with every device and reject private creation and audience
// changes as not implemented.
type SessionAudienceSource interface {
	// SessionAudience returns the audience of session id (nil = every
	// device), or ErrNoSession.
	SessionAudience(id string) ([]string, error)
	// CreateWith creates a side session carrying the audience record.
	CreateWith(req SessionCreateRequest) (SessionInfo, error)
	// ConfigureWith is Configure plus an optional audience change under the
	// same revision compare-and-swap.
	ConfigureWith(id string, req SessionConfigureRequest) (SessionInfo, error)
}

type callerKey struct{}

// withCaller binds the authenticated caller identity to ctx.
func withCaller(ctx context.Context, caller string) context.Context {
	return context.WithValue(ctx, callerKey{}, caller)
}

// callerFrom returns the caller identity bound by BearerAuth, or "" for an
// anonymous caller (a Verifier without identities). An anonymous caller is
// in no audience, so it sees only sessions shared with every device.
func callerFrom(ctx context.Context) string {
	caller, _ := ctx.Value(callerKey{}).(string)
	return caller
}

type callerKindKey struct{}

// withCallerKind binds the kind of credential the request authenticated
// with (see TokenKindDevice) into ctx, alongside the caller identity.
func withCallerKind(ctx context.Context, kind string) context.Context {
	return context.WithValue(ctx, callerKindKey{}, kind)
}

// callerKindFrom returns the credential kind bound by BearerAuth, or ""
// when the credential carries none — which is the case for every
// operator-minted credential and for anything authenticated by a plain
// Verifier.
func callerKindFrom(ctx context.Context) string {
	kind, _ := ctx.Value(callerKindKey{}).(string)
	return kind
}

type callerDeviceKey struct{}

// withCallerDevice binds the enrolled device_id associated with the request's
// credential into ctx, alongside caller identity and kind (DEVICE-RAILS-SPEC D-ENROLL).
func withCallerDevice(ctx context.Context, deviceID string) context.Context {
	return context.WithValue(ctx, callerDeviceKey{}, deviceID)
}

// callerDeviceIDFrom returns the device_id bound by BearerAuth, or "" for
// unbound credentials (admin/web/legacy/owner-paired) or verifiers that do
// not implement BindingVerifier.
func callerDeviceIDFrom(ctx context.Context) string {
	devID, _ := ctx.Value(callerDeviceKey{}).(string)
	return devID
}

// Channel names on the verdict wire (CUSTOS-SPEC §6.4b). Declared here
// rather than imported from the door client so the surface layer does not
// depend on it for two string constants.
const (
	viaWeb    = "web"
	viaDevice = "device"
)

// viaForKind maps a credential kind to the channel a verdict from it must
// be attributed to. Only a credential the daemon itself minted for a
// paired device yields the device channel; everything else — including
// every credential minted before the kind field existed — is the web
// channel, which preserves the pre-existing meaning of those records.
func viaForKind(kind string) string {
	if kind == TokenKindDevice {
		return viaDevice
	}
	return viaWeb
}

// audienceAllows reports whether caller may see a session with audience.
func audienceAllows(audience []string, caller string) bool {
	if len(audience) == 0 {
		return true
	}
	return caller != "" && slices.Contains(audience, caller)
}

// sessionVisibleTo reports whether caller may see session id. An unknown
// session reports true, so each handler's own not-found path still answers
// it unchanged; any other lookup failure (an unreadable or corrupt header)
// reports false, failing closed. Callers must apply this gate before acting
// on the session and answer a false with the same not-found reply the path
// gives an unknown id.
func (s *Server) sessionVisibleTo(caller, id string) bool {
	src, ok := s.Sessions.(SessionAudienceSource)
	if !ok {
		return true
	}
	audience, err := src.SessionAudience(id)
	if errors.Is(err, ErrNoSession) {
		return true
	}
	if err != nil {
		return false
	}
	return audienceAllows(audience, caller)
}

// SessionExistenceChecker is implemented by session sources that can say
// whether a session id names a stored session (PenatusSource). Sources
// without it are not asked: every well-formed id is taken to exist.
type SessionExistenceChecker interface {
	SessionExists(id string) bool
}

// sessionExists reports whether id names a stored session, true when the
// source cannot tell.
func (s *Server) sessionExists(id string) bool {
	src, ok := s.Sessions.(SessionExistenceChecker)
	if !ok {
		return true
	}
	return src.SessionExists(id)
}

// visibleSessions drops the sessions caller may not see, keeping order.
func visibleSessions(sessions []SessionInfo, caller string) []SessionInfo {
	out := make([]SessionInfo, 0, len(sessions))
	for _, info := range sessions {
		if audienceAllows(info.audience, caller) {
			out = append(out, info)
		}
	}
	return out
}

// createSession creates a side session for caller through the audience
// source when the Sessions source has one; otherwise it falls back to the
// plain Create, where private creation is not implemented.
func (s *Server) createSession(caller, title, modelPin string, private bool) (SessionInfo, error) {
	if src, ok := s.Sessions.(SessionAudienceSource); ok {
		return src.CreateWith(SessionCreateRequest{
			Title:    title,
			ModelPin: modelPin,
			Creator:  caller,
			Private:  private,
		})
	}
	if private {
		return SessionInfo{}, errAudienceUnsupported
	}
	return s.Sessions.Create(title, modelPin)
}

// errAudienceUnsupported means the session source does not persist the
// audience record, so a private session or audience change cannot be
// honored.
var errAudienceUnsupported = errors.New("session audience not supported")

// errPrivateNeedsIdentity means a private session was requested by an
// anonymous caller, which no audience can name.
var errPrivateNeedsIdentity = errors.New("private session requires a caller identity")

// dropRPCSubsOutside withdraws every live sessions.subscribe registration
// on session id held by a connection whose caller is not in audience, so a
// session scoped down mid-subscription stops streaming to devices that can
// no longer see it. Callers must write the new audience first. The
// registrations leave the map under rpcSubsMu; each is closed afterwards
// under its own lock, never with both held.
func (s *Server) dropRPCSubsOutside(id string, audience []string) {
	if len(audience) == 0 {
		return
	}
	var dropped []*rpcSessionSub
	s.rpcSubsMu.Lock()
	conns := s.rpcSubs[id]
	for conn, sub := range conns {
		if !audienceAllows(audience, conn.caller) {
			delete(conns, conn)
			dropped = append(dropped, sub)
		}
	}
	if conns != nil && len(conns) == 0 {
		delete(s.rpcSubs, id)
	}
	s.rpcSubsMu.Unlock()
	for _, sub := range dropped {
		sub.close()
	}
}
