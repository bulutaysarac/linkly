// Command linkly is a URL shortener written as a study project for the
// System Design Primer's "Design Pastebin.com / Bit.ly" exercise.
//
// EN: This file is the wiring, and the most instructive part of it is the last
//
//	twenty lines: the shutdown order. Everything above is construction;
//	everything below Shutdown is where data gets lost if you get the order wrong.
//
// TR: Bu dosya bağlantı şeması ve en öğretici kısmı son yirmi satırı: kapatma
//
//	sırası. Yukarısı kurulum; Shutdown'ın altındaki kısım ise sırayı yanlış
//	yaparsan verinin kaybolduğu yer.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bulutaysarac/linkly/internal/analytics"
	"github.com/bulutaysarac/linkly/internal/cache"
	"github.com/bulutaysarac/linkly/internal/config"
	"github.com/bulutaysarac/linkly/internal/httpapi"
	"github.com/bulutaysarac/linkly/internal/metrics"
	"github.com/bulutaysarac/linkly/internal/ratelimit"
	"github.com/bulutaysarac/linkly/internal/shortener"
	"github.com/bulutaysarac/linkly/internal/store"
)

// version is injected at build time with -ldflags "-X main.version=...".
// TR: Build sırasında -ldflags ile enjekte edilir.
var version = "dev"

