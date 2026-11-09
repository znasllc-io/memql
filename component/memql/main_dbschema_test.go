package memql

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
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
	// Taken after EnsureSchema, which may publish the DSN it settled on.
	environ := os.Environ()
	code := m.Run()
	if leaked := mountedOverlayDomains(); code == 0 && len(leaked) > 0 {
		// A mounted overlay is process state. One left behind makes every engine
		// booted after it load the fixture's constructs, and sends each private
		// engine back to a real boot (bootPrivateEngine) with nothing failing:
		// on 2026-11-02 two of these cost the suite 88s of its 442s.
		fmt.Fprintf(os.Stderr, "tests left these DSL overlays mounted, each needs a t.Cleanup calling memqldsl.UnregisterTree: %v\n", leaked)
		code = 1
	}
	if changed := environmentChangedSince(environ); len(changed) > 0 {
		// The environment is process state too, and every engine reads its
		// configuration from it when built (loadEngineConfigFromEnv). On
		// 2026-11-08 TestLegacyEngineEnvNamesStillConfigure left five engine
		// variables set, so every later engine in the run had a 37-row list
		// cap and a plan test read 37 rows with nothing failing. Reported even
		// when a test failed, since a leaked variable can be why it did.
		fmt.Fprintf(os.Stderr, "tests left these environment variables changed, each needs t.Setenv or a t.Cleanup that restores it: %s\n", strings.Join(changed, ", "))
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

// environmentChangedSince names every variable added, changed or removed
// since `before` was taken from os.Environ. Names only: a value may be a
// credential, and this is printed into CI logs.
func environmentChangedSince(before []string) []string {
	was := make(map[string]string, len(before))
	for _, kv := range before {
		name, value, _ := strings.Cut(kv, "=")
		was[name] = value
	}
	var changed []string
	for _, kv := range os.Environ() {
		name, value, _ := strings.Cut(kv, "=")
		old, existed := was[name]
		delete(was, name)
		switch {
		case !existed:
			changed = append(changed, name+" (added)")
		case old != value:
			changed = append(changed, name+" (changed)")
		}
	}
	for name := range was {
		changed = append(changed, name+" (removed)")
	}
	sort.Strings(changed)
	return changed
}
