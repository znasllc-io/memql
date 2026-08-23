// Package authactivity owns retention for v1:identity:authActivity -- the
// routine authentication-mechanics log split out of the audit log in
// memql#4328.
//
// # Why this deletes and the audit sweep does not
//
// v1:identity:auditEvent has a daily automation (auditEventRetentionSweep,
// dsl/identity/logic.memql) that COUNTS candidate rows and changes nothing,
// because MemQL has no delete() mutation and an append-only audit log cannot
// be soft-deleted via active=false without changing what the log means.
//
// authActivity is a different kind of record and needs a different answer. It
// is one row per refresh-token rotation and one per PAT-authenticated request:
// two orders of magnitude more volume than the audit log, with value that
// decays in weeks. Leaving it to grow forever is not a compliance posture, it
// is a table nobody pruned.
//
// So this job deletes from the node table DIRECTLY, bypassing the engine's
// versioning model on purpose, on the pattern component/node/delivery_store_pg.go
// already established for the mesh delivery substrate. Two consequences worth
// stating rather than leaving to be rediscovered:
//
//   - It removes EVERY VERSION of a row, not only the latest. MemoryNodes is
//     keyed (id, createdAt); deleting the newest version alone would leave the
//     older ones behind as rows no read ever returns and no sweep ever finds
//     again.
//   - Nothing else is notified. There is no graph event, no CDC row and no
//     subscriber fan-out for these deletions, which is correct for a
//     retention sweep and would not be for a user-visible delete.
//
// # What depends on the window
//
// Refresh-token reuse detection (memql#4329) reaches back exactly as far as
// these rows survive: a replayed token is recognised by matching a
// retiredTokenHash some rotation recorded, and once the row is pruned the
// replay is indistinguishable from a stale cookie. The default 30 days exceeds
// both the 14-day idle timeout and the 30-day refresh-token TTL, so a token
// older than the window is already dead on its own account -- which is what
// makes the limit safe rather than merely documented.
package authactivity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/metrics"
)

// ConceptID is the concept whose rows this job prunes.
const ConceptID = "v1:identity:authActivity"

const (
	// DefaultBatchSize bounds one DELETE. Large enough that a busy cluster
	// drains in a handful of statements, small enough that no single
	// statement holds locks across a meaningful slice of the hypertable.
	DefaultBatchSize = 1000

	// DefaultInterval is how often the sweep runs. Daily: the window is
	// measured in days, so a finer cadence buys nothing and a coarser one
	// lets a burst sit past its retention.
	DefaultInterval = 24 * time.Hour
)

// batchDeleter is the one operation this package performs against storage.
// An interface so the LOOP can be tested without a database -- what the SQL
// does is covered by prune_db_test.go against real Postgres.
//
// TWO COUNTS, because they answer different questions and conflating them
// makes the loop wrong. `rows` is the number of logical activity rows the
// batch covered, which is what the LIMIT bounded and therefore the only number
// the drained-yet test can compare against. `versions` is table rows removed,
// which is >= rows whenever a row has history, and is what the counter
// reports. Returning versions alone would make a short batch whose rows carry
// history look full, and the sweep would loop on an already-drained window.
type batchDeleter interface {
	deleteOlderThan(ctx context.Context, cutoff time.Time, limit int) (rows, versions int64, err error)
}

// Pruner hard-deletes authActivity rows past the retention window.
//
// Safe to run on every identity replica. Deletes are idempotent -- a row a
// sibling already removed simply is not there -- so concurrent sweeps race
// harmlessly and neither errors. That is the same multi-node posture
// delivery_store_pg.go's SweepOlderThan records, and it is why there is no
// advisory lock here: a lock would serialize the replicas for no correctness
// gain and add a failure mode (a dropped lock under a transaction pooler) that
// the work itself does not have.
type Pruner struct {
	// DB returns the database handle. A func so the caller can hand over a
	// getter resolved at boot without this package caring how it was built.
	DB func() *sql.DB

	// Retention is the window. Rows older than now-Retention are deleted.
	// Zero is REFUSED rather than defaulted: a zero window means "delete
	// everything up to this instant", and guessing on behalf of a caller who
	// left it unset would be the worst possible guess.
	Retention time.Duration

	// Interval between sweeps. Zero uses DefaultInterval.
	Interval time.Duration

	// BatchSize bounds one DELETE. Zero or negative uses DefaultBatchSize.
	BatchSize int

	Logger *slog.Logger

	// Now is injectable for tests. Nil uses time.Now.
	Now func() time.Time

	// deleter is the storage operation. Nil resolves to the Postgres
	// implementation over DB.
	deleter batchDeleter
}

