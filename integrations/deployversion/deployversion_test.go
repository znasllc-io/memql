package deployversion

import (
	"context"
	"encoding/json"
	"testing"
)

func callNext(t *testing.T, current, bump string) (map[string]any, error) {
	t.Helper()
	i := NewIntegration()
	args := map[string]any{"current": current}
	if bump != "" {
		args["bump"] = bump
	}
	nodes, err := i.handleSuggestNextVersion(context.Background(), args, 0)
	if err != nil {
		return nil, err
	}
	if len(nodes) != 1 {
		t.Fatalf("want 1 result node, got %d", len(nodes))
	}
	var payload map[string]any
	if uerr := json.Unmarshal(nodes[0].Payload, &payload); uerr != nil {
		t.Fatalf("unmarshal payload: %v", uerr)
	}
	return payload, nil
}

func TestSuggestNextVersion_Bumps(t *testing.T) {
	cases := []struct {
		current, bump, want string
	}{
		{"0.9.9", "patch", "0.9.10"},
		{"0.9.9", "", "0.9.10"},             // default is patch
		{"2.4.7", "minor", "2.5.0"},         // minor zeros patch
		{"2.4.7", "major", "3.0.0"},         // major zeros minor + patch
		{"2026.6.21", "patch", "2026.6.22"}, // CalVer is just three ints
		{"1.0.0", "major", "2.0.0"},
	}
	for _, c := range cases {
		got, err := callNext(t, c.current, c.bump)
		if err != nil {
			t.Errorf("suggestNextVersion(%q,%q) errored: %v", c.current, c.bump, err)
			continue
		}
		if got["next"] != c.want {
			t.Errorf("suggestNextVersion(%q,%q) = %v, want %q", c.current, c.bump, got["next"], c.want)
		}
		if got["current"] != c.current {
			t.Errorf("expected current echoed as %q, got %v", c.current, got["current"])
		}
	}
}

func TestSuggestNextVersion_Rejects(t *testing.T) {
	bad := []struct{ current, bump string }{
		{"", "patch"},          // empty
		{"1.2", "patch"},       // too few segments
		{"1.2.3.4", "patch"},   // too many
		{"1.2.x", "patch"},     // non-numeric
		{"1.2.-3", "patch"},    // negative
		{"1.2.3-rc1", "patch"}, // pre-release suffix
		{"1.2.3", "sideways"},  // invalid bump part
	}
	for _, c := range bad {
		if _, err := callNext(t, c.current, c.bump); err == nil {
			t.Errorf("suggestNextVersion(%q,%q) should have errored, but did not", c.current, c.bump)
		}
	}
}

// TestParity locks this self-contained semver to the same contract as
// deploycontrol/semver.go: a clean three-part numeric version, no suffix.
func TestParity_ParseAndBump(t *testing.T) {
	v, err := parseSemver(" 1.2.3 ")
	if err != nil {
		t.Fatalf("parseSemver trims + parses: %v", err)
	}
	if v.String() != "1.2.3" {
		t.Fatalf("String() = %q, want 1.2.3", v.String())
	}
	b, err := v.bump("minor")
	if err != nil || b.String() != "1.3.0" {
		t.Fatalf("bump minor = %q (err %v), want 1.3.0", b.String(), err)
	}
}
