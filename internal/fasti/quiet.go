package fasti

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParseQuietWindow parses an "HH:MM-HH:MM" window string into start and end hours/minutes.
func ParseQuietWindow(window string) (sh, sm, eh, em int, ok bool) {
	if !quietSpanRegex.MatchString(window) {
		return 0, 0, 0, 0, false
	}
	parts := strings.Split(window, "-")
	startParts := strings.Split(parts[0], ":")
	endParts := strings.Split(parts[1], ":")
	sh, _ = strconv.Atoi(startParts[0])
	sm, _ = strconv.Atoi(startParts[1])
	eh, _ = strconv.Atoi(endParts[0])
	em, _ = strconv.Atoi(endParts[1])
	return sh, sm, eh, em, true
}

// IsInQuietWindow checks whether instant t falls inside the local "HH:MM-HH:MM" window.
// The window has inclusive start, exclusive end, may span midnight, and is evaluated in loc (spec D5/D7).
// An empty window string means no window (returns false).
func IsInQuietWindow(window string, t time.Time, loc *time.Location) bool {
	if strings.TrimSpace(window) == "" {
		return false
	}
	if loc == nil {
		loc = time.Local
	}
	sh, sm, eh, em, ok := ParseQuietWindow(window)
	if !ok {
		return false
	}
	startM := sh*60 + sm
	endM := eh*60 + em
	if startM == endM {
		return false
	}

	tl := t.In(loc)
	curM := tl.Hour()*60 + tl.Minute()

	if startM < endM {
		return curM >= startM && curM < endM
	}
	// Spans midnight (e.g. 22:00-07:00)
	return curM >= startM || curM < endM
}

// QuietWindowEnd calculates the exact end instant of the active quiet window covering t.
// If t is currently inside the window, it returns when this active window ends in loc.
// If t is not in the window, it returns when the next quiet window ends.
func QuietWindowEnd(window string, t time.Time, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.Local
	}
	sh, sm, eh, em, ok := ParseQuietWindow(window)
	if !ok {
		return t
	}
	startM := sh*60 + sm
	endM := eh*60 + em
	if startM == endM {
		return t
	}

	tl := t.In(loc)
	curM := tl.Hour()*60 + tl.Minute()

	if startM < endM {
		// Does not span midnight (e.g. 09:00-17:00)
		if curM < endM {
			return time.Date(tl.Year(), tl.Month(), tl.Day(), eh, em, 0, 0, loc)
		}
		// Current time is past today's window end, next window ends tomorrow
		return time.Date(tl.Year(), tl.Month(), tl.Day()+1, eh, em, 0, 0, loc)
	}

	// Spans midnight (e.g. 22:00-07:00)
	if curM < endM {
		// After midnight, before end (e.g. 06:30): ends today at endM
		return time.Date(tl.Year(), tl.Month(), tl.Day(), eh, em, 0, 0, loc)
	}
	// Before midnight or during the day: window ends tomorrow at endM
	return time.Date(tl.Year(), tl.Month(), tl.Day()+1, eh, em, 0, 0, loc)
}

// EffectiveQuiet determines the quiet window and mode for a job, falling back to engine baseline (spec D5/D7).
// Baseline quiet defaults to mode "announce_only".
func EffectiveQuiet(job *Job, baseline string) (window string, mode string) {
	if job != nil && job.Quiet != nil && strings.TrimSpace(job.Quiet.Window) != "" {
		w := strings.TrimSpace(job.Quiet.Window)
		m := strings.TrimSpace(job.Quiet.Mode)
		if m == "" {
			m = "announce_only" // Spec D5: default announce_only
		}
		return w, m
	}
	if strings.TrimSpace(baseline) != "" {
		return strings.TrimSpace(baseline), "announce_only"
	}
	return "", "announce_only"
}

// AutoPauseNotice formats the fixed template for the 5-skip auto-pause operational notice (spec D3/D7).
// Templates are machine text, containing name and counts only, no model text.
func AutoPauseNotice(job *Job) string {
	name := job.Name
	if strings.TrimSpace(name) == "" {
		name = job.ID
	}
	return fmt.Sprintf("Job %q paused after %d consecutive skips", name, job.ConsecutiveSkips)
}

// NoiseDigestNotice formats the fixed template for the noise budget digest operational notice (spec D7).
func NoiseDigestNotice(overflowDays int) string {
	return fmt.Sprintf("Notice: noise budget overflow on %d days in past 7 days", overflowDays)
}
