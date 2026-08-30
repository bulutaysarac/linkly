// Package shortener holds the business logic, with no knowledge of HTTP.
//
// EN: Three layers, borrowed from the Go-kit shape large Go microservice fleets follow:
//
//	    transport (httpapi)  →  endpoint/middleware (httpapi)  →  service (here)
//	The point is not tidiness. Cross-cutting protections — rate limiting,
//	timeouts, panic recovery — live in the middleware layer, so every handler
//	gets them for free and no one can forget to add them. A protection that
//	depends on each developer remembering it is not a protection; it is a
//	statistic waiting to happen.
//
// TR: Üç katman; büyük Go mikroservis filolarının izlediği Go-kit şablonundan ödünç:
//
//	    transport (httpapi) → endpoint/middleware (httpapi) → service (burası)
//	Amaç düzen değil. Enine kesen korumalar — rate limiting, timeout, panic
//	yakalama — middleware katmanında yaşıyor; böylece her handler bunları
//	bedavaya alıyor ve kimse eklemeyi unutamıyor. Her geliştiricinin
//	hatırlamasına bağlı bir koruma, koruma değildir; olmayı bekleyen bir
//	istatistiktir.
//
// [Topic · Konu: Uygulama katmanı, şablonla zorlanan koruma]
package shortener

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/bulutaysarac/linkly/internal/analytics"
	"github.com/bulutaysarac/linkly/internal/cache"
	"github.com/bulutaysarac/linkly/internal/metrics"
	"github.com/bulutaysarac/linkly/internal/shortcode"
	"github.com/bulutaysarac/linkly/internal/store"
)

var (
	ErrInvalidURL   = errors.New("shortener: invalid or unsafe target url")
	ErrAliasTaken   = errors.New("shortener: alias already taken")
	ErrExhausted    = errors.New("shortener: could not allocate a free code")
	ErrGone         = errors.New("shortener: link expired")
	ErrInvalidAlias = errors.New("shortener: invalid alias")
)

type Service struct {
	store  store.Store
	cache  *cache.Cache[store.Link]
	clicks *analytics.Collector
	met    *metrics.Registry

	codeLength  int
	maxAttempts int
	now         func() time.Time
}

func New(st store.Store, c *cache.Cache[store.Link], cl *analytics.Collector, met *metrics.Registry, codeLength, maxAttempts int) *Service {
	return &Service{
		store: st, cache: c, clicks: cl, met: met,
		codeLength: codeLength, maxAttempts: maxAttempts,
		now: time.Now,
	}
}

type CreateRequest struct {
	TenantID string
	LongURL  string
	Alias    string        // optional custom code
	TTL      time.Duration // optional; zero means never expires
}

