package planner

import (
	"strings"
	"testing"
)

// TestJaccardOverlap pins the catalog-search heuristic's math against
// hand-computed fixtures. The handler treats overlap > 0.60
// (strictly greater) as a near-match -> reject + escalate to
// extendSpecialist instead of minting.
func TestJaccardOverlap(t *testing.T) {
	const threshold = 0.60
	cases := []struct {
		name        string
		a, b        []string
		wantOverlap float64
		wantHit     bool // jaccard > 0.60
	}{
		{
			name:        "identical bundles",
			a:           []string{"workbench", "computer-use"},
			b:           []string{"workbench", "computer-use"},
			wantOverlap: 1.0,
			wantHit:     true,
		},
		{
			name:        "three of four shared -> 0.6 exactly (NOT a hit -- strict >)",
			a:           []string{"a", "b", "c", "d"},
			b:           []string{"a", "b", "c", "e"},
			wantOverlap: 0.6,
			wantHit:     false,
		},
		{
			name:        "four of five shared -> 0.667 (hit)",
			a:           []string{"a", "b", "c", "d", "e"},
			b:           []string{"a", "b", "c", "d", "f"},
			wantOverlap: 4.0 / 6.0,
			wantHit:     true,
		},
		{
			name:        "one of five shared -> 0.111 (miss)",
			a:           []string{"a", "b", "c"},
			b:           []string{"a", "d", "e"},
			wantOverlap: 1.0 / 5.0,
			wantHit:     false,
		},
		{
			name:        "disjoint sets",
			a:           []string{"a", "b"},
			b:           []string{"c", "d"},
			wantOverlap: 0,
			wantHit:     false,
		},
		{
			name:        "empty proposed",
			a:           []string{},
			b:           []string{"a", "b"},
			wantOverlap: 0,
			wantHit:     false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			aSet := unionStringSets(tc.a)
			bSet := unionStringSets(tc.b)
			got := jaccardOverlap(aSet, bSet)
			if abs(got-tc.wantOverlap) > 0.001 {
				t.Errorf("jaccardOverlap = %.4f; want %.4f", got, tc.wantOverlap)
			}
			gotHit := got > threshold
			if gotHit != tc.wantHit {
				t.Errorf("hit-at-threshold(%.2f) = %v; want %v (overlap=%.4f)",
					threshold, gotHit, tc.wantHit, got)
			}
		})
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// TestValidateMintPayload guards the missing-field + bad-tier rejects
// the handler runs before any catalog / authority lookup.
func TestValidateMintPayload(t *testing.T) {
	good := plannerDecision{
		Action:        "mintSkill",
		Slug:          "test-skill",
		Name:          "Test Skill",
		Tier:          "A",
		Justification: "no existing skill covers this gap",
	}
	if err := validateMintPayload(good); err != nil {
		t.Fatalf("expected pass for valid payload, got: %v", err)
	}

	t.Run("missing slug", func(t *testing.T) {
		bad := good
		bad.Slug = ""
		err := validateMintPayload(bad)
		if err == nil || !strings.Contains(err.Error(), "slug") {
			t.Errorf("expected error naming 'slug', got: %v", err)
		}
	})
	t.Run("missing name", func(t *testing.T) {
		bad := good
		bad.Name = ""
		err := validateMintPayload(bad)
		if err == nil || !strings.Contains(err.Error(), "name") {
			t.Errorf("expected error naming 'name', got: %v", err)
		}
	})
	t.Run("missing tier", func(t *testing.T) {
		bad := good
		bad.Tier = ""
		err := validateMintPayload(bad)
		if err == nil || !strings.Contains(err.Error(), "tier") {
			t.Errorf("expected error naming 'tier', got: %v", err)
		}
	})
	t.Run("missing justification", func(t *testing.T) {
		bad := good
		bad.Justification = ""
		err := validateMintPayload(bad)
		if err == nil || !strings.Contains(err.Error(), "justification") {
			t.Errorf("expected error naming 'justification', got: %v", err)
		}
	})
	t.Run("bad tier", func(t *testing.T) {
		bad := good
		bad.Tier = "Z"
		err := validateMintPayload(bad)
		if err == nil || !strings.Contains(err.Error(), `"Z"`) {
			t.Errorf("expected error naming tier 'Z', got: %v", err)
		}
	})
}

// TestContains is a trivial regression net for the slice-containment
// helper the authority gate uses to check skillTierAllowlist
// membership.
func TestContains(t *testing.T) {
	cases := []struct {
		s    []string
		v    string
		want bool
	}{
		{[]string{"A"}, "A", true},
		{[]string{"A", "B"}, "B", true},
		{[]string{"A", "B"}, "C", false},
		{nil, "A", false},
		{[]string{}, "A", false},
	}
	for _, tc := range cases {
		if got := contains(tc.s, tc.v); got != tc.want {
			t.Errorf("contains(%v, %q) = %v; want %v", tc.s, tc.v, got, tc.want)
		}
	}
}
