// Package httpapi is the transport layer: HTTP in, HTTP out, plus the
// cross-cutting middleware every route inherits.
//
// EN: The order of the chain is a design decision, not an accident. Reading from
//
//	the outside in, each layer only makes sense if the ones outside it already
//	ran:
//	    recover → requestID → accessLog → timeout → rateLimit → [tenant]
//	· recover is outermost so it can catch a panic from ANY layer, including
//	  the middleware below it.
//	· rateLimit sits as far out as it can while still knowing the tenant,
//	  because work rejected early costs nothing.
//	· tenant is innermost: only the management API needs it; the public
//	  redirect must work for a visitor who has no tenant at all.
//
// TR: Zincirin sırası bir tasarım kararı, tesadüf değil. Dıştan içe okunduğunda
//
//	her katman, ancak dışındakiler çalıştıysa anlamlı:
//	    recover → requestID → accessLog → timeout → rateLimit → [tenant]
//	· recover en dışta, çünkü HERHANGİ bir katmandan gelen panic'i yakalamalı —
//	  altındaki middleware'ler dahil.
//	· rateLimit, kiracıyı bilebileceği kadar dışta duruyor; erken reddedilen iş
//	  hiçbir şeye mal olmaz.
//	· tenant en içte: yalnızca yönetim API'sinin ihtiyacı var; açık yönlendirme,
//	  hiç kiracısı olmayan bir ziyaretçi için de çalışmak zorunda.
//
// [Topic · Konu: Endpoint middleware katmanı]
package httpapi

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/bulutaysarac/linkly/internal/metrics"
	"github.com/bulutaysarac/linkly/internal/ratelimit"
)

type ctxKey int

const (
	ctxKeyRequestID ctxKey = iota
	ctxKeyTenant
)

// TenantHeader is where the caller declares which tenant it is acting for.
//
// EN: READ THIS BEFORE COPYING ANYTHING FROM THIS FILE.
//
//	A plain header is NOT authentication. Anyone can send
//	`X-Tenant-ID: someone-else` with curl. It is used here so the tenant-boundary
//	logic — which is the part worth studying — can be demonstrated and tested
//	without dragging an identity provider into a teaching project.
//
//	In production the tenant must be *derived* from something the client cannot
//	forge: a validated JWT/session, or injected by a trusted gateway that
//	already authenticated the user. The pattern to copy is the reverse-proxy one
//	I have seen work well: a single entry point runs session validation, 2FA and route
//	authorisation, then injects the identity and the authorisation scope as
//	headers for the service behind it. The service trusts those headers only
//	because nothing else can reach it.
//
// TR: BU DOSYADAN BİR ŞEY KOPYALAMADAN ÖNCE BUNU OKU.
//
//	Düz bir header kimlik doğrulama DEĞİLDİR. Herkes curl ile
//	`X-Tenant-ID: baskasi` gönderebilir. Burada bulunma sebebi, asıl çalışılmaya
//	değer kısım olan kiracı sınırı mantığının, öğretim amaçlı bir projeye kimlik
//	sağlayıcı sokmadan gösterilebilmesi ve test edilebilmesi.
//
//	Üretimde kiracı, istemcinin taklit edemeyeceği bir şeyden *türetilmeli*:
//	doğrulanmış bir JWT/oturum ya da kullanıcıyı zaten doğrulamış güvenilir bir
//	geçidin enjekte etmesi. Kopyalanacak kalıp klasik reverse-proxy kalıbı:
//	tek bir giriş noktası oturum doğrulama, 2FA ve rota yetkilendirmesini
//	çalıştırır, sonra kimliği ve yetki kapsamını arkadaki servise header olarak
//	enjekte eder. Servis o header'lara yalnızca başka hiçbir şey ona
//	ulaşamadığı için güvenir.
//
// [Topic · Konu: Kimlik doğrulama ≠ yetkilendirme; proxy'nin asıl işi]
const TenantHeader = "X-Tenant-ID"

type Middleware func(http.Handler) http.Handler

func chain(h http.Handler, mw ...Middleware) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}

