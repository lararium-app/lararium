package surface

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/fasti"
)

func TestVPhSurface_SSE_BootstrapAndBroadcast(t *testing.T) {
	env := setupFastiEnv(t, true)

	// Pre-populate one job and one goal before connecting
	_, err := env.eng.AddJob(context.Background(), fasti.Job{
		Name:     "existing-job",
		Schedule: fasti.Schedule{Kind: "interval", Every: "30m"},
		Prompt:   "existing prompt",
	})
	if err != nil {
		t.Fatalf("add pre-existing job: %v", err)
	}

	_, err = env.eng.AddGoal(context.Background(), fasti.GoalSpec{
		Title:        "existing-goal",
		Requirements: []string{"req1"},
	})
	if err != nil {
		t.Fatalf("add pre-existing goal: %v", err)
	}

	ts := httptest.NewServer(env.h)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ch := readSSE(t, ctx, ts.URL+"/v1/fasti/events", env.tok)

	// Bootstrap events: should receive job, goal, and heartbeat
	var sawJob, sawGoal, sawHeartbeat bool
	deadline := time.After(3 * time.Second)

bootstrapLoop:
	for !sawJob || !sawGoal || !sawHeartbeat {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for bootstrap events (job=%v, goal=%v, heartbeat=%v)", sawJob, sawGoal, sawHeartbeat)
		case frame, ok := <-ch:
			if !ok {
				t.Fatalf("stream closed prematurely")
			}
			if frame[0] == "fasti_event" {
				var ev fasti.Event
				if err := json.Unmarshal([]byte(frame[1]), &ev); err == nil {
					switch ev.Kind {
					case "job":
						sawJob = true
					case "goal":
						sawGoal = true
					case "heartbeat":
						sawHeartbeat = true
					}
				}
			}
			if sawJob && sawGoal && sawHeartbeat {
				break bootstrapLoop
			}
		}
	}

	// Trigger subsequent job creation via REST POST
	code, body := env.do(http.MethodPost, "/v1/fasti/jobs", `{
		"name": "broadcast-job",
		"schedule": {"kind": "interval", "every": "10m"},
		"prompt": "broadcast prompt"
	}`)
	if code != http.StatusCreated {
		t.Fatalf("create job: %d (%s)", code, body)
	}

	// Verify broadcast event arrives on the SSE rail
	var sawBroadcast bool
	broadcastDeadline := time.After(3 * time.Second)
	for !sawBroadcast {
		select {
		case <-broadcastDeadline:
			t.Fatalf("timed out waiting for broadcast event")
		case frame, ok := <-ch:
			if !ok {
				t.Fatalf("stream closed prematurely waiting for broadcast")
			}
			if frame[0] == "fasti_event" {
				var ev fasti.Event
				if err := json.Unmarshal([]byte(frame[1]), &ev); err == nil {
					if ev.Kind == "job" && ev.Op == "upsert" {
						b, err := json.Marshal(ev.Row)
						if err == nil && strings.Contains(string(b), "broadcast-job") {
							sawBroadcast = true
							break
						}
					}
				}
			}
		}
	}
}

func TestVPhSurface_SSE_SlowClientDisconnect(t *testing.T) {
	env := setupFastiEnv(t, true)

	blocked := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env.h.ServeHTTP(&slowFastiWriter{ResponseWriter: w, blocked: blocked, release: release}, r)
	}))
	defer ts.Close()
	defer unblock()

	wait := 30 * time.Second
	if deadline, ok := t.Deadline(); ok {
		wait = min(wait, max(time.Until(deadline)-time.Second, time.Millisecond))
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/v1/fasti/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+env.tok)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// Hold the handler in its bootstrap flush so it cannot drain its subscription.
	select {
	case <-blocked:
	case <-ctx.Done():
		t.Fatalf("bootstrap flush did not start: %v", ctx.Err())
	}

	// Publish >64 events directly to the engine hub without reading from the client
	// This overflows the 64-slot buffer and must trigger disconnection
	start := time.Now()
	for i := range 75 {
		env.eng.Publish(fasti.Event{
			Kind: "job",
			Op:   "upsert",
			Row:  map[string]any{"id": fmt.Sprintf("overflow-%d", i)},
		})
	}

	// Publisher must never be blocked (should take well under 100ms)
	if time.Since(start) > 500*time.Millisecond {
		t.Fatalf("publisher was blocked: took %v", time.Since(start))
	}
	unblock()

	// Verify the connection closes because slow client was disconnected
	done := make(chan struct{})
	go func() {
		buf := make([]byte, 1024)
		for {
			_, err := resp.Body.Read(buf)
			if err != nil {
				close(done)
				return
			}
		}
	}()

	select {
	case <-done:
		// Client disconnected as expected
	case <-ctx.Done():
		t.Fatalf("slow client was not disconnected within deadline: %v", ctx.Err())
	}
}

type slowFastiWriter struct {
	http.ResponseWriter
	blocked chan<- struct{}
	release <-chan struct{}
	flushes int
}

func (w *slowFastiWriter) Flush() {
	w.flushes++
	if w.flushes == 2 {
		close(w.blocked)
		<-w.release
	}
	w.ResponseWriter.(http.Flusher).Flush()
}

func TestVPhSurface_SSE_ConcurrentSubscribersRace(t *testing.T) {
	env := setupFastiEnv(t, true)

	ts := httptest.NewServer(env.h)
	defer ts.Close()

	const numSubscribers = 10
	var wg sync.WaitGroup
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	for range numSubscribers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch := readSSE(t, ctx, ts.URL+"/v1/fasti/events", env.tok)
			for range ch {
				// drain
			}
		}()
	}

	// Concurrent publisher goroutine
	publishDone := make(chan struct{})
	go func() {
		defer close(publishDone)
		for i := range 20 {
			env.eng.Publish(fasti.Event{
				Kind: "job",
				Op:   "upsert",
				Row:  map[string]any{"seq": i},
			})
			time.Sleep(5 * time.Millisecond)
		}
	}()

	<-publishDone
	cancel()
	wg.Wait()
}
