// Package shortcode generates the short identifier that appears in linkly URLs.
//
// EN: This is the single most consequential design decision in a URL shortener.
//
//	The code is simultaneously (a) the primary key, (b) the cache key,
//	(c) the shard key, and (d) — because anyone can type it into a browser —
//	part of the security surface. Getting it wrong is expensive in four
//	different ways at once.
//
// TR: Bir link kısaltıcıdaki en belirleyici tasarım kararı budur. Bu kod aynı anda
//
//	(a) birincil anahtar, (b) cache anahtarı, (c) shard anahtarı ve (d) tarayıcıya
//	elle yazılabildiği için güvenlik yüzeyinin parçasıdır. Yanlış seçmek dört
//	ayrı yerden birden fatura keser.
//
// [Topic · Konu: Anahtar tasarımı, sharding, güvenlik]
package shortcode

import (
	"crypto/rand"
	"errors"
	"math/big"
	"strings"
)

// Base62 alphabet: digits + upper + lower. 62 symbols.
//
// EN: Why base62 and not base64? Base64 contains '+' and '/', which are not
//
//	URL-safe, and '=' padding. Base62 is the largest alphabet that survives a
//	URL, a shell, a spreadsheet and a phone keyboard without escaping.
//
// TR: Neden base62, base64 değil? Base64'te '+' ve '/' var; ikisi de URL-güvenli
//
//	değil, üstüne '=' dolgusu geliyor. Base62, URL'den de shell'den de
//	tablodan da telefon klavyesinden de kaçış karakteri gerektirmeden geçen
//	en geniş alfabe.
const Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// DefaultLength is 7 characters.
//
// EN: Capacity math you should be able to do on a napkin in an interview:
//
//	  62^6 ≈ 5.7e10  (57 milyar)
//	  62^7 ≈ 3.5e12  (3,5 trilyon)
//	At 100 new links/second you would need ~1,100 years to fill 62^7. So 7 is
//	not "a nice round number" — it is the smallest length whose keyspace makes
//	collisions statistically irrelevant for this write rate. This is the same
//	kind of reasoning as a hard per-partition throughput ceiling: the number
//	comes from a measured constraint, not from taste.
//
// TR: Mülakatta peçete üstünde yapabilmen gereken kapasite hesabı:
//
//	  62^6 ≈ 5,7e10 · 62^7 ≈ 3,5e12
//	Saniyede 100 yeni link üretsen 62^7'yi doldurman ~1.100 yıl sürer. Yani 7
//	"güzel bir sayı" değil; bu yazma hızında çakışmayı istatistiksel olarak
//	önemsiz kılan en kısa uzunluk. Notlardaki "partition başına 3.000 RCU/s"
//	ile aynı cins akıl yürütme: sayı zevkten değil, ölçülen bir kısıttan geliyor.
const DefaultLength = 7

// ErrInvalidCode is returned by Validate.
var ErrInvalidCode = errors.New("shortcode: invalid code")

// reserved codes can never be handed out, because they would shadow real routes.
//
// EN: /api, /healthz and /metrics are real endpoints. If a user could claim the
//
//	alias "api", their link would silently shadow the management API. This is a
//	namespace collision — the same class of bug as letting user input into a
//	cache key. User input must never participate in the *syntax*
//	of a structure, only in its values.
//
// TR: /api, /healthz ve /metrics gerçek uçlar. Kullanıcı "api" takma adını
//
//	alabilseydi, linki yönetim API'sini sessizce gölgelerdi. Bu bir isim alanı
//	çakışması — kullanıcı girdisinin cache anahtarına girmesiyle aynı sınıf hata
//	Kullanıcı girdisi hiçbir yapının *sözdizimine* katılamaz,
//	yalnızca değerine katılır.
var reserved = map[string]struct{}{
	"api": {}, "healthz": {}, "readyz": {}, "metrics": {}, "static": {},
	"admin": {}, "login": {}, "favicon.ico": {}, "robots.txt": {},
}

