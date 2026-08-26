package memql

import (
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// THE OTHER HALF OF memql#4075's SPLIT (memql#4569).
//
// memql#4075 moved 83 of 103 per-test engine boots onto one package-shared
// engine and took this package from 504.7s to ~322s. It shared the DB-GATED
// boot. The DB-LESS boot -- `LoadUnifiedConcepts(nil)` + `New(nil)` +
// `Init(registry)`, over the embedded tree, for a test that only wants to ASK
// the engine something -- kept booting privately, and there are now a hundred
// of those.
//
// MEASURED, on the machine and database that produced this file:
//
//	go test -count=1 -v ./component/memql/   ->  268.3s, 1974 top-level tests
//	  119 tests at >= 1.0s                   ->  215.4s  (80% of the package)
//	  102 of them in the 1-2s band           ->  164.1s  (61% of the package)
//	  1745 tests at < 0.1s                   ->   10.2s  ( 4% of the package)
//
//	BenchmarkDblessEngineInit                ->  1.428s/op
//
// So the 1-2s band IS the db-less boot, one per test, and it is the single
// growth driver -- the same shape memql#4075 found, in the half it did not
// cover. (The tree parse alone is 24.75ms; it is Init, not the load, that
// costs.)
//
// WHAT MAY BORROW THIS, AND WHAT MAY NOT. Exactly the rule sharedReadMergeEngine
// states: a test that only READS -- resolves a construct, inspects the
// registry, classifies a verb, asks what a relationship traverses to -- may
// borrow. A test that MUTATES engine or registry state must keep booting
// privately, and several deliberately do:
//
//   - lint_parity_test.go mounts overlay domains and restores the registry,
//   - strict_boot_test.go loads deliberately-broken fixtures,
//   - construct_catalog_test.go mounts a fixture pack,
//   - contract_gates_3629_test.go feeds Init a violating bundle.
//
// Each of those is a test ABOUT booting, so sharing one would remove the thing
// under test. They are the twenty that stayed private in memql#4075, for the
// same reason.
//
// NO DATABASE, DELIBERATELY. This engine is `New(nil)`. Nothing that borrows it
// touches a row, so it neither skips without a database nor consumes a
// connection -- which is also why these tests are not a db-gated concern at all
// and their cost sitting in the db-gated lane's budget was itself the
// misattribution.
var sharedDblessEngineState struct {
	once sync.Once
	// boots counts Once-body executions so the sharing can be asserted as a
	// NUMBER rather than inferred from wall time, exactly as its db sibling does.
	boots   int
	eng     *MemQLEngine
	bootErr error
}

// sharedDblessEngine returns the package-wide db-less engine, booting it on
// first use.
//
// LAZY, and the verdict is re-reported per caller: the Once body never touches
// a *testing.T, so a boot failure fails EVERY borrower with the same message
// rather than letting the first caller's t answer for the package and handing
// everyone after it a nil engine.
func sharedDblessEngine(t *testing.T) *MemQLEngine {
	t.Helper()
	s := &sharedDblessEngineState
	s.once.Do(func() {
		s.boots++
		if _, err := LoadUnifiedConcepts(nil); err != nil {
			s.bootErr = fmt.Errorf("LoadUnifiedConcepts: %w", err)
			return
		}
		registry := concept.DefaultRegistry()
		if registry == nil || len(registry.List()) == 0 {
			s.bootErr = fmt.Errorf("concept registry empty after LoadUnifiedConcepts")
			return
		}
		eng, err := New(nil)
		if err != nil {
			s.bootErr = fmt.Errorf("New(nil): %w", err)
			return
		}
		// The provider loader WARNs once per provider with no DB or secrets,
		// which is not what any borrower checks.
		eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
		if err := eng.Init(registry); err != nil {
			s.bootErr = fmt.Errorf("Init: %w", err)
			return
		}
		s.eng = eng
	})
	if s.bootErr != nil {
		t.Fatalf("shared db-less engine did not boot: %v", s.bootErr)
	}
	return s.eng
}

// TestSharedDblessEngine_SharesOneBoot is the gate that keeps the saving.
//
// Asserted as a COUNT, not as wall time: a regression that reintroduced a boot
// per borrower would still pass a duration threshold on a fast machine and
// would only show up as CI going red months later, which is the shape this
// whole issue is about (memql#3257, three times).
func TestSharedDblessEngine_SharesOneBoot(t *testing.T) {
	for i := 0; i < 3; i++ {
		if eng := sharedDblessEngine(t); eng == nil {
			t.Fatal("shared db-less engine is nil")
		}
	}
	if got := sharedDblessEngineState.boots; got != 1 {
		t.Errorf("the shared db-less engine booted %d times, want exactly 1. Each boot is ~1.4s "+
			"(BenchmarkDblessEngineInit), and a hundred of them is 61%% of this package's "+
			"runtime (memql#4569).", got)
	}
}
