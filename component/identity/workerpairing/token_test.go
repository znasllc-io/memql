package workerpairing

import (
	"strings"
	"testing"
)

func TestMintShape(t *testing.T) {
	plain, hash, err := Mint()
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if len(plain) != 9 {
		t.Fatalf("expected 9-char plain (XXXX-XXXX), got %q (len %d)", plain, len(plain))
	}
	if plain[4] != '-' {
		t.Fatalf("expected hyphen at index 4, got %q", plain)
	}
	if hash == "" {
		t.Fatal("hash empty")
	}
	if hash != Hash(plain) {
		t.Fatal("hash mismatch")
	}
}

func TestMintIsUnique(t *testing.T) {
	seen := map[string]struct{}{}
	for i := 0; i < 200; i++ {
		plain, _, err := Mint()
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		if _, dup := seen[plain]; dup {
			t.Fatalf("duplicate code after %d iterations", i)
		}
		seen[plain] = struct{}{}
	}
}

func TestCanonicalizeAcceptsSloppyInput(t *testing.T) {
	// Reference code: "ABCD-EFGH" -- canonicalizes to itself.
	canon := "ABCD-EFGH"
	cases := []string{
		"ABCD-EFGH",
		"ABCDEFGH",
		"abcd-efgh",
		"  ABCD-EFGH  ",
		"abcd efgh",
		"ABCD_EFGH",
	}
	for _, in := range cases {
		got := Canonicalize(in)
		if got != canon {
			t.Errorf("Canonicalize(%q) = %q, want %q", in, got, canon)
		}
	}
}

func TestCanonicalizeRejectsAmbiguousAndShort(t *testing.T) {
	cases := []string{
		"ABCD-EF",       // too short
		"ABCDEFGHIJ",    // too long
		"ABCD-EF0H",     // 0 not in alphabet
		"ABCD-EF1H",     // 1 not in alphabet
		"ABCD-EFIH",     // I not in alphabet
		"ABCD-EFOH",     // O not in alphabet
		"",
		"-",
	}
	for _, in := range cases {
		if got := Canonicalize(in); got != "" {
			t.Errorf("Canonicalize(%q) = %q, want empty", in, got)
		}
	}
}

func TestHashStableAcrossSloppyInput(t *testing.T) {
	plain := "ABCD-EFGH"
	if Hash("abcdefgh") != Hash(plain) {
		t.Fatal("Hash should normalize")
	}
	if Hash("  abcd efgh  ") != Hash(plain) {
		t.Fatal("Hash should normalize whitespace")
	}
	if Hash("") != "" {
		t.Fatal("Hash on empty should be empty")
	}
}

func TestAlphabetIsThirtyTwoUnambiguous(t *testing.T) {
	if len(codeAlphabet) != 32 {
		t.Fatalf("alphabet should be 32 chars, got %d", len(codeAlphabet))
	}
	for _, banned := range []rune{'0', '1', 'I', 'O'} {
		if strings.ContainsRune(codeAlphabet, banned) {
			t.Errorf("alphabet contains banned ambiguous char %q", banned)
		}
	}
}
