package emailrules

// firing_gate.go -- one rule's firings are recorded one at a time, on every
// replica at once (memql#5431).
//
// # What was lost
//
// A firing's count was written read-then-plus-one: the fire path read the rule,
// sent, and wrote back what it had read plus one. Two firings that overlapped
// both read N and both wrote N+1, so the rule's "Times fired" fell behind the
// firings that happened (measured: 9 to 12 after 64 firings from two replicas,
// fired_count_db_test.go). The sends were never the problem -- only the count.
//
// # Why a lock, and why this one
//
// A DSL update is a read-merge-write onto an append-only row: two concurrent
// updates produce two versions and the later one wins, with neither writer able
// to tell it lost. There is no predicate to hang a compare-and-swap on, and the
// count cannot be derived from anything else -- a created rule has its per-row
// claims, but an "updated" rule fires on every change and leaves nothing behind
// to count. So the read and the write happen while ONE firing holds a Postgres
// advisory lock keyed on the rule, the identity gate's magic-link pattern
// (component/identity/magic_link_gate.go). An in-process mutex would not do:
// the authored owner gate runs an author's rules on one node only while every
// node agrees on the membership, and two nodes that disagree during a rollout or
// a scale both run them.
//
// The lock is TRANSACTION-scoped (pg_advisory_xact_lock under SET LOCAL
// lock_timeout, on a connection held for it alone): the server bounds the wait,
// and ROLLBACK -- or the connection being discarded -- releases the lock
// whatever the client does. The transaction writes nothing; it exists to hold
// the lock while the record runs.
//
// # What the lock needs from the record
//
// Two things, and each one alone is not enough (fire.go, recordFiring):
//
//   - the count is re-read INSIDE the lock, and read FRESH -- a read served from
//     this node's result cache returns the count the previous holder already
//     advanced, because the eviction reaches this node by an asynchronous
//     broadcast and the lock was released the moment the write committed;
//   - the new version is stamped AFTER the version it read. A version's
//     createdAt is the writing process's wall clock, and the latest version is
//     the one with the newest createdAt -- so a replica whose clock lags writes
//     its increment BEHIND the one it read, and every later read skips it.
//
// # Waiting: bounded, and only behind what it has to be
//
// Three things stand between a firing and its lock, and none of them can hold
// the node up:
//
//   - THIS RULE, IN MEMORY. A node's firings of one rule queue on a per-rule
//     in-memory lock before any of them asks the database for anything. So a
//     contended rule occupies at most one connection and one slot below,
//     however many of its firings arrive at once, and a rule's queue holds up
//     nobody but that rule.
//   - A SLOT. At most firingGateSlots firings per node hold a lock connection at
//     once. The lock holds its connection for the length of the record, and the
//     record's engine calls take another; with the direct endpoint unset both
//     come from the main pool, which the base deploy caps at four, and firings
//     each holding one connection while waiting for a second would deadlock.
//     The slots are sized from the lock pool's own cap -- at most half of it --
//     so the pool always has a connection for a record to finish on.
//   - THE ACQUISITION, as a whole. A slot, a connection, BEGIN, SET LOCAL and
//     the advisory lock all happen inside ONE deadline (firingGateAcquireTimeout),
//     and the server's lock_timeout is set from what remains of it, so the
//     server gives up first with a clean refusal. A starved pool, a leader node
//     whose direct pool is half spent on its leases, or a holder that never lets
//     go all cost one firing at most that long.
//
// # Degradation, stated rather than hidden
//
// A gate that cannot lock in time -- no database handle, no slot, no
// connection, a lock wait past the deadline -- records the firing UNLOCKED and
// logs why, still inside the per-rule queue: this node's firings of the rule
// stay exact, and only another replica's overlapping firing may lose its
// increment -- the pre-memql#5431 behaviour, for that one firing. lastFiredAt
// and lastError still land. Refusing instead would lose the whole record, fail
// the run, and count toward the rule's circuit breaker for a fault that is not
// the rule's.
//
// # What the gate does NOT cover
//
// It orders FIRINGS against firings, and nothing else. The rule row has other
// writers -- the owner's edits (updateEmailRule), pause and resume
// (setEmailRuleStatus), the generator's record (recordEmailRuleGeneration) --
// and none of them takes this lock. Each is a read-merge-write of the WHOLE
// row, so one that overlaps a firing's record can revert what the record just
// wrote, or the record can revert it: an edit that read the row before a firing
// landed writes the old count back, and a firing that read the row before a
// pause landed writes `active` back. Clock skew between replicas does the same
// without any overlap, filing one writer's version behind the other's. That is
// the append-only store's general read-merge property, not this package's to
// fix -- ordering every writer of a row belongs at the write chokepoint
// (executeWrite) -- so do not read this gate as a guarantee about the row.

