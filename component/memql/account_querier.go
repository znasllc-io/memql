package memql

import (
	"context"
	"database/sql"
)

// AccountQuerier is what the account readers need from a database handle: one
// statement at a time (design record
// docs/superpowers/specs/2026-11-12-account-export-design.md, section B).
//
// *sql.DB, *sql.Conn and *sql.Tx all satisfy it, and that is the point. The
// same read answers the inventory on the request pool, the export inside its
// one snapshot on a maintenance session, and a test inside a transaction it
// rolls back. A reader that asked for a pool could not run inside the snapshot,
// and the export's whole claim is that every line it writes comes from one
// instant.
type AccountQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}
