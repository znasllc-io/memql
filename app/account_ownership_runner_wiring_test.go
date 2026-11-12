package app

// account_ownership_runner_wiring_test.go -- guards the wiring of the account
// ownership runner (component/memql/account_ownership_runner.go).
//
// WHY A SOURCE-LEVEL GUARD, as campaign_digest_sweep_wiring_test.go: nothing in
// the repo calls app.Build(), and standing up a real App needs a database. The
// runner's behaviour is tested where it lives, in component/memql. What only
// this file can lose is the CONNECTION. Delete the construction, or the line
// that registers it as a dependency, and every one of those suites still passes
// while no node ever reads the switch: the engine's flag stays off for good,
// nothing is stamped, nothing is assigned, and nothing says so.

import (
	"os"
	"strings"
	"testing"
)

func TestTheAccountOwnershipRunnerIsBuiltAndStartedWithTheEngine(t *testing.T) {
	src, err := os.ReadFile("engine.go")
	if err != nil {
		t.Fatalf("read engine.go: %v", err)
	}
	body := string(src)

	// The app's own methods, resolved on every tick: the database connects and
	// reconnects, and the node's identity is not known until the cluster phase.
	const build = "memql.NewAccountOwnershipRunner(a.engine, a.BunDB, a.startupNodeID, a.Logger)"
	buildAt := strings.Index(body, build)
	if buildAt < 0 {
		t.Fatal("engineAndBus no longer builds the account ownership runner. Without it no node reads the account-stamping " +
			"switch, so the engine never records the account of a row and existing rows are never assigned. Restore " + build + ".")
	}
	// Built is not started: only a registered dependency is.
	const register = "a.Dependencies = append(a.Dependencies, accountOwnership)"
	registerAt := strings.Index(body, register)
	if registerAt < 0 {
		t.Fatalf("the account ownership runner is built but never registered as a dependency, so its loop never runs. Restore %q.", register)
	}
	if buildAt > registerAt {
		t.Error("the runner is registered before it is built")
	}
	// Registered after the engine: at the engine's own order, the one registered
	// later is stopped first, while the engine it flags is still up.
	const engine = "a.Dependencies = append(a.Dependencies, a.engine)"
	engineAt := strings.Index(body, engine)
	if engineAt < 0 {
		t.Fatalf("could not find %q in engine.go; this guard needs updating", engine)
	}
	if registerAt < engineAt {
		t.Error("the account ownership runner must be registered AFTER the engine, so that it is stopped before it")
	}
}

// engineAndBus runs before the cluster phase gives the node its identity, and
// the runner is built there. What it is handed has to answer "not yet", not
// panic, for as long as that lasts.
func TestTheAccountOwnershipRunnerIsHandedGettersThatAnswerBeforeTheNodeIsUp(t *testing.T) {
	a := &App{}
	if db := a.BunDB(); db != nil {
		t.Errorf("an app with no database handed the runner a handle: %v", db)
	}
	if id := a.startupNodeID(); id != "" {
		t.Errorf("an app with no node identity handed the runner the id %q", id)
	}
}
