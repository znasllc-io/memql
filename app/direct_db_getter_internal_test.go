package app

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/database"
	memoryNodesDatabase "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// newAppWithDB builds a minimal App whose db carries distinct main (pooled) and
// direct (non-pooled) bun handles, so a test can tell which endpoint the
// coordination-lock wiring resolves. It does NOT stand up the dependency graph.
func newAppWithDB(mainBun, directBun *bun.DB) *App {
	return &App{
		db: &memoryNodesDatabase.MemoryNodesDatabase{
			TimescaleDBDatabase: &database.TimescaleDBDatabase{
				PostgresDatabase: &database.PostgresDatabase{
					Database: &database.Database{Bun: mainBun, DirectBun: directBun},
				},
			},
		},
	}
}

// TestDirectDBGetter_ResolvesDirectEndpoint is the memql#1930 wiring guard: the
// getter handed to the session-scoped, lifetime-held leader-election locks
// (CronLeader in app/engine.go, TopologyReconciler in app/cluster.go) MUST
// resolve the DIRECT (non-pooled) endpoint, not the pooled main pool -- a
// transaction-mode PgBouncer recycles the server backend between statements and
// would drop a held advisory lock. Both call sites use a.directDBGetter(), so
// this exercises the single source of truth for both.
func TestDirectDBGetter_ResolvesDirectEndpoint(t *testing.T) {
	mainBun := &bun.DB{}
	directBun := &bun.DB{}
	a := newAppWithDB(mainBun, directBun)

	got := a.directDBGetter()()

	assert.Same(t, directBun, got, "leader locks must resolve the DIRECT endpoint")
	assert.NotSame(t, mainBun, got, "leader locks must NOT resolve the pooled main pool")
}

// TestDirectDBGetter_FallsBackToMainWhenNoDirect asserts the env-agnostic
// fallback (DIRECT_DSN unset -> behavior unchanged): when no direct handle is
// configured, the getter resolves the main pool, so local/dev without a pooler
// keeps working exactly as before.
func TestDirectDBGetter_FallsBackToMainWhenNoDirect(t *testing.T) {
	mainBun := &bun.DB{}
	a := newAppWithDB(mainBun, nil)

	assert.Same(t, mainBun, a.directDBGetter()(), "with no DIRECT_DSN, the getter falls back to the main pool")
}

// TestDirectDBGetter_NilDBYieldsNil asserts the getter tolerates a not-yet-wired
// database (DB resolved lazily by the leader's poll loop) by returning nil
// rather than panicking.
func TestDirectDBGetter_NilDBYieldsNil(t *testing.T) {
	a := &App{}
	assert.Nil(t, a.directDBGetter()(), "nil db -> nil handle (lazy resolution, no panic)")
}