// Create allocates a short code for a URL.
func (s *Service) Create(ctx context.Context, req CreateRequest) (store.Link, error) {
	if err := ValidateTargetURL(req.LongURL); err != nil {
		return store.Link{}, err
	}

	l := store.Link{
		LongURL:   req.LongURL,
		TenantID:  req.TenantID,
		CreatedAt: s.now(),
	}
	if req.TTL > 0 {
		l.ExpiresAt = s.now().Add(req.TTL)
	}

	// --- custom alias path ------------------------------------------------
	if req.Alias != "" {
		if err := shortcode.Validate(req.Alias); err != nil {
			return store.Link{}, ErrInvalidAlias
		}
		l.Code = req.Alias
		if err := s.store.CreateUnique(ctx, l); err != nil {
			if errors.Is(err, store.ErrCodeTaken) {
				// EN: A taken alias is a normal outcome, not an error condition.
				//     Note we do NOT say who owns it — that would let anyone probe
				//     which aliases exist across all tenants.
				// TR: Alınmış bir takma ad normal bir sonuç, hata durumu değil.
				//     Kimin sahibi olduğunu SÖYLEMİYORUZ — söyleseydik herkes bütün
				//     kiracılardaki takma adları yoklayabilirdi.
				return store.Link{}, ErrAliasTaken
			}
			return store.Link{}, err
		}
		s.afterCreate(l)
		return l, nil
	}

	// --- generated code path ----------------------------------------------
	//
	// EN: Generate, try to insert, retry on collision. Two things worth noticing:
	//
	//     1. The retry bound. With 62^7 keys and a small corpus, a collision is
	//        already improbable; a *second* collision on a fresh random code is
	//        improbable squared. So if we burn through maxAttempts, the honest
	//        conclusion is not "unlucky" — it is "the keyspace is filling up, or
	//        something is broken". Failing loudly at that point is the signal.
	//        An unbounded retry loop would hide exactly that signal, and would also
	//        be a way to spend a whole request budget on one lookup.
	//
	//     2. Collision detection is the store's job, not a pre-flight "does it
	//        exist?" check here. Read-then-write has a race window; a conditional
	//        insert does not.
	// TR: Üret, eklemeyi dene, çakışırsa tekrar dene. Dikkat edilecek iki şey:
	//
	//     1. Retry sınırı. 62^7 anahtar ve küçük bir veri kümesiyle çakışma zaten
	//        düşük ihtimalli; taze bir rastgele kodda *ikinci* bir çakışma bunun
	//        karesi. Yani maxAttempts'i tüketirsek dürüst sonuç "şanssızlık" değil,
	//        "anahtar uzayı doluyor ya da bir şey bozuk". O noktada gürültüyle
	//        başarısız olmak sinyalin kendisi. Sınırsız bir retry döngüsü tam da bu
	//        sinyali gizler — üstelik tek bir kayıt için bütün istek bütçesini
	//        harcamanın da yolu olur.
	//
	//     2. Çakışma tespiti deponun işi; burada "var mı?" diye ön kontrol değil.
	//        Oku-sonra-yaz'da yarış penceresi var, koşullu insert'te yok.
	// [Topic · Konu: Anahtar tasarımı, atomiklik, retry sınırı]
	for attempt := 0; attempt < s.maxAttempts; attempt++ {
		code, err := shortcode.Random(s.codeLength)
		if err != nil {
			return store.Link{}, err
		}
		l.Code = code

		err = s.store.CreateUnique(ctx, l)
		switch {
		case err == nil:
			s.met.Inc("create_ok")
			s.afterCreate(l)
			return l, nil
		case errors.Is(err, store.ErrCodeTaken):
			s.met.Inc("create_collision")
			continue
		default:
			return store.Link{}, err
		}
	}

	s.met.Inc("create_exhausted")
	return store.Link{}, ErrExhausted
}

// afterCreate invalidates any cached "does not exist" answer for the new code.
//
// EN: Easy to miss, and it bites in exactly one scenario: someone requested this
//
//	code BEFORE it existed (a probe, a typo, a link shared before creation).
//	That request cached a negative entry. If we do not clear it here, the newly
//	created link 404s until the negative TTL expires — a broken link that fixes
//	itself later, which is the worst kind of bug report to receive.
//
//	This is why negative TTL is kept short: it bounds the blast radius of
//	exactly this race. And it is a miniature of the invalidation problem —
//	with more than one pod, this local delete would have to become a broadcast.
//
// TR: Gözden kaçması kolay ve tam olarak tek bir senaryoda ısırıyor: biri bu kodu
//
//	var olmadan ÖNCE istemiş (bir yoklama, bir yazım hatası, oluşturulmadan
//	paylaşılmış bir link). O istek negatif bir kayıt cache'ledi. Burada
//	temizlemezsek yeni oluşturulan link, negatif TTL sönene kadar 404 verir —
//	sonradan kendi kendini düzelten bozuk bir link, ki alınabilecek en kötü hata
//	bildirimi budur.
//
//	Negatif TTL'in kısa tutulmasının sebebi bu: tam olarak bu yarışın etki
//	alanını sınırlıyor. Ve invalidation probleminin minyatürü — birden çok pod
//	olsaydı bu yerel silme bir yayına dönüşmek zorunda kalırdı.
//
// [Topic · Konu: Negatif cache + invalidation]
func (s *Service) afterCreate(l store.Link) {
	s.cache.Invalidate(l.Code)
}

