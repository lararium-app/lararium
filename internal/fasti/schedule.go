package fasti

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// QuietOverride specifies per-job quiet hours configuration (spec D5).
type QuietOverride struct {
	Window string `json:"window"` // "HH:MM-HH:MM" local
	Mode   string `json:"mode"`   // "defers" or "announce_only"
}

// Schedule describes a cron or interval recurring pattern (spec D4/D5).
type Schedule struct {
	Kind  string `json:"kind"`            // "cron" or "interval"
	Expr  string `json:"expr,omitempty"`  // 5-field cron expression: M H DOM MON DOW
	Every string `json:"every,omitempty"` // interval string, e.g. "30m"
}

// NextDueResult contains the computed next due instant and whether it fell in a DST gap.
type NextDueResult struct {
	Due        time.Time
	IsDstGap   bool
	SkippedDue time.Time
}

// CronSchedule is a parsed 5-field cron schedule.
type CronSchedule struct {
	raw         string
	minutes     [60]bool
	hours       [24]bool
	dom         [32]bool // 1..31
	months      [13]bool // 1..12
	dow         [8]bool  // 0..7 (0=Sun, 7=Sun)
	domWildcard bool
	dowWildcard bool
}

var monthNames = map[string]int{
	"JAN": 1, "FEB": 2, "MAR": 3, "APR": 4, "MAY": 5, "JUN": 6,
	"JUL": 7, "AUG": 8, "SEP": 9, "OCT": 10, "NOV": 11, "DEC": 12,
}

var dowNames = map[string]int{
	"SUN": 0, "MON": 1, "TUE": 2, "WED": 3, "THU": 4, "FRI": 5, "SAT": 6,
}

// ParseSchedule validates and parses a Schedule specification.
func ParseSchedule(s Schedule) (*ParsedSchedule, error) {
	kind := strings.ToLower(strings.TrimSpace(s.Kind))
	switch kind {
	case "cron":
		expr := strings.TrimSpace(s.Expr)
		if expr == "" {
			return nil, errors.New("cron expression must not be empty")
		}
		cs, err := ParseCron(expr)
		if err != nil {
			return nil, err
		}
		return &ParsedSchedule{
			schedule: s,
			cron:     cs,
		}, nil
	case "interval":
		every := strings.TrimSpace(s.Every)
		if every == "" {
			return nil, errors.New("interval 'every' must not be empty")
		}
		dur, err := parseIntervalDuration(every)
		if err != nil {
			return nil, err
		}
		const minDur = 1 * time.Minute
		const maxDur = 365 * 24 * time.Hour
		if dur < minDur || dur > maxDur {
			return nil, fmt.Errorf("interval %v out of bounds (1m..1yr)", dur)
		}
		return &ParsedSchedule{
			schedule: s,
			interval: dur,
		}, nil
	default:
		return nil, fmt.Errorf("unknown schedule kind %q (must be 'cron' or 'interval')", s.Kind)
	}
}

