package surface

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/lararium-app/lararium/docker/hearth"
	"github.com/lararium-app/lararium/internal/penatus"
)

// ProfileAuditChange records one applied persona file edit for audit logging
// without carrying any personal content.
type ProfileAuditChange struct {
	Field    string
	OldBytes int
	NewBytes int
}

type profileFieldSpec struct {
	field     string
	filename  string
	budget    int
	hasBudget bool
}

// Budgets match internal/loop/prompt.go and RPC-SPEC §4b (lines 146-147).
// memory enforces an 8192-byte body cap on write, while profile.describe
// intentionally omits memory_budget per spec line 146.
var profileFields = []profileFieldSpec{
	{field: "soul", filename: "SOUL.md", budget: 4096, hasBudget: true},
	{field: "identity", filename: "IDENTITY.md", budget: 2048, hasBudget: true},
	{field: "user", filename: "USER.md", budget: 4096, hasBudget: true},
	{field: "memory", filename: "MEMORY.md", budget: 8192, hasBudget: false},
}

// rpcProfileDescribe implements profile.describe (RPC-SPEC §4b).
// Revisions are file modification times in Unix milliseconds (fits JS safe
// integers; external SSH edits naturally invalidate CAS). A missing file has
// revision 0 and omits its content and *_bytes fields.
func (s *Server) rpcProfileDescribe(_ context.Context, conn *RPCConn, req *RPCRequest) {
	if s.PersonaHome == "" {
		_ = conn.SendError(*req.ID, "profile unavailable", "not_implemented", nil)
		return
	}

	res := map[string]any{}
	revisions := map[string]int64{}

	for _, spec := range profileFields {
		if spec.hasBudget {
			res[spec.field+"_budget"] = spec.budget
		}
		path := filepath.Join(s.PersonaHome, spec.filename)
		info, err := os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				revisions[spec.field] = 0
				continue
			}
			log.Printf("surface: rpc profile.describe stat %s failed: %v", spec.filename, err)
			_ = conn.SendError(*req.ID, "internal error", "internal_error", nil)
			return
		}
		rawBytes, err := os.ReadFile(path)
		if err != nil {
			log.Printf("surface: rpc profile.describe read %s failed: %v", spec.filename, err)
			_ = conn.SendError(*req.ID, "internal error", "internal_error", nil)
			return
		}

		// File mtime in Unix milliseconds (fits JS safe integers; external edits
		// invalidate CAS).
		revisions[spec.field] = info.ModTime().UnixMilli()
		res[spec.field+"_bytes"] = len(rawBytes)

		if doc, perr := penatus.ParseDoc(rawBytes); perr == nil {
			res[spec.field] = doc.Body
		} else {
			// Parse failure: return raw file bytes so the editor shows the truth.
			res[spec.field] = string(rawBytes)
		}
	}

	res["revisions"] = revisions
	_ = conn.SendResult(*req.ID, res)
}