// Resolve returns the target URL for a code, through the cache.
//
// EN: The read path, and the reason a cache exists in this project at all.
//
//	A shortener's traffic is roughly 1 write to 100–1000 reads, and the mapping
//	is effectively immutable. That profile — huge read amplification over stable
//	data — is the textbook case for cache-aside.
//
//	Note that expiry is evaluated AFTER the cache returns. The cache TTL and the
//	link TTL are two different clocks and must not be conflated: the cache TTL
//	says "how stale may my copy be", the link TTL says "when does this link stop
//	being valid". Mixing them means either serving dead links or evicting live
//	ones for no reason.
//
// TR: Okuma yolu ve bu projede cache'in var olma sebebi. Bir kısaltıcının trafiği
//
//	kabaca 1 yazmaya 100–1000 okuma ve eşleme fiilen değişmez. Bu profil —
//	kararlı veri üzerinde devasa okuma çarpımı — cache-aside'ın ders kitabı
//	vakası.
//
//	Son kullanma kontrolünün cache DÖNDÜKTEN SONRA yapıldığına dikkat. Cache
//	TTL'i ile link TTL'i iki ayrı saat ve karıştırılmamalı: cache TTL'i
//	"kopyam ne kadar bayat olabilir" der, link TTL'i "bu link ne zaman geçersiz
//	olur" der. Karıştırmak ya ölü link servis etmek ya da canlı olanı sebepsiz
//	tahliye etmek demektir.
//
// [Topic · Konu: Cache-aside, TTL ≠ TTL]
func (s *Service) Resolve(ctx context.Context, code string) (store.Link, error) {
	l, found, err := s.cache.GetOrLoad(ctx, code, func(ctx context.Context) (store.Link, bool, error) {
		got, err := s.store.Get(ctx, code)
		if errors.Is(err, store.ErrNotFound) {
			// EN: "not found" is returned as (zero, false, nil) — a real answer the
			//     cache is allowed to remember, NOT an error. This is the
			//     empty-vs-absent distinction made structural.
			// TR: "bulunamadı" (sıfır, false, nil) olarak dönüyor — cache'in
			//     hatırlamasına izin verilen gerçek bir cevap, hata DEĞİL.
			//     "boş" ile "yok" ayrımının yapısal hâli bu.
			return store.Link{}, false, nil
		}
		if err != nil {
			return store.Link{}, false, err
		}
		return got, true, nil
	})
	if err != nil {
		return store.Link{}, err
	}
	if !found {
		return store.Link{}, store.ErrNotFound
	}
	if l.Expired(s.now()) {
		return store.Link{}, ErrGone
	}
	return l, nil
}

// RecordClick hands the click to the async collector and returns immediately.
//
// EN: One line, and it is the whole point of the asynchronism chapter: the user's
//
//	redirect does not wait for analytics, and cannot be failed by them.
//
// TR: Tek satır ve asenkronizm bölümünün tamamı: kullanıcının yönlendirmesi
//
//	analitiği beklemiyor ve analitik yüzünden başarısız olamıyor.
//
// [Topic · Konu: Pahalı işi istek yolundan çıkarmak]
func (s *Service) RecordClick(l store.Link, referer string) {
	s.clicks.Record(analytics.Event{
		Code: l.Code, TenantID: l.TenantID, At: s.now(), Referer: referer,
	})
}

func (s *Service) GetOwned(ctx context.Context, tenantID, code string) (store.Link, error) {
	return s.store.GetOwned(ctx, tenantID, code)
}

func (s *Service) List(ctx context.Context, tenantID string, limit int) ([]store.Link, error) {
	return s.store.ListByTenant(ctx, tenantID, limit)
}

// Delete removes a link and drops it from cache.
//
// EN: Order matters: delete from the store first, then invalidate the cache.
//
//	The reverse order leaves a window in which a concurrent read repopulates the
//	cache from a store that has not been updated yet — and the stale entry then
//	survives for a full TTL. This is the classic dual-write hazard:
//	atomicity ends at the boundary of a single system. There is no
//	transaction spanning store and cache, only an ordering that makes the window
//	as small and as harmless as possible.
//
// TR: Sıra önemli: önce depodan sil, sonra cache'i düşür. Ters sıra, eşzamanlı bir
//
//	okumanın cache'i henüz güncellenmemiş bir depodan yeniden doldurduğu bir
//	pencere bırakır — ve bayat kayıt bir TTL boyu yaşar. Bu, klasik dual-write
//	tehlikesi: atomiklik tek bir sistemin sınırında biter.
//	tehlikesi: depo ile cache'i kapsayan bir transaction yok, yalnızca
//	pencereyi olabildiğince küçük ve zararsız yapan bir sıra var.
//
// [Topic · Konu: Dual-write, atomiklik sınırı]
func (s *Service) Delete(ctx context.Context, tenantID, code string) error {
	if err := s.store.Delete(ctx, tenantID, code); err != nil {
		return err
	}
	s.cache.Invalidate(code)
	return nil
}

