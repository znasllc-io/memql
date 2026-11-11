package datasync

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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
	lookup  func(ctx context.Context, name string) (memqlsync.Connector, bool)
	now     func() time.Time
	logger  *slog.Logger
}

// NewDispatcher builds the inbound router.
func NewDispatcher(store *Store, applier *Applier) *Dispatcher {
	return &Dispatcher{store: store, applier: applier, lookup: memqlsync.ConnectorForSource, now: time.Now}
}

// WithLogger gives the dispatcher somewhere to say that a health write was
// refused; without one the refusal is dropped, which is how the first
// SyncStateID kept every health row off the database unnoticed.
func (d *Dispatcher) WithLogger(logger *slog.Logger) *Dispatcher {
	d.logger = logger
	return d
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
// A row whose source no connector serves is a NO-OP: nothing is asked,
// nothing is stamped, and Handled is false. It belongs to whatever does serve
// that source -- a product automation -- and so does its status.
//
// A row a connector DOES serve is stamped `processed` or `failed` either way,
// so an operator reading the staged queue sees what happened to each delivery
// rather than a row that stayed `received` forever. That includes a row whose
// staged metadata cannot be read (DispatchStaged): it is stamped `failed`
// once the connector has claimed it, and never before, so the no-op above
// holds for every row of an unserved source whatever its metadata says.
func (d *Dispatcher) Dispatch(ctx context.Context, req memqlsync.InboundRequest) (DispatchResult, error) {
	connector, ok := d.connectorFor(ctx, req.Source)
	if !ok {
		return DispatchResult{}, nil
	}
	return d.dispatchTo(ctx, connector, req, 0)
}

// StagedRequest is one staged row as the dispatcher read it
// (Store.StagedInboundRequest), its delivery metadata still in the stored
// string form. There is no topic: a staged row carries none, and a connector
// that wants the origin's event name reads it from the headers.
type StagedRequest struct {
	RequestId   string
	Source      string
	Body        []byte
	HeadersJSON string
	ReceivedAt  string
	// VerifiedBy is the tier whose secret verified the delivery, as the
	// receiver stamped it (memql#5795); empty on a row that predates it.
	VerifiedBy string
	// SignatureVerified is the receiver's verdict: false on a row staged by a
	// source configured to verify nothing (scheme none).
	SignatureVerified bool
	// Attempts is how many times the dispatcher has tried the delivery; 0 on
	// a row nobody has tried, or one an operator put back in line.
	Attempts int
	// Status is the row's handling status as read: `received` until the row
	// is worked.
	Status string
	// Version is the stored version's time (row.createdAt), the witness the
	// retry pass's claim is fenced on.
	Version time.Time
}

// Staged reads one staged delivery by its row id, under the operator
// identity the staged inbound requests are the deployment's bookkeeping under
// (store.go). The dispatch builtin is handed that id and nothing else, so this
// is where every delivery a connector sees comes from.
func (d *Dispatcher) Staged(ctx context.Context, requestID string) (StagedRequest, bool, error) {
	return d.store.StagedInboundRequest(OperatorContext(ctx), requestID)
}

// DispatchStaged is Dispatch for a row still in its staged form. The
// connector is resolved FIRST and the row judged only after, which is what
// keeps a row of an unserved source a no-op whatever its tier, its verdict or
// its metadata say. A served row another tier verified, that nothing verified,
// or whose metadata does not parse is stamped `failed` and returned as an
// error, as Dispatch does with an apply failure: left `received`, it would say
// nothing to the operator and, because @createOnly keeps the stored values,
// re-fire identically on every re-stage.
func (d *Dispatcher) DispatchStaged(ctx context.Context, staged StagedRequest) (DispatchResult, error) {
	connector, ok := d.connectorFor(ctx, staged.Source)
	if !ok {
		return DispatchResult{}, nil
	}
	// THE CONNECTOR's SECRET, OR NOTHING (memql#5795). The connector reads a
	// row under a name it claims as signed by its own secret for that tenant
	// -- and binds a per-store source's tenant off the NAME -- so a row another
	// tier verified must not reach it. The receiver refuses such a pin at
	// receipt; this is the half that holds when the claim appeared after
	// receipt (a store connected between staging and dispatch). An absent tier
	// is a row staged before the field existed and is dispatched as before:
	// only server-side code can stage a row, so absence is history, not a
	// forgery.
	if tier := strings.TrimSpace(staged.VerifiedBy); tier != "" && tier != memqlsync.VerifiedByConnector {
		reason := fmt.Sprintf("source %q is served by connector %q, but this delivery was verified by the %s tier's secret, not the connector's; "+
			"rename or remove the env or registered source that verified it", strings.TrimSpace(staged.Source), connector.Name(), tier)
		d.stampFailed(OperatorContext(ctx), strings.TrimSpace(staged.RequestId), staged.Attempts, reason, false)
		return DispatchResult{Handled: true}, fmt.Errorf("datasync: %s", reason)
	}
	// THE RECEIVER'S VERDICT, OR NOTHING (epic memql#5477). The tier above says
	// WHOSE secret verified a row; this says that one did. A row a source
	// configured to verify nothing (scheme none) staged was signed by nobody,
	// and a connector reads a claimed row as signed. No connector declares
	// scheme none today, so a delivery the receiver stages for one always
	// carries true; this is the half that holds if one ever does, and for a
	// row with no tier -- staged before the field existed -- that no secret
	// verified. The mailbox reader's rows carry true: provenance is their
	// verification.
	if !staged.SignatureVerified {
		reason := fmt.Sprintf("source %q is served by connector %q, but this delivery's signature was not verified; "+
			"a connector applies only a delivery a secret verified, so its source must not be configured scheme none",
			strings.TrimSpace(staged.Source), connector.Name())
		d.stampFailed(OperatorContext(ctx), strings.TrimSpace(staged.RequestId), staged.Attempts, reason, false)
		return DispatchResult{Handled: true}, fmt.Errorf("datasync: %s", reason)
	}
	req, reason := staged.parse(d.now)
	if reason != "" {
		d.stampFailed(OperatorContext(ctx), req.RequestId, staged.Attempts, reason, false)
		return DispatchResult{Handled: true}, fmt.Errorf("datasync: %s", reason)
	}
	return d.dispatchTo(ctx, connector, req, staged.Attempts)
}

// parse turns the staged strings into the connector's InboundRequest, or
// names why it cannot. An absent receipt time falls back to now.
func (s StagedRequest) parse(now func() time.Time) (memqlsync.InboundRequest, string) {
	req := memqlsync.InboundRequest{
		RequestId:  strings.TrimSpace(s.RequestId),
		Source:     strings.TrimSpace(s.Source),
		Body:       s.Body,
		ReceivedAt: now().UTC(),
	}
	if s.HeadersJSON != "" {
		if err := json.Unmarshal([]byte(s.HeadersJSON), &req.Headers); err != nil {
			return req, "invalid staged delivery headers"
		}
	}
	if s.ReceivedAt != "" {
		at, err := time.Parse(time.RFC3339Nano, s.ReceivedAt)
		if err != nil {
			return req, "invalid staged delivery timestamp"
		}
		req.ReceivedAt = at.UTC()
	}
	return req, ""
}

// connectorFor resolves the connector serving a source, or reports none.
func (d *Dispatcher) connectorFor(ctx context.Context, source string) (memqlsync.Connector, bool) {
	source = strings.TrimSpace(source)
	if source == "" {
		return nil, false
	}
	connector, ok := d.lookup(ctx, source)
	if !ok || connector == nil {
		return nil, false
	}
	return connector, true
}

// dispatchTo works a request its connector has already claimed. prior is how
// many times it was tried before this one.
func (d *Dispatcher) dispatchTo(ctx context.Context, connector memqlsync.Connector, req memqlsync.InboundRequest, prior int) (DispatchResult, error) {
	out := DispatchResult{Handled: true}
	source := strings.TrimSpace(req.Source)

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
		d.failed(connCtx, opCtx, connector, req, prior, err)
		return out, fmt.Errorf("datasync: %q applying inbound request %q: %w", source, req.RequestId, err)
	}
	if len(writes) == 0 {
		// A delivery this connector does not recognise. Stamped
		// `processed` rather than left alone: the row HAS been worked,
		// and leaving it `received` makes the staged queue grow without
		// bound and lose its meaning as a to-do list.
		d.stampProcessed(opCtx, req.RequestId, prior)
		return out, nil
	}

	res, applyErr := d.applier.Apply(connCtx, connector.Name(), specs, writes)
	out.Applied, out.Stale = res.Applied, res.Stale
	if applyErr != nil {
		d.failed(connCtx, opCtx, connector, req, prior, applyErr)
		return out, applyErr
	}
	d.stampProcessed(opCtx, req.RequestId, prior)

	d.recordInboundHealth(opCtx, connector, writes, req.ReceivedAt)
	return out, nil
}

