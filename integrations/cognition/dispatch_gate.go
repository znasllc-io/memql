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

// greetGateLockClass namespaces the greet-on-join advisory locks. A DISTINCT
// class from dispatchGateLockClass keeps the greeting gate's key space wholly
// separate from the utterance-dispatch gate -- a greeting keyed on
// (spaceId, agentId) and an utterance keyed on its id can never alias onto the
// same lock even if their FNV hashes collide. 0x47524554 spells "GRET".
const greetGateLockClass int32 = 0x47524554

// feedbackAnnounceGateLockClass namespaces the assistant-mediated
// plan-feedback announcement advisory locks (epic memql#1404 / child #1406).
// When a Plan in a space transitions to awaitingFeedback, the
// graph.node.updated.v1:planner:plan event is broadcast to BOTH cognition
// replicas, so both would post the "I need your input on X" chat message
// without a cross-replica guard -- the same double-post hazard #1386 fixed
// for greetings. This gate is keyed on (spaceId, planId) under a distinct
// class so it never aliases an utterance-dispatch or greeting lock even on an
// FNV collision. 0x464E4452 spells "FNDR" (FeedbackANnounceR).
const feedbackAnnounceGateLockClass int32 = 0x464E4452

// dispatchGate gives exactly one cognition replica the right to dispatch the
// turn for a given triggering utterance (znasllc-io/memql#1217).
//
// The bug: the utterance-created event is broadcast to BOTH cognition replicas
// (the graph.node.created.v1:cognition:* bridge rule fans out to all peers), so
// both run handleUtteranceForCognition. The only cross-replica guard was the
// read-before-write queryHasAIResponseForReply SELECT, which BOTH replicas pass
// because neither inserts its AI response until the multi-second LLM turn ends.
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
// from the gate until its handler returns (after insertAIResponse lands), then
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
//     the auto-recovery path open: the survivor (or the crashed pod's restart)
//     re-runs the handler and wins the now-free lock.
//
// WHY THE GATE IS SAFE TO ENFORCE NOW (znasllc-io/memql#1272, epic #1259
// Phase 4). The #1217 gate kills the duplicate that two cognition replicas
// produce when both run the turn for one utterance. It was correct from day
// one, but it removed the double-dispatch redundancy that had been MASKING an
// unreliable mesh: on the old star topology a worker could not reliably reach
// the bff replica that owned the user's WebSocket, so the lone surviving
// dispatch sometimes never reached the browser (#1232 / the "assistant not
// responding" incidents). With redundancy gone, a strict gate risked turning a
// duplicate (recoverable) into a DROP (not recoverable), so the gate was rolled
// back at the deploy layer until delivery was made reliable.
//
// Delivery is now reliable. #1264 routes the chat-reply (utterance / presence /
// canvas) path through the durable DeliverySubstrate: the producer writes a
// durable, logically-addressed (space:<id>) outbox row and the WS-owning bff
// receives it via its durable subscription regardless of which worker produced
// it, with per-EventID dedup and per-key ordering + replay on (re)connect.
// #1265 moves the dispatch/return RPC leg onto the same substrate, and #1271
// reverts the harmful #1245 dead-peer skip (the mesh fast-path now buffers,
// never drops). So the SINGLE reply the gate lets through is GUARANTEED to be
// delivered. The gate now gives no dup AND no drop -- it is the genuine
// exactly-once boundary, not merely "no worse than the old double dispatch."
//
// FAIL-SAFE (preserved -- for genuine error conditions only). The gate's
// exactly-once decision (the !acquired bail below) is now enforced for real,
// because the substrate backstops delivery of the reply it admits. But the
// cardinal rule is unchanged: a BUG or INFRASTRUCTURE FAILURE in the gate must
// never DROP a reply. So on ANY infrastructure failure (nil getter, DB not
// ready, connection error, lock-query error) tryDispatch returns
// (proceed=true, release=noop): the replica falls through to the existing
// queryHasAIResponseForReply read-before-write check downstream. The fail-safe
// fires ONLY when the gate cannot make a trustworthy lock decision (the DB is
// the gate's source of truth); a clean "another replica owns this" answer is
// honored strictly. A duplicate is recoverable; a dropped reply is not.
//
// NO-OP on single replica. The lone replica always wins pg_try_advisory_lock,
// so local/single-node dev (one cognition replica) is unaffected.
type dispatchGate struct {
	// directDBGetter resolves the DIRECT (non-pooled) *bun.DB. The gate holds a
	// session-scoped Postgres advisory lock across a whole turn (incl. the
	// multi-second LLM call), so it MUST run on the direct endpoint:
	// transaction-mode PgBouncer recycles the server backend between statements
	// and would silently drop a held session lock (epic memql#1925). When
	// DIRECT_DSN is unset, DirectBunDB() falls back to the main pool, so
	// local/single-pool behavior is unchanged.
	directDBGetter func() *bun.DB
	logger         *slog.Logger
}

