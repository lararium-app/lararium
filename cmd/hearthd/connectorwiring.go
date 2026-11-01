package main

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/lararium-app/lararium/internal/custosdoor"
	"github.com/lararium-app/lararium/internal/surface"
)

// connectorDoorAdapter adapts *custosdoor.Client to surface.ConnectorDoor
// (CONNECTOR-LIFE-SPEC D3-D7): status/tools come from the door client's
// mirror of the MCP-STATUS / MCP-TOOLS broadcasts, the lifecycle verbs ride
// dedicated doors.sock connections. surface never imports custody, so every
// door error is mapped onto surface sentinels here.
type connectorDoorAdapter struct {
	client *custosdoor.Client
}

// newConnectorDoorAdapter returns nil for a nil client so the caller can
// leave the surface unwired (503s) instead of holding a dead adapter.
func newConnectorDoorAdapter(client *custosdoor.Client) surface.ConnectorDoor {
	if client == nil {
		return nil
	}
	return &connectorDoorAdapter{client: client}
}

var _ surface.ConnectorDoor = (*connectorDoorAdapter)(nil)

func (a *connectorDoorAdapter) ConnectorStatuses() []surface.ConnectorDoorStatus {
	sts := a.client.Statuses()
	out := make([]surface.ConnectorDoorStatus, 0, len(sts))
	for _, st := range sts {
		out = append(out, surface.ConnectorDoorStatus{
			Name: st.Name, State: st.State, Reason: st.Reason,
			Tools: st.Tools, Restarts: st.Restarts, LastUsed: st.LastUsed,
		})
	}
	return out
}

func (a *connectorDoorAdapter) ConnectorTools(name string) []surface.ConnectorDoorTool {
	return toSurfaceTools(a.client.ToolsOf(name))
}

func toSurfaceTools(in []custosdoor.ToolInfo) []surface.ConnectorDoorTool {
	out := make([]surface.ConnectorDoorTool, 0, len(in))
	for _, t := range in {
		out = append(out, surface.ConnectorDoorTool{Name: t.Name, Flags: t.Flags})
	}
	return out
}

// SubscribeConnectors pokes on every pushed rail broadcast. The door client
// also fires once at subscribe time with the current snapshot; the surface
// diff makes that a no-op.
func (a *connectorDoorAdapter) SubscribeConnectors(poke func()) func() {
	return a.client.SubscribeMCP(func([]custosdoor.ConnectorStatus, []custosdoor.ToolInfo) { poke() })
}

func (a *connectorDoorAdapter) Probe(ctx context.Context, configJSON []byte) (surface.ConnectorProbe, error) {
	res, err := a.client.Probe(ctx, configJSON)
	if err != nil {
		return surface.ConnectorProbe{}, mapConnectorDoorErr(err)
	}
	return surface.ConnectorProbe{Fingerprint: res.Fingerprint, Tools: toSurfaceTools(res.Tools)}, nil
}

func (a *connectorDoorAdapter) Add(ctx context.Context, name string, configJSON []byte) error {
	return mapConnectorDoorErr(a.client.Add(ctx, name, configJSON))
}

func (a *connectorDoorAdapter) Remove(ctx context.Context, name string) error {
	return mapConnectorDoorErr(a.client.Remove(ctx, name))
}

func (a *connectorDoorAdapter) SetState(ctx context.Context, name string, enabled bool) error {
	return mapConnectorDoorErr(a.client.SetState(ctx, name, enabled))
}

// Refresh is the named door over the frozen MCP-REFRESH verb (D7 reauth).
func (a *connectorDoorAdapter) Refresh(ctx context.Context, name string) error {
	return mapConnectorDoorErr(a.client.Refresh(ctx, name))
}

var reasonTokenRe = regexp.MustCompile(`^[a-z0-9_]{1,40}$`)

// mapConnectorDoorErr maps door-client errors onto surface sentinels. A
// plain error whose text is a closed reason token (MCP-REFRESH replies are
// bare "ERR <reason>" strings) becomes a reason error; "connector … not
// found" becomes not_found; anything else passes through for a generic 502.
func mapConnectorDoorErr(err error) error {
	var le *custosdoor.LifecycleError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, custosdoor.ErrDoorDown), errors.Is(err, custosdoor.ErrBadToken):
		return surface.ErrConnectorDoorDown
	case errors.Is(err, custosdoor.ErrVerbUnsupported):
		return surface.ErrConnectorDaemonTooOld
	case errors.Is(err, custosdoor.ErrLocked):
		return surface.ErrLocked
	case errors.As(err, &le):
		return &surface.ConnectorReasonError{Reason: le.Reason}
	}
	msg := err.Error()
	switch {
	case strings.HasPrefix(msg, "connector ") && strings.HasSuffix(msg, " not found"):
		return &surface.ConnectorReasonError{Reason: "not_found"}
	case reasonTokenRe.MatchString(msg):
		return &surface.ConnectorReasonError{Reason: msg}
	}
	return err
}
