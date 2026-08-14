package sense

// sourcehash_test.go pins the canonical construct hash's normalization rules.
//
// Each case is a pair that must hash the SAME or DIFFERENT, because that is the
// only thing the hash is for: "is this the same construct". A rule that
// collapses too much marks a real edit as no change; one that collapses too
// little marks a reformat as drift, which is the failure that reads as
// "everything is broken".

import "testing"

func TestConstructSourceHashIgnoresInsignificantEdits(t *testing.T) {
	cases := []struct {
		name string
		a    string
		b    string
	}{
		{
			name: "re-indentation",
			a:    "query widget w {\n  filter  a==1\n}",
			b:    "query widget w {\n\t\tfilter a==1\n}",
		},
		{
			name: "line comments",
			a:    "// what this is for\nquery widget w {\n  filter a==1 // and why\n}",
			b:    "query widget w {\n  filter a==1\n}",
		},
		{
			name: "block comments",
			a:    "/* a note\n   over two lines */\nquery widget w { filter a==1 }",
			b:    "query widget w { filter a==1 }",
		},
		{
			name: "trailing whitespace and newlines",
			a:    "query widget w { filter a==1 }   \n\n",
			b:    "query widget w { filter a==1 }",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if ConstructSourceHash(tc.a) != ConstructSourceHash(tc.b) {
				t.Errorf("%s changed the hash but cannot change what the engine runs\n  a: %q -> %q\n  b: %q -> %q",
					tc.name, tc.a, NormalizeConstructSource(tc.a), tc.b, NormalizeConstructSource(tc.b))
			}
		})
	}
}

func TestConstructSourceHashKeepsSignificantEdits(t *testing.T) {
	cases := []struct {
		name string
		a    string
		b    string
	}{
		{
			name: "a doc comment is part of the declaration",
			a:    "/// Restrict to one widget.\nquery widget w { filter a==1 }",
			b:    "query widget w { filter a==1 }",
		},
		{
			name: "editing a doc comment",
			a:    "/// Restrict to one widget.\nquery widget w { filter a==1 }",
			b:    "/// Restrict to two widgets.\nquery widget w { filter a==1 }",
		},
		{
			name: "whitespace inside a string literal",
			a:    `@description("a  b")` + "\nquery widget w { filter a==1 }",
			b:    `@description("a b")` + "\nquery widget w { filter a==1 }",
		},
		{
			name: "an annotation",
			a:    "@actor\nquery widget w { filter a==1 }",
			b:    "query widget w { filter a==1 }",
		},
		{
			name: "a doc comment must not collide with a bare identifier",
			a:    "/// active\nquery widget w { filter a==1 }",
			b:    "active query widget w { filter a==1 }",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if ConstructSourceHash(tc.a) == ConstructSourceHash(tc.b) {
				t.Errorf("%s did not change the hash, but changes what the construct declares\n  normalized: %q",
					tc.name, NormalizeConstructSource(tc.a))
			}
		})
	}
}

// TestConstructSourceHashDoesNotReadCommentsInsideStrings: a `//` inside a
// string literal is text, not a comment. Getting this wrong truncates the rest
// of the declaration out of the hash, so two genuinely different constructs
// would hash the same.
func TestConstructSourceHashDoesNotReadCommentsInsideStrings(t *testing.T) {
	a := `query widget w { filter url=="http://a" && x==1 }`
	b := `query widget w { filter url=="http://a" && x==2 }`
	if ConstructSourceHash(a) == ConstructSourceHash(b) {
		t.Fatalf("the body after a `//` inside a string literal was dropped: %q", NormalizeConstructSource(a))
	}
	if got := NormalizeConstructSource(a); got != `query widget w { filter url=="http://a" && x==1 }` {
		t.Errorf("string literal not preserved verbatim: %q", got)
	}
}

// TestConstructSourceHashEmptyIsNotAHash: a construct with no source is a
// MISSING answer. Hashing "" would make two missing answers compare equal as
// though both were known -- which, for drift detection, reads as "trained".
func TestConstructSourceHashEmptyIsNotAHash(t *testing.T) {
	for _, in := range []string{"", "   \n\t ", "// only a comment\n"} {
		if got := ConstructSourceHash(in); got != "" {
			t.Errorf("ConstructSourceHash(%q) = %q, want the empty string", in, got)
		}
	}
}

// TestConstructSourceHashIsStable: the same input hashes the same twice, and
// the value is the lowercase hex the contract states.
func TestConstructSourceHashIsStable(t *testing.T) {
	const src = "query widget w { filter a==1 }"
	first := ConstructSourceHash(src)
	if first != ConstructSourceHash(src) {
		t.Fatal("the hash is not stable across calls")
	}
	if len(first) != 64 {
		t.Fatalf("expected a 64-char lowercase hex SHA-256, got %d chars: %q", len(first), first)
	}
	for _, c := range first {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			t.Fatalf("not lowercase hex: %q", first)
		}
	}
}

// TestNormalizeConstructSourceTerminatesOnUnclosedInput: a half-typed buffer is
// the ordinary case in an editor, so an unterminated string or block comment
// must run to end of input rather than loop.
func TestNormalizeConstructSourceTerminatesOnUnclosedInput(t *testing.T) {
	for _, in := range []string{
		`query widget w { filter a=="unterminated`,
		"query widget w { /* unterminated",
		`query widget w { filter a=="trailing escape \`,
	} {
		_ = NormalizeConstructSource(in)
	}
}
