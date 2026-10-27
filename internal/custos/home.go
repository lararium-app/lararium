package custos

import (
	"os"
	"path/filepath"
	"strings"
)

// ResolveHearthHomeEnvWithFallback resolves the hearth persona home directory with the precedence:
// override (CLI flag, if non-empty) > env HEARTH_HOME > configHome > fallback.
// If getenv is nil, os.Getenv is used.
func ResolveHearthHomeEnvWithFallback(override, configHome, fallback string, getenv func(string) string) string {
	if getenv == nil {
		getenv = os.Getenv
	}
	if override != "" {
		return expandAndAbs(override, getenv)
	}
	if env := getenv("HEARTH_HOME"); env != "" {
		return expandAndAbs(env, getenv)
	}
	if configHome != "" {
		return expandAndAbs(configHome, getenv)
	}
	return fallback
}

// ResolveHearthHomeWithFallback resolves the hearth persona home directory using os.Getenv
// and the provided fallback when override, env, and configHome are empty.
func ResolveHearthHomeWithFallback(override, configHome, fallback string) string {
	return ResolveHearthHomeEnvWithFallback(override, configHome, fallback, os.Getenv)
}

// DefaultHearthHomeEnv returns the default hearth persona home directory ($HOME/.lararium).
// If getenv is nil, os.Getenv is used.
func DefaultHearthHomeEnv(getenv func(string) string) string {
	if getenv == nil {
		getenv = os.Getenv
	}
	var homeDir string
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		homeDir = h
	} else {
		homeDir = getenv("HOME")
	}

	defaultHome := filepath.Join(homeDir, ".lararium")
	if abs, err := filepath.Abs(defaultHome); err == nil {
		return abs
	}
	return defaultHome
}

// DefaultHearthHome returns the default hearth persona home directory using os.Getenv.
func DefaultHearthHome() string {
	return DefaultHearthHomeEnv(os.Getenv)
}

// ResolveHearthHomeEnv resolves the hearth persona home directory with the precedence:
// override (CLI flag, if non-empty) > env HEARTH_HOME > configHome > $HOME/.lararium fallback.
// If getenv is nil, os.Getenv is used.
func ResolveHearthHomeEnv(override, configHome string, getenv func(string) string) string {
	return ResolveHearthHomeEnvWithFallback(override, configHome, DefaultHearthHomeEnv(getenv), getenv)
}

// ResolveHearthHome resolves the hearth persona home directory using os.Getenv.
func ResolveHearthHome(override, configHome string) string {
	return ResolveHearthHomeEnv(override, configHome, os.Getenv)
}

func expandAndAbs(path string, getenv func(string) string) string {
	if strings.HasPrefix(path, "~/") {
		var homeDir string
		if h, err := os.UserHomeDir(); err == nil && h != "" {
			homeDir = h
		} else {
			if getenv == nil {
				getenv = os.Getenv
			}
			homeDir = getenv("HOME")
		}
		path = filepath.Join(homeDir, path[2:])
	}
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}
