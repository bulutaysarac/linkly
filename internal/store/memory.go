package store

import (
	"context"
	"hash/fnv"
	"sync"
)

// Memory is a sharded in-memory Store.
//
// EN: Sharding an in-process map may look like over-engineering — and for a demo
//
//	it is. It is here because it makes the *shape* of sharding visible at a
//	size you can hold in your head:
//	  · one big map + one mutex  → every writer serialises behind one lock
//	  · N maps  + N mutexes      → writers spread across N locks
//	That is the same reasoning that puts shards behind a columnar store and
//	TENANT#{tenant}#{shard} in DynamoDB. Same idea, three orders of magnitude
//	apart. And it shows the failure mode too: a shard is only as balanced as the
//	hash of its key — a hot key still lands on exactly one shard.
//
// TR: Süreç içi bir map'i shard'lamak fazla mühendislik gibi durabilir — demo için
//
//	öyle de. Burada olma sebebi, sharding'in *şeklini* kafada tutabileceğin bir
//	ölçekte göstermesi:
//	  · tek büyük map + tek mutex → bütün yazanlar tek kilidin arkasında sıraya girer
//	  · N map + N mutex          → yazanlar N kilide dağılır
//	kolon tabanlı bir deponun shard yapısını ve DynamoDB'deki TENANT#{tenant}#{shard}
//	anahtarını doğuran akıl yürütme bu. Aynı fikir, üç mertebe farkla. Arıza modunu
//	da gösteriyor: bir shard, anahtarının hash'i kadar dengelidir — sıcak bir
//	anahtar yine tek bir shard'a düşer.
//
// [Topic · Konu: Sharding, kilit çekişmesi, hot partition]
type Memory struct {
	shards []*shard
	mask   uint32
}

type shard struct {
	mu sync.RWMutex
	m  map[string]Link
}

// NewMemory creates a store with shardCount shards (rounded up to a power of two).
//
// EN: Power of two so shard selection is a bitmask (hash & mask) instead of a
//
//	modulo. Cheap, but the real reason is different: with a power of two the
//	distribution is stable and easy to reason about. Note the honest limitation —
//	changing shardCount rehashes *every* key. That is exactly why consistent
//	hashing exists.
//
// TR: İkinin kuvveti seçilir ki shard seçimi modulo yerine bit maskesi olsun
//
//	(hash & mask). Ucuz, ama asıl sebep başka: ikinin kuvvetinde dağılım kararlı
//	ve akıl yürütmesi kolay. Dürüst sınırı da not et — shardCount'u değiştirmek
//	*bütün* anahtarları yeniden hash'ler. Consistent hashing tam olarak bu yüzden
//	var.
func NewMemory(shardCount int) *Memory {
	n := 1
	for n < shardCount {
		n <<= 1
	}
	m := &Memory{shards: make([]*shard, n), mask: uint32(n - 1)}
	for i := range m.shards {
		m.shards[i] = &shard{m: make(map[string]Link)}
	}
	return m
}

func (m *Memory) shardFor(code string) *shard {
	h := fnv.New32a()
	_, _ = h.Write([]byte(code))
	return m.shards[h.Sum32()&m.mask]
}

func (m *Memory) CreateUnique(ctx context.Context, l Link) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s := m.shardFor(l.Code)

	// EN: The check and the write happen inside ONE lock acquisition. Splitting
	//     them ("exists?" then "insert") reintroduces the race the conditional
	//     insert exists to close. In a real store this is one round trip with a
	//     condition attached, for the same reason.
	// TR: Kontrol ve yazma TEK bir kilit alımının içinde. İkiye bölmek ("var mı?"
	//     sonra "ekle") koşullu insert'in kapatmak için var olduğu yarışı geri
	//     getirir. Gerçek bir depoda bu, aynı sebeple, koşul eklenmiş tek bir tur.
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.m[l.Code]; exists {
		return ErrCodeTaken
	}
	s.m[l.Code] = l
	return nil
}

