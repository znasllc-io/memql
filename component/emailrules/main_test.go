package emailrules

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/memql"
)

// TestMain loads the tree's concepts before any test runs, because Gate 1
// compiles the generated automation FOR REAL in this test binary.
//
// It did not always. The sandbox reaches the automation compiler through a
// hook component/automations registers from init(), and a binary that does
// not link that package leaves the hook nil -- the sandbox then SKIPS the
// automation kind and reports the bundle OK. This package's tests never
// imported component/automations, so every "the generated construct
// compiles" assertion here passed without compiling anything, while the
// generated call was one the real parser refused (its arguments carried no
// commas). generate_v1_test.go links the package, which makes the gate real;
// a real gate resolves the trigger's concept against the core registry, so the
// registry must hold the tree's concepts.
//
// It also migrates the shared test database before any db-gated case runs
// (memql#2551), which is the precondition for this package being in the
// db-tests lane at all: the fired count is a claim about what several
// replicas' writes leave in the table (memql#5431), and a fake engine
// serialises because the fake serialises. EnsureSchema answers (false, nil)
// when no Postgres is reachable, so the db-gated cases self-skip, and fail
// under MEMQL_REQUIRE_DB=1, exactly as every other db-gated package does.
func TestMain(m *testing.M) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := memql.LoadUnifiedConcepts(quiet); err != nil {
		fmt.Fprintf(os.Stderr, "load the tree's concepts: %v\n", err)
		os.Exit(1)
	}
	if _, err := dbtest.EnsureSchema(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "dbtest.EnsureSchema: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
