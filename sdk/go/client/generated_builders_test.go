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
// dangling comma after the prefix `<kind> name(`. Pre-fix the separator
// guard was the hardcoded `b.Len() > 17`, always true for this 33-char
// function name, producing `... updateParticipantPresence(, ...)`.
//
// Story 9 (#2335): the builder now emits the kind-prefixed, named-args
// invocation form `mutation updateParticipantPresence(participantId: ...)`,
// not the legacy object-literal wrapper.
func TestGeneratedBuilder_OmittedLeadingOptionalHasNoDanglingComma(t *testing.T) {
	got := UpdateParticipantPresenceBuild(UpdateParticipantPresenceArgs{
		// PresenceId intentionally omitted -- the leading optional field.
		ParticipantId: "v1:cognition:participant:p1",
		PartitionId:   "v1:cognition:space:s1",
		State:         "idle",
		Label:         "probe",
	})

	if strings.Contains(got, "(,") {
		t.Fatalf("dangling comma after prefix (memql#1319 regression): %s", got)
	}
	wantPrefix := `mutation updateParticipantPresence(participantId: `
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
	if strings.Contains(got, "(,") || strings.Contains(got, ", )") {
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

// TestGeneratedBuilder_CampaignSendActionsAndIntegrationStatus pins the
// five builtins memql#4239 put on the generated surface -- the four
// operator send actions and the integration-status read -- against the
// REAL generated builders. The exact strings are load-bearing: they are
// the wire forms the portal's tests assert on
// (clients/portal/test/campaignAuthoring.test.tsx,
// clients/portal/test/integrations.test.tsx), and the portal now composes
// them through these builders' TypeScript twins rather than by hand. A
// builtin that drops out of the @sdk set, or a generator change to the
// kind-prefixed invocation form, fails here first.
func TestGeneratedBuilder_CampaignSendActionsAndIntegrationStatus(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{
			name: "campaignStartSend",
			got:  CampaignStartSendBuild(CampaignStartSendArgs{CampaignId: "camp-1"}),
			want: `builtin campaignStartSend(campaignId: "camp-1")`,
		},
		{
			name: "campaignScheduleSend",
			got: CampaignScheduleSendBuild(CampaignScheduleSendArgs{
				CampaignId:  "camp-1",
				ScheduledAt: "2026-09-01T09:00:00Z",
			}),
			want: `builtin campaignScheduleSend(campaignId: "camp-1", scheduledAt: "2026-09-01T09:00:00Z")`,
		},
		{
			name: "campaignPauseSend",
			got:  CampaignPauseSendBuild(CampaignPauseSendArgs{CampaignId: "camp-1"}),
			want: `builtin campaignPauseSend(campaignId: "camp-1")`,
		},
		{
			name: "campaignResumeSend",
			got:  CampaignResumeSendBuild(CampaignResumeSendArgs{CampaignId: "camp-1"}),
			want: `builtin campaignResumeSend(campaignId: "camp-1")`,
		},
		{
			// probe is an optional bool: unset means "the configuration
			// question only", and the Go builder omits it unless ProbeSet.
			name: "integrationStatus (configuration read)",
			got:  IntegrationStatusBuild(IntegrationStatusArgs{}),
			want: `builtin integrationStatus()`,
		},
		{
			name: "integrationStatus (live probe)",
			got:  IntegrationStatusBuild(IntegrationStatusArgs{Probe: true, ProbeSet: true}),
			want: `builtin integrationStatus(probe: true)`,
		},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s: call = %q, want %q", tc.name, tc.got, tc.want)
		}
		mustParseCall(t, tc.got)
	}
}
