package memql

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
)

// TestPackageDslDomainsGuardDecisions is the guard's rule as a table. The
// component/packages DB tests (dsl_domains_db_test.go) prove the engine
// consults it on a raw insert for every caller a package's row tier admits;
// this states what it answers, including the cases a careless rule would get
// wrong: an empty list and null are naming the field as much as a full one is,
// a context that was never stamped with an origin is a client, and a write
// that does not name the field is not this guard's at all.
func TestPackageDslDomainsGuardDecisions(t *testing.T) {
	client := auth.ContextWithClientOrigin(context.Background())
	internal := auth.ContextWithInternalOrigin(context.Background())

	for _, tc := range []struct {
		name    string
		ctx     context.Context
		delta   map[string]any
		refuses bool
	}{
		// Naming the field, without internal origin.
		{"a client naming a list", client, map[string]any{"dslDomains": []any{"acme"}}, true},
		{"a client naming an empty list", client, map[string]any{"dslDomains": []any{}}, true},
		{"a client naming null", client, map[string]any{"dslDomains": nil}, true},
		{"a client naming it beside other fields", client, map[string]any{"name": "acme", "dslDomains": []any{"acme"}}, true},
		{"a context that carries no origin", context.Background(), map[string]any{"dslDomains": []any{"acme"}}, true},

		// The deploy and the rollback.
		{"internal origin naming a list", internal, map[string]any{"dslDomains": []any{"acme", "beta"}}, false},
		{"internal origin emptying the list", internal, map[string]any{"dslDomains": []any{}}, false},

		// Writes that do not name it, which is every other write to the row and
		// leaves the list as the read-merge finds it.
		{"a client writing other fields", client, map[string]any{"name": "acme", "declares": []any{}, "latestKnownVersion": "sha-1"}, false},
		{"a client re-sending the rest of the row", client, map[string]any{"name": "acme", "ownerUserId": "u", "sourceKind": "repo", "status": "active"}, false},
		{"an empty delta", client, map[string]any{}, false},
		{"no delta", client, nil, false},
		// A different key is a different field, not a spelling of this one.
		{"a field that only resembles it", client, map[string]any{"dslDomain": []any{"acme"}, "DslDomains": []any{"acme"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validatePackageDslDomainsServerOnly(tc.ctx, tc.delta)
			if !tc.refuses {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			// The sentence a person is shown names the concept, the field, and
			// who may write it, so they know what to remove.
			require.Contains(t, err.Error(), conceptPlatformPackage)
			require.Contains(t, err.Error(), "`dslDomains` refused")
			require.Contains(t, err.Error(), "written only by a package deploy or rollback")
		})
	}
}
