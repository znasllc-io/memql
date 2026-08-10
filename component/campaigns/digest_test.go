package campaigns

import "testing"

// digest_test.go -- NormalizeEmail decides what an existing suppression
// row matches, so it is a stored format rather than a helper. These cases
// pin the folding that IS done and, just as deliberately, the folding
// that is NOT.

func TestNormalizeFoldsCaseAndWhitespace(t *testing.T) {
	want := EmailDigest("person@example.test")
	for _, spelling := range []string{
		"person@example.test",
		"PERSON@EXAMPLE.TEST",
		"  Person@Example.Test  ",
		"person@EXAMPLE.test",
	} {
		if got := EmailDigest(spelling); got != want {
			t.Errorf("EmailDigest(%q) did not fold onto the canonical digest", spelling)
		}
	}
}

// The deliberate NON-equivalences. Each of these would suppress an
// address the person never opted out of, so folding them is the unsafe
// direction even though it looks tidier.
func TestNormalizeDoesNotFoldSubaddressingOrDots(t *testing.T) {
	base := EmailDigest("person@example.test")
	for _, other := range []string{
		"person+news@example.test", // sub-addressing is the recipient's own separation
		"per.son@example.test",     // dot-equivalence is Gmail-specific
	} {
		if EmailDigest(other) == base {
			t.Errorf("%q folded onto person@example.test; that suppresses an address the person did not opt out", other)
		}
	}
}

func TestUnusableAddressesDigestToEmpty(t *testing.T) {
	for _, bad := range []string{
		"", "   ", "no-at-sign", "@leading", "trailing@", "spa ce@example.test", "line\nbreak@example.test",
	} {
		if got := EmailDigest(bad); got != "" {
			t.Errorf("EmailDigest(%q) = %q, want empty -- a non-empty digest for an unusable address suppresses nothing while appearing to work", bad, got)
		}
	}
}

func TestEmailDomainIsLowercasedAndBounded(t *testing.T) {
	if got := EmailDomain(" User@Sub.Example.TEST "); got != "sub.example.test" {
		t.Errorf("EmailDomain = %q, want sub.example.test", got)
	}
	if got := EmailDomain("nonsense"); got != "" {
		t.Errorf("EmailDomain(%q) = %q, want empty", "nonsense", got)
	}
	// An address with more than one @ takes the LAST one as the
	// separator, matching NormalizeEmail -- a local part may quote an @.
	if got := EmailDomain(`"a@b"@example.test`); got != "example.test" {
		t.Errorf("EmailDomain = %q, want example.test", got)
	}
}

func TestDigestIsStableAndOpaque(t *testing.T) {
	got := EmailDigest("person@example.test")
	if len(got) != 64 {
		t.Fatalf("digest length %d, want 64 hex characters", len(got))
	}
	// The digest must not contain the address: it is the entire reason a
	// cluster-wide list costs nobody a mailbox.
	if got == "person@example.test" || len(got) < 32 {
		t.Fatal("the digest is not a digest")
	}
}
