package node

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/events"
)

// DeliverySubstrate is the durable event-delivery backbone for the mesh
// (memql#1263, Phase 1 of epic memql#1259). It implements the "outbox +
// cursor" contract decided in the spike ADR
// (docs/internal/design/mesh-delivery-substrate-adr.md, memql#1262).
//
// The contract, restated:
//
//   - Logical addressing (ADR 4.1). Every deliverable is addressed by a
//     RoutingKey -- a typed "<kind>:<id>" string (space:/session:/user:) --
//     never a physical node-id. "Deliver to whoever owns space X" resolves to
//     space:X; the substrate does not know or care which BFF replica holds
//     that key's consumer.
//   - At-least-once + idempotent dedup (ADR 4.2). A published deliverable is
//     delivered to every active subscriber of its key at least once, across
//     the durable pull and/or the mesh fast-path, surviving consumer restart
//     and mesh churn. Each deliverable carries a stable EventID; consumers
//     dedup by EventID using the existing time-windowed eventDedup
//     (component/node/dedup.go) so the two paths can never double-deliver.
//   - Per-key ordering (ADR 4.3). Deliverables for a single key are delivered
//     in produce order via a per-key monotonic Seq; the cursor advances
//     monotonically. No cross-key ordering is promised (fan-out parallelism).
//   - Replay / catch-up on (re)connect (ADR 4.4, amended by memql#1328). On
//     every (re)connect a consumer resumes from its cursor (the highest acked
//     Seq for the key) and replays "seq > cursor" in ascending order before
//     tailing live. A BRAND-NEW consumer (no cursor row) starts at the key's
//     current HIGH WATERMARK by default: its cursor is atomically initialized
//     and persisted at the key's highest allocated seq, so it receives only
//     deliverables produced after it first subscribed. Backlog replay is
//     OPT-IN per subscription (WithReplayBacklog) for consumers that
//     genuinely need first-subscribe history; retention (memql#1328) bounds
//     the replayable window either way.
//   - Mesh fast-path coexistence (ADR 4.5). The mesh push (EventBridge) MAY
//     deliver the same deliverable ahead of the durable pull for latency; it
//     carries the same EventID and is deduped against the durable path. The
//     mesh is best-effort and never the source of truth: the CURSOR ADVANCES
//     ON THE DURABLE PATH ONLY, so a dropped durable row can never be skipped
//     by a mesh copy on replay.
//
// This is a new, available capability landing ALONGSIDE the current ad-hoc
// mesh forwards; nothing is cut over yet. Migrating the chat-reply path
// (memql#1264), dispatch/client-tool RPC (memql#1265), and token/audio
// streaming (memql#1266) onto it, and retiring the superseded forwards
// (memql#1267), are downstream issues.
type DeliverySubstrate interface {
	// Publish appends a deliverable to the durable outbox for its routing key
	// and returns the assigned per-key seq, then best-effort emits it on the
	// mesh fast-path. EventID must be stable and unique; a duplicate EventID is
	// an idempotent no-op (ADR 4.2) and returns the already-assigned seq.
	Publish(ctx context.Context, d Deliverable) (seq int64, err error)

	// Subscribe returns an ordered, replayed, deduped stream for one logical
	// key. An EXISTING consumer (one with a cursor row) replays from its
	// cursor (ADR 4.4) before tailing live. A BRAND-NEW consumer starts at
	// the key's current high watermark -- only deliverables produced after
	// Subscribe returns are guaranteed delivered -- unless WithReplayBacklog
	// opts it into replaying the retained backlog from seq 0 (memql#1328).
	// The returned channel is closed when ctx is cancelled. Each delivered
	// Deliverable carries its assigned Seq; the consumer Acks to advance its
	// cursor.
	Subscribe(ctx context.Context, key RoutingKey, consumerID string, opts ...SubscribeOption) (<-chan Deliverable, error)

	// Ack advances the persisted cursor for (key, consumerID) to seq (ADR 4.4).
	// The cursor never moves backwards.
	Ack(ctx context.Context, key RoutingKey, consumerID string, seq int64) error
}

// SubscribeOption customizes one Subscribe call.
type SubscribeOption func(*subscribeOptions)

type subscribeOptions struct {
	replayBacklog bool
}

