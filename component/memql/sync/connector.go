// Package sync is the connector contract: what a system that owns data
// MemQL mirrors, or that holds a mirror of data MemQL owns, has to
// implement (epic memql#4378, design section 5).
//
// The package is named for the directory the design names. Importers in
// component/memql alias it -- `memqlsync "github.com/znasllc-io/memql/component/memql/sync"` --
// because that tree uses the standard library's sync heavily; inside
// this package the two do not collide, since a package clause binds no
// identifier in its own file scope.
//
// # What a connector is, and is not
//
// A connector is an INTEGRATION that implements this interface. It is
// not a fourth extension word: component, integration and pack are
// still the three (docs/public/concepts/component-integration-pack.md).
// What makes an integration a connector is that a concept names it in
// @origin or @mirroredTo, and that this registry can find it under that
// name.
//
// # Why the registry has two halves
//
// A connector's IMPLEMENTATION cannot exist until the runtime can build
// it -- Shopify's needs an API client, which needs configuration. But
// the engine's boot check has to refuse a concept naming a connector
// nobody serves BEFORE that, because the check runs inside
// MemQLEngine.Init and the bootstrap order is config -> database ->
// engine -> integrations (app/build_*.go). Init happens first, so a
// registry holding only live instances is empty exactly when the check
// reads it, and the check would refuse every declaration in the tree.
//
// So the registry records two different facts:
//
//   - Declare(name), called from an init() -- "this BUILD knows how to
//     serve a connector by this name". Available before Init, which is
//     what makes the boot check possible at all.
//   - Bind(c), called once the runtime can construct the connector --
//     "here is the implementation". Refuses a name nothing declared, so
//     the two halves cannot drift into naming different sets.
//
// Splitting them is not a workaround for the bootstrap order; it is an
// honest statement that "this build can serve shopify" and "shopify is
// configured and running here" are different claims. A cluster with no
// Shopify credentials still loads the v1:shopify:* concepts -- it just
// has no connector bound to fill it, which is an operational condition
// the Data origins page reports rather than a boot refusal.
package sync

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Connector is what a system on the other side of a mirror implements.
// One connector serves one external system and may cover several
// concepts -- Shopify's covers products today and the rest of the store
// later -- which is why every method names the concept it acts on.
//
// Every method runs under the connector's own actor
// (auth.ConnectorActor(Name())), which is admitted to exactly the
// concepts that name this connector and to nothing else. A connector
// never sees a caller's identity and never acts as one.
type Connector interface {
	// Name is the origin / target name concepts write in @origin and
	// @mirroredTo. It must equal the name Declare recorded.
	Name() string

	// Domains lists the concepts this connector originates or mirrors,
	// with the field each one's version guard reads.
	Domains() []DomainSpec

	// Apply turns one staged inbound delivery into version-stamped
	// mirror writes. Returning zero writes is a normal outcome: a
	// delivery this connector does not recognise is skipped, not
	// failed.
	Apply(ctx context.Context, req InboundRequest) ([]MirrorWrite, error)

	// Backfill returns one resumable page of the origin's current
	// state for a concept. The cursor is opaque to the runtime, which
	// persists it on syncState so a restart resumes rather than
	// restarts.
	Backfill(ctx context.Context, conceptName string, cursor string) (BackfillPage, error)

	// Reconcile compares the origin against the mirror for everything
	// changed since a point in time, heals what drifted through the
	// same version-guarded apply path, and counts the drift.
	Reconcile(ctx context.Context, conceptName string, since time.Time) (ReconcileReport, error)

	// Propagate pushes ONE MemQL change out to this connector's
	// external mirror. Called by the outbox drain worker, which owns
	// ordering, retry and dead-lettering -- an implementation delivers
	// once and reports what happened.
	Propagate(ctx context.Context, entry OutboxEntry) (PropagateResult, error)

	// EnsureSubscriptions registers or verifies whatever the origin
	// needs in order to deliver changes to MemQL -- webhooks, a polling
	// registration, a subscription id. Idempotent by contract: the
	// runtime calls it at boot and after a configuration change.
	EnsureSubscriptions(ctx context.Context) error
}

// DomainSpec describes one concept a connector handles.
type DomainSpec struct {
	// Concept is the canonical concept id, e.g. "v1:shopify:product".
	Concept string
	// VersionField names the payload field carrying the ORIGIN's
	// version of the row -- an updated_at, a sequence, or the delivery
	// timestamp when the origin offers nothing better (D6). Empty means
	// the connector stamps MirrorWrite.Version itself and the runtime
	// compares against the stored row's recorded version.
	VersionField string
	// ReconcileInterval is how often the reconciliation runner should
	// sweep this domain. Zero means "do not schedule" -- reconciliation
	// is then operator-driven only.
	ReconcileInterval time.Duration
	// Direction records whether MemQL mirrors this concept inbound,
	// originates it and pushes outbound, or both.
	Direction Direction
}

