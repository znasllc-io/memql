package email

import (
	"strings"
	"testing"
)

// status_delivery_test.go -- the portal's line in the roll-call is one of the
// layers that reported success (memql#4477).
//
// "Degraded" is the right word for log-only on a developer's machine: the
// integration is doing exactly what was asked of it. On an install that must
// really deliver mail the same state is not a degradation, it is a broken
// integration whose every send is now refused -- and an operations console
// that renders both the same way leaves the operator reading an amber row as
// a known dev posture.

func TestStatusCallsLogOnlyUnhealthyOnAnInstallThatMustDeliver(t *testing.T) {
	clearEmailEnv(t)
	t.Setenv(DomainEnv, "acme.com")

	i := integrationWith(map[string]string{}, map[string]string{})
	report := i.emailReport(ownerContext(), false)

	if report.Configured != AnswerNo {
		t.Errorf("configured = %q, want %q", report.Configured, AnswerNo)
	}
	if report.Health != HealthUnhealthy {
		t.Errorf("health = %q, want %q -- on this install nothing can be sent at all, which is a "+
			"different fact from the log-only dev posture", report.Health, HealthUnhealthy)
	}
	// The detail is what an operator reads before they know which env var to
	// set, so it has to name them.
	for _, want := range []string{"refus", DomainEnv, "MEMQL_EMAIL_AZURE_TENANT_ID", AllowLogOnlyEnv} {
		if !strings.Contains(report.Detail, want) {
			t.Errorf("detail does not mention %q\ngot: %s", want, report.Detail)
		}
	}
}

// TestStatusKeepsDegradedForABreakGlassInstall: an operator who set the
// opt-out has said log-only is deliberate here. Reporting that as unhealthy
// would be crying wolf on the one install where the state is a decision.
func TestStatusKeepsDegradedForABreakGlassInstall(t *testing.T) {
	clearEmailEnv(t)
	t.Setenv(DomainEnv, "acme.com")
	t.Setenv(AllowLogOnlyEnv, "true")

	report := integrationWith(map[string]string{}, map[string]string{}).emailReport(ownerContext(), false)

	if report.Health != HealthDegraded {
		t.Errorf("health = %q, want %q", report.Health, HealthDegraded)
	}
}

// TestStatusStillHealthyWhenConfigured is the negative control for both of
// the above: the gate must be invisible to a configured lane.
func TestStatusStillHealthyWhenConfigured(t *testing.T) {
	clearEmailEnv(t)
	t.Setenv(DomainEnv, "acme.com")
	g := DefaultGraphEnvKeys()
	t.Setenv(g.TenantId, "tenant-abc")
	t.Setenv(g.ClientId, "client-abc")
	t.Setenv(g.ClientSecret, "secret-abc")
	t.Setenv(g.SenderAddr, "no-reply@acme.com")

	report := integrationWith(map[string]string{}, map[string]string{}).emailReport(ownerContext(), false)

	if report.Configured != AnswerYes {
		t.Errorf("configured = %q, want %q", report.Configured, AnswerYes)
	}
	if report.Mode != "graph" {
		t.Errorf("mode = %q, want %q", report.Mode, "graph")
	}
	// Unprobed, so health is honestly unknown -- what matters is that it is
	// not the unhealthy verdict the log-only branch produces.
	if report.Health == HealthUnhealthy {
		t.Errorf("a configured lane must not be reported unhealthy without a probe; detail: %s", report.Detail)
	}
}
