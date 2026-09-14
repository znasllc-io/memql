package emailrules

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"

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
func TestMain(m *testing.M) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := memql.LoadUnifiedConcepts(quiet); err != nil {
		fmt.Fprintf(os.Stderr, "load the tree's concepts: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
