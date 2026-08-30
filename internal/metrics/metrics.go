// Package metrics is a dependency-free counter registry.
//
// EN: Every failure mode in this project is *countable*. If a guard silently stops
//
//	working, the only way you will ever notice is a counter that stopped moving
//	(or one that started moving). Metrics are not decoration here — they are the
//	difference between a loud failure and a silent one.
//
// TR: Bu projedeki her arıza modu *sayılabilir*. Bir koruma sessizce çalışmayı
//
//	bırakırsa bunu fark etmenin tek yolu, duran (ya da hareketlenen) bir sayaçtır.
//	Metrikler burada süs değil — gürültülü arıza ile sessiz arıza arasındaki fark.
//
// [Topic · Konu: Gözlemlenebilirlik]
package metrics

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Registry holds named monotonic counters.
// TR: İsimli, yalnızca artan sayaçları tutar.
type Registry struct {
	mu       sync.RWMutex
	counters map[string]*atomic.Uint64
}

func NewRegistry() *Registry {
	return &Registry{counters: make(map[string]*atomic.Uint64)}
}

// Inc increments a counter by 1, creating it on first use.
// TR: Sayacı 1 artırır; ilk kullanımda oluşturur.
func (r *Registry) Inc(name string) { r.Add(name, 1) }

func (r *Registry) Add(name string, delta uint64) {
	r.mu.RLock()
	c, ok := r.counters[name]
	r.mu.RUnlock()
	if !ok {
		r.mu.Lock()
		if c, ok = r.counters[name]; !ok {
			c = new(atomic.Uint64)
			r.counters[name] = c
		}
		r.mu.Unlock()
	}
	c.Add(delta)
}

func (r *Registry) Get(name string) uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if c, ok := r.counters[name]; ok {
		return c.Load()
	}
	return 0
}

// Snapshot renders all counters in Prometheus text exposition format.
// TR: Tüm sayaçları Prometheus metin formatında döker.
func (r *Registry) Snapshot() string {
	r.mu.RLock()
	names := make([]string, 0, len(r.counters))
	for n := range r.counters {
		names = append(names, n)
	}
	vals := make(map[string]uint64, len(names))
	for _, n := range names {
		vals[n] = r.counters[n].Load()
	}
	r.mu.RUnlock()

	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		fmt.Fprintf(&b, "linkly_%s %d\n", n, vals[n])
	}
	return b.String()
}
