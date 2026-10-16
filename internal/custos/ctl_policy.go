package custos

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"
)

func (s *CtlServer) handlePolicyCmd(conn net.Conn, cmd string, parts []string) bool {
	switch cmd {
	case "POLICY-ADD":
		if len(parts) < 3 {
			fmt.Fprint(conn, "ERR bad_request\n")
			return true
		}
		var req struct {
			Lane    string `json:"lane"`
			Pattern string `json:"pattern"`
			Verdict string `json:"verdict"`
		}
		if err := json.Unmarshal([]byte(parts[2]), &req); err != nil {
			fmt.Fprintf(conn, "ERR %s\n", err.Error())
			return true
		}
		var notes []string
		var err error
		if req.Lane == "egress" {
			notes, err = s.vault.Policy().AddEgressRule(req.Pattern, req.Verdict, false, "cli", "cli")
		} else {
			notes, err = s.vault.Policy().AddCredentialRule(req.Pattern, req.Verdict, false, "cli", "cli")
		}
		if err != nil {
			fmt.Fprintf(conn, "ERR %s\n", err.Error())
			return true
		}
		fmt.Fprintf(conn, "OK %s\n", strings.Join(notes, "\x00"))
		return true

	case "POLICY-LIST":
		rows := s.vault.Policy().ListRules()
		b, err := json.Marshal(rows)
		if err != nil {
			fmt.Fprintf(conn, "ERR %s\n", err.Error())
			return true
		}
		fmt.Fprintf(conn, "OK %s\n", string(b))
		return true

	case "POLICY-RM":
		if len(parts) < 3 {
			fmt.Fprint(conn, "ERR bad_request\n")
			return true
		}
		var lane, pat string
		if strings.Contains(parts[2], "\x00") {
			sub := strings.SplitN(parts[2], "\x00", 2)
			lane, pat = sub[0], sub[1]
		} else {
			pat = parts[2]
		}
		removed, err := s.vault.Policy().RemoveRule(lane, pat, "cli")
		if err != nil {
			fmt.Fprintf(conn, "ERR %s\n", err.Error())
			return true
		}
		if !removed {
			fmt.Fprint(conn, "ERR rule not found\n")
			return true
		}
		fmt.Fprint(conn, "OK removed\n")
		return true

	case "POLICY-RESET":
		if err := s.vault.Policy().Reset("cli"); err != nil {
			fmt.Fprintf(conn, "ERR %s\n", err.Error())
			return true
		}
		fmt.Fprint(conn, "OK always-rules cleared\n")
		return true

	case "EGRESS-STRICT":
		if len(parts) < 3 {
			fmt.Fprint(conn, "ERR bad_request\n")
			return true
		}
		strict := parts[2] == "on"
		if err := s.vault.Policy().SetEgressStrict(strict, "cli"); err != nil {
			fmt.Fprintf(conn, "ERR %s\n", err.Error())
			return true
		}
		fmt.Fprintf(conn, "OK egress strict: %s\n", parts[2])
		return true

	default:
		return false
	}
}