// WithReplayBacklog opts this subscription into first-subscribe backlog
// replay (memql#1328): a BRAND-NEW consumer (no cursor row) starts from seq 0
// and replays the retained backlog instead of initializing its cursor at the
// key's current high watermark. Reserve it for subscriptions that genuinely
// need first-subscribe history -- per-request stream/RPC keys, where the
// "backlog" is the head of the very exchange being consumed and the producer
// legitimately starts before the consumer subscribes. Long-lived keys with
// per-pod consumer ids (the chat-reply space subscriptions) must NOT use it:
// every rollout mints brand-new consumers, and replaying a full retention
// window of history to each fresh pod is exactly the failure mode the
// high-watermark default removes. An EXISTING cursor row always wins over
// either mode (the consumer resumes exactly).
func WithReplayBacklog() SubscribeOption {
	return func(o *subscribeOptions) { o.replayBacklog = true }
}

// RoutingKey is a logical delivery address (ADR 4.1): a typed "<kind>:<id>"
// string. It is NEVER a physical node-id. The substrate routes by this key
// alone, so a producer never has to find the physical replica that owns a key.
type RoutingKey struct {
	Kind string // "space" | "session" | "user"
	ID   string
}

// String renders the routing key as the canonical "<kind>:<id>" wire form used
// as the outbox/cursor column value.
func (k RoutingKey) String() string {
	return k.Kind + ":" + k.ID
}

// Valid reports whether the routing key is non-empty in both components. The
// substrate rejects an empty key at Publish/Subscribe time -- an unaddressed
// deliverable has no logical owner.
func (k RoutingKey) Valid() bool {
	return k.Kind != "" && k.ID != ""
}

// Deliverable is one durable, logically-addressed event. EventID is the dedup
// key carried byte-identical on the durable row AND the mesh fast-path copy.
// Seq is assigned by Publish (the per-key ordering position); callers leave it
// zero on the way in.
type Deliverable struct {
	EventID    string         // dedup key, carried on BOTH paths (ADR 4.2)
	Key        RoutingKey     // logical addressing (ADR 4.1)
	Seq        int64          // per-key ordering (ADR 4.3); set by Publish
	Topic      string         // event topic, e.g. graph.node.created....
	Kind       events.Kind    // events.Kind
	Payload    map[string]any // event payload
	OriginNode string         // producer node id; audit only, NOT for addressing
}

// outboxStore is the durable persistence seam behind the substrate. The
// production implementation (delivery_store_pg.go) is bun/Postgres-backed; the
// substrate's ordering/replay/dedup logic is unit-tested against an in-memory
// fake of this interface. Implementations MUST be safe for concurrent use.
type outboxStore interface {
	// Append idempotently inserts a deliverable, allocating the next per-key
	// seq. If a row with the same EventID already exists it is a no-op and the
	// existing seq is returned with duplicate=true. The returned seq is the
	// deliverable's authoritative ordering position either way.
	Append(ctx context.Context, d Deliverable) (seq int64, duplicate bool, err error)

	// ReadAfter returns deliverables for key with seq strictly greater than
	// afterSeq, in ascending seq order, up to limit rows (limit<=0 means no
	// bound). This is the catch-up/replay read (ADR 4.4).
	ReadAfter(ctx context.Context, key RoutingKey, afterSeq int64, limit int) ([]Deliverable, error)

	// EnsureCursor resolves the starting cursor for (key, consumerID)
	// (ADR 4.4, amended by memql#1328). An existing cursor row is returned
	// as-is (the consumer resumes exactly). When no row exists and fromStart
	// is true, it returns 0 WITHOUT creating a row -- the opt-in backlog
	// replay; the row appears on the first Ack, as before. When no row exists
	// and fromStart is false, it atomically initializes AND persists the
	// cursor at the key's current high watermark (the highest committed seq
	// the allocator has handed out) and returns it with initialized=true.
	// The init MUST NOT race a concurrent producer into losing events: any
	// append that commits after the watermark snapshot has seq > watermark
	// and is picked up by ReadAfter; appends committed before it are skipped
	// by design.
	EnsureCursor(ctx context.Context, key RoutingKey, consumerID string, fromStart bool) (seq int64, initialized bool, err error)

	// SaveCursor advances the persisted cursor for (key, consumerID) to seq.
	// It MUST be monotonic: a lower seq than the stored value is ignored.
	SaveCursor(ctx context.Context, key RoutingKey, consumerID string, seq int64) error
}

// meshFastPath is the best-effort low-latency hint surface (ADR 4.5). The mesh
// EventBridge satisfies it. It is intentionally narrow and fire-and-forget:
// the substrate's correctness never depends on it arriving.
type meshFastPath interface {
	// PublishHint emits the deliverable on the mesh as a latency optimization.
	// It carries the same EventID as the durable row so a receiver dedups the
	// two against each other. It MUST NOT block the durable path and MUST NOT
	// be treated as a delivery guarantee.
	PublishHint(d Deliverable)
}

