package custos_test

import "github.com/lararium-app/lararium/internal/surface"

// Configure satisfies surface.SessionSource for vt1Sessions without modifying
// existing *_test.go files.
func (vt1Sessions) Configure(string, *string, *int64) (surface.SessionInfo, error) {
	return surface.SessionInfo{}, nil
}
