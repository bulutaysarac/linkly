#!/usr/bin/env python3
"""End-to-end smoke test for linkly.

EN: Exercises every behaviour the study notes care about against a RUNNING
    instance: cache, negative cache, TTL, tenant isolation, back pressure,
    status-code semantics and metrics. Standard library only, so it runs
    anywhere the binary runs.
TR: Çalışan bir instance'a karşı, çalışma notlarının önemsediği her davranışı
    sınar: cache, negatif cache, TTL, kiracı izolasyonu, back pressure, durum
    kodu semantiği ve metrikler. Yalnızca standart kütüphane.

Usage: python3 scripts/smoke.py [base_url]
"""
import json
import sys
import time
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor

BASE = (sys.argv[1] if len(sys.argv) > 1 else "http://localhost:8080").rstrip("/")

GREEN, RED, YELLOW, DIM, RESET = "\033[32m", "\033[31m", "\033[33m", "\033[2m", "\033[0m"
passed = failed = 0


def req(method, path, body=None, tenant=None, follow=False, timeout=10):
    url = path if path.startswith("http") else BASE + path
    data = json.dumps(body).encode() if body is not None else None
    r = urllib.request.Request(url, data=data, method=method)
    if data:
        r.add_header("Content-Type", "application/json")
    if tenant:
        r.add_header("X-Tenant-ID", tenant)

    class NoRedirect(urllib.request.HTTPRedirectHandler):
        def redirect_request(self, *a, **k):
            return None

    opener = urllib.request.build_opener() if follow else urllib.request.build_opener(NoRedirect)
    try:
        with opener.open(r, timeout=timeout) as resp:
            return resp.status, dict(resp.headers), resp.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        return e.code, dict(e.headers), e.read().decode("utf-8", "replace")


def check(name, cond, detail=""):
    global passed, failed
    if cond:
        passed += 1
        print(f"  {GREEN}PASS{RESET}  {name}")
    else:
        failed += 1
        print(f"  {RED}FAIL{RESET}  {name}" + (f"\n        {DIM}{detail}{RESET}" if detail else ""))


def section(title, topic):
    print(f"\n{YELLOW}{title}{RESET}  {DIM}{topic}{RESET}")


def wait_ready(seconds=20):
    deadline = time.time() + seconds
    while time.time() < deadline:
        try:
            s, _, _ = req("GET", "/healthz", timeout=2)
            if s == 200:
                return True
        except Exception:
            pass
        time.sleep(0.25)
    return False


def metric(text, name):
    for line in text.splitlines():
        if line.startswith("linkly_" + name + " "):
            return int(line.split()[1])
    return 0


