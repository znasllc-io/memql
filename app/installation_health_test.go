package app

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun/migrate"
	"github.com/znasllc-io/memql/sdk/go/client"
)

func TestInstallationMigrationHealthRequiresEveryCompiledMigrationApplied(t *testing.T) {
	complete := migrate.MigrationSlice{{ID: 1, Name: "20261021010000"}, {ID: 2, Name: "20261022010000"}}
	digest, err := installedMigrationDigest(complete)
	require.NoError(t, err)
	other, err := installedMigrationDigest(migrate.MigrationSlice{complete[1], complete[0]})
	require.NoError(t, err)
	require.Equal(t, digest, other, "independent replicas agree on the migration set")
	for _, rows := range []migrate.MigrationSlice{nil, {{Name: "20261022010000"}}, {complete[0], {Name: complete[1].Name}}, {complete[0], complete[0]}, {{ID: 1}}} {
		_, err := installedMigrationDigest(rows)
		require.Error(t, err, "an empty or partially applied ledger must not claim readiness")
	}
	changed, err := installedMigrationDigest(append(complete, migrate.Migration{ID: 3, Name: "20261023010000"}))
	require.NoError(t, err)
	require.NotEqual(t, digest, changed)
}

func TestInstallationServingHealthMatchesNonceAndAuthenticatedStoredUser(t *testing.T) {
	health := client.Row{"nonce": "fresh", "userId": "person", "identityId": "identity", "engineCommit": "commit", "migrationsDigest": "digest"}
	_, err := installationHealthReply(health, client.Row{"id": "v1:identity:user:person"}, "fresh")
	require.NoError(t, err)
	for _, row := range []client.Row{{"id": "other"}, {}, {"id": 4}} {
		_, err = installationHealthReply(health, row, "fresh")
		require.Error(t, err)
	}
	_, err = installationHealthReply(health, client.Row{"id": "person"}, "replayed")
	require.Error(t, err)
	delete(health, "identityId")
	_, err = installationHealthReply(health, client.Row{"id": "person"}, "fresh")
	require.Error(t, err)
}
