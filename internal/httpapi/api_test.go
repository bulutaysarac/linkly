package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bulutaysarac/linkly/internal/analytics"
	"github.com/bulutaysarac/linkly/internal/cache"
	"github.com/bulutaysarac/linkly/internal/config"
	"github.com/bulutaysarac/linkly/internal/metrics"
	"github.com/bulutaysarac/linkly/internal/ratelimit"
	"github.com/bulutaysarac/linkly/internal/shortener"
	"github.com/bulutaysarac/linkly/internal/store"
)

type harness struct {
	srv  *httptest.Server
	met  *metrics.Registry
	sink *analytics.MemorySink
}

func newHarness(t *testing.T, tweak func(*config.Config)) *harness {
	t.Helper()

	cfg := config.Load()
	cfg.BaseURL = "http://short.test"
	cfg.RateLimitPerSec = 1000
	cfg.RateLimitBurst = 1000
	cfg.RequestTimeout = 2 * time.Second
	if tweak != nil {
		tweak(&cfg)
	}

	met := metrics.NewRegistry()
	st := store.NewMemory(8)
	c := cache.New[store.Link](cache.Options{
		Capacity: 100, TTL: time.Minute, NegativeTTL: 10 * time.Second,
	}, met)
	sink := analytics.NewMemorySink()
	cl := analytics.New(sink, analytics.Options{QueueSize: 100, BatchSize: 1, FlushEvery: 10 * time.Millisecond}, met)
	cl.Run()

	svc := shortener.New(st, c, cl, met, cfg.CodeLength, cfg.MaxAttempts)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	lim := ratelimit.New(cfg.RateLimitPerSec, cfg.RateLimitBurst, met)
	api := NewServer(svc, cfg, met, log, lim)

	srv := httptest.NewServer(api.Handler())
	t.Cleanup(func() {
		srv.Close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = cl.Shutdown(ctx)
	})
	return &harness{srv: srv, met: met, sink: sink}
}

// noRedirectClient stops the client from following redirects, so tests can
// inspect the 302 itself.
func noRedirectClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       5 * time.Second,
	}
}

// mustGet / mustDo fail the test on a transport error instead of letting a nil
// response through. `go vet` flags the lazy version, and it is right to: using a
// response without checking the error is how a test panics with a nil pointer
// instead of reporting what actually went wrong.
// TR: mustGet / mustDo, taşıma hatasında testi düşürür; nil bir response'un
// geçmesine izin vermez. `go vet` tembel hâli işaretliyor ve haklı: hatayı kontrol
// etmeden response kullanmak, testin gerçekte ne olduğunu bildirmek yerine nil
// pointer ile panic etmesinin yoludur.
func mustGet(t *testing.T, url string) *http.Response {
	t.Helper()
	resp, err := noRedirectClient().Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp
}

func mustDo(t *testing.T, req *http.Request) *http.Response {
	t.Helper()
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	return resp
}

func (h *harness) create(t *testing.T, tenant, body string) (*http.Response, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/api/v1/links", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if tenant != "" {
		req.Header.Set(TenantHeader, tenant)
	}
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	b, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(b, &out)
	return resp, out
}

