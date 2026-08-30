// Package cache is a bounded, TTL'd, stampede-protected read-through cache.
//
// EN: A URL shortener is the canonical read-heavy workload — writes are rare,
//
//	reads are enormous, and the mapping almost never changes. That profile is
//	what makes a cache worth its complexity here. This file implements, in one
//	place, the five cache decisions the study notes insist you must answer
//	before adding any cache at all:
//	  1. strategy      → cache-aside (read-through helper), see GetOrLoad
//	  2. invalidation  → TTL + explicit Invalidate
//	  3. eviction      → LRU, bounded capacity
//	  4. failure modes → stampede lock, negative caching, TTL jitter
//	  5. observability → a counter for every path, including the silent ones
//
// TR: Link kısaltıcı, okuma-ağırlıklı iş yükünün ders kitabı örneğidir — yazma
//
//	nadir, okuma devasa, ve eşleme neredeyse hiç değişmiyor. Cache'in buradaki
//	karmaşıklığını hak etmesinin sebebi bu profil. Bu dosya, çalışma notlarının
//	"cache eklemeden önce cevaplamalısın" dediği beş kararı tek yerde uyguluyor:
//	  1. strateji     → cache-aside, bkz. GetOrLoad
//	  2. invalidation → TTL + açık Invalidate
//	  3. tahliye      → LRU, sınırlı kapasite
//	  4. arıza modları→ stampede kilidi, negatif cache, TTL jitter
//	  5. gözlem       → her yol için bir sayaç, sessiz olanlar dahil
//
// [Topic · Konu: Cache'in tamamı]
package cache

import (
	"container/list"
	"context"
	"math/rand"
	"sync"
	"time"

	"github.com/bulutaysarac/linkly/internal/metrics"
)

// Loader fetches a value on a miss.
//
// EN: Note the signature: (value, found, error) — three outcomes, not two.
//
//	"found=false, err=nil" means *the source of truth says this does not exist*,
//	which is a legitimate, cacheable answer. Collapsing that into a zero value
//	or into an error is a classic production failure: when "empty" and "absent" share one
//	representation, the cache silently stops working for that key and every
//	read falls through to the store forever.
//
// TR: İmzaya dikkat: (değer, bulundu, hata) — iki değil üç sonuç.
//
//	"found=false, err=nil" demek, *kaynak veri bunun olmadığını söylüyor*
//	demektir ve bu meşru, cache'lenebilir bir cevaptır. Bunu sıfır değere ya da
//	hataya indirgemek klasik bir üretim hatasıdır: "boş" ile "yok" tek bir gösterimi
//	paylaşınca cache o anahtar için sessizce çalışmayı bırakır ve her okuma
//	sonsuza kadar depoya iner.
//
// [Topic · Konu: Cache penetration / negatif cache]
type Loader[V any] func(ctx context.Context) (V, bool, error)

type entry[V any] struct {
	key     string
	val     V
	found   bool
	expires time.Time
}

type call[V any] struct {
	wg    sync.WaitGroup
	val   V
	found bool
	err   error
}

// Cache is an LRU + TTL cache with single-flight loading.
type Cache[V any] struct {
	mu       sync.Mutex
	ll       *list.List
	items    map[string]*list.Element
	inflight map[string]*call[V]

	capacity int
	ttl      time.Duration
	negTTL   time.Duration
	jitter   float64

	met *metrics.Registry
	now func() time.Time
}