// recoverer turns a panic into a 500 and a counter instead of a dropped request.
//
// EN: Go's http server already survives a handler panic, but it does so by killing
//
//	the connection — the client sees a broken pipe, you see nothing useful.
//	Catching it here buys three things: a proper status code, a log line with the
//	request id, and a counter you can alarm on. The same rule applies to every
//	goroutine you spawn: an unrecovered panic in a background worker takes the
//	whole process down, which is how one bad message becomes an outage.
//
// TR: Go'nun http sunucusu handler panic'inden zaten sağ çıkar ama bunu bağlantıyı
//
//	öldürerek yapar — istemci kırık boru görür, sen işe yarar hiçbir şey görmezsin.
//	Burada yakalamak üç şey kazandırır: düzgün bir durum kodu, istek kimliğiyle
//	bir log satırı ve alarm bağlayabileceğin bir sayaç. Aynı kural açtığın her
//	goroutine için geçerli: arka plan worker'ında yakalanmamış bir panic bütün
//	süreci düşürür — bozuk tek bir mesajın kesintiye dönüşme yolu budur.
//
// [Topic · Konu: ]
func recoverer(met *metrics.Registry, log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					met.Inc("http_panic")
					log.Error("panic recovered",
						"request_id", RequestID(r.Context()),
						"path", r.URL.Path,
						"panic", rec)
					writeError(w, http.StatusInternalServerError, "internal error")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// requestID attaches a correlation id to the context and the response.
//
// EN: Cheap, and it is the only thing that makes an async system debuggable. The
//
//	moment work crosses a queue boundary you lose the call stack; a correlation
//	id carried on the event is what lets you stitch "request arrived" back to
//	"work finished" minutes later. Asynchronism hides failures — observability
//	is what un-hides them.
//
// TR: Ucuz ve asenkron bir sistemi hata ayıklanabilir kılan tek şey. İş bir kuyruk
//
//	sınırını geçtiği an çağrı yığınını kaybedersin; event üzerinde taşınan bir
//	korelasyon kimliği, "istek geldi" ile dakikalar sonraki "iş bitti"yi birbirine
//	dikmeni sağlar. Asenkronizm arızayı gizler; gözlemlenebilirlik onu geri
//	görünür yapar.
//
// [Topic · Konu: Trace sürekliliği]
func requestID() Middleware {
	var counter uint64
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get("X-Request-ID")
			if id == "" {
				counter++
				id = strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + strconv.FormatUint(counter, 36)
			}
			w.Header().Set("X-Request-ID", id)
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeyRequestID, id)))
		})
	}
}

func RequestID(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyRequestID).(string); ok {
		return v
	}
	return ""
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func accessLog(log *slog.Logger, met *metrics.Registry) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w, status: 200}
			next.ServeHTTP(sw, r)

			met.Inc("http_requests_total")
			met.Inc("http_status_" + strconv.Itoa(sw.status/100) + "xx")

			// EN: Deliberately NOT logging the full query string, the Referer or the
			//     client IP. Logs are copied to more places and protected less than
			//     databases, so they are the easiest way to leak personal data by
			//     accident. Under KVKK/GDPR "we only put it in the logs" is not a
			//     defence. Log what you need to debug, not everything you have.
			// TR: Tam sorgu dizesi, Referer ve istemci IP'si bilinçli olarak
			//     loglanmıyor. Log'lar veritabanlarından daha çok yere kopyalanır ve
			//     daha az korunur; bu yüzden kişisel veriyi kazara sızdırmanın en
			//     kolay yolu. KVKK/GDPR açısından "sadece log'a yazdık" bir savunma
			//     değil. Hata ayıklamak için gerekeni logla, elindeki her şeyi değil.
			// [Topic · Konu: Kişisel veri]
			log.Info("http",
				"request_id", RequestID(r.Context()),
				"method", r.Method,
				"path", r.URL.Path,
				"status", sw.status,
				"dur_ms", time.Since(start).Milliseconds())
		})
	}
}

// timeout bounds every request.
//
// EN: Two mechanisms, and you need both:
//
//	  · http.TimeoutHandler replies 503 to the CLIENT after d
//	  · the context deadline tells the WORK to stop
//	Without the second one you free the client but keep burning CPU and database
//	connections on a result nobody will read. That is precisely how a slow query
//	turns into pool exhaustion: the front gives up, the back does not.
//
// TR: İki mekanizma var ve ikisi de gerekli:
//
//	  · http.TimeoutHandler, d sonunda İSTEMCİYE 503 döner
//	  · context deadline, İŞE durmasını söyler
//	İkincisi olmazsa istemciyi serbest bırakırsın ama kimsenin okumayacağı bir
//	sonuç için CPU ve veritabanı bağlantısı yakmaya devam edersin. Yavaş bir
//	sorgunun havuz tükenmesine dönüşme biçimi tam olarak budur: ön taraf
//	vazgeçer, arka taraf vazgeçmez.
//
// [Topic · Konu: Timeout zinciri, connection pool doygunluğu]
func timeout(d time.Duration) Middleware {
	return func(next http.Handler) http.Handler {
		withCtx := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
		return http.TimeoutHandler(withCtx, d+50*time.Millisecond, `{"error":"timeout"}`)
	}
}