// ErrNoRetentionWindow is returned when Retention is not positive.
var ErrNoRetentionWindow = errors.New("authactivity: retention window must be positive")

// Run sweeps once and then on every Interval until ctx is done. Intended to be
// started as a goroutine at boot.
//
// It sweeps IMMEDIATELY rather than after the first interval. A pod that
// restarts daily would otherwise never prune at all, which is the failure mode
// where the job exists, is wired, and does nothing.
func (p *Pruner) Run(ctx context.Context) {
	p.sweep(ctx)
	ticker := time.NewTicker(p.interval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.sweep(ctx)
		}
	}
}

// sweep runs one prune and logs the outcome. Errors are logged, never fatal:
// a retention job that took the node down would be worse than one that is
// behind.
func (p *Pruner) sweep(ctx context.Context) {
	started := p.now()
	deleted, err := p.PruneOnce(ctx)
	if err != nil && !errors.Is(err, context.Canceled) {
		p.log().Warn("auth activity retention sweep failed",
			slog.Int64("deleted", deleted),
			slog.String("error", err.Error()),
			slog.String("component", "identity"))
		return
	}
	if errors.Is(err, context.Canceled) {
		return
	}
	// ONE LINE PER RUN, including the zero-row run. A sweep that logs only
	// when it deletes something is indistinguishable from a sweep that is not
	// running, which is precisely the question an operator asks of it.
	p.log().Info("auth activity retention sweep",
		slog.Int64("deleted", deleted),
		slog.String("older_than", p.cutoff().Format(time.RFC3339)),
		slog.Duration("took", p.now().Sub(started)),
		slog.String("component", "identity"))
}

// PruneOnce deletes every row past the window, in bounded batches, and returns
// how many it removed.
//
// On an error mid-sweep it returns the count deleted SO FAR alongside the
// error: those rows are gone either way, and a caller that reported only the
// error would under-count the metric by a whole batch on every hiccup.
func (p *Pruner) PruneOnce(ctx context.Context) (int64, error) {
	if p.Retention <= 0 {
		return 0, ErrNoRetentionWindow
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	del, err := p.resolveDeleter()
	if err != nil {
		return 0, err
	}

	cutoff := p.cutoff()
	limit := p.batchSize()
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		rows, versions, err := del.deleteOlderThan(ctx, cutoff, limit)
		total += versions
		if err != nil {
			metrics.AuthActivityPruned(total)
			return total, err
		}
		// A SHORT batch means the window is drained. Stopping only on zero
		// would cost one extra round trip per sweep, forever.
		if rows < int64(limit) {
			break
		}
	}
	metrics.AuthActivityPruned(total)
	return total, nil
}

func (p *Pruner) resolveDeleter() (batchDeleter, error) {
	if p.deleter != nil {
		return p.deleter, nil
	}
	if p.DB == nil {
		return nil, errors.New("authactivity: no database handle")
	}
	db := p.DB()
	if db == nil {
		return nil, errors.New("authactivity: database handle is nil")
	}
	return &pgDeleter{db: db}, nil
}

func (p *Pruner) cutoff() time.Time { return p.now().Add(-p.Retention) }

func (p *Pruner) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now().UTC()
}

func (p *Pruner) batchSize() int {
	if p.BatchSize <= 0 {
		return DefaultBatchSize
	}
	return p.BatchSize
}

