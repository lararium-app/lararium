package fasti

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// HeartbeatInfo contains the parsed frontmatter and body checklist from HEARTBEAT.md (spec D6, PENATUS-SPEC §1.5).
type HeartbeatInfo struct {
	Cadence   string
	Quiet     string
	Checklist string
	Enabled   bool
}

// HeartbeatFilePath returns the standard path to <hearth>/HEARTBEAT.md.
func HeartbeatFilePath(home string) string {
	return filepath.Join(home, "HEARTBEAT.md")
}

// ParseHeartbeat parses the frontmatter and checklist body from HEARTBEAT.md data.
// Frontmatter lines can be enclosed in --- fences or leading key: value lines.
// Missing file, unreadable, empty/blank body, or cadence "off" => Enabled = false (spec D6, R8).
func ParseHeartbeat(data []byte) HeartbeatInfo {
	content := string(data)
	lines := strings.Split(content, "\n")

	cadence := ""
	quiet := ""
	var bodyLines []string

	firstNonEmpty := -1
	for i, l := range lines {
		if strings.TrimSpace(l) != "" {
			firstNonEmpty = i
			break
		}
	}

	if firstNonEmpty >= 0 && strings.TrimSpace(lines[firstNonEmpty]) == "---" {
		// YAML frontmatter delimited by ---
		sawClosing := false
		for i := firstNonEmpty + 1; i < len(lines); i++ {
			trimmed := strings.TrimSpace(lines[i])
			if trimmed == "---" {
				sawClosing = true
				bodyLines = lines[i+1:]
				break
			}
			parseHeartbeatFrontmatterLine(trimmed, &cadence, &quiet)
		}
		if !sawClosing {
			bodyLines = nil
		}
	} else {
		// Leading frontmatter lines without --- delimiters
		for i, line := range lines {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				continue
			}
			if !strings.HasPrefix(trimmed, "cadence:") && !strings.HasPrefix(trimmed, "quiet:") &&
				!strings.HasPrefix(trimmed, "penatus:") && !strings.HasPrefix(trimmed, "updated:") {
				bodyLines = lines[i:]
				break
			}
			parseHeartbeatFrontmatterLine(trimmed, &cadence, &quiet)
		}
	}

	checklist := strings.TrimSpace(strings.Join(bodyLines, "\n"))

	if cadence == "" {
		cadence = "daily" // Spec D6: default daily when file exists and line absent
	}
	cadence = strings.ToLower(strings.TrimSpace(cadence))

	enabled := cadence != "off" && checklist != ""

	return HeartbeatInfo{
		Cadence:   cadence,
		Quiet:     quiet,
		Checklist: checklist,
		Enabled:   enabled,
	}
}

func parseHeartbeatFrontmatterLine(line string, cadence, quiet *string) {
	if strings.HasPrefix(line, "cadence:") {
		val := strings.TrimSpace(strings.TrimPrefix(line, "cadence:"))
		val = strings.Trim(val, `"'`)
		*cadence = val
	} else if strings.HasPrefix(line, "quiet:") {
		val := strings.TrimSpace(strings.TrimPrefix(line, "quiet:"))
		val = strings.Trim(val, `"'`)
		*quiet = val
	}
}

// ReadHeartbeatInfo reads <hearth>/HEARTBEAT.md FRESH from disk (no caching; spec D6, R8).
// The daemon NEVER writes this file.
func ReadHeartbeatInfo(home string) HeartbeatInfo {
	data, err := os.ReadFile(HeartbeatFilePath(home))
	if err != nil {
		return HeartbeatInfo{Enabled: false}
	}
	return ParseHeartbeat(data)
}

// BuildHeartbeatPrompt formats the operator-tier prompt consisting of checklist lines verbatim
// plus the fixed frame instruction (spec D6).
func BuildHeartbeatPrompt(checklist string) string {
	return strings.TrimSpace(checklist) + "\n\nEvaluate each item, act where able; if nothing is worth sending, reply HEARTBEAT_OK."
}

// IsHeartbeatOK reports whether the reply matches HEARTBEAT_OK (exact, trimmed, case-insensitive; spec D6).
func IsHeartbeatOK(reply string) bool {
	return strings.EqualFold(strings.TrimSpace(reply), "HEARTBEAT_OK")
}

func parseDailyAt(dailyAt string) (int, int) {
	h, m := 7, 30
	if strings.TrimSpace(dailyAt) != "" {
		parts := strings.Split(dailyAt, ":")
		if len(parts) == 2 {
			parsedH, errH := strconv.Atoi(parts[0])
			parsedM, errM := strconv.Atoi(parts[1])
			if errH == nil && errM == nil {
				h, m = parsedH, parsedM
			}
		}
	}
	return h, m
}

// CalculateNextHeartbeatWake computes the next due time strictly after 'from' in loc (spec D6).
// hourly = on the hour (local); daily = at dailyAt ("HH:MM", default 07:30) local.
func CalculateNextHeartbeatWake(info HeartbeatInfo, from time.Time, loc *time.Location, dailyAt string) (time.Time, bool) {
	if !info.Enabled {
		return time.Time{}, false
	}
	if loc == nil {
		loc = time.Local //nolint:gosmopolitan // spec D11: unset fasti.local_timezone means the system zone
	}
	tl := from.In(loc)

	switch info.Cadence {
	case "hourly":
		next := time.Date(tl.Year(), tl.Month(), tl.Day(), tl.Hour()+1, 0, 0, 0, loc)
		return next, true
	case "daily":
		h, m := parseDailyAt(dailyAt)
		cand := time.Date(tl.Year(), tl.Month(), tl.Day(), h, m, 0, 0, loc)
		if !cand.After(tl) {
			cand = time.Date(tl.Year(), tl.Month(), tl.Day()+1, h, m, 0, 0, loc)
		}
		return cand, true
	default:
		return time.Time{}, false
	}
}