// newDispatchGate builds a gate over a lazy DIRECT *bun.DB getter (the DB may
// not be Ready at construction, so resolution is deferred to each call,
// matching cron_leader / admission). The getter MUST resolve the direct
// (non-pooled) endpoint -- the gate's session-scoped advisory locks cannot
// survive a transaction-mode pooler (epic memql#1925). A nil getter or nil
// logger is tolerated -- the gate then proceeds (fails safe) on every call.
func newDispatchGate(directDBGetter func() *bun.DB, logger *slog.Logger) *dispatchGate {
	if logger == nil {
		logger = slog.Default()
	}
	return &dispatchGate{directDBGetter: directDBGetter, logger: logger}
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
	if g == nil || g.directDBGetter == nil {
		return true, dispatchGateNoop, nil // fail safe: no DB accounting available
	}
	db := g.directDBGetter()
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

// tryGreet gives exactly one cognition replica the right to fire the
// greet-on-join greeting for a given (spaceId, agentId) (znasllc-io/memql#1386).
//
// The bug it fixes: the v1:cognition:participant.created event is broadcast to
// BOTH cognition replicas, so both run handleAIParticipantGreeting and both
// call runGreetingTurn. The only cross-replica guard was the read-before-write
// greetingExists SELECT, which BOTH replicas pass because neither inserts its
// greeting utterance until after the multi-second initial-delay sleep + LLM
// call. Each then inserts an agentGreeting utterance -> the greeting is posted
// twice. (The per-space greetingPacing mutex serializes greetings WITHIN one
// process; it does nothing across replicas.)
//
// The fix mirrors tryDispatch exactly: a Postgres session-level advisory lock,
// here keyed by (spaceId, agentId) instead of utterance id and namespaced under
// greetGateLockClass so it never aliases an utterance-dispatch lock. Exactly
// one replica wins pg_try_advisory_lock (non-blocking); the loser bails without
// greeting. The winner holds the lock from the gate across the initial-delay
// sleep + LLM call + insert, then releases via the returned release func --
// option (a), same lifetime reasoning as tryDispatch (bounded contention,
// crash-safe auto-release vs. a wedging claim row).
//
// FAIL-SAFE. On ANY infrastructure failure (nil gate/getter, DB not ready,
// connection error, lock-query error) tryGreet returns (proceed=true,
// release=noop): the replica falls through to the existing greetingExists
// read-before-write check, which is itself a durable dedup once one greeting
// lands. So a gate-infra failure can at worst reproduce the old duplicate, never
// silently SUPPRESS a legitimate greeting.
//
// NO-OP on single replica. The lone replica always wins, so single-node dev is
// unaffected.
func (g *dispatchGate) tryGreet(ctx context.Context, spaceId, agentId string) (proceed bool, release func(), err error) {
	if g == nil || g.directDBGetter == nil {
		return true, dispatchGateNoop, nil // fail safe: no DB accounting available
	}
	db := g.directDBGetter()
	if db == nil || db.DB == nil {
		return true, dispatchGateNoop, nil // fail safe: DB not ready
	}

	conn, cerr := db.DB.Conn(ctx)
	if cerr != nil {
		return true, dispatchGateNoop, cerr // fail safe
	}

	objid := greetGateLockKey(spaceId, agentId)
	var acquired bool
	if qerr := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1, $2)", greetGateLockClass, objid).Scan(&acquired); qerr != nil {
		_ = conn.Close()
		return true, dispatchGateNoop, qerr // fail safe: never suppress a greeting because the gate query errored
	}

	if !acquired {
		// Another cognition replica already holds this (space, agent) greeting
		// lock and is firing the greeting. Release the connection and bail.
		_ = conn.Close()
		return false, dispatchGateNoop, nil
	}

	// We won. Hold the session lock (and its connection) until the caller's
	// greeting turn completes; release unlocks on a background context so a
	// cancelled ctx still frees the lock, then returns the connection to the pool.
	release = func() {
		_, _ = conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1, $2)", greetGateLockClass, objid)
		_ = conn.Close()
	}
	return true, release, nil
}

