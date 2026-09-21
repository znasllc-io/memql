package memql

import (
	"encoding/base64"
	"net/url"
	"strings"
	"testing"
	"time"
)

// site_preview_token_test.go -- the preview credential and the wire spellings
// around it (epic memql#5531, issue memql#5545).
//
// A preview URL is a BEARER CREDENTIAL for an unpublished version of somebody's
// storefront, and every assertion below is a property of that sentence.

func TestAMintedPreviewTokenIsWellFormed(t *testing.T) {
	token, digest, err := MintPreviewToken()
	if err != nil {
		t.Fatalf("MintPreviewToken: %v", err)
	}
	if !strings.HasPrefix(token, "mql_prv_") {
		t.Errorf("the token carries no family prefix: %q -- a prefixed token is what makes "+
			"one greppable in a leak scan and unmistakable in a support transcript", token)
	}
	if !WellFormedPreviewToken(token) {
		t.Errorf("a freshly minted token is not well formed: %q", token)
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(token, PreviewTokenPrefix))
	if err != nil {
		t.Fatalf("the token body is not unpadded base64url: %v", err)
	}
	if len(raw) != 32 {
		t.Errorf("the token carries %d bytes of entropy, want 32 -- the same 256 bits every "+
			"other token in the mql_ family carries", len(raw))
	}
	if digest != PreviewTokenDigest(token) {
		t.Errorf("the digest returned beside the token is not the token's digest")
	}
}

// TWO MINTS DIFFER. A token that repeated would be a preview link that opened
// somebody else's candidate, and crypto/rand is only as good as the call.
func TestTwoMintedPreviewTokensDiffer(t *testing.T) {
	a, da, err := MintPreviewToken()
	if err != nil {
		t.Fatalf("MintPreviewToken: %v", err)
	}
	b, db, err := MintPreviewToken()
	if err != nil {
		t.Fatalf("MintPreviewToken: %v", err)
	}
	if a == b {
		t.Fatalf("two mints produced the same token: %q", a)
	}
	if da == db {
		t.Fatalf("two mints produced the same digest: %q", da)
	}
}

func TestThePreviewTokenDigestIsStableLowercaseHex(t *testing.T) {
	const token = "mql_prv_" + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	first := PreviewTokenDigest(token)
	if first != PreviewTokenDigest(token) {
		t.Fatal("the digest of one string is not stable, so no stored digest could ever match")
	}
	if len(first) != 64 {
		t.Errorf("the digest is %d characters, want 64 (hex SHA-256)", len(first))
	}
	if first != strings.ToLower(first) {
		t.Errorf("the digest is not lowercase: %q -- the stored form and the looked-up form "+
			"are compared as strings", first)
	}
	for _, c := range first {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Fatalf("the digest carries a non-hex character %q: %q", c, first)
		}
	}
}

// THE WHOLE STRING IS HASHED, PREFIX INCLUDED, so a caller cannot present the
// suffix alone and match a stored digest. Hashing the body only would make the
// family prefix decorative -- and would admit a credential that is 43
// characters of somebody else's token family.
func TestThePreviewTokenDigestCoversThePrefix(t *testing.T) {
	token, _, err := MintPreviewToken()
	if err != nil {
		t.Fatalf("MintPreviewToken: %v", err)
	}
	body := strings.TrimPrefix(token, PreviewTokenPrefix)
	if body == token {
		t.Fatal("the minted token carries no prefix, so the comparison below proves nothing")
	}
	if PreviewTokenDigest(token) == PreviewTokenDigest(body) {
		t.Error("the digest of the token equals the digest of its body -- the prefix is not hashed, " +
			"so presenting the suffix alone would match a stored digest")
	}
}

// WHITESPACE IS TRIMMED BEFORE HASHING. A token travels through a URL, a cookie
// and occasionally a copy-paste; a trailing newline reading as a wrong
// credential is a refusal nobody could diagnose from either side.
func TestPreviewTokenWhitespaceIsTrimmedBeforeHashing(t *testing.T) {
	token, _, err := MintPreviewToken()
	if err != nil {
		t.Fatalf("MintPreviewToken: %v", err)
	}
	want := PreviewTokenDigest(token)
	for _, padded := range []string{token + "\n", " " + token, "\t" + token + " \r\n"} {
		if got := PreviewTokenDigest(padded); got != want {
			t.Errorf("PreviewTokenDigest(%q) differs from the unpadded token's digest", padded)
		}
	}
}

