package devicecode

import (
	"math"
	"strings"
	"testing"
)

// The user_code is the only part of this grant a human has to move by
// hand, so its two properties -- an alphabet with nothing confusable in
// it, and an input path that forgives everything except a wrong code --
// are what these tests pin.

func TestUserCodeAlphabetHasNoConfusablePairs(t *testing.T) {
	// The literal requirement: neither member of the 0/O pair and
	// neither member of the 1/I pair may appear. Removing BOTH is what
	// makes an aliasing pass unnecessary -- if only one were dropped, a
	// reader who saw the other would still have to guess.
	for _, banned := range []rune{'0', 'O', '1', 'I'} {
		if strings.ContainsRune(userCodeAlphabet, banned) {
			t.Fatalf("alphabet contains the ambiguous character %q: %s", banned, userCodeAlphabet)
		}
	}
	// No duplicates -- a repeated symbol would silently cost entropy.
	seen := map[rune]bool{}
	for _, c := range userCodeAlphabet {
		if seen[c] {
			t.Fatalf("alphabet repeats %q: %s", c, userCodeAlphabet)
		}
		seen[c] = true
		if c < '2' || (c > '9' && c < 'A') || c > 'Z' {
			t.Fatalf("alphabet contains a non-uppercase-alphanumeric symbol %q", c)
		}
	}
}

func TestUserCodeCarriesAtLeast40Bits(t *testing.T) {
	bits := float64(UserCodeChars) * math.Log2(float64(len(userCodeAlphabet)))
	if bits < 40 {
		t.Fatalf("user_code entropy = %.2f bits from %d symbols of a %d-char alphabet; want >= 40",
			bits, UserCodeChars, len(userCodeAlphabet))
	}
	// 256 must be an exact multiple of the alphabet size, or the
	// modulo in MintUserCode biases toward the low symbols and the
	// entropy computed above is an overestimate.
	if 256%len(userCodeAlphabet) != 0 {
		t.Fatalf("alphabet size %d does not divide 256; the modulo in MintUserCode would be biased",
			len(userCodeAlphabet))
	}
}

func TestMintUserCodeShape(t *testing.T) {
	for i := 0; i < 200; i++ {
		display, hash, err := MintUserCode()
		if err != nil {
			t.Fatalf("MintUserCode: %v", err)
		}
		if hash == "" || len(hash) != 64 {
			t.Fatalf("hash = %q, want a 64-char sha256 hex digest", hash)
		}
		// Two readable groups, separated.
		parts := strings.Split(display, UserCodeSeparator)
		if len(parts) != 2 || len(parts[0]) != UserCodeChars/2 || len(parts[1]) != UserCodeChars/2 {
			t.Fatalf("display form = %q, want two groups of %d", display, UserCodeChars/2)
		}
		for _, c := range parts[0] + parts[1] {
			if !strings.ContainsRune(userCodeAlphabet, c) {
				t.Fatalf("minted code %q contains off-alphabet character %q", display, c)
			}
		}
		// The digest must be over the canonical form, so that a user
		// who omits the separator still hits the same row.
		if got := HashUserCode(strings.ReplaceAll(display, UserCodeSeparator, "")); got != hash {
			t.Fatalf("hash of the separator-less form differs from the minted hash")
		}
	}
}

func TestCanonicalizeUserCodeAcceptsSloppyInput(t *testing.T) {
	// One canonical answer, reached from every spelling a human plausibly
	// produces.
	const canonical = "ABCD2345"
	for _, in := range []string{
		"ABCD-2345",
		"abcd-2345",
		"AbCd-2345",
		"ABCD2345",
		"abcd2345",
		"  ABCD-2345  ",
		"ABCD 2345",
		"ABCD_2345",
		"a b c d 2 3 4 5",
	} {
		if got := CanonicalizeUserCode(in); got != canonical {
			t.Fatalf("CanonicalizeUserCode(%q) = %q, want %q", in, got, canonical)
		}
	}
	// And the same lookup key falls out of each.
	want := HashUserCode(canonical)
	if want == "" {
		t.Fatal("HashUserCode of a valid canonical code returned empty")
	}
	for _, in := range []string{"abcd-2345", "ABCD2345", " AbCd_2345 "} {
		if got := HashUserCode(in); got != want {
			t.Fatalf("HashUserCode(%q) differs from the canonical hash", in)
		}
	}
}

func TestCanonicalizeUserCodeRejectsBadInput(t *testing.T) {
	for _, in := range []string{
		"",
		"ABCD",           // too short
		"ABCD-23456",     // too long
		"ABCD-234O",      // 'O' is not in the alphabet
		"ABCD-234I",      // nor 'I'
		"ABC0-2345",      // nor '0'
		"ABC1-2345",      // nor '1'
		"ABCD-23!5",      // punctuation that is not a separator
		"ABCD-2345-6789", // two separators, wrong length
		"АBCD-2345",      // Cyrillic A: looks right, is not in the alphabet
	} {
		if got := CanonicalizeUserCode(in); got != "" {
			t.Fatalf("CanonicalizeUserCode(%q) = %q, want a rejection", in, got)
		}
		if got := HashUserCode(in); got != "" {
			t.Fatalf("HashUserCode(%q) = %q, want empty for an invalid code", in, got)
		}
	}
}

func TestMintDeviceCodeIsPrefixedAndHashed(t *testing.T) {
	plain, hash, err := MintDeviceCode()
	if err != nil {
		t.Fatalf("MintDeviceCode: %v", err)
	}
	if !strings.HasPrefix(plain, DeviceCodePrefix) {
		t.Fatalf("device_code %q lacks the %q prefix", plain, DeviceCodePrefix)
	}
	body := strings.TrimPrefix(plain, DeviceCodePrefix)
	if len(body) != 43 { // base64url, no padding, of 32 bytes
		t.Fatalf("device_code body length = %d, want 43", len(body))
	}
	if len(hash) != 64 {
		t.Fatalf("hash = %q, want a 64-char sha256 hex digest", hash)
	}
	if strings.Contains(hash, body) {
		t.Fatal("the digest contains the plaintext body")
	}
	// Trimming is the only normalization: a case fold would throw away
	// entropy on a machine-held secret.
	if HashDeviceCode("  "+plain+"\n") != hash {
		t.Fatal("surrounding whitespace changed the device_code digest")
	}
	if HashDeviceCode(strings.ToUpper(plain)) == hash && strings.ToUpper(plain) != plain {
		t.Fatal("device_code hashing is case-insensitive; it must not be")
	}
	if HashDeviceCode("") != "" {
		t.Fatal("HashDeviceCode(\"\") must return empty rather than the digest of the empty string")
	}
}

func TestRowIntervalIsDefaultedAndClamped(t *testing.T) {
	cases := []struct {
		stored int
		want   int
	}{
		{stored: 0, want: DefaultIntervalSeconds},
		{stored: -3, want: DefaultIntervalSeconds},
		{stored: 7, want: 7},
		{stored: MaxIntervalSeconds + 100, want: MaxIntervalSeconds},
	}
	for _, c := range cases {
		if got := (Row{IntervalSeconds: c.stored}).Interval(); got != c.want {
			t.Fatalf("Row{IntervalSeconds:%d}.Interval() = %d, want %d", c.stored, got, c.want)
		}
	}
}
