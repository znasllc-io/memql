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
	// bound lists the connectors a continuing pass walks: the registry's
	// bound names, unless a test sets it.
	bound func() []string
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
		bound:    memqlsync.BoundNames,
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
	// Waiting says the origin was busy with another operation of the same
	// kind and took none of this one. The row is left queued (or running,
	// when it already was) with no error, and the continuing pass asks again.
	Waiting bool
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
		if res.Waiting {
			out.Waiting = true
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
			return nil, nil, nil, noPartitions(connectorName, partition)
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

// noPartitions is the refusal of a call naming a partition on a connector
// that does not implement memqlsync.Partitioned: its runners only ever read
// the shared row, so a partition's row would be one nothing reads.
func noPartitions(connectorName, partition string) error {
	return fmt.Errorf("datasync: connector %q has no partitions, so partition %q names nothing it serves", connectorName, strings.TrimSpace(partition))
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
	priorStatus, priorError := st.BackfillStatus, st.LastError
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

	polling := false
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
		if pageErr != nil && memqlsync.IsBusy(pageErr) {
			// The origin is busy with another operation of the same kind
			// (Shopify runs one bulk query per shop at a time). Nothing is
			// wrong with this domain and nothing is recorded as an error: a
			// backfill that had not begun goes back to queued, one already
			// under way stays running at its cursor, and the continuing pass
			// asks again on its next tick. Marking it failed would end it,
			// because nothing retries a failed backfill.
			if priorStatus != "running" && out.Pages == 0 {
				st.BackfillStatus = "queued"
			}
			st.LastError = ""
			st.BackfillCursor = cursor
			r.writeState(opCtx, st)
			r.logger.Info("datasync backfill: the origin is busy with another operation; waiting for the next tick",
				"connector", connectorName, "concept", conceptID, "partition", partition, "reason", pageErr.Error())
			out.Cursor, out.Waiting = cursor, true
			return out, nil
		}
		if pageErr != nil && (memqlsync.IsNotGranted(pageErr) || memqlsync.IsNotRequested(pageErr)) &&
			!st.LastReconcileAt.IsZero() && !standingRefusal(priorError) {
			// The same refusal from a domain whose row shows the sweep READS
			// it: lastReconcileAt is set and the row's error is no refusal.
			// That is not a standing answer about the domain but a failure
			// of the backfill alone, and recording it as a refusal would let
			// the next clean sweep queue it again, refusal after refusal,
			// forever. It is failed, worded so that neither the refusal
			// checks nor the clean-sweep rule read it as a refusal.
			st.BackfillStatus = "failed"
			st.LastError = truncateError(backfillRefusedPrefix + pageErr.Error())
			st.LastAttemptAt = r.now().UTC()
			st.BackfillCursor = cursor
			r.writeState(opCtx, st)
			return out, fmt.Errorf("datasync: backfilling %s from %q%s: %s%v", conceptID, connectorName, partitionSuffix(partition), backfillRefusedPrefix, pageErr)
		}
		if pageErr != nil && (memqlsync.IsNotGranted(pageErr) || memqlsync.IsNotRequested(pageErr)) {
			// The origin's standing answer -- a domain the grant does not
			// reach, or one outside the connection -- is not a failure. It
			// is recorded the way the sweep records it, and the backfill
			// leaves queued/running for `none`, so a continuing pass does
			// not ask the same question on every tick. A clean sweep of the
			// domain (reconcileOne) queues it again once the answer changes.
			// That holds only while the row shows no clean sweep; see above.
			st.BackfillStatus = "none"
			st.LastError = truncateError(pageErr.Error())
			st.LastAttemptAt = r.now().UTC()
			st.BackfillCursor = cursor
			r.writeState(opCtx, st)
			return out, fmt.Errorf("datasync: backfilling %s from %q%s: %w", conceptID, connectorName, partitionSuffix(partition), pageErr)
		}
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

		if !page.Done && len(page.Writes) == 0 && strings.TrimSpace(cursor) != "" && page.NextCursor == cursor {
			// No writes and the cursor it was given back: a poll of an
			// operation the origin is still running. Asking again at once
			// would spend the page budget waiting, so the invocation stops
			// here, still running, and the next one polls again.
			polling = true
			break
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
		// Ran out of the per-invocation page budget, or the origin is still
		// preparing what the next page reads. Still `running`: the work is
		// not finished and the next invocation continues.
		st.BackfillStatus = "running"
		if !polling {
			r.logger.Info("datasync backfill: page budget reached; resuming on the next run",
				"connector", connectorName, "concept", conceptID, "partition", partition, "pages", out.Pages)
		}
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
		return ReconcileResult{}, noPartitions(connectorName, partition)
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

	// The watermark is when the sweep STARTED, less the overlap, so a row
	// that changed during the sweep is read by the next one.
	sweepStart := r.now().UTC()
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
	st.LastReconcileAt = sweepStart.Add(-reconcileOverlap)
	st.LastAttemptAt = r.now().UTC()
	st.DriftCount = report.Drifted
	if standingRefusal(st.LastError) && st.BackfillStatus != "running" {
		// The first clean sweep of a domain the origin had refused: the
		// answer changed (a wider grant), and the domain's past was never
		// read. Its backfill is queued from the start, in the same write.
		st.BackfillStatus = "queued"
		st.BackfillCursor = ""
	}
	if st.BackfillStatus != "failed" || !strings.HasPrefix(st.LastError, backfillRefusedPrefix) {
		// A clean sweep clears the sweep's own error. A backfill the origin
		// refused while the sweep reads the domain keeps its reason: the
		// sweep being clean is what made that refusal a failure.
		st.LastError = ""
	}
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
		// Partitions could not be listed, or the partition names nothing
		// this connector serves: not due, and nothing is read.
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
// connector that is not bound, or has no partitions, does not fan out. A
// partition named on a bound connector without partitions is refused, as
// resolve refuses it: its row would be one this connector's runners never
// read.
func (r *Runner) fanOut(ctx context.Context, connectorName, partition string) ([]string, bool, error) {
	connector, ok := r.lookup(connectorName)
	if !ok || connector == nil {
		return nil, false, nil
	}
	p, partitioned := connector.(memqlsync.Partitioned)
	if partition != "" {
		if !partitioned {
			return nil, false, noPartitions(connectorName, partition)
		}
		return nil, false, nil
	}
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

// reconcileOverlap is how far before a sweep's start the next sweep's
// `since` reaches. A row that changed while the sweep ran, behind the page
// it had already read, is older than the sweep's end; reading from the
// start less this margin picks it up. Applying a row twice is harmless:
// the apply is version-guarded.
const reconcileOverlap = 2 * time.Minute

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

// backfillRefusedPrefix opens the lastError of a backfill the origin refused
// although the sweep reads the domain: a failure, which standingRefusal and
// the connector's phase reading must not take for a standing answer.
const backfillRefusedPrefix = "backfill refused: "

// standingRefusal reports whether a stored lastError is the origin's standing
// answer -- not granted, or not requested -- rather than a failure. The row
// keeps the reason's text, so it is read by the sentinels' own prefixes, as
// the connector's store health reads it.
func standingRefusal(lastError string) bool {
	return strings.HasPrefix(lastError, memqlsync.ErrNotGranted.Error()) ||
		strings.HasPrefix(lastError, memqlsync.ErrNotRequested.Error())
}

// QueueBackfill records that a domain's backfill is wanted, and runs
// nothing: the continuing pass (ContinueBackfills) starts it. It writes
// `queued` and an empty cursor. A backfill already `running` is left as it is;
// any other -- a complete one included -- is queued again from the start, as a
// reconnect reads the store again and every write is idempotent on the row id.
//
// A partitioned connector asked for no partition is queued for every
// partition it names, and its shared row is never written; a partition named
// on a connector without partitions is refused, as StartBackfill refuses it.
func (r *Runner) QueueBackfill(ctx context.Context, connectorName, conceptID, partition string) error {
	connector, parts, _, err := r.resolve(ctx, connectorName, conceptID, partition)
	if err != nil {
		return err
	}
	partition = strings.TrimSpace(partition)
	if _, ok := connector.(memqlsync.Partitioned); !ok || partition != "" {
		return r.queueOne(ctx, connector.Name(), conceptID, partition)
	}
	var errs []error
	for _, part := range parts {
		if err := r.queueOne(ctx, connector.Name(), conceptID, part); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// queueOne queues one domain of one partition.
func (r *Runner) queueOne(ctx context.Context, connectorName, conceptID, partition string) error {
	opCtx := OperatorContext(ctx)
	st, err := r.store.SyncStateFor(opCtx, conceptID, connectorName, string(memqlsync.DirectionInbound), partition)
	if err != nil {
		return err
	}
	if st.BackfillStatus == "running" {
		return nil
	}
	st.ConceptID, st.Connector, st.Direction, st.Partition = conceptID, connectorName, string(memqlsync.DirectionInbound), partition
	st.BackfillStatus = "queued"
	st.BackfillCursor = ""
	return r.store.WriteSyncState(opCtx, st)
}

// ContinueResult is what one continuing pass did. Started counts domains
// moved from queued to running in the pass, Advanced those already running
// that made progress and are still running, and Completed those that reached
// complete in it. A domain started and completed in one pass counts in both
// Started and Completed.
type ContinueResult struct{ Started, Advanced, Completed int }

// ContinueBackfills is one tick of the continuing pass: for each partition of
// each bound connector ("" for a connector without partitions), it takes at
// most one domain -- the one that is running, else the first queued in
// Domains() order -- and advances it by one invocation of the page budget. A
// partition never has a second domain started while one is running, and a
// paused domain is left alone: it is neither taken nor counted as running.
//
// It reads each domain's row BY ID (SyncStateFor), never by listing the rows
// that are queued or running. A list over v1:platform:syncState answers each
// row at its current version, but the engine finds those by enumerating the
// concept's versions, and every page of every backfill writes one, so a tick
// that listed would cost more with every backfill ever run. By id it costs the
// partitions times the domains, and exact. A partitioned connector's shared
// row is never read, so a row with no partition is not continued.
//
// A refusal that is the origin's standing answer is recorded on its row and is
// not an error of the pass; any other failure is logged, recorded `failed` on
// its row, and returned with the rest once every partition has had its turn.
// The pass stops when its context is done.
func (r *Runner) ContinueBackfills(ctx context.Context) (ContinueResult, error) {
	var out ContinueResult
	var errs []error
	for _, name := range r.bound() {
		if err := ctx.Err(); err != nil {
			return out, errors.Join(append(errs, err)...)
		}
		connector, ok := r.lookup(name)
		if !ok || connector == nil {
			continue
		}
		parts, fanOut, err := r.fanOut(ctx, name, "")
		if err != nil {
			r.logger.Warn("datasync: could not list a connector's partitions; its backfills wait for the next tick",
				"connector", name, "error", err)
			errs = append(errs, err)
			continue
		}
		if !fanOut {
			parts = []string{""}
		}
		specs := domainSpecsByConcept(connector)
		for _, part := range parts {
			if err := ctx.Err(); err != nil {
				return out, errors.Join(append(errs, err)...)
			}
			res, err := r.continueOne(ctx, connector, specs, part)
			out.Started += res.Started
			out.Advanced += res.Advanced
			out.Completed += res.Completed
			if err != nil {
				errs = append(errs, err)
			}
		}
	}
	return out, errors.Join(errs...)
}

// continueOne advances the one domain of one partition the pass takes.
func (r *Runner) continueOne(ctx context.Context, connector memqlsync.Connector, specs map[string]memqlsync.DomainSpec, partition string) (ContinueResult, error) {
	var out ContinueResult
	name := connector.Name()
	opCtx := OperatorContext(ctx)
	var running, queued *SyncState
	for _, spec := range connector.Domains() {
		if spec.Direction == memqlsync.DirectionOutbound {
			continue
		}
		st, err := r.store.SyncStateFor(opCtx, spec.Concept, name, string(memqlsync.DirectionInbound), partition)
		if err != nil {
			return out, err
		}
		if st.Paused {
			continue
		}
		st.ConceptID = spec.Concept
		if st.BackfillStatus == "running" {
			running = &st
			break
		}
		if st.BackfillStatus == "queued" && queued == nil {
			queued = &st
		}
	}
	pick := running
	if pick == nil {
		pick = queued
	}
	if pick == nil {
		return out, nil
	}

	res, err := r.backfillOne(ctx, connector, specs, pick.ConceptID, partition)
	if res.Waiting {
		// The origin is busy: nothing moved, so nothing is counted.
		return out, nil
	}
	if pick == queued {
		out.Started++
	}
	switch {
	case err != nil && (memqlsync.IsNotGranted(err) || memqlsync.IsNotRequested(err)):
		r.logger.Info("datasync backfill: the origin refuses this domain; recorded, and not asked again until it changes",
			"connector", name, "concept", pick.ConceptID, "partition", partition, "reason", err.Error())
		return out, nil
	case err != nil:
		r.logger.Warn("datasync backfill: a continued backfill failed; the other partitions still run",
			"connector", name, "concept", pick.ConceptID, "partition", partition, "error", err)
		return out, err
	case res.Done:
		out.Completed++
	case pick == running && (res.Cursor != pick.BackfillCursor || res.Applied+res.Stale > 0):
		out.Advanced++
	}
	return out, nil
}
