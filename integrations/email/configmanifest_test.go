package email

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// configmanifest_test.go -- the declared configuration surface (memql#4825).
//
// The point of the collapse is that the RESOLVER and the REPORTER can no
// longer disagree about what there is to configure. So the load-bearing test
// here is not any single assertion about a lane -- it is
// TestResolverAndReporterAgreeOnEverySlot, which walks the manifest and the
// report side by side. A test that checked each half against a hardcoded list
// would pass forever and prove nothing about the two agreeing with each
// OTHER, which is the property that was actually missing.

// fakeStore fakes a globalVariable / globalSecret resolver.
func fakeStore(values map[string]string) func(context.Context, string) (string, error) {
	return func(_ context.Context, name string) (string, error) { return values[name], nil }
}

func envMap(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := values[name]
		return v, ok
	}
}

func graphEnv() map[string]string {
	k := DefaultGraphEnvKeys()
	return map[string]string{
		k.TenantId: "tenant", k.ClientId: "client",
		k.ClientSecret: "a-long-enough-secret-value", k.SenderAddr: "no-reply@example.test",
	}
}

func TestManifestDeclaresBothLanesAndTheirRequirements(t *testing.T) {
	m := EmailConfigManifest()
	if m.Integration != ComponentName {
		t.Errorf("manifest names %q, want %q", m.Integration, ComponentName)
	}
	// Order is PREFERENCE order, and Graph must come first: it is the only
	// lane that can send as more than one identity, so a deployment
	// configured for both must not silently take the single-identity one.
	if len(m.Lanes) != 2 || m.Lanes[0].Name != LaneGraph || m.Lanes[1].Name != LaneSMTP {
		t.Fatalf("lanes = %v, want graph then smtp", laneNames(m))
	}
	graph, _ := m.Lane(LaneGraph)
	if got := strings.Join(graph.Required(), ","); got != "tenantId,clientId,senderAddress,clientSecret" {
		t.Errorf("graph required = %q; a change here changes what 'configured' MEANS on every surface", got)
	}
	smtp, _ := m.Lane(LaneSMTP)
	if got := strings.Join(smtp.Required(), ","); got != "smtpHost,smtpFromAddress" {
		t.Errorf("smtp required = %q", got)
	}
	// Exactly one secret per lane, and it must be marked, because Secret is
	// what routes the lookup to the sealed store and keeps the value out of
	// the report.
	for _, lane := range m.Lanes {
		secrets := 0
		for _, slot := range lane.Slots {
			if slot.Secret {
				secrets++
			}
		}
		if secrets != 1 {
			t.Errorf("lane %q declares %d secret slots, want exactly 1", lane.Name, secrets)
		}
	}
}

func laneNames(m ConfigManifest) []string {
	out := []string{}
	for _, lane := range m.Lanes {
		out = append(out, lane.Name)
	}
	return out
}

