package penatus

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
)

// fields builds an event payload for the race tests.
func fields(text string) map[string]json.RawMessage {
	return map[string]json.RawMessage{"text": json.RawMessage(fmt.Sprintf("%q", text))}
}

// jam appends a raw line straight to the file, bypassing Log (models a
// corrupt foreign writer).
func jam(path, line string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	if _, err := f.Write([]byte(line + "\n")); err != nil {
		panic(err)
	}
}

// SOAK-INCIDENT-2026-11-15 defect B: a scheduler wake (separate Log
// instance opened from disk) and a turn completion (long-lived Log)
// appended to the same session concurrently and both took seq 898 —
// the transcript failed every later OpenLog with "corrupt log".
// Append is now flock-serialized with on-disk seq re-derivation.

func TestConcurrentAppendSeparateInstances(t *testing.T) {
	dir := t.TempDir()
	const writers, perWriter = 4, 25

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		// Each writer opens its OWN Log from disk — models the
		// scheduler-wake path (fresh OpenLog per event) racing a
		// persistent turn Log.
		log, err := OpenLog(dir)
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				if err := log.Append(Event{
					T:      "msg",
					Fields: fields(fmt.Sprintf("w%d_%d", w, i)),
				}); err != nil {
					t.Errorf("writer %d append %d: %v", w, i, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	// Re-open strictly: any duplicate or gapped seq fails here.
	log, err := OpenLog(dir)
	if err != nil {
		t.Fatalf("re-open after concurrent appends: %v", err)
	}
	events := log.Live()
	if len(events) != writers*perWriter {
		t.Fatalf("lost writes: %d events, want %d", len(events), writers*perWriter)
	}
	for i, ev := range events {
		if ev.Seq != int64(i+1) {
			t.Fatalf("seq disorder at line %d: %d", i+1, ev.Seq)
		}
	}
}

func TestConcurrentAppendSameInstance(t *testing.T) {
	dir := t.TempDir()
	log, err := OpenLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	const writers, perWriter = 8, 50

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				if err := log.Append(Event{
					T:      "tool_result",
					Fields: fields(fmt.Sprintf("g%d_%d", w, i)),
				}); err != nil {
					t.Errorf("goroutine %d append %d: %v", w, i, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	if _, err := OpenLog(dir); err != nil {
		t.Fatalf("log corrupt after same-instance concurrency: %v", err)
	}
	if got := len(log.Live()); got != writers*perWriter {
		t.Fatalf("%d events, want %d", got, writers*perWriter)
	}
}

// TestResyncDetectsExternalCorruption: if a foreign writer already broke
// seq ordering, our append refuses rather than piling on.
func TestResyncDetectsExternalCorruption(t *testing.T) {
	dir := t.TempDir()
	log, err := OpenLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := log.Append(Event{T: "msg", Fields: fields("ok")}); err != nil {
			t.Fatal(err)
		}
	}
	// Foreign writer jams a duplicate seq 1 line onto the file.
	jam(log.path, `{"seq":1,"t":"msg","text":"foreign"}`)

	err = log.Append(Event{T: "msg", Fields: fields("post-jam")})
	if err == nil {
		t.Fatal("append after external corruption must fail")
	}
	if !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("want corrupt-class error, got %v", err)
	}
}
