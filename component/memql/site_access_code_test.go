package memql

import (
	"encoding/base64"
	"regexp"
	"strings"
	"testing"
	"time"
)

// site_access_code_test.go -- the access code, how people type it, the session
// token it trades for, and the two time rules around them (deployable access
// codes, decisions D1, D2, D8 and D10 of the design record).
//
// A code is a BEARER CREDENTIAL that one person hands to another: it is read
// aloud, pasted from an email and retyped. Every assertion below is a property
// of that sentence -- it has to survive being typed by a human, it has to be
// unguessable, and what is stored has to be useless to whoever reads it.

var (
	// The format in the design record, spelled out here rather than derived from
	// the implementation's alphabet, so a drift in either shows against the other.
	accessCodeShape = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{5}(-[0-9A-HJKMNP-TV-Z]{5}){4}$`)

	// A stored digest is lowercase hex SHA-256 and is compared as a string.
	lowerHexSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Two codes written the way the normalizer is meant to leave them, with the
// SHA-256 of each worked out OUTSIDE this package. The digests are pinned
// because every code in a database is stored as this function's output: a
// change to WHAT is hashed (hyphens left in, case kept) would make every code
// ever minted stop opening anything, and nothing else would notice.
const (
	// The design record's example code.
	exampleAccessCode       = "K7Q4M2XPDN8RT3VWJ6HC9FBZA"
	exampleAccessCodeDigest = "5d019d0e2df8e90faa86ef5b169bfc4980778282fa6ad3b7128d1752118cf948"

	// A code that carries a zero and a one, the two digits people mistake O, I
	// and L for.
	lookalikeAccessCode       = "0123456789ABCDEFGHJKMNPQR"
	lookalikeAccessCodeDigest = "d6213dc13e91424d6329234d774aeeaff252696a15c5690c30deeece46907694"
)

func TestMintAccessCodeShape(t *testing.T) {
	code, digest, hint, err := MintAccessCode()
	if err != nil {
		t.Fatalf("MintAccessCode: %v", err)
	}
	if !accessCodeShape.MatchString(code) {
		t.Fatalf("the code %q is not five hyphenated groups of five Crockford base32 symbols -- the "+
			"shape people read aloud and retype, with no I, L, O or U to be mistaken for something else", code)
	}
	symbols := strings.ReplaceAll(code, "-", "")
	if want := symbols[len(symbols)-4:]; hint != want {
		t.Errorf("hint = %q, want the last four symbols of the code, %q -- it is what lets an "+
			"operator tell two codes apart in a list", hint, want)
	}
	if digest != AccessCodeDigest(code) {
		t.Errorf("the digest returned beside the code is not the code's digest -- a row stored " +
			"under it would never be found when the code is presented")
	}
	if !lowerHexSHA256.MatchString(digest) {
		t.Errorf("the digest %q is not lowercase hex SHA-256 -- stored and presented digests are "+
			"compared as strings", digest)
	}
}

// WHAT PEOPLE DO TO A CODE BETWEEN BEING READ AND BEING TYPED must not read as a
// wrong code, and nothing beyond that may read as the right one. The first half
// is the reason for the format; the second is what keeps "forgiving" from
// becoming "sloppy" -- an input that matches by dropping what it should have
// refused is a wider door than the 125 bits behind it suggest.
func TestNormalizeAccessCodeForgivesHowPeopleTypeIt(t *testing.T) {
	for name, typed := range map[string]string{
		"as displayed":                            "K7Q4M-2XPDN-8RT3V-WJ6HC-9FBZA",
		"lower case":                              "k7q4m-2xpdn-8rt3v-wj6hc-9fbza",
		"no separators at all":                    "K7Q4M2XPDN8RT3VWJ6HC9FBZA",
		"spaces where the hyphens were":           "K7Q4M 2XPDN 8RT3V WJ6HC 9FBZA",
		"padded, mixed and wrapped across a line": "  k7q4m-2xpdn 8rt3v\nWJ6HC-9FBZA ",
		"tabs and a trailing CRLF":                "\tK7Q4M-2XPDN-8RT3V-WJ6HC-9FBZA\r\n",
		"a non-breaking space from a web page":    "K7Q4M-2XPDN\u00a08RT3V-WJ6HC-9FBZA",
	} {
		t.Run(name, func(t *testing.T) {
			if got := NormalizeAccessCode(typed); got != exampleAccessCode {
				t.Errorf("NormalizeAccessCode(%q) = %q, want %q", typed, got, exampleAccessCode)
			}
			if got := AccessCodeDigest(typed); got != exampleAccessCodeDigest {
				t.Errorf("AccessCodeDigest(%q) = %q, want the digest of the canonical spelling -- "+
					"this spelling would read as a wrong code", typed, got)
			}
		})
	}

	// O is read as zero, and I and L as one, in either case. The canonical form
	// has a zero and a one, so the mapping is exercised in the direction it runs.
	for name, typed := range map[string]string{
		"capital O for zero, capital I for one": "OI23456789ABCDEFGHJKMNPQR",
		"capital O for zero, capital L for one": "OL23456789ABCDEFGHJKMNPQR",
		"lower-case o and l":                    "ol23456789abcdefghjkmnpqr",
		"lower-case o and i":                    "oi23456789abcdefghjkmnpqr",
		"the digits themselves":                 lookalikeAccessCode,
	} {
		t.Run(name, func(t *testing.T) {
			if got := NormalizeAccessCode(typed); got != lookalikeAccessCode {
				t.Errorf("NormalizeAccessCode(%q) = %q, want %q", typed, got, lookalikeAccessCode)
			}
			if got := AccessCodeDigest(typed); got != lookalikeAccessCodeDigest {
				t.Errorf("AccessCodeDigest(%q) differs from the digest of the canonical spelling -- "+
					"a code read aloud with an O for its zero would be refused", typed)
			}
		})
	}

	// Nothing past those spellings is forgiven.
	for name, typed := range map[string]string{
		"empty":                         "",
		"a stray mark between symbols":  "K7Q4M#2XPDN8RT3VWJ6HC9FBZA",
		"U is not V":                    "K7Q4M2XPDN8RT3UWJ6HC9FBZA",
		"a different last symbol":       "K7Q4M-2XPDN-8RT3V-WJ6HC-9FBZB",
		"a truncated code":              "K7Q4M-2XPDN-8RT3V-WJ6HC-9FBZ",
		"a code with a symbol too many": "K7Q4M-2XPDN-8RT3V-WJ6HC-9FBZAA",
		"a dotless i is not a one":      "0\u0131" + "23456789ABCDEFGHJKMNPQR",
	} {
		t.Run("not forgiven: "+name, func(t *testing.T) {
			got := AccessCodeDigest(typed)
			if got == exampleAccessCodeDigest || got == lookalikeAccessCodeDigest {
				t.Errorf("AccessCodeDigest(%q) matched a real code -- an input that is not a spelling "+
					"of the code must not open it", typed)
			}
		})
	}

	// Case is folded for ASCII only. strings.ToUpper would turn a dotless i into
	// I, and so into a one, and a long s into S: characters that are not among the
	// documented spellings of any symbol would start to open the door.
	for _, letter := range []string{"\u0131", "\u017f"} {
		if got := NormalizeAccessCode(letter); got != letter {
			t.Errorf("NormalizeAccessCode(%q) = %q, want it left alone -- only ASCII letters are folded", letter, got)
		}
	}
}

// TWO MINTS DIFFER. A code that repeated would be one client's engagement
// opening for another's, and crypto/rand is only as good as the call.
func TestMintAccessCodeIsNotRepeatable(t *testing.T) {
	const mints = 1000
	codes := make(map[string]bool, mints)
	digests := make(map[string]bool, mints)
	for i := 0; i < mints; i++ {
		code, digest, _, err := MintAccessCode()
		if err != nil {
			t.Fatalf("MintAccessCode: %v", err)
		}
		if codes[code] {
			t.Fatalf("mint %d repeated the code %q", i, code)
		}
		if digests[digest] {
			t.Fatalf("mint %d repeated the digest %q", i, digest)
		}
		codes[code], digests[digest] = true, true
	}
}

// EVERY SYMBOL COMES UP, AND NONE COMES UP MUCH MORE THAN ANOTHER. The draw
// turns a random byte into one of 32 symbols, and its failure is a quiet one: a
// symbol that never appears, or that appears more often, is not a wrong code, it
// is a code with fewer than 125 bits that nobody can see.
//
// The band is wide on purpose. 4000 mints are 100,000 symbols, about 3,125 of
// each with a spread near 55, so plus or minus 15 percent sits more than eight
// spreads out and will not trip by chance. It exists for the gross failures: a
// symbol dropped, or a draw that favours half the alphabet.
func TestMintAccessCodeDrawsEverySymbolEvenly(t *testing.T) {
	const mints = 4000
	counts := map[rune]int{}
	for i := 0; i < mints; i++ {
		code, _, _, err := MintAccessCode()
		if err != nil {
			t.Fatalf("MintAccessCode: %v", err)
		}
		for _, r := range strings.ReplaceAll(code, "-", "") {
			counts[r]++
		}
	}
	drawn := mints * accessCodeSymbols
	mean := drawn / len(accessCodeAlphabet)
	for _, r := range accessCodeAlphabet {
		if n := counts[r]; n < mean*85/100 || n > mean*115/100 {
			t.Errorf("symbol %q came up %d times in %d symbols, want about %d -- a draw that does "+
				"not use the alphabet evenly gives a code fewer than 125 bits", r, n, drawn, mean)
		}
	}
}

// THE ALPHABET IS TIED TO TWO OTHER THINGS, and each tie fails silently.
//
// The draw masks five bits of a random byte, which is exact only for 32 symbols:
// 31 or 36 would make some symbols likelier than others. And the normalizer
// rewrites O, I and L, so a minted code that carried one would be rewritten when
// typed back and would never match its own digest -- an operator would be handed
// a code that is wrong from the moment it is shown.
func TestAccessCodeAlphabetIsSafeToDrawAndToNormalize(t *testing.T) {
	if len(accessCodeAlphabet) != 32 {
		t.Fatalf("the alphabet has %d symbols, want 32 -- the draw masks five bits of a random byte, "+
			"which is unbiased only for exactly 32", len(accessCodeAlphabet))
	}
	seen := map[rune]bool{}
	for _, r := range accessCodeAlphabet {
		if seen[r] {
			t.Errorf("the symbol %q appears twice in the alphabet, so it is drawn twice as often", r)
		}
		seen[r] = true
		if strings.ContainsRune("ILOU", r) {
			t.Errorf("the alphabet carries %q, which is not a Crockford base32 symbol", r)
		}
		if got := NormalizeAccessCode(string(r)); got != string(r) {
			t.Errorf("the symbol %q normalizes to %q -- a minted code carrying it would be rewritten "+
				"when typed back and could never match its own digest", r, got)
		}
	}
}

func TestMintAccessSessionToken(t *testing.T) {
	token, digest, err := MintAccessSessionToken()
	if err != nil {
		t.Fatalf("MintAccessSessionToken: %v", err)
	}
	if !strings.HasPrefix(token, "mql_acs_") {
		t.Fatalf("the token carries no family prefix: %q -- a prefixed token is greppable in a leak "+
			"scan and unmistakable in a support transcript", token)
	}
	body := strings.TrimPrefix(token, "mql_acs_")
	if len(body) != 43 {
		t.Errorf("the token has %d characters after its prefix, want 43", len(body))
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		t.Fatalf("the token body is not unpadded base64url: %v", err)
	}
	if len(raw) != 32 {
		t.Errorf("the token carries %d bytes of entropy, want 32 -- the same 256 bits every other "+
			"token in the mql_ family carries", len(raw))
	}
	if digest != AccessSessionTokenDigest(token) {
		t.Errorf("the digest returned beside the token is not the token's digest")
	}
	if !lowerHexSHA256.MatchString(digest) {
		t.Errorf("the digest %q is not lowercase hex SHA-256", digest)
	}

	// THE WHOLE STRING IS HASHED, PREFIX INCLUDED, so the suffix alone cannot
	// match a stored digest.
	if digest == AccessSessionTokenDigest(body) {
		t.Error("the digest of the token equals the digest of its body -- the prefix is not hashed")
	}

	// WHITESPACE IS TRIMMED BEFORE HASHING. A token travels in a cookie and
	// occasionally through a copy-paste; a trailing newline reading as a wrong
	// credential is a refusal nobody could diagnose from either side.
	for _, padded := range []string{token + "\n", " " + token, "\t" + token + " \r\n"} {
		if got := AccessSessionTokenDigest(padded); got != digest {
			t.Errorf("AccessSessionTokenDigest(%q) differs from the unpadded token's digest", padded)
		}
	}

	// The digest of a fixed token is pinned, worked out outside this package:
	// it is what is stored, so changing what is hashed orphans every session.
	const fixed = "mql_acs_" + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	const fixedDigest = "e89a632bb9c22f14fb7b174fd9a54b255d90e62e3938224f74fceb9f0f541b83"
	if got := AccessSessionTokenDigest(fixed); got != fixedDigest {
		t.Errorf("AccessSessionTokenDigest(%q) = %q, want %q", fixed, got, fixedDigest)
	}

	// Two mints differ.
	other, otherDigest, err := MintAccessSessionToken()
	if err != nil {
		t.Fatalf("MintAccessSessionToken: %v", err)
	}
	if other == token || otherDigest == digest {
		t.Errorf("two mints produced the same token or digest: %q", token)
	}
}

// EVERY CODE ENDS, AND THE BOUNDS ARE THE ONLY ONES IT HAS. More than a minute
// from now and at most 366 days; the zero time is refused as already past, so
// an unset expiry can never be read as "no expiry".
//
// `now` is a fixed instant, not the wall clock, so the answer cannot depend on
// the clock of the machine running the test: the function judges against the
// clock it is handed, which in production is the engine's.
func TestValidateAccessCodeExpiry(t *testing.T) {
	now := time.Date(2026, time.October, 29, 12, 0, 0, 0, time.UTC)
	const day = 24 * time.Hour

	for _, tc := range []struct {
		name      string
		expiresAt time.Time
		// A phrase the sentence shown to the operator must carry, naming the
		// bound that was broken. Empty means the expiry is accepted.
		mention string
	}{
		{"59 seconds is too soon", now.Add(59 * time.Second), "1 minute"},
		{"exactly a minute is still not more than a minute", now.Add(time.Minute), "1 minute"},
		{"61 seconds is accepted", now.Add(61 * time.Second), ""},
		{"ten minutes, the shortest preset", now.Add(10 * time.Minute), ""},
		{"thirty days, the longest preset", now.Add(30 * day), ""},
		{"exactly 366 days is accepted", now.Add(366 * day), ""},
		{"a nanosecond past 366 days is refused", now.Add(366*day + 1), "366 days"},
		{"367 days is refused", now.Add(367 * day), "366 days"},
		{"the year 9999 is refused, not overflowed", time.Date(9999, time.December, 31, 0, 0, 0, 0, time.UTC), "366 days"},
		{"this very instant", now, "already passed"},
		{"an hour ago", now.Add(-time.Hour), "already passed"},
		{"a year ago", now.Add(-366 * day), "already passed"},
		{"the zero time, which is how an unset expiry arrives", time.Time{}, "already passed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateAccessCodeExpiry(now, tc.expiresAt)
			if tc.mention == "" {
				if err != nil {
					t.Fatalf("ValidateAccessCodeExpiry refused an expiry it should accept: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateAccessCodeExpiry accepted %s -- a code with that expiry is either "+
					"dead on arrival or outlives the bound a bearer credential must have", tc.expiresAt)
			}
			if !strings.Contains(err.Error(), tc.mention) {
				t.Errorf("the refusal %q does not name the bound (%q) -- the operator is shown this "+
					"sentence as written and has to be able to act on it", err.Error(), tc.mention)
			}
		})
	}
}

// A SESSION ENDS AT THE EARLIER OF TWO LIMITS: the code's expiry, and a day
// after the session opened. A short code must not be outlived by its session,
// and a long one must not produce a session that never ends.
func TestAccessSessionEnd(t *testing.T) {
	issued := time.Date(2026, time.October, 29, 12, 0, 0, 0, time.UTC)
	const day = 24 * time.Hour

	for _, tc := range []struct {
		name          string
		codeExpiresAt time.Time
		want          time.Time
	}{
		{"a ten-minute code ends the session with the code", issued.Add(10 * time.Minute), issued.Add(10 * time.Minute)},
		{"a seven-day code ends the session at 24 hours", issued.Add(7 * day), issued.Add(day)},
		{"a code that ends exactly at 24 hours", issued.Add(day), issued.Add(day)},
		{"a nanosecond under 24 hours ends with the code", issued.Add(day - 1), issued.Add(day - 1)},
		{"a nanosecond over 24 hours is capped at 24", issued.Add(day + 1), issued.Add(day)},
		{"a code that had already expired when the session opened", issued.Add(-time.Minute), issued.Add(-time.Minute)},
		// Fails closed: a code with no expiry must end the session, not be
		// read as "nothing to cap it" and hand back a full day.
		{"a code with no expiry ends the session at once", time.Time{}, time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := AccessSessionEnd(issued, tc.codeExpiresAt); !got.Equal(tc.want) {
				t.Errorf("AccessSessionEnd(%s, %s) = %s, want %s", issued, tc.codeExpiresAt, got, tc.want)
			}
		})
	}
}
