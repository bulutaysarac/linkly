// Package ratelimit is a per-key token bucket with a Retry-After hint.
//
// EN: Rate limiting is back pressure applied at the cheapest possible place: the
//
//	door. Work rejected here consumes no connection, no goroutine, no database
//	round trip. Every other form of back pressure (queue bounds, concurrency
//	limits, circuit breakers) fires after you have already paid for the request.
//
// TR: Rate limiting, back pressure'ın olabilecek en ucuz yerde uygulanmış hâli:
//
//	kapıda. Burada reddedilen iş ne bir bağlantı, ne bir goroutine, ne de bir
//	veritabanı turu harcar. Back pressure'ın diğer bütün biçimleri (kuyruk
//	sınırı, eşzamanlılık sınırı, devre kesici) sen isteğin bedelini ödedikten
//	sonra devreye girer.
//
// [Topic · Konu: Back pressure — girişte]
package ratelimit

import (
	"sync"
	"time"

	"github.com/bulutaysarac/linkly/internal/metrics"
)

type bucket struct {
	tokens float64
	last   time.Time
}

// Limiter allows Rate requests/second per key, bursting up to Burst.
type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket

	rate  float64 // tokens per second
	burst float64

	// DryRun counts what it *would* have rejected, but rejects nothing.
	//
	// EN: This flag is the most underrated line in the file. The most expensive
	//     mistake in rate limiting is picking a number that cuts legitimate
	//     traffic — and you cannot know that number from a design meeting, only
	//     from the measured distribution. So a new limit ships in dry-run first,
	//     you watch ratelimit_would_reject for a while, and only then enforce.
	//     Mature platforms do exactly this: the limit ships as provisional, and is only
	//     fixed once the dry-run feed shows what the real distribution looks like.
	// TR: Bu bayrak dosyanın en hafife alınan satırı. Rate limiting'de yapılabilecek
	//     en pahalı hata, meşru trafiği kesen bir sayı seçmektir — ve o sayıyı bir
	//     tasarım toplantısından değil, yalnızca ölçülen dağılımdan bilebilirsin.
	//     Bu yüzden yeni bir limit önce dry-run ile çıkar, bir süre
	//     ratelimit_would_reject izlenir, ancak ondan sonra zorlamaya geçilir.
	//     Olgun platformlar birebir bunu yapıyor: limit geçici olarak çıkıyor ve ancak
	//     dry-run beslemesi gerçek dağılımı gösterdikten sonra sabitleniyor.
	DryRun bool

	met *metrics.Registry
	now func() time.Time
}

func New(ratePerSec, burst float64, met *metrics.Registry) *Limiter {
	return &Limiter{
		buckets: make(map[string]*bucket),
		rate:    ratePerSec,
		burst:   burst,
		met:     met,
		now:     time.Now,
	}
}

// Allow reports whether the key may proceed, and how long to wait if not.
//
// EN: Returning retryAfter is not politeness, it is protocol. Answering a bare
//
//	"no" invites a retry storm — every rejected client comes back immediately
//	and in unison. Telling the client *when* to return converts a defensive
//	measure into a two-sided agreement, and spreads the retries out.
//
// TR: retryAfter döndürmek kibarlık değil, protokol. Çıplak bir "hayır" retry
//
//	storm'u davet eder — reddedilen her istemci hemen ve hep birlikte geri
//	gelir. İstemciye *ne zaman* döneceğini söylemek, savunma önlemini iki
//	taraflı bir anlaşmaya çevirir ve retry'ları yayar.
//
// [Topic · Konu: 429 + Retry-After]
func (l *Limiter) Allow(key string) (ok bool, retryAfter time.Duration) {
	now := l.now()

	l.mu.Lock()
	b, exists := l.buckets[key]
	if !exists {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}

	// Refill proportionally to elapsed time, capped at burst.
	// TR: Geçen süreyle orantılı doldur, burst ile sınırla.
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens = min(l.burst, b.tokens+elapsed*l.rate)
		b.last = now
	}

	if b.tokens >= 1 {
		b.tokens--
		l.mu.Unlock()
		l.met.Inc("ratelimit_allow")
		return true, 0
	}

	// Not enough tokens: compute how long until one is available.
	deficit := 1 - b.tokens
	wait := time.Duration(deficit / l.rate * float64(time.Second))
	l.mu.Unlock()

	if l.DryRun {
		l.met.Inc("ratelimit_would_reject")
		return true, 0
	}
	l.met.Inc("ratelimit_reject")
	return false, wait
}

// GC drops buckets untouched for longer than idle.
//
// EN: Without this the map grows once per distinct key, forever. A rate limiter
//
//	that leaks memory is a denial of service you built yourself: an attacker
//	only has to send one request from many keys. Any per-key map needs an
//	eviction story — the same rule as the cache.
//
// TR: Bu olmasa map her farklı anahtar için bir kez, sonsuza kadar büyür. Bellek
//
//	sızdıran bir rate limiter, kendi ellerinle kurduğun bir hizmet dışı bırakma
//	saldırısıdır: saldırganın tek yapması gereken çok sayıda anahtardan birer
//	istek atmak. Anahtar başına tutulan her map'in bir tahliye hikâyesi olmalı —
//	cache'le aynı kural.
func (l *Limiter) GC(idle time.Duration) int {
	cutoff := l.now().Add(-idle)
	removed := 0
	l.mu.Lock()
	for k, b := range l.buckets {
		if b.last.Before(cutoff) {
			delete(l.buckets, k)
			removed++
		}
	}
	l.mu.Unlock()
	return removed
}