func TestResolveTakesALaneWholeOrNotAtAll(t *testing.T) {
	m := EmailConfigManifest()
	k := DefaultGraphEnvKeys()

	t.Run("whole from env", func(t *testing.T) {
		res := m.Resolve(context.Background(), ConfigResolver{Env: envMap(graphEnv())})
		if res.Active != LaneGraph || res.ActiveSource != SourceEnv {
			t.Fatalf("active = %q from %q, want graph from env", res.Active, res.ActiveSource)
		}
		if len(res.Reasons()) != 0 {
			t.Errorf("a resolved configuration produced reasons: %v", res.Reasons())
		}
	})

	t.Run("whole from rows", func(t *testing.T) {
		vars := graphEnv()
		secret := vars[k.ClientSecret]
		delete(vars, k.ClientSecret)
		res := m.Resolve(context.Background(), ConfigResolver{
			Vars:    fakeStore(vars),
			Secrets: fakeStore(map[string]string{k.ClientSecret: secret}),
		})
		if res.Active != LaneGraph || res.ActiveSource != SourceGlobalVariable {
			t.Fatalf("active = %q from %q, want graph from stored rows", res.Active, res.ActiveSource)
		}
	})

	t.Run("split across tiers resolves nothing", func(t *testing.T) {
		// The state that LOOKS configured: every value present, none of it
		// usable. Half a migration builds a credential nobody configured, so
		// the resolver refuses -- and has to SAY so, which is what the Split
		// flag is for.
		vars := graphEnv()
		secret := vars[k.ClientSecret]
		delete(vars, k.ClientSecret)
		res := m.Resolve(context.Background(), ConfigResolver{
			Env:     envMap(vars),
			Secrets: fakeStore(map[string]string{k.ClientSecret: secret}),
		})
		if res.Active != "" {
			t.Fatalf("a split lane resolved to %q", res.Active)
		}
		graph := laneResolution(t, res, LaneGraph)
		if !graph.Split || len(graph.Missing) != 0 {
			t.Errorf("split=%v missing=%v, want split with nothing missing", graph.Split, graph.Missing)
		}
		reasons := res.Reasons()
		if len(reasons) != 1 || reasons[0].Code != ReasonSplitLane {
			t.Fatalf("reasons = %+v, want one split_lane", reasons)
		}
		// "You are missing a client secret" and "your client secret is in
		// the wrong place" have different fixes, so they must not share a
		// code.
		if strings.Contains(reasons[0].Detail, "not set") {
			t.Errorf("the split reason reads like a missing value: %q", reasons[0].Detail)
		}
	})

	t.Run("partly configured names what is short", func(t *testing.T) {
		vars := graphEnv()
		delete(vars, k.ClientSecret)
		res := m.Resolve(context.Background(), ConfigResolver{Env: envMap(vars)})
		graph := laneResolution(t, res, LaneGraph)
		if !graph.Partial || strings.Join(graph.Missing, ",") != "clientSecret" {
			t.Fatalf("partial=%v missing=%v", graph.Partial, graph.Missing)
		}
		reasons := res.Reasons()
		if len(reasons) != 1 || reasons[0].Code != ReasonMissingSlot || reasons[0].EnvVar != k.ClientSecret {
			t.Fatalf("reasons = %+v, want one missing_slot naming %s", reasons, k.ClientSecret)
		}
	})

	t.Run("an untouched lane says nothing", func(t *testing.T) {
		// On a Graph install the SMTP lane is empty ON PURPOSE. Listing its
		// six absent values as six things to fix would bury the one that
		// matters and train the operator to ignore the list.
		vars := graphEnv()
		delete(vars, k.ClientSecret)
		res := m.Resolve(context.Background(), ConfigResolver{Env: envMap(vars)})
		for _, reason := range res.Reasons() {
			if reason.Lane == LaneSMTP {
				t.Errorf("an untouched lane produced a reason: %+v", reason)
			}
		}
	})

	t.Run("nothing configured is not an error", func(t *testing.T) {
		res := m.Resolve(context.Background(), ConfigResolver{})
		if res.Active != "" {
			t.Fatalf("active = %q on an empty environment", res.Active)
		}
		if len(res.Reasons()) != 0 {
			t.Errorf("a fresh cluster produced complaints rather than silence: %v", res.Reasons())
		}
	})
}

func laneResolution(t *testing.T, res ConfigResolution, name string) LaneResolution {
	t.Helper()
	for _, lane := range res.Lanes {
		if lane.Lane.Name == name {
			return lane
		}
	}
	t.Fatalf("no lane %q in the resolution", name)
	return LaneResolution{}
}

// TestSecretSlotIsNeverReadFromThePlaintextStore is the one property of the
// ladder that is a security property rather than an ergonomic one: a client
// secret resolved out of v1:platform:globalVariable would be a secret sitting
// in a row any reader can read.
func TestSecretSlotIsNeverReadFromThePlaintextStore(t *testing.T) {
	k := DefaultGraphEnvKeys()
	res := EmailConfigManifest().Resolve(context.Background(), ConfigResolver{
		// The secret planted in the WRONG store.
		Vars: fakeStore(map[string]string{k.ClientSecret: "a-long-enough-secret-value"}),
	})
	slot, _ := res.Slot(slotClientSecret)
	if slot.Value != "" || slot.Source != SourceUnset {
		t.Fatalf("a secret was taken from the plaintext variable store: source=%q", slot.Source)
	}
}

// --- the reporter reads the same declaration -----------------------------

// TestResolverAndReporterAgreeOnEverySlot is the whole point of memql#4825.
// It is written as a walk over the manifest rather than against a fixed list,
// so a slot added to the declaration is covered by construction -- which is
// the only shape that stays true after the next slot is added.
func TestResolverAndReporterAgreeOnEverySlot(t *testing.T) {
	clearEmailEnv(t)
	report := integrationWith(map[string]string{}, map[string]string{}).emailReport(ownerContext(), false)

	reported := map[string]bool{}
	for _, s := range report.Settings {
		reported[s.Name] = true
		if s.Lane == "" {
			t.Errorf("setting %q reports no lane, so a surface cannot group it", s.Name)
		}
	}
	for _, c := range report.Credentials {
		reported[c.Name] = true
		if c.Lane == "" {
			t.Errorf("credential %q reports no lane", c.Name)
		}
	}

	for _, slot := range EmailConfigManifest().Slots() {
		if !reported[slot.Name] {
			t.Errorf("slot %q is declared and the resolver reads it, but the report does not mention it -- "+
				"which is a console showing a complete configuration beside a sender that did not resolve", slot.Name)
		}
		delete(reported, slot.Name)
	}
	for name := range reported {
		t.Errorf("the report carries slot %q, which the manifest does not declare", name)
	}
}

