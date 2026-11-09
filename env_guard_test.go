package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/envregistry"
)

// TestMain fails the package when its tests leave the process environment
// changed. The subcommand tests run the boot shims (applySubcommandEnv), which
// write the environment with a bare os.Setenv; until 2026-11-09 they left five
// domain derivations set for every later test in this binary.
// component/memql's and component/envregistry's TestMain carry the same guard.
//
// THE SHIMS' INHERITED INPUTS ARE CLEARED FIRST. Every subcommand entry point
// (backup, dsl-fetch, error-pages, migrate, pat, provider-auth, node-token,
// service-account-token) runs applySubcommandEnv, and its layers read the
// environment the test binary inherited: an inherited MEMQL_DOMAIN makes each
// of them derive and set eight variables, and an inherited legacy name makes
// them bridge it. A CI machine carrying its cluster's domain failed this guard
// that way (2026-11-09) while a workstation without one passed. Cleared, a test
// derives only from what it sets itself, with t.Setenv, which is the only
// hermetic test. The repo-root .env the shims also load cannot be cleared from
// here; the failure message says when one is present.
func TestMain(m *testing.M) {
	clearInheritedShimInputs()
	environ := os.Environ()
	code := m.Run()
	if changed := environmentChangedSince(environ); len(changed) > 0 {
		hint := ""
		if _, err := os.Stat(envregistry.LocalEnvFilename); err == nil {
			hint = " (a " + envregistry.LocalEnvFilename + " beside this package is loaded by every subcommand test, through applySubcommandEnv)"
		}
		fmt.Fprintf(os.Stderr, "tests left these environment variables changed, each needs t.Setenv or a t.Cleanup that restores it%s: %s\n", hint, strings.Join(changed, ", "))
		code = 1
	}
	os.Exit(code)
}

// clearInheritedShimInputs unsets MEMQL_DOMAIN and every legacy alias name, the
// inherited variables applySubcommandEnv derives from or bridges. The process
// exits when the tests end, so nothing is restored.
func clearInheritedShimInputs() {
	_ = os.Unsetenv("MEMQL_DOMAIN")
	for _, legacy := range envregistry.LegacyAliases {
		_ = os.Unsetenv(legacy)
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
