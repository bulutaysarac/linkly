// Package analytics records click events off the request path.
//
// EN: This package is the asynchronism chapter in miniature. A redirect must
//
//	answer in single-digit milliseconds; counting the click must never be
//	allowed to slow it down or fail it. So the write path is:
//	    handler → bounded channel → worker → batch → sink
//	and every arrow in that line is a deliberate decision documented below.
//
// TR: Bu paket, asenkronizm bölümünün küçültülmüş hâli. Bir yönlendirme tek haneli
//
//	milisaniyede cevap vermeli; tıklamayı saymak bunu ne yavaşlatabilmeli ne de
//	düşürebilmeli. Bu yüzden yazma yolu şu:
//	    handler → sınırlı kanal → worker → batch → sink
//	ve bu satırdaki her ok, aşağıda gerekçesi yazılı bilinçli bir karar.
//
// [Topic · Konu: Asenkronizm, back pressure, write-behind, graceful shutdown]
package analytics

import (
	"context"
	"sync"
	"time"

	"github.com/bulutaysarac/linkly/internal/metrics"
)

// Event is one click.
type Event struct {
	Code     string
	TenantID string
	At       time.Time
	Referer  string
}

// Sink persists a batch. In production this is ClickHouse, Kinesis, S3, ...
//
// EN: Batching is write-behind: throughput up, freshness down. Columnar stores in
//
//	particular hate row-at-a-time inserts (each insert becomes a part, and merge
//	cost explodes). Large ingestion platforms pay this exact price on purpose — events buffer in S3
//	and land in ClickHouse in bulk, which is why an event is not instantly
//	visible in a report. That is not a bug, the buffer just has not flushed.
//
// TR: Batch'leme write-behind'dır: throughput yukarı, tazelik aşağı. Özellikle
//
//	kolon tabanlı depolar satır satır insert'ten nefret eder (her insert bir
//	part olur, merge maliyeti patlar). Büyük ingestion platformları bu bedeli bilerek ödüyor — event'ler
//	S3'te tamponlanıp ClickHouse'a toplu iniyor, bu yüzden bir event raporda
//	anında görünmez. Bu bir hata değil; tampon henüz boşalmadı.
//
// [Topic · Konu: Write-behind]
type Sink interface {
	Write(ctx context.Context, batch []Event) error
}

type Collector struct {
	ch   chan Event
	sink Sink
	met  *metrics.Registry

	batchSize  int
	flushEvery time.Duration

	wg       sync.WaitGroup
	stopOnce sync.Once
	stop     chan struct{}
}

type Options struct {
	// QueueSize is the bound. This single number IS the back pressure policy.
	//
	// EN: An unbounded queue is not a safety net, it is a delayed crash: the queue
	//     outgrows memory, spills, slows down, and the slowdown makes the queue grow
	//     faster. Bounding it forces you to answer the real question — what do we do
	//     when we cannot keep up? — instead of pretending it never happens.
	// TR: Sınırsız kuyruk bir emniyet ağı değil, ertelenmiş bir çöküştür: kuyruk
	//     belleği aşar, taşar, yavaşlar ve yavaşlama kuyruğu daha hızlı büyütür.
	//     Sınır koymak seni asıl soruyu cevaplamaya zorlar — yetişemediğimizde ne
	//     yapacağız? — onu hiç olmayacakmış gibi davranmak yerine.
	QueueSize  int
	BatchSize  int
	FlushEvery time.Duration
}

func New(sink Sink, o Options, met *metrics.Registry) *Collector {
	if o.QueueSize <= 0 {
		o.QueueSize = 4096
	}
	if o.BatchSize <= 0 {
		o.BatchSize = 128
	}
	if o.FlushEvery <= 0 {
		o.FlushEvery = time.Second
	}
	return &Collector{
		ch:         make(chan Event, o.QueueSize),
		sink:       sink,
		met:        met,
		batchSize:  o.BatchSize,
		flushEvery: o.FlushEvery,
		stop:       make(chan struct{}),
	}
}

// Record enqueues an event without ever blocking the caller.
//
// EN: The most important decision in this package, and it is one line: when the
//
//	queue is full we DROP the event and count the drop. We do not block.
//
//	Why dropping is right here: losing a click count is cheap and recoverable
//	(the curve barely moves); blocking a user-facing redirect is expensive and
//	visible. This is a fail-open choice, and it is the same reasoning used for a
//	messaging frequency cap — the question is never "which is safer" in the
//	abstract, it is "what does being wrong cost on THIS path".
//
//	It also means analytics are at-most-once, i.e. weak consistency, which is
//	exactly the level you would assign to metric counters. Had this been billing
//	rather than analytics, the answer would flip: bound the queue, and return 503
//	upstream when it is full.
//
//	analytics_dropped is a first-class metric for that reason. A drop counter
//	nobody watches is a silent failure with extra steps.
//
// TR: Bu paketteki en önemli karar ve tek satır: kuyruk doluyken event'i DÜŞÜRÜYORUZ
//
//	ve düşüşü sayıyoruz. Bloklamıyoruz.
//
//	Neden düşürmek doğru: bir tıklama sayısını kaybetmek ucuz ve telafi edilebilir
//	(eğri kıpırdamaz); kullanıcıya dönük bir yönlendirmeyi bloklamak pahalı ve
//	görünür. Bu bir fail-open tercihi ve mesaj frekans sınırı için
//	kullanılan akıl yürütmenin aynısı — soru hiçbir zaman soyut olarak "hangisi
//	daha güvenli" değil, "BU yolda yanılmanın bedeli ne".
//
//	Bu aynı zamanda analitiğin at-most-once, yani weak consistency olduğu anlamına
//	geliyor — metrik sayaçlarına vereceğin seviyenin tam olarak kendisi.
//	Burası analitik değil faturalama olsaydı cevap tersine dönerdi: kuyruğu sınırla,
//	dolduğunda yukarıya 503 dön.
//
//	analytics_dropped bu yüzden birinci sınıf bir metrik. Kimsenin bakmadığı bir
//	düşme sayacı, fazladan adımı olan bir sessiz arızadır.
//
// [Topic · Konu: Back pressure, fail-open, teslimat garantileri]
func (c *Collector) Record(e Event) {
	select {
	case c.ch <- e:
		c.met.Inc("analytics_enqueued")
	default:
		c.met.Inc("analytics_dropped")
	}
}