// greetGateLockKey hashes (spaceId, agentId) to the 32-bit object key of the
// greet-on-join advisory lock. FNV-1a over a delimiter-joined key is stable
// across processes so every replica derives the same key for the same greeting.
// The NUL delimiter cannot appear in an id, so distinct (space, agent) pairs
// never collide via concatenation ambiguity.
func greetGateLockKey(spaceId, agentId string) int32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(spaceId))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(agentId))
	return int32(h.Sum32())
}

// tryAnnounceFeedback gives exactly one cognition replica the right to post
// the assistant's "Plan X needs your input" chat message for a given
// (spaceId, planId) (epic memql#1404 / child #1406).
//
// The bug it fixes mirrors #1386 exactly: the
// graph.node.updated.v1:planner:plan event for a Plan entering
// awaitingFeedback is broadcast to BOTH cognition replicas (the
// graph.node.updated.v1:planner:* forward rule in component/node/routing.go
// is a broadcast), so both run the announce handler and both insert the
// announcement utterance. The only other guard is a read-before-write dedup
// (queryHasFeedbackAnnouncement), which BOTH replicas pass because neither
// inserts until after its query + insert -- so a strict cross-replica gate is
// needed to make the announcement exactly-once.
//
// The fix is the proven tryGreet/tryDispatch primitive: a Postgres
// session-level advisory lock keyed by (spaceId, planId), namespaced under
// feedbackAnnounceGateLockClass so it never aliases an utterance-dispatch or
// greeting lock. Exactly one replica wins pg_try_advisory_lock (non-blocking);
// the loser bails without announcing. The winner holds the lock from the gate
// across its query + insert, then releases via the returned release func.
//
// FAIL-SAFE. On ANY infrastructure failure (nil gate/getter, DB not ready,
// connection error, lock-query error) tryAnnounceFeedback returns
// (proceed=true, release=noop): the replica falls through to the
// read-before-write announcement dedup, which is itself durable once one
// announcement lands. So a gate-infra failure can at worst reproduce the old
// duplicate, never silently SUPPRESS a legitimate announcement.
//
// NO-OP on single replica. The lone replica always wins, so single-node dev is
// unaffected.
func (g *dispatchGate) tryAnnounceFeedback(ctx context.Context, spaceId, planId string) (proceed bool, release func(), err error) {
	if g == nil || g.directDBGetter == nil {
		return true, dispatchGateNoop, nil // fail safe: no DB accounting available
	}
	db := g.directDBGetter()
	if db == nil || db.DB == nil {
		return true, dispatchGateNoop, nil // fail safe: DB not ready
	}

	conn, cerr := db.DB.Conn(ctx)
	if cerr != nil {
		return true, dispatchGateNoop, cerr // fail safe
	}

	objid := feedbackAnnounceGateLockKey(spaceId, planId)
	var acquired bool
	if qerr := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1, $2)", feedbackAnnounceGateLockClass, objid).Scan(&acquired); qerr != nil {
		_ = conn.Close()
		return true, dispatchGateNoop, qerr // fail safe: never suppress an announcement because the gate query errored
	}

	if !acquired {
		// Another cognition replica already holds this (space, plan) announce
		// lock and is posting the message. Release the connection and bail.
		_ = conn.Close()
		return false, dispatchGateNoop, nil
	}

	release = func() {
		_, _ = conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1, $2)", feedbackAnnounceGateLockClass, objid)
		_ = conn.Close()
	}
	return true, release, nil
}

// feedbackAnnounceGateLockKey hashes (spaceId, planId) to the 32-bit object key
// of the plan-feedback announcement advisory lock. FNV-1a over a
// NUL-delimiter-joined key is stable across processes so every replica derives
// the same key for the same (space, plan). The NUL delimiter cannot appear in
// an id, so distinct pairs never collide via concatenation ambiguity.
func feedbackAnnounceGateLockKey(spaceId, planId string) int32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(spaceId))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(planId))
	return int32(h.Sum32())
}
