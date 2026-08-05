package automations

import (
	"testing"
)

// split_coalesce_escape_test.go -- memql#3046, the second site.
//
// splitCoalesceArgs carries the identical defect to splitTopLevelArgs: a
// one-byte lookback (`s[i-1] != '\\'`) cannot tell an escaped quote from a
// quote that follows a COMPLETED `\\` escape, so a literal ending in a
// backslash pair never leaves string state and swallows the following
// top-level commas.
//
// The shape differs in one way that matters for the fix: this splitter
// supports BOTH `"` and `'` quotes, so it does not reuse the parser package's
// blankCommentsAndStrings (which handles `"` only). See the comment on the
// function itself.
//
// It also fails LATER than the rewriter site: this splits `??` arguments at
// automation-condition EVALUATION time, so a mis-split surfaces during a live
// automation run rather than at boot.

func eqArgs(a, b []string) bool {
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

func TestSplitCoalesceArgs_CompletedEscapeDoesNotSwallowCommas(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "literal ending in a completed backslash escape",
			in:   `"C:\\", args.b, args.c`,
			want: []string{`"C:\\"`, ` args.b`, ` args.c`},
		},
		{
			name: "escaped quote does not end the string",
			in:   `"he said \"hi\", ok", args.b`,
			want: []string{`"he said \"hi\", ok"`, ` args.b`},
		},
		{
			name: "single-quoted literal ending in a backslash pair",
			in:   `'C:\\', args.b`,
			want: []string{`'C:\\'`, ` args.b`},
		},
		{
			name: "single-quoted escaped quote",
			in:   `'it\'s, fine', args.b`,
			want: []string{`'it\'s, fine'`, ` args.b`},
		},
		{
			name: "commas inside a string are never split points",
			in:   `"a,b,c"`,
			want: []string{`"a,b,c"`},
		},
		{
			name: "parenthesised args are not split at depth",
			in:   `f(a, b), args.c`,
			want: []string{`f(a, b)`, ` args.c`},
		},
		{
			name: "plain args are unaffected",
			in:   `args.a, "fallback"`,
			want: []string{`args.a`, ` "fallback"`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := splitCoalesceArgs(tc.in)
			if err != nil {
				t.Fatalf("splitCoalesceArgs(%q): unexpected error %v", tc.in, err)
			}
			if !eqArgs(got, tc.want) {
				t.Errorf("splitCoalesceArgs(%q)\n got %d: %q\nwant %d: %q",
					tc.in, len(got), got, len(tc.want), tc.want)
			}
		})
	}
}

// The existing contract must survive the fix: unbalanced parens are an error,
// and empty input yields no args.
func TestSplitCoalesceArgs_PreservesExistingContract(t *testing.T) {
	if got, err := splitCoalesceArgs("  "); err != nil || got != nil {
		t.Errorf("empty input: got %q, %v; want nil, nil", got, err)
	}
	if _, err := splitCoalesceArgs(`a)`); err == nil {
		t.Error("unbalanced closing paren must still be an error")
	}
}
