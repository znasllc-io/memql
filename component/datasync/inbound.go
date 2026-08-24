package datasync

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

// inbound.go -- the inbound dispatcher and the version guard
// (epic memql#4378, D6).
//
// A webhook arrives at POST /inbound/{source}, is signature-checked at
// the edge, and is STAGED as a v1:platform:inboundRequest row. This is
// what happens next: route the row to the connector its `source` names,
// hand it to Apply, write what comes back behind the version guard, and
// stamp the request.
//
// # The version guard, and what "the stored row's version" means
//
// Webhooks are at-least-once and out-of-order. Without a guard, a
// delivery that overtook another on the wire overwrites newer data with
// older, and the mirror is quietly wrong until the next reconciliation
// -- which, if the same reordering recurs, never settles.
//
// So every MirrorWrite carries the ORIGIN's version and is compared
// against what MemQL already holds. What "already holds" means depends on
// what the origin gives, and DomainSpec.VersionField is how a connector
// says which:
//
//   - VersionField SET -- the mirror concept keeps the origin's own
//     version (an updated_at, a sequence) in that payload field, and the
//     comparison is between two of the origin's values. Exact.
//   - VersionField EMPTY -- the origin offers nothing better, so the
//     version IS the delivery time (D6's stated fallback) and the
//     comparison is against the row's own createdAt: when MemQL last
//     applied a delivery for it. Coarser, and honest about being so --
//     two deliveries inside one clock tick are indistinguishable, which
//     for an origin that publishes no version is the best available
//     answer rather than a bug.
//
// A write that loses the comparison is RECORDED as stale and skipped,
// never applied. Recorded rather than dropped because a mirror that
// keeps rejecting stale writes is a webhook stream that is reordering,
// and an operator who cannot see that has no way to know their origin
// needs looking at.

// Applier applies inbound mirror writes. Split from the dispatcher so
// the backfill and reconciliation runners share one write path with it
// -- the version guard has to be the same guard on all three, or the
// sweep that heals drift becomes the sweep that reintroduces it.
type Applier struct {
	store  *Store
	writer MirrorWriter
	now    func() time.Time
}

// MirrorWriter performs one write into a mirror. Narrow on purpose: the
// runtime decides WHETHER to write (the guard) and something else knows
// HOW, because the how is per-connector -- the mutations that carry the
// concept's field contract belong to the domain, not here.
type MirrorWriter interface {
	// WriteMirror applies one write under the connector's actor and
	// returns the version now stored for that row.
	WriteMirror(ctx context.Context, connector string, w memqlsync.MirrorWrite) error
	// StoredVersion returns the version MemQL currently holds for a row,
	// and whether the row exists at all.
	StoredVersion(ctx context.Context, spec memqlsync.DomainSpec, rowID string) (string, bool, error)
}

// NewApplier builds the shared write path.
func NewApplier(store *Store, writer MirrorWriter) *Applier {
	return &Applier{store: store, writer: writer, now: time.Now}
}

// ApplyResult is what one batch of mirror writes did.
type ApplyResult struct {
	Applied int
	Stale   int
}

// Apply writes each MirrorWrite that is not stale, under the connector's
// actor.
//
// Errors are returned on the FIRST failure rather than accumulated: the
// writes in one batch come from one delivery and are usually one row's
// worth, so continuing past a failure would apply half a change and
// report success for it.
func (a *Applier) Apply(
	ctx context.Context,
	connector string,
	specs map[string]memqlsync.DomainSpec,
	writes []memqlsync.MirrorWrite,
) (ApplyResult, error) {
	var out ApplyResult
	for _, w := range writes {
		spec := specs[strings.TrimSpace(w.Concept)]
		stale, err := a.isStale(ctx, spec, w)
		if err != nil {
			return out, err
		}
		if stale {
			out.Stale++
			continue
		}
		if err := a.writer.WriteMirror(ctx, connector, w); err != nil {
			return out, fmt.Errorf("datasync: applying %s %q from %q: %w", w.Concept, w.RowId, connector, err)
		}
		out.Applied++
	}
	return out, nil
}

// isStale reports whether this write is older than what MemQL holds.
//
// A row MemQL does not have is never stale: a first delivery has nothing
// to be older than, and refusing it would leave the mirror permanently
// empty. Same for a write carrying no version -- a connector that cannot
// say when a change happened gets last-write-wins, which is what it had
// before the guard existed.
func (a *Applier) isStale(ctx context.Context, spec memqlsync.DomainSpec, w memqlsync.MirrorWrite) (bool, error) {
	incoming := strings.TrimSpace(w.Version)
	if incoming == "" {
		return false, nil
	}
	stored, exists, err := a.writer.StoredVersion(ctx, spec, w.RowId)
	if err != nil {
		return false, err
	}
	if !exists || strings.TrimSpace(stored) == "" {
		return false, nil
	}
	return compareVersions(incoming, stored) < 0, nil
}

// compareVersions orders two version strings.
//
// RFC3339 timestamps are compared as INSTANTS, not as strings: the two
// orders agree only while both sides use the same precision and offset,
// and an origin that sends "+00:00" where MemQL stored "Z" would compare
// wrong for every row. Anything that does not parse as a timestamp falls
// back to a lexicographic compare, which is the right answer for a
// zero-padded sequence and an honest guess for anything else.
func compareVersions(a, b string) int {
	ta, aok := parseVersionTime(a)
	tb, bok := parseVersionTime(b)
	if aok && bok {
		switch {
		case ta.Before(tb):
			return -1
		case ta.After(tb):
			return 1
		default:
			return 0
		}
	}
	return strings.Compare(a, b)
}

func parseVersionTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// Dispatcher routes a staged inbound request to its connector.
type Dispatcher struct {
	store   *Store
	applier *Applier
	lookup  func(name string) (memqlsync.Connector, bool)
	now     func() time.Time
}

// NewDispatcher builds the inbound router.
func NewDispatcher(store *Store, applier *Applier) *Dispatcher {
	return &Dispatcher{store: store, applier: applier, lookup: memqlsync.Lookup, now: time.Now}
}

// DispatchResult is what one staged request produced.
type DispatchResult struct {
	// Handled is false when no connector serves this source. NOT an
	// error: /inbound/{source} is a shared door and most of what comes
	// through it belongs to something else.
	Handled bool
	Applied int
	Stale   int
}

// Dispatch works one staged inbound request.
//
// The request row is stamped `processed` or `failed` either way, so an
// operator reading the staged queue sees what happened to each delivery
// rather than a row that stayed `received` forever.
func (d *Dispatcher) Dispatch(ctx context.Context, req memqlsync.InboundRequest) (DispatchResult, error) {
	var out DispatchResult
	source := strings.TrimSpace(req.Source)
	if source == "" {
		return out, nil
	}
	connector, ok := d.lookup(source)
	if !ok || connector == nil {
		return out, nil
	}
	out.Handled = true

	specs := domainSpecsByConcept(connector)

	// Apply runs under the CONNECTOR's actor, which is the only identity
	// a mirror accepts a write from. The staged-request stamps below run
	// under the OPERATOR identity, because the request row is the
	// deployment's bookkeeping rather than the connector's domain -- two
	// authorities, one dispatch, and the call sites say which is which.
	connCtx := auth.ContextWithConnectorActor(ctx, connector.Name())
	opCtx := OperatorContext(ctx)

	writes, err := connector.Apply(connCtx, req)
	if err != nil {
		d.stamp(opCtx, req.RequestId, "failed", err.Error())
		return out, fmt.Errorf("datasync: %q applying inbound request %q: %w", source, req.RequestId, err)
	}
	if len(writes) == 0 {
		// A delivery this connector does not recognise. Stamped
		// `processed` rather than left alone: the row HAS been worked,
		// and leaving it `received` makes the staged queue grow without
		// bound and lose its meaning as a to-do list.
		d.stamp(opCtx, req.RequestId, "processed", "")
		return out, nil
	}

	res, applyErr := d.applier.Apply(connCtx, connector.Name(), specs, writes)
	out.Applied, out.Stale = res.Applied, res.Stale
	if applyErr != nil {
		d.stamp(opCtx, req.RequestId, "failed", applyErr.Error())
		return out, applyErr
	}
	d.stamp(opCtx, req.RequestId, "processed", "")

	d.recordInboundHealth(opCtx, connector, writes, req.ReceivedAt)
	return out, nil
}

// stamp records the request's handling outcome. Best effort: a failure
// to stamp must not turn a successful apply into a reported failure, and
// the delivery is durable either way.
func (d *Dispatcher) stamp(ctx context.Context, requestID, status, lastError string) {
	if strings.TrimSpace(requestID) == "" {
		return
	}
	_, _ = d.store.exec(ctx, call("mutation", "updateInboundRequestStatus",
		arg{"requestId", requestID},
		arg{"status", status},
		arg{"lastError", truncateError(lastError)},
		arg{"processedAt", d.now().UTC().Format(time.RFC3339)}))
}

// recordInboundHealth updates lastInboundAt and lagSeconds for every
// domain this delivery touched.
//
// Lag is measured from the ORIGIN's version to now, not from when the
// row was staged: staging latency is MemQL's own and is small, while the
// number an operator needs is how far behind the mirror is from the
// system of record.
func (d *Dispatcher) recordInboundHealth(
	ctx context.Context,
	connector memqlsync.Connector,
	writes []memqlsync.MirrorWrite,
	receivedAt time.Time,
) {
	now := d.now().UTC()
	seen := map[string]struct{}{}
	for _, w := range writes {
		conceptID := strings.TrimSpace(w.Concept)
		if conceptID == "" {
			continue
		}
		if _, done := seen[conceptID]; done {
			continue
		}
		seen[conceptID] = struct{}{}

		origin := receivedAt
		if t, ok := parseVersionTime(w.Version); ok {
			origin = t
		}
		lag := 0
		if !origin.IsZero() && now.After(origin) {
			lag = int(now.Sub(origin).Seconds())
		}

		st, err := d.store.SyncStateFor(ctx, conceptID, connector.Name(), string(memqlsync.DirectionInbound))
		if err != nil {
			continue
		}
		st.ConceptID, st.Connector, st.Direction = conceptID, connector.Name(), string(memqlsync.DirectionInbound)
		st.LastInboundAt = now
		st.LagSeconds = lag
		st.LastError = ""
		_ = d.store.WriteSyncState(ctx, st)
	}
}

// domainSpecsByConcept indexes a connector's declared domains.
func domainSpecsByConcept(c memqlsync.Connector) map[string]memqlsync.DomainSpec {
	out := map[string]memqlsync.DomainSpec{}
	if c == nil {
		return out
	}
	for _, spec := range c.Domains() {
		if name := strings.TrimSpace(spec.Concept); name != "" {
			out[name] = spec
		}
	}
	return out
}
