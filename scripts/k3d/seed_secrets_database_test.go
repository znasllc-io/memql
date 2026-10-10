package k3d

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSeedSecretsPreservesRecoveredDatabaseConnections(t *testing.T) {
	for _, pooler := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "pooler"}[pooler], func(t *testing.T) {
			direct := "postgres://memql:retained-password@recovered-rw:5432/memql?sslmode=require"
			application := direct
			if pooler {
				application = "postgres://memql:retained-password@recovered-pooler:5432/memql?sslmode=require"
			}
			stdout, stderr, calls, code := runSeedSecretsFull(t, scenario{
				secretState: "present", clusterDatabaseDSN: application, clusterDatabaseDirectDSN: direct,
			})
			require.Zero(t, code, stdout+stderr)
			require.Equal(t, application, seededLiteral(t, calls, "MEMQL_DATABASE_DSN"))
			require.Equal(t, direct, seededLiteral(t, calls, "MEMORY_NODES_DATABASE_DIRECT_DSN"))
			require.NotContains(t, stdout+stderr, "retained-password")
		})
	}
}

func TestSeedSecretsInitialDatabaseConnections(t *testing.T) {
	for _, state := range []string{"absent", "present"} {
		t.Run(state, func(t *testing.T) {
			stdout, stderr, calls, code := runSeedSecretsFull(t, scenario{secretState: state})
			require.Zero(t, code, stdout+stderr)
			want := "postgres://memql:memql_dev@memql-db-rw:5432/memql?sslmode=disable"
			require.Equal(t, want, seededLiteral(t, calls, "MEMQL_DATABASE_DSN"))
			require.Equal(t, want, seededLiteral(t, calls, "MEMORY_NODES_DATABASE_DIRECT_DSN"))
		})
	}
}

func TestSeedSecretsRefusesUnsafeDatabaseReplacement(t *testing.T) {
	for name, sc := range map[string]scenario{
		"unreadable-application": {databaseReadFails: "MEMQL_DATABASE_DSN"},
		"unreadable-direct":      {databaseReadFails: "MEMORY_NODES_DATABASE_DIRECT_DSN"},
		"missing-direct":         {clusterDatabaseDSN: "postgres://recovered/memql"},
		"missing-application":    {clusterDatabaseDirectDSN: "postgres://recovered/memql"},
		"invalid-encoding":       {databaseInvalidEncoding: true},
		"blank":                  {clusterDatabaseDSN: "   "},
	} {
		t.Run(name, func(t *testing.T) {
			sc.secretState = "present"
			stdout, stderr, calls, code := runSeedSecretsFull(t, sc)
			require.NotZero(t, code, stdout+stderr)
			require.False(t, parseEnvelope(t, stdout).OK)
			require.Empty(t, mutatedAnything(calls), "refuse before any cluster mutation")
		})
	}
}