type Options struct {
	Capacity int
	TTL      time.Duration

	// NegativeTTL is the (much shorter) lifetime of a "does not exist" answer.
	//
	// EN: Shorter on purpose. A negative entry protects the store from repeated
	//     lookups of a key that does not exist (cache penetration — and, with a
	//     hostile client, a cheap way to aim traffic straight at your database).
	//     But it must expire fast, because "does not exist" can become "exists"
	//     the moment someone creates that link. Positive answers are stable;
	//     negative answers are a bet on the near future.
	// TR: Bilinçli olarak kısa. Negatif kayıt, var olmayan bir anahtarın tekrar
	//     tekrar sorgulanmasından depoyu korur (cache penetration — kötü niyetli
	//     bir istemci için doğrudan veritabanına nişan almanın ucuz yolu). Ama
	//     hızlı sönmeli, çünkü "yok", biri o linki oluşturduğu anda "var" olur.
	//     Pozitif cevaplar kararlıdır; negatif cevaplar yakın geleceğe oynanan
	//     bir bahistir.
	NegativeTTL time.Duration

	// Jitter spreads expiry times, as a fraction of TTL (0.2 = up to +20%).
	//
	// EN: One line that prevents cache avalanche. Without it, everything written
	//     during the same warm-up window expires in the same second and the store
	//     briefly sees full traffic as if no cache existed. With it, expiries are
	//     smeared across a window.
	// TR: Cache avalanche'ı önleyen tek satır. Olmazsa, aynı ısınma penceresinde
	//     yazılan her şey aynı saniyede expire olur ve depo kısa süreliğine cache
	//     hiç yokmuş gibi tam yükü görür. Olunca expire'lar bir pencereye yayılır.
	// [Topic · Konu: Cache avalanche]
	Jitter float64
}

func New[V any](o Options, met *metrics.Registry) *Cache[V] {
	if o.Capacity <= 0 {
		o.Capacity = 1024
	}
	if o.TTL <= 0 {
		o.TTL = time.Minute
	}
	if o.NegativeTTL <= 0 {
		o.NegativeTTL = 10 * time.Second
	}
	return &Cache[V]{
		ll:       list.New(),
		items:    make(map[string]*list.Element, o.Capacity),
		inflight: make(map[string]*call[V]),
		capacity: o.Capacity,
		ttl:      o.TTL,
		negTTL:   o.NegativeTTL,
		jitter:   o.Jitter,
		met:      met,
		now:      time.Now,
	}
}

// GetOrLoad returns a cached value, or loads it exactly once across all callers.
//
// EN: This is the stampede guard. When a hot key expires under load, the naive
//
//	cache lets *every* concurrent request run the same expensive load, the store
//	saturates, latency grows, which delays the winner that would refill the
//	cache — a self-feeding collapse (thundering herd). Here the first caller
//	loads and the rest wait on it.
//
//	The subtle part is what happens on *error*: the in-flight entry is cleared
//	and waiters get the error immediately rather than hanging until some TTL.
//	A guard that deadlocks when its dependency is sick has become the new
//	outage. The notes phrase it as: if you do not design the failure mode of
//	your protection, your protection becomes your failure mode.
//
// TR: Bu, stampede koruması. Sıcak bir anahtar yük altında expire olduğunda naif
//
//	cache *bütün* eşzamanlı isteklerin aynı pahalı yüklemeyi yapmasına izin
//	verir; depo doyar, gecikme büyür, gecikme büyüdükçe cache'i dolduracak
//	kazanan da gecikir — kendini besleyen bir çöküş (thundering herd). Burada
//	ilk çağıran yükler, kalanlar onu bekler.
//
//	İnce kısım *hata* durumunda ne olduğu: uçuştaki kayıt temizlenir ve
//	bekleyenler bir TTL boyunca asılı kalmak yerine hatayı hemen alır. Bağımlısı
//	hastalandığında kilitlenen bir koruma, yeni arızanın kendisi olmuştur.
//	Notlardaki hâli: korumanın arıza modunu tasarlamazsan, koruma senin arıza
//	modun olur.
//
// [Topic · Konu: Stampede / thundering herd]
func (c *Cache[V]) GetOrLoad(ctx context.Context, key string, load Loader[V]) (V, bool, error) {
	var zero V

	c.mu.Lock()
	if e, ok := c.lookupLocked(key); ok {
		val, found := e.val, e.found
		c.mu.Unlock()
		if found {
			c.met.Inc("cache_hit")
		} else {
			c.met.Inc("cache_negative_hit")
		}
		return val, found, nil
	}

	if cl, ok := c.inflight[key]; ok {
		c.mu.Unlock()
		// EN: Counted separately from hit and miss. A rising stampede_wait is not
		//     an error — it is proof the guard is doing its job. You cannot tell
		//     that from hit ratio alone.
		// TR: Hit ve miss'ten ayrı sayılıyor. Artan stampede_wait bir hata değil —
		//     korumanın işini yaptığının kanıtı. Bunu hit oranından anlayamazsın.
		c.met.Inc("cache_stampede_wait")
		cl.wg.Wait()
		return cl.val, cl.found, cl.err
	}

	cl := &call[V]{}
	cl.wg.Add(1)
	c.inflight[key] = cl
	c.mu.Unlock()

	c.met.Inc("cache_miss")
	cl.val, cl.found, cl.err = load(ctx)

	if cl.err == nil {
		c.set(key, cl.val, cl.found)
	} else {
		// EN: Errors are never cached. Caching a transient failure would turn a
		//     blip into a TTL-long outage for that key.
		// TR: Hatalar asla cache'lenmez. Geçici bir arızayı cache'lemek, o anahtar
		//     için bir anlık kesintiyi TTL boyu bir kesintiye çevirirdi.
		c.met.Inc("cache_load_error")
	}

	cl.wg.Done()

	c.mu.Lock()
	delete(c.inflight, key)
	c.mu.Unlock()

	if cl.err != nil {
		return zero, false, cl.err
	}
	return cl.val, cl.found, nil
}