func main() {
	// EN: A tiny self-probe subcommand. The runtime image is distroless — no shell,
	//     no curl, no wget — which is exactly the point: a container with no tooling
	//     inside is a container an attacker cannot pivot from. But then HEALTHCHECK
	//     has nothing to call, so the binary probes itself. Idiomatic for distroless,
	//     and a nice illustration of least privilege: you do not ship a shell "just
	//     in case", you ship the one capability you actually need.
	// TR: Küçük bir kendini-yoklama alt komutu. Çalışma imajı distroless — ne shell,
	//     ne curl, ne wget — ki asıl mesele bu: içinde araç olmayan bir konteyner,
	//     saldırganın sıçrayamayacağı bir konteynerdir. Ama o zaman HEALTHCHECK'in
	//     çağıracağı bir şey kalmıyor, bu yüzden binary kendini yokluyor. Distroless
	//     için idiyomatik ve en az yetki ilkesinin güzel bir örneği: "ne olur ne
	//     olmaz" diye shell koymuyorsun, gerçekten ihtiyacın olan tek yeteneği
	//     koyuyorsun.
	// [Topic · Konu: En az yetki, saldırı yüzeyi]
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(selfCheck())
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg := config.Load()
	met := metrics.NewRegistry()

	// EN: Pre-register every counter at zero. See metrics/known.go for why this
	//     one line is the difference between an alertable metric and a blind spot.
	// TR: Her sayacı sıfırla önceden kaydet. Bu tek satırın neden alarm
	//     bağlanabilir bir metrikle bir kör nokta arasındaki fark olduğu için
	//     metrics/known.go'ya bak.
	met.Register(metrics.Known()...)

	// --- construction, outside in -------------------------------------------
	st := store.NewMemory(cfg.ShardCount)

	linkCache := cache.New[store.Link](cache.Options{
		Capacity:    cfg.CacheSize,
		TTL:         cfg.CacheTTL,
		NegativeTTL: cfg.CacheNegativeTTL,
		Jitter:      cfg.CacheJitter,
	}, met)

	sink := analytics.NewMemorySink()
	clicks := analytics.New(sink, analytics.Options{
		QueueSize:  cfg.AnalyticsQueue,
		BatchSize:  cfg.AnalyticsBatch,
		FlushEvery: cfg.AnalyticsFlushEvery,
	}, met)
	clicks.Run()

	limiter := ratelimit.New(cfg.RateLimitPerSec, cfg.RateLimitBurst, met)
	limiter.DryRun = cfg.RateLimitDryRun

	svc := shortener.New(st, linkCache, clicks, met, cfg.CodeLength, cfg.MaxAttempts)
	api := httpapi.NewServer(svc, cfg, met, log, limiter)
	srv := api.HTTPServer()

	// Housekeeping: expire idle rate-limit buckets so the map cannot grow forever.
	// TR: Bakım: boşta kalan rate-limit kovalarını sil ki map sonsuza kadar büyümesin.
	stopGC := make(chan struct{})
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if n := limiter.GC(10 * time.Minute); n > 0 {
					met.Add("ratelimit_buckets_gc", uint64(n))
				}
			case <-stopGC:
				return
			}
		}
	}()

	go func() {
		log.Info("linkly listening", "version", version, "addr", cfg.Addr, "base_url", cfg.BaseURL,
			"permanent_redirect", cfg.PermanentRedirect, "rate_dry_run", cfg.RateLimitDryRun)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server failed", "err", err)
			os.Exit(1)
		}
	}()

	// --- shutdown, and the order is the whole lesson -------------------------
	//
	// EN: SIGTERM is not "stop now", it is "stop accepting and finish what you have".
	//     The rule, which mature fleets codify explicitly:
	//         stop accepting → drain what you hold → close dependencies → close stores
	//     Each closer must run BEFORE the closer of the thing it depends on.
	//
	//     Concretely, here:
	//       1. srv.Shutdown  — stops accepting, waits for in-flight requests.
	//                          Those requests may STILL enqueue click events, which
	//                          is exactly why the collector cannot be stopped first.
	//       2. clicks.Shutdown — now that nothing can enqueue, drain the queue and
	//                          flush the final batch.
	//       3. st.Close      — the store is last, because both of the above may
	//                          still need it.
	//
	//     Get this backwards and nothing errors. No log line, no alarm — the events
	//     simply never happened. One audit of a large Go fleet found this exact ordering bug in
	//     21 services at once, which is the other lesson: in a fleet, one mistaken
	//     pattern is copied as many times as you have services.
	// TR: SIGTERM "hemen dur" değil, "kabul etmeyi kes ve elindekini bitir" demek.
	//     Kural, olgun filoların açıkça yazıya döktüğü hâliyle:
	//         kabul etmeyi kes → elindekini boşalt → bağımlılıkları kapat → depoları kapat
	//     Her closer, bağımlı olduğu şeyin closer'ından ÖNCE çalışmalı.
	//
	//     Somut olarak burada:
	//       1. srv.Shutdown   — kabul etmeyi keser, uçuştaki istekleri bekler.
	//                           O istekler HÂLÂ tıklama event'i kuyruğa koyabilir;
	//                           collector'ın önce durdurulamamasının sebebi tam bu.
	//       2. clicks.Shutdown— artık kimse kuyruğa koyamazken kuyruğu boşalt ve son
	//                           batch'i yaz.
	//       3. st.Close       — depo en sonda, çünkü yukarıdaki ikisi hâlâ ona
	//                           ihtiyaç duyabilir.
	//
	//     Bunu ters yaparsan hiçbir şey hata vermez. Ne log satırı ne alarm —
	//     event'ler sadece hiç olmamış olur. Büyük bir Go filosunda yapılan bir tarama tam bu sıra hatasını aynı
	//     anda 21 serviste buldu; diğer ders de bu: bir filoda yanlış bir kalıp,
	//     servis sayın kadar çoğalır.
	// [Topic · Konu: Graceful shutdown, drain sırası]
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	sig := <-sigCh
	log.Info("shutdown signal received, draining", "signal", sig.String(),
		"timeout", cfg.ShutdownTimeout.String())

	ctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	// 1. stop accepting, let in-flight requests finish
	if err := srv.Shutdown(ctx); err != nil {
		log.Error("http shutdown", "err", err)
	}
	close(stopGC)

	// 2. drain the async pipeline
	if err := clicks.Shutdown(ctx); err != nil {
		// EN: Draining is best-effort under a deadline. Waiting forever for a wedged
		//     sink would turn a clean rollout into a stuck one — so we bound it, and
		//     we say out loud that we may have lost the tail.
		// TR: Drain, süre sınırı altında elden gelenin en iyisi. Takılmış bir sink'i
		//     sonsuza kadar beklemek temiz bir deploy'u takılmış bir deploy'a
		//     çevirirdi — o yüzden sınırlıyoruz ve kuyruğu kaybetmiş olabileceğimizi
		//     açıkça söylüyoruz.
		log.Error("analytics drain did not finish in time", "err", err)
	}

	// 3. close stores last
	if err := st.Close(); err != nil {
		log.Error("store close", "err", err)
	}

	log.Info("stopped",
		"redirects", met.Get("redirect_ok"),
		"cache_hit", met.Get("cache_hit"),
		"cache_miss", met.Get("cache_miss"),
		"stampede_wait", met.Get("cache_stampede_wait"),
		"analytics_written", met.Get("analytics_written"),
		"analytics_dropped", met.Get("analytics_dropped"))
}

// selfCheck probes the local /healthz endpoint. Exit 0 = healthy.
func selfCheck() int {
	addr := os.Getenv("LINKLY_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	if addr[0] == ':' {
		addr = "127.0.0.1" + addr
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
