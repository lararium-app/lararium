package custos

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/lararium-app/lararium/internal/surface"
)

// ApprovalCardWire is the uniform wire shape for pending approval cards per CUSTOS-SPEC §6.4a.
type ApprovalCardWire struct {
	ID         string `json:"id"`
	Cell       string `json:"cell"`
	Cred       string `json:"cred"`
	Dest       string `json:"dest"`
	Tool       string `json:"tool"`
	Review     string `json:"review"`
	AgeS       int    `json:"age_s"`
	ExpiresInS int    `json:"expires_in_s"`
}

// handleApprovalCmd handles standalone approval door verbs: CARDS, APPROVE, DENY per CUSTOS-SPEC §6.4a.
func (s *CtlServer) handleApprovalCmd(conn net.Conn, cmd string, parts []string) bool {
	upper := strings.ToUpper(cmd)
	if upper != "CARDS" && upper != "APPROVE" && upper != "DENY" {
		return false
	}
	if s.vault == nil || !s.vault.IsUnlocked() {
		fmt.Fprint(conn, "ERR locked\n")
		return true
	}
	switch upper {
	case "CARDS":
		s.handleCards(conn)
		return true

	case "APPROVE":
		if len(parts) < 3 {
			fmt.Fprint(conn, "ERR bad_request\n")
			return true
		}
		cardID := strings.TrimSpace(parts[2])
		if cardID == "" {
			fmt.Fprint(conn, "ERR bad_request\n")
			return true
		}
		s.handleResolve(conn, cardID, true)
		return true

	case "DENY":
		if len(parts) < 3 {
			fmt.Fprint(conn, "ERR bad_request\n")
			return true
		}
		cardID := strings.TrimSpace(parts[2])
		if cardID == "" {
			fmt.Fprint(conn, "ERR bad_request\n")
			return true
		}
		s.handleResolve(conn, cardID, false)
		return true

	default:
		return false
	}
}

// CardSnapshot produces the uniform ApprovalCardWire list by joining pending hub cards
// with worker and proxy park managers per CUSTOS-SPEC §6.4a, §6.4b.
func CardSnapshot(hub *surface.ApprovalHub, workers *WorkerServer, proxy *Proxy) []ApprovalCardWire {
	if hub == nil {
		return []ApprovalCardWire{}
	}

	pending := hub.Pending()
	if len(pending) == 0 {
		return []ApprovalCardWire{}
	}

	cards := make([]ApprovalCardWire, 0, len(pending))
	for _, card := range pending {
		wire := ApprovalCardWire{
			ID:     card.ID,
			Cell:   card.Cell,
			Cred:   card.Cred,
			Tool:   "",
			Dest:   "",
			Review: "",
		}

		found := false
		if workers != nil && workers.parkMgr != nil {
			workers.parkMgr.mu.Lock()
			for _, cellParks := range workers.parkMgr.byCell {
				if flow, ok := cellParks[card.ID]; ok {
					found = true
					wire.Tool = flow.path
					wire.Dest = ""
					wire.Review = flow.review
					if flow.cred != "" {
						wire.Cred = flow.cred
					}
					if flow.cellID != "" {
						wire.Cell = flow.cellID
					}
					now := time.Now()
					wire.AgeS = int(now.Sub(flow.createdAt).Seconds())
					if wire.AgeS < 0 {
						wire.AgeS = 0
					}
					wire.ExpiresInS = int(flow.createdAt.Add(workers.parkMgr.holdTimeout).Sub(now).Seconds())
					if wire.ExpiresInS < 0 {
						wire.ExpiresInS = 0
					}
					break
				}
			}
			workers.parkMgr.mu.Unlock()
		}

		if !found && proxy != nil && proxy.parkMgr != nil {
			proxy.parkMgr.mu.Lock()
			for _, cellParks := range proxy.parkMgr.byCell {
				if flow, ok := cellParks[card.ID]; ok {
					found = true
					if flow.cred != "" {
						wire.Cred = flow.cred
					} else {
						wire.Cred = card.Cred
					}
					wire.Tool = ""
					if flow.port > 0 {
						wire.Dest = fmt.Sprintf("%s:%d%s", flow.host, flow.port, flow.path)
					} else if flow.host != "" {
						wire.Dest = fmt.Sprintf("%s%s", flow.host, flow.path)
					}
					if flow.cellID != "" {
						wire.Cell = flow.cellID
					}
					wire.Review = flow.review
					now := time.Now()
					wire.AgeS = int(now.Sub(flow.createdAt).Seconds())
					if wire.AgeS < 0 {
						wire.AgeS = 0
					}
					wire.ExpiresInS = int(flow.createdAt.Add(proxy.parkMgr.holdTimeout).Sub(now).Seconds())
					if wire.ExpiresInS < 0 {
						wire.ExpiresInS = 0
					}
					break
				}
			}
			proxy.parkMgr.mu.Unlock()
		}

		if !found {
			if (workers == nil || workers.parkMgr == nil) && (proxy == nil || proxy.parkMgr == nil) {
				cards = append(cards, wire)
			}
			continue
		}

		cards = append(cards, wire)
	}
	return cards
}

