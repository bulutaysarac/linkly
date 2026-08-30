// Package store defines the persistence contract for links.
//
// EN: The interface is deliberately narrower than "a database". Every method
//
//	that touches a single tenant's data takes a tenantID and enforces it — the
//	tenant boundary lives in the *signature*, not in the caller's discipline.
//	That is the whole lesson of "shard key is also a security boundary":
//	if forgetting the filter is possible, someone will eventually forget it.
//
// TR: Arayüz bilinçli olarak "bir veritabanı"ndan dar tutuldu. Tek bir kiracının
//
//	verisine dokunan her metot tenantID alır ve zorlar — kiracı sınırı çağıranın
//	dikkatinde değil, *imzada* yaşıyor. "Shard anahtarı aynı zamanda güvenlik
//	sınırıdır" dersinin tamamı bu: filtreyi unutmak mümkünse, biri eninde
//	sonunda unutur.
//
// [Topic · Konu: Çok kiracılılık = güvenlik sınırı]
package store

import (
	"context"
	"errors"
	"time"
)

var (
	// ErrNotFound: the code does not exist at all.
	// TR: Kod hiç yok.
	ErrNotFound = errors.New("store: not found")

	// ErrCodeTaken: the code exists and belongs to someone (maybe someone else).
	//
	// EN: Kept distinct from ErrNotFound on purpose. Collapsing "missing" and
	//     "present" into one signal is exactly the bug behind a class of silent cache outage,
	//     where "empty" and "absent" were represented by the same value and the
	//     cache silently stopped working for that key.
	// TR: ErrNotFound'dan bilinçli olarak ayrı. "Yok" ile "var"ı tek sinyale
	//     indirmek, sessiz cache arızalarının ta kendisi: "boş" ile "yok" aynı
	//     değerle temsil edilince cache o anahtar için sessizce çalışmayı bıraktı.
	ErrCodeTaken = errors.New("store: code already taken")

	// ErrForbidden: the code exists but belongs to another tenant.
	//
	// EN: Note this is returned to the *caller*, but the HTTP layer deliberately
	//     turns it into 404, not 403 — see httpapi. Telling an attacker "this code
	//     exists but is not yours" is itself an information leak.
	// TR: Bu hata çağırana döner ama HTTP katmanı bunu bilinçli olarak 403 değil
	//     404'e çevirir — bkz. httpapi. Saldırgana "bu kod var ama senin değil"
	//     demek başlı başına bir bilgi sızıntısıdır.
	ErrForbidden = errors.New("store: forbidden")
)

// Link is one shortened URL.
type Link struct {
	Code      string    `json:"code"`
	LongURL   string    `json:"long_url"`
	TenantID  string    `json:"tenant_id"`
	CreatedAt time.Time `json:"created_at"`

	// ExpiresAt zero value means "never expires".
	//
	// EN: A TTL is a contract — "this link is valid until X" — and contracts must
	//     be readable. I have seen a 30-day TTL hard-coded as
	//     time.Hour*24*30, which silently detonated exactly 30 days after go-live.
	//     Here the expiry is data, not a constant buried in a function.
	// TR: TTL bir sözleşmedir — "bu link şu ana kadar geçerli" — ve sözleşmeler
	//     okunabilir olmalı. Koda gömülü 30 günlük bir TTL gördüm ve
	//     yayına alındıktan tam 30 gün sonra sessizce patladı. Burada son kullanma
	//     bir fonksiyonun içine gömülü sabit değil, veri.
	// [Topic · Konu: TTL bir sözleşmedir]
	ExpiresAt time.Time `json:"expires_at,omitzero"`
}

// Expired reports whether the link is past its expiry at time now.
func (l Link) Expired(now time.Time) bool {
	return !l.ExpiresAt.IsZero() && now.After(l.ExpiresAt)
}

// Store is the persistence port.
//
// EN: Every method takes a context. This is not ceremony — it is how a deadline
//
//	set at the HTTP edge propagates all the way down, so an inner call can never
//	outlive the request that started it. Timeouts must decrease from outside in
//	; a context is the mechanism that makes that possible.
//
// TR: Her metot context alır. Bu tören değil — HTTP kenarında konan bir sürenin
//
//	en dibe kadar yayılma biçimi bu; böylece içerideki bir çağrı, kendisini
//	başlatan istekten uzun yaşayamaz. Timeout'lar dıştan içe azalmalı
//	; context bunu mümkün kılan mekanizma.
type Store interface {
	// CreateUnique inserts the link only if Code is free.
	//
	// EN: This is a *conditional* insert, not "read then write". Read-then-write
	//     has a race window in which two requests both see "free" and both insert.
	//     In DynamoDB the same semantics are expressed as
	//     ConditionExpression: attribute_not_exists(code); in SQL as a UNIQUE index.
	//     The atomicity has to live in the store, never in the caller.
	// TR: Bu *koşullu* bir insert; "önce oku sonra yaz" değil. Oku-sonra-yaz'da iki
	//     isteğin de "boşta" görüp ikisinin de yazdığı bir yarış penceresi vardır.
	//     DynamoDB'de aynı semantik attribute_not_exists ile, SQL'de UNIQUE index
	//     ile ifade edilir. Atomiklik depoda yaşamalı, asla çağıranda değil.
	// [Topic · Konu: Atomiklik ≠ idempotency]
	CreateUnique(ctx context.Context, l Link) error

	// Get resolves a code globally (the redirect path — no tenant).
	//
	// EN: Deliberately NOT tenant-scoped: a short link must resolve for any visitor
	//     on the internet, who has no tenant. The tenant check belongs on the
	//     management endpoints below, and forgetting it there is the IDOR bug.
	// TR: Bilinçli olarak kiracı kapsamlı DEĞİL: kısa link, internetteki herhangi
	//     bir ziyaretçi için çözülmeli ve o ziyaretçinin kiracısı yok. Kiracı
	//     kontrolü aşağıdaki yönetim uçlarının işi; orada unutmak IDOR hatasıdır.
	Get(ctx context.Context, code string) (Link, error)

	// GetOwned resolves a code and verifies tenant ownership.
	// TR: Kodu çözer ve kiracı sahipliğini doğrular.
	GetOwned(ctx context.Context, tenantID, code string) (Link, error)

	// ListByTenant returns a tenant's links. Always scoped.
	// TR: Kiracının linklerini döner. Her zaman kapsamlı.
	ListByTenant(ctx context.Context, tenantID string, limit int) ([]Link, error)

	// Delete removes a link the tenant owns.
	// TR: Kiracının sahibi olduğu bir linki siler.
	Delete(ctx context.Context, tenantID, code string) error

	// Close releases resources. Called last during shutdown.
	// TR: Kaynakları bırakır. Kapatma sırasında en son çağrılır.
	Close() error
}
