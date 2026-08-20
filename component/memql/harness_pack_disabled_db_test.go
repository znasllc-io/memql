package memql

// harness_pack_disabled_db_test.go -- the memql#4190 both-states proof: a
// REAL engine (same New + Init path app.Run runs, real Postgres) boots
// GREEN with the harness pack in the disabled set, and what survives is
// exactly the mounted-inert contract from the module-registry design
// section 4.2:
//
//   - the harness CONCEPTS are registered (schemas, relationship targets,
//     and existing rows keep resolving -- including the one hard
//     cross-domain edge, dsl/actions importing harness plan + step);
//   - every harness BEHAVIORAL construct is absent from the registries:
//     the recall/harnessTrace builtins, the eleven mutations, the
//     consolidateMemory logic and prompt;
//   - the namespace stays OWNED: a second RegisterTree("harness") is
//     refused exactly as when enabled.
//
// The enabled state needs no twin boot here: every other db-gated test in
// this package boots the shared engine with the default (empty) disabled
// set, and the positive control below proves the same probes light up
// there -- an absent-when-disabled assertion is only evidence if the same
// instrument reads present-when-enabled.
//
// PRIVATE engine boot, not the shared borrow: SetDisabledPackDomains is
// process-global state a shared engine must never inherit (see the
// engine-MUTATING vs engine-BORROWING split documented on
// readMergeTestEngine). The cleanup restores the empty set FIRST
// (t.Cleanup is LIFO relative to registration order here) so no later
// borrower sees a disabled harness.

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/database/dbtest"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// harnessBehavioralProbes are constructs that must be PRESENT enabled and
// ABSENT disabled -- one of each behavioral kind the harness carries.
var harnessBehavioralProbes = struct {
	functions []string // mutations + logic + builtins land in FunctionRegistry
	prompts   []string
}{
	functions: []string{
		"createHarnessPlan", "addHarnessStep", "recordHarnessObservation",
		"recall", "harnessTrace", // the two Go-backed builtins
		"consolidateMemory", // the logic
	},
	prompts: []string{"consolidateMemory", "decomposeGoal"},
}

func TestHarnessPackMountedInertWhenDisabled(t *testing.T) {
	dsn := dbtest.DSN()
	connector := pgdriver.NewConnector(pgdriver.WithDSN(dsn))
	db := bun.NewDB(sql.OpenDB(connector), pgdialect.New())
	if err := db.PingContext(context.Background()); err != nil {
		dbtest.Unreachable(t, "harness pack disabled boot test", dsn, err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// POSITIVE CONTROL first, against the default all-enabled state: the
	// probes must be reachable through the exact instrument the disabled
	// half reads, or its silence proves nothing.
	memqldsl.SetDisabledPackDomains(nil)
	t.Cleanup(func() { memqldsl.SetDisabledPackDomains(nil) })

	enabledEng := bootHarnessTestEngine(t, db)
	enabledFns := enabledEng.Functions().LookupIndex()
	for _, name := range harnessBehavioralProbes.functions {
		if _, ok := enabledFns[name]; !ok {
			t.Fatalf("positive control: %q absent from an ENABLED-harness engine; the probe cannot detect the disable", name)
		}
	}
	for _, name := range harnessBehavioralProbes.prompts {
		if _, ok := enabledEng.Prompts().Get(name); !ok {
			t.Fatalf("positive control: prompt %q absent from an ENABLED-harness engine", name)
		}
	}

	// THE DISABLED BOOT.
	memqldsl.SetDisabledPackDomains([]string{memqldsl.HarnessPackDomain})

	eng := bootHarnessTestEngine(t, db)

	// Concepts: still registered, and the actions edge still resolves --
	// Init returning nil above already proved strict boot passed with
	// dsl/actions' `use harness.concepts.{ plan, step }` in the corpus.
	registry := concept.DefaultRegistry()
	for _, name := range []string{
		"v1:harness:plan", "v1:harness:step", "v1:harness:observation",
		"v1:harness:semanticMemory", "v1:harness:consolidationCursor",
		"v1:actions:action",
	} {
		if _, err := registry.Get(name); err != nil {
			t.Errorf("mounted-inert: concept %q must stay registered with the harness disabled: %v", name, err)
		}
	}

	// Behavioral constructs: absent from every registry.
	fns := eng.Functions().LookupIndex()
	for _, name := range harnessBehavioralProbes.functions {
		if _, ok := fns[name]; ok {
			t.Errorf("disabled harness still registered function/builtin/logic %q", name)
		}
	}
	for _, name := range harnessBehavioralProbes.prompts {
		if _, ok := eng.Prompts().Get(name); ok {
			t.Errorf("disabled harness still registered prompt %q", name)
		}
	}

	// The namespace stays owned: a colliding registration is refused
	// exactly as when enabled. Disablement never frees a namespace.
	func() {
		defer func() {
			if recover() == nil {
				t.Errorf("RegisterTree(%q) on a DISABLED pack must still panic on collision", memqldsl.HarnessPackDomain)
			}
		}()
		memqldsl.RegisterTree(memqldsl.HarnessPackDomain, testPackTree(t))
	}()
}

// bootHarnessTestEngine is the minimal private boot (see the
// engine-MUTATING rationale on readMergeTestEngine; this file mutates the
// process-global disabled set, so it cannot borrow the shared engine).
func bootHarnessTestEngine(t *testing.T, db *bun.DB) *MemQLEngine {
	t.Helper()
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	registry := concept.DefaultRegistry()
	eng, err := New(db)
	require.NoError(t, err)
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	require.NoError(t, eng.Init(registry))
	return eng
}
