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

// A quote that opens but never closes must not make the splitter swallow the
// rest of the input silently on the FIRST byte either -- the old code indexed
// s[i-1] with i possibly 0, which is a panic waiting on a leading quote in
// string state. Guarded here because the reuse below changes that code path.
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
