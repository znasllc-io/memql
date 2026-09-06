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
// optional arg of a long-named mutation must not emit a dangling comma
// after the prefix `<kind> name(`. Pre-fix the separator guard was the
// hardcoded `b.Len() > 17`, always true for a name this long, producing
// `... advanceMemoryConsolidationCursor(, ...)`.
//
// Story 9 (#2335): the builder emits the kind-prefixed, named-args
// invocation form, not the legacy object-literal wrapper.
//
// It used to drive updateParticipantPresence, which went with the cognition
// concepts (epic memql#4988). What the probe needs is any mutation whose name
// is longer than 17 characters and whose FIRST declared arg is optional.
func TestGeneratedBuilder_OmittedLeadingOptionalHasNoDanglingComma(t *testing.T) {
	got := AdvanceMemoryConsolidationCursorBuild(AdvanceMemoryConsolidationCursorArgs{
		// CursorId intentionally omitted -- the leading optional field.
		Watermark:    "2026-09-06T00:00:00Z",
		EpisodesSeen: 3,
	})

	if strings.Contains(got, "(,") {
		t.Fatalf("dangling comma after prefix (memql#1319 regression): %s", got)
	}
	wantPrefix := `mutation advanceMemoryConsolidationCursor(watermark: `
	if !strings.HasPrefix(got, wantPrefix) {
		t.Fatalf("call = %q, want prefix %q", got, wantPrefix)
	}
	mustParseCall(t, got)
}

// TestGeneratedBuilder_NilObjectArgsAreOmitted pins memql#1321 against
// the REAL generated builder: optional object args left nil must be
// omitted entirely, not rendered as `{}` (which the engine treats as a
// real empty value and runs through concept validation). With a real
// map the field must render. This is what makes a multi-object mutation
// callable from the Go SDK both
// with the optional objects omitted and with a real object. It used to drive
// createSessionForParticipant, which went with the cognition concepts (epic
// memql#4988).
func TestGeneratedBuilder_NilObjectArgsAreOmitted(t *testing.T) {
	// All optional objects nil -> none of them appear.
	got := UpdatePlanStatusBuild(UpdatePlanStatusArgs{
		PlanId: "v1:planner:plan:p1",
		Status: "running",
	})
	for _, absent := range []string{"output", "feedbackRequest", "feedbackResponse", "phases", "estimate", "metrics"} {
		if strings.Contains(got, absent) {
			t.Errorf("nil optional object %q must be omitted, got: %s", absent, got)
		}
	}
	if strings.Contains(got, "(,") || strings.Contains(got, ", )") {
		t.Fatalf("malformed separators: %s", got)
	}
	mustParseCall(t, got)

	// Real object -> the field renders with its content.
	got = UpdatePlanStatusBuild(UpdatePlanStatusArgs{
		PlanId:  "v1:planner:plan:p1",
		Status:  "running",
		Metrics: map[string]any{"tokensSpent": "12"},
	})
	if !strings.Contains(got, `metrics: {tokensSpent: "12"}`) {
		t.Errorf("real metrics object must render, got: %s", got)
	}
	mustParseCall(t, got)
}

// TestGeneratedBuilder_CampaignSendActionsAndIntegrationStatus pins the
// five builtins memql#4239 put on the generated surface -- the four
// operator send actions and the integration-status read -- against the
// REAL generated builders. The exact strings are load-bearing: they are the
// wire forms a client composes through these builders' TypeScript twins
// rather than by hand. The assertions that pinned them from the other side
// were the portal's (campaignAuthoring.test.tsx, integrations.test.tsx) and
// went with it in epic memql#4984, which makes THIS file the remaining
// guard rather than half of a pair -- worth knowing before weakening it. A
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
