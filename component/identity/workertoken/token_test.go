package workertoken

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
		t.Fatalf("plain missing prefix %q: got %q", TokenPrefix, plain)
	}
	if len(plain) != len(TokenPrefix)+43 {
		t.Fatalf("expected %d chars (prefix + 32-byte b64url), got %d", len(TokenPrefix)+43, len(plain))
	}
	if hash == "" {
		t.Fatal("hash is empty")
	}
	if hash != Hash(plain) {
		t.Fatal("hash mismatch between Mint and Hash")
	}
}

func TestMintIsUnique(t *testing.T) {
	seen := map[string]struct{}{}
	for i := 0; i < 100; i++ {
		plain, _, err := Mint()
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		if _, dup := seen[plain]; dup {
			t.Fatalf("Mint produced duplicate token after %d iterations", i)
		}
		seen[plain] = struct{}{}
	}
}

func TestHashIsStable(t *testing.T) {
	plain := TokenPrefix + "abcdef0123456789abcdef0123456789abcdef0123"
	first := Hash(plain)
	second := Hash(plain)
	if first != second {
		t.Fatalf("Hash unstable: %q vs %q", first, second)
	}
	if Hash("") != "" {
		t.Fatal("Hash on empty input should be empty")
	}
}

func TestIsWorkerToken(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"mql_wkr_abc", true},
		{"   mql_wkr_abc   ", true},
		{"mql_pat_abc", false},
		{"", false},
		{"foobar", false},
	}
	for _, c := range cases {
		if got := IsWorkerToken(c.in); got != c.want {
			t.Errorf("IsWorkerToken(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