func TestWellFormedPreviewTokenRejectsWhatIsNotOne(t *testing.T) {
	good, _, err := MintPreviewToken()
	if err != nil {
		t.Fatalf("MintPreviewToken: %v", err)
	}
	// The reachable positive, so a function that rejected everything could not
	// pass this test vacuously.
	if !WellFormedPreviewToken(good) {
		t.Fatalf("a minted token is rejected: %q", good)
	}

	body := strings.TrimPrefix(good, PreviewTokenPrefix)
	for name, bad := range map[string]string{
		"empty":               "",
		"whitespace only":     "   ",
		"no prefix":           body,
		"another family":      "mql_pat_" + body,
		"too short":           PreviewTokenPrefix + body[:40],
		"too long":            PreviewTokenPrefix + body + "AAAA",
		"not base64url":       PreviewTokenPrefix + strings.Repeat("!", 43),
		"standard base64 pad": PreviewTokenPrefix + base64.StdEncoding.EncodeToString(make([]byte, 32)),
		"prefix alone":        PreviewTokenPrefix,
	} {
		if WellFormedPreviewToken(bad) {
			t.Errorf("%s was accepted as a well-formed preview token: %q", name, bad)
		}
	}
}

// A NON-POSITIVE TTL IS THE DEFAULT, NOT "NO EXPIRY", AND THAT IS THE TRAP.
// Reading an operator's typo or an unset variable as unlimited would remove
// exactly the bound a bearer credential sitting in a URL exists to have.
func TestClampPreviewGrantTTL(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{"zero is the default, never unlimited", 0, DefaultPreviewGrantTTL},
		{"a negative value is the default, never unlimited", -time.Hour, DefaultPreviewGrantTTL},
		{"one nanosecond under zero is still the default", -1, DefaultPreviewGrantTTL},
		{"an in-range value passes through", 90 * time.Minute, 90 * time.Minute},
		{"the default passes through", DefaultPreviewGrantTTL, DefaultPreviewGrantTTL},
		{"the maximum passes through", MaxPreviewGrantTTL, MaxPreviewGrantTTL},
		{"over the maximum clamps to the maximum", 24 * time.Hour, MaxPreviewGrantTTL},
		{"one nanosecond over the maximum clamps", MaxPreviewGrantTTL + 1, MaxPreviewGrantTTL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClampPreviewGrantTTL(tc.in); got != tc.want {
				t.Errorf("ClampPreviewGrantTTL(%s) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}

	// Said plainly, because "zero means forever" is the reading this function
	// exists to refuse and a table reads past it.
	if ClampPreviewGrantTTL(0) == 0 {
		t.Error("a zero TTL was returned unchanged -- an unexpiring preview grant is a bearer " +
			"credential for an unpublished storefront with no bound at all")
	}
}

// THE LINK IS ON THE SITE'S OWN ORIGIN, OVER https, WITH THE TOKEN ONCE.
// The origin is a requirement rather than a convenience: a storefront's
// cookies, its cart and Shopify's checkout return all assume it.
func TestPreviewEnterURL(t *testing.T) {
	const token = "mql_prv_TOKEN"
	got := PreviewEnterURL("Shop.Example.COM", token)

	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("PreviewEnterURL produced an unparseable URL %q: %v", got, err)
	}
	if u.Scheme != "https" {
		t.Errorf("scheme = %q, want https -- a preview token is a credential and the cookie "+
			"the edge sets is Secure", u.Scheme)
	}
	if u.Host != "shop.example.com" {
		t.Errorf("host = %q, want the lowercased hostname", u.Host)
	}
	if u.Path != PreviewEnterPath {
		t.Errorf("path = %q, want %q", u.Path, PreviewEnterPath)
	}
	if got := u.Query().Get(PreviewGrantParam); got != token {
		t.Errorf("the %s parameter carries %q, want the token", PreviewGrantParam, token)
	}
	// The token appears in the query and nowhere else: it is in the address bar
	// for one navigation and a cookie thereafter.
	if strings.Contains(u.Path, token) {
		t.Errorf("the token leaked into the path: %q", got)
	}
}

func TestPreviewEnterURLTrimsWhatItIsHanded(t *testing.T) {
	got := PreviewEnterURL("  shop.example.com  ", "  mql_prv_TOKEN  ")
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("PreviewEnterURL produced an unparseable URL %q: %v", got, err)
	}
	if u.Host != "shop.example.com" {
		t.Errorf("host = %q -- a padded hostname composes a link to a host nobody serves", u.Host)
	}
	if v := u.Query().Get(PreviewGrantParam); v != "mql_prv_TOKEN" {
		t.Errorf("grant = %q, want the trimmed token", v)
	}
}