// failed records a try that failed, and when the failure is worth another try,
// tells a connector with a short-circuit to forget the delivery
// (memqlsync.InboundForgetter): the connector may have recorded it as handled
// before the failure -- an applier failure comes after Apply returned -- and
// would answer the retry "already seen".
func (d *Dispatcher) failed(connCtx, opCtx context.Context, connector memqlsync.Connector, req memqlsync.InboundRequest, prior int, err error) {
	retry := worthAnotherTry(err)
	if forgetter, ok := connector.(memqlsync.InboundForgetter); ok && retry {
		forgetter.ForgetInbound(connCtx, req)
	}
	d.stampFailed(opCtx, req.RequestId, prior, err.Error(), retry)
}

// worthAnotherTry reports whether a failure may go differently on a later try.
// Everything is, except what the contract says will recur: a permanent
// refusal, a grant that does not cover the domain, a domain the connector was
// not asked for and a capability it does not have. A busy origin
// (memqlsync.IsBusy) is worth another try like any other transient failure.
// The refusals DispatchStaged makes before the connector is asked -- another
// tier's secret, no verified signature, metadata that does not parse -- are
// facts about the stored row, which no retry changes, and stamp no next try.
func worthAnotherTry(err error) bool {
	return !(memqlsync.IsPermanent(err) || memqlsync.IsNotGranted(err) ||
		memqlsync.IsNotRequested(err) || memqlsync.IsNotImplemented(err))
}

