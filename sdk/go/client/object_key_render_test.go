package client

import "testing"

// A label key is user-chosen text, not a schema field name, so it routinely
// contains a hyphen -- the workers runbook's own example is `has-blender=true`.
// Rendered bare, that key lexes as `has`, `-`, `blender` and the call fails to
// parse in a caller that did nothing wrong. parseObject takes a quoted key as
// its first branch, which is the grammar's answer for exactly this.
func TestObjectKeysAreQuotedOnlyWhenTheLexerNeedsIt(t *testing.T) {
	cases := map[string]string{
		"ownerUserId": "ownerUserId",   // an identifier stays bare
		"_private":    "_private",      // leading underscore is an identifier
		"a1":          "a1",            // digits after the first char are fine
		"has-blender": `"has-blender"`, // the documented label shape
		"os.name":     `"os.name"`,     // a dot would start a path
		"2fast":       `"2fast"`,       // digit-leading is not an identifier
		"":            `""`,            // empty is not an identifier
	}
	for key, want := range cases {
		if got := renderObjectKey(key); got != want {
			t.Errorf("renderObjectKey(%q) = %s, want %s", key, got, want)
		}
	}
}

func TestRenderMemQLValueQuotesHyphenatedLabelKeys(t *testing.T) {
	got := renderMemQLValue(map[string]any{"has-blender": "true", "os": "darwin"})
	want := `{"has-blender": "true", os: "darwin"}`
	if got != want {
		t.Fatalf("renderMemQLValue = %s, want %s", got, want)
	}
}

// Keys sort on their RAW name, before quoting. Sorting the quoted forms would
// put every hyphenated key first and make this renderer disagree with the
// TypeScript one on the same input -- the two are meant to produce identical
// strings, and sdk/ts/test/memqlValue.test.ts asserts the same ordering.
func TestObjectKeysSortBeforeTheyAreQuoted(t *testing.T) {
	got := renderMemQLValue(map[string]any{"zeta": 1, "alpha": 2, "m-key": 3})
	want := `{alpha: 2, "m-key": 3, zeta: 1}`
	if got != want {
		t.Fatalf("renderMemQLValue = %s, want %s", got, want)
	}
}
