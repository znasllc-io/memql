package parser

import (
	"strings"
	"testing"
)

// memql#4123: the struct-query body scan reads the first token of each
// physical line as a field keyword, so a boolean split across lines used to
// fail with `unknown struct-query field on line "..."` -- naming a FIELD,
// which reads as a typo rather than as "multi-line values are not
// supported". The byte-identical expression joined onto one line loaded
// fine.
//
// The contract these tests pin: a multi-line field value produces exactly
// what the one-line spelling produces. Not "it parses" -- the same output,
// because a joiner that parsed but reassociated the expression would be a
// worse bug than the one it replaced.

// singleLine is the reference spelling every multi-line case must match.
const multilineFilterReference = `query space q {
  args {
    ownerId string
    status  string
  }
  filter  when(args.ownerId) { ownerId==args.ownerId } && when(args.status) { status==args.status }
}`

func TestQueryMultiLineFilterMatchesSingleLine(t *testing.T) {
	want, err := NormaliseQuerySource(multilineFilterReference)
	if err != nil {
		t.Fatalf("reference (single-line) source failed to rewrite: %v", err)
	}

	cases := []struct {
		name   string
		source string
	}{
		{
			// The break-after-operator style, verbatim from memql#4123.
			name: "trailing operator",
			source: `query space q {
  args {
    ownerId string
    status  string
  }
  filter  when(args.ownerId) { ownerId==args.ownerId } &&
          when(args.status) { status==args.status }
}`,
		},
		{
			// The break-before-operator style.
			name: "leading operator",
			source: `query space q {
  args {
    ownerId string
    status  string
  }
  filter  when(args.ownerId) { ownerId==args.ownerId }
          && when(args.status) { status==args.status }
}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormaliseQuerySource(tc.source)
			if err != nil {
				t.Fatalf("NormaliseQuerySource: %v", err)
			}
			if got != want {
				t.Errorf("multi-line rewrite differs from the single-line spelling\n got: %s\nwant: %s", got, want)
			}
		})
	}
}

// A continuation must never swallow the NEXT field. `shape` and `sort`
// follow a filter without any operator between them, which is precisely the
// case a naive "join every indented line" rule would break.
func TestQueryMultiLineFilterDoesNotSwallowFollowingFields(t *testing.T) {
	source := `query space q {
  args {
    ownerId string
  }
  filter  when(args.ownerId) { ownerId==args.ownerId } &&
          isActiveRecord
  sort    "row.createdAt", "desc"
  shape   spaceFull
}`
	out, err := NormaliseQuerySource(source)
	if err != nil {
		t.Fatalf("NormaliseQuerySource: %v", err)
	}
	for _, want := range []string{`"row.createdAt", "desc"`, `"spaceFull"`, "isActiveRecord"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q, got:\n%s", want, out)
		}
	}
}

// An unbalanced delimiter continues the value even with no operator at the
// seam -- a parenthesised group, and a `when(...) {` guard whose brace opens
// on one line and closes on the next.
func TestQueryMultiLineFilterUnclosedDelimiters(t *testing.T) {
	cases := map[string]string{
		"open paren": `query space q {
  args {
    a string
    b string
  }
  filter  (a==args.a ||
           b==args.b)
}`,
		"open brace": `query space q {
  args {
    a string
  }
  filter  when(args.a) {
            a==args.a
          }
}`,
	}
	for name, source := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NormaliseQuerySource(source); err != nil {
				t.Errorf("NormaliseQuerySource: %v", err)
			}
		})
	}
}

// A brace inside a STRING is not an opener. Without the string-awareness in
// unclosedDelimiters, this regex pattern would read as an unclosed `{` and
// swallow the `shape` line after it.
func TestQueryMultiLineFilterIgnoresBracesInStrings(t *testing.T) {
	source := `query space q {
  args {
    a string
  }
  filter  matchesPattern(a, "^x{2}$")
  shape   spaceFull
}`
	out, err := NormaliseQuerySource(source)
	if err != nil {
		t.Fatalf("NormaliseQuerySource: %v", err)
	}
	if !strings.Contains(out, `"spaceFull"`) {
		t.Errorf("the shape field was swallowed by a brace inside a string literal, got:\n%s", out)
	}
}

// An unrecognised field must still be rejected, and still name itself. The
// joiner sits in front of that check, so a bug there would turn a clear
// authoring error into a confusing one -- or into silence.
func TestQueryUnknownFieldStillRejected(t *testing.T) {
	source := `query space q {
  filter  isActiveRecord
  shpe    spaceFull
}`
	_, err := NormaliseQuerySource(source)
	if err == nil {
		t.Fatal("expected an error for an unknown struct-query field, got nil")
	}
	if !strings.Contains(err.Error(), "shpe") {
		t.Errorf("the error should name the offending line; got: %v", err)
	}
}
