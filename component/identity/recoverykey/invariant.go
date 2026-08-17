package recoverykey

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// invariant.go -- the single-flight mint (memql#3965).
//
// # The rule is an INVARIANT, not a first-boot action
//
// "If a cluster owner exists and there is no active, unredeemed recovery key,
// mint one." Evaluated on every identity-node start, and again immediately
// after an owner row is created.
//
// It is stated that way because "mint once on first boot" is not implementable
// as written: on a genuinely fresh cluster there is no owner USER at first
// boot to bind a key to. A cluster is claimed by its first sign-in and
// Store.CreateUserOnFirstLogin makes the row then; attemptAutoBootstrap only
// writes clusterSettings and emails the claim link (it does name an owner up
// front when the env bootstrap is complete -- memql#3591 -- but that path is
// not guaranteed to have run). A mint that demanded an owner at process start
// would therefore either bind to nobody or never happen.
//
// As an invariant it also produces the SUCCESSOR after a redemption for free:
// redemption deactivates the row, the next evaluation sees no active key, and
// mints a fresh unclaimed one. There is exactly one place in the tree that
// decides a recovery key should exist.
//
// # Why the lock, and why not the shape next door
//
// app.attemptAutoBootstrap is a read-then-write with a bounded retry and NO
// LOCK OF ANY KIND. It is explicitly not the template here. That shape is what
// memql#3400 punished: identity runs `replicas: 2`, both pods start together,
// both read "nothing exists", and both write. For a signing key that produced
// divergent JWKS and coin-flip auth failures; here it would produce two live
// recovery keys, of which the operator claims one -- leaving a second,
// unclaimed, fully-valid owner-equivalent credential in the database that
// nobody knows about and no runbook retires.
//
// So the check and the write happen inside ONE transaction holding
// pg_advisory_xact_lock, following component/database/timescaledb.go's
// extension-creation lock. Transaction-scoped rather than session-scoped
// (pg_try_advisory_lock, the CronLeader pattern) because this is a
// short critical section that must not outlive its work, not a leadership
// claim held for a process lifetime.
//
// # The lock must be taken on the DIRECT connection
//
// A transaction-mode PgBouncer recycles the server backend between statements
// and would silently drop the lock (epic memql#1925). The caller passes the
// same directDBGetter the cron leader and the topology reconciler use. Silently
// is the operative word: the code would look correct and the mutual exclusion
// would simply not be there.

// mintLockKey is an arbitrary fixed 64-bit key, distinct from every other
// advisory lock in the tree (reconcilerLockKey 7756010113207010574,
// cronLeaderLockKey 7756010113207010561, schemaLockKey 7756010113207025510,
// timescaleExtensionAdvisoryLockKey 0x746d736c64626578).
//
// Postgres advisory locks share ONE namespace cluster-wide, so a collision
// would not error -- it would mean two unrelated subsystems serialising
// against each other, which presents as an unexplained stall rather than as a
// bug. Hence a value chosen to be far from the others.
const mintLockKey int64 = 7756010113207031964

// OwnerResolver reports the cluster's owner user ids.
//
// An interface rather than a concrete *identity.Store so this package does not
// import back into its parent, and so the concurrency test can drive two
// minters without a cluster.
type OwnerResolver interface {
	OwnerUserIds(ctx context.Context) ([]string, error)
}

// EnsureOptions carries what one evaluation of the invariant needs.
type EnsureOptions struct {
	// DB resolves the DIRECT (non-pooled) database handle. Required: without
	// it there is no lock, and without the lock there is no invariant.
	DB func() *sql.DB
	// Store performs the reads and the write.
	Store *Store
	// Owners resolves the cluster owner(s).
	Owners OwnerResolver
	// MintedBy is the attribution stamped on a row this path creates. The
	// system actor for a boot-time mint.
	MintedBy string
	// Now is injectable for tests; time.Now when nil.
	Now func() time.Time
	// Logger is optional.
	Logger *slog.Logger
}

// EnsureResult reports what one evaluation did.
type EnsureResult struct {
	// Minted is the number of keys created (0 or 1 per owner).
	Minted int
	// Plain carries the plaintext of a key minted by THIS call, keyed by owner
	// user id.
	//
	// IT IS RETURNED AND NEVER LOGGED, and that asymmetry is the whole point of
	// returning it at all. The boot path discards it -- the operator claims the
	// key later through `memql recovery-key claim`, deliberately, so that a
	// plaintext owner credential never reaches a pod log, a log aggregator, or
	// whatever ships those logs off the cluster. A caller that wants the value
	// (the claim path minting on demand) can have it; the caller that does not
	// simply drops it.
	Plain map[string]string
	// AlreadyPresent lists owners that already had a live key.
	AlreadyPresent []string
}

