package email

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// status_test.go -- memql#3323.
//
// The load-bearing test in this file is TestStatusNeverLeaksACredential. Every
// other one checks that the report says something useful; that one checks it
// never says the one thing it must not.
//
// It is written as a SWEEP rather than as a field-by-field assertion on
// purpose. A test that asserts `credentials[0].value == ""` passes forever and
// tells you nothing the day somebody adds a `debugConfig` field carrying the
// resolved GraphConfig -- which is exactly how a credential reaches a browser.
// Planting a distinctive value in every slot and then grepping the WHOLE
// serialized reply is the only shape that catches the field nobody thought to
// assert on.

// plantedGraphSecret / plantedSMTPPassword are deliberately long and
// distinctive: redactSecrets skips values under eight characters, and a short
// planted value would make the sweep vacuous.
const (
	plantedGraphSecret  = "PLANTED-GRAPH-CLIENT-SECRET-8f21c0"
	plantedSMTPPassword = "PLANTED-SMTP-PASSWORD-3ba97e"
	plantedRowSecret    = "PLANTED-ROW-SECRET-c41d55"
)

// clearEmailEnv blanks every environment variable the email lane consults, so
// a developer machine that happens to carry real AZURE_* / SMTP_* values
// cannot change what these tests measure. Blank counts as absent: the reader
// trims and the slot resolver treats "" as unset.
func clearEmailEnv(t *testing.T) {
	t.Helper()
	for _, spec := range append(graphSlotSpecs(), smtpSlotSpecs()...) {
		if spec.envVar != "" {
			t.Setenv(spec.envVar, "")
		}
		if spec.legacy != "" {
			t.Setenv(spec.legacy, "")
		}
	}
}

// rowStore fakes the v1:platform:globalVariable / globalSecret resolvers the
// plug-in receives from PluginContext.
func rowStore(values map[string]string) VariableResolver {
	return func(_ context.Context, name string) (string, error) {
		return values[name], nil
	}
}

func ownerContext() context.Context {
	return auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "v1:identity:user:owner",
		Role:   auth.RoleOwner,
	})
}

// integrationWith builds an Integration whose sender is a LazySender wired to
// the given row stores -- the production shape (see plugin.go).
func integrationWith(vars, secrets map[string]string) *Integration {
	lazy := NewLazySender(NewLogSender(nil), rowStore(vars), SecretResolver(rowStore(secrets)), nil)
	return NewIntegration(lazy, nil)
}

// statusPayload runs the capability and returns the single synthetic node's
// payload, both decoded and as raw JSON.
func statusPayload(t *testing.T, i *Integration, ctx context.Context) (map[string]any, string) {
	t.Helper()
	nodes, err := i.handleStatus(ctx, map[string]any{"probe": false}, 0)
	if err != nil {
		t.Fatalf("handleStatus: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("expected exactly one synthetic node, got %d", len(nodes))
	}
	var decoded map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &decoded); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	return decoded, string(nodes[0].Payload)
}

func TestStatusNeverLeaksACredential(t *testing.T) {
	clearEmailEnv(t)
	keys := DefaultGraphEnvKeys()
	smtpKeys := DefaultEnvKeys()

	// Graph fully configured from the environment, INCLUDING the secret.
	t.Setenv(keys.TenantId, "tenant-abc")
	t.Setenv(keys.ClientId, "client-abc")
	t.Setenv(keys.ClientSecret, plantedGraphSecret)
	t.Setenv(keys.SenderAddr, "no-reply@example.com")
	t.Setenv(keys.FromName, "Example")
	// An SMTP password in the environment too, so the sweep covers the lane
	// that did NOT win -- an unused slot still has to stay quiet.
	t.Setenv(smtpKeys.Password, plantedSMTPPassword)

	// ...and a third secret reachable only through the stored-secret
	// resolver, so the row lane is swept as well as the env lane.
	i := integrationWith(
		map[string]string{},
		map[string]string{smtpKeys.Password: plantedRowSecret},
	)

	_, raw := statusPayload(t, i, ownerContext())

	for _, planted := range []string{plantedGraphSecret, plantedSMTPPassword, plantedRowSecret} {
		if strings.Contains(raw, planted) {
			t.Errorf("the status reply contains a credential value (%q).\n"+
				"Nothing this capability returns may carry a secret -- report presence, source and a\n"+
				"rotation command instead. Full reply:\n%s", planted, raw)
		}
	}
}

