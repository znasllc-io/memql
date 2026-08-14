package identity

import (
	"strings"
	"testing"
)

// cors_origins_test.go -- memql#3716, the origin grammar.
//
// Both directions are asserted from one table, because a validator that refuses
// everything satisfies every negative case and nothing else. The accepted rows
// ARE the positive control.

func TestValidateCORSOrigin(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string // "" = must be refused
	}{
		// Accepted -- the positive control.
		{"https host", "https://shop.customer.test", "https://shop.customer.test"},
		{"https host and port", "https://shop.customer.test:8443", "https://shop.customer.test:8443"},
		{"loopback dev server", "http://localhost:5173", "http://localhost:5173"},
		{"loopback by address", "http://127.0.0.1:8080", "http://127.0.0.1:8080"},
		{"an IPv6 literal", "https://[::1]:8443", "https://[::1]:8443"},
		{"surrounding whitespace is trimmed", "  https://shop.customer.test  ", "https://shop.customer.test"},
		{"scheme and host are lowercased", "HTTPS://Shop.Customer.Test", "https://shop.customer.test"},
		// Punycode is the form a browser actually puts in the Origin header for
		// an internationalised domain, so it must be accepted...
		{"a punycode IDN", "https://xn--80ak6aa92e.com", "https://xn--80ak6aa92e.com"},
		// ...and an underscore host is admitted deliberately. RFC 1123 forbids
		// it and container / internal service names use it anyway; refusing an
		// origin somebody legitimately serves from is the worse failure, and an
		// underscore can alter neither a string literal nor a header comparison.
		{"an underscore in the host", "https://internal_service:8085", "https://internal_service:8085"},

		// A browser OMITS the scheme's default port from the Origin header, so an
		// entry carrying it would never match what arrives. Canonicalised away
		// rather than refused: there is one unambiguous right answer and the
		// operator plainly meant it (fix round 1, item 4).
		{"the https default port is dropped", "https://shop.customer.test:443", "https://shop.customer.test"},
		{"the http default port is dropped", "http://shop.customer.test:80", "http://shop.customer.test"},
		{"a non-default port survives", "https://shop.customer.test:8443", "https://shop.customer.test:8443"},
		// An IPv6 literal has a canonical spelling and a non-canonical one compares
		// UNEQUAL, so it is canonicalised through net.IP.String() rather than
		// stored verbatim (fix round 1, item 4).
		{"a non-canonical IPv6 literal", "https://[0:0:0:0:0:0:0:1]", "https://[::1]"},
		{"a non-canonical IPv6 literal with a port", "https://[0:0:0:0:0:0:0:1]:8443", "https://[::1]:8443"},
		// Bracketing is decided from the RESULT: String() renders a v4-mapped
		// address as a dotted quad, which must not end up inside brackets.
		{"a v4-mapped IPv6 literal", "https://[::ffff:127.0.0.1]", "https://127.0.0.1"},

		// ...but the RAW unicode spelling is refused rather than stored, because
		// the browser will send the punycode form and this entry would sit in the
		// row looking granted while matching nothing.
		{"a raw unicode host", "https://münchen.example", ""},
		// Ports url.Parse is happy with and no browser can send from. Same failure
		// as the raw-unicode case: an entry that reads as granted and matches
		// nothing (fix round 1, item 4).
		{"port zero", "https://shop.customer.test:0", ""},
		{"port above the 16-bit range", "https://shop.customer.test:99999", ""},

		// Refused. Each of these would be STORED and then never matched, so the
		// operator would see a grant that silently does nothing -- which is why
		// they are refused rather than normalised.
		{"a path", "https://shop.customer.test/app", ""},
		{"a trailing slash", "https://shop.customer.test/", ""},
		{"a query string", "https://shop.customer.test?a=1", ""},
		{"a fragment", "https://shop.customer.test#x", ""},
		{"userinfo", "https://user:pw@shop.customer.test", ""},
		{"no scheme", "shop.customer.test", ""},
		{"a scheme-relative reference", "//shop.customer.test", ""},
		{"a non-http scheme", "ftp://shop.customer.test", ""},
		{"a data URI", "data:text/html,x", ""},
		{"the wildcard", "*", ""},
		{"a wildcard host", "https://*.customer.test", ""},
		{"a bare wildcard host", "https://*", ""},
		{"a scheme with no host", "https://", ""},
		{"empty", "", ""},
		{"whitespace only", "   ", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateCORSOrigin(tc.in)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("ValidateCORSOrigin(%q) = %q with no error, want a refusal", tc.in, got)
				}
				// The message must name the entry, because the caller is a person
				// holding a list and the only useful message says which line.
				if trimmed := strings.TrimSpace(tc.in); trimmed != "" && !strings.Contains(err.Error(), trimmed) {
					t.Errorf("error %q does not quote the offending entry %q", err, tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateCORSOrigin(%q) refused a legitimate origin: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ValidateCORSOrigin(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestValidateCORSOriginsRejectsTheWholeListOnOneBadEntry pins the all-or-nothing
// rule. A partially-applied allowance is the state nobody can reason about
// afterwards -- the operator sees success and a list that is not what they sent.
// TestValidateCORSOriginSuggestsAFixItWouldAccept covers fix round 1, item 5.
//
// The missing-scheme message interpolated the raw input, so "//shop.example" was
// answered with `e.g. "https:////shop.example"` -- a suggestion the validator
// would also refuse. An error that costs the reader a second round trip to learn
// nothing is worse than no suggestion, so the shapes that cannot produce a good
// one drop it instead.
func TestValidateCORSOriginSuggestsAFixItWouldAccept(t *testing.T) {
	for _, in := range []string{
		"shop.customer.test",
		"//shop.customer.test",
		"shop.customer.test:8443",
		"/shop.customer.test/app",
		"//",
	} {
		_, err := ValidateCORSOrigin(in)
		if err == nil {
			t.Fatalf("ValidateCORSOrigin(%q) was accepted; the case no longer measures anything", in)
		}
		msg := err.Error()
		// Whatever the message suggests must itself validate. Pull the suggestion
		// out of the `e.g. "..."` clause; a message with no suggestion is fine.
		const marker = `e.g. "`
		i := strings.Index(msg, marker)
		if i < 0 {
			continue
		}
		rest := msg[i+len(marker):]
		j := strings.IndexByte(rest, '"')
		if j < 0 {
			t.Errorf("ValidateCORSOrigin(%q) produced an unterminated suggestion: %s", in, msg)
			continue
		}
		suggestion := rest[:j]
		if _, sugErr := ValidateCORSOrigin(suggestion); sugErr != nil {
			t.Errorf("ValidateCORSOrigin(%q) suggested %q, which the validator itself refuses (%v).\n"+
				"Full message: %s", in, suggestion, sugErr, msg)
		}
	}
}

// TestValidateCORSOriginCannotSmuggleStringLiteralSyntax is the injection
// property.
//
// A granted origin is stored by embedding it in a MemQL string literal
// (identity.Store.SetOAuthClientCORSOrigins) and is later compared against a
// request header. So whatever survives validation must contain nothing that
// changes the meaning of either: no quote, no backslash, no control character,
// no comma that could read as a second list entry.
//
// Asserted as a PROPERTY of whatever comes back rather than as a list of
// expected refusals, because the interesting failure is an input nobody thought
// of being accepted. Most of these are refused by net/url before the checks in
// ValidateCORSOrigin get a say; that is fine, but it is not something to assume
// -- it is what this test measures.
func TestValidateCORSOriginCannotSmuggleStringLiteralSyntax(t *testing.T) {
	for _, in := range []string{
		`https://a"b.example`,
		`https://a\b.example`,
		"https://a\nb.example",
		"https://a\tb.example",
		"https://a.example\x00",
		`https://a.example"]`,
		`https://a.example,https://b.example`,
		`https://a.example;https://b.example`,
		`https://a b.example`,
		`https://a.example:notaport`,
	} {
		got, err := ValidateCORSOrigin(in)
		if err != nil {
			continue // refused, which is the safe outcome
		}
		for _, forbidden := range []string{`"`, `\`, "\n", "\r", "\t", "\x00", ",", ";", " "} {
			if strings.Contains(got, forbidden) {
				t.Errorf("ValidateCORSOrigin(%q) accepted and returned %q, which contains %q.\n"+
					"An accepted origin is embedded in a MemQL string literal on the write and "+
					"compared against an Origin header on the read; neither can carry this.",
					in, got, forbidden)
			}
		}
	}
}

func TestValidateCORSOriginsRejectsTheWholeListOnOneBadEntry(t *testing.T) {
	_, err := ValidateCORSOrigins([]string{
		"https://good.customer.test",
		"https://bad.customer.test/oops",
		"https://also-good.customer.test",
	})
	if err == nil {
		t.Fatal("a list carrying one malformed entry was accepted")
	}
	if !strings.Contains(err.Error(), "https://bad.customer.test/oops") {
		t.Errorf("error %q does not name the entry that failed", err)
	}
}

func TestValidateCORSOriginsEmptyIsARevoke(t *testing.T) {
	for _, in := range [][]string{nil, {}} {
		got, err := ValidateCORSOrigins(in)
		if err != nil {
			t.Fatalf("ValidateCORSOrigins(%#v) = error %v, want the empty allowance -- that is how "+
				"a grant is revoked", in, err)
		}
		if len(got) != 0 {
			t.Errorf("ValidateCORSOrigins(%#v) = %#v, want empty", in, got)
		}
	}
}

func TestValidateCORSOriginsCapsTheCount(t *testing.T) {
	tooMany := make([]string, MaxCORSOriginsPerClient+1)
	for i := range tooMany {
		tooMany[i] = "https://site.customer.test"
	}
	if _, err := ValidateCORSOrigins(tooMany); err == nil {
		t.Fatalf("a list of %d origins was accepted, the cap is %d", len(tooMany), MaxCORSOriginsPerClient)
	}
}

// TestParseCORSOriginsJSONIsLenientPerEntry is the read side, and the asymmetry
// with the write side is the point: one unusable entry must not take the rest of
// the row -- or, via the union the middleware reads, every OTHER customer's
// origin -- down with it. The entry itself still fails closed.
func TestParseCORSOriginsJSONIsLenientPerEntry(t *testing.T) {
	origins, dropped := ParseCORSOriginsJSON(
		`["https://good.customer.test","*","https://bad.customer.test/path","https://also-good.customer.test"]`)

	want := []string{"https://good.customer.test", "https://also-good.customer.test"}
	if len(origins) != len(want) {
		t.Fatalf("origins = %#v, want %#v", origins, want)
	}
	for i := range want {
		if origins[i] != want[i] {
			t.Errorf("origins[%d] = %q, want %q", i, origins[i], want[i])
		}
	}
	if len(dropped) != 2 {
		t.Errorf("dropped = %#v, want the wildcard and the path entry", dropped)
	}
}

func TestParseCORSOriginsJSONHandlesTheNoAllowanceForms(t *testing.T) {
	for _, in := range []string{"", "   ", "[]"} {
		origins, dropped := ParseCORSOriginsJSON(in)
		if len(origins) != 0 || len(dropped) != 0 {
			t.Errorf("ParseCORSOriginsJSON(%q) = (%#v, %#v), want no allowance and nothing dropped",
				in, origins, dropped)
		}
	}
}

// TestParseCORSOriginsJSONOnGarbageAllowsNothing covers a value that is not even
// a JSON array. It must allow nothing and SAY so, rather than being swallowed:
// the only way such a value reaches the row is a direct server-side write, and
// that is worth a log line.
func TestParseCORSOriginsJSONOnGarbageAllowsNothing(t *testing.T) {
	origins, dropped := ParseCORSOriginsJSON(`{"origins": "https://shop.customer.test"}`)
	if len(origins) != 0 {
		t.Errorf("origins = %#v, want none", origins)
	}
	if len(dropped) != 1 {
		t.Errorf("dropped = %#v, want the whole unreadable value reported once", dropped)
	}
}

// TestMarshalCORSOriginsWritesEmptyForARevoke pins the state collapse.
//
// A revoke must write "" and NOT "[]". `filter corsOriginsJSON != ""` -- the
// canonical is-set idiom, which already excludes the ABSENT field every
// freshly-registered row carries -- MATCHES "[]", so a revoked row written that
// way would sit in oAuthClientCORSGrants's result set forever while contributing
// no origins: an @unbounded read growing monotonically, and a query whose comment
// claims to return the rows a human has granted.
func TestMarshalCORSOriginsWritesEmptyForARevoke(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want string
	}{
		{nil, ``},
		{[]string{}, ``},
		{[]string{"https://shop.customer.test"}, `["https://shop.customer.test"]`},
	} {
		got, err := MarshalCORSOrigins(tc.in)
		if err != nil {
			t.Fatalf("MarshalCORSOrigins(%#v): %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("MarshalCORSOrigins(%#v) = %q, want %q -- a revoke returns the row to the "+
				"state it had before it was ever granted, rather than inventing a third spelling "+
				"of 'no allowance' that the query's filter matches", tc.in, got, tc.want)
		}
	}
}

// TestCORSOriginRoundTrip is the property that matters across the two sides: what
// the write stores, the read accepts unchanged. If these two ever diverge the
// divergence is fail-open -- a write accepting something the read then matches
// loosely -- which is why the grammar has exactly one implementation.
func TestCORSOriginRoundTrip(t *testing.T) {
	in := []string{"HTTPS://Shop.Customer.Test", "http://localhost:5173", "https://[::1]:8443"}

	canonical, err := ValidateCORSOrigins(in)
	if err != nil {
		t.Fatalf("ValidateCORSOrigins: %v", err)
	}
	encoded, err := MarshalCORSOrigins(canonical)
	if err != nil {
		t.Fatalf("MarshalCORSOrigins: %v", err)
	}
	out, dropped := ParseCORSOriginsJSON(encoded)
	if len(dropped) != 0 {
		t.Fatalf("the read path dropped %#v out of what the write path stored", dropped)
	}
	if len(out) != len(canonical) {
		t.Fatalf("round trip = %#v, want %#v", out, canonical)
	}
	for i := range canonical {
		if out[i] != canonical[i] {
			t.Errorf("round trip [%d] = %q, want %q", i, out[i], canonical[i])
		}
	}
}
