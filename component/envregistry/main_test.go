package envregistry

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
)

// TestMain fails the package when its tests leave the process environment
// changed.
//
// This package's subjects are the two boot shims that write the environment
// with a bare os.Setenv, ApplyLegacyEnvAliases and ApplyDomainDerivations. A
// test that restores only the names it set leaves the shim's other writes
// behind, and every later test in the binary runs against them: until
// 2026-11-09 these tests left eight variables set, MEMQL_EMAIL_SENDER and
// seven domain derivations. component/memql's TestMain carries the same guard.
func TestMain(m *testing.M) {
	environ := os.Environ()
	code := m.Run()
	if changed := environmentChangedSince(environ); len(changed) > 0 {
		fmt.Fprintf(os.Stderr, "tests left these environment variables changed, each needs t.Setenv or a t.Cleanup that restores it: %s\n", strings.Join(changed, ", "))
		code = 1
	}
	os.Exit(code)
}

// preserveShimTargets restores, when t ends, every variable the two boot shims
// can write: each LegacyAliases target and each name DomainDerivations fills.
// Call it before the shim in every test that runs one. t.Setenv restores only
// the names a test hands it, while each shim writes every name it finds to
// fill, so restoring the names a case sets is not enough.
func preserveShimTargets(t *testing.T) {
	t.Helper()
	derived := DomainDerivations("memql.localhost")
	if len(derived) == 0 {
		t.Fatal("DomainDerivations named no variables, so the derived names would go unrestored")
	}
	names := make(map[string]bool, len(LegacyAliases)+len(derived))
	for name := range LegacyAliases {
		names[name] = true
	}
	for name := range derived {
		names[name] = true
	}
	for name := range names {
		value, set := os.LookupEnv(name)
		t.Setenv(name, value) // registers the restore
		if !set {
			// Unset meanwhile, as it was before the test.
			if err := os.Unsetenv(name); err != nil {
				t.Fatalf("unset %s: %v", name, err)
			}
		}
	}
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
