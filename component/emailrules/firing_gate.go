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
// lock_timeout): the server bounds the wait, and COMMIT or ROLLBACK releases the
// lock whatever the client does -- no session lock can be left held on a pooled
// connection by an acquire whose reply was lost. The transaction writes
// nothing; it exists to hold the lock while the record runs.
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
// # The process-local mutex is connection economy, not the fix
//
// The lock holds a connection for the length of the record, and the record's
// own engine calls take another. With the direct endpoint unset, both come from
// the main pool, which the base deploy caps at four -- and four firings each
// holding one connection while waiting for a second would wait on each other
// for as long as the lock timeout. One firing per process in the locked section
// makes that impossible and costs nothing that matters: the section is two
// engine round trips. It is per GATE, and a gate is per node.
//
// # Degradation, stated rather than hidden
//
// A gate that cannot lock -- no database handle, a connection error, a lock
// wait past the timeout -- records the firing UNLOCKED and logs why. That is
// exactly the pre-memql#5431 behaviour: an overlapping firing may lose its
// increment, but lastFiredAt and lastError still land. Refusing instead would
// lose the whole record, fail the run, and count toward the rule's circuit
// breaker for a fault that is not the rule's.

import (
	"context"
	"hash/fnv"
	"log/slog"
	"sync"

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

// firingGateLockTimeout is the SERVER's bound on the wait (SET LOCAL
// lock_timeout). The locked section is two engine round trips, so a wait this
// long means something is wrong elsewhere, and past it the firing is recorded
// unlocked rather than not at all.
const firingGateLockTimeout = "5s"

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
	if db == nil {
		return nil
	}
	var local sync.Mutex
	unlocked := func(ctx context.Context, record func(context.Context) error, reason string, err error) error {
		if logger != nil {
			attrs := []any{"reason", reason}
			if err != nil {
				attrs = append(attrs, "error", err.Error())
			}
			logger.Warn("email rule firing recorded without the rule lock; an overlapping firing may lose its increment", attrs...)
		}
		return record(ctx)
	}
	return func(ctx context.Context, ruleID string, record func(context.Context) error) error {
		local.Lock()
		defer local.Unlock()

		handle := db()
		if handle == nil {
			return unlocked(ctx, record, "no database handle", nil)
		}
		tx, err := handle.BeginTx(ctx, nil)
		if err != nil {
			return unlocked(ctx, record, "begin", err)
		}
		// Ending the transaction is what releases the lock. It wrote nothing,
		// so rolling back and committing are the same statement; a second
		// rollback after the explicit ones below is a no-op.
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx, "SET LOCAL lock_timeout = '"+firingGateLockTimeout+"'"); err != nil {
			_ = tx.Rollback()
			return unlocked(ctx, record, "lock timeout", err)
		}
		if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(?, ?)", firingGateLockClass, firingGateKey(ruleID)); err != nil {
			_ = tx.Rollback()
			return unlocked(ctx, record, "lock", err)
		}
		return record(ctx)
	}
}