// stampProcessed records a try that worked: the try is counted and no next
// try is left on the row.
func (d *Dispatcher) stampProcessed(ctx context.Context, requestID string, prior int) {
	d.stamp(ctx, requestID, "processed", "",
		arg{"attempts", prior + 1}, arg{"nextAttemptAt", ""})
}

// stampFailed records a failed try: attempts is the try count, and
// nextAttemptAt is when the next try is due -- inboundRetryAfter after this
// one, while retry holds and fewer than maxInboundAttempts were made -- or ""
// for none. A next time is always named, because the retry pass's claim may
// have moved it forward and a stale one would make the row due again.
// The engine stores "" as written, which is why inboundRequestsDueForRetry
// requires a set nextAttemptAt as well as one at or before its asOf.
func (d *Dispatcher) stampFailed(ctx context.Context, requestID string, prior int, reason string, retry bool) {
	attempts := prior + 1
	next := ""
	if retry && attempts < maxInboundAttempts {
		next = d.now().UTC().Add(inboundRetryAfter[attempts-1]).Format(time.RFC3339)
	}
	d.stamp(ctx, requestID, "failed", reason, arg{"attempts", attempts}, arg{"nextAttemptAt", next})
}

// stamp records the request's handling outcome. Best effort: a failure
// to stamp must not turn a successful apply into a reported failure, and
// the delivery is durable either way.
//
// It names only the fields it means to write: updateInboundRequestStatus keeps
// every one it is not handed, so a stamp cannot reset what another writer of
// the row recorded.
func (d *Dispatcher) stamp(ctx context.Context, requestID, status, lastError string, fields ...arg) {
	if strings.TrimSpace(requestID) == "" {
		return
	}
	args := append([]arg{
		{"requestId", requestID},
		{"status", status},
		{"lastError", truncateError(lastError)},
		{"processedAt", d.now().UTC().Format(time.RFC3339)},
	}, fields...)
	_, _ = d.store.exec(ctx, call("mutation", "updateInboundRequestStatus", args...))
}

// recordInboundHealth updates lastInboundAt and lagSeconds for every
// domain this delivery touched -- the row of each write's own concept and
// partition, so a delivery for one store of a partitioned connector moves
// that store's row and no other.
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
	seen := map[[2]string]struct{}{}
	for _, w := range writes {
		conceptID := strings.TrimSpace(w.Concept)
		if conceptID == "" {
			continue
		}
		partition := strings.TrimSpace(w.Partition)
		key := [2]string{conceptID, partition}
		if _, done := seen[key]; done {
			continue
		}
		seen[key] = struct{}{}

		origin := receivedAt
		if t, ok := parseVersionTime(w.Version); ok {
			origin = t
		}
		lag := 0
		if !origin.IsZero() && now.After(origin) {
			lag = int(now.Sub(origin).Seconds())
		}

		// The dispatcher owns lastInboundAt and lagSeconds, and writes
		// nothing else: a backfill or a sweep of the same domain may be
		// writing its own fields at this moment.
		patch := SyncPatch{"lastInboundAt": now, "lagSeconds": lag}
		if err := d.store.PatchSyncState(ctx, conceptID, connector.Name(), string(memqlsync.DirectionInbound), partition,
			func(SyncState) (SyncPatch, bool) { return patch, true }); err != nil && d.logger != nil {
			d.logger.Warn("datasync: could not record a domain's inbound health",
				"connector", connector.Name(), "concept", conceptID, "partition", partition, "error", err)
		}
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