// Run consumes the queue until Shutdown. Call it in a goroutine.
func (c *Collector) Run() {
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		ticker := time.NewTicker(c.flushEvery)
		defer ticker.Stop()

		batch := make([]Event, 0, c.batchSize)

		flush := func() {
			if len(batch) == 0 {
				return
			}
			// EN: A bounded context so a stuck sink cannot wedge the worker forever.
			//     Timeouts belong on every outbound call, not only the ones you
			//     expect to be slow.
			// TR: Sınırlı bir context ki takılan bir sink worker'ı sonsuza kadar
			//     kilitlemesin. Timeout, yalnızca yavaş olmasını beklediğin değil,
			//     dışa giden her çağrının hakkıdır.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := c.sink.Write(ctx, batch)
			cancel()
			if err != nil {
				// EN: In production this is where a dead letter queue goes, plus an
				//     alarm on its depth. A DLQ nobody watches is a bin, not a
				//     safety net — and replaying it weeks later is usually
				//     impossible because the surrounding state has moved on.
				// TR: Üretimde dead letter queue tam burada durur, üstüne derinlik
				//     alarmı. Kimsenin bakmadığı bir DLQ emniyet ağı değil çöp
				//     kutusudur — haftalar sonra geri oynatmak da genelde imkânsız
				//     olur, çünkü çevredeki durum çoktan değişmiştir.
				// [Topic · Konu: retry → backoff → DLQ]
				c.met.Add("analytics_write_error", 1)
			} else {
				c.met.Add("analytics_written", uint64(len(batch)))
			}
			batch = batch[:0]
		}

		for {
			select {
			case e := <-c.ch:
				batch = append(batch, e)
				if len(batch) >= c.batchSize {
					flush()
				}
			case <-ticker.C:
				// EN: Time-based flush guarantees a latency ceiling for the tail of
				//     a batch. Without it, the last few events of a quiet period
				//     would sit in memory until traffic resumed.
				// TR: Zaman tabanlı flush, bir batch'in kuyruğuna gecikme tavanı
				//     verir. Olmasa, sakin bir dönemin son birkaç event'i trafik
				//     geri gelene kadar bellekte beklerdi.
				flush()
			case <-c.stop:
				// EN: DRAIN. This is the graceful-shutdown lesson, in code.
				//     A synchronous service that dies mid-request produces an error
				//     the client can see and retry. An async consumer that dies
				//     holding events produces NOTHING — no error, no log, just data
				//     that quietly never happened. One fleet audit found this exact
				//     ordering bug in 21 services at once.
				//     Order: stop accepting → finish what you hold → then close.
				// TR: DRAIN. Graceful shutdown dersinin koddaki hâli.
				//     İstek ortasında ölen senkron bir servis, istemcinin görüp
				//     tekrar deneyebileceği bir hata üretir. Elinde event'lerle ölen
				//     asenkron bir consumer HİÇBİR ŞEY üretmez — ne hata, ne log,
				//     sadece sessizce hiç olmamış veri. Bir filo taraması tam bu sıra
				//     hatasını aynı anda 21 serviste buldu.
				//     Sıra: kabul etmeyi kes → elindekini bitir → sonra kapat.
				// [Topic · Konu: Graceful shutdown]
				for {
					select {
					case e := <-c.ch:
						batch = append(batch, e)
						if len(batch) >= c.batchSize {
							flush()
						}
					default:
						flush()
						return
					}
				}
			}
		}
	}()
}

// Shutdown signals the worker to drain and waits for it (or for ctx).
func (c *Collector) Shutdown(ctx context.Context) error {
	c.stopOnce.Do(func() { close(c.stop) })

	done := make(chan struct{})
	go func() { c.wg.Wait(); close(done) }()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		// EN: Draining is best-effort under a deadline. Waiting forever for a
		//     wedged sink would turn a clean rollout into a stuck one.
		// TR: Drain, bir süre sınırı altında elden gelenin en iyisidir. Takılmış bir
		//     sink'i sonsuza kadar beklemek, temiz bir deploy'u takılmış bir deploy'a
		//     çevirirdi.
		return ctx.Err()
	}
}

// MemorySink counts clicks per code. Stands in for ClickHouse/Kinesis/S3.
type MemorySink struct {
	mu     sync.Mutex
	counts map[string]uint64
}

func NewMemorySink() *MemorySink { return &MemorySink{counts: make(map[string]uint64)} }

func (s *MemorySink) Write(_ context.Context, batch []Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range batch {
		s.counts[e.Code]++
	}
	return nil
}

func (s *MemorySink) Count(code string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[code]
}
