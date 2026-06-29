package baseparser

import (
	"strings"
	"testing"
)

func newBase(input string) *Base {
	b := &Base{}
	b.Init(input, "test")
	return b
}

func TestEOFAndPeek(t *testing.T) {
	b := newBase("ab")
	if b.EOF() {
		t.Fatal("EOF at start of non-empty input")
	}
	if got := b.Peek(); got != 'a' {
		t.Fatalf("Peek = %q, want %q", got, "a")
	}
	if got := b.PeekAt(1); got != 'b' {
		t.Fatalf("PeekAt(1) = %q, want %q", got, "b")
	}
	if got := b.PeekAt(2); got != 0 {
		t.Fatalf("PeekAt past EOF = %q, want 0", got)
	}
	b.Advance()
	b.Advance()
	if !b.EOF() {
		t.Fatal("expected EOF after consuming all input")
	}
	if got := b.Peek(); got != 0 {
		t.Fatalf("Peek at EOF = %q, want 0", got)
	}
}

func TestAdvanceTracksLineCol(t *testing.T) {
	b := newBase("ab\ncd\ne")

	for i := 0; i < 2; i++ {
		b.Advance()
	}
	if b.Line != 1 || b.Col != 3 {
		t.Fatalf("after 'ab': line=%d col=%d, want 1 3", b.Line, b.Col)
	}
	b.Advance() // newline
	if b.Line != 2 || b.Col != 1 {
		t.Fatalf("after newline: line=%d col=%d, want 2 1", b.Line, b.Col)
	}
	b.Advance() // c
	b.Advance() // d
	if b.Line != 2 || b.Col != 3 {
		t.Fatalf("after 'cd': line=%d col=%d, want 2 3", b.Line, b.Col)
	}
	b.Advance() // newline
	b.Advance() // e
	if b.Line != 3 || b.Col != 2 {
		t.Fatalf("after final 'e': line=%d col=%d, want 3 2", b.Line, b.Col)
	}
}