func TestCreateAndRedirectEndToEnd(t *testing.T) {
	h := newHarness(t, nil)

	resp, body := h.create(t, "acme", `{"url":"https://example.com/landing"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %v)", resp.StatusCode, body)
	}
	code, _ := body["code"].(string)
	if code == "" {
		t.Fatalf("no code in response: %v", body)
	}
	if su, _ := body["short_url"].(string); su != "http://short.test/"+code {
		t.Fatalf("short_url = %q", su)
	}

	r := mustGet(t, h.srv.URL+"/"+code)
	defer r.Body.Close()

	if r.StatusCode != http.StatusFound {
		t.Fatalf("redirect status = %d, want 302", r.StatusCode)
	}
	if loc := r.Header.Get("Location"); loc != "https://example.com/landing" {
		t.Fatalf("Location = %q", loc)
	}
	// 302 must not be cacheable, or a proxy would swallow the clicks.
	// TR: 302 cache'lenebilir olmamalı, yoksa bir proxy tıklamaları yutar.
	if cc := r.Header.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}

	// the click is recorded asynchronously
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.sink.Count(code) == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("click not recorded, sink count = %d", h.sink.Count(code))
}

func TestPermanentRedirectMode(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.PermanentRedirect = true })
	_, body := h.create(t, "acme", `{"url":"https://example.com/"}`)
	code := body["code"].(string)

	r := mustGet(t, h.srv.URL+"/"+code)
	defer r.Body.Close()
	if r.StatusCode != http.StatusMovedPermanently {
		t.Fatalf("status = %d, want 301", r.StatusCode)
	}
	if cc := r.Header.Get("Cache-Control"); !strings.Contains(cc, "max-age=86400") {
		t.Fatalf("Cache-Control = %q, want a long max-age for 301", cc)
	}
}

func TestUnknownCodeIs404(t *testing.T) {
	h := newHarness(t, nil)
	r := mustGet(t, h.srv.URL+"/zzzzzzz")
	defer r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", r.StatusCode)
	}
}

func TestExpiredLinkIs410(t *testing.T) {
	h := newHarness(t, nil)
	_, body := h.create(t, "acme", `{"url":"https://example.com/","ttl":"50ms"}`)
	code := body["code"].(string)

	time.Sleep(120 * time.Millisecond)

	r := mustGet(t, h.srv.URL+"/"+code)
	defer r.Body.Close()
	if r.StatusCode != http.StatusGone {
		t.Fatalf("status = %d, want 410 Gone (not 404: it existed and is now over)", r.StatusCode)
	}
}

func TestUnsafeURLsRejected(t *testing.T) {
	h := newHarness(t, nil)
	for _, u := range []string{
		"javascript:alert(1)",
		"http://169.254.169.254/latest/meta-data/",
		"http://127.0.0.1:8080/admin",
		"ftp://example.com/x",
	} {
		b, _ := json.Marshal(map[string]string{"url": u})
		resp, _ := h.create(t, "acme", string(b))
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("POST %q → %d, want 400", u, resp.StatusCode)
		}
	}
}

// TestCrossTenantReadReturns404NotForbidden is the security test of this package.
// A 403 would confirm the code exists, which is an enumeration oracle.
// TR: Bu paketin güvenlik testi. 403 dönmek kodun var olduğunu onaylar; bu bir
// numaralandırma kâhinidir.
func TestCrossTenantReadReturns404NotForbidden(t *testing.T) {
	h := newHarness(t, nil)
	_, body := h.create(t, "tenant-a", `{"url":"https://secret.example.com/"}`)
	code := body["code"].(string)

	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/api/v1/links/"+code, nil)
	req.Header.Set(TenantHeader, "tenant-b")
	r := mustDo(t, req)
	defer r.Body.Close()

	if r.StatusCode == http.StatusForbidden {
		t.Fatal("403 leaks existence: tenant-b now knows this code is real")
	}
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", r.StatusCode)
	}

	// and the owner still sees it
	req2, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/api/v1/links/"+code, nil)
	req2.Header.Set(TenantHeader, "tenant-a")
	r2 := mustDo(t, req2)
	defer r2.Body.Close()
	if r2.StatusCode != http.StatusOK {
		t.Fatalf("owner status = %d, want 200", r2.StatusCode)
	}
}

func TestCrossTenantDeleteRefused(t *testing.T) {
	h := newHarness(t, nil)
	_, body := h.create(t, "tenant-a", `{"url":"https://example.com/"}`)
	code := body["code"].(string)

	req, _ := http.NewRequest(http.MethodDelete, h.srv.URL+"/api/v1/links/"+code, nil)
	req.Header.Set(TenantHeader, "tenant-b")
	r := mustDo(t, req)
	defer r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", r.StatusCode)
	}

	// link must still work
	rr := mustGet(t, h.srv.URL+"/"+code)
	defer rr.Body.Close()
	if rr.StatusCode != http.StatusFound {
		t.Fatal("a refused cross-tenant delete must not affect the link")
	}
}

func TestListIsTenantScoped(t *testing.T) {
	h := newHarness(t, nil)
	h.create(t, "tenant-a", `{"url":"https://a1.example.com/"}`)
	h.create(t, "tenant-a", `{"url":"https://a2.example.com/"}`)
	h.create(t, "tenant-b", `{"url":"https://b1.example.com/"}`)

	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/api/v1/links", nil)
	req.Header.Set(TenantHeader, "tenant-a")
	r := mustDo(t, req)
	defer r.Body.Close()

	var out struct {
		Count int `json:"count"`
		Links []struct {
			LongURL string `json:"long_url"`
		} `json:"links"`
	}
	_ = json.NewDecoder(r.Body).Decode(&out)
	if out.Count != 2 {
		t.Fatalf("count = %d, want 2", out.Count)
	}
	for _, l := range out.Links {
		if strings.Contains(l.LongURL, "b1.example.com") {
			t.Fatal("tenant-b's link leaked into tenant-a's list")
		}
	}
}

func TestMissingTenantHeaderIs401(t *testing.T) {
	h := newHarness(t, nil)
	resp, _ := h.create(t, "", `{"url":"https://example.com/"}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

// TestRateLimitReturns429WithRetryAfter: a bare "no" invites a retry storm.
// TR: Çıplak bir "hayır" retry storm'u davet eder.
func TestRateLimitReturns429WithRetryAfter(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.RateLimitPerSec = 1
		c.RateLimitBurst = 2
	})

	var got429 bool
	for i := 0; i < 10; i++ {
		r := mustGet(t, h.srv.URL+"/healthz")
		status := r.StatusCode
		retryAfter := r.Header.Get("Retry-After")
		r.Body.Close()

		if status == http.StatusTooManyRequests {
			got429 = true
			if retryAfter == "" {
				t.Fatal("429 without Retry-After: a bare rejection invites a retry storm")
			}
			break
		}
	}
	if !got429 {
		t.Fatal("rate limiter never rejected")
	}
}