func parseIntervalDuration(s string) (time.Duration, error) {
	trimmed := strings.TrimSpace(s)
	if strings.HasSuffix(trimmed, "d") {
		daysStr := strings.TrimSuffix(trimmed, "d")
		days, err := strconv.Atoi(daysStr)
		if err != nil || days <= 0 {
			return 0, fmt.Errorf("invalid interval days %q", s)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	dur, err := time.ParseDuration(trimmed)
	if err != nil {
		return 0, fmt.Errorf("parse interval %q: %w", s, err)
	}
	return dur, nil
}

// ParseCron parses a 5-field cron expression: M H DOM MON DOW.
func ParseCron(expr string) (*CronSchedule, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron expression %q must have exactly 5 fields, got %d", expr, len(fields))
	}

	cs := &CronSchedule{raw: expr}

	minSet, _, err := parseCronField(fields[0], 0, 59, nil)
	if err != nil {
		return nil, fmt.Errorf("cron minute field %q: %w", fields[0], err)
	}
	copy(cs.minutes[:], minSet[:])

	hourSet, _, err := parseCronField(fields[1], 0, 23, nil)
	if err != nil {
		return nil, fmt.Errorf("cron hour field %q: %w", fields[1], err)
	}
	copy(cs.hours[:], hourSet[:])

	domSet, domWild, err := parseCronField(fields[2], 1, 31, nil)
	if err != nil {
		return nil, fmt.Errorf("cron day-of-month field %q: %w", fields[2], err)
	}
	copy(cs.dom[:], domSet[:])
	cs.domWildcard = domWild

	monSet, _, err := parseCronField(fields[3], 1, 12, monthNames)
	if err != nil {
		return nil, fmt.Errorf("cron month field %q: %w", fields[3], err)
	}
	copy(cs.months[:], monSet[:])

	dowSet, dowWild, err := parseCronField(fields[4], 0, 7, dowNames)
	if err != nil {
		return nil, fmt.Errorf("cron day-of-week field %q: %w", fields[4], err)
	}
	copy(cs.dow[:], dowSet[:])
	cs.dowWildcard = dowWild

	return cs, nil
}

func parseCronField(spec string, minVal, maxVal int, names map[string]int) ([64]bool, bool, error) {
	var match [64]bool
	trimmed := strings.TrimSpace(spec)
	if trimmed == "" {
		return match, false, errors.New("empty field")
	}

	if trimmed == "*" {
		for i := minVal; i <= maxVal; i++ {
			match[i] = true
		}
		return match, true, nil
	}

	items := strings.Split(trimmed, ",")
	isStar := false
	if len(items) == 1 {
		item := strings.TrimSpace(items[0])
		if strings.Contains(item, "/") {
			parts := strings.Split(item, "/")
			if len(parts) == 2 && strings.TrimSpace(parts[0]) == "*" {
				// Spec D4: "steps (*/5, 1-30/2) — standard Vixie-cron semantics, DOM/DOW OR-joined when both restricted".
				// Vixie keeps the star bit when the field begins with '*'. Base == "*" with step returns star=true.
				isStar = true
			}
		}
	}
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			return match, false, errors.New("empty item in list")
		}

		var base string
		step := 1
		if strings.Contains(item, "/") {
			parts := strings.Split(item, "/")
			if len(parts) != 2 {
				return match, false, fmt.Errorf("invalid step syntax %q", item)
			}
			base = parts[0]
			s, err := strconv.Atoi(parts[1])
			if err != nil || s <= 0 {
				return match, false, fmt.Errorf("invalid step value %q", parts[1])
			}
			step = s
		} else {
			base = item
		}

		var start, end int
		switch {
		case base == "*":
			start = minVal
			end = maxVal
		case strings.Contains(base, "-"):
			parts := strings.Split(base, "-")
			if len(parts) != 2 {
				return match, false, fmt.Errorf("invalid range syntax %q", base)
			}
			s, err := parseFieldValue(parts[0], minVal, maxVal, names)
			if err != nil {
				return match, false, err
			}
			e, err := parseFieldValue(parts[1], minVal, maxVal, names)
			if err != nil {
				return match, false, err
			}
			if s > e {
				return match, false, fmt.Errorf("invalid range start %d > end %d", s, e)
			}
			start = s
			end = e
		default:
			val, err := parseFieldValue(base, minVal, maxVal, names)
			if err != nil {
				return match, false, err
			}
			if step > 1 {
				start = val
				end = maxVal
			} else {
				start = val
				end = val
			}
		}

		for v := start; v <= end; v += step {
			if v >= minVal && v <= maxVal {
				match[v] = true
			}
		}
	}

	return match, isStar, nil
}

func parseFieldValue(s string, minVal, maxVal int, names map[string]int) (int, error) {
	upper := strings.ToUpper(strings.TrimSpace(s))
	if names != nil {
		if val, ok := names[upper]; ok {
			return val, nil
		}
	}
	val, err := strconv.Atoi(upper)
	if err != nil {
		return 0, fmt.Errorf("invalid value %q", s)
	}
	if val < minVal || val > maxVal {
		return 0, fmt.Errorf("value %d out of range [%d, %d]", val, minVal, maxVal)
	}
	return val, nil
}

// matchDay applies Vixie-cron DOM/DOW OR-join semantics (spec D4).
func (c *CronSchedule) matchDay(t time.Time) bool {
	day := t.Day()
	weekday := int(t.Weekday())

	domMatch := c.dom[day]
	dowMatch := c.dow[weekday] || (weekday == 0 && c.dow[7])

	if !c.domWildcard && !c.dowWildcard {
		// Both restricted: OR semantics
		return domMatch || dowMatch
	}
	if !c.domWildcard {
		return domMatch
	}
	if !c.dowWildcard {
		return dowMatch
	}
	return true
}

// Matches reports whether t corresponds to a scheduled firing instant in timezone loc.
func (c *CronSchedule) Matches(t time.Time, loc *time.Location) bool {
	if loc == nil {
		loc = time.Local //nolint:gosmopolitan // spec D11: unset fasti.local_timezone means the system zone
	}
	tLoc := t.In(loc)
	if tLoc.Second() != 0 || tLoc.Nanosecond() != 0 {
		return false
	}
	if !c.months[int(tLoc.Month())] {
		return false
	}
	if !c.matchDay(tLoc) {
		return false
	}
	if !c.hours[tLoc.Hour()] {
		return false
	}
	if !c.minutes[tLoc.Minute()] {
		return false
	}
	return true
}

// Matches reports whether t corresponds to a scheduled firing instant in timezone loc.
func (ps *ParsedSchedule) Matches(t time.Time, loc *time.Location) bool {
	if ps.cron != nil {
		return ps.cron.Matches(t, loc)
	}
	return false
}

