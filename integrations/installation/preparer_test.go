package installation

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
)

func TestNativePreparerRequiresCompletePortsAndRetainsEntryGate(t *testing.T) {
	receiver := newReceiverFixture(t)
	deps := PreparationDependencies{API: receiver, Database: func() *sql.DB { return nil }, Executor: &captureExecutorFixture{}, Files: &captureFilesFixture{}, Tokens: preparationTokenFixture{}, Catalog: receiver.factory(t), Namespace: "memql", ConfigurationName: "receiver", EngineRevision: strings.Repeat("a", 40)}
	for _, field := range []string{"API", "Database", "Executor", "Files", "Tokens", "Catalog", "Namespace", "ConfigurationName", "EngineRevision"} {
		t.Run(field, func(t *testing.T) {
			missing := deps
			switch field {
			case "API":
				missing.API = nil
			case "Database":
				missing.Database = nil
			case "Executor":
				missing.Executor = nil
			case "Files":
				missing.Files = nil
			case "Tokens":
				missing.Tokens = nil
			case "Catalog":
				missing.Catalog = nil
			case "Namespace":
				missing.Namespace = ""
			case "ConfigurationName":
				missing.ConfigurationName = ""
			case "EngineRevision":
				missing.EngineRevision = "main"
			}
			_, err := NewPreparer(missing)
			require.Error(t, err)
		})
	}
	preparer, err := NewPreparer(deps)
	require.NoError(t, err)
	for _, ctx := range []context.Context{context.Background(), operator(auth.RoleOwner, "operator"), captureOperator(auth.RoleReader, "operator")} {
		result, err := preparer.Prepare(ctx, PrepareSelection{})
		require.Error(t, err)
		require.Empty(t, result.PlanID)
	}
	require.Empty(t, receiver.reads, "construction and rejected external calls must not read receiving credentials")
	var empty *Preparer
	_, err = empty.Prepare(context.Background(), PrepareSelection{})
	require.Error(t, err)
}