func (s *CtlServer) handleCards(conn net.Conn) {
	cards := CardSnapshot(s.hubInstance(), s.workersInstance(), s.proxyInstance())
	b, err := json.Marshal(cards) //nolint:gosec // ApprovalCardWire.Cred carries provider/credential name, never secret value
	if err != nil {
		fmt.Fprintf(conn, "ERR %s\n", err.Error())
		return
	}
	fmt.Fprintf(conn, "OK %s\n", string(b))
}

func (s *CtlServer) handleResolve(conn net.Conn, cardID string, allow bool) {
	hub := s.hubInstance()
	if hub == nil {
		fmt.Fprint(conn, "ERR no such card\n")
		return
	}

	var session, cred string
	for _, card := range hub.Pending() {
		if card.ID == cardID {
			session = card.Cell
			cred = card.Cred
			break
		}
	}

	if session == "" {
		if sid, ok := hub.SessionOf(cardID); ok {
			session = sid
		}
	}

	workers := s.workersInstance()
	if cred == "" && workers != nil && workers.parkMgr != nil {
		workers.parkMgr.mu.Lock()
		for _, cellParks := range workers.parkMgr.byCell {
			if flow, ok := cellParks[cardID]; ok {
				cred = flow.cred
				if session == "" {
					session = flow.cellID
				}
				break
			}
		}
		workers.parkMgr.mu.Unlock()
	}

	proxy := s.proxyInstance()
	if cred == "" && proxy != nil && proxy.parkMgr != nil {
		proxy.parkMgr.mu.Lock()
		for _, cellParks := range proxy.parkMgr.byCell {
			if flow, ok := cellParks[cardID]; ok {
				cred = flow.cred
				if session == "" {
					session = flow.cellID
				}
				break
			}
		}
		proxy.parkMgr.mu.Unlock()
	}

	status := hub.ResolveFrom(session, cardID, allow, "ctl")
	switch status {
	case 200:
		if allow {
			if s.vault != nil && s.vault.Audit() != nil {
				_ = s.vault.Audit().AppendApprovalAnswered(cred, "ok")
			}
			fmt.Fprint(conn, "OK approved\n")
		} else {
			if s.vault != nil && s.vault.Audit() != nil {
				_ = s.vault.Audit().AppendApprovalAnswered(cred, "denied")
			}
			fmt.Fprint(conn, "OK denied\n")
		}
	case 410:
		fmt.Fprint(conn, "ERR already answered\n")
	case 404:
		fmt.Fprint(conn, "ERR no such card\n")
	default:
		fmt.Fprintf(conn, "ERR %d\n", status)
	}
}
