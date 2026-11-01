package devices

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// applyRailRetention applies retention rules and merges snapshot/deltas per DEVICE-RAILS-SPEC Rail Data At Rest.
func applyRailRetention(rail string, currentItems json.RawMessage, snapshot []byte, deltas []Delta, now time.Time) (json.RawMessage, error) {
	switch rail {
	case RailHealth:
		// Last-snapshot-only (current day metrics; no multi-year historical telemetry stored)
		if len(snapshot) > 0 {
			var js json.RawMessage
			if err := json.Unmarshal(snapshot, &js); err != nil {
				return nil, fmt.Errorf("invalid health snapshot JSON: %w", err)
			}
			return js, nil
		}
		return currentItems, nil

	case RailContacts:
		// Full address book snapshot, updated via incremental deltas
		items, err := loadItemsArray(currentItems, snapshot)
		if err != nil {
			return nil, err
		}
		items = applyContactDeltas(items, deltas)
		data, err := json.Marshal(items)
		if err != nil {
			return nil, err
		}
		return json.RawMessage(data), nil

	case RailNotifications:
		// TTL 7 days, max-items cap 500 events (rolling FIFO window pruned on sync)
		items, err := loadItemsArray(currentItems, snapshot)
		if err != nil {
			return nil, err
		}
		items = appendDeltaItems(items, deltas)

		cutoff := now.Add(-7 * 24 * time.Hour)
		items = filterItemsByTTL(items, cutoff, true)

		// Cap at 500 FIFO: retain newest 500
		if len(items) > 500 {
			sortItemsByTime(items)
			items = items[len(items)-500:]
		}

		data, err := json.Marshal(items)
		if err != nil {
			return nil, err
		}
		return json.RawMessage(data), nil

	case RailCallLog:
		// TTL 30 days, max-items cap 200 entries
		items, err := loadItemsArray(currentItems, snapshot)
		if err != nil {
			return nil, err
		}
		items = appendDeltaItems(items, deltas)

		cutoff := now.Add(-30 * 24 * time.Hour)
		items = filterItemsByTTL(items, cutoff, true)

		// Cap at 200 entries: retain newest 200
		if len(items) > 200 {
			sortItemsByTime(items)
			items = items[len(items)-200:]
		}

		data, err := json.Marshal(items)
		if err != nil {
			return nil, err
		}
		return json.RawMessage(data), nil

	case RailCalendar:
		// Events bounded to window [-30 days, +90 days] relative to current time
		items, err := loadItemsArray(currentItems, snapshot)
		if err != nil {
			return nil, err
		}
		items = appendDeltaItems(items, deltas)

		minWindow := now.Add(-30 * 24 * time.Hour)
		maxWindow := now.Add(90 * 24 * time.Hour)
		items = filterCalendarWindow(items, minWindow, maxWindow)

		data, err := json.Marshal(items)
		if err != nil {
			return nil, err
		}
		return json.RawMessage(data), nil

	default:
		if len(snapshot) > 0 {
			return json.RawMessage(snapshot), nil
		}
		return currentItems, nil
	}
}

func loadItemsArray(current json.RawMessage, snapshot []byte) ([]map[string]any, error) {
	source := snapshot
	if len(source) == 0 {
		source = current
	}
	if len(source) == 0 {
		return []map[string]any{}, nil
	}
	var items []map[string]any
	if err := json.Unmarshal(source, &items); err != nil {
		// If wrapped in {"items": [...]}, try unmarshaling that
		var wrapper struct {
			Items []map[string]any `json:"items"`
		}
		if werr := json.Unmarshal(source, &wrapper); werr == nil && wrapper.Items != nil {
			return wrapper.Items, nil
		}
		// If single object, wrap as single item
		var single map[string]any
		if serr := json.Unmarshal(source, &single); serr == nil {
			return []map[string]any{single}, nil
		}
		return nil, fmt.Errorf("invalid items JSON: %w", err)
	}
	return items, nil
}

