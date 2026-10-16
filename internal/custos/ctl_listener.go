package custos

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"
)

func (s *CtlServer) handleListenerCmd(conn net.Conn, cmd string, parts []string) bool {
	proxy := s.proxyInstance()
	switch strings.ToUpper(cmd) {
	case "BIND-LISTENER", "BIND_LISTENER":
		if len(parts) < 3 {
			fmt.Fprint(conn, "ERR bad_request\n")
			return true
		}
		if proxy == nil {
			fmt.Fprint(conn, "ERR proxy_not_configured\n")
			return true
		}
		arg := strings.TrimSpace(parts[2])
		var addr, cellID, peer, logPath string
		if strings.HasPrefix(arg, "{") {
			var req struct {
				Addr    string `json:"addr"`
				CellID  string `json:"cell_id"`
				Peer    string `json:"peer"`
				LogPath string `json:"log_path"`
			}
			if err := json.Unmarshal([]byte(arg), &req); err != nil {
				fmt.Fprintf(conn, "ERR %s\n", err.Error())
				return true
			}
			addr, cellID, peer, logPath = req.Addr, req.CellID, req.Peer, req.LogPath
		} else {
			fields := strings.Fields(arg)
			addr = fields[0]
			if len(fields) > 1 {
				cellID = fields[1]
			}
			if len(fields) > 2 {
				peer = fields[2]
			}
			if len(fields) > 3 {
				logPath = fields[3]
			}
		}
		if err := proxy.BindListener(addr, cellID, peer, logPath); err != nil {
			fmt.Fprintf(conn, "ERR %s\n", err.Error())
			return true
		}
		fmt.Fprint(conn, "OK listener_bound\n")
		return true

	case "CLOSE-LISTENER", "CLOSE_LISTENER":
		if len(parts) < 3 {
			fmt.Fprint(conn, "ERR bad_request\n")
			return true
		}
		if proxy == nil {
			fmt.Fprint(conn, "ERR proxy_not_configured\n")
			return true
		}
		arg := strings.TrimSpace(parts[2])
		var addr string
		if strings.HasPrefix(arg, "{") {
			var req struct {
				Addr string `json:"addr"`
			}
			if err := json.Unmarshal([]byte(arg), &req); err != nil {
				fmt.Fprintf(conn, "ERR %s\n", err.Error())
				return true
			}
			addr = req.Addr
		} else {
			addr = strings.Fields(arg)[0]
		}
		if err := proxy.CloseListener(addr); err != nil {
			fmt.Fprintf(conn, "ERR %s\n", err.Error())
			return true
		}
		fmt.Fprint(conn, "OK listener_closed\n")
		return true

	case "LIST-LISTENERS", "LIST_LISTENERS":
		if proxy == nil {
			fmt.Fprint(conn, "OK []\n")
			return true
		}
		list := proxy.ListListeners()
		b, err := json.Marshal(list)
		if err != nil {
			fmt.Fprintf(conn, "ERR %s\n", err.Error())
			return true
		}
		fmt.Fprintf(conn, "OK %s\n", string(b))
		return true

	default:
		return false
	}
}