import (
	"context"
	"database/sql/driver"
	"fmt"
	"hash/fnv"
	"log/slog"
	"sync"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/memql"
)

// FiringGate runs record while no other firing of the same rule -- on this
// node or any other -- is inside its own record.
type FiringGate func(ctx context.Context, ruleID string, record func(context.Context) error) error

// firingGateLockClass namespaces the rule-firing locks in Postgres's two-key
// advisory lock space, which is separate from the single-key space the cron
// leader, the topology reconciler and the schema lock use. Clear of the other
// two-key classes: magic-link 0x4D4C4E4B "MLNK", GitHub 0x47484342 "GHCB",
// Shopify 0x53484F50 "SHOP", cognition "COGN", greeting "GRET",
// feedback-announce "FNDR" and the planner's "PLAN". 0x454D524C spells "EMRL".
const firingGateLockClass int32 = 0x454D524C

// firingGateAcquireTimeout bounds everything before the record runs: a slot, a
// connection, BEGIN, and the advisory lock. The locked section is two engine
// round trips, so a wait this long means something is wrong elsewhere, and past
// it the firing is recorded unlocked rather than not at all.
const firingGateAcquireTimeout = 5 * time.Second

// firingGateServerMargin is how much sooner than the client the SERVER gives up
// on the lock, so the refusal arrives as a clean lock_timeout rather than as a
// client deadline cutting a statement off mid-wait. firingGateMinLockWait is the
// least lock wait worth asking for; less is reported as the acquisition's time
// having been spent before the lock.
const (
	firingGateServerMargin = 250 * time.Millisecond
	firingGateMinLockWait  = 50 * time.Millisecond
)

// firingGateMaxSlots caps the lock connections one node holds at once.
const firingGateMaxSlots = 2

// firingGateSlots sizes the slot semaphore from the lock pool's cap: at most
// half of it, so a pool the records' engine calls may share always has a
// connection for them, and never more than firingGateMaxSlots. 0 is database/
// sql's "unlimited". A pool of ONE cannot lend its connection to the lock at
// all -- the record would wait for it forever -- and gets no slots.
func firingGateSlots(maxOpenConns int) int {
	if maxOpenConns <= 0 {
		return firingGateMaxSlots
	}
	return min(firingGateMaxSlots, maxOpenConns/2)
}

// firingGateKey derives the advisory objid for one rule. FNV-32a over the BARE
// id, so the canonical and bare spellings of one rule take one lock. A
// collision serialises two rules' records against each other for the length of
// one section -- invisible, and never a correctness problem.
func firingGateKey(ruleID string) int32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(memql.BareShortId(ruleID)))
	return int32(h.Sum32())
}

// newFiringGate builds the gate over db, which should resolve the DIRECT
// endpoint where one is configured: lock waits then queue on a pool the bulk
// traffic does not use. A nil getter yields a nil gate, and the Firer records
// unlocked -- the pre-memql#5431 behaviour, for a node with no database.
func newFiringGate(db func() *bun.DB, logger *slog.Logger) FiringGate {
	return newFiringGateWithin(db, logger, firingGateAcquireTimeout)
}

// newFiringGateWithin is newFiringGate with the acquisition deadline named, for
// the tests that show a wait ends at it.
func newFiringGateWithin(db func() *bun.DB, logger *slog.Logger, acquireTimeout time.Duration) FiringGate {
	if db == nil {
		return nil
	}
	g := &firingGate{db: db, logger: logger, acquireTimeout: acquireTimeout, rules: newKeyedMutex()}
	return g.run
}

type firingGate struct {
	db             func() *bun.DB
	logger         *slog.Logger
	acquireTimeout time.Duration
	rules          *keyedMutex

	slotsOnce sync.Once
	// slots is nil when the lock pool cannot spare a connection.
	slots chan struct{}
}

func (g *firingGate) run(ctx context.Context, ruleID string, record func(context.Context) error) error {
	release, err := g.rules.lock(ctx, memql.BareShortId(ruleID))
	if err != nil {
		return fmt.Errorf("emailrules: gave up waiting to record a firing of %q: %w", ruleID, err)
	}
	defer release()

	unlock, reason, err := g.acquire(ctx, ruleID)
	if unlock == nil {
		g.recordedUnlocked(reason, err)
		return record(ctx)
	}
	defer unlock()
	return record(ctx)
}

