package sense

import (
	"strings"
	"testing"
)

// TestTokenize_StringEndCoversQuotes is the regression net for
// memql-cockpit#114. Pre-fix the tokenizer computed
// `endCol = pt.Column + len(pt.Literal)`, but for string tokens
// the lexer strips the quotes from Literal -- so the reported
// End.Column landed two cells before the closing quote. Consumers
// using the Range to drive syntax highlighting then left the last
// 1-2 cells of every quoted string unstyled.
//
// The contract this pins: the string token's Range covers the
// FULL source span, opening quote through closing quote inclusive.
func TestTokenize_StringEndCoversQuotes(t *testing.T) {
	cases := []struct {
		name        string
		source      string
		wantLiteral string
		wantStart   Position // 1-indexed start (opening quote)
		wantEnd     Position // 1-indexed exclusive end (one past closing quote)
	}{
		{
			name:        "simple string",
			source:      `"hello"`,
			wantLiteral: "hello",
			wantStart:   Position{Line: 1, Column: 1},
			wantEnd:     Position{Line: 1, Column: 8}, // 7 source chars + 1
		},
		{
			name:        "string with escaped quote",
			source:      `"a\"b"`,
			wantLiteral: `a"b`,
			wantStart:   Position{Line: 1, Column: 1},
			wantEnd:     Position{Line: 1, Column: 7}, // 6 source chars + 1
		},
		{
			name:        "indented concept-id string",
			source:      "  " + `"v1:agents:agent:abc"`,
			wantLiteral: "v1:agents:agent:abc",
			wantStart:   Position{Line: 1, Column: 3},
			wantEnd:     Position{Line: 1, Column: 24}, // 21 source chars starting at col 3 -> end at col 24
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := &Service{}
			tokens := svc.Tokenize(tc.source)

			// Find the string token. Other tokens (whitespace, EOF)
			// aren't emitted by Tokenize, but trailing punctuation
			// might be -- pick by Type.
			var strTok *Token
			for i := range tokens {
				if tokens[i].Type == "string" {
					strTok = &tokens[i]
					break
				}
			}
			if strTok == nil {
				t.Fatalf("no string token emitted for source %q; got %d tokens", tc.source, len(tokens))
			}

			if strTok.Literal != tc.wantLiteral {
				t.Errorf("Literal = %q, want %q", strTok.Literal, tc.wantLiteral)
			}
			if strTok.Range.Start != tc.wantStart {
				t.Errorf("Start = %+v, want %+v", strTok.Range.Start, tc.wantStart)
			}
			if strTok.Range.End != tc.wantEnd {
				t.Errorf("End = %+v, want %+v -- string token end must include the closing quote", strTok.Range.End, tc.wantEnd)
			}

			// Source-span sanity: End.Column - Start.Column should equal
			// the number of source runes consumed by the string literal
			// (quotes + content + escape-bytes). Cross-check with the
			// raw source length to lock the invariant.
			//
			// Note: for the indented case the source has 2 leading
			// spaces, so total runes = 2 + (End.Column - Start.Column).
			gotSpan := tc.wantEnd.Column - tc.wantStart.Column
			wantSpan := len(tc.source) - (tc.wantStart.Column - 1)
			if gotSpan != wantSpan {
				t.Errorf("source span = %d cols, want %d (full source length minus leading offset)", gotSpan, wantSpan)
			}
		})
	}
}

// TestTokenize_IdentifierEndIsExclusive covers the common non-string
// path -- identifiers, where the Literal length already matched the
// source length, but the new End* path should still produce the same
// answer.
func TestTokenize_IdentifierEndIsExclusive(t *testing.T) {
	svc := &Service{}
	tokens := svc.Tokenize("foo bar")

	if len(tokens) < 2 {
		t.Fatalf("want >= 2 tokens, got %d: %+v", len(tokens), tokens)
	}
	if tokens[0].Literal != "foo" || tokens[0].Range.Start.Column != 1 || tokens[0].Range.End.Column != 4 {
		t.Errorf("foo token: %+v, want Start.Column=1 End.Column=4", tokens[0].Range)
	}
	if tokens[1].Literal != "bar" || tokens[1].Range.Start.Column != 5 || tokens[1].Range.End.Column != 8 {
		t.Errorf("bar token: %+v, want Start.Column=5 End.Column=8", tokens[1].Range)
	}
}

// TestTokenize_NumberEnd: numbers are non-string but worth pinning
// since the path-through-mapTokenType is the same.
func TestTokenize_NumberEnd(t *testing.T) {
	svc := &Service{}
	tokens := svc.Tokenize("42")
	if len(tokens) != 1 {
		t.Fatalf("want 1 token, got %d", len(tokens))
	}
	if tokens[0].Type != "number" {
		t.Errorf("Type = %q, want number", tokens[0].Type)
	}
	if tokens[0].Range.End.Column != 3 {
		t.Errorf("End.Column = %d, want 3 (one past last digit)", tokens[0].Range.End.Column)
	}
}

// firstTokenType returns the Type of the first token whose Literal matches, or
// "" if none. Helper for the construct-keyword highlighting tests.
func firstTokenType(tokens []Token, literal string) string {
	for _, tk := range tokens {
		if tk.Literal == literal {
			return tk.Type
		}
	}
	return ""
}

