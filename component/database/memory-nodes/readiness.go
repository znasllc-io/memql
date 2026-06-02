package memoryNodes

import (
	"context"
	"database/sql"
	"fmt"
)

// criticalTables are the schema invariants asserted by the server-side
// readiness probe (GET /readyz). Their absence means a migration did not fully
// apply, so the deploy must NOT be treated as ready -- this is the runtime
// counterpart to the #671 migrate gate and closes the gap that let #624 ship a
// broken schema behind a green deploy (#657).
//
// Names are passed to to_regclass exactly as the catalog stores them: a
// mixed-case identifier MUST carry embedded double-quotes or Postgres folds it
// to lower-case and the lookup spuriously fails.
var criticalTables = []string{
	`"MemoryNodes"`,               // core time-series node store
	"automation_execution_claims", // single-flight automation claims (#624 regression)
}

// AssertCriticalSchema returns a non-nil error if any required table is absent
// (or the database is not connected). It lets GET /readyz prove schema presence
// WITHOUT database credentials, since the staging DB is firewalled to in-cluster
// egress and an external deploy gate cannot reach it directly (#657).
func (db *MemoryNodesDatabase) AssertCriticalSchema(ctx context.Context) error {
	bunDB := db.BunDB()
	if bunDB == nil {
		return fmt.Errorf("database not ready: no connection")
	}

	for _, tbl := range criticalTables {
		var reg sql.NullString
		if err := bunDB.NewRaw("SELECT to_regclass(?)::text", tbl).Scan(ctx, &reg); err != nil {
			return fmt.Errorf("schema check for %s failed: %w", tbl, err)
		}
		if !reg.Valid {
			return fmt.Errorf("required table %s is missing (to_regclass IS NULL)", tbl)
		}
	}

	return nil
}
