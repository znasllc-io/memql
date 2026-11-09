package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
)

// TestMain fails the package when its tests leave the process environment
// changed. The subcommand tests run the boot shims (applySubcommandEnv), which
// write the environment with a bare os.Setenv; until 2026-11-09 they left five
// domain derivations set for every later test in this binary.
// component/memql's and component/envregistry's TestMain carry the same guard.
func TestMain(m *testing.M) {
	environ := os.Environ()
	code := m.Run()
	if changed := environmentChangedSince(environ); len(changed) > 0 {
		fmt.Fprintf(os.Stderr, "tests left these environment variables changed, each needs t.Setenv or a t.Cleanup that restores it: %s\n", strings.Join(changed, ", "))
		code = 1
	}
	os.Exit(code)
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