func TestReservedAliasCannotShadowRoutes(t *testing.T) {
	h := newHarness(t, nil)
	for _, alias := range []string{"api", "healthz", "metrics"} {
		b, _ := json.Marshal(map[string]string{"url": "https://example.com/", "alias": alias})
		resp, _ := h.create(t, "acme", string(b))
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("alias %q accepted with status %d — it would shadow a real route", alias, resp.StatusCode)
		}
	}
	// and the real routes still answer
	r := mustGet(t, h.srv.URL+"/healthz")
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("/healthz status = %d", r.StatusCode)
	}
}

func TestAliasConflictIs409(t *testing.T) {
	h := newHarness(t, nil)
	body := `{"url":"https://example.com/","alias":"launch"}`
	if resp, _ := h.create(t, "tenant-a", body); resp.StatusCode != http.StatusCreated {
		t.Fatalf("first create status = %d", resp.StatusCode)
	}
	resp, _ := h.create(t, "tenant-b", body)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
}

// TestMalformedJSONIs400 pins the OTHER side of the body-size branch.
//
// The 413 branch is a conditional in front of a pre-existing 400. Only the
// matching side was covered, which means a guard that erroneously matched EVERY
// decoder error — turning all bad JSON into 413 — would pass the whole suite.
// A test that only proves the happy half of a new conditional is not covering
// the conditional; it is covering the branch it happens to take.
// TR: Gövde-boyutu dalının ÖTEKİ tarafını sabitliyor. 413 dalı, mevcut bir 400'ün
// önüne konmuş bir koşul; yalnızca eşleşen taraf kapsanıyordu. Yani yanlışlıkla
// HER decoder hatasını yakalayan bir guard — tüm bozuk JSON'ı 413'e çeviren —
// tüm paketi geçerdi. Yeni bir koşulun sadece mutlu yarısını kanıtlayan bir test,
// koşulu kapsamıyor; koşulun rastgele girdiği dalı kapsıyor.
func TestMalformedJSONIs400(t *testing.T) {
	h := newHarness(t, nil)
	for _, body := range []string{
		`{"url":`,         // truncated
		`not json at all`, // not JSON
		`{"url":123}`,     // right shape, wrong type
		``,                // empty
	} {
		resp, _ := h.create(t, "acme", body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("body %q -> %d, want 400 (413 would mean the size guard is "+
				"swallowing every decode error)", body, resp.StatusCode)
		}
	}
}

