package shortener

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bulutaysarac/linkly/internal/analytics"
	"github.com/bulutaysarac/linkly/internal/cache"
	"github.com/bulutaysarac/linkly/internal/metrics"
	"github.com/bulutaysarac/linkly/internal/store"
)

func newTestService(t *testing.T) (*Service, *store.Memory, *analytics.MemorySink) {
	t.Helper()
	met := metrics.NewRegistry()
	st := store.NewMemory(8)
	c := cache.New[store.Link](cache.Options{
		Capacity: 100, TTL: time.Minute, NegativeTTL: time.Minute,
	}, met)
	sink := analytics.NewMemorySink()
	cl := analytics.New(sink, analytics.Options{QueueSize: 100, BatchSize: 1, FlushEvery: 10 * time.Millisecond}, met)
	cl.Run()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = cl.Shutdown(ctx)
	})
	return New(st, c, cl, met, 7, 5), st, sink
}

func TestValidateTargetURL(t *testing.T) {
	ok := []string{
		"https://example.com",
		"http://example.com/a/b?c=d",
		"https://sub.example.co.uk/path",
	}
	for _, u := range ok {
		if err := ValidateTargetURL(u); err != nil {
			t.Errorf("ValidateTargetURL(%q) = %v, want nil", u, err)
		}
	}

	bad := map[string]string{
		"":                              "empty",
		"   ":                           "blank",
		"not a url":                     "no scheme",
		"ftp://example.com":             "scheme not allow-listed",
		"javascript:alert(1)":           "would execute in the visitor's context",
		"data:text/html,<script>":       "data URI payload",
		"file:///etc/passwd":            "local file",
		"http://localhost:8080/admin":   "loopback by name",
		"http://127.0.0.1/admin":        "loopback by IP",
		"http://10.0.0.5/internal":      "RFC1918",
		"http://192.168.1.1/router":     "RFC1918",
		"http://169.254.169.254/latest": "cloud instance metadata endpoint",
		"http://foo.internal/x":         "internal TLD",
	}
	for u, why := range bad {
		if err := ValidateTargetURL(u); err == nil {
			t.Errorf("ValidateTargetURL(%q) = nil, want error (%s)", u, why)
		}
	}
}

func TestCreateAndResolve(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()

	l, err := svc.Create(ctx, CreateRequest{TenantID: "t1", LongURL: "https://example.com/x"})
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Code) != 7 {
		t.Fatalf("code %q length %d, want 7", l.Code, len(l.Code))
	}

	got, err := svc.Resolve(ctx, l.Code)
	if err != nil {
		t.Fatal(err)
	}
	if got.LongURL != "https://example.com/x" {
		t.Fatalf("LongURL = %q", got.LongURL)
	}
}

func TestResolveUnknownIsNotFound(t *testing.T) {
	svc, _, _ := newTestService(t)
	if _, err := svc.Resolve(context.Background(), "nosuch1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestCreateInvalidatesNegativeCache is the subtle one.
//
// Someone probes a code BEFORE it exists → a negative entry is cached.
// Creating that code must clear it, otherwise the brand-new link 404s until the
// negative TTL expires: a broken link that mysteriously fixes itself later.
// TR: İnce olan bu. Biri kodu var olmadan ÖNCE yokluyor → negatif kayıt cache'leniyor.
// O kodu oluşturmak onu temizlemeli; yoksa yepyeni link, negatif TTL sönene kadar
// 404 verir — sonradan kendi kendini düzelten, gizemli bir bozuk link.
func TestCreateInvalidatesNegativeCache(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	const alias = "promo24"

	// 1. probe before it exists → caches "not found"
	if _, err := svc.Resolve(ctx, alias); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("probe err = %v, want ErrNotFound", err)
	}

	// 2. create it
	if _, err := svc.Create(ctx, CreateRequest{
		TenantID: "t1", LongURL: "https://example.com", Alias: alias,
	}); err != nil {
		t.Fatal(err)
	}

	// 3. it must resolve immediately, not after the negative TTL
	got, err := svc.Resolve(ctx, alias)
	if err != nil {
		t.Fatalf("resolve after create: %v — the stale negative cache entry was not "+
			"invalidated; the new link would 404 until the negative TTL expired", err)
	}
	if got.LongURL != "https://example.com" {
		t.Fatalf("LongURL = %q", got.LongURL)
	}
}

func TestAliasTaken(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	req := CreateRequest{TenantID: "t1", LongURL: "https://example.com", Alias: "takenx"}

	if _, err := svc.Create(ctx, req); err != nil {
		t.Fatal(err)
	}
	req.TenantID = "t2"
	if _, err := svc.Create(ctx, req); !errors.Is(err, ErrAliasTaken) {
		t.Fatalf("err = %v, want ErrAliasTaken", err)
	}
}

func TestReservedAliasRejected(t *testing.T) {
	svc, _, _ := newTestService(t)
	_, err := svc.Create(context.Background(), CreateRequest{
		TenantID: "t1", LongURL: "https://example.com", Alias: "api",
	})
	if !errors.Is(err, ErrInvalidAlias) {
		t.Fatalf("err = %v, want ErrInvalidAlias — 'api' would shadow the management API", err)
	}
}

// TestExpiryIsCheckedAfterCache: the cache TTL and the link TTL are two clocks.
// TR: Cache TTL'i ile link TTL'i iki ayrı saat.
func TestExpiryIsCheckedAfterCache(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()

	now := time.Now()
	svc.now = func() time.Time { return now }

	l, err := svc.Create(ctx, CreateRequest{
		TenantID: "t1", LongURL: "https://example.com", TTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}

	// warm the cache while still valid
	if _, err := svc.Resolve(ctx, l.Code); err != nil {
		t.Fatal(err)
	}

	// move past the link's expiry, but stay well inside the cache TTL
	now = now.Add(2 * time.Hour)

	if _, err := svc.Resolve(ctx, l.Code); !errors.Is(err, ErrGone) {
		t.Fatalf("err = %v, want ErrGone — an expired link was served from cache", err)
	}
}

func TestDeleteInvalidatesCache(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()

	l, err := svc.Create(ctx, CreateRequest{TenantID: "t1", LongURL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Resolve(ctx, l.Code); err != nil { // warm the cache
		t.Fatal(err)
	}
	if err := svc.Delete(ctx, "t1", l.Code); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Resolve(ctx, l.Code); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound — a deleted link is still served from cache", err)
	}
}

func TestDeleteAcrossTenantsIsRefused(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	l, _ := svc.Create(ctx, CreateRequest{TenantID: "t1", LongURL: "https://example.com"})

	if err := svc.Delete(ctx, "t2", l.Code); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
	if _, err := svc.Resolve(ctx, l.Code); err != nil {
		t.Fatal("a refused cross-tenant delete must not affect the link")
	}
}

func TestRecordClickReachesSink(t *testing.T) {
	svc, _, sink := newTestService(t)
	ctx := context.Background()
	l, _ := svc.Create(ctx, CreateRequest{TenantID: "t1", LongURL: "https://example.com"})

	for i := 0; i < 3; i++ {
		svc.RecordClick(l, "")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if sink.Count(l.Code) == 3 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("sink saw %d clicks, want 3", sink.Count(l.Code))
}