func applyContactDeltas(items []map[string]any, deltas []Delta) []map[string]any {
	itemIndex := make(map[string]int, len(items))
	for i, it := range items {
		if id := extractID(it); id != "" {
			itemIndex[id] = i
		}
	}

	for _, d := range deltas {
		op := strings.ToLower(d.Op)
		id := d.ID

		var data map[string]any
		if len(d.Data) > 0 {
			_ = json.Unmarshal(d.Data, &data)
		}
		if id == "" && data != nil {
			id = extractID(data)
		}

		switch op {
		case "delete", "remove":
			if idx, ok := itemIndex[id]; ok {
				items = append(items[:idx], items[idx+1:]...)
				// Rebuild index
				itemIndex = make(map[string]int, len(items))
				for i, it := range items {
					if itID := extractID(it); itID != "" {
						itemIndex[itID] = i
					}
				}
			}

		case "add", "update", "upsert":
			if data == nil {
				continue
			}
			if idx, ok := itemIndex[id]; ok && id != "" {
				items[idx] = data
			} else {
				itemIndex[id] = len(items)
				items = append(items, data)
			}
		}
	}
	return items
}

func appendDeltaItems(items []map[string]any, deltas []Delta) []map[string]any {
	for _, d := range deltas {
		if len(d.Data) > 0 {
			var m map[string]any
			if err := json.Unmarshal(d.Data, &m); err == nil {
				items = append(items, m)
			}
		}
	}
	return items
}

func extractID(m map[string]any) string {
	for _, key := range []string{"id", "contact_id", "key", "uid", "name"} {
		if v, ok := m[key]; ok {
			return fmt.Sprintf("%v", v)
		}
	}
	return ""
}

func extractItemTime(m map[string]any) (time.Time, bool) {
	for _, key := range []string{"timestamp", "time", "created_at", "date", "posted_at", "start", "start_time", "start_at"} {
		v, ok := m[key]
		if !ok || v == nil {
			continue
		}
		switch val := v.(type) {
		case string:
			for _, layout := range []string{
				time.RFC3339Nano,
				time.RFC3339,
				"2006-01-02T15:04:05",
				"2006-01-02 15:04:05",
				"2006-01-02",
			} {
				if t, err := time.Parse(layout, val); err == nil {
					return t.UTC(), true
				}
			}
		case float64:
			if val > 1e11 {
				return time.UnixMilli(int64(val)).UTC(), true
			}
			return time.Unix(int64(val), 0).UTC(), true
		case int64:
			if val > 1e11 {
				return time.UnixMilli(val).UTC(), true
			}
			return time.Unix(val, 0).UTC(), true
		case json.Number:
			if n, err := val.Int64(); err == nil {
				if n > 1e11 {
					return time.UnixMilli(n).UTC(), true
				}
				return time.Unix(n, 0).UTC(), true
			}
		}
	}
	return time.Time{}, false
}

func filterItemsByTTL(items []map[string]any, cutoff time.Time, keepUnparseable bool) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		t, ok := extractItemTime(it)
		if !ok {
			if keepUnparseable {
				out = append(out, it)
			}
			continue
		}
		if !t.Before(cutoff) {
			out = append(out, it)
		}
	}
	return out
}

func filterCalendarWindow(items []map[string]any, minWindow, maxWindow time.Time) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		t, ok := extractItemTime(it)
		if !ok {
			out = append(out, it)
			continue
		}
		if !t.Before(minWindow) && !t.After(maxWindow) {
			out = append(out, it)
		}
	}
	return out
}

func sortItemsByTime(items []map[string]any) {
	sort.SliceStable(items, func(i, j int) bool {
		ti, oki := extractItemTime(items[i])
		tj, okj := extractItemTime(items[j])
		if !oki || !okj {
			return false
		}
		return ti.Before(tj)
	})
}
