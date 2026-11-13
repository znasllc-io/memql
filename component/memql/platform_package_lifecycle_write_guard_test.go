package memql

import (
	"context"
	"maps"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

func TestEveryPackageFieldHasAWriter(t *testing.T) {
	loadedConceptRegistry(t)
	concept, err := memorynodes.Get(conceptPlatformPackage)
	require.NoError(t, err)
	// Fields outside the observation contract retain their existing writer:
	// ownership/transfer, source-policy, retirement, or the initial registration.
	decided := map[string]string{
		"accountId": "organization ownership and transfer", "ownerUserId": "original owner restoration",
		"name": "registration then server manifest recorder", "autoDeployChangedAt": "source policy",
		"autoDeploy": "source policy", "sourceRemoved": "source policy", "sourceConnectionId": "source policy",
		"sourceKind": "registration then source policy", "repoUrl": "registration then source policy",
		"repoRef": "source policy", "credentialId": "source policy", "artifactId": "source policy",
		"status": "retirement", "disabledDeployables": "retirement",
	}
	for _, field := range packageObservedFields {
		require.NotContains(t, decided, field)
		decided[field] = "server observation"
	}
	declared := concept.DeclaredFields()
	require.Len(t, decided, len(declared), "a package field needs an explicit writer decision")
	for _, field := range declared {
		require.Contains(t, decided, field, "a new package field must not silently become a client-writable deployment control")
	}
}

func TestPackageLifecycleObservationsCannotBeForgedOrRemoved(t *testing.T) {
	engine := &MemQLEngine{}
	client := auth.ContextWithClientOrigin(context.Background())
	internal := auth.ContextWithInternalOrigin(client)
	prior := map[string]any{
		"name": "Source name", "deployedVersion": "deployed", "latestKnownVersion": "upstream",
		"updateAvailable": true, "declares": []any{map[string]any{"name": "storefront"}},
		"dslDomains": []any{"fylo"}, "dslDeployedBy": "operator", "autoDeployChangedAt": "original-epoch",
	}
	for _, field := range append(append([]string{}, packageObservedFields...), "name") {
		t.Run(field, func(t *testing.T) {
			for _, remove := range []bool{false, true} {
				final := maps.Clone(prior)
				if remove {
					delete(final, field)
				} else {
					final[field] = "forged-after-hook"
				}
				err := engine.validatePackageLifecycleWrite(client, prior, final)
				require.ErrorContains(t, err, "internal origin")
				require.ErrorContains(t, err, field)
				require.NoError(t, engine.validatePackageLifecycleWrite(internal, prior, final))
			}
		})
	}
	// A merge retaining recorded observations is not a new attestation.
	final := maps.Clone(prior)
	final["ownerUserId"] = "unchanged-by-this-guard"
	require.NoError(t, engine.validatePackageLifecycleWrite(client, prior, final))
}

func TestPackageLifecycleRegistrationDefaultsAndPolicyEpoch(t *testing.T) {
	engine := &MemQLEngine{}
	client := auth.ContextWithClientOrigin(context.Background())
	for _, final := range []map[string]any{
		{"name": "Registered source", "updateAvailable": false},
		{"name": "Registered source", "deployedVersion": "", "latestKnownVersion": "", "declares": []any{}},
	} {
		require.NoError(t, engine.validatePackageLifecycleWrite(client, nil, final))
	}
	for field, value := range map[string]any{
		"deployedVersion": "fake", "latestKnownVersion": "fake", "updateAvailable": true,
		"declares": []any{map[string]any{"name": "fake"}}, "dslDomains": []any{"foreign"}, "dslDeployedBy": "operator",
	} {
		require.ErrorContains(t, engine.validatePackageLifecycleWrite(client, nil, map[string]any{field: value}), "internal origin")
	}
	prior := map[string]any{"autoDeploy": true, "autoDeployChangedAt": "old"}
	final := map[string]any{"autoDeploy": true, "autoDeployChangedAt": "new"}
	require.ErrorContains(t, engine.validatePackageLifecycleWrite(client, prior, final), "app:pipelines/repositories")
	require.NoError(t, engine.validatePackageLifecycleWrite(auth.ContextWithInternalOrigin(client), prior, final))
	// Existing owner/status/source guards still decide these fields. This
	// focused observation guard does not turn its success into write admission.
	require.NoError(t, engine.validatePackageLifecycleWrite(client, nil, map[string]any{"status": "archived", "ownerUserId": "other"}))
}
