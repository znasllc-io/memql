package dslconformance

// concat_literal_test.go -- the one reader of "a string concatenation that
// starts with this literal", for the gates that look for a prefix baked into
// an id (epic memql#5363, task memql#5368).
//
// Edition 2026 retires `concat(a, b)` for `a + b`, and the codemod rewrites
// every call. Three gates in this package looked for the call's spelling --
// `concat("ga-"`, `concat("v1:ns:concept:",`, `, ":",` inside one -- and each
// would have matched nothing on the migrated tree and reported it clean. They
// read both spellings here, from one place, so they cannot drift apart about
// what the new one looks like.

import (
	"regexp"
	"testing"
)

var (
	// concatCallLiteralRe is the legacy spelling: a concat() call whose first
	// argument is a string literal.
	concatCallLiteralRe = regexp.MustCompile(`\bconcat\(\s*"((?:[^"\\]|\\.)*)"`)
	// plusLiteralRe is edition 2026's: a string literal that is the left
	// operand of `+`. A literal that only ever sits on the RIGHT of a `+` is
	// not a prefix of what the expression produces, so it is not read.
	plusLiteralRe = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"\s*\+`)
)

// concatLiteralPrefixes returns the literal each string concatenation on line
// starts with, in either edition's spelling.
func concatLiteralPrefixes(line string) []string {
	var out []string
	for _, re := range []*regexp.Regexp{concatCallLiteralRe, plusLiteralRe} {
		for _, m := range re.FindAllStringSubmatch(line, -1) {
			out = append(out, m[1])
		}
	}
	return out
}

// concatSeparatorRe matches a `":"` literal used as a SEPARATOR in a string
// concatenation: `, ":",` between two concat() arguments, or `+ ":" +`
// between two operands.
var concatSeparatorRe = regexp.MustCompile(`,\s*":"\s*,|\+\s*":"\s*\+`)

func TestConcatLiteralReadersReadBothEditions(t *testing.T) {
	for _, tc := range []struct {
		line string
		want []string
	}{
		{`id: concat("ga-", hash(email))`, []string{"ga-"}},
		{`id: "ga-" + hash(email)`, []string{"ga-"}},
		{`id: args.id ?? ("node-" + hash(hash(args.nodeType) + hash(now)))`, []string{"node-"}},
		{`ref: concat("v1:identity:user:", args.userId)`, []string{"v1:identity:user:"}},
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
	for _, sep := range []string{`hash(concat(a, ":", b))`, `hash(a + ":" + b)`, `shortId(args.d) + ":" +`} {
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
