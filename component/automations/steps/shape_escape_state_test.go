package steps

import "testing"

// shape_escape_state_test.go -- memql#3120.
//
// Four scanners in shape.go decided whether a quote closed a string literal by
// looking ONE BYTE BACK for a backslash. That cannot tell an escaped quote
// (`\"`) from a quote following a COMPLETED escape (`\\"`), so a value ending
// in a backslash pair -- a Windows path is the everyday example -- left each
// scanner stuck inside the literal.
//
// Every case below is a literal ending in `\\` and is confirmed to fail
// against the pre-fix scanners.

func TestScanToMatchingDelimHandlesCompletedEscapes(t *testing.T) {
	cases := []struct {
		name          string
		src           string
		open, closing byte
		wantEnd       int
		wantClosed    bool
	}{
		{
			// THE DEFECT. The lookback sees `\` before the literal's closing
			// quote, stays in quote state, and never counts the `)`.
			name: `paren after a literal ending in \\`,
			src:  `("C:\\")rest`,
			open: '(', closing: ')',
			wantEnd: 8, wantClosed: true,
		},
		{
			name: `brace after a literal ending in \\`,
			src:  `{path: "C:\\"}rest`,
			open: '{', closing: '}',
			wantEnd: 14, wantClosed: true,
		},
		{
			name: `bracket after a literal ending in \\`,
			src:  `["a\\", "b"]rest`,
			open: '[', closing: ']',
			wantEnd: 12, wantClosed: true,
		},
		{
			// Single quotes are string delimiters here too -- the reason this
			// scanner stays local instead of delegating to a `"`-only helper.
			name: `single-quoted literal ending in \\`,
			src:  `('C:\\')rest`,
			open: '(', closing: ')',
			wantEnd: 8, wantClosed: true,
		},
		{
			// The case the lookback got right; kept so the fix does not trade
			// one failure for the other.
			name: `escaped quote does not close the literal`,
			src:  `("he said \")")rest`,
			open: '(', closing: ')',
			wantEnd: 15, wantClosed: true,
		},
		{
			name: `a delimiter inside a literal is not counted`,
			src:  `("(((")rest`,
			open: '(', closing: ')',
			wantEnd: 7, wantClosed: true,
		},
		{
			name: `nesting still counts`,
			src:  `((a)(b))rest`,
			open: '(', closing: ')',
			wantEnd: 8, wantClosed: true,
		},
		{
			// NEGATIVE CONTROL. Without it, a scanner that always reported
			// "closed" would pass every case above.
			name: `genuinely unmatched reports not-closed`,
			src:  `("a\\"`,
			open: '(', closing: ')',
			wantEnd: 6, wantClosed: false,
		},
		{
			name: `unterminated literal reports not-closed`,
			src:  `("abc`,
			open: '(', closing: ')',
			wantEnd: 5, wantClosed: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			end, closed := scanToMatchingDelim(tc.src, tc.open, tc.closing)
			if closed != tc.wantClosed {
				t.Fatalf("scanToMatchingDelim(%q) closed = %v, want %v", tc.src, closed, tc.wantClosed)
			}
			if closed && end != tc.wantEnd {
				t.Errorf("scanToMatchingDelim(%q) end = %d (%q), want %d (%q)",
					tc.src, end, tc.src[:end], tc.wantEnd, tc.src[:tc.wantEnd])
			}
		})
	}
}