// EnsureForAllOwners evaluates the invariant for every cluster owner.
//
// Idempotent and safe to call on every boot: with a live key present it does
// nothing but two reads.
func EnsureForAllOwners(ctx context.Context, opts EnsureOptions) (EnsureResult, error) {
	var res EnsureResult
	res.Plain = map[string]string{}

	if opts.Store == nil || opts.Store.Engine == nil {
		return res, errors.New("recoverykey.Ensure: store not wired")
	}
	if opts.Owners == nil {
		return res, errors.New("recoverykey.Ensure: owner resolver not wired")
	}
	if opts.DB == nil {
		return res, errors.New("recoverykey.Ensure: direct database getter not wired -- " +
			"without it the mint cannot hold an advisory lock, and two identity replicas would each " +
			"mint a live owner-equivalent credential (memql#3400's shape, on a credential)")
	}
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}

	owners, err := opts.Owners.OwnerUserIds(ctx)
	if err != nil {
		return res, fmt.Errorf("recoverykey.Ensure: resolve owners: %w", err)
	}
	if len(owners) == 0 {
		// NOT AN ERROR, AND NOT A NO-OP TO BE FIXED. A fresh cluster has no
		// owner until its first sign-in, so this is the expected outcome of
		// every boot before the cluster is claimed. The invariant is evaluated
		// again once the owner row appears.
		if opts.Logger != nil {
			opts.Logger.Debug("recovery key: no cluster owner yet; nothing to bind a key to",
				"component", "recoverykey")
		}
		return res, nil
	}

	for _, ownerId := range owners {
		ownerId = strings.TrimSpace(ownerId)
		if ownerId == "" {
			continue
		}
		plain, minted, err := ensureOne(ctx, opts, ownerId, now())
		if err != nil {
			return res, err
		}
		if minted {
			res.Minted++
			res.Plain[ownerId] = plain
		} else {
			res.AlreadyPresent = append(res.AlreadyPresent, ownerId)
		}
	}
	return res, nil
}

// ensureOne runs the critical section for one owner.
func ensureOne(ctx context.Context, opts EnsureOptions, ownerId string, at time.Time) (string, bool, error) {
	db := opts.DB()
	if db == nil {
		return "", false, errors.New("recoverykey.Ensure: direct database handle unavailable")
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, fmt.Errorf("recoverykey.Ensure: begin: %w", err)
	}
	//nolint:errcheck // no-op once Commit succeeds; best-effort on the error path
	defer tx.Rollback()

	// pg_advisory_xact_lock BLOCKS rather than returning a boolean, which is
	// what makes this correct with no retry loop: the replica that loses the
	// race waits, then re-reads inside the lock and finds the key the winner
	// just wrote. A try-lock would need the loser to either skip (leaving the
	// invariant unevaluated on that node) or spin.
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", mintLockKey); err != nil {
		return "", false, fmt.Errorf("recoverykey.Ensure: acquire advisory lock: %w", err)
	}

	// RE-READ INSIDE THE LOCK. This is the entire point: the read that decides
	// must be the one the lock protects, not a read taken before it.
	live, err := opts.Store.ActiveForUser(ctx, ownerId)
	if err != nil {
		return "", false, fmt.Errorf("recoverykey.Ensure: read active keys: %w", err)
	}
	if len(live) > 0 {
		// Commit rather than roll back: nothing was written, and committing
		// releases the lock at the same point in the code either way. Rolling
		// back would be equally correct and reads as though something failed.
		if err := tx.Commit(); err != nil {
			return "", false, fmt.Errorf("recoverykey.Ensure: commit (no-op): %w", err)
		}
		return "", false, nil
	}

	plain, hash, err := Mint()
	if err != nil {
		return "", false, fmt.Errorf("recoverykey.Ensure: mint: %w", err)
	}
	identityId, err := NewId()
	if err != nil {
		return "", false, fmt.Errorf("recoverykey.Ensure: new id: %w", err)
	}
	mintedBy := opts.MintedBy
	if strings.TrimSpace(mintedBy) == "" {
		mintedBy = "system:identity-svc"
	}
	if err := opts.Store.Create(ctx, identityId, ownerId, hash, mintedBy, "", DefaultLabel); err != nil {
		return "", false, fmt.Errorf("recoverykey.Ensure: create: %w", err)
	}

	// THE WRITE GOES THROUGH THE ENGINE, THE LOCK IS HELD ON THIS TX.
	//
	// So the two are not the same transaction, and this comment exists because
	// that looks like a bug and is not the one it looks like. What the lock
	// serialises is the READ-DECIDE-WRITE window: a second replica cannot even
	// perform its read until this transaction commits, by which time the
	// engine's write has returned and its row is visible. The lock is a
	// mutex around a critical section, not an atomicity boundary over the
	// write itself.
	//
	// What that costs, stated plainly: if the process dies between the engine
	// write and the commit below, the row exists and the lock is released by
	// the backend terminating. The next evaluation reads that row, finds a live
	// key, and does nothing. The invariant converges either way -- which is why
	// this is acceptable here and would not be if the write had to be undone.
	if err := tx.Commit(); err != nil {
		return "", false, fmt.Errorf("recoverykey.Ensure: commit: %w", err)
	}

	if opts.Logger != nil {
		// The plaintext is NOT logged, here or anywhere. The operator obtains
		// it through `memql recovery-key claim`, which is the whole reason this
		// path does not print it.
		opts.Logger.Info("recovery key minted for the cluster owner; claim it with `memql recovery-key claim` -- the plaintext is revealed once and is not in any log",
			"owner_user_id", ownerId,
			"identity_id", identityId,
			"minted_at", at.UTC().Format(time.RFC3339),
			"component", "recoverykey")
	}
	return plain, true, nil
}
