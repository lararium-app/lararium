package nuntius

import (
	"sync"
)

// turnQueue is the per-chat backpressure FIFO (§6: queue-one, then
// reject). Queued messages are in-memory only and die with the
// process — acceptable because the durable inbox replays any update
// not marked done, so a restart redelivers. depth is queue_depth: the
// number of messages held WHILE a turn runs (0 or 1).
//
// One worker goroutine per queue runs turns serially, so the poll
// loop never blocks on a model call (the in-flight-polling deadlock
// §2 closes) and one chat is one stream at a time.
type turnQueue struct {
	depth int
	run   func(job) // executes one turn; called on the worker goroutine

	mu      sync.Mutex
	running bool
	held    []job
	closed  bool
}

type job struct {
	rec    Record
	text   string
	chatID string
}

func newTurnQueue(depth int, run func(job)) *turnQueue {
	return &turnQueue{depth: depth, run: run}
}

// submit applies the §6 gate: idle → start now; turn running and a
// queue slot free → hold; both slots occupied → reject with feedback
// (never silently swallowed). Returns false when rejected.
func (q *turnQueue) submit(j job) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return false
	}
	if !q.running {
		q.running = true
		q.startLocked(j)
		return true
	}
	if len(q.held) >= q.depth {
		return false // "still working — /status"
	}
	q.held = append(q.held, j)
	return true
}

func (q *turnQueue) startLocked(j job) {
	go func() {
		q.run(j)
		q.finish()
	}()
}

func (q *turnQueue) finish() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.held) > 0 {
		next := q.held[0]
		q.held = q.held[1:]
		q.startLocked(next) // /cancel keeps the queue: release the next
		return
	}
	q.running = false
}

// queued reports the number of held (not started) messages (§5).
func (q *turnQueue) queued() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.held)
}

// close stops intake and returns the held messages so the bridge can
// refuse them with the static shutdown line (§2 shutdown).
func (q *turnQueue) close() []job {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	held := q.held
	q.held = nil
	return held
}
