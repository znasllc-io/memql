package sense

import (
	"strings"
	"testing"
)

// startswith_test.go -- the editor surfaces for the `startsWith` predicate
// (memql#4208): it colours as a keyword like `in`, it hovers with a doc, and
// the bare-row-intrinsic scanner treats it as a comparison operator so
// `filter row => id startsWith "v1:"` is flagged like
// `filter row => id in args.ids`.

func TestTokenize_StartsWithIsKeyword(t *testing.T) {
	svc := &Service{}
	tokens := svc.Tokenize(`codeReference startsWith "integration."`)
	if got := firstTokenType(tokens, "startsWith"); got != "keyword" {
		t.Errorf("`startsWith` rendered as %q, want keyword (the string-prefix operator, memql#4208)", got)
	}
	if got := firstTokenType(tokens, "codeReference"); got != "identifier" {
		t.Errorf("the field on the left rendered as %q, want identifier", got)
	}
}

// startsWith hovers as the operator it is (memql#5365): the operator table's
// card, whose form and prose explain the prefix semantics this test has always
// asked for. It used to hover as a bare keyword doc.
func TestHover_StartsWithKeyword(t *testing.T) {
	s := New(&stubRegistry{})
	// "  filter row => row.codeReference startsWith \"integration.\"" -- column
	// 39 is inside the keyword.
	src := "query codeMetric q {\n  filter row => row.codeReference startsWith \"integration.\"\n}"
	res := hoverAt(t, s, src, 2, 39)
	if res == nil || !strings.HasPrefix(res.Contents, "```memql\ns startsWith p\n```") {
		t.Fatalf("expected the startsWith operator card, got %+v", res)
	}
	if !strings.Contains(res.Contents, "prefix") {
		t.Errorf("hover should explain the prefix semantics: %q", res.Contents)
	}
}

func TestBareRowIntrinsicRuleFlagsStartsWith(t *testing.T) {
	got := bareRowIntrinsicRule("query widget q {\n  filter row => id startsWith \"v1:widget:\"\n}\n")
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 diagnostic for a bare `id startsWith`, got %d", len(got))
	}
	if !strings.Contains(got[0].Message, "`row.id`") {
		t.Errorf("message must name the replacement `row.id`, got: %s", got[0].Message)
	}
	// The canonical spelling, and a payload property, stay silent.
	for _, src := range []string{
		"query widget q {\n  filter row => row.id startsWith \"v1:widget:\"\n}\n",
		"query widget q {\n  filter row => row.codeReference startsWith args.prefixes\n}\n",
	} {
		if diags := bareRowIntrinsicRule(src); len(diags) != 0 {
			t.Errorf("%q: expected no diagnostic, got %s", src, diags[0].Message)
		}
	}
}
