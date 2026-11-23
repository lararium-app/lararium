package penatus

import (
	"strings"
	"testing"
)

func TestValidSessionDirName(t *testing.T) {
	ok := []string{
		"main",
		"s_01ARZ3NDEKTSV4RRFFQ69G5FAV",
		"cron-j_20261123_abc",
		"goal-g_1",
		"a",
		"A-b_c-9",
		strings.Repeat("a", 128),
	}
	for _, id := range ok {
		if !ValidSessionDirName(id) {
			t.Errorf("ValidSessionDirName(%q) = false, want true", id)
		}
	}

	bad := map[string]string{
		"empty":            "",
		"dot":              ".",
		"dotdot":           "..",
		"parent traversal": "../x",
		"deep traversal":   "../../../../etc",
		"embedded slash":   "a/b",
		"leading slash":    "/abs",
		"backslash":        `a\b`,
		"hidden":           ".hidden",
		"trailing dot":     "a.",
		"embedded dot":     "a.b",
		"leading dash":     "-flag",
		"nul":              "a\x00b",
		"newline":          "a\nb",
		"space":            "a b",
		"tab":              "a\tb",
		"unicode":          "séance",
		"fullwidth dot":    "a\uff0eb",
		"too long":         strings.Repeat("a", 129),
	}
	for name, id := range bad {
		if ValidSessionDirName(id) {
			t.Errorf("%s: ValidSessionDirName(%q) = true, want false", name, id)
		}
	}
}
