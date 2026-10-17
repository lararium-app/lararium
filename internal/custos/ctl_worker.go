package custos

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
)

// handleWorkerCmd serves the slice-4 worker-lane ctl ops (§5.1, §4.5):
// BIND-CELL binds a per-cell custos.sock; UNBIND-CELL releases it;
// LOGIN-STORE stores an exchanged oauth2 grant through the Mutate path.
func (s *CtlServer) handleWorkerCmd(conn net.Conn, cmd string, parts []string) bool {
	workers := s.workersInstance()
	switch strings.ToUpper(cmd) {
	case "BIND-CELL", "BIND_CELL":
		if len(parts) < 3 {
			fmt.Fprint(conn, "ERR bad_request\n")
			return true
		}
		if workers == nil {
			fmt.Fprint(conn, "ERR workers_not_configured\n")
			return true
		}
		var req struct {
			CellID string `json:"cell_id"`
			Sock   string `json:"sock"`
			Token  string `json:"token"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(parts[2])), &req); err != nil {
			fmt.Fprint(conn, "ERR bad_request\n")
			return true
		}
		if err := workers.BindCell(req.CellID, req.Sock, req.Token); err != nil {
			fmt.Fprint(conn, "ERR bind_failed\n")
			return true
		}
		fmt.Fprint(conn, "OK cell_bound\n")
		return true

	case "UNBIND-CELL", "UNBIND_CELL":
		if len(parts) < 3 {
			fmt.Fprint(conn, "ERR bad_request\n")
			return true
		}
		if workers == nil {
			fmt.Fprint(conn, "ERR workers_not_configured\n")
			return true
		}
		cellID := strings.TrimSpace(parts[2])
		if err := workers.UnbindCell(cellID); err != nil {
			fmt.Fprint(conn, "ERR unbind_failed\n")
			return true
		}
		fmt.Fprint(conn, "OK cell_unbound\n")
		return true

	case "LOGIN-STORE", "LOGIN_STORE":
		if len(parts) < 3 {
			fmt.Fprint(conn, "ERR bad_request\n")
			return true
		}
		var req struct {
			Name string     `json:"name"`
			Cred Credential `json:"cred"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(parts[2])), &req); err != nil || req.Name == "" {
			fmt.Fprint(conn, "ERR bad_request\n")
			return true
		}
		// Overwriting an existing credential is a rotation: audit it with
		// the frozen credential_rotated kind (§8.1) so a same-name replace
		// is distinguishable from a first store in the chain.
		_, existed := s.vault.GetCredential(req.Name)
		if err := s.vault.StoreOAuthGrant("", req.Name, req.Cred, "cli"); err != nil {
			// Name grammar (CA-1(e)) and the store cap surface as distinct
			// refusals; everything else is a generic store failure.
			switch {
			case errors.Is(err, ErrBadName):
				fmt.Fprint(conn, "ERR bad_name\n")
			case errors.Is(err, ErrFull):
				fmt.Fprint(conn, "ERR store_full\n")
			default:
				fmt.Fprint(conn, "ERR store_failed\n")
			}
			return true
		}
		if existed {
			_ = s.vault.Audit().Append(AuditRecord{
				Kind:  AuditKindCredentialRotated,
				Cred:  req.Name,
				Actor: "cli",
			})
			fmt.Fprint(conn, "OK credential_rotated\n")
			return true
		}
		fmt.Fprint(conn, "OK credential_added\n")
		return true

	default:
		return false
	}
}
