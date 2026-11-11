package datasync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

// runners.go -- backfill and reconciliation (epic memql#4378).
//
// Both fill the same gap from different ends. A webhook stream tells you
// what changed AFTER you started listening; it says nothing about what
// was already there, and it drops things. So:
//
//	BACKFILL       reads the origin's current state once, page by page,
//	               to populate a mirror that has never been filled.
//	RECONCILIATION runs on a schedule forever, comparing the origin
//	               against the mirror and healing what drifted.
//
// Both write through the SAME version-guarded Applier the inbound
// dispatcher uses. That is the load-bearing part: a sweep with its own
// write path would apply an older snapshot over a newer webhook and
// "heal" the mirror backwards, which is the exact failure the guard
// exists to prevent -- arriving from the one direction nobody watches.
//
// # Progress is persisted per page, not per run
//
// A backfill of a large catalog is minutes to hours of paging, and the
// node will be restarted during one eventually. The cursor is written to
// v1:platform:syncState after EVERY page, so a restart resumes from the
// last completed page. Writing it at the end would make a restart mean
// starting over, and a backfill that can never finish is worse than one
// that is slow.

// Runner drives a connector's backfill and reconciliation.
type Runner struct {
	store   *Store
	applier *Applier
	logger  *slog.Logger
	now     func() time.Time
	lookup  func(name string) (memqlsync.Connector, bool)
	// maxPages bounds one backfill invocation so a connector whose
	// cursor never advances cannot spin forever. Reached, the run stops
	// with its cursor persisted and the next invocation continues -- a
	// bound on one call, not on the backfill.
	maxPages int
}

// NewRunner builds the backfill / reconciliation driver.
func NewRunner(store *Store, applier *Applier, logger *slog.Logger) *Runner {
	if logger == nil {
		logger = slog.Default()
	}
	return &Runner{
		store:    store,
		applier:  applier,
		logger:   logger.With("component", "datasync.runner"),
		now:      time.Now,
		lookup:   memqlsync.Lookup,
		maxPages: 500,
	}
}

// BackfillResult is what one backfill invocation achieved.
type BackfillResult struct {
	Pages   int
	Applied int
	Stale   int
	Done    bool
	Cursor  string
}

