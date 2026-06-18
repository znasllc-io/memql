package memoryNodes

import "testing"

// An OPTIONAL datetime concept field must accept the "unset" sentinels
// (empty string "" and JSON null) in addition to a valid RFC3339 value,
// while a REQUIRED datetime field stays strict. Garbage strings are
// rejected in both cases. (memql#1629)
func TestConceptOptionalDatetimeAcceptsEmptyAndNull(t *testing.T) {
	content := []byte(`
@description("Probe concept for optional/required datetime handling.")
concept Probe {
  startedAt    datetime  @required
  completedAt  datetime
}
`)
	concept, err := ParseConceptMemQL(content, "v1/test/probe")
	if err != nil {
		t.Fatalf("ParseConceptMemQL failed: %v", err)
	}

	valid := "2026-06-18T00:00:00Z"

	type tc struct {
		name      string
		completed any
		wantErr   bool
	}
	cases := []tc{
		{"optional-empty", "", false},
		{"optional-null", nil, false},
		{"optional-valid", valid, false},
		{"optional-garbage", "soon", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			payload := map[string]any{"startedAt": valid, "completedAt": c.completed}
			err := concept.validate("definition", payload)
			if c.wantErr && err == nil {
				t.Fatalf("expected validation error for completedAt=%v, got nil", c.completed)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("unexpected validation error for completedAt=%v: %v", c.completed, err)
			}
		})
	}

	// Required datetime stays strict: empty / null / garbage all rejected,
	// only a real RFC3339 value passes.
	for _, bad := range []any{"", nil, "soon"} {
		if err := concept.validate("definition", map[string]any{"startedAt": bad, "completedAt": ""}); err == nil {
			t.Fatalf("required startedAt=%v should have been rejected", bad)
		}
	}
	if err := concept.validate("definition", map[string]any{"startedAt": valid, "completedAt": ""}); err != nil {
		t.Fatalf("required startedAt with a valid timestamp should pass: %v", err)
	}
}
