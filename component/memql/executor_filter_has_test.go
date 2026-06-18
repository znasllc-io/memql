package memql

import "testing"

// executor_filter_has_test.go guards memql#1674: the `has` array-membership
// operator (the desugared form the parser emits for `<scalar> in
// payload.<arrayField>`, #976) was supported in the SQL fast-path but NOT in
// the in-memory comparison path (compareScalarValues), so any query that fell
// back to post-filter / non-pushdown evaluation failed with
// `operator "has" is not supported for payload comparisons`. This broke
// notesSearch, queryNotesByTag, and querySkillNeedsRefresh whenever tagged /
// array data existed. These cases pin the in-memory OpHas semantics:
// `actual` is the array field value, `expected` is the membership scalar.

func TestCompareScalarValues_OpHas(t *testing.T) {
	cases := []struct {
		name     string
		actual   any
		expected any
		want     bool
	}{
		{"string-in-array", []any{"red", "green", "blue"}, "green", true},
		{"string-not-in-array", []any{"red", "green"}, "blue", false},
		{"typed-string-slice", []string{"alpha", "beta"}, "beta", true},
		{"number-in-array", []any{float64(1), float64(2), float64(3)}, float64(2), true},
		{"number-not-in-array", []any{float64(1), float64(2)}, float64(9), false},
		{"empty-array", []any{}, "x", false},
		{"non-array-field", "scalar-not-array", "scalar-not-array", false},
		{"nil-field", nil, "x", false},
		// Heterogeneous array element vs scalar: type mismatch is a
		// non-match, never a query failure.
		{"mixed-array-string-hit", []any{float64(1), "tag"}, "tag", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := compareScalarValues(tc.actual, OpHas, tc.expected)
			if err != nil {
				t.Fatalf("compareScalarValues OpHas returned error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("compareScalarValues(%v has %v) = %v, want %v", tc.actual, tc.expected, got, tc.want)
			}
		})
	}
}
