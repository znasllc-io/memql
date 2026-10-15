// Package gatecontext carries one explicitly shared advisory-lock transaction
// through a synchronous capability. Work still uses the ordinary request pool;
// only lock acquisition uses this transaction. Never share the derived context
// between independently executing operations: its locks share one lifetime.
package gatecontext

import (
	"context"
	"database/sql"
)

type key struct{}
type scope struct {
	pool *sql.DB
	tx   *sql.Tx
}

// WithTransaction lends tx to nested gate acquisitions on this exact pool. The
// owner alone rolls it back. A stale context keeps the ended transaction, so a
// nested caller fails instead of silently acquiring a new, uncoordinated gate.
func WithTransaction(ctx context.Context, pool *sql.DB, tx *sql.Tx) context.Context {
	return context.WithValue(ctx, key{}, scope{pool: pool, tx: tx})
}

func Transaction(ctx context.Context, pool *sql.DB) *sql.Tx {
	s, _ := ctx.Value(key{}).(scope)
	if pool != nil && s.pool == pool {
		return s.tx
	}
	return nil
}
