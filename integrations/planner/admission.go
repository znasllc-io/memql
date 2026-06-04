package planner

import (
	"context"
	"database/sql"
	"hash/fnv"
	"log/slog"

	"github.com/uptrace/bun"
)

// maxAdmitPerSweep bounds a single AdmitNext sweep so a pathological queue +
// promote loop can never spin unbounded while holding the account lock.
const maxAdmitPerSweep = 1000

// admissionLockClass namespaces the planner's per-account admission advisory
// locks. Postgres two-key advisory locks (pg_advisory_lock(classid, objid))
// occupy a DIFFERENT lock space than the single-key form, so this never
// collides with the cron-leader lease (component/automations/cron_leader.go),
// which uses the one-argument bigint form. 0x504C414E spells "PLAN".
const admissionLockClass int32 = 0x504C414E

// AdmissionController enforces a per-account cap on the number of
// concurrently-running Plans (epic memql#902 / child #904). It is the
// race-free half of the gate: the EntitlementResolver (#903) supplies the cap,
// this decides -- atomically -- whether one more Plan may occupy a slot.
//
// Atomicity uses a per-account session-scoped Postgres advisory lock (the
// cron_leader.go pattern): while the lock is held we COUNT the account's
// current running Plans and, if that would exceed the cap, demote the
// just-dispatched Plan to waitingForSlot. The demote runs INSIDE the lock (and
// commits before the lock is released), so a concurrent admission for the same
// account always sees the reduced count -- two dispatches can never both slip
// past the same free slot. Different accounts hash to different lock keys and
// never block each other.
//
// FAIL-OPEN. The cap is an optimization over the default-uncapped behavior,
// not a safety gate. Any infrastructure failure (no DB, no connection, lock or
// count error) makes Resolve... admit the Plan rather than wedge dispatch: the
// worst case is an account briefly exceeding its cap, which self-corrects as
// each running Plan re-passes admission.
type AdmissionController struct {
	dbGetter func() *bun.DB
	logger   *slog.Logger
}

// NewAdmissionController builds a controller over a lazy *bun.DB getter (the
// DB may not be Ready at construction, so resolution is deferred to each call,
// matching cron_leader). A nil getter or nil logger is tolerated -- the
// controller then fails open on every call.
func NewAdmissionController(dbGetter func() *bun.DB, logger *slog.Logger) *AdmissionController {
	if logger == nil {
		logger = slog.Default()
	}
	return &AdmissionController{dbGetter: dbGetter, logger: logger}
}

// decideAdmission is the pure cap rule: a non-positive cap is unlimited (a
// defensive backstop -- callers skip the gate entirely for unlimited
// accounts), otherwise the total running count (which INCLUDES the Plan being
// admitted, since the Run click already stamped it running) must be within the
// cap.
func decideAdmission(runningCount, cap int) bool {
	if cap <= 0 {
		return true
	}
	return runningCount <= cap
}

// countRunningQuery counts the LATEST version of each Plan row that is
// currently running and owned by the account. MemoryNodes is an append-only
// hypertable (PK (id, createdAt)), so the DISTINCT ON picks each Plan's current
// version before the status / owner filter.
const countRunningQuery = `WITH latest AS (
    SELECT DISTINCT ON (id) id, payload
    FROM "MemoryNodes"
    WHERE concept = 'v1:planner:plan'
    ORDER BY id, "createdAt" DESC
)
SELECT count(*) FROM latest
WHERE payload->>'status' = 'running'
  AND payload->>'requestedBy' = $1`

// TryAdmit decides whether planId may run under the account's cap, atomically.
//
// Contract:
//   - returns (true, nil)  -> a slot is available; the caller proceeds to run.
//   - returns (false, nil) -> over cap; `demote` ran (the Plan was parked to
//     waitingForSlot); the caller must NOT run it.
//   - returns (_, err)     -> an infrastructure failure; the caller must FAIL
//     OPEN and run the Plan anyway (the returned bool is best-effort).
//
// `demote` is invoked inside the held lock when the Plan is over cap; it is the
// caller's engine-side transition to waitingForSlot. Keeping it as a callback
// keeps this controller decoupled from the engine and unit-testable against a
// raw DB.
func (ac *AdmissionController) TryAdmit(
	ctx context.Context,
	accountId string,
	planId string,
	cap int,
	demote func(context.Context) error,
) (bool, error) {
	if ac == nil || ac.dbGetter == nil {
		return true, nil // fail open: no accounting available
	}
	db := ac.dbGetter()
	if db == nil || db.DB == nil {
		return true, nil // fail open: DB not ready
	}

	conn, err := db.DB.Conn(ctx)
	if err != nil {
		return true, err // fail open
	}
	objid := accountLockKey(accountId)
	// Block until the per-account lock is ours. Session-scoped (not xact) so we
	// can interleave the engine-side demote (a different connection) before
	// releasing -- guaranteeing the next holder sees the committed transition.
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1, $2)", admissionLockClass, objid); err != nil {
		_ = conn.Close()
		return true, err // fail open
	}
	defer func() {
		// Release on a background context so a cancelled ctx still frees the
		// lock; closing the connection returns it to the pool.
		_, _ = conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1, $2)", admissionLockClass, objid)
		_ = conn.Close()
	}()

	var runningCount int
	if err := conn.QueryRowContext(ctx, countRunningQuery, accountId).Scan(&runningCount); err != nil {
		return true, err // fail open
	}

	if decideAdmission(runningCount, cap) {
		return true, nil
	}

	// Over cap: park the Plan inside the lock so the next admission for this
	// account counts one fewer running Plan.
	if demote != nil {
		if derr := demote(ctx); derr != nil {
			// Demote failed -> the Plan is still running; fail open and let it
			// proceed rather than leave it stuck mid-gate.
			return true, derr
		}
	}
	return false, nil
}

