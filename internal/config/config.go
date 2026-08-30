// Package config loads settings from the environment.
//
// EN: Every tunable here was a hard-coded constant in the first draft. They were
//
//	pulled out for one reason: a TTL, a queue size or a rate limit is a
//	*contract* with the rest of the system, and contracts must be readable and
//	changeable without a deploy. Here is what happens otherwise —
//	a 30-day TTL written as time.Hour*24*30, which detonated exactly 30 days
//	after go-live, on a day nobody had changed anything.
//
// TR: Buradaki her ayar ilk taslakta koda gömülü bir sabitti. Tek bir sebeple
//
//	dışarı alındı: bir TTL, bir kuyruk boyutu ya da bir rate limit, sistemin geri
//	kalanıyla yapılmış bir *sözleşmedir*; sözleşmeler okunabilir ve deploy
//	gerektirmeden değiştirilebilir olmalı. Aksi hâlde şu olur —
//	time.Hour*24*30 diye yazılmış 30 günlük bir TTL, yayına alındıktan tam 30 gün
//	sonra, kimsenin hiçbir şey değiştirmediği bir günde patladı.
//
// [Topic · Konu: TTL bir sözleşmedir]
package config

import (
	"os"
	"strconv"
	"time"
)

type Config struct {
	Addr    string
	BaseURL string

	ShardCount  int
	CodeLength  int
	MaxAttempts int

	CacheSize        int
	CacheTTL         time.Duration
	CacheNegativeTTL time.Duration
	CacheJitter      float64

	RateLimitPerSec float64
	RateLimitBurst  float64
	RateLimitDryRun bool

	AnalyticsQueue      int
	AnalyticsBatch      int
	AnalyticsFlushEvery time.Duration

	// RequestTimeout must be smaller than the reverse proxy / LB timeout in front
	// of this service, and larger than any single downstream call it makes.
	//
	// EN: Timeouts have to shrink from the outside in. If the edge gives up before
	//     the app does, the user sees a 504 while the app keeps burning resources on
	//     work nobody is waiting for any more. That is exactly the shape of the
	//     a 504 I once traced: the root cause was a query losing its index, but what the
	//     user met was a gateway that had stopped waiting.
	// TR: Timeout'lar dıştan içe küçülmeli. Kenar, uygulamadan önce vazgeçerse
	//     kullanıcı 504 görür ama uygulama artık kimsenin beklemediği bir iş için
	//     kaynak yakmaya devam eder. İzini sürdüğüm bir 504'ün şekli tam olarak buydu:
	//     kök sebep index'ini kaybeden bir sorguydu ama kullanıcının karşılaştığı
	//     şey beklemeyi bırakmış bir geçitti.
	// [Topic · Konu: Timeout bir sözleşmedir]
	RequestTimeout  time.Duration
	ShutdownTimeout time.Duration

	// PermanentRedirect switches 302 → 301.
	//
	// EN: The most interesting one-line trade-off in any URL shortener.
	//       301 Moved Permanently — browsers and proxies cache it, often for a very
	//         long time. Fastest possible redirect, near-zero load on you...
	//         and you stop seeing the clicks. Analytics go dark, and you can never
	//         change or revoke that link for anyone who already visited it.
	//       302 Found — not cached by default. Every click comes to you: you keep
	//         the analytics, you keep the ability to change or kill the target,
	//         and you pay for the traffic.
	//     Defaults to 302 because for a shortener the click data and the ability to
	//     revoke ARE the product. This is a caching decision disguised as an HTTP
	//     status code.
	// TR: Herhangi bir link kısaltıcıdaki en ilginç tek satırlık takas.
	//       301 Moved Permanently — tarayıcılar ve proxy'ler cache'ler, çoğu zaman
	//         çok uzun süre. Mümkün olan en hızlı yönlendirme, sana neredeyse sıfır
	//         yük... ve tıklamaları görmeyi bırakırsın. Analitik kararır, üstelik o
	//         linki bir kez ziyaret etmiş kimse için artık değiştiremez ya da iptal
	//         edemezsin.
	//       302 Found — varsayılan olarak cache'lenmez. Her tıklama sana gelir:
	//         analitiği korursun, hedefi değiştirme ve kapatma yeteneğini korursun,
	//         karşılığında trafiğin bedelini ödersin.
	//     Varsayılan 302, çünkü bir kısaltıcıda tıklama verisi ve iptal edebilmek
	//     ürünün kendisi. Bu, HTTP durum koduna kılık değiştirmiş bir cache kararı.
	// [Topic · Konu: Cache — nerede durur, kim kontrol eder]
	PermanentRedirect bool
}

func Load() Config {
	return Config{
		Addr:    env("LINKLY_ADDR", ":8080"),
		BaseURL: env("LINKLY_BASE_URL", "http://localhost:8080"),

		ShardCount:  envInt("LINKLY_SHARDS", 16),
		CodeLength:  envInt("LINKLY_CODE_LENGTH", 7),
		MaxAttempts: envInt("LINKLY_MAX_ATTEMPTS", 5),

		CacheSize:        envInt("LINKLY_CACHE_SIZE", 10000),
		CacheTTL:         envDur("LINKLY_CACHE_TTL", 5*time.Minute),
		CacheNegativeTTL: envDur("LINKLY_CACHE_NEG_TTL", 10*time.Second),
		CacheJitter:      envFloat("LINKLY_CACHE_JITTER", 0.2),

		RateLimitPerSec: envFloat("LINKLY_RATE_PER_SEC", 50),
		RateLimitBurst:  envFloat("LINKLY_RATE_BURST", 100),
		RateLimitDryRun: envBool("LINKLY_RATE_DRY_RUN", false),

		AnalyticsQueue:      envInt("LINKLY_ANALYTICS_QUEUE", 4096),
		AnalyticsBatch:      envInt("LINKLY_ANALYTICS_BATCH", 128),
		AnalyticsFlushEvery: envDur("LINKLY_ANALYTICS_FLUSH", time.Second),

		RequestTimeout:  envDur("LINKLY_REQUEST_TIMEOUT", 3*time.Second),
		ShutdownTimeout: envDur("LINKLY_SHUTDOWN_TIMEOUT", 15*time.Second),

		PermanentRedirect: envBool("LINKLY_PERMANENT_REDIRECT", false),
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return v
	}
	return def
}

func envFloat(k string, def float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(k), 64); err == nil {
		return v
	}
	return def
}

func envBool(k string, def bool) bool {
	if v, err := strconv.ParseBool(os.Getenv(k)); err == nil {
		return v
	}
	return def
}

func envDur(k string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(os.Getenv(k)); err == nil {
		return v
	}
	return def
}