func TestSkipWhitespaceAndComments(t *testing.T) {
	cases := []struct {
		input string
		want  byte
	}{
		{"   abc", 'a'},
		{"\t\n  abc", 'a'},
		{"// line\nabc", 'a'},
		{"/* block */abc", 'a'},
		{"  // line\n  /* block */\nabc", 'a'},
		{"abc", 'a'},
		{"", 0},
	}
	for _, tc := range cases {
		b := newBase(tc.input)
		b.SkipWhitespaceAndComments()
		if got := b.Peek(); got != tc.want {
			t.Errorf("input %q: Peek after skip = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestSkipWhitespaceInline(t *testing.T) {
	b := newBase("  \t\nabc")
	b.SkipWhitespaceInline()
	if b.Peek() != '\n' {
		t.Fatalf("SkipWhitespaceInline stopped at %q, want '\\n'", b.Peek())
	}
}

func TestReadWord(t *testing.T) {
	b := newBase("hello_world42 next")
	if w := b.ReadWord(); w != "hello_world42" {
		t.Fatalf("ReadWord = %q, want hello_world42", w)
	}
	if b.Peek() != ' ' {
		t.Fatalf("Peek after ReadWord = %q, want ' '", b.Peek())
	}

	b2 := newBase("---")
	if w := b2.ReadWord(); w != "" {
		t.Fatalf("ReadWord on non-ident = %q, want \"\"", w)
	}
}

func TestMatchWord(t *testing.T) {
	b := newBase("spec foo")
	if !b.MatchWord("spec") {
		t.Fatal("MatchWord(spec) returned false")
	}
	if b.Peek() != ' ' {
		t.Fatalf("Peek after match = %q, want ' '", b.Peek())
	}
	if b.Col != 5 {
		t.Fatalf("Col = %d, want 5", b.Col)
	}

	b2 := newBase("specs")
	if b2.MatchWord("spec") {
		t.Fatal("MatchWord(spec) on 'specs' returned true; word-boundary check failed")
	}
	if b2.Pos != 0 {
		t.Fatalf("Pos = %d after non-match, want 0", b2.Pos)
	}

	b3 := newBase("spec")
	if !b3.MatchWord("spec") {
		t.Fatal("MatchWord(spec) at EOF boundary returned false")
	}
}

func TestParseParenString(t *testing.T) {
	b := newBase(`("hello world")`)
	v, err := b.ParseParenString()
	if err != nil {
		t.Fatalf("ParseParenString err: %v", err)
	}
	if v != "hello world" {
		t.Fatalf("ParseParenString = %q, want hello world", v)
	}

	b2 := newBase(`(  "with escape \"x\""  )`)
	v2, err := b2.ParseParenString()
	if err != nil {
		t.Fatalf("ParseParenString err: %v", err)
	}
	if v2 != `with escape "x"` {
		t.Fatalf("ParseParenString = %q, want with escape \"x\"", v2)
	}
}

func TestParseParenIdent(t *testing.T) {
	b := newBase(`(space)`)
	v, err := b.ParseParenIdent()
	if err != nil {
		t.Fatalf("ParseParenIdent err: %v", err)
	}
	if v != "space" {
		t.Fatalf("ParseParenIdent = %q, want space", v)
	}
}

func TestParseParenStringList(t *testing.T) {
	b := newBase(`("a", "b", "c")`)
	xs, err := b.ParseParenStringList()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if strings.Join(xs, ",") != "a,b,c" {
		t.Fatalf("list = %v, want [a b c]", xs)
	}

	b2 := newBase(`()`)
	xs2, err := b2.ParseParenStringList()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(xs2) != 0 {
		t.Fatalf("empty list got %d items", len(xs2))
	}
}

func TestParseParenIdentList(t *testing.T) {
	b := newBase(`(space, group, agent)`)
	xs, err := b.ParseParenIdentList()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if strings.Join(xs, ",") != "space,group,agent" {
		t.Fatalf("list = %v, want [space group agent]", xs)
	}
}

func TestParseParenInt(t *testing.T) {
	b := newBase(`(42)`)
	n, err := b.ParseParenInt()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if n != 42 {
		t.Fatalf("ParseParenInt = %d, want 42", n)
	}

	b2 := newBase(`(  0  )`)
	n2, err := b2.ParseParenInt()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if n2 != 0 {
		t.Fatalf("ParseParenInt = %d, want 0", n2)
	}
}

func TestReadQuotedString(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{`"hello"`, "hello"},
		{`"a\nb"`, "a\nb"},
		{`"\\path\\to"`, `\path\to`},
		{`"a\tb\rc"`, "a\tb\rc"},
		{`"slash \/ ok"`, "slash / ok"},
		{`"unknown \z escape"`, `unknown \z escape`},
	}
	for _, tc := range cases {
		b := newBase(tc.input)
		v, err := b.ReadQuotedString()
		if err != nil {
			t.Errorf("input %q: err %v", tc.input, err)
			continue
		}
		if v != tc.want {
			t.Errorf("input %q: got %q, want %q", tc.input, v, tc.want)
		}
	}
}

func TestSkipBalancedParens(t *testing.T) {
	b := newBase(`(a (b "c)d") e) tail`)
	b.SkipBalancedParens()
	if b.Peek() != ' ' {
		t.Fatalf("Peek after skip = %q, want ' '", b.Peek())
	}
	b.SkipWhitespaceInline()
	rest := b.Input[b.Pos:]
	if rest != "tail" {
		t.Fatalf("remaining = %q, want %q", rest, "tail")
	}
}

func TestSkipBalancedBraces(t *testing.T) {
	b := newBase(`{a {b "}c"} d} tail`)
	b.SkipBalancedBraces()
	if b.Peek() != ' ' {
		t.Fatalf("Peek after skip = %q, want ' '", b.Peek())
	}
}

func TestSkipOptionalParens(t *testing.T) {
	b := newBase(`  (x) rest`)
	b.SkipOptionalParens()
	if got := strings.TrimSpace(b.Input[b.Pos:]); got != "rest" {
		t.Fatalf("after optional parens, remaining = %q, want 'rest'", got)
	}

	b2 := newBase(`bare rest`)
	b2.SkipOptionalParens()
	if b2.Pos != 0 {
		t.Fatalf("bare input: Pos = %d, want 0", b2.Pos)
	}
}

func TestFindClosingBrace(t *testing.T) {
	src := `payload.x == "}" && {nested}`
	b := newBase(src)
	idx, err := b.FindClosingBrace(0)
	if err == nil {
		t.Fatalf("expected unbalanced error, got idx=%d", idx)
	}

	src2 := `payload.x == "}" } trailing`
	b2 := newBase(src2)
	idx2, err := b2.FindClosingBrace(0)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if src2[idx2] != '}' {
		t.Fatalf("FindClosingBrace pointed at %q, want '}'", src2[idx2])
	}

	src3 := `a // }
} tail`
	b3 := newBase(src3)
	idx3, err := b3.FindClosingBrace(0)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if src3[idx3] != '}' {
		t.Fatalf("FindClosingBrace skipped line-comment '}', want real '}'")
	}
}

func TestErrorf(t *testing.T) {
	b := newBase("ab\ncd")
	b.Advance()
	b.Advance()
	b.Advance()
	b.Advance()
	err := b.Errorf("oh no: %s", "wrong")
	want := "test:2:2: oh no: wrong"
	if err.Error() != want {
		t.Fatalf("Errorf = %q, want %q", err.Error(), want)
	}
}

func TestValidateConstructAnnotations(t *testing.T) {
	// Plain happy-path: only allow-listed annotations present.
	src := `@enabled
@description("x")
spec activeRowTrait foo {
  return x == 1
}`
	allowed := map[string]bool{"description": true, "enabled": true, "shape": true}
	if err := ValidateConstructAnnotations(src, "spec", allowed); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}

	src2 := `@description("x")
@bogus
spec activeRowTrait foo { return true }`
	err := ValidateConstructAnnotations(src2, "spec", allowed)
	if err == nil {
		t.Fatal("expected error for @bogus")
	}
	if !strings.Contains(err.Error(), "@bogus") {
		t.Fatalf("err = %v, expected mention of @bogus", err)
	}

	// The @use* family is hard-rejected with a migration hint even
	// when the construct's allow-list includes them. Verifies the
	// PR C lockdown: file-top `use <module>.{ ... }` imports replace
	// the per-construct annotations.
	srcUse := `@description("x")
@useShape(participantFull)
spec activeRowTrait foo { return x == 1 }`
	allowedWithUse := map[string]bool{"description": true, "useShape": true}
	if err = ValidateConstructAnnotations(srcUse, "spec", allowedWithUse); err == nil {
		t.Fatal("expected @useShape to be rejected post-lockdown")
	}
	if !strings.Contains(err.Error(), "@useShape") || !strings.Contains(err.Error(), "retired") {
		t.Fatalf("err = %v, expected mention of @useShape + retired", err)
	}

	srcAuto := `@description("ok")
@trigger(event="x")
automation foo {
  // body
}`
	allowedAuto := map[string]bool{"description": true, "trigger": true}
	if err := ValidateConstructAnnotations(srcAuto, "automation", allowedAuto); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestPosPlus(t *testing.T) {
	b := newBase("abcd")
	if got := b.PosPlus(2); got != 2 {
		t.Fatalf("PosPlus(2) = %d, want 2", got)
	}
	if got := b.PosPlus(99); got != 4 {
		t.Fatalf("PosPlus(99) = %d, want 4 (clamped)", got)
	}
}

// TestReadIdentWithHyphens covers the kebab-case-name reader added
// for memql#180 (seed loader silently dropped hyphenated names).
// Hyphens are admitted from the second rune onward, never at the
// start; the function stops at the first non-identifier-non-hyphen
// rune.
func TestReadIdentWithHyphens(t *testing.T) {
	cases := []struct {
		in       string
		wantRead string
		// wantRemain is what should still be at the cursor afterwards.
		wantRemain string
	}{
		{"graphic-designer", "graphic-designer", ""},
		{"music-theory-teacher { ... }", "music-theory-teacher", " { ... }"},
		{"copresent-voice-agents,", "copresent-voice-agents", ","},
		// Plain identifier still works.
		{"assistant", "assistant", ""},
		// Underscore + digit mid-name accepted (same as ReadWord).
		{"foo_bar2-baz", "foo_bar2-baz", ""},
		// Leading hyphen rejected -- must start with letter or _.
		{"-leading", "", "-leading"},
		// Leading digit rejected per Go-identifier convention.
		{"9designer", "", "9designer"},
		// Trailing hyphen is greedily included; no special trim.
		{"trailing-", "trailing-", ""},
	}
	for _, c := range cases {
		b := newBase(c.in)
		got := b.ReadIdentWithHyphens()
		if got != c.wantRead {
			t.Errorf("ReadIdentWithHyphens(%q) read %q, want %q", c.in, got, c.wantRead)
		}
		if remain := c.in[b.Pos:]; remain != c.wantRemain {
			t.Errorf("ReadIdentWithHyphens(%q) left %q at cursor, want %q", c.in, remain, c.wantRemain)
		}
	}
}
