package memql

// concept_registry_broadcast.go -- the in-process registry-change broadcaster
// (memql#4238).
//
// # The gap this closes
//
// A connected client reads the concept registry ONCE (ConceptsListMsg /
// ConceptsSubscribeMsg) and keeps a snapshot-per-dial: a concept trained,
// promoted, retired or removed into a RUNNING cluster does not appear until the
// client reconnects. This broadcaster is the engine-side half of the fix -- a
// subscribe/unsubscribe fan-out of registry DELTAS that the gRPC session rides
// to keep a client's registry live without a reconnect.
//
// # Why hooking the registry mutation is the whole cross-node story
//
// A promote/demote is already made visible to EVERY replica at runtime, not
// only at boot: a durable promote broadcasts authoring.promote.<bundleId>
// (authoring_promote_durable.go), a single routing rule forwards it to every
// node (component/node/routing.go), and each node's
// StartAuthoringPromoteSubscriber re-hydrates the bundle into ITS OWN shared
// registry (authoring_promote_propagate.go). Demote mirrors it (memql#2163).
//
// Both the LOCAL promote (the node that handled the gRPC call) and the
// CROSS-NODE observation (a peer's re-hydration subscriber) funnel through the
// SAME two shared-registry mutations:
//
//   - promoteConceptIntoLiveRegistry  (authoring_promote_concept.go)  -- add / re-promote
//   - removeConceptFromLiveRegistry   (authoring_concept_retire.go)   -- remove
//
// So emitting a delta from those two functions -- and nowhere else -- makes the
// notification fire on the path EACH replica runs when it learns of the change,
// with no new bus event and no new routing rule: the existing promote/demote
// broadcast already converges every replica's registry, and this broadcaster
// rides that convergence on each node. A client on replica B hears a promote on
// replica A because replica B's re-hydration subscriber re-registers the concept
// locally, which fires replica B's own broadcaster.
//
// A demote that RETIRES a concept (rows still exist -- registered, readable,
// writes closed) is deliberately NOT a delta: the concept stays in the registry
// set, so ConceptsListMsg still returns it and there is nothing for a
// set-shaped delta to say. Only add (promote / re-promote) and remove
// (zero-row demote) change the set.

import (
	"sync"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// conceptRegistryDeltaBuffer bounds each subscriber's delta channel. A slow
// consumer that fills it has a delta DROPPED rather than blocking the emitter;
// the dropped delta leaves a generation gap the consumer detects and recovers
// from by re-snapshotting. Mirrors the event bus's drop-if-full policy.
const conceptRegistryDeltaBuffer = 64

// ConceptRegistryDelta is one registry-change notification. Added carries whole
// concept descriptors (the client upserts by id, so a re-promote whose schema
// or displayCard changed rides Added too); Removed carries concept ids. Every
// delta stamps the monotonically increasing Generation the registry reached
// AFTER applying it -- a client that receives a Generation that is not exactly
// one past the last it saw has missed a delta and must re-snapshot.
type ConceptRegistryDelta struct {
	Generation uint64
	Added      []*memoryNodes.Concept
	Removed    []string
}

// ConceptRegistrySubscription is what SubscribeConceptRegistry hands back: the
// registry Snapshot and the Generation it corresponds to, both captured
// ATOMICALLY with the channel registration, plus the delta channel and an
// idempotent Unsubscribe. Because the snapshot and the channel are taken under
// one lock, no mutation can slip between them: a concurrent promote is reflected
// either in Snapshot or in the first delta (or, harmlessly, both -- the client
// reducer is idempotent).
type ConceptRegistrySubscription struct {
	Snapshot    []*memoryNodes.Concept
	Generation  uint64
	Deltas      <-chan ConceptRegistryDelta
	Unsubscribe func()
}

// conceptRegistryBroadcaster is the fan-out. It owns the generation counter and
// the set of subscriber channels.
type conceptRegistryBroadcaster struct {
	mu   sync.Mutex
	gen  uint64
	next uint64
	subs map[uint64]chan ConceptRegistryDelta
}

func newConceptRegistryBroadcaster() *conceptRegistryBroadcaster {
	return &conceptRegistryBroadcaster{subs: make(map[uint64]chan ConceptRegistryDelta)}
}

// emit stamps the next generation onto the delta and fans it out to every live
// subscriber. A full subscriber channel is skipped (see the buffer const): the
// generation gap is the recovery signal, not a blocked emitter.
//
// It holds only its own lock -- never a registry lock -- so the call sites
// (which have already released the registry lock by the time they emit) create
// no lock-order inversion with SubscribeConceptRegistry, which takes the
// registry read lock UNDER this one.
func (b *conceptRegistryBroadcaster) emit(delta ConceptRegistryDelta) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.gen++
	delta.Generation = b.gen
	for _, ch := range b.subs {
		select {
		case ch <- delta:
		default:
		}
	}
}

// subscribe registers a channel and returns it with the generation captured at
// registration time, plus an idempotent unsubscribe that removes and closes the
// channel. snapshot is invoked under the broadcaster lock so the returned
// generation and the concept list it pairs with are one atomic reading.
func (b *conceptRegistryBroadcaster) subscribe(snapshot func() []*memoryNodes.Concept) ConceptRegistrySubscription {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.next
	b.next++
	ch := make(chan ConceptRegistryDelta, conceptRegistryDeltaBuffer)
	b.subs[id] = ch
	var list []*memoryNodes.Concept
	if snapshot != nil {
		list = snapshot()
	}
	var once sync.Once
	unsub := func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			if c, ok := b.subs[id]; ok {
				delete(b.subs, id)
				close(c)
			}
		})
	}
	return ConceptRegistrySubscription{Snapshot: list, Generation: b.gen, Deltas: ch, Unsubscribe: unsub}
}

// conceptRegBroadcaster returns the engine's broadcaster, lazily allocating it
// so an engine built as &MemQLEngine{...} in a test (never through New) still
// broadcasts.
func (e *MemQLEngine) conceptRegBroadcaster() *conceptRegistryBroadcaster {
	e.conceptRegBroadcastOnce.Do(func() {
		if e.conceptRegBroadcast == nil {
			e.conceptRegBroadcast = newConceptRegistryBroadcaster()
		}
	})
	return e.conceptRegBroadcast
}

// SubscribeConceptRegistry atomically snapshots the current concept set +
// generation and registers a delta channel. The caller (the gRPC concepts
// handler) sends the snapshot at the returned generation, then forwards every
// delta from the channel until it closes Unsubscribe or the stream ends.
func (e *MemQLEngine) SubscribeConceptRegistry() ConceptRegistrySubscription {
	return e.conceptRegBroadcaster().subscribe(func() []*memoryNodes.Concept {
		if e.concepts == nil {
			return nil
		}
		return e.concepts.List()
	})
}

// broadcastConceptAdded emits an add/re-promote delta. Called from
// promoteConceptIntoLiveRegistry after the merge + derived-state rebuild commit.
func (e *MemQLEngine) broadcastConceptAdded(c *memoryNodes.Concept) {
	if e == nil || c == nil {
		return
	}
	e.conceptRegBroadcaster().emit(ConceptRegistryDelta{Added: []*memoryNodes.Concept{c}})
}

// broadcastConceptRemoved emits a remove delta. Called from
// removeConceptFromLiveRegistry after the concept actually leaves the registry.
func (e *MemQLEngine) broadcastConceptRemoved(conceptId string) {
	if e == nil || conceptId == "" {
		return
	}
	e.conceptRegBroadcaster().emit(ConceptRegistryDelta{Removed: []string{conceptId}})
}