// accountLockKey hashes an account id to the 32-bit object key of the
// per-account advisory lock. FNV-1a is stable across processes so every node
// derives the same key for the same account.
func accountLockKey(accountId string) int32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(accountId))
	return int32(h.Sum32())
}

// anyWaitingQuery is the lock-free fast path: does the account have ANY Plan
// parked at waitingForSlot? When not (the overwhelmingly common case -- no
// queue), AdmitNext skips the advisory lock + count entirely.
const anyWaitingQuery = `WITH latest AS (
    SELECT DISTINCT ON (id) id, payload
    FROM "MemoryNodes"
    WHERE concept = 'v1:planner:plan'
    ORDER BY id, "createdAt" DESC
)
SELECT 1 FROM latest
WHERE payload->>'status' = 'waitingForSlot'
  AND payload->>'requestedBy' = $1
LIMIT 1`

// pickOldestWaitingQuery selects the FIFO head of the account's waiting queue:
// the longest-parked Plan, ordered by when it entered waitingForSlot (the
// createdAt of its current version).
const pickOldestWaitingQuery = `WITH latest AS (
    SELECT DISTINCT ON (id) id, payload, "createdAt"
    FROM "MemoryNodes"
    WHERE concept = 'v1:planner:plan'
    ORDER BY id, "createdAt" DESC
)
SELECT id FROM latest
WHERE payload->>'status' = 'waitingForSlot'
  AND payload->>'requestedBy' = $1
ORDER BY "createdAt" ASC
LIMIT 1`

// AdmitNext admits as many of the account's waiting Plans as there are free
// slots, FIFO (epic memql#902 / #905). It is the event-driven counterpart to
// TryAdmit: when a running Plan reaches a slot-freeing state, the caller invokes
// this to pull the longest-waiting Plan(s) into running.
//
// Under the per-account advisory lock (same key as TryAdmit, so the two never
// race), it loops: count the account's running Plans; while a slot is free
// (cap <= 0 means unlimited -> drain the whole queue) and a waiting Plan
// exists, promote the FIFO head via `promote` (the caller's engine transition
// to running) and re-count. The promote commits before the next count, so the
// loop never over-admits past the cap. Returns the number promoted.
//
// FAIL-OPEN / no-op: a nil getter or nil DB returns (0, nil); infra errors
// return what was admitted so far plus the error (the caller logs and moves on
// -- a missed wakeup self-heals on the next slot-free event).
func (ac *AdmissionController) AdmitNext(
	ctx context.Context,
	accountId string,
	cap int,
	promote func(context.Context, string) error,
) (int, error) {
	if ac == nil || ac.dbGetter == nil {
		return 0, nil
	}
	db := ac.dbGetter()
	if db == nil || db.DB == nil {
		return 0, nil
	}

	// Fast path: no queue -> nothing to admit, don't even take the lock.
	var probe int
	switch err := db.DB.QueryRowContext(ctx, anyWaitingQuery, accountId).Scan(&probe); err {
	case sql.ErrNoRows:
		return 0, nil
	case nil:
		// there is at least one waiting Plan; fall through to the locked sweep
	default:
		return 0, err
	}

	conn, err := db.DB.Conn(ctx)
	if err != nil {
		return 0, err
	}
	objid := accountLockKey(accountId)
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1, $2)", admissionLockClass, objid); err != nil {
		_ = conn.Close()
		return 0, err
	}
	defer func() {
		_, _ = conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1, $2)", admissionLockClass, objid)
		_ = conn.Close()
	}()

	admitted := 0
	for i := 0; i < maxAdmitPerSweep; i++ {
		var running int
		if err := conn.QueryRowContext(ctx, countRunningQuery, accountId).Scan(&running); err != nil {
			return admitted, err
		}
		if cap > 0 && running >= cap {
			break // no free slot
		}
		var planId string
		switch err := conn.QueryRowContext(ctx, pickOldestWaitingQuery, accountId).Scan(&planId); err {
		case sql.ErrNoRows:
			return admitted, nil // queue drained
		case nil:
			// got the FIFO head
		default:
			return admitted, err
		}
		if promote != nil {
			if perr := promote(ctx, planId); perr != nil {
				return admitted, perr
			}
		}
		admitted++
	}
	return admitted, nil
}