func TestStatusReportsPresenceAndSourceWithoutTheValue(t *testing.T) {
	clearEmailEnv(t)
	keys := DefaultGraphEnvKeys()
	t.Setenv(keys.TenantId, "tenant-abc")
	t.Setenv(keys.ClientId, "client-abc")
	t.Setenv(keys.ClientSecret, plantedGraphSecret)
	t.Setenv(keys.SenderAddr, "no-reply@example.com")

	i := integrationWith(map[string]string{}, map[string]string{})
	report := i.emailReport(ownerContext(), false)

	if report.Configured != AnswerYes {
		t.Errorf("configured = %q, want %q", report.Configured, AnswerYes)
	}
	if report.Mode != "graph" {
		t.Errorf("mode = %q, want %q", report.Mode, "graph")
	}
	// Configured but unprobed is "unknown", NOT "healthy". Conflating the two
	// is the exact mistake this surface exists to avoid.
	if report.Health != HealthUnknown {
		t.Errorf("health = %q, want %q -- present configuration is not evidence of a working provider", report.Health, HealthUnknown)
	}

	var clientSecret *Credential
	for idx := range report.Credentials {
		if report.Credentials[idx].Name == "clientSecret" {
			clientSecret = &report.Credentials[idx]
		}
	}
	if clientSecret == nil {
		t.Fatal("no clientSecret credential slot in the report")
	}
	if !clientSecret.Present {
		t.Error("clientSecret should be reported present")
	}
	if clientSecret.Source != SourceEnv {
		t.Errorf("clientSecret source = %q, want %q", clientSecret.Source, SourceEnv)
	}
	if !strings.Contains(clientSecret.Rotate, keys.ClientSecret) {
		t.Errorf("clientSecret rotate hint %q should name the variable to set", clientSecret.Rotate)
	}

	// A setting the environment supplies cannot be overridden from the graph,
	// because the resolver reads env first and stops. The report has to say so
	// or the portal renders an editable field that silently does nothing.
	for _, s := range report.Settings {
		if s.Name == "tenantId" && s.Editable {
			t.Error("an env-sourced setting must not be reported editable -- the resolver never reaches the stored row")
		}
	}
}

func TestStatusReportsStoredRowsAsTheirOwnSource(t *testing.T) {
	clearEmailEnv(t)
	keys := DefaultGraphEnvKeys()

	i := integrationWith(
		map[string]string{
			keys.TenantId:   "tenant-from-row",
			keys.ClientId:   "client-from-row",
			keys.SenderAddr: "no-reply@example.com",
		},
		map[string]string{keys.ClientSecret: plantedRowSecret},
	)
	report := i.emailReport(ownerContext(), false)

	if report.Configured != AnswerYes || report.Mode != "graph" {
		t.Fatalf("configured=%q mode=%q, want yes/graph", report.Configured, report.Mode)
	}
	for _, s := range report.Settings {
		if s.Name != "tenantId" {
			continue
		}
		if s.Source != SourceGlobalVariable {
			t.Errorf("tenantId source = %q, want %q", s.Source, SourceGlobalVariable)
		}
		if !s.Editable {
			t.Error("a stored-variable setting is the one an operator CAN change from the portal; it must be reported editable")
		}
	}
	for _, c := range report.Credentials {
		if c.Name == "clientSecret" && c.Source != SourceGlobalSecret {
			t.Errorf("clientSecret source = %q, want %q", c.Source, SourceGlobalSecret)
		}
	}
}

// The degradation case, and the reason "configured" and "healthy" are separate
// questions: with nothing configured the integration still answers every send
// with success, having delivered nothing.
func TestStatusCallsLogOnlyModeDegradedRatherThanHealthy(t *testing.T) {
	clearEmailEnv(t)

	i := integrationWith(map[string]string{}, map[string]string{})
	report := i.emailReport(ownerContext(), false)

	if report.Configured != AnswerNo {
		t.Errorf("configured = %q, want %q", report.Configured, AnswerNo)
	}
	if report.Health != HealthDegraded {
		t.Errorf("health = %q, want %q -- log-only mode succeeds at everything and delivers nothing", report.Health, HealthDegraded)
	}
	if !strings.Contains(report.Detail, "log-only") {
		t.Errorf("the detail must say the messages are not delivered; got %q", report.Detail)
	}
}

