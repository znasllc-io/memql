package memql

import (
	"regexp"
	"testing"
	"unicode"
)

// The whitespace class, pinned rune by rune.
//
// The corpus sweep in shortid_fixpoint_2981_test.go is only as good as its
// alphabet -- which is memql#2981's own closing argument, made after #2978
// "verified" a wrong formulation over 349,524 values whose alphabet could not
// contain a counterexample.
//
// The first fix for #2981 repeated the mistake one layer in. It guarded the
// boundary with RE2's `[[:space:]]`, which is the ASCII class only, while
// BareShortId trims with strings.TrimSpace, which is unicode.IsSpace. So the
// fork closed for U+0020 and stayed open for six other codepoints:
//
//	bare  " abc"                       -> shortId "abc"
//	canon "v1:cluster:deployment: abc" -> shortId " abc"
//
// Two rows for one logical deployment, which is what authoring-rules §20
// exists to prevent, and both spellings passed the guard.
//
// A count moving in a sweep is a weak signal for that. This names every rune
// the gap consisted of, so a future pattern that reintroduces the ASCII/Unicode
// split fails with the codepoint in the test name.
func TestDeploymentIDPatternRejectsEveryWhitespaceRune(t *testing.T) {
	const canonicalPrefix = "v1:cluster:deployment:"
	re := regexp.MustCompile(deploymentIDPattern(t))

	cases := []struct {
		name string
		r    rune
	}{
		{"U+0009_TAB", '\t'},
		{"U+000A_LF", '\n'},
		{"U+000B_VT", '\v'},
		{"U+000C_FF", '\f'},
		{"U+000D_CR", '\r'},
		{"U+0020_SPACE", ' '},
		{"U+0085_NEL", ''},
		{"U+00A0_NBSP", ' '},
		{"U+1680_OGHAM_SPACE", ' '},
		{"U+2000_EN_QUAD", ' '},
		{"U+2003_EM_SPACE", ' '},
		{"U+200A_HAIR_SPACE", ' '},
		{"U+2028_LINE_SEPARATOR", ' '},
		{"U+2029_PARAGRAPH_SEPARATOR", ' '},
		{"U+202F_NARROW_NBSP", ' '},
		{"U+205F_MEDIUM_MATH_SPACE", ' '},
		{"U+3000_IDEOGRAPHIC_SPACE", '　'},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !unicode.IsSpace(tc.r) {
				t.Fatalf("test bug: U+%04X is not unicode.IsSpace, so BareShortId would not "+
					"trim it and it does not belong in this table", tc.r)
			}
			bare := string(tc.r) + "abc"
			canonical := canonicalPrefix + bare

			if re.MatchString(bare) {
				t.Errorf("the boundary accepts bare %q, which BareShortId trims to %q while the "+
					"canonical spelling keeps it verbatim -- the pair derives two ids for one "+
					"logical deployment (memql#2981)", bare, BareShortId(bare))
			}
			if re.MatchString(canonical) {
				t.Errorf("the boundary accepts canonical %q -> %q, which differs from bare %q -> %q",
					canonical, BareShortId(canonical), bare, BareShortId(bare))
			}
		})
	}
}

// The counterpart, so the table above cannot be satisfied by a pattern that
// rejects everything: the shapes callers actually send must still be accepted.
func TestDeploymentIDPatternStillAcceptsRealIds(t *testing.T) {
	re := regexp.MustCompile(deploymentIDPattern(t))

	for _, v := range []string{
		"550e8400-e29b-41d4-a716-446655440000",                       // id.NewShortId is a uuid
		"v1:cluster:deployment:550e8400-e29b-41d4-a716-446655440000", // its canonical form
		"dep-1",                   // examples/deploypack fixtures
		"dep_e2e",                 //
		"v1:cluster:deployment:x", //
	} {
		if !re.MatchString(v) {
			t.Errorf("the boundary rejects %q, which callers legitimately send. Constraining "+
				"deploymentId is a wire-contract narrowing on a generated-SDK argument; it must "+
				"stay wide enough for every id the tree actually mints.", v)
		}
	}

	// And the aliasing prefix stays rejected: an unpinned prefix let
	// `v1:ns:Name:x` and `x` collapse onto one composite id (#2980's class on
	// the leading part).
	for _, v := range []string{"v1:ns:Name:x", "v9:zz:Q_1:x", "v1:cluster:deployment:"} {
		if re.MatchString(v) {
			t.Errorf("the boundary accepts %q; the prefix must be pinned to this concept's own "+
				"canonical form, or two distinct deploymentIds collapse onto one id", v)
		}
	}
}
