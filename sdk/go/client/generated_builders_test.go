package client

import (
	"strings"
	"testing"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// mustParseCall asserts a generated builder's output is a syntactically
// valid MemQL call expression -- the engine-side contract that
// memql#1319 broke (dangling comma -> parse error at the engine).
func mustParseCall(t *testing.T, call string) {
	t.Helper()
	if _, err := langparser.ParseExpression(call); err != nil {
		t.Fatalf("generated call does not parse: %v\ncall: %s", err, call)
	}
}

// TestGeneratedBuilder_OmittedLeadingOptionalHasNoDanglingComma pins
// memql#1319 against the REAL generated builder: omitting the leading
// optional arg (PresenceId) of a long-named mutation must not emit a
// dangling comma after `({`. Pre-fix the separator guard was the
// hardcoded `b.Len() > 17`, always true for this 33-char function
// name, producing `updateParticipantPresence({, ...})`.
func TestGeneratedBuilder_OmittedLeadingOptionalHasNoDanglingComma(t *testing.T) {
	got := UpdateParticipantPresenceBuild(UpdateParticipantPresenceArgs{
		// PresenceId intentionally omitted -- the leading optional field.
		ParticipantId: "v1:cognition:participant:p1",
		PartitionId:   "v1:cognition:space:s1",
		State:         "idle",
		Label:         "probe",
	})

	if strings.Contains(got, "({,") {
		t.Fatalf("dangling comma after prefix (memql#1319 regression): %s", got)
	}
	wantPrefix := `updateParticipantPresence({participantId: `
	if !strings.HasPrefix(got, wantPrefix) {
		t.Fatalf("call = %q, want prefix %q", got, wantPrefix)
	}
	mustParseCall(t, got)
}

// TestGeneratedBuilder_NilObjectArgsAreOmitted pins memql#1321 against
// the REAL generated builder: optional object args left nil must be
// omitted entirely, not rendered as `{}` (which the engine treats as a
// real empty value and runs through concept validation). With a real
// map the field must render. This is what makes
// createSessionForParticipant callable from the Go SDK both
// with streams omitted and with a real object.
func TestGeneratedBuilder_NilObjectArgsAreOmitted(t *testing.T) {
	// All optional objects nil -> none of them appear.
	got := CreateSessionForParticipantBuild(CreateSessionForParticipantArgs{
		PartitionId:   "v1:cognition:space:s1",
		ParticipantId: "v1:cognition:participant:p1",
	})
	for _, absent := range []string{"streams", "humanInput", "aiOutput"} {
		if strings.Contains(got, absent) {
			t.Errorf("nil optional object %q must be omitted, got: %s", absent, got)
		}
	}
	if strings.Contains(got, "({,") || strings.Contains(got, ", }") {
		t.Fatalf("malformed separators: %s", got)
	}
	mustParseCall(t, got)

	// Real object -> the field renders with its content.
	got = CreateSessionForParticipantBuild(CreateSessionForParticipantArgs{
		PartitionId:   "v1:cognition:space:s1",
		ParticipantId: "v1:cognition:participant:p1",
		Streams:       map[string]any{"realtimeSessionId": "rt-1"},
	})
	if !strings.Contains(got, `streams: {realtimeSessionId: "rt-1"}`) {
		t.Errorf("real streams object must render, got: %s", got)
	}
	mustParseCall(t, got)
}
