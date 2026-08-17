package recoverykey

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/database/dbtest"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/identity"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// invariant_real_engine_db_test.go -- the mint invariant against a REAL
// engine, because a fake one cannot refuse anything.
//
// # Why this exists
//
// This package already had a Postgres-gated test of the invariant
// (mint_singleflight_db_test.go). It passed throughout, and the invariant had
// NEVER successfully minted a key on any cluster. Two independent, total
// defects sat underneath it:
//
//  1. Every construct the store issues is @serverOnly, and the store reached
//     the engine on an unstamped context -- so the invariant's read was
//     refused with `function "activeRecoveryKeys" is server-only and cannot be
//     called by a client`.
//  2. `recovery_key` is a machineCredentialIdentityType, so the memql#2513
//     guard admits the WRITE only from a system actor -- and every recovery-key
//     write path stamped identity.ContextWithSystemActor, which is not one. It
//     sets role="owner" and an email, and auth.ActorFromToken PREFERS email
//     over subject, so the actor resolved to "system@identity.memql.local":
//     neither role=="system" nor prefixed "system:". Both arms of isSystemActor
//     fail. ContextWithSystemCredentialActor is the actor that exists for this,
//     and its own doc comment names the guard.
//
// The existing test could see NEITHER, and not by oversight: it fakes the
// engine deliberately, because it tests a Postgres advisory lock and a fake is
// what widens the race window enough to observe. A fake engine has no
// @serverOnly gate and no credential-actor guard, so it answers both calls
// happily. Bug (2) was invisible even after (1) was fixed -- fixing the read
// simply moved the failure one line down, onto the write.
//
// So this is the complement, not a duplicate: same invariant, real engine, real
// gates, no lock race. Run both.
//
// # What it drives
//
// The production context, exactly: app/integrations_identity.go's
// ensureOwnerRecoveryKey calls EnsureForAllOwners with
// identity.ContextWithSystemCredentialActor. Passing anything else here would
// test a path nothing runs.
//
// The one thing faked is the OWNER RESOLVER, and it is a one-method interface
// the package defines precisely so it need not import its parent. Owner
// resolution was never implicated; everything the two defects touched -- the
// engine, the store, the gate, the guard, the database -- is real.
func TestInvariantMintsAgainstARealEngine(t *testing.T) {
	eng, rawDB := realEngine(t)
	store := &Store{Engine: eng, Logger: discardLogger()}

	// A distinct owner per run: MemoryNodes is append-only, so a fixed id
	// would find the previous run's key and report "already present", turning
	// the mint assertion into a no-op after the first ever execution.
	owner := "v1:identity:user:rk-invariant-" + time.Now().UTC().Format("20060102T150405.000000000")

	opts := EnsureOptions{
		DB:    func() *sql.DB { return rawDB.DB },
		Store: store,
		// fixedOwners is this package's existing OwnerResolver stub
		// (mint_singleflight_db_test.go). See the header for why this one
		// thing is faked and nothing else is.
		Owners:   fixedOwners{owner},
		MintedBy: "system:identity-svc",
		Logger:   discardLogger(),
	}

	// The production context. See the header.
	ctx := identity.ContextWithSystemCredentialActor(context.Background())

	t.Run("first evaluation mints", func(t *testing.T) {
		res, err := EnsureForAllOwners(ctx, opts)
		if err != nil {
			t.Fatalf("the invariant did not complete, so this cluster would boot with no "+
				"break-glass route for its owner -- the exact failure that reached production "+
				"as one WARN per boot: %v", err)
		}
		if res.Minted != 1 {
			t.Fatalf("Minted = %d, want 1. The invariant ran without error and produced no key, "+
				"which is the silent half of the same failure", res.Minted)
		}
		if res.Plain[owner] == "" {
			t.Error("no plaintext returned for the minted key. The boot caller discards it, but " +
				"`memql recovery-key claim` is the caller that needs it")
		}
		if !IsRecoveryKey(res.Plain[owner]) {
			t.Errorf("minted plaintext is not a recovery key (wrong prefix); redeem would reject it "+
				"before reaching the database: %q", redact(res.Plain[owner]))
		}
	})

	t.Run("the key is readable back, which is the half the @serverOnly gate broke", func(t *testing.T) {
		live, err := store.ActiveForUser(ctx, owner)
		if err != nil {
			t.Fatalf("ActiveForUser: %v", err)
		}
		if len(live) != 1 {
			t.Fatalf("ActiveForUser returned %d live keys, want 1. The invariant reads through this "+
				"to decide whether to mint, so a wrong answer here mints a duplicate owner "+
				"credential on every boot", len(live))
		}
		if live[0].IsClaimed() {
			t.Error("a freshly minted key must be UNCLAIMED -- claiming is what makes it " +
				"non-re-mintable, and a key born claimed can never be handed to an operator")
		}
	})

	t.Run("a second evaluation is a no-op", func(t *testing.T) {
		// Idempotence is what makes this safe to run on every boot. Without
		// it, every restart mints another live owner-equivalent credential.
		res, err := EnsureForAllOwners(ctx, opts)
		if err != nil {
			t.Fatalf("re-evaluation: %v", err)
		}
		if res.Minted != 0 {
			t.Errorf("Minted = %d on re-evaluation, want 0 -- every restart would add another live "+
				"owner-equivalent credential", res.Minted)
		}
		if len(res.AlreadyPresent) != 1 || res.AlreadyPresent[0] != owner {
			t.Errorf("AlreadyPresent = %v, want [%s]", res.AlreadyPresent, owner)
		}
	})
}