const (
	// substratePollInterval is how often a live subscriber polls the outbox for
	// new rows when no fast-path wake-up arrives. Correctness never depends on
	// the wake-up (ADR 2c): the poll is the floor. Kept short enough that the
	// durable-only path (mesh down) stays responsive without hammering the DB.
	substratePollInterval = 250 * time.Millisecond

	// substrateReplayBatch bounds how many rows a single catch-up read pulls,
	// so a consumer rejoining against a large backlog streams it in chunks
	// rather than buffering it all at once.
	substrateReplayBatch = 256

	// substrateChannelBuffer is the per-subscription delivery channel depth.
	substrateChannelBuffer = 64
)


// Substrate is the concrete DeliverySubstrate. It owns the durable store, the
// optional mesh fast-path hint surface, and a registry of live subscriptions.
//
// Dedup scope (ADR 4.2): the substrate reuses eventDedup -- the SAME primitive
// the mesh uses -- but the dedup WINDOW IS PER-SUBSCRIPTION. A given consumer
// applies whichever copy of an EventID reaches it first (the mesh fast-path
// hint or its own durable pull) and drops the other; a DIFFERENT consumer
// replaying the same key (e.g. a BFF replica taking over) gets its own clean
// window and re-receives the rows after its cursor. A process-wide dedup would
// wrongly suppress a legitimate replay to a new owner -- the very catch-up the
// design exists to guarantee -- so dedup is scoped to the subscription, exactly
// as the ADR frames it ("consumers dedup by event-id before acting").
//
// The cursor advances on the DURABLE path only (ADR 4.5 rule 4): a mesh hint
// short-circuits latency and feeds the per-sub dedup window, but the local
// replay position only moves as durable rows are read, so a dropped durable row
// can never be skipped by a mesh copy on replay.
type Substrate struct {
	store    outboxStore
	dedupTTL time.Duration
	fastPath meshFastPath // optional; nil when the mesh is not wired
	logger   *slog.Logger
	now      func() time.Time

	mu   sync.Mutex
	subs map[string][]*subscription // routing-key -> live subscriptions
}

// subscription is one live Subscribe call's state: its own dedup window (so its
// mesh-hint copy and durable-pull copy collapse to one delivery without
// affecting other consumers), a wake nudge channel, and a mesh-hint inbox that
// carries fast-path deliverables straight to the consumer for latency.
type subscription struct {
	key        RoutingKey
	consumerID string
	out        chan<- Deliverable
	dedup      *eventDedup    // per-subscription cross-path dedup (ADR 4.2)
	wake       chan struct{}  // "pull the durable outbox now" nudge
	hints      chan Deliverable // mesh fast-path deliverables (latency short-circuit)
	startPos   int64            // starting durable position, resolved by Subscribe via EnsureCursor
}

// NewSubstrate builds a delivery substrate over the given durable store.
// dedupTTL sets each subscription's cross-path dedup window (mirror the mesh's
// eventDedup TTL); fastPath is the optional mesh hint surface (nil =
// durable-only, which still delivers -- mesh-down is a supported mode per ADR
// 4.5 rule 3).
func NewSubstrate(store outboxStore, dedupTTL time.Duration, fastPath meshFastPath, logger *slog.Logger) *Substrate {
	return &Substrate{
		store:    store,
		dedupTTL: dedupTTL,
		fastPath: fastPath,
		logger:   logger,
		now:      time.Now,
		subs:     make(map[string][]*subscription),
	}
}

// Publish appends to the durable outbox (the guarantee) and then best-effort
// emits the mesh fast-path hint (the latency optimization). See ADR 4.1-4.3.
func (s *Substrate) Publish(ctx context.Context, d Deliverable) (int64, error) {
	if !d.Key.Valid() {
		return 0, fmt.Errorf("delivery substrate: invalid routing key %q", d.Key.String())
	}
	if d.EventID == "" {
		return 0, fmt.Errorf("delivery substrate: deliverable for %s missing EventID", d.Key.String())
	}

	seq, duplicate, err := s.store.Append(ctx, d)
	if err != nil {
		return 0, fmt.Errorf("delivery substrate: append to outbox for %s: %w", d.Key.String(), err)
	}
	d.Seq = seq

	// A duplicate produce (same EventID) is an idempotent no-op (ADR 4.2): the
	// row already exists, so we neither re-emit the fast-path nor re-wake. We
	// still return the authoritative seq.
	if duplicate {
		return seq, nil
	}

	// Best-effort fast-path. Fire-and-forget, carries the same EventID+seq;
	// correctness does not depend on it (ADR 4.5 rule 3). The mesh delivers it
	// to the remote node, which calls HandleFastPath on its own substrate.
	if s.fastPath != nil {
		s.fastPath.PublishHint(d)
	}

	// Nudge live LOCAL subscribers of this key to pull immediately rather than
	// waiting for the next poll tick. Best-effort (non-blocking).
	s.wakeKey(d.Key)

	return seq, nil
}

