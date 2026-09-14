package dslconformance

// concat_literal_test.go -- the one reader of "a string concatenation that
// starts with this literal", for the gates that look for a prefix baked into
// an id (epic memql#5363, task memql#5368).
//
// A string concatenation is `a + b` (edition 2026 retired `concat(a, b)`).
// Three gates in this package look for a prefix or separator baked into one --
// `"ga-" +`, `"v1:ns:concept:" +`, `+ ":" +` -- and read it here, from one
// place, so they cannot drift apart about what the spelling looks like.

import (
	"regexp"
	"testing"
)

// plusLiteralRe matches a string literal that is the left operand of `+`. A
// literal that only ever sits on the RIGHT of a `+` is not a prefix of what the
// expression produces, so it is not read.
var plusLiteralRe = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"\s*\+`)

// concatLiteralPrefixes returns the literal each string concatenation on line
// starts with.
func concatLiteralPrefixes(line string) []string {
	var out []string
	for _, m := range plusLiteralRe.FindAllStringSubmatch(line, -1) {
		out = append(out, m[1])
	}
	return out
}

// concatSeparatorRe matches a `":"` literal used as a SEPARATOR in a string
// concatenation: `+ ":" +` between two operands.
var concatSeparatorRe = regexp.MustCompile(`\+\s*":"\s*\+`)

func TestConcatLiteralReaders(t *testing.T) {
	for _, tc := range []struct {
		line string
		want []string
	}{
		{`id: "ga-" + hash(email)`, []string{"ga-"}},
		{`id: args.id ?? ("node-" + hash(hash(args.nodeType) + hash(now)))`, []string{"node-"}},
		{`ref: "v1:identity:user:" + args.userId`, []string{"v1:identity:user:"}},
		// A literal on the right of `+` is a suffix, not a prefix.
		{`label: args.name + " (copy)"`, nil},
		// Not a concatenation at all.
		{`status: "active"`, nil},
	} {
		got := concatLiteralPrefixes(tc.line)
		if len(got) != len(tc.want) {
			t.Errorf("concatLiteralPrefixes(%q) = %q, want %q", tc.line, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("concatLiteralPrefixes(%q) = %q, want %q", tc.line, got, tc.want)
			}
		}
	}
	for _, sep := range []string{`hash(a + ":" + b)`, `shortId(args.d) + ":" +`} {
		if !concatSeparatorRe.MatchString(sep) {
			t.Errorf("concatSeparatorRe does not match the separator in %q", sep)
		}
	}
	for _, ok := range []string{`hash(hash(a) + hash(b))`, `kind == "v1:cognition:utterance"`, `a + ":suffix"`} {
		if concatSeparatorRe.MatchString(ok) {
			t.Errorf("concatSeparatorRe matches %q, which carries no separator", ok)
		}
	}
}