// A split configuration -- half the Graph slots in the environment, half in
// stored rows -- resolves to NEITHER lane, because the real resolver takes a
// lane wholesale. The report has to reflect that rather than showing four
// present slots and an unexplained log-only sender.
func TestStatusDoesNotClaimConfiguredWhenALaneIsSplitAcrossSources(t *testing.T) {
	clearEmailEnv(t)
	keys := DefaultGraphEnvKeys()
	t.Setenv(keys.TenantId, "tenant-abc")
	t.Setenv(keys.ClientId, "client-abc")

	i := integrationWith(
		map[string]string{keys.SenderAddr: "no-reply@example.com"},
		map[string]string{keys.ClientSecret: plantedRowSecret},
	)
	report := i.emailReport(ownerContext(), false)

	if report.Configured != AnswerNo {
		t.Errorf("configured = %q, want %q -- the resolver does not mix env and stored rows within a lane", report.Configured, AnswerNo)
	}
	present := 0
	for _, s := range report.Settings {
		if s.Source != SourceUnset {
			present++
		}
	}
	if present == 0 {
		t.Error("the report should still show which individual slots have values, so an operator can see the split")
	}
}

func TestStatusRefusesCallersBelowAdmin(t *testing.T) {
	i := integrationWith(map[string]string{}, map[string]string{})

	cases := []struct {
		name string
		ctx  context.Context
		want bool // want an error
	}{
		{"no authenticated caller", context.Background(), true},
		{"reader", auth.ContextWithAccess(context.Background(), &auth.AccessContext{Role: auth.RoleReader}), true},
		{"writer", auth.ContextWithAccess(context.Background(), &auth.AccessContext{Role: auth.RoleWriter}), true},
		{"admin", auth.ContextWithAccess(context.Background(), &auth.AccessContext{Role: auth.RoleAdmin}), false},
		{"owner", ownerContext(), false},
	}
	for _, tc := range cases {
		_, err := i.handleStatus(tc.ctx, map[string]any{}, 0)
		if tc.want && err == nil {
			t.Errorf("%s: expected refusal, got a report", tc.name)
		}
		if !tc.want && err != nil {
			t.Errorf("%s: expected a report, got %v", tc.name, err)
		}
	}
}

func TestStatusListsEveryRegisteredPluginExactlyOnce(t *testing.T) {
	clearEmailEnv(t)
	i := integrationWith(map[string]string{}, map[string]string{})

	decoded, _ := statusPayload(t, i, ownerContext())
	rows, ok := decoded["integrations"].([]any)
	if !ok || len(rows) == 0 {
		t.Fatalf("payload has no integrations list: %#v", decoded["integrations"])
	}

	seen := map[string]int{}
	for _, raw := range rows {
		row, isMap := raw.(map[string]any)
		if !isMap {
			t.Fatalf("integration row is not an object: %#v", raw)
		}
		name, _ := row["name"].(string)
		seen[name]++
		if registered, _ := row["registered"].(bool); !registered {
			t.Errorf("%s: every row in the roll-call is a registered plug-in", name)
		}
	}
	if seen["email"] != 1 {
		t.Errorf("email appears %d times, want exactly 1 (it is skipped in the roll-call and prepended)", seen["email"])
	}
	for name, count := range seen {
		if count > 1 {
			t.Errorf("%s appears %d times", name, count)
		}
	}
}

func TestRedactSecretsSkipsShortValuesAndBlanksRealOnes(t *testing.T) {
	got := redactSecrets("relay said: bad password SUPER-LONG-SECRET-VALUE-99", "SUPER-LONG-SECRET-VALUE-99")
	if strings.Contains(got, "SUPER-LONG-SECRET-VALUE-99") {
		t.Errorf("a long secret must be blanked; got %q", got)
	}
	if !strings.Contains(got, "<redacted>") {
		t.Errorf("the redaction should be visible in the diagnostic; got %q", got)
	}

	// A short "secret" is far more likely to be a substring of ordinary prose
	// than a real credential, and blanking it would corrupt the diagnostic
	// while protecting nothing.
	unchanged := redactSecrets("connect port 587 refused", "587")
	if unchanged != "connect port 587 refused" {
		t.Errorf("a short value must not be redacted out of prose; got %q", unchanged)
	}
}
