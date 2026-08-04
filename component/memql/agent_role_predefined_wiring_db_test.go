package memql

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// The wiring test memql#2985 shipped without, and the adversarial review of
// memql#3061 found missing.
//
// Every other case in agent_role_predefined_validation_test.go calls
// validateAgentRolePredefinedLock DIRECTLY. That proves the predicate and
// nothing about whether anything invokes it -- deleting the call site in
// executeWrite left the entire suite green, measured. A correct validator that
// nothing calls is the same silent-absence shape this repo keeps finding
// (memql#2965, #2971, #2972), and it is worse here because the artefact it
// would leave behind is a security guard everyone believes is on.
//
// So this drives the REAL engine: it seeds a predefined catalog row as the
// SeedMaterializer does, then attempts the edit a user would make, and
// requires the write to be refused. It fails if the guard is unhooked, if the
// concept branch is misspelled, or if the hook moves above the read-merge that
// supplies priorPredefined.
//
// Postgres-gated, like its neighbours: skips when no DB is reachable.
func TestAgentRolePredefinedLockIsWiredIntoExecuteWrite(t *testing.T) {
	eng, _, sysCtx := readMergeTestEngine(t)

	slug := "wiring-guard-" + uniqueSuffix("role")

	// Seeded by a system actor, exactly as the SeedMaterializer does. That this
	// succeeds is half the assertion: a guard that rejected the system actor
	// would fail boot on the first re-seed.
	runMutation(t, sysCtx, eng, "createAgentRole", map[string]any{
		"slug":       slug,
		"name":       "Wiring Guard Role",
		"predefined": true,
	})

	// The same write a cockpit user would issue. `createAgentRole` is the only
	// mutation bound to this concept, and it restates every field with `??`
	// defaults, so this is genuinely what an edit looks like on the wire.
	userCtx := auth.ContextWithToken(context.Background(),
		&auth.TokenInfo{Subject: "user-wiring-guard"})

	_, err := eng.Execute(userCtx,
		`mutation createAgentRole(slug: "`+slug+`", name: "Renamed By A User")`)
	if err == nil {
		t.Fatal("a non-system actor renamed a predefined agent-role catalog row through the real " +
			"engine. The guard is not reached: either the executeWrite hook is gone, the concept " +
			"branch does not match, or priorPredefined is not populated for this concept.")
	}
	for _, want := range []string{"v1:agents:agentRole", "locked", slug} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the rejection came from somewhere other than the predefined-lock guard "+
				"(missing %q): %v", want, err)
		}
	}
}
