package analytics

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/bulutaysarac/linkly/internal/metrics"
)

type blockingSink struct {
	mu    sync.Mutex
	got   []Event
	block chan struct{}
	err   error
}

func (s *blockingSink) Write(_ context.Context, batch []Event) error {
	if s.block != nil {
		<-s.block
	}
	if s.err != nil {
		return s.err
	}
	s.mu.Lock()
	s.got = append(s.got, batch...)
	s.mu.Unlock()
	return nil
}

func (s *blockingSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.got)
}

// TestRecordNeverBlocks is the core promise: the redirect path is never held up
// by analytics, even when the queue is completely full.
// TR: Temel söz: kuyruk tamamen doluyken bile yönlendirme yolu analitik yüzünden
// bekletilmez.
func TestRecordNeverBlocksAndDropsAreCounted(t *testing.T) {
	met := metrics.NewRegistry()
	sink := &blockingSink{}
	c := New(sink, Options{QueueSize: 2, BatchSize: 100, FlushEvery: time.Hour}, met)
	// Deliberately NOT calling Run(): nothing drains, so the queue fills up.
	// TR: Bilinçli olarak Run() çağrılmıyor: kimse boşaltmıyor, kuyruk doluyor.

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			c.Record(Event{Code: "abc", At: time.Now()})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Record blocked — a full analytics queue is stalling the request path")
	}

	if met.Get("analytics_enqueued") != 2 {
		t.Fatalf("enqueued = %d, want 2 (queue size)", met.Get("analytics_enqueued"))
	}
	if met.Get("analytics_dropped") != 98 {
		t.Fatalf("dropped = %d, want 98 — drops must be counted, not silent",
			met.Get("analytics_dropped"))
	}
}

// TestShutdownDrains is the graceful-shutdown lesson.
// TR: Graceful shutdown dersi.
func TestShutdownDrains(t *testing.T) {
	met := metrics.NewRegistry()
	sink := &blockingSink{}
	c := New(sink, Options{QueueSize: 1000, BatchSize: 1000, FlushEvery: time.Hour}, met)
	c.Run()

	const n = 500
	for i := 0; i < n; i++ {
		c.Record(Event{Code: "abc", At: time.Now()})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	if got := sink.count(); got != n {
		t.Fatalf("sink received %d of %d events — the queue was not drained; "+
			"in production this is silent data loss with no error and no log", got, n)
	}
}

func TestPeriodicFlush(t *testing.T) {
	met := metrics.NewRegistry()
	sink := &blockingSink{}
	c := New(sink, Options{QueueSize: 100, BatchSize: 1000, FlushEvery: 20 * time.Millisecond}, met)
	c.Run()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = c.Shutdown(ctx)
	}()

	c.Record(Event{Code: "abc", At: time.Now()})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if sink.count() == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("time-based flush never fired: the tail of a batch would sit in memory forever")
}

func TestSinkErrorIsCounted(t *testing.T) {
	met := metrics.NewRegistry()
	sink := &blockingSink{err: errors.New("sink down")}
	c := New(sink, Options{QueueSize: 10, BatchSize: 1, FlushEvery: time.Hour}, met)
	c.Run()

	c.Record(Event{Code: "abc", At: time.Now()})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = c.Shutdown(ctx)

	if met.Get("analytics_write_error") == 0 {
		t.Fatal("sink failure was not counted — this is where a DLQ and an alarm belong")
	}
}

func TestMemorySinkCounts(t *testing.T) {
	s := NewMemorySink()
	_ = s.Write(context.Background(), []Event{{Code: "a"}, {Code: "a"}, {Code: "b"}})
	if s.Count("a") != 2 || s.Count("b") != 1 {
		t.Fatalf("a=%d b=%d", s.Count("a"), s.Count("b"))
	}
}
