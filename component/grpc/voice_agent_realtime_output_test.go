package memql

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// TestBuildRealtimeCitationsClause covers the citations-clause builder
// the realtime output handler (#437) uses to reach byte-for-byte chat-
// render parity with the cascade's insertAIResponse. The clause is the
// exact serialized form that lands on v1:cognition:utterance.citations,
// which the frontend's splitTextAtCitations wraps in chips.
func TestBuildRealtimeCitationsClause(t *testing.T) {
	t.Run("empty input emits no clause", func(t *testing.T) {
		// No citations -> the field is omitted from the insert entirely
		// (no `, citations: []`) so an un-grounded realtime reply is
		// visually identical to an un-grounded text reply.
		assert.Equal(t, "", buildRealtimeCitationsClause(nil))
		assert.Equal(t, "", buildRealtimeCitationsClause([]*memqlv1.AgentTurnCitation{}))
	})

	t.Run("nil and partial entries are dropped", func(t *testing.T) {
		// nil entry, missing domain, and missing phrase are all skipped;
		// a malformed citation must never land a broken chip.
		clause := buildRealtimeCitationsClause([]*memqlv1.AgentTurnCitation{
			nil,
			{DomainId: "", MatchedPhrase: "phrase"},
			{DomainId: "domain", MatchedPhrase: ""},
		})
		assert.Equal(t, "", clause)
	})

	t.Run("valid citations render the parity clause", func(t *testing.T) {
		clause := buildRealtimeCitationsClause([]*memqlv1.AgentTurnCitation{
			{DomainId: "customer_relations", MatchedPhrase: "escalation policy"},
		})
		// Leading `, citations: ` then a JSON array of {domainId,
		// matchedPhrase} -- the same shape insertAIResponse writes.
		assert.True(t, strings.HasPrefix(clause, ", citations: "), "clause=%q", clause)
		assert.Contains(t, clause, `"domainId":"customer_relations"`)
		assert.Contains(t, clause, `"matchedPhrase":"escalation policy"`)
	})

	t.Run("whitespace-only fields are trimmed then dropped", func(t *testing.T) {
		clause := buildRealtimeCitationsClause([]*memqlv1.AgentTurnCitation{
			{DomainId: "   ", MatchedPhrase: "  "},
		})
		assert.Equal(t, "", clause)
	})
}

// TestRealtimeOutputPayloadIsVoiceAgentScoped pins the new realtime
// output message into the voice-agent interceptor allowlist. Without
// this the message would be rejected before reaching the handler, since
// the interceptor restricts the voice-agent service identity to ONLY the
// VoiceAgent* surface (no direct graph writes).
func TestRealtimeOutputPayloadIsVoiceAgentScoped(t *testing.T) {
	payload := &memqlv1.MemqlClientMessage_VoiceAgentRealtimeOutput{
		VoiceAgentRealtimeOutput: &memqlv1.VoiceAgentRealtimeOutput{},
	}
	assert.True(t, isVoiceAgentPayload(payload),
		"VoiceAgentRealtimeOutput must be admitted on the voice-agent stream")
}
