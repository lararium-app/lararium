package custos

import (
	"encoding/json"
	"fmt"
	"net"
)

func (s *CtlServer) handleSurrogateCmd(conn net.Conn, cmd string, parts []string) bool {
	switch cmd {
	case "SURROGATE-ADD":
		if len(parts) < 3 {
			fmt.Fprint(conn, "ERR bad_request\n")
			return true
		}
		var req struct {
			Credential  string `json:"credential"`
			Host        string `json:"host"`
			Port        int    `json:"port"`
			PathPrefix  string `json:"path_prefix"`
			AllowBinary bool   `json:"allow_binary"`
		}
		if err := json.Unmarshal([]byte(parts[2]), &req); err != nil {
			fmt.Fprintf(conn, "ERR %s\n", err.Error())
			return true
		}
		tok, err := s.vault.AddSurrogate("", req.Credential, req.Host, req.Port, req.PathPrefix, req.AllowBinary, "cli")
		if err != nil {
			fmt.Fprintf(conn, "ERR %s\n", err.Error())
			return true
		}
		fmt.Fprintf(conn, "OK %s\n", tok)
		return true

	case "SURROGATE-LIST":
		list, err := s.vault.ListSurrogates()
		if err != nil {
			fmt.Fprintf(conn, "ERR %s\n", err.Error())
			return true
		}
		b, err := json.Marshal(list)
		if err != nil {
			fmt.Fprintf(conn, "ERR %s\n", err.Error())
			return true
		}
		fmt.Fprintf(conn, "OK %s\n", string(b))
		return true

	case "SURROGATE-REVOKE":
		if len(parts) < 3 {
			fmt.Fprint(conn, "ERR bad_request\n")
			return true
		}
		if err := s.vault.RevokeSurrogate("", parts[2], "cli"); err != nil {
			fmt.Fprintf(conn, "ERR %s\n", err.Error())
			return true
		}
		fmt.Fprint(conn, "OK surrogate_revoked\n")
		return true

	default:
		return false
	}
}