func (p *Pruner) interval() time.Duration {
	if p.Interval <= 0 {
		return DefaultInterval
	}
	return p.Interval
}

func (p *Pruner) log() *slog.Logger {
	if p.Logger != nil {
		return p.Logger
	}
	return slog.Default()
}

// pgDeleter is the real storage operation.
type pgDeleter struct{ db *sql.DB }

// selectExpiredIDs picks a bounded set of row ids whose activity is past the
// cutoff.
//
// THREE THINGS IN THE PREDICATE, each load-bearing:
//
//   - `concept = $1` scopes the sweep. Without it this statement deletes the
//     whole graph.
//   - `"createdAt" < $2` is what makes it fast. createdAt is the hypertable's
//     partition key, so this prunes chunks; the payload predicate alone would
//     scan every chunk of the largest table in the system.
//   - the CASE on `occurredAt` is what makes it CORRECT against the field the
//     concept actually documents as the event time. It is wrapped in a CASE
//     rather than cast directly because a malformed value would abort the
//     whole statement, and one bad row must not stop retention for every other
//     row; CASE is the idiom Postgres guarantees evaluates in order. A row
//     whose occurredAt is unreadable falls back to createdAt, which for an
//     append-only log written at the moment of the event is the same instant
//     to within milliseconds.
const selectExpiredIDs = `
SELECT DISTINCT id
  FROM "MemoryNodes"
 WHERE concept = $1
   AND "createdAt" < $2
   AND (CASE
          WHEN payload->>'occurredAt' ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}T'
          THEN (payload->>'occurredAt')::timestamptz
          ELSE "createdAt"
        END) < $2
 LIMIT $3`

// deleteExpiredByIDs builds `DELETE ... WHERE concept = $1 AND id IN ($2,...)`.
//
// An explicit placeholder list rather than `id = ANY($2)` with an array
// parameter: the array form needs the driver to encode a Go slice as a
// Postgres text[], which bun's pgdriver does not do for a bare []string, and a
// silently-wrong encoding here would delete nothing while reporting success.
// The batch is bounded (DefaultBatchSize 1000) and Postgres allows 65535
// parameters, so the list can never approach the limit.
//
// Deliberately NOT scoped by createdAt: MemoryNodes is keyed (id, createdAt),
// so deleting only the versions past the cutoff would leave older ones behind
// as rows no read returns and no future sweep finds -- once the newest version
// is gone the id never comes back from selectExpiredIDs again.
func deleteExpiredByIDs(ids []string) (string, []any) {
	args := make([]any, 0, len(ids)+1)
	args = append(args, ConceptID)
	var b strings.Builder
	b.WriteString(`DELETE FROM "MemoryNodes" WHERE concept = $1 AND id IN (`)
	for i, id := range ids {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "$%d", i+2)
		args = append(args, id)
	}
	b.WriteByte(')')
	return b.String(), args
}

func (d *pgDeleter) deleteOlderThan(ctx context.Context, cutoff time.Time, limit int) (int64, int64, error) {
	rows, err := d.db.QueryContext(ctx, selectExpiredIDs, ConceptID, cutoff, limit)
	if err != nil {
		return 0, 0, fmt.Errorf("authactivity: select expired ids: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if scanErr := rows.Scan(&id); scanErr != nil {
			rows.Close()
			return 0, 0, fmt.Errorf("authactivity: scan expired id: %w", scanErr)
		}
		ids = append(ids, id)
	}
	if iterErr := rows.Err(); iterErr != nil {
		rows.Close()
		return 0, 0, fmt.Errorf("authactivity: iterate expired ids: %w", iterErr)
	}
	rows.Close()
	if len(ids) == 0 {
		return 0, 0, nil
	}

	stmt, args := deleteExpiredByIDs(ids)
	res, err := d.db.ExecContext(ctx, stmt, args...)
	if err != nil {
		return 0, 0, fmt.Errorf("authactivity: delete expired rows: %w", err)
	}
	versions, _ := res.RowsAffected()
	return int64(len(ids)), versions, nil
}