def main():
    print(f"{DIM}linkly smoke test → {BASE}{RESET}")
    if not wait_ready():
        print(f"{RED}service never became ready{RESET}")
        return 1

    # ── 1. health & routing ────────────────────────────────────────────────
    section("1. Health and routing", "liveness ≠ readiness")
    s, _, _ = req("GET", "/healthz")
    check("GET /healthz → 200", s == 200, f"got {s}")
    s, _, _ = req("GET", "/readyz")
    check("GET /readyz → 200", s == 200, f"got {s}")
    s, _, body = req("GET", "/")
    check("GET / → 200 index", s == 200 and "linkly" in body, f"got {s}")

    # ── 2. authorization ───────────────────────────────────────────────────
    section("2. Authorization", "authentication ≠ authorization")
    s, _, _ = req("POST", "/api/v1/links", {"url": "https://example.com/"})
    check("create without tenant header → 401", s == 401, f"got {s}")

    # ── 3. unsafe targets ──────────────────────────────────────────────────
    section("3. Unsafe targets rejected", "never trust input")
    for url, why in [
        ("javascript:alert(1)", "would execute in the visitor's context"),
        ("http://169.254.169.254/latest/meta-data/", "cloud metadata endpoint"),
        ("http://127.0.0.1:8080/admin", "loopback"),
        ("http://10.0.0.5/internal", "RFC1918"),
        ("ftp://example.com/x", "scheme not allow-listed"),
    ]:
        s, _, _ = req("POST", "/api/v1/links", {"url": url}, tenant="acme")
        check(f"reject {url[:38]:<38} ({why})", s == 400, f"got {s}")

    s, _, _ = req("POST", "/api/v1/links", {"url": "https://example.com/" + "a" * 40000}, tenant="acme")
    check("oversized body -> 413 (not 400: tells the client to send LESS)", s == 413, f"got {s}")

    # ── 4. create & redirect ───────────────────────────────────────────────
    section("4. Create and redirect", "302 keeps the click data")
    s, _, body = req("POST", "/api/v1/links", {"url": "https://example.com/landing"}, tenant="acme")
    check("create → 201", s == 201, f"got {s}: {body}")
    link = json.loads(body)
    code = link["code"]
    check("code is 7 chars", len(code) == 7, f"got {code!r}")
    check("short_url is well formed", link["short_url"].endswith("/" + code), link["short_url"])

    s, h, _ = req("GET", "/" + code)
    check("redirect → 302", s == 302, f"got {s}")
    check("Location is the target", h.get("Location") == "https://example.com/landing", h.get("Location"))
    check("Cache-Control: no-store on 302", "no-store" in h.get("Cache-Control", ""), h.get("Cache-Control"))

    # ── 5. cache ───────────────────────────────────────────────────────────
    section("5. Cache on the read path", "cache-aside")
    _, _, m0 = req("GET", "/metrics")
    before_hit, before_miss = metric(m0, "cache_hit"), metric(m0, "cache_miss")
    for _ in range(5):
        req("GET", "/" + code)
    _, _, m1 = req("GET", "/metrics")
    d_hit = metric(m1, "cache_hit") - before_hit
    d_miss = metric(m1, "cache_miss") - before_miss
    check("5 repeat reads → 5 cache hits, 0 misses", d_hit == 5 and d_miss == 0, f"hits +{d_hit}, misses +{d_miss}")

    # ── 6. negative cache invalidation on create ───────────────────────────
    section("6. Negative cache is invalidated on create", "cache penetration")
    alias = "smoke" + str(int(time.time()) % 100000)
    s, _, _ = req("GET", "/" + alias)
    check(f"probe /{alias} before it exists → 404", s == 404, f"got {s}")
    s, _, _ = req("POST", "/api/v1/links", {"url": "https://example.com/promo", "alias": alias}, tenant="acme")
    check("create with that alias → 201", s == 201, f"got {s}")
    s, h, _ = req("GET", "/" + alias)
    check("resolves IMMEDIATELY (stale negative entry cleared)", s == 302,
          f"got {s} — the new link would 404 until the negative TTL expired")

    # ── 7. reserved aliases ────────────────────────────────────────────────
    section("7. Reserved aliases cannot shadow routes", "layered defence")
    for a in ["api", "healthz", "metrics"]:
        s, _, _ = req("POST", "/api/v1/links", {"url": "https://example.com/", "alias": a}, tenant="acme")
        check(f"alias {a!r} → 400", s == 400, f"got {s}")
    s, _, _ = req("GET", "/healthz")
    check("real /healthz still answers", s == 200, f"got {s}")

    # ── 8. alias conflict ──────────────────────────────────────────────────
    section("8. Alias conflict", "atomicity in the store")
    conflict = "dup" + str(int(time.time()) % 100000)
    s, _, _ = req("POST", "/api/v1/links", {"url": "https://a.example.com/", "alias": conflict}, tenant="tenant-a")
    check("first claim → 201", s == 201, f"got {s}")
    s, _, _ = req("POST", "/api/v1/links", {"url": "https://b.example.com/", "alias": conflict}, tenant="tenant-b")
    check("second claim → 409 (and no owner leaked)", s == 409, f"got {s}")

    # ── 9. tenant isolation ────────────────────────────────────────────────
    section("9. Tenant isolation", "the shard key is a security boundary")
    s, _, body = req("POST", "/api/v1/links", {"url": "https://secret.example.com/"}, tenant="tenant-a")
    secret = json.loads(body)["code"]

    s, _, _ = req("GET", "/api/v1/links/" + secret, tenant="tenant-b")
    check("cross-tenant read → 404 (NOT 403)", s == 404,
          f"got {s} — 403 would confirm the code exists: an enumeration oracle")
    s, _, _ = req("GET", "/api/v1/links/" + secret, tenant="tenant-a")
    check("owner read → 200", s == 200, f"got {s}")
    s, _, _ = req("DELETE", "/api/v1/links/" + secret, tenant="tenant-b")
    check("cross-tenant delete → 404", s == 404, f"got {s}")
    s, _, _ = req("GET", "/" + secret)
    check("link survives the refused delete", s == 302, f"got {s}")

    s, _, body = req("GET", "/api/v1/links", tenant="tenant-a")
    listed = json.loads(body)
    check("list is tenant-scoped", all("b.example.com" not in l["long_url"] for l in listed["links"]),
          json.dumps(listed)[:200])

    # ── 10. TTL / expiry ───────────────────────────────────────────────────
    section("10. TTL and expiry", "TTL is a contract")
    s, _, body = req("POST", "/api/v1/links", {"url": "https://example.com/temp", "ttl": "2s"}, tenant="acme")
    temp = json.loads(body)["code"]
    s, _, _ = req("GET", "/" + temp)
    check("fresh link redirects", s == 302, f"got {s}")
    time.sleep(2.5)
    s, _, _ = req("GET", "/" + temp)
    check("expired link → 410 Gone (not 404)", s == 410,
          f"got {s} — 410 means 'it existed and is over', which is different information")

    # ── 11. delete invalidates the cache ───────────────────────────────────
    section("11. Delete invalidates the cache", "atomicity ends at one system")
    s, _, body = req("POST", "/api/v1/links", {"url": "https://example.com/gone"}, tenant="acme")
    doomed = json.loads(body)["code"]
    req("GET", "/" + doomed)  # warm the cache
    s, _, _ = req("DELETE", "/api/v1/links/" + doomed, tenant="acme")
    check("delete → 204", s == 204, f"got {s}")
    s, _, _ = req("GET", "/" + doomed)
    check("deleted link → 404 immediately (not served from cache)", s == 404, f"got {s}")

    # ── 12. unknown code and negative caching ──────────────────────────────
    section("12. Unknown code and negative caching", "cache penetration")
    _, _, mA = req("GET", "/metrics")
    neg_before = metric(mA, "cache_negative_hit")
    s, _, _ = req("GET", "/zzzzzzz")
    check("unknown code → 404", s == 404, f"got {s}")
    for _ in range(3):
        req("GET", "/zzzzzzz")
    _, _, mB = req("GET", "/metrics")
    d_neg = metric(mB, "cache_negative_hit") - neg_before
    check("repeat lookups of a missing code hit the negative cache",
          d_neg == 3, f"negative hits +{d_neg}, want 3 — every miss is reaching the store")

    # ── 13. back pressure ──────────────────────────────────────────────────
    section("13. Back pressure", "429 + Retry-After is a protocol")
    def hit(_):
        return req("GET", "/healthz")
    with ThreadPoolExecutor(max_workers=24) as ex:
        results = list(ex.map(hit, range(400)))
    codes = [r[0] for r in results]
    n429 = codes.count(429)
    check(f"burst of 400 requests produced 429s ({n429} of 400)", n429 > 0,
          "no 429 at all — the limiter is not enforcing")
    if n429:
        ra = next(h.get("Retry-After") for s, h, _ in results if s == 429)
        check("429 carries Retry-After", ra not in (None, ""), f"Retry-After={ra!r}")

    # EN: Let the token bucket refill before the remaining checks, otherwise the
    #     limiter we just proved works would reject them too.
    # TR: Kalan kontrollerden önce kova dolsun; yoksa az önce çalıştığını
    #     kanıtladığımız limiter onları da reddeder.
    time.sleep(3.5)

    # ── 14. metrics ────────────────────────────────────────────────────────
    section("14. Metrics", "a guard you cannot see is a guard you do not have")
    _, _, m = req("GET", "/metrics")
    for name in ["http_requests_total", "redirect_ok", "redirect_not_found", "cache_hit",
                 "cache_miss", "cache_negative_hit", "analytics_enqueued", "ratelimit_reject"]:
        check(f"metric linkly_{name} present", ("linkly_" + name + " ") in m,
              "counter missing from /metrics")
    check("analytics were enqueued off the request path", metric(m, "analytics_enqueued") > 0)
    check("nothing was dropped at this volume", metric(m, "analytics_dropped") == 0,
          f"dropped={metric(m, 'analytics_dropped')}")

    print()
    total = passed + failed
    if failed == 0:
        print(f"{GREEN}ALL {total} CHECKS PASSED{RESET}")
        return 0
    print(f"{RED}{failed} of {total} checks FAILED{RESET}")
    return 1


if __name__ == "__main__":
    sys.exit(main())