// Direction is which way data moves for one domain.
type Direction string

const (
	// DirectionInbound -- the origin is external; MemQL receives.
	DirectionInbound Direction = "inbound"
	// DirectionOutbound -- MemQL is the origin; the connector pushes.
	DirectionOutbound Direction = "outbound"
)

// InboundRequest is one staged delivery handed to Apply. It mirrors the
// v1:platform:inboundRequest row the HTTP edge writes (memql#2957)
// rather than the HTTP request itself: a connector never sees a socket,
// and the signature was verified before the row was written.
type InboundRequest struct {
	// RequestId is the inboundRequest row id, carried so Apply can
	// reference the staged row in an audit line.
	RequestId string
	// Source is the /inbound/{source} name the delivery arrived under.
	Source string
	// Topic is the origin's event name when it sends one (Shopify's
	// X-Shopify-Topic, a webhook "type" field). Empty is normal.
	Topic string
	// Body is the raw delivery body, exactly as staged.
	Body []byte
	// Headers carries the delivery's headers, lowercased.
	Headers map[string]string
	// ReceivedAt is when the delivery was staged -- the fallback
	// version for an origin that stamps nothing of its own (D6).
	ReceivedAt time.Time
}

// MirrorWrite is one row Apply wants written into a mirror. The runtime
// performs the write, under the connector's actor and behind the
// version guard; a connector never writes rows itself.
type MirrorWrite struct {
	// Concept is the canonical concept id being written.
	Concept string
	// RowId is the row's id -- for a mirror this is normally the
	// origin's own identifier, so the two systems agree on what a row
	// is without a mapping table.
	RowId string
	// Payload is the row's fields.
	Payload map[string]any
	// Version is the ORIGIN's version of this row. The runtime refuses
	// to apply a write whose version is older than the stored row's and
	// records it as stale (D6), so an out-of-order webhook cannot
	// regress a mirror.
	Version string
	// Retire marks the row gone at the origin. A mirror retires a row
	// it already had; it never invents one, which is why this is a flag
	// on a write rather than a separate delete verb.
	Retire bool
}

// BackfillPage is one resumable page of a connector's current state.
type BackfillPage struct {
	// Writes are the rows in this page, in the same shape Apply
	// returns, so backfill and inbound converge on one write path.
	Writes []MirrorWrite
	// NextCursor is where the next call resumes. Empty means done.
	NextCursor string
	// Done reports completion explicitly, so a connector whose last
	// page happens to be empty is not mistaken for one that stalled.
	Done bool
}

// ReconcileReport is what one reconciliation sweep found.
type ReconcileReport struct {
	// Checked is how many rows the sweep compared.
	Checked int
	// Drifted is how many disagreed with the origin.
	Drifted int
	// Healed is how many of those the sweep corrected.
	Healed int
}

// OutboxEntry is one pending outbound delivery handed to Propagate.
// The runtime owns the durable row (v1:platform:outboxEntry); this is
// the value a connector sees.
type OutboxEntry struct {
	// Id is the outbox row id.
	Id string
	// Concept and RowId name the MemQL row that changed.
	Concept string
	RowId   string
	// Action is what happened to it.
	Action OutboxAction
	// Version is the row's version after the change -- half of the
	// idempotency key, and what lets a receiver discard a replay.
	Version string
	// Target is the connector name this entry is destined for. Present
	// even though the drain worker is per-connector, because a
	// connector implementation may serve several targets in principle
	// and an entry that cannot say where it is going is unauditable.
	Target string
	// Payload is the row's current payload.
	Payload map[string]any
	// IdempotencyKey is (concept, rowId, version, target), rendered
	// once by the runtime so every attempt at this entry carries the
	// same key.
	IdempotencyKey string
	// Attempts is how many delivery attempts have already been made.
	Attempts int
}

// OutboxAction is what happened to the MemQL row.
type OutboxAction string

const (
	// OutboxUpsert -- the row was created or changed.
	OutboxUpsert OutboxAction = "upsert"
	// OutboxRetire -- the row was retired. MemQL has no hard delete;
	// a retirement is an append that marks the row gone.
	OutboxRetire OutboxAction = "retire"
)

