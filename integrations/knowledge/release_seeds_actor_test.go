package knowledge

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/common"
)

// A run of the release reconcile is the engine's own automation's, as the
// automation executor minted its actor. An actor spelled with its id, and the
// actor an automation of another source with its name runs as, are refused
// before anything is read.
func TestReleaseKnowledgeRunsOnlyAsTheEnginesOwnAutomation(t *testing.T) {
	i := New(nil)
	run := common.RunContext{RunId: "run", Mode: common.RunModeLive}
	at := func(ac *auth.AccessContext) context.Context {
		return auth.ContextWithInternalOrigin(common.ContextWithRun(auth.ContextWithAccess(context.Background(), ac), run))
	}
	for name, ac := range map[string]*auth.AccessContext{
		"an actor built with its id":   {UserId: "system:automation:reconcileReleaseKnowledge", Role: auth.RoleReader, Synthetic: true, Unranked: true},
		"another source with its name": auth.AutomationActor(auth.AutomationOriginOther, "reconcileReleaseKnowledge"),
		"a system actor spelled so":    auth.SystemActor("automation:reconcileReleaseKnowledge"),
	} {
		if err := i.ReconcileReleaseSeeds(at(ac), false); err == nil || !strings.Contains(err.Error(), "server lifecycle operation") {
			t.Errorf("%s: want the lifecycle refusal, got %v", name, err)
		}
	}
	// The engine's own automation passes this check; with no engine wired the
	// next one refuses, which is what shows the first was cleared.
	if err := i.ReconcileReleaseSeeds(at(auth.AutomationActor(auth.AutomationOriginEngine, "reconcileReleaseKnowledge")), false); err == nil || strings.Contains(err.Error(), "server lifecycle operation") {
		t.Errorf("the engine's own automation was refused as a lifecycle operation: %v", err)
	}
}