// Random returns a cryptographically random base62 code of length n.
//
// EN: crypto/rand, not math/rand. With math/rand an attacker who observes a few
//
//	codes can predict the next ones and enumerate other people's links — every
//	shortener holds links that are "unlisted but not secret" (a draft invoice,
//	an internal doc). Unpredictability *is* the access control here.
//	This is the counterpart of the sequential-counter design below.
//
// TR: math/rand değil crypto/rand. math/rand ile birkaç kodu gören saldırgan
//
//	sonrakini tahmin edip başkalarının linklerini tarayabilir — her kısaltıcıda
//	"listelenmemiş ama gizli de olmayan" linkler vardır (taslak fatura, iç
//	doküman). Burada erişim kontrolünün kendisi tahmin edilemezliktir.
//	Aşağıdaki sıralı sayaç tasarımının karşı kutbu budur.
//
// [Topic · Konu: Güvenlik — sıralama/numaralandırma saldırısı]
func Random(n int) (string, error) {
	if n <= 0 {
		return "", ErrInvalidCode
	}
	max := big.NewInt(int64(len(Alphabet)))
	var b strings.Builder
	b.Grow(n)
	for i := 0; i < n; i++ {
		idx, err := rand.Int(rand.Reader, max)
		if err != nil {
			// EN: A failing CSPRNG is not something to paper over with a fallback to
			//     math/rand. Fail loudly — a weak code is worse than no code.
			// TR: Bozuk bir CSPRNG'yi math/rand'a düşerek örtmezsin. Gürültüyle
			//     başarısız ol — zayıf bir kod, hiç kod olmamasından kötüdür.
			return "", err
		}
		b.WriteByte(Alphabet[idx.Int64()])
	}
	return b.String(), nil
}

// EncodeCounter renders an integer as base62 (the "sequential ID" strategy).
//
// EN: Kept here on purpose as the *rejected* alternative, so the trade-off is
//
//	visible in code rather than in a design doc nobody reads:
//	  + dense keyspace, zero collisions by construction, no retry loop
//	  − codes are guessable: id 1000 → "g8", 1001 → "g9". Anyone can walk the
//	    entire corpus. It also leaks your growth rate to competitors.
//	Real systems that want both properties use a sequential ID *plus* an
//	encryption step (e.g. Feistel/Hashids) so the output is dense AND opaque.
//
// TR: Bilinçli olarak *reddedilen* alternatif olarak burada duruyor ki takas,
//
//	kimsenin okumadığı bir tasarım dokümanında değil kodda görünsün:
//	  + yoğun anahtar uzayı, yapısı gereği sıfır çakışma, retry döngüsü yok
//	  − kodlar tahmin edilebilir: 1000 → "g8", 1001 → "g9". İsteyen tüm veriyi
//	    gezebilir. Üstüne büyüme hızını da rakibine sızdırırsın.
//	İkisini birden isteyen gerçek sistemler sıralı ID'yi bir şifreleme adımıyla
//	(Feistel/Hashids) birleştirir: çıktı hem yoğun hem opak olur.
func EncodeCounter(id uint64) string {
	if id == 0 {
		return string(Alphabet[0])
	}
	base := uint64(len(Alphabet))
	buf := make([]byte, 0, 11)
	for id > 0 {
		buf = append(buf, Alphabet[id%base])
		id /= base
	}
	for i, j := 0, len(buf)-1; i < j; i, j = i+1, j-1 {
		buf[i], buf[j] = buf[j], buf[i]
	}
	return string(buf)
}

// Validate checks a user-supplied custom alias.
//
// EN: Custom aliases are user input that becomes a URL path AND a database key.
//
//	Two independent checks are needed and neither implies the other:
//	  1. character set  — keeps it URL-safe and cache-key-safe
//	  2. reserved list  — keeps it from shadowing a route
//	Defence in depth: one check failing must not open the door.
//
// TR: Takma adlar, hem URL yoluna hem veritabanı anahtarına dönüşen kullanıcı
//
//	girdisidir. İki bağımsız kontrol gerekir ve biri diğerini kapsamaz:
//	  1. karakter kümesi — URL ve cache anahtarı güvenliği
//	  2. rezerve liste   — bir rotayı gölgelememesi
//	Katmanlı savunma: bir kontrolün düşmesi kapıyı açmamalı.
func Validate(code string) error {
	if len(code) < 3 || len(code) > 32 {
		return ErrInvalidCode
	}
	if _, bad := reserved[strings.ToLower(code)]; bad {
		return ErrInvalidCode
	}
	for i := 0; i < len(code); i++ {
		if !strings.ContainsRune(Alphabet, rune(code[i])) {
			return ErrInvalidCode
		}
	}
	return nil
}
