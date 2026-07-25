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

// tokenTypeAtLine returns the Type of the token on `line` whose Literal
// matches, or "" if none. Unlike firstTokenType this disambiguates a
// spelling that appears in several positions -- `action` is both a
// construct keyword and a bound concept name in the same snippet.
func tokenTypeAtLine(tokens []Token, literal string, line int) string {
	for _, tk := range tokens {
		if tk.Literal == literal && tk.Range.Start.Line == line {
			return tk.Type
		}
	}
	return ""
}

// TestTokenize_ConstructKeywordPositionAware pins the fix for the
// over-coloring the tokenizer previously accepted as a trade-off: an
// identifier that merely SHARES a construct keyword's spelling rendered
// as `keyword` wherever it appeared. `capability` is both a construct
// keyword and a payload field of v1:actions:action, so a shape
// projecting that field lit up as a declaration.
//
// The contract this pins: a construct keyword colors as `keyword` only
// where it leads a top-level declaration header or an in-body invocation
// step; everywhere else it is an ordinary identifier. The concept a
// signature binds colors as `concept`, so `shape action actionFull`
// renders keyword / concept / identifier rather than three keywords.
func TestTokenize_ConstructKeywordPositionAware(t *testing.T) {
	const shapeDecl = "@row\n" +
		"shape action actionFull {\n" +
		"  intent\n" +
		"  capability\n" +
		"  sideEffectClass\n" +
		"}"
	const conceptDecl = "concept action {\n" +
		"  capability  string\n" +
		"}"
	const invocation = "automation onThing {\n" +
		"  step run {\n" +
		"    mutation mutationCreateCanvasState(stateId: \"x\")\n" +
		"  }\n" +
		"}"
	// The three forms where a construct kind does NOT lead its line or
	// sits outside a declaration header. Each regressed during the fix
	// before the classifier learned it.
	const midLineCall = "logic bootstrap {\n" +
		"  body {\n" +
		"    existing := query existingCluster()\n" +
		"  }\n" +
		"}"
	const arrowTarget = "automation onUser @trigger(event=\"node.created\") => logic handleUser"
	const queryClause = "query participant activeOnes {\n" +
		"  filter status==\"active\"\n" +
		"  shape participantFull\n" +
		"}"

	cases := []struct {
		name    string
		source  string
		literal string
		line    int
		want    string
	}{
		{"declaration keyword leads its line", shapeDecl, "shape", 2, "keyword"},
		{"signature-bound concept", shapeDecl, "action", 2, "concept"},
		{"declared construct name", shapeDecl, "actionFull", 2, "identifier"},
		{"projected field sharing a keyword spelling", shapeDecl, "capability", 4, "identifier"},
		{"concept declaration keyword", conceptDecl, "concept", 1, "keyword"},
		{"declared concept name", conceptDecl, "action", 1, "concept"},
		{"payload field sharing a keyword spelling", conceptDecl, "capability", 2, "identifier"},
		{"in-body invocation step keeps keyword", invocation, "mutation", 3, "keyword"},
		{"mid-line invocation keeps keyword", midLineCall, "query", 3, "keyword"},
		{"automation arrow target keeps keyword", arrowTarget, "logic", 1, "keyword"},
		{"query shape clause keeps keyword", queryClause, "shape", 3, "keyword"},
		{"query signature concept", queryClause, "participant", 1, "concept"},
	}
	svc := &Service{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tokenTypeAtLine(svc.Tokenize(tc.source), tc.literal, tc.line)
			if got != tc.want {
				t.Errorf("%q on line %d rendered as %q, want %q", tc.literal, tc.line, got, tc.want)
			}
		})
	}
}

// TestTokenize_SingleIdentifierHeaderNamesItself pins the boundary of the
// signature-concept rule: a one-identifier header (`trait x`, `provider
// x`) declares no concept, so its name must stay an identifier. Only the
// two-identifier signature forms and `concept <Name>` bind one.
func TestTokenize_SingleIdentifierHeaderNamesItself(t *testing.T) {
	svc := &Service{}
	tokens := svc.Tokenize("trait isActiveRecord {\n  return active == true\n}")
	if got := firstTokenType(tokens, "trait"); got != "keyword" {
		t.Errorf("trait rendered as %q, want keyword", got)
	}
	if got := firstTokenType(tokens, "isActiveRecord"); got != "identifier" {
		t.Errorf("isActiveRecord rendered as %q, want identifier", got)
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

// TestTokenize_HeaderBlockCommentSortedBeforeLineComment is the regression net
// for memql#2585: extractComments appended block comments after line comments,
// so a leading /* header */ landed AFTER a later // line comment. Tokenize
// output must be globally sorted by (line, col), so the header block comment
// precedes the trailing line comment and no consumer (notably the LSP delta
// encoder) drops it on a backwards delta.
func TestTokenize_HeaderBlockCommentSortedBeforeLineComment(t *testing.T) {
	src := "/* header */\nconcept X {\n  a int // trailing\n}\n"
	svc := &Service{}
	tokens := svc.Tokenize(src)

	// Globally sorted by (line, col): no token starts before its predecessor.
	for i := 1; i < len(tokens); i++ {
		if tokenBefore(tokens[i], tokens[i-1]) {
			t.Fatalf("tokens not globally sorted at %d: %+v came before %+v",
				i, tokens[i].Range.Start, tokens[i-1].Range.Start)
		}
	}

	// Both comment tokens survive, header (line 1) before trailing (line 3).
	var comments []Token
	for _, tk := range tokens {
		if tk.Type == "comment" {
			comments = append(comments, tk)
		}
	}
	if len(comments) != 2 {
		t.Fatalf("want 2 comment tokens (header + trailing), got %d: %+v", len(comments), comments)
	}
	if comments[0].Range.Start.Line != 1 {
		t.Errorf("first comment should be the line-1 header, got line %d (%q)",
			comments[0].Range.Start.Line, comments[0].Literal)
	}
	if comments[1].Range.Start.Line != 3 {
		t.Errorf("second comment should be the line-3 trailing comment, got line %d (%q)",
			comments[1].Range.Start.Line, comments[1].Literal)
	}
}

// TestTokenize_NoCommentInsideStringLiteral is the regression net for the
// second memql#2585 defect: a `/*` inside a string literal minted a spurious
// comment token overlapping the lexer's string token (LSP disallows overlaps).
func TestTokenize_NoCommentInsideStringLiteral(t *testing.T) {
	src := "concept X {\n  a string = \"/* not */\"\n}\n"
	svc := &Service{}
	tokens := svc.Tokenize(src)

	for _, tk := range tokens {
		if tk.Type == "comment" {
			t.Fatalf("no comment token expected (the /* is inside a string), got %+v", tk)
		}
	}
	// No overlap: on any single line, each token starts at or after the
	// previous token's end column.
	for i := 1; i < len(tokens); i++ {
		prev, cur := tokens[i-1], tokens[i]
		if cur.Range.Start.Line == prev.Range.End.Line &&
			cur.Range.Start.Column < prev.Range.End.Column {
			t.Errorf("overlapping tokens: %+v (%q) overlaps %+v (%q)",
				cur.Range, cur.Literal, prev.Range, prev.Literal)
		}
	}
}