// TestBodySizeBoundary pins the cap itself, from both sides.
// TR: Sınırın kendisini iki taraftan da sabitliyor.
func TestBodySizeBoundary(t *testing.T) {
	h := newHarness(t, nil)

	// `pad` is an unknown field, so encoding/json ignores it — it exists only to
	// move the body across the 8 KiB limit without touching the 2048-char URL cap.
	// TR: `pad` bilinmeyen bir alan, encoding/json yok sayıyor — tek işi gövdeyi
	// 8 KiB sınırının öteki tarafına taşımak, 2048 karakterlik URL sınırına
	// dokunmadan.
	body := func(padBytes int) string {
		return `{"url":"https://example.com/","pad":"` + strings.Repeat("a", padBytes) + `"}`
	}

	if resp, _ := h.create(t, "acme", body(7000)); resp.StatusCode != http.StatusCreated {
		t.Fatalf("body under the cap -> %d, want 201", resp.StatusCode)
	}
	if resp, _ := h.create(t, "acme", body(9000)); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("body over the cap -> %d, want 413", resp.StatusCode)
	}
}

// TestOversizedBodyRejected also pins the STATUS, not just the rejection.
// 400 would tell a client "I could not parse that", and it would retry with the
// same oversized body; 413 tells it to send less.
// TR: Sadece reddi değil DURUMU da sabitliyor. 400, istemciye "ayrıştıramadım"
// der ve aynı büyük gövdeyle tekrar dener; 413 daha az göndermesi gerektiğini söyler.
func TestOversizedBodyRejected(t *testing.T) {
	h := newHarness(t, nil)
	big := bytes.Repeat([]byte("a"), 32<<10)
	resp, _ := h.create(t, "acme", `{"url":"https://example.com/`+string(big)+`"}`)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
}

func TestMetricsEndpointExposesCounters(t *testing.T) {
	h := newHarness(t, nil)
	_, body := h.create(t, "acme", `{"url":"https://example.com/"}`)
	code := body["code"].(string)
	for i := 0; i < 3; i++ {
		r := mustGet(t, h.srv.URL+"/"+code)
		r.Body.Close()
	}

	r := mustGet(t, h.srv.URL+"/metrics")
	defer r.Body.Close()
	out, _ := io.ReadAll(r.Body)
	text := string(out)

	for _, want := range []string{"linkly_redirect_ok", "linkly_cache_hit", "linkly_http_requests_total"} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics output missing %q\n%s", want, text)
		}
	}
	// 1 miss on the first redirect, 2 hits after — proof the cache is on the path.
	// TR: İlk yönlendirmede 1 miss, sonra 2 hit — cache'in yolda olduğunun kanıtı.
	if h.met.Get("cache_hit") != 2 || h.met.Get("cache_miss") != 1 {
		t.Fatalf("cache_hit=%d cache_miss=%d, want 2 and 1",
			h.met.Get("cache_hit"), h.met.Get("cache_miss"))
	}
}
