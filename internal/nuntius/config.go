// Package nuntius is the Telegram channel bridge: a long-polling Bot
// API client that reaches the same sessions, loop engine, and approval
// hub as the web surface (NUNTIUS-SPEC). Nothing here listens for
// inbound connections — the bridge only dials out (P5).
//
// This package holds the transport-independent core: config (§4),
// pairing and the owner allowlist (§3), the gate (§3, N2), and the
// durable write-ahead inbox (§2, N10). The real Bot API transport,
// flood-control buckets, streaming edits, and approval cards are
// separate; everything here is testable against an in-memory fake.
package nuntius

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultBotTokenEnv names the env var holding the bot token when
// bot_token_env is unset (spec §4). It is a variable NAME, never a
// secret value — the token itself is never a YAML value or constant.
const DefaultBotTokenEnv = "LARARIUM_TELEGRAM_BOT_TOKEN" //nolint:gosec // G101: an env var NAME, not a credential value

// Config is the nuntius: block of lararium.yaml (spec §4). Zero values
// normalize to the spec defaults via Normalize.
type Config struct {
	Enabled         bool          `yaml:"enabled"`
	BotTokenEnv     string        `yaml:"bot_token_env"`
	EditInterval    time.Duration `yaml:"edit_interval"`
	ApprovalTimeout time.Duration `yaml:"approval_timeout"`

	// QueueDepth is the number of messages held while a turn is in
	// flight: 0 or 1 (spec §4). Pointer so "unset" (default 1) is
	// distinguishable from an explicit 0.
	QueueDepth *int `yaml:"queue_depth"`

	PairCodeTTL    time.Duration  `yaml:"pair_code_ttl"`
	PairFailWindow time.Duration  `yaml:"pair_fail_window"`
	PairMute       time.Duration  `yaml:"pair_mute"`
	PairReplyGap   *time.Duration `yaml:"pair_reply_gap"` // explicit 0 allowed (CI)
}

// ParseConfig decodes the nuntius: block with the strict-config
// doctrine: unknown keys are an error (spec §4).
func ParseConfig(data []byte) (Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("nuntius config: %w", err)
	}
	return c, nil
}

// Normalize fills spec defaults and floors. Floors are 1 s for the
// pairing durations (CI shortens; spec §4) and 0 for pair_reply_gap.
// edit_interval below its 1 s floor normalizes UP with a warning
// (spec §4); queue_depth outside {0,1} is a config error.
func (c *Config) Normalize() ([]string, error) {
	var warns []string

	if c.BotTokenEnv == "" {
		c.BotTokenEnv = DefaultBotTokenEnv
	}
	const editFloor = time.Second
	switch {
	case c.EditInterval == 0:
		c.EditInterval = 2 * time.Second
	case c.EditInterval < editFloor:
		c.EditInterval = editFloor
		warns = append(warns, "nuntius: edit_interval below 1s floor; normalized to 1s")
	}
	if c.ApprovalTimeout == 0 {
		c.ApprovalTimeout = 5 * time.Minute
	}
	if c.QueueDepth == nil {
		one := 1
		c.QueueDepth = &one
	} else if *c.QueueDepth != 0 && *c.QueueDepth != 1 {
		return warns, errors.New("nuntius: queue_depth must be 0 or 1")
	}

	const floor = time.Second
	if c.PairCodeTTL == 0 {
		c.PairCodeTTL = 15 * time.Minute
	} else if c.PairCodeTTL < floor {
		c.PairCodeTTL = floor
	}
	if c.PairFailWindow == 0 {
		c.PairFailWindow = 10 * time.Minute
	} else if c.PairFailWindow < floor {
		c.PairFailWindow = floor
	}
	if c.PairMute == 0 {
		c.PairMute = time.Hour
	} else if c.PairMute < floor {
		c.PairMute = floor
	}
	if c.PairReplyGap == nil {
		gap := 60 * time.Second
		c.PairReplyGap = &gap
	} else if *c.PairReplyGap < 0 {
		zero := time.Duration(0)
		c.PairReplyGap = &zero
	}

	return warns, nil
}

// Token reads the bot token from the named env var (spec §4: the token
// lives in the environment, never in YAML, never in any file nuntius
// owns). getenv is injectable for tests; nil means os.Getenv.
func (c *Config) Token(getenv func(string) string) (string, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	tok := getenv(c.BotTokenEnv)
	if tok == "" {
		return "", fmt.Errorf("nuntius: enabled but %s is unset or empty", c.BotTokenEnv)
	}
	return tok, nil
}
