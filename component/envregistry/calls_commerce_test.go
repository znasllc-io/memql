package envregistry

import "testing"

func TestCallsCommerceRegistryIsOptionalAndClassifiesProviderSecrets(t *testing.T) {
	m, err := LoadManifestFromBytes(embeddedManifest, "embedded snapshot")
	if err != nil {
		t.Fatal(err)
	}
	names := []string{
		"MEMQL_CALLS_COMMERCE_MODE", "MEMQL_CALLS_COMMERCE_VALID_UNTIL",
		"MEMQL_CALLS_COMMERCE_CASH_CENTS", "MEMQL_CALLS_COMMERCE_PROTECTED_CENTS", "MEMQL_CALLS_COMMERCE_FIXED_CENTS",
		"MEMQL_CALLS_COMMERCE_FULFILLMENT_RESERVE_BPS", "MEMQL_CALLS_COMMERCE_VERIFICATION_UNIT_CENTS",
		"MEMQL_CALLS_COMMERCE_VERIFICATION_BUDGET_CENTS", "MEMQL_CALLS_COMMERCE_PROVIDER_CREDIT_CENTS", "MEMQL_CALLS_COMMERCE_CALL_BUDGET_CENTS",
		"MEMQL_CALLS_COMMERCE_MAX_ACCOUNTS", "MEMQL_CALLS_COMMERCE_MAX_PENDING_VERIFICATIONS", "MEMQL_CALLS_COMMERCE_MAX_VERIFICATION_ATTEMPTS",
		"MEMQL_CALLS_COMMERCE_MAX_OPEN_CALLS", "MEMQL_CALLS_COMMERCE_MAX_PARTICIPANTS",
		"MEMQL_CALLS_DIDIT_WORKFLOW_ID", "MEMQL_CALLS_DIDIT_WORKFLOW_VERSION",
	}
	secrets := []string{"MEMQL_CALLS_STRIPE_SECRET_KEY", "MEMQL_CALLS_STRIPE_WEBHOOK_SECRET", "MEMQL_CALLS_DIDIT_API_KEY", "MEMQL_CALLS_DIDIT_WEBHOOK_SECRET"}
	for _, group := range []struct {
		names  []string
		secret bool
	}{{names, false}, {secrets, true}} {
		for _, name := range group.names {
			e, ok := m.Lookup(name)
			if !ok || e.Component != "calls" || e.Scope != "node" || !e.Optional || e.Default != "" || len(e.Required) != 0 {
				t.Errorf("%s must be optional, server-configured and without default/boot authority: %+v", name, e)
			}
			if m.IsSecret(name) != group.secret {
				t.Errorf("%s secret classification = %v, want %v", name, m.IsSecret(name), group.secret)
			}
		}
	}
}