// Subscribe returns an ordered, replayed, deduped stream for one logical key
// (ADR 4.4, amended by memql#1328). An existing consumer replays from its
// cursor; a brand-new consumer starts at the key's current high watermark
// unless WithReplayBacklog is passed. It then tails live, waking on a Publish
// to this key, a mesh fast-path hint, or the poll interval. The channel closes
// on ctx cancellation.
//
// The starting position is resolved SYNCHRONOUSLY, before Subscribe returns,
// so the high-watermark init can never race a producer into losing events:
// every deliverable produced after Subscribe returns has seq above the pinned
// watermark and is delivered; deliverables produced before it MAY be skipped
// (that is the point of the default). Callers that publish a row and then
// expect it on a subscription they opened FIRST keep that guarantee verbatim.
func (s *Substrate) Subscribe(ctx context.Context, key RoutingKey, consumerID string, opts ...SubscribeOption) (<-chan Deliverable, error) {
	if !key.Valid() {
		return nil, fmt.Errorf("delivery substrate: invalid routing key %q", key.String())
	}
	if consumerID == "" {
		return nil, fmt.Errorf("delivery substrate: subscribe to %s missing consumerID", key.String())
	}
	var o subscribeOptions
	for _, opt := range opts {
		opt(&o)
	}

	pos, initialized, err := s.store.EnsureCursor(ctx, key, consumerID, o.replayBacklog)
	if err != nil {
		return nil, fmt.Errorf("delivery substrate: ensure cursor for %s/%s: %w", key.String(), consumerID, err)
	}
	if initialized {
		// Structured marker for staging verification (memql#1328): a brand-new
		// consumer was pinned at the key's tip instead of replaying backlog.
		s.info("delivery substrate: brand-new consumer; cursor initialized at high watermark",
			"key", key.String(), "consumer", consumerID, "watermark", pos)
	}

	out := make(chan Deliverable, substrateChannelBuffer)
	sub := &subscription{
		key:        key,
		consumerID: consumerID,
		out:        out,
		dedup:      newEventDedup(s.dedupTTL),
		wake:       make(chan struct{}, 1),
		hints:      make(chan Deliverable, substrateChannelBuffer),
		startPos:   pos,
	}
	s.register(sub)

	go s.tail(ctx, sub)
	return out, nil
}

// Ack advances the persisted cursor for (key, consumerID) (ADR 4.4). The store
// enforces monotonicity, so an out-of-order or stale Ack is harmless.
func (s *Substrate) Ack(ctx context.Context, key RoutingKey, consumerID string, seq int64) error {
	if !key.Valid() {
		return fmt.Errorf("delivery substrate: invalid routing key %q", key.String())
	}
	if err := s.store.SaveCursor(ctx, key, consumerID, seq); err != nil {
		return fmt.Errorf("delivery substrate: save cursor for %s/%s: %w", key.String(), consumerID, err)
	}
	return nil
}

// HandleFastPath is the mesh-side entry point for a fast-path copy (ADR 4.5).
// The mesh EventBridge calls this when it receives an EventForward that carries
// a substrate deliverable. The hint is routed to every live local subscription
// of the key: each delivers it to its consumer for LATENCY and records the
// EventID in its own dedup window so its later durable pull of the same row is
// dropped (first path wins). The hint NEVER advances a cursor -- only the
// durable read does (rule 4) -- so a mesh copy can short-circuit latency but
// never correctness. It is fire-and-forget: a hint for a key with no live
// local subscriber is simply discarded (the durable pull still delivers).
func (s *Substrate) HandleFastPath(d Deliverable) {
	if !d.Key.Valid() {
		return
	}
	s.mu.Lock()
	subs := append([]*subscription(nil), s.subs[d.Key.String()]...)
	s.mu.Unlock()
	for _, sub := range subs {
		select {
		case sub.hints <- d:
		default:
			// Hint inbox full: the consumer is behind; drop the hint. The
			// durable pull is the guarantee and will deliver this row.
		}
	}
}

