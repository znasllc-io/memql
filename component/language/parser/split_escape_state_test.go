package parser

import (
	"testing"
)

// split_escape_state_test.go -- memql#3046.
//
// splitTopLevelArgs decided whether a quote was escaped with a ONE-BYTE
// LOOKBACK (`s[i-1] != '\\'`). That cannot distinguish an escaped quote from a
// quote that follows a COMPLETED `\\` escape, so a literal whose last content
// byte is a backslash pair read its own closing quote as escaped, never left
// string state, and swallowed every top-level comma after it.
//
// The same class was fixed in component/automations/args_resolution.go under
// memql#2949: escape state is TRACKED, never inferred from the preceding byte.
//
// This runs in the struct-form rewriter, on every authored construct, and it
// fails by silently mis-parsing an argument list rather than by rejecting it.

func eqStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestSplitTopLevelArgs_CompletedEscapeDoesNotSwallowCommas(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want []string
	}{
		{
			// The reported case: a Windows path ending in an escaped
			// backslash. Three arguments collapsed into one.
			name: "literal ending in a completed backslash escape",
			in:   `"C:\\", args.b, args.c`,
			want: []string{`"C:\\"`, ` args.b`, ` args.c`},
		},
		{
			// The case the one-byte lookback got RIGHT, kept so the fix
			// cannot regress it: an escaped quote must not end the string.
			name: "escaped quote does not end the string",
			in:   `"he said \"hi\", ok", args.b`,
			want: []string{`"he said \"hi\", ok"`, ` args.b`},
		},
		{
			name: "escaped backslash then escaped quote",
			in:   `"a\\\"b, c", args.d`,
			want: []string{`"a\\\"b, c"`, ` args.d`},
		},
		{
			name: "regex literal ending in a backslash pair",
			in:   `"^\\d+\\\\", args.pattern`,
			want: []string{`"^\\d+\\\\"`, ` args.pattern`},
		},
		{
			name: "commas inside a string are never split points",
			in:   `"a,b,c"`,
			want: []string{`"a,b,c"`},
		},
		{
			name: "nested call args are not split at depth",
			in:   `f(a, b), args.c`,
			want: []string{`f(a, b)`, ` args.c`},
		},
		{
			name: "plain args are unaffected",
			in:   `args.a, args.b, args.c`,
			want: []string{`args.a`, ` args.b`, ` args.c`},
		},
		{
			name: "empty input yields one empty part",
			in:   ``,
			want: []string{``},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := splitTopLevelArgs(tc.in)
			if !eqStrings(got, tc.want) {
				t.Errorf("splitTopLevelArgs(%q)\n got %d: %q\nwant %d: %q",
					tc.in, len(got), got, len(tc.want), tc.want)
			}
		})
	}
}

// Unterminated and degenerate inputs must not panic.
//
// An earlier version of this comment claimed the old code could index s[i-1]
// with i == 0 on a leading quote. It could not: the old scanner set inStr in
// the `case c == '"'` arm at index i, so its string branch first ran at i+1.
// Measured -- the old code does not panic on any input below. The guard is kept
// because it is free and the splitter has now been rewritten twice, but it is a
// guard rather than a reproduction, and saying so keeps the next reader from
// trusting a hazard that was never there.
func TestSplitTopLevelArgs_LeadingQuoteDoesNotPanic(t *testing.T) {
	for _, in := range []string{`"`, `",a`, `\`, `"a`} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("splitTopLevelArgs(%q) panicked: %v", in, r)
				}
			}()
			_ = splitTopLevelArgs(in)
		}()
	}
}

// A string literal spanning lines must not break the split, and the LEXER is
// the oracle for how many arguments there are.
//
// This is a regression test in the strict sense: a draft of the #3046 fix
// delegated string state to blankCommentsAndStrings, which back then ended a
// literal at a newline (memql#3116 has since made it agree with the lexer, but
// the guard stays -- the splitter still tracks string state itself).
// The lexer does not end a literal there -- it accepts a literal spanning lines and
// returns it as ONE string token (memql#3047 is a separate bug about the line
// counter not advancing through exactly such a literal, which only exists
// because they are accepted). So the blanker closed the string early, the real
// closing quote reopened one, and every following comma was swallowed:
// `"line one\nline two", args.b` split into ONE argument where the lexer sees
// two. That is the identical symptom #3046 exists to remove, reintroduced by
// the fix for it -- and the code being replaced got this case right.
//
// Asserting against the lexer's own comma count rather than a hardcoded number
// is deliberate: the defect in both directions is the splitter disagreeing with
// the lexer about where a string ends, so the lexer is the only oracle that
// cannot drift with the bug.
func TestSplitTopLevelArgs_AgreesWithTheLexer(t *testing.T) {
	for _, in := range []string{
		"\"line one\nline two\", args.b",
		"\"a\nb\", args.b, args.c",
		`"C:\\", args.b, args.c`,
		`args.a, args.b`,
		`f(a, b), args.c`,
		`"a,b", args.c`,
		`"say \"hi\"", args.b`,
	} {
		toks, err := NewLexer(in).Tokenize()
		if err != nil {
			t.Fatalf("lexer refused the fixture %q: %v", in, err)
		}
		// Switch on TYPE, not Literal. A TokenString's Literal is its decoded
		// CONTENT, so a fixture whose string content happens to be "," or "}"
		// would be counted as a separator and silently corrupt the oracle --
		// which is exactly the class of self-deception this test exists to
		// prevent, so it must not contain one.
		want := 1
		depth := 0
		for _, tk := range toks {
			switch tk.Type {
			case TokenParenOpen, TokenBraceOpen, TokenBracketOpen:
				depth++
			case TokenParenClose, TokenBraceClose, TokenBracketClose:
				depth--
			case TokenComma:
				if depth == 0 {
					want++
				}
			}
		}
		if got := len(splitTopLevelArgs(in)); got != want {
			t.Errorf("splitTopLevelArgs(%q) produced %d arguments; the lexer sees %d.\n"+
				"  The splitter and the lexer disagree about where a string ends, which is "+
				"memql#3046 in whichever direction it points.\n  parts: %q",
				in, got, want, splitTopLevelArgs(in))
		}
	}
}
