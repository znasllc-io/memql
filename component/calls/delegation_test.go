package calls

import (
	"math"
	"testing"
	"time"
)

func TestDelegationBudgetCountsPendingAndDoesNotOverflow(t *testing.T) {
	if WithinDelegationBudget(1000, 1500, 1500, 1000, 2000, 10000) {
		t.Fatal("pending commitment overspent period")
	}
	if WithinDelegationBudget(1, math.MaxInt64, 0, 1000, 2000, 10000) {
		t.Fatal("overflow admitted")
	}
	if !WithinDelegationBudget(500, 1500, 9500, 1000, 2000, 10000) {
		t.Fatal("exact remaining budget refused")
	}
}

func TestDelegationRequiresSeparateTopupAuthority(t *testing.T) {
	now := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	r := DelegationRequest{Action: "create", Terms: AgentTermsVersion, Purpose: "Help with the approved conversation", ConductVersion: AgentConductVersion, Participation: "adults_only", Label: "Assistant", Scopes: []string{"read", "host"}, MaxCallCents: 1000, PeriodLimitCents: 5000, LifetimeLimitCents: 10000, MaxParticipants: 4, MaxDurationMinutes: 60, ExpiresAt: now.Add(24 * time.Hour)}
	if !ValidDelegation(r, now) {
		t.Fatal("valid grant refused")
	}
	r.Topup = AutoTopupLimit{Enabled: true, AmountCents: 1000, ThresholdCents: 500, PerChargeLimitCents: 1000, PeriodLimitCents: 2000, LifetimeLimitCents: 4000}
	if ValidDelegation(r, now) {
		t.Fatal("implicit funding authority")
	}
	r.Scopes = append(r.Scopes, "topup")
	if !ValidDelegation(r, now) {
		t.Fatal("explicit bounded authority refused")
	}
	r.Topup.PeriodLimitCents = 999
	if ValidDelegation(r, now) {
		t.Fatal("charge exceeds period")
	}
}

func TestDelegationInvitationApprovalIsExplicitAndOldGrantIsReadable(t *testing.T) {
	now := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	r := DelegationRequest{Action: "create", Terms: AgentTermsVersion, Purpose: "Help with the approved conversation", ConductVersion: AgentConductVersion, Participation: "adults_only", Label: "Assistant", Scopes: []string{"read", "invite", "schedule"}, MaxCallCents: 1000, PeriodLimitCents: 5000, LifetimeLimitCents: 10000, MaxParticipants: 4, MaxDurationMinutes: 60, ExpiresAt: now.Add(24 * time.Hour)}
	if !ValidDelegation(r, now) || ValidDelegationApproval(r, now) || DelegationRecipientAllowed(r, "guest@example.test") {
		t.Fatal("legacy grant must stay readable but cannot send or be approved with implicit recipients")
	}
	r.InvitationRecipients = []string{"Guest@example.test"}
	if !ValidDelegationApproval(r, now) || !DelegationRecipientAllowed(r, " guest@EXAMPLE.test ") || DelegationRecipientAllowed(r, "other@example.test") {
		t.Fatal("recipient allowlist did not constrain normalized mailbox")
	}
	r.AllowAnyRecipient = true
	if ValidDelegationApproval(r, now) {
		t.Fatal("ambiguous recipient scope accepted")
	}
	r.InvitationRecipients = nil
	if !ValidDelegationApproval(r, now) || !DelegationRecipientAllowed(r, "other@example.test") {
		t.Fatal("explicit standing communication authority refused")
	}
	r.ScheduleStart = now.Add(time.Hour)
	if ValidDelegationApproval(r, now) {
		t.Fatal("fixed schedule must be a single call")
	}
	r.MaxCalls = 1
	if !ValidDelegationApproval(r, now) {
		t.Fatal("bounded scheduled grant refused")
	}
}
