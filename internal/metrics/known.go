package metrics

// Known lists every counter linkly can emit.
//
// EN: These are pre-registered at zero on startup, and that is not cosmetic.
//
//	A counter that only appears in /metrics the first time it fires cannot be
//	alerted on, because the monitoring system cannot tell "this never happened"
//	from "this endpoint is not reporting". On a dashboard, no data and zero look
//	identical — and the difference is exactly the one you care about at 3am.
//
//	Concrete example from this codebase: cache_negative_hit only increments when
//	the same missing key is looked up twice. If you never pre-register it, a
//	silent regression that disables negative caching entirely produces... the
//	same empty graph you already had. Nothing to notice.
//
// TR: Bunlar açılışta sıfır değerle önceden kaydediliyor ve bu kozmetik değil.
//
//	Yalnızca ilk tetiklendiğinde /metrics'te beliren bir sayaca alarm bağlayamazsın;
//	çünkü izleme sistemi "bu hiç olmadı" ile "bu uç raporlamıyor"u ayırt edemez.
//	Bir dashboard'da veri yok ile sıfır birebir aynı görünür — ve aradaki fark,
//	gecenin üçünde tam olarak umursadığın fark.
//
//	Bu kod tabanından somut örnek: cache_negative_hit yalnızca aynı eksik anahtar
//	iki kez arandığında artıyor. Önceden kaydetmezsen, negatif cache'i tamamen
//	devre dışı bırakan sessiz bir regresyon... zaten sahip olduğun boş grafiğin
//	aynısını üretir. Fark edilecek hiçbir şey yok.
//
// [Topic · Konu: Gözlemlenebilirlik, sessiz arıza]
func Known() []string {
	return []string{
		// http
		"http_requests_total", "http_panic",
		"http_status_2xx", "http_status_3xx", "http_status_4xx", "http_status_5xx",
		// redirect path
		"redirect_ok", "redirect_not_found", "redirect_gone",
		// create path
		"create_ok", "create_collision", "create_exhausted",
		// api
		"api_not_found_or_forbidden",
		// cache
		"cache_hit", "cache_negative_hit", "cache_miss", "cache_stampede_wait",
		"cache_expired", "cache_eviction", "cache_invalidate", "cache_load_error",
		// rate limiting
		"ratelimit_allow", "ratelimit_reject", "ratelimit_would_reject", "ratelimit_buckets_gc",
		// analytics
		"analytics_enqueued", "analytics_dropped", "analytics_written", "analytics_write_error",
	}
}

// Register creates the named counters at zero if they do not exist yet.
func (r *Registry) Register(names ...string) {
	for _, n := range names {
		r.Add(n, 0)
	}
}