// TestInvariantWithNoOwnerIsASuccessPath pins the branch most at risk of being
// "fixed" into an error.
//
// A fresh cluster has no owner until its first sign-in, so EVERY boot before
// the cluster is claimed lands here. It must be a quiet success: returning an
// error would make the identity node log a scary warning on every boot of every
// unclaimed cluster, and anything that treated it as a failure-to-mint would be
// wrong in the other direction -- there is no owner to bind a key to, so
// minting one is not possible, not merely skipped.
//
// The CLI has the matching branch (`no owner yet` -> recoveryKeyState=awaitingOwner,
// exit 0), which the installer's recovery-key step already handles as a
// non-failure.
func TestInvariantWithNoOwnerIsASuccessPath(t *testing.T) {
	eng, rawDB := realEngine(t)

	res, err := EnsureForAllOwners(
		identity.ContextWithSystemCredentialActor(context.Background()),
		EnsureOptions{
			DB:       func() *sql.DB { return rawDB.DB },
			Store:    &Store{Engine: eng, Logger: discardLogger()},
			Owners:   fixedOwners{}, // an unclaimed cluster
			MintedBy: "system:identity-svc",
			Logger:   discardLogger(),
		})
	if err != nil {
		t.Fatalf("no owner must be a SUCCESS, not an error -- this is the state of every cluster "+
			"between first boot and first sign-in: %v", err)
	}
	if res.Minted != 0 {
		t.Errorf("Minted = %d with no owner, want 0. A key must be bound to an owner; minting one "+
			"unbound would create a credential that authorizes nothing and can never be redeemed",
			res.Minted)
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// redact keeps a failure message from printing a live credential. A test that
// leaks the thing under test into CI logs is its own defect.
func redact(plain string) string {
	if len(plain) <= 8 {
		return "<short>"
	}
	return plain[:8] + "<redacted>"
}

// realEngine builds an engine over the test database. Mirrors component/memql's
// readMergeTestEngine; kept local because that helper is unexported and this
// package cannot be imported from there (component/memql is one of this
// package's own dependencies).
func realEngine(t *testing.T) (*memqlengine.MemQLEngine, *bun.DB) {
	t.Helper()
	dsn := dbtest.DSN()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	if err := db.PingContext(context.Background()); err != nil {
		dbtest.Unreachable(t, "recovery-key invariant real-engine DB test", dsn, err)
	}
	// Close with the test: every helper that leaks a pool holds database/sql's
	// two idle connections for the rest of the package run, and the ceiling is
	// a stock max_connections of 100 (see readMergeTestEngine's note).
	t.Cleanup(func() { _ = db.Close() })

	if _, err := memqlengine.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	eng, err := memqlengine.New(db)
	if err != nil {
		t.Fatalf("engine New: %v", err)
	}
	eng.Logger = discardLogger()
	if err := eng.Init(concept.DefaultRegistry()); err != nil {
		t.Fatalf("engine Init: %v", err)
	}
	return eng, db
}
