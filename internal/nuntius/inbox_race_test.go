package nuntius

import (
	"io"
	"log"
	"path/filepath"
	"sync"
	"testing"
)

// M21: targeted race exposer between poll path (enqueue via accept) and
// worker path (dequeue/ack via markDone).
func TestBridgeInboxConcurrentEnqueueAckRace(t *testing.T) {
	home := t.TempDir()
	ndir := filepath.Join(home, "nuntius")
	sf, err := NewStateFile(ndir)
	if err != nil {
		t.Fatal(err)
	}
	_ = sf.Save(State{ActiveSession: "main", OwnerChat: "123"})

	fake := &Fake{}
	b, err := New(Deps{
		Cfg:  Config{Enabled: true},
		Home: home,
		TG:   fake,
		Log:  log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Stop()

	var wg sync.WaitGroup
	const iterations = 100

	// Poll path: concurrent accepts/enqueues
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := int64(1); i <= iterations; i++ {
			u := Update{
				UpdateID: i,
				Message: &Message{
					MessageID: i,
					Chat:      Chat{ID: "123", Type: "private"},
					From:      &User{ID: "42", FirstName: "owner"},
					Text:      "hello",
				},
			}
			_, _ = b.accept([]Update{u})
		}
	}()

	// Worker path: concurrent dequeue / markDone
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := int64(1); i <= iterations; i++ {
			b.markDone(Record{UpdateID: i}, nil)
		}
	}()

	wg.Wait()
}
