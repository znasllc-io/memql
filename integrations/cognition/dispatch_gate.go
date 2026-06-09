package cognition

import (
	"context"
	"hash/fnv"
	"log/slog"

	"github.com/uptrace/bun"
)

// dispatchGateLockClass namespaces the cognition dispatch-gate advisory locks.
//
// Postgres two-key advisory locks (pg_try_advisory_lock(classid, objid)) occupy
// a DIFFERENT lock space than the single-key bigint form, so this never
// collides with the cron-leader lease (component/automations/cron_leader.go,
// single-key form) or the bun migrator's own keyspace. Within the two-key
// space the class byte keeps us clear of the planner's per-account admission
// lock (integrations/planner/admission.go, class 0x504C414E = "PLAN").
// 0x434F474E spells "COGN".
const dispatchGateLockClass int32 = 0x434F474E

// dispatchGate gives exactly one cognition replica the right to dispatch the
// turn for a given triggering utterance (znasllc-io/memql#1217).
//
// The bug: the utterance-created event is broadcast to BOTH cognition replicas
// (the graph.node.created.v1:cognition:* bridge rule fans out to all peers), so
// both run handleUtteranceForCognition. The only cross-replica guard was the
// read-before-write queryHasSIResponseForReply SELECT, which BOTH replicas pass
// because neither inserts its SI response until the multi-second LLM turn ends.
// Each then mints its own replyId and inserts a row -> two identical replies.
//
// The fix: a Postgres session-level advisory lock keyed by the utterance id.
// Exactly one replica wins pg_try_advisory_lock (non-blocking); the loser bails
// immediately without dispatching. This is the proven cron_leader.go /
// admission.go primitive -- DB-backed, so it is CONSISTENT across replicas
// (unlike each replica's local peer-membership view, which can disagree and
// leave an utterance owned by ZERO replicas -> a dropped reply).
//
// Lock lifetime -- option (a), held across the turn. The winner holds the lock
// from the gate until its handler returns (after insertSIResponse lands), then
// releases via the returned release func. We deliberately do NOT release early
// and write a separate "claim" row (option (b)), because:
//
//   - Contention is bounded: the broadcast delivers the event to each replica
//     exactly once, so at most N (today 2) contenders ever race for a given
//     utterance, each holding ONE pooled connection only for its own in-flight
//     turn. The pool pressure is a handful of connections at 2 replicas --
//     negligible. (Re-evaluate option (b) if replica count grows large.)
//   - Crash-safety: a session-scoped lock is auto-released by Postgres if the
//     winner's connection drops (pod crash). A durable claim row (option (b))
//     would instead WEDGE the utterance -- a crashed winner's claim would block
//     every retry forever, SILENTLY DROPPING the reply, which is the one
//     outcome #1217 says is worse than a duplicate. Holding the live lock keeps
//     the failure mode at "no worse than today."
//
// FAIL-SAFE. The gate is an optimization over today's read-before-write check,
// not a hard safety boundary. On ANY infrastructure failure (nil getter, DB not
// ready, connection error, lock-query error) tryDispatch returns
// (proceed=true, release=noop): the replica falls through to today's behavior
// (the existing queryHasSIResponseForReply check downstream still runs). The
// cardinal rule is that a bug in this gate must never DROP a reply -- a
// duplicate is recoverable, a dropped reply is not.
//
// NO-OP on single replica. The lone replica always wins pg_try_advisory_lock,
// so local/single-node dev (one cognition replica) is unaffected.
type dispatchGate struct {
	dbGetter func() *bun.DB
	logger   *slog.Logger
}

// newDispatchGate builds a gate over a lazy *bun.DB getter (the DB may not be
// Ready at construction, so resolution is deferred to each call, matching
// cron_leader / admission). A nil getter or nil logger is tolerated -- the gate
// then proceeds (fails safe) on every call.
func newDispatchGate(dbGetter func() *bun.DB, logger *slog.Logger) *dispatchGate {
	if logger == nil {
		logger = slog.Default()
	}
	return &dispatchGate{dbGetter: dbGetter, logger: logger}
}

// dispatchGateNoop is the release returned whenever the gate failed safe (no
// lock was actually taken). Calling it is harmless.
func dispatchGateNoop() {}

// tryDispatch attempts to claim the right to dispatch utteranceId on this
// replica.
//
// Contract:
//   - returns (true, release, nil)  -> this replica WON; it proceeds to
//     dispatch and MUST call release (defer it) when the turn completes.
//   - returns (false, noop, nil)    -> another replica already owns this
//     utterance; the caller must return WITHOUT dispatching. The returned
//     release is a safe no-op.
//   - returns (true, noop, err)     -> infrastructure failure; the caller FAILS
//     SAFE and proceeds (the returned release is a no-op; the err is for
//     logging only).
func (g *dispatchGate) tryDispatch(ctx context.Context, utteranceId string) (proceed bool, release func(), err error) {
	if g == nil || g.dbGetter == nil {
		return true, dispatchGateNoop, nil // fail safe: no DB accounting available
	}
	db := g.dbGetter()
	if db == nil || db.DB == nil {
		return true, dispatchGateNoop, nil // fail safe: DB not ready (e.g. single-binary dev before Ready)
	}

	conn, cerr := db.DB.Conn(ctx)
	if cerr != nil {
		return true, dispatchGateNoop, cerr // fail safe
	}

	objid := dispatchGateLockKey(utteranceId)
	var acquired bool
	if qerr := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1, $2)", dispatchGateLockClass, objid).Scan(&acquired); qerr != nil {
		_ = conn.Close()
		return true, dispatchGateNoop, qerr // fail safe: never drop a reply because the gate query errored
	}

	if !acquired {
		// Another cognition replica already holds this utterance's lock and is
		// dispatching it. Release this replica's connection and bail.
		_ = conn.Close()
		return false, dispatchGateNoop, nil
	}

	// We won. Hold the session lock (and its connection) until the caller's
	// turn completes; release unlocks on a background context so a cancelled
	// ctx still frees the lock, then returns the connection to the pool.
	release = func() {
		_, _ = conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1, $2)", dispatchGateLockClass, objid)
		_ = conn.Close()
	}
	return true, release, nil
}

// dispatchGateLockKey hashes an utterance id to the 32-bit object key of its
// dispatch advisory lock. FNV-1a is stable across processes so every replica
// derives the same key for the same utterance.
func dispatchGateLockKey(utteranceId string) int32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(utteranceId))
	return int32(h.Sum32())
}
