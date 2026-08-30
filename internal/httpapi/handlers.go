package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/bulutaysarac/linkly/internal/shortener"
	"github.com/bulutaysarac/linkly/internal/store"
)

type createRequest struct {
	URL   string `json:"url"`
	Alias string `json:"alias,omitempty"`
	TTL   string `json:"ttl,omitempty"` // Go duration string, e.g. "720h"
}

type linkResponse struct {
	Code      string     `json:"code"`
	ShortURL  string     `json:"short_url"`
	LongURL   string     `json:"long_url"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

func (s *Server) toResponse(l store.Link) linkResponse {
	r := linkResponse{
		Code:      l.Code,
		ShortURL:  s.svc.ShortURL(s.cfg.BaseURL, l),
		LongURL:   l.LongURL,
		CreatedAt: l.CreatedAt,
	}
	if !l.ExpiresAt.IsZero() {
		e := l.ExpiresAt
		r.ExpiresAt = &e
	}
	return r
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	// EN: Bound the body before decoding. Without MaxBytesReader a single request
	//     can make the process allocate until it is OOMKilled — a denial of service
	//     that needs no cleverness at all, just a big POST.
	// TR: Gövdeyi decode etmeden önce sınırla. MaxBytesReader olmadan tek bir istek
	//     süreci OOMKilled olana kadar bellek ayırtabilir — hiç zekâ gerektirmeyen,
	//     sadece büyük bir POST isteyen bir hizmet dışı bırakma.
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)

	var req createRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		// EN: 413, not 400, when the body blew the cap. The two say different
		//     things: 400 means "I could not parse what you sent", 413 means
		//     "what you sent is too big — send less". A client retrying after a
		//     400 will send the same oversized body again; after a 413 it knows
		//     to shrink it. Same discipline as 410-vs-404 on the redirect path:
		//     the accurate status is free, the vague one costs the client a
		//     wasted retry loop.
		// TR: Gövde sınırı aştığında 400 değil 413. İkisi farklı şey söylüyor:
		//     400 "gönderdiğini ayrıştıramadım", 413 "gönderdiğin çok büyük, daha
		//     az gönder" demek. 400 alan bir istemci aynı büyük gövdeyi tekrar
		//     yollar; 413 alan küçültmesi gerektiğini bilir. Yönlendirme
		//     yolundaki 410-vs-404 ile aynı disiplin: doğru durum bedava,
		//     belirsiz olanı istemciye boşa bir retry döngüsü olarak fatura ediliyor.
		// [Topic · Konu: Durum kodları sözleşmenin parçası]
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid json body")
		return
	}

	var ttl time.Duration
	if req.TTL != "" {
		d, err := time.ParseDuration(req.TTL)
		if err != nil || d <= 0 {
			writeError(w, http.StatusBadRequest, "invalid ttl")
			return
		}
		ttl = d
	}

	l, err := s.svc.Create(r.Context(), shortener.CreateRequest{
		TenantID: Tenant(r.Context()),
		LongURL:  req.URL,
		Alias:    req.Alias,
		TTL:      ttl,
	})
	switch {
	case err == nil:
		writeJSON(w, http.StatusCreated, s.toResponse(l))
	case errors.Is(err, shortener.ErrInvalidURL):
		writeError(w, http.StatusBadRequest, "invalid or unsafe url")
	case errors.Is(err, shortener.ErrInvalidAlias):
		writeError(w, http.StatusBadRequest, "invalid alias")
	case errors.Is(err, shortener.ErrAliasTaken):
		writeError(w, http.StatusConflict, "alias already taken")
	case errors.Is(err, shortener.ErrExhausted):
		// EN: 503, not 500. This says "try again", which is true: the next attempt
		//     draws different random codes. Status codes are part of the contract —
		//     a wrong one sends the client's retry logic the wrong way.
		// TR: 500 değil 503. Bu "tekrar dene" der ve doğrudur: sonraki deneme farklı
		//     rastgele kodlar çeker. Durum kodları sözleşmenin parçası — yanlış olanı
		//     istemcinin retry mantığını yanlış yöne yollar.
		writeError(w, http.StatusServiceUnavailable, "could not allocate a code, retry")
	default:
		s.log.Error("create failed", "request_id", RequestID(r.Context()), "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

// handleRedirect is the hot path: one cache lookup, one 302, one async event.
func (s *Server) handleRedirect(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")

	l, err := s.svc.Resolve(r.Context(), code)
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.met.Inc("redirect_not_found")
		writeError(w, http.StatusNotFound, "not found")
		return
	case errors.Is(err, shortener.ErrGone):
		// EN: 410 Gone, not 404. "It existed and is now over" is different
		//     information from "it never existed", and clients (and crawlers) treat
		//     them differently: 410 means stop asking. Choosing the accurate status
		//     is free; choosing the vague one costs you traffic forever.
		// TR: 404 değil 410 Gone. "Vardı ve bitti" ile "hiç olmadı" farklı bilgiler
		//     ve istemciler (ve tarayıcı botları) ikisine farklı davranır: 410
		//     "sormayı bırak" demek. Doğru durumu seçmek bedava; belirsizini seçmek
		//     sana sonsuza kadar trafik olarak geri döner.
		s.met.Inc("redirect_gone")
		writeError(w, http.StatusGone, "link expired")
		return
	case err != nil:
		s.log.Error("resolve failed", "request_id", RequestID(r.Context()), "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// EN: Fire-and-forget. Never before the redirect, never blocking it.
	// TR: Gönder-unut. Asla yönlendirmeden önce, asla onu bloklamadan.
	s.svc.RecordClick(l, r.Header.Get("Referer"))

	status := http.StatusFound // 302
	if s.cfg.PermanentRedirect {
		status = http.StatusMovedPermanently // 301
	}

	// EN: Explicitly uncacheable when we are on 302. A CDN or corporate proxy that
	//     decides to cache the redirect would erase the clicks just as effectively
	//     as a 301 — and you would not even know it was happening, because the
	//     requests simply stop arriving. "No data" and "no traffic" look identical
	//     on a dashboard.
	// TR: 302'deyken açıkça cache'lenemez. Yönlendirmeyi cache'lemeye karar veren
	//     bir CDN ya da kurumsal proxy, tıklamaları 301 kadar etkili biçimde siler —
	//     üstelik olduğunu fark bile etmezsin, çünkü istekler sadece gelmeyi bırakır.
	//     "Veri yok" ile "trafik yok" bir dashboard'da birebir aynı görünür.
	// [Topic · Konu: Cache nerede durur, kim kontrol eder]
	if status == http.StatusFound {
		w.Header().Set("Cache-Control", "private, no-store, max-age=0")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=86400")
	}

	s.met.Inc("redirect_ok")
	http.Redirect(w, r, l.LongURL, status)
}

func (s *Server) handleGetOne(w http.ResponseWriter, r *http.Request) {
	l, err := s.svc.GetOwned(r.Context(), Tenant(r.Context()), r.PathValue("code"))
	if err != nil {
		s.writeOwnedError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.toResponse(l))
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.Delete(r.Context(), Tenant(r.Context()), r.PathValue("code")); err != nil {
		s.writeOwnedError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeOwnedError maps store errors for tenant-scoped endpoints.
//
// EN: ErrForbidden is deliberately answered with 404, not 403. Returning 403 would
//
//	confirm "this code exists, it is just not yours" — which is exactly the
//	oracle an attacker needs to enumerate other tenants' links one guess at a
//	time. When the resource is not yours, the honest and the safe answer are the
//	same: as far as you are concerned, it does not exist.
//
// TR: ErrForbidden bilinçli olarak 403 değil 404 ile cevaplanıyor. 403 dönmek "bu
//
//	kod var, sadece senin değil" bilgisini onaylardı — ki saldırganın başka
//	kiracıların linklerini tek tek tarayabilmesi için gereken tam olarak bu
//	kâhin. Kaynak senin değilken dürüst cevapla güvenli cevap aynıdır: seni
//	ilgilendiren kadarıyla o şey yok.
//
// [Topic · Konu: Bilgi sızıntısı, IDOR]
func (s *Server) writeOwnedError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrForbidden):
		s.met.Inc("api_not_found_or_forbidden")
		writeError(w, http.StatusNotFound, "not found")
	default:
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	links, err := s.svc.List(r.Context(), Tenant(r.Context()), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]linkResponse, 0, len(links))
	for _, l := range links {
		out = append(out, s.toResponse(l))
	}
	writeJSON(w, http.StatusOK, map[string]any{"links": out, "count": len(out)})
}

// handleHealthz is liveness: "is this process alive?"
//
// EN: Liveness and readiness answer different questions and must not share an
//
//	implementation. Liveness failing means "restart me". Readiness failing means
//	"stop sending me traffic, but I am fine". Wiring a dependency check into
//	liveness is a classic self-inflicted outage: the database blips, every pod
//	reports unhealthy, the orchestrator restarts the entire fleet at once, and
//	now you have a cold cache on top of a sick database.
//
// TR: Liveness ve readiness farklı sorulara cevap verir ve aynı implementasyonu
//
//	paylaşmamalı. Liveness'ın düşmesi "beni yeniden başlat" demek. Readiness'ın
//	düşmesi "bana trafik gönderme ama ben iyiyim" demek. Bağımlılık kontrolünü
//	liveness'a bağlamak klasik bir kendi ayağına sıkma: veritabanı bir an
//	takılır, bütün pod'lar sağlıksız der, orkestratör tüm filoyu aynı anda
//	yeniden başlatır ve şimdi hasta bir veritabanının üstüne bir de soğuk cache
//	eklemiş olursun.
//
// [Topic · Konu: Soğuk başlangıç, erişilebilirlik]
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(s.met.Snapshot()))
}

func (s *Server) handleIndex(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"service": "linkly",
		"docs":    "POST /api/v1/links  ·  GET /{code}  ·  GET /metrics",
	})
}
