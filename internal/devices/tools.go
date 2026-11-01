package devices

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/lararium-app/lararium/internal/custos"
	"github.com/lararium-app/lararium/internal/loop"
	"github.com/lararium-app/lararium/internal/router"
)

// StalenessMarker formats the exact staleness marker string per DEVICE-RAILS-SPEC D7 & Deliverable 2:
// [stale: phone offline since <RFC3339>, lag <duration>].
func StalenessMarker(lastSeen time.Time, lag time.Duration) string {
	lagStr := lag.Round(time.Second).String()
	if lagStr == "" || lagStr == "0s" {
		lagStr = "0s"
	}
	return fmt.Sprintf("[stale: phone offline since %s, lag %s]", lastSeen.UTC().Format(time.RFC3339), lagStr)
}

// ToolSource builds the dynamic tool provider for registered, non-tombstoned devices per DEVICE-RAILS-SPEC D1/D4/D5/D6/D7.
func (m *Manager) ToolSource(deciders ...PolicyDecider) func() []loop.Tool {
	var decider PolicyDecider
	if len(deciders) > 0 {
		decider = deciders[0]
	}

	return func() []loop.Tool {
		devs := m.List()
		if len(devs) == 0 {
			return nil
		}

		var tools []loop.Tool
		seen := make(map[string]bool)

		for _, dev := range devs {
			for _, rail := range dev.RailsDeclared {
				if !rail.IsEnabled() {
					continue
				}

				toolName, ok := ToolForRail(rail.Name)
				if !ok {
					continue
				}

				modelName := ToolName(dev.DeviceID, toolName)
				if seen[modelName] {
					continue
				}
				seen[modelName] = true

				lt := m.buildLoopTool(dev.DeviceID, rail.Name, toolName, modelName, rail.ReadOnly, decider)
				tools = append(tools, lt)
			}
		}
		return tools
	}
}

// buildLoopTool constructs one loop.Tool for a device rail.
func (m *Manager) buildLoopTool(deviceID, rail, toolName, modelName string, readOnly bool, decider PolicyDecider) loop.Tool {
	schema := ToolSchema(toolName)
	desc := ToolDescription(deviceID, toolName)

	verdict := custos.VerdictAsk
	if decider != nil {
		verdict = decider.Decide(deviceID, toolName)
	}

	trusted := false
	if readOnly {
		// Read rails are read_only: true and execute in-process without approval cards
		trusted = true
		if verdict == custos.VerdictDeny {
			trusted = true // Gate passes execution to Run, which fails with policy rejection error
		}
	} else {
		// Action rails ride the loop untrusted approval path
		switch verdict {
		case custos.VerdictAuto:
			trusted = true
		case custos.VerdictDeny:
			trusted = true
		default:
			trusted = false
		}
	}

	if readOnly {
		return loop.Tool{
			Spec: router.ToolSpec{
				Name:             modelName,
				Description:      desc,
				ParamsJSONSchema: schema,
			},
			Trusted:  trusted,
			FullArgs: false,
			Run: func(_ context.Context, _ string) (string, error) {
				if decider != nil {
					curVerdict := decider.Decide(deviceID, toolName)
					if curVerdict == custos.VerdictDeny {
						return "", fmt.Errorf("device %s/%s: blocked by policy", deviceID, toolName)
					}
				}

				items, lag, err := m.Read(deviceID, rail)
				if err != nil {
					if !errors.Is(err, os.ErrNotExist) {
						return "", fmt.Errorf("read rail %s/%s: %w", deviceID, rail, err)
					}
					items = []byte("[]")
				}

				res := string(items)
				if !m.IsOnline(deviceID) {
					lastSeen := m.LastSeen(deviceID)
					if lastSeen.IsZero() {
						if dev, gerr := m.Get(deviceID); gerr == nil {
							lastSeen = dev.LastSeen
						}
					}
					marker := StalenessMarker(lastSeen, lag)
					res = marker + "\n" + res
				}
				return res, nil
			},
		}
	}

	// Action rail
	return loop.Tool{
		Spec: router.ToolSpec{
			Name:             modelName,
			Description:      desc,
			ParamsJSONSchema: schema,
		},
		Trusted:  trusted,
		FullArgs: true, // Action tools carry full args JSON to the approval card
		Run: func(ctx context.Context, argsJSON string) (string, error) {
			if decider != nil {
				curVerdict := decider.Decide(deviceID, toolName)
				if curVerdict == custos.VerdictDeny {
					return "", fmt.Errorf("device %s/%s: blocked by policy", deviceID, toolName)
				}
				if curVerdict == custos.VerdictAsk && trusted {
					return "", fmt.Errorf("device %s/%s: policy changed to ask; retry the turn", deviceID, toolName)
				}
			}

			// Liveness check: fast-fail if offline or broker not attached (no queueing)
			if !m.IsOnline(deviceID) {
				return "", ErrDeviceOffline
			}
			broker := m.ActionBroker()
			if broker == nil {
				return "", ErrDeviceOffline
			}

			// Validate arguments against stored schema; drift rejects without touching broker
			if err := ValidateArgs(toolName, argsJSON); err != nil {
				return "", err
			}

			rawArgs := json.RawMessage(argsJSON)
			if len(rawArgs) == 0 {
				rawArgs = json.RawMessage("{}")
			}

			res, err := broker.Dispatch(ctx, deviceID, toolName, rawArgs)
			if err != nil {
				return "", fmt.Errorf("dispatch %s/%s: %w", deviceID, toolName, err)
			}
			return string(res), nil
		},
	}
}

// RulePattern reverse-maps a model-facing tool name to the canonical worker rule pattern "<device_id>/<tool>".
func (m *Manager) RulePattern(modelName string) (string, bool) {
	if modelName == "" {
		return "", false
	}
	for _, dev := range m.List() {
		for _, rail := range dev.RailsDeclared {
			toolName, ok := ToolForRail(rail.Name)
			if !ok {
				continue
			}
			if ToolName(dev.DeviceID, toolName) == modelName {
				return dev.DeviceID + "/" + toolName, true
			}
		}
	}
	return "", false
}
