package cache

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bulutaysarac/linkly/internal/metrics"
)

func newTestCache(t *testing.T, o Options) (*Cache[string], *metrics.Registry) {
	t.Helper()
	met := metrics.NewRegistry()
	return New[string](o, met), met
}

func TestHitAndMiss(t *testing.T) {
	c, met := newTestCache(t, Options{Capacity: 10, TTL: time.Minute})
	ctx := context.Background()
	var calls atomic.Int64

	load := func(context.Context) (string, bool, error) {
		calls.Add(1)
		return "value", true, nil
	}

	for i := 0; i < 3; i++ {
		v, found, err := c.GetOrLoad(ctx, "k", load)
		if err != nil || !found || v != "value" {
			t.Fatalf("got (%q,%v,%v)", v, found, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("loader called %d times, want 1", calls.Load())
	}
	if met.Get("cache_hit") != 2 || met.Get("cache_miss") != 1 {
		t.Fatalf("hit=%d miss=%d", met.Get("cache_hit"), met.Get("cache_miss"))
	}
}

// TestNegativeCaching locks in a lesson learned the hard way: "does not exist" is a real,
// cacheable answer, distinct from an error and from an empty value.
// TR: Zor yoldan öğrenilmiş bir ders: "yok", hatadan da boş değerden de ayrı, gerçek ve
// cache'lenebilir bir cevaptır.
func TestNegativeCaching(t *testing.T) {
	c, met := newTestCache(t, Options{Capacity: 10, TTL: time.Minute, NegativeTTL: time.Minute})
	ctx := context.Background()
	var calls atomic.Int64

	load := func(context.Context) (string, bool, error) {
		calls.Add(1)
		return "", false, nil // not found, but NOT an error
	}

	for i := 0; i < 5; i++ {
		_, found, err := c.GetOrLoad(ctx, "missing", load)
		if err != nil {
			t.Fatal(err)
		}
		if found {
			t.Fatal("found = true, want false")
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("loader called %d times — negative answers are not being cached "+
			"(this is cache penetration: every read reaches the store)", calls.Load())
	}
	if met.Get("cache_negative_hit") != 4 {
		t.Fatalf("negative_hit = %d, want 4", met.Get("cache_negative_hit"))
	}
}

// TestEmptyValueIsNotTreatedAsMissing guards a bug that has been seen in production:
// an empty string is a legitimate VALUE and must be cached as found.
// TR: Üretimde görülmüş bir hatanın birebir koruması: boş string meşru bir DEĞERdir ve
// bulundu olarak cache'lenmeli.
func TestEmptyValueIsNotTreatedAsMissing(t *testing.T) {
	c, _ := newTestCache(t, Options{Capacity: 10, TTL: time.Minute})
	ctx := context.Background()
	var calls atomic.Int64

	load := func(context.Context) (string, bool, error) {
		calls.Add(1)
		return "", true, nil // empty, but it EXISTS
	}

	for i := 0; i < 3; i++ {
		v, found, _ := c.GetOrLoad(ctx, "empty", load)
		if !found || v != "" {
			t.Fatalf("got (%q,%v), want ('',true)", v, found)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("loader called %d times — 'empty' is being confused with 'absent'", calls.Load())
	}
}

func TestTTLExpiry(t *testing.T) {
	c, _ := newTestCache(t, Options{Capacity: 10, TTL: 20 * time.Millisecond})
	ctx := context.Background()
	var calls atomic.Int64
	load := func(context.Context) (string, bool, error) {
		calls.Add(1)
		return "v", true, nil
	}

	_, _, _ = c.GetOrLoad(ctx, "k", load)
	time.Sleep(60 * time.Millisecond)
	_, _, _ = c.GetOrLoad(ctx, "k", load)

	if calls.Load() != 2 {
		t.Fatalf("loader called %d times, want 2 (entry did not expire)", calls.Load())
	}
}

func TestLRUEvictionIsBounded(t *testing.T) {
	const capacity = 32
	c, met := newTestCache(t, Options{Capacity: capacity, TTL: time.Minute})
	ctx := context.Background()

	for i := 0; i < capacity*4; i++ {
		key := string(rune('a'+i%26)) + string(rune('0'+i/26))
		_, _, _ = c.GetOrLoad(ctx, key, func(context.Context) (string, bool, error) {
			return "v", true, nil
		})
	}
	if c.Len() > capacity {
		t.Fatalf("cache holds %d entries, capacity is %d — this is a memory leak", c.Len(), capacity)
	}
	if met.Get("cache_eviction") == 0 {
		t.Fatal("no evictions recorded, but capacity was exceeded")
	}
}

// TestSingleFlight is the stampede guard: N concurrent misses, 1 load.
// TR: Stampede koruması: N eşzamanlı miss, 1 yükleme.
func TestSingleFlight(t *testing.T) {
	c, met := newTestCache(t, Options{Capacity: 10, TTL: time.Minute})
	ctx := context.Background()

	release := make(chan struct{})
	var calls atomic.Int64
	load := func(context.Context) (string, bool, error) {
		calls.Add(1)
		<-release
		return "v", true, nil
	}

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, _, _ = c.GetOrLoad(ctx, "hot", load)
		}()
	}

	// Give the goroutines time to pile up on the same key, then let the winner go.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if calls.Load() != 1 {
		t.Fatalf("loader called %d times, want 1 — this is a thundering herd", calls.Load())
	}
	if met.Get("cache_stampede_wait") == 0 {
		t.Fatal("stampede_wait was never incremented; the guard is invisible")
	}
}

// TestErrorsAreNotCachedAndDoNotWedgeWaiters is the failure mode of the guard
// itself: on a load error, waiters must get the error, not hang for a TTL.
// TR: Korumanın kendi arıza modu: yükleme hatasında bekleyenler bir TTL boyu
// asılı kalmamalı, hatayı almalı.
func TestErrorsAreNotCachedAndDoNotWedgeWaiters(t *testing.T) {
	c, met := newTestCache(t, Options{Capacity: 10, TTL: time.Minute})
	ctx := context.Background()
	boom := errors.New("store is down")

	var calls atomic.Int64
	load := func(context.Context) (string, bool, error) {
		calls.Add(1)
		return "", false, boom
	}

	done := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			_, _, err := c.GetOrLoad(ctx, "k", load)
			done <- err
		}()
	}
	for i := 0; i < 8; i++ {
		select {
		case err := <-done:
			if !errors.Is(err, boom) {
				t.Fatalf("err = %v, want boom", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("a waiter hung: the stampede guard became the outage")
		}
	}

	// A later successful load must work — the error must not have been cached.
	v, found, err := c.GetOrLoad(ctx, "k", func(context.Context) (string, bool, error) {
		return "ok", true, nil
	})
	if err != nil || !found || v != "ok" {
		t.Fatalf("after error, got (%q,%v,%v)", v, found, err)
	}
	if met.Get("cache_load_error") == 0 {
		t.Fatal("cache_load_error never incremented")
	}
}

func TestInvalidate(t *testing.T) {
	c, _ := newTestCache(t, Options{Capacity: 10, TTL: time.Minute})
	ctx := context.Background()
	var calls atomic.Int64
	load := func(context.Context) (string, bool, error) {
		calls.Add(1)
		return "v", true, nil
	}

	_, _, _ = c.GetOrLoad(ctx, "k", load)
	c.Invalidate("k")
	_, _, _ = c.GetOrLoad(ctx, "k", load)

	if calls.Load() != 2 {
		t.Fatalf("loader called %d times, want 2 — Invalidate did nothing", calls.Load())
	}
}

func TestJitterStaysWithinBounds(t *testing.T) {
	base := 100 * time.Millisecond
	c, _ := newTestCache(t, Options{Capacity: 4, TTL: base, Jitter: 0.2})
	for i := 0; i < 200; i++ {
		d := c.withJitter(base)
		if d < base || d > time.Duration(float64(base)*1.2)+time.Millisecond {
			t.Fatalf("jittered ttl %v out of [%v, %v]", d, base, time.Duration(float64(base)*1.2))
		}
	}
}