// PropagateResult is what one delivery attempt achieved.
type PropagateResult struct {
	// ExternalId is the origin's identifier for the delivered object,
	// when it returns one. Recorded for audit.
	ExternalId string
	// AlreadyDelivered reports that the receiver recognised the
	// idempotency key and had this change already. It is a SUCCESS --
	// the whole point of the key -- and is distinguished from a fresh
	// delivery only so the audit line can say which happened.
	AlreadyDelivered bool
	// RetryAfter asks the drain worker to park this entry for at least
	// this long, honouring a rate limit the receiver stated. Zero means
	// the worker's own backoff applies.
	RetryAfter time.Duration
}

// ErrNotImplemented is what a connector returns for a contract method
// it does not serve yet.
//
// It is a TYPED error rather than a nil return or a generic message
// because the runtime has to tell "this connector cannot do that" from
// "this delivery failed": the first is a configuration fact to report
// on the Data origins page, the second is a retry. A connector serving
// one direction of one domain -- which is where Shopify starts -- would
// otherwise dead-letter every outbound entry it was ever handed.
var ErrNotImplemented = errors.New("connector: not implemented")

// NotImplemented wraps ErrNotImplemented with what was asked for, so an
// operator reading a log line learns which capability is missing rather
// than only that one is.
func NotImplemented(connector, capability string) error {
	return fmt.Errorf("%w: connector %q does not implement %s", ErrNotImplemented, connector, capability)
}

// IsNotImplemented reports whether err is a connector's "I do not serve
// this" answer.
func IsNotImplemented(err error) bool { return errors.Is(err, ErrNotImplemented) }

// registry holds both halves: the names this build can serve, and the
// implementations bound so far.
var registry = struct {
	mu       sync.RWMutex
	declared map[string]struct{}
	bound    map[string]Connector
}{
	declared: make(map[string]struct{}),
	bound:    make(map[string]Connector),
}

// Declare records that this build knows how to serve a connector by
// this name. Called from an init(), so the engine's boot check can
// resolve @origin and @mirroredTo names before any integration has been
// constructed.
//
// Declaring twice is a no-op rather than a panic: a name may legitimately
// be declared by more than one file in a build (a connector and its
// test fixture), and a duplicate declaration asserts nothing different.
// Binding twice is the case that is refused, because two implementations
// under one name is an ambiguity.
func Declare(name string) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.declared[name] = struct{}{}
}

// Bind attaches a live implementation. It refuses a name nothing
// declared, which is what keeps the two halves of the registry honest:
// a connector reachable at runtime but invisible to the boot check
// would let a concept name it and still refuse boot, and the operator
// would be told the connector does not exist while it is running.
func Bind(c Connector) error {
	if c == nil {
		return fmt.Errorf("connector: Bind(nil)")
	}
	name := strings.TrimSpace(c.Name())
	if name == "" {
		return fmt.Errorf("connector: Bind: the implementation reports an empty Name()")
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if _, ok := registry.declared[name]; !ok {
		return fmt.Errorf(
			"connector: %q was never declared -- call sync.Declare(%q) from an init() in the package that binds it, so the engine's boot check can resolve @origin(%q) before integrations are wired",
			name, name, name)
	}
	if _, ok := registry.bound[name]; ok {
		return fmt.Errorf("connector: %q is already bound -- one implementation per name", name)
	}
	registry.bound[name] = c
	return nil
}

// Lookup returns the bound implementation for a name.
//
// A declared-but-unbound connector returns (nil, false), which is the
// same answer as an unknown name AND IS DELIBERATE at this seam: a
// caller here wants to DO something with a connector, and "declared but
// not configured" is as unable to do it as "never heard of it". The two
// are distinguished where the difference matters -- IsDeclared for the
// boot check, and the Data origins page's health read.
func Lookup(name string) (Connector, bool) {
	name = strings.TrimSpace(name)
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	c, ok := registry.bound[name]
	return c, ok
}

// IsDeclared reports whether this build knows how to serve a connector
// by this name. THIS is what the engine's boot check reads -- not
// Lookup -- because a cluster that has not configured Shopify still
// loads Shopify's concepts.
func IsDeclared(name string) bool {
	name = strings.TrimSpace(name)
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	_, ok := registry.declared[name]
	return ok
}

// DeclaredNames returns every declared connector name, sorted. Used by
// the boot refusal to tell an operator what this build does serve.
func DeclaredNames() []string {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	return sortedKeysOf(registry.declared)
}

// BoundNames returns every connector with a live implementation,
// sorted.
func BoundNames() []string {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	out := make([]string, 0, len(registry.bound))
	for k := range registry.bound {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedKeysOf(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// resetForTest clears both halves. Test-only; the registry is process
// global because init() is where declaration happens.
func resetForTest() {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.declared = make(map[string]struct{})
	registry.bound = make(map[string]Connector)
}
