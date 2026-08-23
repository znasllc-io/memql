package identity

import (
	"context"
	"hash/fnv"
	"time"
)

// magic_link_gate.go -- the critical section that makes a magic-link
// consume exactly-once (memql#4301).
//
// # Why a gate exists at all
//
// ConsumeMagicLinkRequest was a read-then-write, and its own comment said
// so: "the narrow race is acceptable for v1; the proper fix is a CAS-style
// consume mutation" (magiclink/verifier.go). It was survivable while the
// only finisher was a human clicking once. The device-bound flow ends that.
// There are now two LEGITIMATE finishers of one request -- the /check-email
// poller and a same-device click on the landing page -- and they can arrive
// milliseconds apart on two identity replicas. Two consumes mean two auth
// codes minted from one link, which is precisely the single-use property
// magic links are for.
//
// # Why a lock rather than a conditional update
//
// A DSL update{} is a read-merge-write against a time-series row: two
// concurrent updates produce two versions and the later one wins, with
// neither writer able to tell it lost. There is no predicate to hang a
// compare-and-swap on. Write-then-read-back does not rescue it either --
// A can write, read back its own value and declare victory before B
// writes, so both "win".
//
// So the compare and the swap happen while ONE caller holds a Postgres
// advisory lock keyed on the request id. The winner's write is committed
// (the engine autocommits) before the lock is released, so the next holder
// re-reads and sees consumedAt set. That is the property the test pins:
// N concurrent consumers of one row yield exactly one success.
//
// # The lock must be taken on the DIRECT connection
//
// A transaction-mode PgBouncer recycles the server backend between
// statements and would silently drop a session-scoped lock (epic
// memql#1925). The wiring passes the same directDBGetter the cron leader,
// the topology reconciler and the recovery-key mint use. Silently is the
// operative word: the code would read as correct and the mutual exclusion
// would simply not be there.
//
// # Degradation, stated rather than hidden
//
// With no DB getter wired (unit tests, an engine-only harness) the caller
// runs the same read-check-write WITHOUT the lock. That is exactly the
// pre-memql#4301 behaviour -- no worse, and no silent loss of a legitimate
// consume. It is not the shipped configuration: app/integrations_identity.go
// wires the getter, and the db-gated test asserts the property against a real
// Postgres.

// magicLinkGateLockClass namespaces the magic-link advisory locks.
//
// Postgres two-key advisory locks (pg_advisory_lock(classid, objid)) occupy a
// DIFFERENT lock space from the single-key bigint form, so this cannot collide
// with the cron-leader lease, the topology reconciler, the schema lock or the
// recovery-key mint (all single-key). Within the two-key space the class byte
// keeps clear of cognition's dispatch (0x434F474E "COGN"), greeting
// (0x47524554 "GRET") and feedback-announce (0x464E4452 "FNDR") gates and the
// planner's admission lock (0x504C414E "PLAN"). 0x4D4C4E4B spells "MLNK".
const magicLinkGateLockClass int32 = 0x4D4C4E4B

// magicLinkGateTimeout bounds the wait for the lock. A holder that wedges
// must not hang a sign-in request: past this the caller proceeds unlocked,
// which reduces it to the pre-memql#4301 race rather than to a failure. A
// consume's critical section is two engine round-trips, so a wait this long
// already means something is wrong elsewhere.
const magicLinkGateTimeout = 5 * time.Second

// magicLinkGateKey derives the advisory objid for one request id. FNV-32a,
// matching the cognition gates. A hash collision costs two unrelated requests
// serialising against each other for the length of one critical section --
// invisible, and never a correctness problem, because the re-read inside the
// section is keyed on the id itself.
func magicLinkGateKey(requestId string) int32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(requestId))
	return int32(h.Sum32())
}

// withMagicLinkGate runs fn while holding the advisory lock for requestId.
//
// It returns fn's error. A gate-infrastructure failure (no getter, DB not
// ready, connection error, lock query error, timeout) never fails the call:
// fn runs anyway, unlocked. See the degradation note above -- suppressing a
// legitimate sign-in because a lock query errored would be a worse outcome
// than the race the lock exists to close.
func (s *Store) withMagicLinkGate(ctx context.Context, requestId string, fn func(context.Context) error) error {
	if s == nil || s.DirectDB == nil {
		return fn(ctx)
	}
	db := s.DirectDB()
	if db == nil {
		return fn(ctx)
	}

	lockCtx, cancel := context.WithTimeout(ctx, magicLinkGateTimeout)
	defer cancel()

	conn, err := db.Conn(lockCtx)
	if err != nil {
		s.warn("identity.store: magic-link gate connection unavailable; proceeding unlocked", "error", err.Error())
		return fn(ctx)
	}
	defer func() { _ = conn.Close() }()

	objid := magicLinkGateKey(requestId)
	// BLOCKING, not pg_try_advisory_lock. The loser of this race must not
	// bail -- it must WAIT, re-read, and discover that it lost, so it can
	// render "this link has already been used" instead of silently doing
	// nothing. tryDispatch's non-blocking form is right for suppressing a
	// duplicate side effect; this one has to produce an answer either way.
	if _, err := conn.ExecContext(lockCtx, "SELECT pg_advisory_lock($1, $2)", magicLinkGateLockClass, objid); err != nil {
		s.warn("identity.store: magic-link gate not acquired; proceeding unlocked", "error", err.Error())
		return fn(ctx)
	}
	defer func() {
		// Unlock on a background context so a client disconnect mid-flight
		// cannot leave the lock held for the life of the pooled connection.
		uctx, ucancel := context.WithTimeout(context.Background(), magicLinkGateTimeout)
		defer ucancel()
		if _, err := conn.ExecContext(uctx, "SELECT pg_advisory_unlock($1, $2)", magicLinkGateLockClass, objid); err != nil {
			s.warn("identity.store: magic-link gate unlock failed", "error", err.Error())
		}
	}()

	return fn(ctx)
}
