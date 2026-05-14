package pat

import (
	"strings"
	"testing"
)

func TestMintProducesPrefixedToken(t *testing.T) {
	plain, hash, err := Mint()
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if !strings.HasPrefix(plain, TokenPrefix) {
		t.Errorf("Mint plaintext missing prefix: %q", plain)
	}
	if !IsPATToken(plain) {
		t.Errorf("IsPATToken should be true for minted token")
	}
	if hash == "" {
		t.Errorf("Mint hash should not be empty")
	}
	if hash == plain {
		t.Errorf("hash should differ from plaintext")
	}
	if Hash(plain) != hash {
		t.Errorf("Hash(plain) should equal Mint hash; got %q vs %q", Hash(plain), hash)
	}
}

func TestMintProducesUniqueTokens(t *testing.T) {
	a, _, err := Mint()
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := Mint()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Errorf("two Mint() calls produced identical tokens: %q", a)
	}
}

func TestIsPATToken(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"empty", "", false},
		{"jwt-shape", "eyJhbGciOiJFZERTQSIsImtpZCI6ImFiYyJ9.payload.sig", false},
		{"prefixed", TokenPrefix + "abc123", true},
		{"prefix only", TokenPrefix, true},
		{"trimmed leading space", "  " + TokenPrefix + "abc", true},
		{"wrong prefix", "mql_pat" + "abc", false}, // no trailing underscore
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsPATToken(tc.in); got != tc.want {
				t.Errorf("IsPATToken(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestHashEmpty(t *testing.T) {
	if got := Hash(""); got != "" {
		t.Errorf("Hash(\"\") should be empty, got %q", got)
	}
}

func TestHashStable(t *testing.T) {
	a := Hash("mql_pat_abc123")
	b := Hash("mql_pat_abc123")
	if a != b {
		t.Errorf("Hash should be stable across calls")
	}
	if len(a) != 64 { // sha256 hex digest length
		t.Errorf("Hash should be 64 hex chars, got %d", len(a))
	}
}
