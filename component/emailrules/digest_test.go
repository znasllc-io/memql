package emailrules

// digest_test.go -- how a row-derived address becomes a membership key
// (memql#6022, F9).
//
// This package keeps its own copy of the campaigns normalization because it does
// not import component/campaigns. A copy that drifts is silent: the rule looks a
// mailbox up under a digest the campaigns package never stamped, finds nobody,
// and enrols a second membership for a person who is already in the audience.
// So the copy is held to the original by a test that does import it -- the one
// place this package's tests reach for component/campaigns.

import (
	"testing"

	"github.com/znasllc-io/memql/component/campaigns"
)

// TestAddressDigestMatchesCampaigns: the same digest campaigns.EmailDigest
// computes, for every address either can use and for the ones neither can.
func TestAddressDigestMatchesCampaigns(t *testing.T) {
	usable := 0
	for _, addr := range []string{
		" Foo@Example.COM ", "ÄBC@x.test", "no-at-sign", "a b@c.test",
		"", "   ", "@leading", "trailing@", "line\nbreak@example.test", "tab\tin@example.test",
		"Person+News@Example.Test", `"a@b"@example.test`, "\u00a0nbsp@example.test\u00a0", "in\u00a0side@example.test", `Q"uote\back?slash{brace}'@Example.TEST`,
	} {
		got, want := addressDigest(addr), campaigns.EmailDigest(addr)
		if got != want {
			t.Errorf("addressDigest(%q) = %q, campaigns.EmailDigest = %q: a rule would look up a digest the campaigns package never stamped", addr, got, want)
		}
		if want != "" {
			usable++
		}
	}
	if usable < 5 {
		t.Fatalf("only %d of the addresses have a digest; the comparison above proves nothing", usable)
	}
}

// TestAddressDigestIsTheMailboxNotItsSpelling: case and surrounding whitespace
// are folded and nothing else is -- a sub-addressed or dotted mailbox is another
// person's mailbox as far as this key is concerned.
func TestAddressDigestIsTheMailboxNotItsSpelling(t *testing.T) {
	want := addressDigest("person@example.test")
	if want != "143b53b0840f6e1fcf8537a38513b59a9f326c72f577f17d1da67393eff1d413" {
		t.Fatalf("addressDigest(person@example.test) = %s: the digest is a stored format, and this is the SHA-256 of that exact text", want)
	}
	for _, spelling := range []string{"PERSON@EXAMPLE.TEST", "  Person@Example.Test\n", "person@EXAMPLE.test"} {
		if got := addressDigest(spelling); got != want {
			t.Errorf("addressDigest(%q) did not fold onto the canonical digest", spelling)
		}
	}
	for _, other := range []string{"person+news@example.test", "per.son@example.test"} {
		if addressDigest(other) == want {
			t.Errorf("%q folded onto person@example.test; sub-addressing and dots are the person's own distinctions", other)
		}
	}
	for _, unusable := range []string{"", "   ", "no-at-sign", "@leading", "trailing@", "a b@c.test"} {
		if got := addressDigest(unusable); got != "" {
			t.Errorf("addressDigest(%q) = %q, want empty: an address with no usable digest has no key to be found by", unusable, got)
		}
	}
}