// ParsedSchedule wraps a parsed cron or interval schedule.
type ParsedSchedule struct {
	schedule Schedule
	cron     *CronSchedule
	interval time.Duration
}

// Next calculates the next due time strictly after from in timezone loc (spec D4).
func (ps *ParsedSchedule) Next(from time.Time, loc *time.Location) (NextDueResult, error) {
	if loc == nil {
		loc = time.Local //nolint:gosmopolitan // spec D11: unset fasti.local_timezone means the system zone
	}

	if ps.interval > 0 {
		return NextDueResult{
			Due:      from.Add(ps.interval),
			IsDstGap: false,
		}, nil
	}

	if ps.cron != nil {
		return ps.cron.Next(from, loc)
	}

	return NextDueResult{}, errors.New("uninitialized schedule")
}

// Next calculates the next due time strictly after from for this cron schedule.
// Generates exact sequences across spring-forward gaps and fall-back overlaps (spec D4).
func (c *CronSchedule) Next(from time.Time, loc *time.Location) (NextDueResult, error) {
	if loc == nil {
		loc = time.Local //nolint:gosmopolitan // spec D11: unset fasti.local_timezone means the system zone
	}

	fromLocal := from.In(loc)
	// Start search at the next minute strictly greater than from
	curMinute := fromLocal.Truncate(time.Minute).Add(time.Minute)

	// Search up to 5 years forward
	limitYear := curMinute.Year() + 5

	curYear := curMinute.Year()
	curMonth := curMinute.Month()
	curDay := curMinute.Day()

	for curYear <= limitYear {
		// 1. Month check
		if !c.months[int(curMonth)] {
			curMonth++
			if curMonth > 12 {
				curMonth = 1
				curYear++
			}
			curDay = 1
			continue
		}

		// Days in current month
		daysInMonth := daysIn(curYear, curMonth, loc)
		if curDay > daysInMonth {
			curMonth++
			if curMonth > 12 {
				curMonth = 1
				curYear++
			}
			curDay = 1
			continue
		}

		// 2. Day check (using noon to avoid midnight DST boundary ambiguity)
		dayRef := time.Date(curYear, curMonth, curDay, 12, 0, 0, 0, loc)
		if !c.matchDay(dayRef) {
			curDay++
			continue
		}

		// 3. Hour and minute checks for this calendar day
		startHour := 0
		startMin := 0
		if curYear == curMinute.Year() && curMonth == curMinute.Month() && curDay == curMinute.Day() {
			startHour = curMinute.Hour()
			startMin = curMinute.Minute()
		}

		for h := startHour; h < 24; h++ {
			if !c.hours[h] {
				continue
			}

			mStart := 0
			if h == startHour && curYear == curMinute.Year() && curMonth == curMinute.Month() && curDay == curMinute.Day() {
				mStart = startMin
			}

			for m := mStart; m < 60; m++ {
				if !c.minutes[m] {
					continue
				}

				cand := time.Date(curYear, curMonth, curDay, h, m, 0, 0, loc)

				// Detect Spring-Forward GAP:
				// If the wall-clock time (h, m) does not exist in local time,
				// time.Date normalizes to a different hour or minute.
				if cand.Hour() != h || cand.Minute() != m {
					// Spec D4: "a due time that falls inside a spring-forward gap is skipped with a fasti_fired{status:skipped,reason:dst_gap} event".
					// Note: gapEnd := h+1 is a US-centric 1-hour spring-forward heuristic; some jurisdictions use 30-minute DST transitions (m12).
					gapEnd := time.Date(curYear, curMonth, curDay, h+1, 0, 0, 0, loc)
					if gapEnd.After(from) {
						// Compute the skipped wall-clock due instant with the prior offset before the gap
						_, fromOffset := from.In(loc).Zone()
						wallDue := time.Date(curYear, curMonth, curDay, h, m, 0, 0, time.FixedZone(loc.String(), fromOffset))
						return NextDueResult{
							Due:        gapEnd,
							IsDstGap:   true,
							SkippedDue: wallDue,
						}, nil
					}
					continue
				}

				// Normal candidate (including fall-back overlap first occurrence)
				if cand.After(from) {
					return NextDueResult{
						Due:      cand,
						IsDstGap: false,
					}, nil
				}
			}
		}

		curDay++
	}

	return NextDueResult{}, fmt.Errorf("no matching cron time found within 5 years for %q", c.raw)
}

func daysIn(year int, month time.Month, loc *time.Location) int {
	// First day of next month minus 1 day
	firstNext := time.Date(year, month+1, 1, 0, 0, 0, 0, loc)
	lastDay := firstNext.Add(-24 * time.Hour)
	return lastDay.Day()
}
