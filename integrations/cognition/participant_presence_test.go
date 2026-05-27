package cognition

import (
	"strings"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// TestPresenceRecordId pins the contract for the deterministic presence
// row id: it MUST be a bare slug with no colons so the concept's
// validateShortId accepts it. Handing in the canonical participant id
// (`v1:cognition:participant:<short>`) used to land verbatim, which
// failed validation because it's neither a bare slug nor prefixed by
// the presence concept name. Every silent upsert error from
// cognition_handler.go (the `_ = c.upsertParticipantPresence(...)`
// pattern) flowed from that bug. Closes copresent#114.
func TestPresenceRecordId(t *testing.T) {
	cases := []struct {
		name          string
		participantId string
		want          string
	}{
		{
			name:          "canonical participant id",
			participantId: "v1:cognition:participant:abc123",
			want:          "abc123",
		},
		{
			name:          "canonical participant id with dash suffix",
			participantId: "v1:cognition:participant:user-abc-456",
			want:          "user-abc-456",
		},
		{
			name:          "bare slug (legacy / test fixture)",
			participantId: "abc123",
			want:          "abc123",
		},
		{
			name:          "whitespace gets trimmed",
			participantId: "  v1:cognition:participant:xyz  ",
			want:          "xyz",
		},
		{
			name:          "empty -> empty",
			participantId: "",
			want:          "",
		},
		{
			name:          "whitespace only -> empty",
			participantId: "   ",
			want:          "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := presenceRecordId(tc.participantId)
			if got != tc.want {
				t.Fatalf("presenceRecordId(%q) = %q, want %q",
					tc.participantId, got, tc.want)
			}
		})
	}
}

// TestPresenceRecordIdNeverContainsColon pins the structural property
// the validation layer cares about: the presence record id MUST be a
// bare slug (no colons) so the concept's validateShortId accepts it.
// The presence concept name is `v1:cognition:participant:presence`,
// so any colon-bearing record id would either need to be already
// prefixed with that full concept-name+":" or it gets rejected --
// the storage layer composes the canonical row id by prepending the
// concept name itself, so feeding it a bare slug is the right shape.
//
// Regression: the prior `return pid` returned the entire canonical
// participant id (`v1:cognition:participant:<short>`), which has
// colons but doesn't start with `v1:cognition:participant:presence:`.
// validateShortId rejected every call; every upsert in
// cognition_handler.go silently dropped the error.
// Closes copresent#114.
func TestPresenceRecordIdNeverContainsColon(t *testing.T) {
	const conceptName = memoryNodes.ConceptCognitionParticipantPresence

	inputs := []string{
		"v1:cognition:participant:abc123",
		"v1:cognition:participant:user-30bf564497aabf77",
		"abc123",
		"some-bare-slug",
	}
	for _, in := range inputs {
		t.Run(in, func(t *testing.T) {
			got := presenceRecordId(in)
			if got == "" {
				t.Fatalf("presenceRecordId(%q) = empty", in)
			}
			if strings.ContainsRune(got, ':') {
				t.Fatalf("presenceRecordId(%q) = %q; must not contain "+
					"':' -- validateShortId on concept %q rejects "+
					"colon-bearing ids that don't start with %q",
					in, got, conceptName, conceptName+":")
			}
		})
	}
}