// lookupLocked returns a live entry, evicting it if expired. Caller holds c.mu.
func (c *Cache[V]) lookupLocked(key string) (*entry[V], bool) {
	el, ok := c.items[key]
	if !ok {
		return nil, false
	}
	e := el.Value.(*entry[V])
	if c.now().After(e.expires) {
		c.removeLocked(el)
		c.met.Inc("cache_expired")
		return nil, false
	}
	c.ll.MoveToFront(el)
	return e, true
}

func (c *Cache[V]) set(key string, val V, found bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	base := c.ttl
	if !found {
		base = c.negTTL
	}
	exp := c.now().Add(c.withJitter(base))

	if el, ok := c.items[key]; ok {
		e := el.Value.(*entry[V])
		e.val, e.found, e.expires = val, found, exp
		c.ll.MoveToFront(el)
		return
	}

	el := c.ll.PushFront(&entry[V]{key: key, val: val, found: found, expires: exp})
	c.items[key] = el

	// EN: Bounded on purpose. An unbounded cache is not a cache, it is a memory
	//     leak with good intentions — and in Kubernetes it ends as OOMKilled.
	// TR: Bilinçli olarak sınırlı. Sınırsız bir cache, cache değildir; iyi niyetli
	//     bir bellek sızıntısıdır — ve Kubernetes'te sonu OOMKilled olur.
	for c.ll.Len() > c.capacity {
		c.removeLocked(c.ll.Back())
		c.met.Inc("cache_eviction")
	}
}

func (c *Cache[V]) removeLocked(el *list.Element) {
	if el == nil {
		return
	}
	c.ll.Remove(el)
	delete(c.items, el.Value.(*entry[V]).key)
}

func (c *Cache[V]) withJitter(d time.Duration) time.Duration {
	if c.jitter <= 0 {
		return d
	}
	return d + time.Duration(rand.Int63n(int64(float64(d)*c.jitter)+1))
}

// Invalidate drops a key immediately.
//
// EN: Explicit invalidation is what makes a write visible before its TTL. In a
//
//	single process this is one map delete. Across a fleet it is a *broadcast*
//	problem, and that is the whole point: the moment you keep a copy in each
//	pod's memory you have taken on a debt for an invalidation channel
//	(NATS pub/sub, a Redis Stream, whatever) — and if you do not build it, the
//	interest is paid as user-visible inconsistency.
//
// TR: Açık invalidation, bir yazmanın TTL'inden önce görünmesini sağlayan şeydir.
//
//	Tek süreçte bu bir map silme. Bir filoda ise bir *yayın* problemi — ve asıl
//	mesele bu: her pod'un belleğinde bir kopya tuttuğun an bir invalidation
//	kanalı borçlanmış olursun (NATS pub/sub, Redis Stream, her neyse). Kurmazsan
//	faizini kullanıcının gördüğü tutarsızlık olarak ödersin.
//
// [Topic · Konu: Invalidation bir yayın problemidir]
func (c *Cache[V]) Invalidate(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.removeLocked(el)
		c.met.Inc("cache_invalidate")
	}
}

// Len returns the current number of entries.
func (c *Cache[V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}
