package fasti

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/loop"
)

func TestSecretNeedles_RefusesAddAndPropose(t *testing.T) {
	home := t.TempDir()
	t0 := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	clk := loop.NewFakeClock(t0)

	const secret = "sk-live-vault-secret-998877" //nolint:gosec // G101: fixture value, not a credential
	eng, err := New(
		DefaultConfig(),
		home,
		WithClock(clk),
		WithSecretNeedles(func() []string { return []string{secret} }),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { eng.Stop() })

	ctx := context.Background()
	if err := eng.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// 1. AddJob with literal secret needle in prompt -> refused naming ${vault:NAME}, no row created.
	_, addErr := eng.AddJob(ctx, Job{
		Name:     "user-job",
		Schedule: Schedule{Kind: "interval", Every: "15m"},
		Prompt:   "fetch status using Bearer " + secret + " now",
	})
	if addErr == nil {
		t.Fatal("AddJob with secret needle succeeded, want refusal")
	}
	if !errors.Is(addErr, ErrSecretInPrompt) {
		t.Fatalf("AddJob err = %v, want ErrSecretInPrompt", addErr)
	}
	if !strings.Contains(addErr.Error(), "${vault:NAME}") {
		t.Fatalf("AddJob err = %q, must mention ${vault:NAME}", addErr.Error())
	}
	if strings.Contains(addErr.Error(), secret) {
		t.Fatalf("AddJob error leaked secret value: %q", addErr.Error())
	}

	// 2. ProposeJob with literal secret needle in prompt -> refused naming ${vault:NAME}, no row created.
	_, propErr := eng.ProposeJob(ctx, Job{
		Name:      "model-job",
		Schedule:  Schedule{Kind: "interval", Every: "30m"},
		Prompt:    "sync records with key=" + secret,
		CreatedBy: "model:s_test",
	})
	if propErr == nil {
		t.Fatal("ProposeJob with secret needle succeeded, want refusal")
	}
	if !errors.Is(propErr, ErrSecretInPrompt) {
		t.Fatalf("ProposeJob err = %v, want ErrSecretInPrompt", propErr)
	}
	if !strings.Contains(propErr.Error(), "${vault:NAME}") {
		t.Fatalf("ProposeJob err = %q, must mention ${vault:NAME}", propErr.Error())
	}
	if strings.Contains(propErr.Error(), secret) {
		t.Fatalf("ProposeJob error leaked secret value: %q", propErr.Error())
	}

	// Job list and on-disk store must remain empty.
	jobs, err := eng.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("ListJobs len = %d, want 0 (%+v)", len(jobs), jobs)
	}
	diskJobs, err := LoadJobs(JobsFilePath(home))
	if err != nil {
		t.Fatalf("LoadJobs: %v", err)
	}
	if len(diskJobs) != 0 {
		t.Fatalf("LoadJobs len = %d, want 0 (%+v)", len(diskJobs), diskJobs)
	}
	events, err := eng.Journal().Events()
	if err != nil {
		t.Fatalf("Journal.Events: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("Journal.Events len = %d, want 0 (%+v)", len(events), events)
	}

	// 3. ${vault:NAME} reference passes through as literal reference (V-Ph).
	created, err := eng.AddJob(ctx, Job{
		Name:     "vault-ref-job",
		Schedule: Schedule{Kind: "interval", Every: "15m"},
		Prompt:   "authenticate via ${vault:API_TOKEN} and summarize",
	})
	if err != nil {
		t.Fatalf("AddJob with ${vault:API_TOKEN} failed: %v", err)
	}
	if created.Prompt != "authenticate via ${vault:API_TOKEN} and summarize" {
		t.Fatalf("stored prompt = %q, want verbatim ${vault:API_TOKEN}", created.Prompt)
	}
}

