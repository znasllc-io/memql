package database

import (
	"context"
	"log/slog"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/migrate"
)

const callsSimulationLookupMigrationName = "20261120150000_calls_simulation_lookup_indexes"

func registerCallsSimulationLookupIndexes(m *migrate.Migrations, logger *slog.Logger) {
	if m == nil || goMigrationRegistered(m, callsSimulationLookupMigrationName) {
		return
	}
	m.MustRegister(func(ctx context.Context, db *bun.DB) error {
		if err := ensurePayloadLookupIndex(ctx, db, logger, memoryNodesTableName, "memory_nodes_calls_simulation_key_lookup_idx", "keyId", []string{"v1:calls:simulatedWalletEntry"}); err != nil {
			return err
		}
		return ensurePayloadLookupIndex(ctx, db, logger, memoryNodesTableName, "memory_nodes_calls_funding_pass_lookup_idx", "fundingPassId", []string{"v1:calls:room"})
	}, func(ctx context.Context, db *bun.DB) error {
		for _, name := range []string{"memory_nodes_calls_funding_pass_lookup_idx", "memory_nodes_calls_simulation_key_lookup_idx"} {
			if _, err := db.ExecContext(ctx, `DROP INDEX IF EXISTS `+quoteIdentifier(name)); err != nil {
				return err
			}
		}
		return nil
	})
}
