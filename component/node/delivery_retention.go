package node

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component"
	"github.com/znasllc-io/memql/core/common"
)

// OutboxRetention is the periodic retention sweep for the mesh delivery
// substrate's coordination tables (memql#1328). The
// 20260610000000_mesh_delivery_substrate migration documents retention as
// intent only ("rows are prunable ... past a max-age watermark"); nothing
// implemented it, so mesh_outbox and mesh_cursor grew without bound -- and
// traffic rose sharply once per-occurrence EventIDs (memql#1325) and streamed
// text chunks (memql#1327) started riding the outbox.
//
// What it deletes, every sweepInterval (first run after a short startup
// delay), mirroring the automation_execution_claims pruning precedent
// (memql#561 -- a per-node Go ticker on a component run hook):
//
//   - mesh_outbox rows older than the max-age watermark. Chat-reply delivery
//     is a live-stream concern, not an archive; the graph rows themselves
//     remain the source of truth, so age-based deletion is safe. Replay for a
//     reconnecting consumer is bounded to the retention window by
//     construction.
//   - mesh_cursor rows whose updated_at is older than the same watermark.
//     Consumer ids are per-pod, so every rollout strands cursor rows that
//     would otherwise accumulate forever.
//   - mesh_key_seq is deliberately NOT swept; see
//     pgOutboxStore.SweepOlderThan for the seq-restart hazard that makes
//     deleting allocator rows unsafe. The rows are tiny.
//
// Multi-node safety: deletes are idempotent, so concurrent sweeps across
// replicas are harmless. The #561 precedent likewise prunes from every
// replica without a cluster claim, so none is taken here either.
//
// The watermark comes from MEMQL_MESH_OUTBOX_RETENTION (a Go duration
// string). Default 24h; zero or negative disables the sweep entirely.
const (
	OutboxRetentionComponentName = common.ComponentName("meshOutboxRetention")

	// outboxRetentionEnvVar is the operator knob (Go duration string;
	// documented in docs/public/operate/env-vars.md).
	outboxRetentionEnvVar = "MEMQL_MESH_OUTBOX_RETENTION"

	// defaultOutboxRetention is the max-age watermark when the env knob is
	// unset or unparsable.
	defaultOutboxRetention = 24 * time.Hour

	// outboxSweepInterval is how often the sweep runs. Hourly is plenty: the
	// watermark is measured in hours, so finer granularity buys nothing.
	outboxSweepInterval = time.Hour

	// outboxSweepStartupDelay defers the first sweep past startup so a booting
	// node spends its first moments serving, not deleting (the backlog has
	// already waited; another minute is free).
	outboxSweepStartupDelay = 2 * time.Minute
)

// retentionStore is the narrow seam the sweeper needs from the durable store.
// *pgOutboxStore implements it in production; tests use the in-memory fake.
type retentionStore interface {
	// SweepOlderThan deletes outbox rows created before cutoff and cursor
	// rows last updated before cutoff, returning the per-table delete counts.
	SweepOlderThan(ctx context.Context, cutoff time.Time) (outboxDeleted, cursorsDeleted int64, err error)
}

// OutboxRetention runs the sweep on a component run hook (the #561 pattern).
type OutboxRetention struct {
	*component.Component

	store        retentionStore
	retention    time.Duration
	interval     time.Duration
	startupDelay time.Duration
	logger       *slog.Logger
	now          func() time.Time
}

// NewOutboxRetention builds the production sweeper over the cluster Postgres
// outbox tables. dbGetter is resolved lazily (the same posture as the
// substrate store and ClusterExecutionGuard) so construction can precede the
// database component. Retention is read from MEMQL_MESH_OUTBOX_RETENTION.
func NewOutboxRetention(dbGetter func() *bun.DB, logger *slog.Logger) *OutboxRetention {
	return newOutboxRetention(newPGOutboxStore(dbGetter), outboxRetentionFromEnv(logger), logger)
}

// newOutboxRetention is the seam-level constructor shared with tests.
func newOutboxRetention(store retentionStore, retention time.Duration, logger *slog.Logger) *OutboxRetention {
	comp, _ := component.New(OutboxRetentionComponentName)
	r := &OutboxRetention{
		Component:    comp,
		store:        store,
		retention:    retention,
		interval:     outboxSweepInterval,
		startupDelay: outboxSweepStartupDelay,
		logger:       logger,
		now:          time.Now,
	}
	_ = r.ConfigureLifecycle(component.WithRunHook(r.run))
	return r
}

// outboxRetentionFromEnv resolves the max-age watermark: unset -> the 24h
// default; a parsable Go duration -> that value (zero/negative DISABLES the
// sweep); an unparsable value -> warn and fall back to the default (a typo'd
// knob must not silently disable retention).
func outboxRetentionFromEnv(logger *slog.Logger) time.Duration {
	raw := os.Getenv(outboxRetentionEnvVar)
	if raw == "" {
		return defaultOutboxRetention
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		if logger != nil {
			logger.Warn("mesh outbox retention: unparsable duration; using default",
				"env", outboxRetentionEnvVar, "value", raw, "default", defaultOutboxRetention.String())
		}
		return defaultOutboxRetention
	}
	return d
}

// Retention exposes the resolved watermark (tests + health surfaces).
func (r *OutboxRetention) Retention() time.Duration { return r.retention }

func (r *OutboxRetention) run(ctx context.Context, markStarted func()) error {
	markStarted()

	if r.retention <= 0 {
		r.info("mesh outbox retention disabled (zero/negative watermark); outbox and cursor rows will not be swept",
			"env", outboxRetentionEnvVar)
		<-ctx.Done()
		return nil
	}

	r.info("mesh outbox retention sweep scheduled",
		"retention", r.retention.String(), "interval", r.interval.String(), "startup_delay", r.startupDelay.String())

	// First run after a short startup delay, then on the interval.
	select {
	case <-ctx.Done():
		return nil
	case <-time.After(r.startupDelay):
	}
	r.sweep(ctx)

	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			r.sweep(ctx)
		}
	}
}

// sweep runs one retention pass. Errors are logged, never fatal: the next
// tick retries, and rows only ever age further into the watermark.
func (r *OutboxRetention) sweep(ctx context.Context) {
	cutoff := r.now().Add(-r.retention)
	outboxDeleted, cursorsDeleted, err := r.store.SweepOlderThan(ctx, cutoff)
	if err != nil {
		r.warn("mesh outbox retention sweep failed; will retry on the next tick", "error", err)
		return
	}
	if outboxDeleted > 0 || cursorsDeleted > 0 {
		r.info("mesh outbox retention sweep completed",
			"outbox_rows_deleted", outboxDeleted,
			"cursor_rows_deleted", cursorsDeleted,
			"cutoff", cutoff.Format(time.RFC3339))
	}
}

func (r *OutboxRetention) info(msg string, args ...any) {
	if r.logger != nil {
		r.logger.Info(msg, args...)
	}
}

func (r *OutboxRetention) warn(msg string, args ...any) {
	if r.logger != nil {
		r.logger.Warn(msg, args...)
	}
}

// Ensure the pg store satisfies the retention seam.
var _ retentionStore = (*pgOutboxStore)(nil)
