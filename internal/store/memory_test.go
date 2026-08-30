package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func newLink(code, tenant string) Link {
	return Link{Code: code, LongURL: "https://example.com", TenantID: tenant, CreatedAt: time.Now()}
}

func TestCreateUniqueIsConditional(t *testing.T) {
	ctx := context.Background()
	s := NewMemory(8)

	if err := s.CreateUnique(ctx, newLink("abc1234", "t1")); err != nil {
		t.Fatal(err)
	}
	err := s.CreateUnique(ctx, newLink("abc1234", "t2"))
	if !errors.Is(err, ErrCodeTaken) {
		t.Fatalf("second insert err = %v, want ErrCodeTaken", err)
	}
}

// TestCreateUniqueUnderRace is the reason CreateUnique exists at all:
// with a read-then-write implementation this test would let two winners through.
// TR: CreateUnique'in var olma sebebi: oku-sonra-yaz bir implementasyonda bu test
// iki kazanana birden izin verirdi.
func TestCreateUniqueUnderRace(t *testing.T) {
	ctx := context.Background()
	s := NewMemory(8)

	const n = 64
	var wg sync.WaitGroup
	var mu sync.Mutex
	winners := 0

	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if err := s.CreateUnique(ctx, newLink("racecode", "t1")); err == nil {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if winners != 1 {
		t.Fatalf("INVARIANT BROKEN: expected exactly 1 winner, got %d", winners)
	}
}

// TestTenantIsolation is the security test of this package.
// TR: Bu paketin güvenlik testi.
func TestTenantIsolation(t *testing.T) {
	ctx := context.Background()
	s := NewMemory(8)
	if err := s.CreateUnique(ctx, newLink("secret1", "tenant-a")); err != nil {
		t.Fatal(err)
	}

	if _, err := s.GetOwned(ctx, "tenant-b", "secret1"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("cross-tenant read err = %v, want ErrForbidden", err)
	}
	if err := s.Delete(ctx, "tenant-b", "secret1"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("cross-tenant delete err = %v, want ErrForbidden", err)
	}
	if _, err := s.GetOwned(ctx, "tenant-a", "secret1"); err != nil {
		t.Fatalf("owner read err = %v, want nil", err)
	}

	// The redirect path is global on purpose: a visitor has no tenant.
	// TR: Yönlendirme yolu bilinçli olarak global: ziyaretçinin kiracısı yok.
	if _, err := s.Get(ctx, "secret1"); err != nil {
		t.Fatalf("global Get err = %v, want nil", err)
	}
}

func TestListIsTenantScoped(t *testing.T) {
	ctx := context.Background()
	s := NewMemory(8)
	_ = s.CreateUnique(ctx, newLink("aaa1111", "t1"))
	_ = s.CreateUnique(ctx, newLink("bbb2222", "t1"))
	_ = s.CreateUnique(ctx, newLink("ccc3333", "t2"))

	got, err := s.ListByTenant(ctx, "t1", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	for _, l := range got {
		if l.TenantID != "t1" {
			t.Fatalf("leaked link from tenant %q", l.TenantID)
		}
	}
}

func TestExpired(t *testing.T) {
	now := time.Now()
	if (Link{}).Expired(now) {
		t.Error("zero ExpiresAt must mean never expires")
	}
	if !(Link{ExpiresAt: now.Add(-time.Second)}).Expired(now) {
		t.Error("past ExpiresAt must be expired")
	}
	if (Link{ExpiresAt: now.Add(time.Hour)}).Expired(now) {
		t.Error("future ExpiresAt must not be expired")
	}
}

func TestShardingDistributes(t *testing.T) {
	s := NewMemory(16)
	ctx := context.Background()
	for i := 0; i < 500; i++ {
		code, _ := randomish(i)
		_ = s.CreateUnique(ctx, newLink(code, "t1"))
	}
	used := 0
	for _, n := range s.Stats() {
		if n > 0 {
			used++
		}
	}
	if used < 8 {
		t.Fatalf("only %d/16 shards used — hash distribution looks broken", used)
	}
}

func randomish(i int) (string, error) {
	const a = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 7)
	for j := range b {
		b[j] = a[(i*7+j*13+j*j)%len(a)]
	}
	return string(b), nil
}
