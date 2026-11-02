package memql

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/znasllc-io/memql/component/database/dbtest"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// TestMain guarantees the shared test schema is fully migrated before any
// Postgres-gated test in this package runs, serialized against the other
// db-gated packages CI runs in the same lane. Without it, these *_db_test.go
// suites raced examples/referencepack's migration on the shared DB and
// intermittently failed with `relation "MemoryNodes" does not exist`
// (memql#2551). When no DB is reachable EnsureSchema is a no-op and the
// individual tests self-skip.
func TestMain(m *testing.M) {
	if _, err := dbtest.EnsureSchema(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "dbtest.EnsureSchema: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	if leaked := mountedOverlayDomains(); code == 0 && len(leaked) > 0 {
		// A mounted overlay is process state. One left behind makes every engine
		// booted after it load the fixture's constructs, and sends each private
		// engine back to a real boot (bootPrivateEngine) with nothing failing:
		// on 2026-11-02 two of these cost the suite 88s of its 442s.
		fmt.Fprintf(os.Stderr, "tests left these DSL overlays mounted, each needs a t.Cleanup calling memqldsl.UnregisterTree: %v\n", leaked)
		code = 1
	}
	os.Exit(code)
}

// mountedOverlayDomains names the domains that came from an overlay rather than
// the embedded tree.
func mountedOverlayDomains() []string {
	var names []string
	for _, domain := range memqldsl.ListPackDomains() {
		if domain.Origin != "embedded" {
			names = append(names, domain.Name)
		}
	}
	return names
}