// StartBackfill drives Connector.Backfill for one domain, resuming from
// whatever cursor syncState holds.
//
// Idempotent by construction: the cursor is the resume point, so calling
// this again after a completed backfill starts from the stored cursor
// and the connector reports Done immediately.
//
// With a partition, the connector must implement memqlsync.Partitioned and
// is asked through BackfillPartition, from that partition's own cursor. A
// partitioned connector asked for no partition is backfilled once per
// partition it names, and the result is their sum: its shared Backfill is
// never called, because walking one shared cursor across tenants is the
// defect partitions remove.
func (r *Runner) StartBackfill(ctx context.Context, connectorName, conceptID, partition string) (BackfillResult, error) {
	connector, parts, specs, err := r.resolve(ctx, connectorName, conceptID, partition)
	if err != nil {
		return BackfillResult{}, err
	}
	if _, ok := connector.(memqlsync.Partitioned); !ok || strings.TrimSpace(partition) != "" {
		return r.backfillOne(ctx, connector, specs, conceptID, strings.TrimSpace(partition))
	}
	out := BackfillResult{Done: true}
	var errs []error
	for _, part := range parts {
		res, err := r.backfillOne(ctx, connector, specs, conceptID, part)
		out.Pages += res.Pages
		out.Applied += res.Applied
		out.Stale += res.Stale
		if !res.Done || err != nil {
			out.Done = false
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return out, errors.Join(errs...)
}

// resolve finds the connector, checks it serves the concept and the
// partition named, and -- for a partitioned connector asked for no
// partition -- lists the partitions a call fans out over.
func (r *Runner) resolve(ctx context.Context, connectorName, conceptID, partition string) (memqlsync.Connector, []string, map[string]memqlsync.DomainSpec, error) {
	connector, ok := r.lookup(connectorName)
	if !ok || connector == nil {
		return nil, nil, nil, fmt.Errorf("datasync: no connector bound for %q; a backfill or a sweep needs an implementation, not just a declaration", connectorName)
	}
	specs := domainSpecsByConcept(connector)
	if _, known := specs[strings.TrimSpace(conceptID)]; !known {
		return nil, nil, nil, fmt.Errorf("datasync: connector %q does not serve concept %q -- a backfill of a domain the connector does not declare has nothing to page through", connectorName, conceptID)
	}
	p, partitioned := connector.(memqlsync.Partitioned)
	if strings.TrimSpace(partition) != "" {
		if !partitioned {
			return nil, nil, nil, fmt.Errorf("datasync: connector %q has no partitions, so partition %q names nothing it serves", connectorName, strings.TrimSpace(partition))
		}
		return connector, nil, specs, nil
	}
	if !partitioned {
		return connector, nil, specs, nil
	}
	parts, err := p.Partitions(auth.ContextWithConnectorActor(ctx, connectorName))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("datasync: listing the partitions of %q: %w", connectorName, err)
	}
	return connector, cleanPartitions(parts), specs, nil
}

// cleanPartitions drops blank and repeated partitions: a blank one would
// address the shared row, which a partitioned connector does not have.
func cleanPartitions(parts []string) []string {
	out := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// backfillOne backfills one domain of one partition ("" for a connector
// without partitions).
func (r *Runner) backfillOne(ctx context.Context, connector memqlsync.Connector, specs map[string]memqlsync.DomainSpec, conceptID, partition string) (BackfillResult, error) {
	var out BackfillResult
	connectorName := connector.Name()
	opCtx := OperatorContext(ctx)
	st, err := r.store.SyncStateFor(opCtx, conceptID, connectorName, string(memqlsync.DirectionInbound), partition)
	if err != nil {
		return out, err
	}
	if st.Paused {
		// A pause is an operator's decision and outranks a request to
		// run. Returning cleanly rather than erroring: "paused" is a
		// state, not a fault, and a caller polling this must not see a
		// stream of errors for a switch somebody flipped on purpose.
		r.logger.Info("datasync backfill: domain is paused; not running",
			"connector", connectorName, "concept", conceptID, "partition", partition)
		out.Cursor = st.BackfillCursor
		return out, nil
	}

	st.ConceptID, st.Connector, st.Direction, st.Partition = conceptID, connectorName, string(memqlsync.DirectionInbound), partition
	st.BackfillStatus = "running"
	st.LastError = ""
	r.writeState(opCtx, st)

	connCtx := auth.ContextWithConnectorActor(ctx, connectorName)
	cursor := st.BackfillCursor
	fetch := func(cursor string) (memqlsync.BackfillPage, error) {
		if partition != "" {
			return connector.(memqlsync.Partitioned).BackfillPartition(connCtx, partition, conceptID, cursor)
		}
		return connector.Backfill(connCtx, conceptID, cursor)
	}

	for out.Pages < r.maxPages {
		select {
		case <-ctx.Done():
			// Cancelled mid-run. The cursor of the last COMPLETED page is
			// already persisted, so this is a resume point rather than a
			// loss; the status is left `running` so the next invocation
			// picks it up.
			out.Cursor = cursor
			return out, ctx.Err()
		default:
		}

		page, pageErr := fetch(cursor)
		if pageErr != nil {
			st.BackfillStatus = "failed"
			st.LastError = pageErr.Error()
			st.BackfillCursor = cursor
			r.writeState(opCtx, st)
			return out, fmt.Errorf("datasync: backfilling %s from %q%s: %w", conceptID, connectorName, partitionSuffix(partition), pageErr)
		}
		out.Pages++

		applied, applyErr := r.applier.Apply(connCtx, connectorName, specs, page.Writes)
		out.Applied += applied.Applied
		out.Stale += applied.Stale
		if applyErr != nil {
			st.BackfillStatus = "failed"
			st.LastError = applyErr.Error()
			st.BackfillCursor = cursor
			r.writeState(opCtx, st)
			return out, applyErr
		}

		cursor = page.NextCursor
		st.BackfillCursor = cursor
		// Persisted per PAGE. See the file header: a restart mid-backfill
		// has to resume, not restart.
		r.writeState(opCtx, st)

		if page.Done || strings.TrimSpace(cursor) == "" {
			out.Done = true
			break
		}
	}

	out.Cursor = cursor
	if out.Done {
		st.BackfillStatus = "complete"
	} else {
		// Ran out of the per-invocation page budget. Still `running`:
		// the work is not finished and the next invocation continues.
		st.BackfillStatus = "running"
		r.logger.Info("datasync backfill: page budget reached; resuming on the next run",
			"connector", connectorName, "concept", conceptID, "partition", partition, "pages", out.Pages)
	}
	st.BackfillCursor = cursor
	r.writeState(opCtx, st)

	r.logger.Info("datasync backfill: pass complete",
		"connector", connectorName, "concept", conceptID, "partition", partition,
		"pages", out.Pages, "applied", out.Applied, "stale", out.Stale, "done", out.Done)
	return out, nil
}

// partitionSuffix names a partition in an error, and nothing for none.
func partitionSuffix(partition string) string {
	if partition == "" {
		return ""
	}
	return " (partition " + partition + ")"
}

// writeState persists a domain's health and SAYS SO when it cannot.
//
// Every caller used to discard the error. That is how a refused row id kept
// every health write off the database for weeks with nothing in any log --
// the sweep ran, the runner "recorded" its result, and the Data origins page
// showed an empty table. A health write that fails is itself health news.
func (r *Runner) writeState(ctx context.Context, st SyncState) {
	if err := r.store.WriteSyncState(ctx, st); err != nil && r.logger != nil {
		r.logger.Warn("datasync: could not record a domain's health",
			"connector", st.Connector, "concept", st.ConceptID, "partition", st.Partition, "error", err)
	}
}

// ReconcileResult is what one reconciliation sweep found.
type ReconcileResult struct {
	Checked int
	Drifted int
	Healed  int
	Skipped bool
}

// Reconcile runs one sweep for a domain, from the last sweep's time.
//
// `since` is the previous lastReconcileAt, so a connector that can
// narrow its comparison does; one that cannot ignores it and sweeps
// everything, which is what the Shopify thin index does and says so.
//
// With a partition, the connector must implement memqlsync.Partitioned and
// is swept through ReconcilePartition from that partition's own watermark.
// A partitioned connector asked for no partition is swept once per
// partition it names and reports the sum; its shared Reconcile is never
// called.
func (r *Runner) Reconcile(ctx context.Context, connectorName, conceptID, partition string) (ReconcileResult, error) {
	connector, ok := r.lookup(connectorName)
	if !ok || connector == nil {
		return ReconcileResult{}, fmt.Errorf("datasync: no connector bound for %q", connectorName)
	}
	_, partitioned := connector.(memqlsync.Partitioned)
	partition = strings.TrimSpace(partition)
	if partition != "" && !partitioned {
		return ReconcileResult{}, fmt.Errorf("datasync: connector %q has no partitions, so partition %q names nothing it serves", connectorName, partition)
	}
	if !partitioned || partition != "" {
		return r.reconcileOne(ctx, connector, conceptID, partition)
	}
	parts, err := connector.(memqlsync.Partitioned).Partitions(auth.ContextWithConnectorActor(ctx, connectorName))
	if err != nil {
		return ReconcileResult{}, fmt.Errorf("datasync: listing the partitions of %q: %w", connectorName, err)
	}
	out := ReconcileResult{Skipped: true}
	var errs []error
	for _, part := range cleanPartitions(parts) {
		res, err := r.reconcileOne(ctx, connector, conceptID, part)
		if err != nil {
			errs = append(errs, err)
			out.Skipped = false
			continue
		}
		out.Checked += res.Checked
		out.Drifted += res.Drifted
		out.Healed += res.Healed
		if !res.Skipped {
			out.Skipped = false
		}
	}
	return out, errors.Join(errs...)
}

// reconcileOne sweeps one domain of one partition ("" for a connector
// without partitions).
func (r *Runner) reconcileOne(ctx context.Context, connector memqlsync.Connector, conceptID, partition string) (ReconcileResult, error) {
	var out ReconcileResult
	connectorName := connector.Name()
	opCtx := OperatorContext(ctx)
	st, err := r.store.SyncStateFor(opCtx, conceptID, connectorName, string(memqlsync.DirectionInbound), partition)
	if err != nil {
		return out, err
	}
	if st.Paused {
		out.Skipped = true
		return out, nil
	}

	connCtx := auth.ContextWithConnectorActor(ctx, connectorName)
	var report memqlsync.ReconcileReport
	var reconcileErr error
	if partition != "" {
		report, reconcileErr = connector.(memqlsync.Partitioned).ReconcilePartition(connCtx, partition, conceptID, st.LastReconcileAt)
	} else {
		report, reconcileErr = connector.Reconcile(connCtx, conceptID, st.LastReconcileAt)
	}

	st.ConceptID, st.Connector, st.Direction, st.Partition = conceptID, connectorName, string(memqlsync.DirectionInbound), partition
	if reconcileErr != nil {
		switch {
		case memqlsync.IsNotImplemented(reconcileErr):
			// A connector that does not reconcile is a configuration
			// fact, not a failure. Recorded as such and not written onto
			// lastError, which is for things that went wrong.
			out.Skipped = true
			return out, nil
		case memqlsync.IsNotGranted(reconcileErr), memqlsync.IsNotRequested(reconcileErr):
			// The origin never granted this deployment the domain (an
			// access scope the connection was made without). Skipped like
			// a connector that does not reconcile -- and unlike it,
			// RECORDED, so the Data origins page says why the domain is
			// idle and what a grant would change. Written when the reason
			// changes and otherwise no more than once per
			// notGrantedRecheck: the health row is an append-only
			// timeline, and 22 domains restating the same reason every
			// ten minutes is a table nobody asked for. The attempt stamp
			// is what holds the domain to its cadence between writes.
			//
			// A domain outside the store's connection (not requested) is
			// held the same way: it is a standing fact, recorded once and
			// restated no more often, and it is not a failure.
			out.Skipped = true
			msg := truncateError(reconcileErr.Error())
			if st.LastError != msg || r.now().UTC().Sub(st.LastAttemptAt) >= notGrantedRecheck {
				st.LastError = msg
				st.LastAttemptAt = r.now().UTC()
				r.writeState(opCtx, st)
			}
			return out, nil
		case memqlsync.IsPermanent(reconcileErr):
			// A refusal the next tick cannot change: a generated query the
			// origin rejects. Still reported -- somebody has to fix the
			// query -- and stamped as an ATTEMPT, so the domain waits its
			// own cadence instead of repeating the identical refusal on
			// every tick. lastReconcileAt is left alone: it is the
			// updated_at watermark the next sweep narrows on, and moving
			// it for a sweep that never ran would skip the drift between
			// the last real sweep and the refusal once the refusal clears.
			st.LastError = reconcileErr.Error()
			st.LastAttemptAt = r.now().UTC()
			r.writeState(opCtx, st)
			return out, fmt.Errorf("datasync: reconciling %s against %q%s: %w", conceptID, connectorName, partitionSuffix(partition), reconcileErr)
		}
		st.LastError = reconcileErr.Error()
		r.writeState(opCtx, st)
		return out, fmt.Errorf("datasync: reconciling %s against %q%s: %w", conceptID, connectorName, partitionSuffix(partition), reconcileErr)
	}

	out.Checked, out.Drifted, out.Healed = report.Checked, report.Drifted, report.Healed
	st.LastReconcileAt = r.now().UTC()
	st.LastAttemptAt = st.LastReconcileAt
	st.DriftCount = report.Drifted
	st.LastError = ""
	r.writeState(opCtx, st)

	// Logged at Warn when drift was found even though it was healed:
	// recurring drift is a webhook stream that is not arriving, and the
	// heal hides the symptom while leaving the cause.
	if report.Drifted > 0 {
		r.logger.Warn("datasync reconcile: drift found",
			"connector", connectorName, "concept", conceptID, "partition", partition,
			"checked", report.Checked, "drifted", report.Drifted, "healed", report.Healed)
	} else {
		r.logger.Info("datasync reconcile: clean",
			"connector", connectorName, "concept", conceptID, "partition", partition, "checked", report.Checked)
	}
	return out, nil
}

// ReconcileDue reports whether a domain's schedule says it is time.
//
// A DomainSpec with a zero ReconcileInterval is never due: reconciliation
// is then operator-driven only, which is the right default for a domain
// whose comparison is expensive at the origin.
//
// It reads the one row the partition names. A partitioned connector asked
// for no partition is due when ANY of its partitions is, and its shared row
// -- which nothing writes -- is never read; the sweep itself asks once per
// partition.
func (r *Runner) ReconcileDue(ctx context.Context, connectorName string, spec memqlsync.DomainSpec, partition string) bool {
	if spec.ReconcileInterval <= 0 {
		return false
	}
	partition = strings.TrimSpace(partition)
	parts, fanOut, err := r.fanOut(ctx, connectorName, partition)
	if err != nil {
		return false
	}
	if !fanOut {
		return r.dueOne(ctx, connectorName, spec, partition)
	}
	for _, part := range parts {
		if r.dueOne(ctx, connectorName, spec, part) {
			return true
		}
	}
	return false
}

// fanOut reports whether a call naming this partition fans out -- a
// partitioned connector asked for none -- and over which partitions. A
// connector that is not bound, or has no partitions, does not fan out.
func (r *Runner) fanOut(ctx context.Context, connectorName, partition string) ([]string, bool, error) {
	if partition != "" {
		return nil, false, nil
	}
	connector, ok := r.lookup(connectorName)
	if !ok || connector == nil {
		return nil, false, nil
	}
	p, partitioned := connector.(memqlsync.Partitioned)
	if !partitioned {
		return nil, false, nil
	}
	parts, err := p.Partitions(auth.ContextWithConnectorActor(ctx, connectorName))
	if err != nil {
		return nil, true, fmt.Errorf("datasync: listing the partitions of %q: %w", connectorName, err)
	}
	return cleanPartitions(parts), true, nil
}

// dueOne is ReconcileDue for one row.
func (r *Runner) dueOne(ctx context.Context, connectorName string, spec memqlsync.DomainSpec, partition string) bool {
	st, err := r.store.SyncStateFor(OperatorContext(ctx), spec.Concept, connectorName, string(memqlsync.DirectionInbound), partition)
	if err != nil || st.Paused {
		return false
	}
	// The cadence runs from the last ATTEMPT when that is later than the
	// last finished sweep: a refused or not-granted domain waits its
	// interval like any other, while `since` keeps pointing at the last
	// sweep that ran.
	last := st.LastReconcileAt
	if st.LastAttemptAt.After(last) {
		last = st.LastAttemptAt
	}
	if last.IsZero() {
		return true
	}
	return r.now().UTC().Sub(last) >= spec.ReconcileInterval
}

// notGrantedRecheck bounds how often a not-granted domain re-writes its
// unchanged reason: a standing fact restated a few times a day, not on
// every tick.
const notGrantedRecheck = 6 * time.Hour

// SetPaused flips a domain's pause switch, stopping both runners and the
// drain for it. A partition's pause is its own: it names that partition's
// row and no other. A partitioned connector asked for no partition is
// paused on every partition's row, and never on the shared row, which
// nothing reads.
func (r *Runner) SetPaused(ctx context.Context, connectorName, conceptID, partition string, paused bool) error {
	partition = strings.TrimSpace(partition)
	parts, fanOut, err := r.fanOut(ctx, connectorName, partition)
	if err != nil {
		return err
	}
	if !fanOut {
		return r.setPausedOne(ctx, connectorName, conceptID, partition, paused)
	}
	var errs []error
	for _, part := range parts {
		if err := r.setPausedOne(ctx, connectorName, conceptID, part, paused); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// setPausedOne flips one row's pause switch.
func (r *Runner) setPausedOne(ctx context.Context, connectorName, conceptID, partition string, paused bool) error {
	opCtx := OperatorContext(ctx)
	st, err := r.store.SyncStateFor(opCtx, conceptID, connectorName, string(memqlsync.DirectionInbound), partition)
	if err != nil {
		return err
	}
	if !st.Stored {
		// The domain has no health row yet, so there is nothing to flip:
		// setSyncPaused is an update, and the engine refuses an update of
		// a row that does not exist. Write one carrying the pause rather
		// than failing: pausing a domain before its first delivery is a
		// legitimate thing to want. (The empty state already carries the
		// id it would have, so an empty-id check here never fired.)
		st.ConceptID, st.Connector, st.Direction, st.Partition = conceptID, connectorName, string(memqlsync.DirectionInbound), partition
		st.Paused = paused
		return r.store.WriteSyncState(opCtx, st)
	}
	return r.store.SetPaused(opCtx, st.ID, paused)
}