func (m *Memory) Get(ctx context.Context, code string) (Link, error) {
	if err := ctx.Err(); err != nil {
		return Link{}, err
	}
	s := m.shardFor(code)
	s.mu.RLock()
	l, ok := s.m[code]
	s.mu.RUnlock()
	if !ok {
		return Link{}, ErrNotFound
	}
	return l, nil
}

// GetOwned is Get plus the tenant check.
//
// EN: This four-line method is the whole IDOR lesson. Without the TenantID
//
//	comparison, tenant A could read tenant B's link — and nothing would error,
//	nothing would log, the dashboard would stay green. That is the "silent
//	failure" class that matters most here: an attack leaves traces, a
//	missing WHERE does not.
//
// TR: Bu dört satırlık metot, IDOR dersinin tamamı. TenantID karşılaştırması
//
//	olmasa A kiracısı B'nin linkini okurdu — ve hiçbir şey hata vermez, log
//	dolmaz, dashboard yeşil kalırdı. Notların tekrar tekrar döndüğü "sessiz
//	arıza" sınıfı bu: saldırı iz bırakır, unutulmuş bir WHERE bırakmaz.
//
// [Topic · Konu: Kimlik doğrulama ≠ yetkilendirme]
func (m *Memory) GetOwned(ctx context.Context, tenantID, code string) (Link, error) {
	l, err := m.Get(ctx, code)
	if err != nil {
		return Link{}, err
	}
	if l.TenantID != tenantID {
		return Link{}, ErrForbidden
	}
	return l, nil
}

func (m *Memory) ListByTenant(ctx context.Context, tenantID string, limit int) ([]Link, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// EN: Full scan across every shard. Honest about what this is: in a real store
	//     you would keep a secondary index keyed by tenant, precisely because a
	//     tenant-scoped list must not cost O(all data). Scanning here is a demo
	//     shortcut, and it is the same shortcut that turns into a production
	//     incident when the dataset grows.
	// TR: Bütün shard'larda tam tarama. Ne olduğu konusunda dürüst olalım: gerçek
	//     bir depoda kiracıya göre ikincil bir index tutardın, çünkü kiracı kapsamlı
	//     bir listeleme O(tüm veri) maliyetinde olmamalı. Buradaki tarama bir demo
	//     kısayolu — ve veri büyüdüğünde üretim arızasına dönüşen kısayolun aynısı.
	out := make([]Link, 0, limit)
	for _, s := range m.shards {
		s.mu.RLock()
		for _, l := range s.m {
			if l.TenantID == tenantID {
				out = append(out, l)
				if len(out) >= limit {
					s.mu.RUnlock()
					return out, nil
				}
			}
		}
		s.mu.RUnlock()
	}
	return out, nil
}

func (m *Memory) Delete(ctx context.Context, tenantID, code string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s := m.shardFor(code)
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.m[code]
	if !ok {
		return ErrNotFound
	}
	// EN: Ownership is re-checked *inside* the lock. Checking outside and deleting
	//     inside would be a time-of-check/time-of-use gap.
	// TR: Sahiplik kilidin *içinde* yeniden kontrol ediliyor. Dışarıda kontrol edip
	//     içeride silmek bir kontrol-anı/kullanım-anı boşluğu olurdu.
	if l.TenantID != tenantID {
		return ErrForbidden
	}
	delete(s.m, code)
	return nil
}

func (m *Memory) Close() error { return nil }

// Stats returns per-shard sizes, for the /metrics endpoint.
//
// EN: Exposed so you can actually *see* imbalance instead of assuming it away.
//
//	A shard distribution you never look at is a hot partition you will discover
//	from a customer complaint.
//
// TR: Dengesizliği varsaymak yerine gerçekten *görebilmen* için açıldı. Hiç
//
//	bakmadığın bir shard dağılımı, müşteri şikâyetiyle öğreneceğin bir hot
//	partition demektir.
func (m *Memory) Stats() []int {
	out := make([]int, len(m.shards))
	for i, s := range m.shards {
		s.mu.RLock()
		out[i] = len(s.m)
		s.mu.RUnlock()
	}
	return out
}

var _ Store = (*Memory)(nil)
