package baseparser

import "testing"

// strip_line_comment_test.go -- memql#3120.
//
// The completed-escape case is the defect; everything else is here so a
// regression cannot hide behind it, and so a rewrite that simply returned the
// line unchanged (which would pass the defect case) fails.

func TestStripLineComment(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string
	}{
		{
			// THE DEFECT. A one-byte lookback sees `\` before the closing
			// quote, decides it is escaped, stays in string state forever, and
			// returns the line with its comment attached.
			name: `literal ending in a completed \\ escape`,
			line: `path: "C:\\"  // a windows path`,
			want: `path: "C:\\"  `,
		},
		{
			name: `two completed escapes`,
			line: `x: "a\\\\" // comment`,
			want: `x: "a\\\\" `,
		},
		{
			// The case the lookback got RIGHT, kept so the fix does not trade
			// one failure for the other.
			name: `escaped quote does not close the string`,
			line: `msg: "he said \" // not a comment"`,
			want: `msg: "he said \" // not a comment"`,
		},
		{
			name: `escaped quote then a real comment`,
			line: `msg: "he said \"hi\"" // greeting`,
			want: `msg: "he said \"hi\"" `,
		},
		{
			name: `comment inside a string is preserved`,
			line: `url: "https://example.com"`,
			want: `url: "https://example.com"`,
		},
		{
			name: `plain trailing comment`,
			line: `field string // a field`,
			want: `field string `,
		},
		{
			name: `whole line is a comment`,
			line: `// just a comment`,
			want: ``,
		},
		{
			name: `no comment at all`,
			line: `field string`,
			want: `field string`,
		},
		{
			name: `single slash is not a comment`,
			line: `ratio: 1/2`,
			want: `ratio: 1/2`,
		},
		{
			name: `empty`,
			line: ``,
			want: ``,
		},
		{
			// A trailing backslash inside the literal must not walk the index
			// past the end of the line.
			name: `unterminated string ending in a backslash`,
			line: `x: "abc\`,
			want: `x: "abc\`,
		},
		{
			name: `unterminated string swallows the comment`,
			line: `x: "abc // not a comment`,
			want: `x: "abc // not a comment`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := StripLineComment(tc.line); got != tc.want {
				t.Errorf("StripLineComment(%q)\n got %q\nwant %q", tc.line, got, tc.want)
			}
		})
	}
}