func TestReportStateIsMachineReadable(t *testing.T) {
	t.Run("nothing configured on a local install", func(t *testing.T) {
		clearEmailEnv(t)
		t.Setenv(DomainEnv, "memql.localhost")
		report := integrationWith(map[string]string{}, map[string]string{}).emailReport(ownerContext(), false)
		if report.State != StateNeedsConfiguration {
			t.Errorf("state = %q, want %q", report.State, StateNeedsConfiguration)
		}
	})

	t.Run("configured", func(t *testing.T) {
		clearEmailEnv(t)
		for name, value := range graphEnv() {
			t.Setenv(name, value)
		}
		report := integrationWith(map[string]string{}, map[string]string{}).emailReport(ownerContext(), false)
		if report.State != StateConfigured {
			t.Errorf("state = %q, want %q", report.State, StateConfigured)
		}
		if len(report.Reasons) != 0 {
			t.Errorf("a configured integration carried reasons: %+v", report.Reasons)
		}
	})

	t.Run("log-only on an install that must deliver is UNHEALTHY, not unconfigured", func(t *testing.T) {
		// The distinction is memql#4477: this install is not merely
		// unconfigured, it is actively refusing every send. A surface that
		// painted it the same amber as a fresh cluster would say "finish
		// setup" about a cluster whose mail is broken.
		clearEmailEnv(t)
		t.Setenv(DomainEnv, "acme.com")
		report := integrationWith(map[string]string{}, map[string]string{}).emailReport(ownerContext(), false)
		if report.State != StateUnhealthy {
			t.Errorf("state = %q, want %q", report.State, StateUnhealthy)
		}
		found := false
		for _, reason := range report.Reasons {
			if reason.Code == ReasonRefused {
				found = true
			}
		}
		if !found {
			t.Errorf("no `refused` reason: %+v", report.Reasons)
		}
	})

	t.Run("a partly configured lane produces a per-slot reason", func(t *testing.T) {
		clearEmailEnv(t)
		t.Setenv(DomainEnv, "memql.localhost")
		k := DefaultGraphEnvKeys()
		env := graphEnv()
		delete(env, k.ClientSecret)
		for name, value := range env {
			t.Setenv(name, value)
		}
		report := integrationWith(map[string]string{}, map[string]string{}).emailReport(ownerContext(), false)

		var secret *Credential
		for idx := range report.Credentials {
			if report.Credentials[idx].Name == slotClientSecret {
				secret = &report.Credentials[idx]
			}
		}
		if secret == nil {
			t.Fatal("no clientSecret slot in the report")
		}
		if secret.Reason == "" {
			t.Error("the one slot blocking the lane carries no reason, so a settings surface cannot point at it")
		}
		if !secret.Required {
			t.Error("clientSecret is not marked required")
		}
		// An OPTIONAL slot that is unset is a normal state and must stay
		// quiet, or the marks that mean something get ignored.
		for _, s := range report.Settings {
			if s.Name == slotFromName && s.Reason != "" {
				t.Errorf("an optional unset slot was marked as a problem: %q", s.Reason)
			}
		}
	})
}

// TestReasonsNeverCarryAValue is the credential-leak sweep extended to the new
// field. Reasons are built from a slot's DECLARATION -- name, env var, lane,
// purpose -- and never from what resolved into it.
func TestReasonsNeverCarryAValue(t *testing.T) {
	clearEmailEnv(t)
	k := DefaultGraphEnvKeys()
	env := graphEnv()
	env[k.ClientSecret] = plantedGraphSecret
	// One value in the environment and the rest in rows: a SPLIT lane, which
	// is the reason path that talks about values being in the wrong place
	// and is therefore the one most likely to quote one.
	t.Setenv(k.ClientSecret, plantedGraphSecret)
	i := integrationWith(map[string]string{
		k.TenantId: "tenant", k.ClientId: "client", k.SenderAddr: "no-reply@example.test",
	}, map[string]string{})

	report := i.emailReport(ownerContext(), false)
	// Non-vacuity: this must actually BE the split path, or the sweep below
	// is sweeping a report with nothing interesting in it.
	split := false
	for _, reason := range report.Reasons {
		if reason.Code == ReasonSplitLane {
			split = true
		}
	}
	if !split {
		t.Fatalf("the fixture did not produce a split lane, so this sweep proves nothing: %+v", report.Reasons)
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), plantedGraphSecret) {
		t.Fatalf("a reason or slot carried a credential value:\n%s", raw)
	}
}
