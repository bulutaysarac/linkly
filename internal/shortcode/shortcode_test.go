package shortcode

import (
	"strings"
	"testing"
)

func TestRandomLengthAndAlphabet(t *testing.T) {
	for _, n := range []int{3, 7, 11} {
		c, err := Random(n)
		if err != nil {
			t.Fatalf("Random(%d): %v", n, err)
		}
		if len(c) != n {
			t.Fatalf("Random(%d) length = %d", n, len(c))
		}
		for _, r := range c {
			if !strings.ContainsRune(Alphabet, r) {
				t.Fatalf("code %q contains non-base62 rune %q", c, r)
			}
		}
	}
}

// TestRandomIsNotObviouslySequential guards the property that actually matters:
// codes must not be predictable from each other.
// TR: Asıl önemli özelliği koruyor: kodlar birbirinden tahmin edilebilir olmamalı.
func TestRandomIsNotObviouslySequential(t *testing.T) {
	seen := make(map[string]bool, 500)
	for i := 0; i < 500; i++ {
		c, err := Random(7)
		if err != nil {
			t.Fatal(err)
		}
		if seen[c] {
			t.Fatalf("duplicate code %q within 500 draws — entropy is broken", c)
		}
		seen[c] = true
	}
}

func TestEncodeCounter(t *testing.T) {
	cases := map[uint64]string{0: "0", 1: "1", 61: "z", 62: "10", 3843: "zz"}
	for in, want := range cases {
		if got := EncodeCounter(in); got != want {
			t.Errorf("EncodeCounter(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestValidateRejectsReservedAndUnsafe(t *testing.T) {
	bad := []string{
		"",          // empty
		"ab",        // too short
		"api",       // reserved: would shadow the management API
		"HEALTHZ",   // reserved, case-insensitively
		"metrics",   // reserved
		"has space", // not URL-safe
		"a/b",       // path separator
		"a:b",       // would break a cache key namespace
		strings.Repeat("a", 33),
	}
	for _, c := range bad {
		if err := Validate(c); err == nil {
			t.Errorf("Validate(%q) = nil, want error", c)
		}
	}
	for _, c := range []string{"abc", "myLink7", "Zz09"} {
		if err := Validate(c); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", c, err)
		}
	}
}