// ValidateTargetURL rejects targets we refuse to shorten.
//
// EN: A URL shortener is, by construction, a machine for laundering the identity
//
//	of a link: it takes something a human could inspect and turns it into seven
//	opaque characters. Two concrete abuses follow from that, and neither needs
//	any exotic attack:
//	  1. Phishing — the short domain borrows your reputation for someone else's
//	     payload. Scheme allow-listing (http/https only) at least kills
//	     javascript:, data: and file: targets, which would otherwise execute in
//	     the visitor's context.
//	  2. Pointing visitors at addresses that are only meaningful *inside* a
//	     network — localhost, RFC1918 ranges, and above all 169.254.169.254,
//	     the cloud instance metadata endpoint.
//
//	Be precise about the threat model: linkly never fetches the target itself,
//	so this is not classic SSRF. The risk is that a trusted-looking short link
//	drives a *browser* — possibly one inside a corporate network — at an
//	internal address. And be equally precise about the limits: a hostname check
//	is a reduction of risk, not an elimination. It does not stop DNS names that
//	resolve to private IPs, decimal or hex encodings of addresses, or redirect
//	chains. A production system pairs this with resolve-time checks and a
//	reputation feed. Written down here because knowing what your control does
//	NOT cover is worth as much as the control.
//
// TR: Bir link kısaltıcı, yapısı gereği bir linkin kimliğini aklayan bir makinedir:
//
//	insanın inceleyebileceği bir şeyi alıp yedi opak karaktere çevirir. Bundan iki
//	somut kötüye kullanım doğar ve ikisi de egzotik bir saldırı gerektirmez:
//	  1. Oltalama — kısa alan adı, başkasının içeriği için senin itibarını ödünç
//	     verir. Şema izin listesi (yalnızca http/https) en azından javascript:,
//	     data: ve file: hedeflerini öldürür; bunlar aksi hâlde ziyaretçinin
//	     bağlamında çalışırdı.
//	  2. Ziyaretçiyi yalnızca bir ağın *içinde* anlamlı olan adreslere yöneltmek —
//	     localhost, RFC1918 aralıkları ve hepsinden önemlisi 169.254.169.254,
//	     yani bulut örnek metadata ucu.
//
//	Tehdit modeli konusunda net olalım: linkly hedefi kendisi hiç çekmiyor, yani
//	bu klasik SSRF değil. Risk, güvenilir görünen kısa bir linkin — muhtemelen bir
//	kurumsal ağın içindeki — bir *tarayıcıyı* iç bir adrese sürmesi. Sınırlar
//	konusunda da aynı ölçüde net olalım: hostname kontrolü riski azaltır, yok
//	etmez. Özel IP'lere çözülen DNS isimlerini, adreslerin ondalık/onaltılık
//	kodlanmış hâllerini ya da yönlendirme zincirlerini durdurmaz. Üretim sistemi
//	bunu çözümleme anındaki kontroller ve bir itibar beslemesiyle birleştirir.
//	Buraya yazılma sebebi: kontrolünün neyi kapsamadığını bilmek, kontrolün
//	kendisi kadar değerlidir.
//
// [Topic · Konu: Girdiye güvenme, katmanlı savunma, kontrolün sınırı]
func ValidateTargetURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 2048 {
		return ErrInvalidURL
	}

	u, err := url.Parse(raw)
	if err != nil {
		return ErrInvalidURL
	}

	// Allow-list, not deny-list. A deny-list of "bad schemes" is a list you will
	// always be one entry behind on.
	// TR: İzin listesi, yasak listesi değil. "Kötü şemalar" listesi, her zaman bir
	// madde geriden geleceğin bir listedir.
	if u.Scheme != "http" && u.Scheme != "https" {
		return ErrInvalidURL
	}

	host := strings.ToLower(u.Hostname())
	if host == "" {
		return ErrInvalidURL
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".internal") {
		return ErrInvalidURL
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
			return ErrInvalidURL
		}
	}
	return nil
}

// ShortURL renders the public form of a link.
func (s *Service) ShortURL(base string, l store.Link) string {
	return fmt.Sprintf("%s/%s", strings.TrimRight(base, "/"), l.Code)
}