// TestTokenize_ConstructKeywordsColored is the E5 (memql#2376) pin: lowercase
// construct keywords lex as identifiers in the core lexer (which only keywords
// the capitalized receiver forms), but the Sense tokenizer must render them as
// `keyword` semantic tokens -- sourced from dslspec so a future grammar epic
// inherits coloring. Strings / annotations / concept-ids / plain identifiers
// are unaffected.
func TestTokenize_ConstructKeywordsColored(t *testing.T) {
	svc := &Service{}

	// A deployment-bundle-shaped snippet: an action declaration whose body is a
	// single capability call, with an annotation, a bound-concept mutate decl,
	// and string args.
	src := "@description(\"cut a release\")\n" +
		"action tagRelease {\n" +
		"  capability script(script: \"deploy.tag\")\n" +
		"}\n" +
		"mutate space recordDeploy { insert { id: \"x\" } }"
	tokens := svc.Tokenize(src)

	// Construct keywords render as `keyword`.
	for _, kw := range []string{"action", "capability", "mutate"} {
		if got := firstTokenType(tokens, kw); got != "keyword" {
			t.Errorf("construct keyword %q rendered as %q, want keyword", kw, got)
		}
	}

	// Non-keyword identifiers (the action name, the capability verb, an arg key)
	// stay identifiers -- the highlighter colors the keyword set, not everything.
	for _, id := range []string{"tagRelease", "script", "recordDeploy"} {
		if got := firstTokenType(tokens, id); got != "identifier" {
			t.Errorf("identifier %q rendered as %q, want identifier", id, got)
		}
	}

	// The annotation `@` and the string literal keep their own types -- the
	// construct-keyword coloring only touches the TokenIdentifier path.
	hasType := func(typ string) bool {
		for _, tk := range tokens {
			if tk.Type == typ {
				return true
			}
		}
		return false
	}
	if !hasType("annotation") {
		t.Errorf("expected an annotation token for `@description`; got none")
	}
	if got := firstTokenType(tokens, "cut a release"); got != "string" {
		t.Errorf("string literal rendered as %q, want string", got)
	}
}

// TestTokenize_RetiredHasNotKeyword pins the E2 (memql#2373) `has` de-keywording
// at the tokenize layer: the core lexer still emits TokenKeywordHas for the
// retired `has` word, but the Sense tokenizer no longer maps it to `keyword`
// (membership is `in` only, #971). It falls through to `identifier`.
func TestTokenize_RetiredHasNotKeyword(t *testing.T) {
	svc := &Service{}
	tokens := svc.Tokenize("a has b")
	if got := firstTokenType(tokens, "has"); got == "keyword" {
		t.Errorf("`has` rendered as keyword; it is retired and must not be a keyword token (#971)")
	}
	// The live membership operator `in` is still a keyword.
	inTokens := svc.Tokenize("a in b")
	if got := firstTokenType(inTokens, "in"); got != "keyword" {
		t.Errorf("`in` rendered as %q, want keyword (the live membership operator)", got)
	}
}

// TestKeywordDocsFreeOfRetiredForms (E2 / memql#2373) pins the KeywordDocs
// purge: the retired procedural `func` receiver keyword and the `has` membership
// operator must not reappear as documented keywords, and no surviving doc may
// teach a retired form (the `use v1:domain:concept` Form-A import, or `has`).
func TestKeywordDocsFreeOfRetiredForms(t *testing.T) {
	for _, retired := range []string{"func", "has"} {
		if _, ok := KeywordDocs[retired]; ok {
			t.Errorf("KeywordDocs still documents retired keyword %q -- purge it (memql#2373)", retired)
		}
	}
	for kw, doc := range KeywordDocs {
		if strings.Contains(doc, "use v1:") {
			t.Errorf("KeywordDocs[%q] teaches the retired Form-A import `use v1:domain:concept`: %q", kw, doc)
		}
		if strings.Contains(doc, "not has") || strings.Contains(doc, "has \"") {
			t.Errorf("KeywordDocs[%q] references the retired `has` operator: %q", kw, doc)
		}
	}
}

// E6 (memql#2392): invocation kind prefixes color as keywords -- notably
// `mutation`, whose DECLARATION keyword is `mutate` (so it is absent from the
// dslspec construct set). The set is sourced from the parser's authoritative
// invocationKindKeywords via parser.InvocationKindKeywords(), drift-proof by
// construction; this test pins the wiring end-to-end.
func TestTokenize_InvocationKindPrefixesAreKeywords(t *testing.T) {
	src := `automation deploy {
  step record {
    mutation createDeployment(deploymentId: x)
  }
  step decide {
    logic deployGateGreen(environment: y)
  }
}`
	s := New(nil)
	tokens := s.Tokenize(src)
	want := map[string]bool{"mutation": false, "logic": false, "automation": false}
	for _, tok := range tokens {
		if _, ok := want[tok.Literal]; ok && tok.Type == "keyword" {
			want[tok.Literal] = true
		}
	}
	for kw, seen := range want {
		if !seen {
			t.Errorf("invocation/declaration keyword %q did not tokenize as keyword", kw)
		}
	}
}