// tail drives one subscription: an initial catch-up replay from the cursor,
// then a live tail that delivers mesh hints (latency) and re-reads the durable
// outbox on wake or poll, deduping the two against each other.
func (s *Substrate) tail(ctx context.Context, sub *subscription) {
	defer close(sub.out)
	defer s.unregister(sub)

	// pos is the local "delivered so far this connection" marker. It starts at
	// the position Subscribe resolved synchronously via EnsureCursor (the
	// persisted cursor, or the freshly-pinned high watermark, or 0 under
	// WithReplayBacklog -- ADR 4.4 / memql#1328) and advances only as DURABLE
	// rows are read -- never on a mesh hint -- so a dropped durable row is
	// never skipped.
	pos := sub.startPos

	ticker := time.NewTicker(substratePollInterval)
	defer ticker.Stop()

	for {
		newPos, derr := s.drainFrom(ctx, sub, pos)
		if derr != nil {
			if ctx.Err() != nil {
				return
			}
			s.warn("delivery substrate: read failed; will retry",
				"key", sub.key.String(), "consumer", sub.consumerID, "error", derr)
		}
		pos = newPos

		select {
		case <-ctx.Done():
			return
		case d := <-sub.hints:
			// Mesh fast-path delivery (latency short-circuit, ADR 4.5). Deliver
			// it now and record it so the durable pull of the same EventID is
			// deduped. The cursor still only advances on the durable read.
			if sub.dedup.Check(d.EventID) {
				continue // already delivered via the durable pull; drop
			}
			select {
			case <-ctx.Done():
				return
			case sub.out <- d:
			}
		case <-sub.wake:
			// A local Publish to this key nudged us; loop and pull the durable row.
		case <-ticker.C:
			// Poll floor: correctness never depends on the wake (ADR 2c).
		}
	}
}

// drainFrom reads and delivers all durable rows after pos in ascending seq
// order, deduping each by EventID against the subscription's window (a row
// already delivered via a mesh hint is dropped here). Returns the new local
// position (the highest seq read, whether or not it was delivered, so a
// deduped row still advances past itself).
func (s *Substrate) drainFrom(ctx context.Context, sub *subscription, pos int64) (int64, error) {
	for {
		rows, err := s.store.ReadAfter(ctx, sub.key, pos, substrateReplayBatch)
		if err != nil {
			return pos, err
		}
		if len(rows) == 0 {
			return pos, nil
		}
		for i := range rows {
			d := rows[i]
			if d.Seq > pos {
				pos = d.Seq
			}
			// Cross-path dedup (ADR 4.2): if the mesh hint for this EventID
			// already delivered it on THIS subscription, drop the durable copy.
			if sub.dedup.Check(d.EventID) {
				continue
			}
			select {
			case <-ctx.Done():
				return pos, ctx.Err()
			case sub.out <- d:
			}
		}
		if len(rows) < substrateReplayBatch {
			return pos, nil
		}
		// A full batch: there may be more; loop to keep draining in order.
	}
}

// --- subscription registry + wake fan-out ----------------------------------

func (s *Substrate) register(sub *subscription) {
	s.mu.Lock()
	k := sub.key.String()
	s.subs[k] = append(s.subs[k], sub)
	s.mu.Unlock()
}

func (s *Substrate) unregister(sub *subscription) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := sub.key.String()
	subs := s.subs[k]
	for i, c := range subs {
		if c == sub {
			s.subs[k] = append(subs[:i], subs[i+1:]...)
			break
		}
	}
	if len(s.subs[k]) == 0 {
		delete(s.subs, k)
	}
}

// wakeKey nudges every live local subscriber of key to pull now. Non-blocking:
// a subscriber whose wake channel is already full is mid-pull and will pick up
// the new rows on its current pass, so a dropped nudge is harmless.
func (s *Substrate) wakeKey(key RoutingKey) {
	s.mu.Lock()
	subs := append([]*subscription(nil), s.subs[key.String()]...)
	s.mu.Unlock()
	for _, sub := range subs {
		select {
		case sub.wake <- struct{}{}:
		default:
		}
	}
}

func (s *Substrate) warn(msg string, args ...any) {
	if s.logger != nil {
		s.logger.Warn(msg, args...)
	}
}

func (s *Substrate) info(msg string, args ...any) {
	if s.logger != nil {
		s.logger.Info(msg, args...)
	}
}

// Ensure Substrate satisfies the public interface.
var _ DeliverySubstrate = (*Substrate)(nil)