// rpcProfileConfigure implements profile.configure (RPC-SPEC §4b).
// Params: {soul?, identity?, user?, memory?, expected_revisions?:{field:int}}.
//
// Each field is validated and applied independently; a bad field never fails
// the whole call. Missing files have revision 0 (so expected_revisions[field]
// != 0 yields "revision_conflict"). Existing files with unparseable
// frontmatter return "invalid" rather than clobbering operator-visible data.
func (s *Server) rpcProfileConfigure(_ context.Context, conn *RPCConn, req *RPCRequest) {
	if s.PersonaHome == "" {
		_ = conn.SendError(*req.ID, "profile unavailable", "not_implemented", nil)
		return
	}

	trimmed := bytes.TrimSpace(req.Params)
	var raw map[string]json.RawMessage
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || json.Unmarshal(trimmed, &raw) != nil || raw == nil {
		_ = conn.SendError(*req.ID, "invalid request body", "invalid_request", nil)
		return
	}

	for k := range raw {
		switch k {
		case "soul", "identity", "user", "memory", "expected_revisions":
		default:
			_ = conn.SendError(*req.ID, "invalid request body", "invalid_request", nil)
			return
		}
	}

	var expectedRevs map[string]int64
	if rawExp, ok := raw["expected_revisions"]; ok && !bytes.Equal(bytes.TrimSpace(rawExp), []byte("null")) {
		if err := json.Unmarshal(rawExp, &expectedRevs); err != nil {
			_ = conn.SendError(*req.ID, "invalid request body", "invalid_request", nil)
			return
		}
	}

	s.profileMu.Lock()
	defer s.profileMu.Unlock()

	applied := map[string]string{}
	var changes []ProfileAuditChange

	for _, spec := range profileFields {
		rawVal, present := raw[spec.field]
		if !present {
			continue
		}
		valTrimmed := bytes.TrimSpace(rawVal)
		if bytes.Equal(valTrimmed, []byte("null")) {
			applied[spec.field] = "invalid"
			continue
		}
		var body string
		if err := json.Unmarshal(valTrimmed, &body); err != nil {
			applied[spec.field] = "invalid"
			continue
		}
		if strings.TrimSpace(body) == "" {
			applied[spec.field] = "invalid"
			continue
		}
		if len(body) > spec.budget {
			applied[spec.field] = "over_budget"
			continue
		}

		path := filepath.Join(s.PersonaHome, spec.filename)
		var curRev int64
		var oldBytes int
		var fileExists bool
		var existingRaw []byte

		info, statErr := os.Stat(path)
		if statErr == nil {
			fileExists = true
			curRev = info.ModTime().UnixMilli()
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				log.Printf("surface: rpc profile.configure read %s failed: %v", spec.filename, rerr)
				applied[spec.field] = "invalid"
				continue
			}
			existingRaw = b
			oldBytes = len(b)
		} else if !os.IsNotExist(statErr) {
			log.Printf("surface: rpc profile.configure stat %s failed: %v", spec.filename, statErr)
			applied[spec.field] = "invalid"
			continue
		}

		// CAS check: missing file has revision 0.
		if expRev, hasExp := expectedRevs[spec.field]; hasExp && expRev != curRev {
			applied[spec.field] = "revision_conflict"
			continue
		}

		var doc penatus.Doc
		if fileExists {
			parsed, perr := penatus.ParseDoc(existingRaw)
			if perr != nil {
				// Never clobber a file with unparseable frontmatter.
				applied[spec.field] = "invalid"
				continue
			}
			doc = parsed
		} else {
			tmplBytes, terr := hearth.Templates.ReadFile(spec.filename)
			if terr != nil {
				log.Printf("surface: rpc profile.configure read template %s failed: %v", spec.filename, terr)
				applied[spec.field] = "invalid"
				continue
			}
			parsed, perr := penatus.ParseDoc(tmplBytes)
			if perr != nil {
				log.Printf("surface: rpc profile.configure parse template %s failed: %v", spec.filename, perr)
				applied[spec.field] = "invalid"
				continue
			}
			doc = parsed
		}

		doc.Body = body
		out := doc.Marshal()

		if err := os.MkdirAll(s.PersonaHome, 0o755); err != nil {
			log.Printf("surface: rpc profile.configure mkdir failed: %v", err)
			applied[spec.field] = "invalid"
			continue
		}
		tmpPath := path + ".tmp"
		if err := os.WriteFile(tmpPath, out, 0o644); err != nil { //nolint:gosec // persona files are 0644 matching setup scaffolding
			log.Printf("surface: rpc profile.configure write %s failed: %v", spec.filename, err)
			applied[spec.field] = "invalid"
			continue
		}
		if err := os.Rename(tmpPath, path); err != nil {
			_ = os.Remove(tmpPath)
			log.Printf("surface: rpc profile.configure rename %s failed: %v", spec.filename, err)
			applied[spec.field] = "invalid"
			continue
		}

		applied[spec.field] = "ok"
		changes = append(changes, ProfileAuditChange{
			Field:    spec.field,
			OldBytes: oldBytes,
			NewBytes: len(out),
		})
	}

	if len(changes) > 0 {
		parts := make([]string, 0, len(changes))
		for _, ch := range changes {
			parts = append(parts, fmt.Sprintf("%s:%d->%d", ch.Field, ch.OldBytes, ch.NewBytes))
		}
		log.Printf("surface: audit profile.configure actor=web changes=%s", strings.Join(parts, ","))
		if s.ProfileAudit != nil {
			s.ProfileAudit("web", changes)
		}
	}

	_ = conn.SendResult(*req.ID, map[string]any{
		"ok":      true,
		"applied": applied,
	})
}
