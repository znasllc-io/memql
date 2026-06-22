package id

import (
	"strings"
	"testing"
)

// TestValidateShortId pins the centralized rule (the single source of
// truth that memory-nodes Concept.validateShortId delegates to).
func TestValidateShortId(t *testing.T) {
	cases := []struct {
		name    string
		concept string
		shortId string
		wantErr bool
	}{
		{"bare slug", "v1:agents:agent", "trainer-agent-30bf", false},
		{"bare uuid", "v1:planner:plan", "474e57df-1234-5678-9abc-def012345678", false},
		{"empty handled downstream", "v1:planner:plan", "", false},
		{"concept-prefixed", "v1:agents:agent", "v1:agents:agent:abc", false},
		{"bare check with no concept context", "", "proactive-abc-def", false},
		// The #1712 failing shape: colon-bearing compound used as shortId.
		{"colon compound rejected", "v1:planner:plan", "proactive:d6ac657f:1781827518721614124", true},
		{"foreign concept prefix rejected", "v1:planner:plan", "v1:agents:agent:abc", true},
		{"with-no-concept colon rejected", "", "train:abc", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateShortId(tc.concept, tc.shortId)
			if tc.wantErr && err == nil {
				t.Fatalf("ValidateShortId(%q, %q) = nil; want error", tc.concept, tc.shortId)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("ValidateShortId(%q, %q) = %v; want nil", tc.concept, tc.shortId, err)
			}
		})
	}
}

// TestSystemNodeShortIdGeneratorsAreValid is the guard the issue asks
// for: every id a system automation generator produces must pass the
// engine's ID validator -- for ANY concept -- regardless of how
// colon-laden the source inputs are. This is the regression test for
// the reactive-loop "proactive:<resp>:<nanos>" bug (#1712).
func TestSystemNodeShortIdGeneratorsAreValid(t *testing.T) {
	// Colon-laden, realistic inputs drawn from the live failure.
	inputs := []string{
		"v1:planner:responsibility:d6ac657f-1234-5678-9abc-def012345678",
		"v1:knowledge:knowledgeDomain:sales-playbook",
		"v1:planner:plan:abc-123",
		"bare-already",
		"", // empty label/context edge case
	}
	concepts := []string{"v1:planner:plan", "v1:agents:agent", ""}

	check := func(t *testing.T, got string) {
		t.Helper()
		if strings.ContainsRune(got, ':') {
			t.Fatalf("generated id %q contains a colon", got)
		}
		for _, c := range concepts {
			if err := ValidateShortId(c, got); err != nil {
				t.Fatalf("generated id %q rejected for concept %q: %v", got, c, err)
			}
		}
	}

	for _, in := range inputs {
		// Unique generator (reactive loop / refresh cron).
		check(t, NewSystemNodeShortId("proactive", in))
		check(t, NewSystemNodeShortId("refresh", in))
		// Deterministic generator (training mint).
		check(t, SystemNodeShortId("train", in))
	}

	// The exact failing case from #1712: a responsibility id fed into the
	// reactive-loop plan-id builder must now yield a valid bare id.
	got := NewSystemNodeShortId("proactive", "v1:planner:responsibility:d6ac657f")
	if err := ValidateShortId("v1:planner:plan", got); err != nil {
		t.Fatalf("#1712 regression: reactive-loop plan id %q invalid: %v", got, err)
	}
	if !strings.HasPrefix(got, "proactive-d6ac657f-") {
		t.Fatalf("expected readable provenance prefix, got %q", got)
	}
}

// TestNewSystemNodeShortIdUnique confirms the unique generator never
// collides across rapid successive mints (it appends a fresh token).
func TestNewSystemNodeShortIdUnique(t *testing.T) {
	const n = 1000
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		got := NewSystemNodeShortId("proactive", "v1:planner:responsibility:same")
		if _, dup := seen[got]; dup {
			t.Fatalf("collision on iteration %d: %q", i, got)
		}
		seen[got] = struct{}{}
	}
}

// TestSystemNodeShortIdDeterministic confirms the deterministic
// generator is idempotent (same input -> same id), which the training
// mint relies on for one-run-per-approval.
func TestSystemNodeShortIdDeterministic(t *testing.T) {
	a := SystemNodeShortId("train", "v1:planner:plan:abc-123")
	b := SystemNodeShortId("train", "v1:planner:plan:abc-123")
	if a != b {
		t.Fatalf("SystemNodeShortId not deterministic: %q != %q", a, b)
	}
	if a != "train-abc-123" {
		t.Fatalf("expected readable deterministic id %q, got %q", "train-abc-123", a)
	}
}