func TestSecretNeedles_LazySourceAfterStart(t *testing.T) {
	home := t.TempDir()
	t0 := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	clk := loop.NewFakeClock(t0)

	var (
		mu      sync.Mutex
		needles []string
	)
	lazySrc := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), needles...)
	}

	eng, err := New(DefaultConfig(), home, WithClock(clk), WithSecretNeedles(lazySrc))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { eng.Stop() })

	ctx := context.Background()
	if err := eng.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	const lateSecret = "late-bound-secret-889900" //nolint:gosec // G101: fixture value, not a credential

	// Before needles are set, create succeeds.
	if _, err := eng.AddJob(ctx, Job{
		Name:     "before-secrets",
		Schedule: Schedule{Kind: "interval", Every: "1h"},
		Prompt:   "harmless prompt before vault unlock",
	}); err != nil {
		t.Fatalf("AddJob before secrets set: %v", err)
	}

	// Populate needles AFTER engine start (models SetSecrets wired after startFasti).
	mu.Lock()
	needles = []string{lateSecret}
	mu.Unlock()

	if _, err := eng.AddJob(ctx, Job{
		Name:     "after-secrets-add",
		Schedule: Schedule{Kind: "interval", Every: "1h"},
		Prompt:   "using " + lateSecret + " inline",
	}); err == nil || !strings.Contains(err.Error(), "${vault:NAME}") {
		t.Fatalf("AddJob after lazy secret update = %v, want refusal mentioning ${vault:NAME}", err)
	}

	if _, err := eng.ProposeJob(ctx, Job{
		Name:      "after-secrets-propose",
		Schedule:  Schedule{Kind: "interval", Every: "1h"},
		Prompt:    "proposing " + lateSecret + " inline",
		CreatedBy: "model:s_lazy",
	}); err == nil || !strings.Contains(err.Error(), "${vault:NAME}") {
		t.Fatalf("ProposeJob after lazy secret update = %v, want refusal mentioning ${vault:NAME}", err)
	}

	jobs, err := eng.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("ListJobs len = %d, want 1", len(jobs))
	}
}

func TestSecretNeedles_PermissiveNilAndShortNeedles(t *testing.T) {
	t0 := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	// Case 1: nil source is permissive.
	t.Run("nil source", func(t *testing.T) {
		home := t.TempDir()
		clk := loop.NewFakeClock(t0)
		eng, err := New(DefaultConfig(), home, WithClock(clk), WithSecretNeedles(nil))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		t.Cleanup(func() { eng.Stop() })
		if err := eng.Start(ctx); err != nil {
			t.Fatalf("Start: %v", err)
		}

		if _, err := eng.AddJob(ctx, Job{
			Name:     "nil-src-add",
			Schedule: Schedule{Kind: "interval", Every: "10m"},
			Prompt:   "any prompt text succeeds when source is nil",
		}); err != nil {
			t.Fatalf("AddJob with nil source failed: %v", err)
		}
		if _, err := eng.ProposeJob(ctx, Job{
			Name:      "nil-src-propose",
			Schedule:  Schedule{Kind: "interval", Every: "10m"},
			Prompt:    "model proposal succeeds when source is nil",
			CreatedBy: "model:s1",
		}); err != nil {
			t.Fatalf("ProposeJob with nil source failed: %v", err)
		}
	})

	// Case 2: needles shorter than 8 bytes are skipped; len == 8 refuses.
	t.Run("short needles skipped", func(t *testing.T) {
		home := t.TempDir()
		clk := loop.NewFakeClock(t0)
		eng, err := New(
			DefaultConfig(),
			home,
			WithClock(clk),
			WithSecretNeedles(func() []string {
				return []string{"", "x", "short", "1234567", "eight888"}
			}),
		)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		t.Cleanup(func() { eng.Stop() })
		if err := eng.Start(ctx); err != nil {
			t.Fatalf("Start: %v", err)
		}

		// Prompt containing 7-char needle "1234567" and "short" must succeed.
		if _, err := eng.AddJob(ctx, Job{
			Name:     "short-needle-add",
			Schedule: Schedule{Kind: "interval", Every: "10m"},
			Prompt:   "check short item 1234567 status",
		}); err != nil {
			t.Fatalf("AddJob with <8 char needle match failed: %v", err)
		}
		if _, err := eng.ProposeJob(ctx, Job{
			Name:      "short-needle-propose",
			Schedule:  Schedule{Kind: "interval", Every: "10m"},
			Prompt:    "check short item 1234567 from model",
			CreatedBy: "model:s1",
		}); err != nil {
			t.Fatalf("ProposeJob with <8 char needle match failed: %v", err)
		}

		// Prompt containing 8-char needle "eight888" must refuse.
		if _, err := eng.AddJob(ctx, Job{
			Name:     "eight-char-needle",
			Schedule: Schedule{Kind: "interval", Every: "10m"},
			Prompt:   "contains eight888 literal",
		}); err == nil || !strings.Contains(err.Error(), "${vault:NAME}") {
			t.Fatalf("AddJob with 8-char needle = %v, want refusal mentioning ${vault:NAME}", err)
		}
	})
}
