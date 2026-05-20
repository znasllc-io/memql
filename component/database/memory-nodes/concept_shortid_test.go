package memoryNodes

import (
	"strings"
	"testing"
)

// TestValidateShortId pins the contract documented in
// docs/core/identifiers.md ("Anti-patterns"). Post-#56 phase 6 the
// canonical form is `concept:shortId` (no partition segment); the
// two landed bugs (seed materializer's "trainerAgent-_system:v1:..."
// compound id, and the checkpoint writer's
// "checkpoint:<executionId>" duplicate concept name) are codified
// as failing inputs here.
func TestValidateShortId(t *testing.T) {
	agent := &Concept{Name: "v1:agents:agent"}
	checkpoint := &Concept{Name: "v1:memql:checkpoint"}

	cases := []struct {
		name    string
		concept *Concept
		nodeId  string
		wantErr bool
		// errSub is a fragment of the expected error message; only
		// checked when wantErr is true. Helps catch silent
		// rule-rewording during future refactors.
		errSub string
	}{
		// Bare shortIds (shape 1) -- always allowed.
		{"bare slug", agent, "trainerAgent-user-30bf564497aabf77fa5875a65a3db455", false, ""},
		{"bare uuid", agent, "474e57df-1234-5678-9abc-def012345678", false, ""},
		{"bare with dash", agent, "bff-local", false, ""},
		{"empty (handled downstream)", agent, "", false, ""},

		// Concept-prefixed (shape 2) -- allowed.
		{"concept-prefixed", agent, "v1:agents:agent:trainerAgent-user-30bf", false, ""},

		// Old partition-prefixed forms are gone post-#56 phase 6.
		{
			"qualified default partition is rejected",
			agent,
			"default:v1:agents:agent:abc",
			true,
			"v1:agents:agent",
		},
		{
			"qualified system partition is rejected",
			agent,
			"_system:v1:agents:agent:abc",
			true,
			"v1:agents:agent",
		},

		// The two landed bugs stay rejected.
		{
			"seed materializer compound id",
			agent,
			"trainerAgent-_system:v1:identity:user:user-30bf564497aabf77fa5875a65a3db455",
			true,
			"v1:agents:agent",
		},
		{
			"checkpoint concept-name-as-prefix",
			checkpoint,
			"checkpoint:exec-20260517-001536-a1z7hw",
			true,
			"v1:memql:checkpoint",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.concept.validateShortId(tc.nodeId)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("validateShortId(%q) returned nil; expected error", tc.nodeId)
				}
				if tc.errSub != "" && !strings.Contains(err.Error(), tc.errSub) {
					t.Fatalf("validateShortId(%q): error %q does not mention expected fragment %q",
						tc.nodeId, err.Error(), tc.errSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateShortId(%q) returned unexpected error: %v", tc.nodeId, err)
			}
		})
	}
}
