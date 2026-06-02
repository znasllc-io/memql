package main

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

// stubDep is a no-op common.Dependency for testing dep-selection logic without
// building or starting the real app.
type stubDep struct{ name common.ComponentName }

func (s stubDep) Start(context.Context)               {}
func (s stubDep) Stop(context.Context)                {}
func (s stubDep) IsRunning() bool                     { return false }
func (s stubDep) Order() int                          { return 0 }
func (s stubDep) ComponentName() common.ComponentName { return s.name }
func (s stubDep) Ready() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

// TestRunPATSubcommand_Help exits 0 for the help flags without building the app
// or touching the database (the switch returns before any bootstrap).
func TestRunPATSubcommand_Help(t *testing.T) {
	for _, arg := range []string{"--help", "-h", "help"} {
		if code := runPATSubcommand([]string{arg}); code != 0 {
			t.Errorf("pat %s: got exit %d, want 0", arg, code)
		}
	}
}

// TestRunPATSubcommand_NoArgs prints usage and exits 2.
func TestRunPATSubcommand_NoArgs(t *testing.T) {
	if code := runPATSubcommand(nil); code != 2 {
		t.Errorf("pat (no args): got exit %d, want 2", code)
	}
}

// TestRunPATSubcommand_Unknown rejects an unknown subcommand with exit 2.
func TestRunPATSubcommand_Unknown(t *testing.T) {
	if code := runPATSubcommand([]string{"bogus"}); code != 2 {
		t.Errorf("pat bogus: got exit %d, want 2", code)
	}
}

// TestRunPATMint_RequiresUserId fails arg validation (exit 2) BEFORE building
// the app, so no database is needed. This pins the "user-id is required" guard.
func TestRunPATMint_RequiresUserId(t *testing.T) {
	if code := runPATMint([]string{"--label", "smoke-test"}); code != 2 {
		t.Errorf("pat mint without --user-id: got exit %d, want 2", code)
	}
	// Routed through the dispatcher with no flags at all.
	if code := runPATSubcommand([]string{"mint"}); code != 2 {
		t.Errorf("pat mint (no flags): got exit %d, want 2", code)
	}
}

// TestRunPATRevoke_RequiresId fails arg validation (exit 2) before any bootstrap.
func TestRunPATRevoke_RequiresId(t *testing.T) {
	if code := runPATRevoke(nil); code != 2 {
		t.Errorf("pat revoke without --id: got exit %d, want 2", code)
	}
}

// TestDepsUpToEngine_StopsAfterEngine pins the #686 bug-1 fix: pat mint starts
// the deps up to AND INCLUDING the engine, and NOT the identity service /
// automations / transport servers that follow it (the identity service would
// fatal-validate and abort the mint).
func TestDepsUpToEngine_StopsAfterEngine(t *testing.T) {
	mk := func(n string) common.Dependency { return stubDep{name: common.ComponentName(n)} }
	all := []common.Dependency{
		mk("config"),
		mk("memoryNodesDB"),
		mk(string(memql.ComponentName)), // the engine
		mk("identityService"),           // must NOT be started
		mk("automations"),               // must NOT be started
		mk("grpcServer"),                // must NOT be started
	}
	got, ok := depsUpToEngine(all)
	require.True(t, ok, "engine must be found")
	require.Len(t, got, 3, "only config + database + engine")
	require.Equal(t, memql.ComponentName, got[len(got)-1].ComponentName(), "engine is last")
	for _, d := range got {
		n := string(d.ComponentName())
		require.NotEqual(t, "identityService", n, "identity service must not start (#686)")
		require.False(t, strings.Contains(strings.ToLower(n), "server"), "no transport server may start")
	}
}

// TestDepsUpToEngine_NoEngine reports not-found when the build has no engine dep.
func TestDepsUpToEngine_NoEngine(t *testing.T) {
	all := []common.Dependency{stubDep{name: "config"}, stubDep{name: "memoryNodesDB"}}
	_, ok := depsUpToEngine(all)
	require.False(t, ok)
}
