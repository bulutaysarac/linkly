package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/bulutaysarac/linkly/internal/config"
	"github.com/bulutaysarac/linkly/internal/metrics"
	"github.com/bulutaysarac/linkly/internal/ratelimit"
	"github.com/bulutaysarac/linkly/internal/shortener"
)

type Server struct {
	svc        *shortener.Service
	cfg        config.Config
	met        *metrics.Registry
	log        *slog.Logger
	lim        *ratelimit.Limiter
	mux        *http.ServeMux
	middleware []Middleware
}

func NewServer(svc *shortener.Service, cfg config.Config, met *metrics.Registry, log *slog.Logger, lim *ratelimit.Limiter) *Server {
	s := &Server{svc: svc, cfg: cfg, met: met, log: log, lim: lim, mux: http.NewServeMux()}
	s.middleware = []Middleware{
		recoverer(met, log),
		requestID(),
		accessLog(log, met),
		timeout(cfg.RequestTimeout),
		withTenantIfPresent,
		rateLimit(lim),
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	// EN: Go 1.22+ pattern routing. Literal paths beat single-segment wildcards, so
	//     /healthz and /metrics are never swallowed by /{code}. The shortcode
	//     package ALSO refuses those names as custom aliases — two independent
	//     defences for one invariant, because relying on routing precedence alone
	//     would break the day someone adds a route.
	// TR: Go 1.22+ desen yönlendirmesi. Literal yollar tek-segment joker'ları
	//     yener, yani /healthz ve /metrics asla /{code} tarafından yutulmaz.
	//     shortcode paketi de bu isimleri takma ad olarak reddediyor — tek bir
	//     değişmez için iki bağımsız savunma; çünkü yalnızca yönlendirme önceliğine
	//     güvenmek, biri yeni bir rota eklediği gün kırılırdı.
	// [Topic · Konu: Katmanlı savunma]
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /readyz", s.handleReadyz)
	s.mux.HandleFunc("GET /metrics", s.handleMetrics)

	s.mux.Handle("POST /api/v1/links", requireTenant(http.HandlerFunc(s.handleCreate)))
	s.mux.Handle("GET /api/v1/links", requireTenant(http.HandlerFunc(s.handleList)))
	s.mux.Handle("GET /api/v1/links/{code}", requireTenant(http.HandlerFunc(s.handleGetOne)))
	s.mux.Handle("DELETE /api/v1/links/{code}", requireTenant(http.HandlerFunc(s.handleDelete)))

	s.mux.HandleFunc("GET /{$}", s.handleIndex)
	s.mux.HandleFunc("GET /{code}", s.handleRedirect)
}

func (s *Server) Handler() http.Handler { return chain(s.mux, s.middleware...) }

// HTTPServer builds the net/http server with the timeouts that matter.
//
// EN: These four timeouts are not boilerplate; each closes a different way a
//
//	connection can pin a resource forever:
//	  ReadHeaderTimeout — a client that opens a socket and dribbles headers
//	                      (Slowloris). Without this, N sockets = N stuck goroutines.
//	  ReadTimeout       — a body that never finishes arriving.
//	  WriteTimeout      — a client that stops reading your response.
//	  IdleTimeout       — keep-alive connections that outlive their usefulness.
//	Go's defaults are all "no timeout", which is the right default for a library
//	and the wrong one for a service on the internet.
//
// TR: Bu dört timeout dolgu malzemesi değil; her biri bir bağlantının kaynağı
//
//	sonsuza kadar tutmasının farklı bir yolunu kapatıyor:
//	  ReadHeaderTimeout — soket açıp header'ları damla damla gönderen istemci
//	                      (Slowloris). Bu olmadan N soket = N takılı goroutine.
//	  ReadTimeout       — gelmesi hiç bitmeyen bir gövde.
//	  WriteTimeout      — cevabını okumayı bırakan istemci.
//	  IdleTimeout       — faydasını yitirmiş keep-alive bağlantıları.
//	Go'nun varsayılanları "timeout yok" — bir kütüphane için doğru, internete
//	açık bir servis için yanlış varsayılan.
//
// [Topic · Konu: TCP bağlantı ömrü, kaynak tükenmesi]
func (s *Server) HTTPServer() *http.Server {
	return &http.Server{
		Addr:              s.cfg.Addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
