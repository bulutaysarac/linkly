package ratelimit

import (
	"testing"
	"time"

	"github.com/bulutaysarac/linkly/internal/metrics"
)

func TestBurstThenReject(t *testing.T) {
	met := metrics.NewRegistry()
	l := New(10, 3, met) // 10/s, burst 3
	now := time.Now()
	l.now = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow("t1"); !ok {
			t.Fatalf("request %d rejected inside burst", i)
		}
	}
	ok, retryAfter := l.Allow("t1")
	if ok {
		t.Fatal("4th request allowed, burst is 3")
	}
	if retryAfter <= 0 {
		t.Fatal("retryAfter must be positive: a bare 'no' invites a retry storm")
	}
	if retryAfter > time.Second {
		t.Fatalf("retryAfter %v looks wrong for 10 tokens/s", retryAfter)
	}
}

func TestRefillOverTime(t *testing.T) {
	met := metrics.NewRegistry()
	l := New(10, 1, met)
	now := time.Now()
	l.now = func() time.Time { return now }

	if ok, _ := l.Allow("t1"); !ok {
		t.Fatal("first request rejected")
	}
	if ok, _ := l.Allow("t1"); ok {
		t.Fatal("second immediate request allowed")
	}

	now = now.Add(200 * time.Millisecond) // 2 tokens at 10/s
	if ok, _ := l.Allow("t1"); !ok {
		t.Fatal("request after refill rejected")
	}
}

// TestKeysAreIsolated is the noisy-neighbour guarantee.
// TR: Gürültülü komşu garantisi.
func TestKeysAreIsolated(t *testing.T) {
	met := metrics.NewRegistry()
	l := New(1, 2, met)
	now := time.Now()
	l.now = func() time.Time { return now }

	for i := 0; i < 5; i++ {
		l.Allow("tenant:noisy")
	}
	if ok, _ := l.Allow("tenant:quiet"); !ok {
		t.Fatal("a quiet tenant was punished for a noisy one's traffic")
	}
}

// TestDryRunCountsButDoesNotReject encodes the "calibrate before enforcing" rule.
// TR: "Zorlamadan önce kalibre et" kuralını kodluyor.
func TestDryRunCountsButDoesNotReject(t *testing.T) {
	met := metrics.NewRegistry()
	l := New(1, 1, met)
	l.DryRun = true
	now := time.Now()
	l.now = func() time.Time { return now }

	for i := 0; i < 5; i++ {
		if ok, _ := l.Allow("t1"); !ok {
			t.Fatal("dry run rejected a request")
		}
	}
	if got := met.Get("ratelimit_would_reject"); got != 4 {
		t.Fatalf("would_reject = %d, want 4 — you cannot calibrate what you do not count", got)
	}
	if met.Get("ratelimit_reject") != 0 {
		t.Fatal("dry run incremented the real reject counter")
	}
}

// TestGCBoundsMemory: a per-key map without eviction is a self-inflicted DoS.
// TR: Tahliyesiz anahtar-başına map, kendi ayağına sıkılmış bir DoS'tur.
func TestGCBoundsMemory(t *testing.T) {
	met := metrics.NewRegistry()
	l := New(10, 10, met)
	now := time.Now()
	l.now = func() time.Time { return now }

	for i := 0; i < 100; i++ {
		l.Allow(string(rune('a'+i%26)) + string(rune('0'+i/26)))
	}
	if len(l.buckets) == 0 {
		t.Fatal("no buckets created")
	}
	now = now.Add(time.Hour)
	removed := l.GC(time.Minute)
	if removed == 0 || len(l.buckets) != 0 {
		t.Fatalf("GC removed %d, %d left", removed, len(l.buckets))
	}
}