// rateLimit applies the token bucket, keyed per tenant (or per IP when anonymous).
//
// EN: The key is the whole design. A single global limit would let one noisy
//
//	tenant consume everyone's budget — the noisy-neighbour failure mode of every
//	multi-tenant system. Prefixing the bucket key with the tenant keeps the limit
//	*inside* the tenant, so one partner overflowing cannot punish another.
//	Note that the isolation boundary shows up here for the third time in this
//	codebase: in the store key, in the cache key, and now in the limiter key.
//	Tenant identity is not a field — it is a context that has to be carried
//	through every layer, and dropping it in any one of them silently removes the
//	isolation from that layer downwards.
//
// TR: Anahtar tasarımın kendisi. Tek bir global limit, gürültülü tek bir kiracının
//
//	herkesin bütçesini yemesine izin verirdi — her çok kiracılı sistemin
//	noisy-neighbour arıza modu. Kova anahtarını kiracıyla ön eklemek limiti
//	kiracının *içinde* tutar; bir partnerin taşması diğerini cezalandıramaz.
//	İzolasyon sınırının bu kod tabanında üçüncü kez ortaya çıktığına dikkat:
//	depo anahtarında, cache anahtarında, şimdi de limiter anahtarında. Kiracı
//	kimliği bir alan değil — her katmanda taşınması gereken bir bağlam; herhangi
//	birinde düşürmek, o katmandan aşağısını sessizce izolasyonsuz bırakır.
//
// [Topic · Konu: Kiracı izolasyonu her katmanda]
func rateLimit(l *ratelimit.Limiter) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := "tenant:" + Tenant(r.Context())
			if Tenant(r.Context()) == "" {
				key = "ip:" + clientIP(r)
			}
			ok, retryAfter := l.Allow(key)
			if !ok {
				// EN: 429 plus Retry-After. Telling the client when to come back is
				//     what turns a rejection into a protocol instead of an invitation
				//     to retry immediately, all at once, forever.
				// TR: 429 ve üstüne Retry-After. İstemciye ne zaman döneceğini
				//     söylemek, reddi bir protokole çevirir — hemen, hep birlikte ve
				//     sonsuza kadar tekrar denemeye davet olmaktan çıkarır.
				w.Header().Set("Retry-After", strconv.Itoa(max(1, int(retryAfter.Seconds()+0.999))))
				writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// requireTenant rejects management calls that do not declare a tenant.
func requireTenant(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t := r.Header.Get(TenantHeader)
		if t == "" {
			writeError(w, http.StatusUnauthorized, "missing "+TenantHeader)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeyTenant, t)))
	})
}

// withTenantIfPresent parses the tenant without requiring it (for rate limiting).
func withTenantIfPresent(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if t := r.Header.Get(TenantHeader); t != "" {
			r = r.WithContext(context.WithValue(r.Context(), ctxKeyTenant, t))
		}
		next.ServeHTTP(w, r)
	})
}

func Tenant(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyTenant).(string); ok {
		return v
	}
	return ""
}

func clientIP(r *http.Request) string {
	// EN: X-Forwarded-For is trustworthy only when EVERY hop in front of you is
	//     yours and strips what the client sent. Otherwise a caller simply sets the
	//     header and gets a fresh rate-limit bucket per fake IP — the limiter looks
	//     like it is working while enforcing nothing.
	// TR: X-Forwarded-For'a yalnızca önündeki HER atlama seninse ve istemcinin
	//     gönderdiğini temizliyorsa güvenilir. Aksi hâlde çağıran header'ı kendisi
	//     ayarlar ve her sahte IP için taze bir rate-limit kovası alır — limiter
	//     çalışıyor görünür ama hiçbir şey zorlamaz.
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := len(xff); i > 0 {
			for j := 0; j < len(xff); j++ {
				if xff[j] == ',' {
					return xff[:j]
				}
			}
			return xff
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
