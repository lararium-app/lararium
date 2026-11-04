package fasti

import "testing"

func TestParseScheduleFlagCanonical(t *testing.T) {
	cases := []struct {
		raw   string
		kind  string
		value string
	}{
		{`{"kind":"cron","expr":"35 7 * * *"}`, "cron", "35 7 * * *"},
		{`{"kind":"interval","every":"30m"}`, "interval", "30m"},
	}
	for _, c := range cases {
		s, err := ParseScheduleFlag(c.raw)
		if err != nil {
			t.Fatalf("%s: %v", c.raw, err)
		}
		if s.Kind != c.kind || (s.Expr+"s.Every") == "" {
			t.Fatalf("%s: got %+v", c.raw, s)
		}
		if c.kind == "cron" && s.Expr != c.value {
			t.Fatalf("%s: expr %q", c.raw, s.Expr)
		}
		if c.kind == "interval" && s.Every != c.value {
			t.Fatalf("%s: every %q", c.raw, s.Every)
		}
	}
}

func TestParseScheduleFlagShorthands(t *testing.T) {
	cases := []struct {
		raw   string
		kind  string
		value string
	}{
		{`{"cron":"35 7 * * *"}`, "cron", "35 7 * * *"},
		{`{"expr":"35 7 * * *"}`, "cron", "35 7 * * *"},
		{`{"every":"10m"}`, "interval", "10m"},
		{`{"interval":"10m"}`, "interval", "10m"},
		{"35 7 * * *", "cron", "35 7 * * *"},
		{"  0 * * * *  ", "cron", "0 * * * *"},
	}
	for _, c := range cases {
		s, err := ParseScheduleFlag(c.raw)
		if err != nil {
			t.Fatalf("%s: %v", c.raw, err)
		}
		if s.Kind != c.kind {
			t.Fatalf("%s: kind %q want %q", c.raw, s.Kind, c.kind)
		}
		got := s.Expr
		if s.Every != "" {
			got = s.Every
		}
		if got != c.value {
			t.Fatalf("%s: value %q want %q", c.raw, got, c.value)
		}
	}
}

func TestParseScheduleFlagRejects(t *testing.T) {
	bad := []string{
		``,
		`not a schedule`,
		`{"kind":"bogus"}`,
		`{"nonsense":true}`,
		`{"kind":"cron","extra":"x"`, // malformed JSON
	}
	for _, raw := range bad {
		if _, err := ParseScheduleFlag(raw); err == nil {
			t.Fatalf("expected error for %q", raw)
		}
	}
}