// acquire takes the rule's lock across every replica, inside the gate's
// deadline. It answers the release, or nil with why it could not.
func (g *firingGate) acquire(ctx context.Context, ruleID string) (unlock func(), reason string, err error) {
	handle := g.db()
	if handle == nil {
		return nil, "no database handle", nil
	}
	slots := g.slotsFor(handle)
	if slots == nil {
		return nil, "the lock pool has one connection, and the record needs it", nil
	}
	deadline := time.Now().Add(g.acquireTimeout)
	actx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	select {
	case slots <- struct{}{}:
	case <-actx.Done():
		return nil, "no free slot on this node", actx.Err()
	}
	freeSlot := func() { <-slots }

	conn, err := handle.Conn(actx)
	if err != nil {
		freeSlot()
		return nil, "no connection", err
	}
	// discard drops a connection whose transaction state is unknown instead
	// of returning it to the pool: closing it is what ends the transaction,
	// and with it the lock.
	discard := func() {
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		_ = conn.Close()
		freeSlot()
	}

	// A raw BEGIN on a connection held for it alone, rather than BeginTx: a
	// database/sql transaction is rolled back when the context it began under
	// ends, and that context has to be the acquisition deadline -- so the lock
	// would go the moment it was taken.
	if _, err := conn.ExecContext(actx, "BEGIN"); err != nil {
		discard()
		return nil, "begin", err
	}
	wait := time.Until(deadline) - firingGateServerMargin
	if wait < firingGateMinLockWait {
		discard()
		return nil, "the deadline was spent before the lock", context.DeadlineExceeded
	}
	if _, err := conn.ExecContext(actx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", wait.Milliseconds())); err != nil {
		discard()
		return nil, "lock timeout", err
	}
	if _, err := conn.ExecContext(actx, "SELECT pg_advisory_xact_lock(?, ?)", firingGateLockClass, firingGateKey(ruleID)); err != nil {
		discard()
		return nil, "lock", err
	}
	return func() {
		// A fresh context: a caller whose context has ended must still
		// release, or the lock rides the pooled connection to its next user.
		rctx, rcancel := context.WithTimeout(context.Background(), firingGateAcquireTimeout)
		defer rcancel()
		if _, err := conn.ExecContext(rctx, "ROLLBACK"); err != nil {
			discard()
			return
		}
		_ = conn.Close()
		freeSlot()
	}, "", nil
}

// slotsFor sizes the semaphore on first use, from the first handle the node
// resolves -- at construction the database may not be connected yet.
func (g *firingGate) slotsFor(handle *bun.DB) chan struct{} {
	g.slotsOnce.Do(func() {
		if n := firingGateSlots(handle.DB.Stats().MaxOpenConnections); n > 0 {
			g.slots = make(chan struct{}, n)
		}
	})
	return g.slots
}

func (g *firingGate) recordedUnlocked(reason string, err error) {
	if g.logger == nil {
		return
	}
	attrs := []any{"reason", reason}
	if err != nil {
		attrs = append(attrs, "error", err.Error())
	}
	g.logger.Warn("email rule firing recorded without the rule lock; an overlapping firing may lose its increment", attrs...)
}

// keyedMutex is one mutex per key, created on demand and dropped when nobody
// holds or waits for it, whose lock gives up when its context ends.
type keyedMutex struct {
	mu   sync.Mutex
	held map[string]*keyedMutexEntry
}

type keyedMutexEntry struct {
	token chan struct{}
	refs  int
}

func newKeyedMutex() *keyedMutex { return &keyedMutex{held: map[string]*keyedMutexEntry{}} }

// lock waits for key, or for ctx to end. It answers the release.
func (k *keyedMutex) lock(ctx context.Context, key string) (func(), error) {
	k.mu.Lock()
	e := k.held[key]
	if e == nil {
		e = &keyedMutexEntry{token: make(chan struct{}, 1)}
		k.held[key] = e
	}
	e.refs++
	k.mu.Unlock()

	drop := func() {
		k.mu.Lock()
		defer k.mu.Unlock()
		if e.refs--; e.refs == 0 {
			delete(k.held, key)
		}
	}
	select {
	case e.token <- struct{}{}:
		return func() { <-e.token; drop() }, nil
	case <-ctx.Done():
		drop()
		return nil, ctx.Err()
	}
}

// size reports how many keys have a holder or a waiter, for tests.
func (k *keyedMutex) size() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.held)
}
