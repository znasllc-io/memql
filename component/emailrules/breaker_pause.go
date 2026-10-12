package emailrules

// breaker_pause.go -- a rule whose generated automation a breaker stopped
// says so on its own row (memql#6022).
//
// The authored runtime's breaker pauses an automation after repeated failed
// runs, on every replica and durably: it records `paused` on the BUNDLE row
// (component/memql/authoring_breaker_pause.go). The app reads the RULE row, so
// without this the rule went on reading active while nothing ran. The engine
// tells the listener this package registers (Bind) on the one node that
// recorded the pause, so the rule row is written once.

import (
	"context"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

// BreakerStoppedSentence is what a rule a breaker stopped says on its row:
// what happened and what to do.
const BreakerStoppedSentence = "Stopped after repeated failures. Fix the cause, then turn it on again."

// pauseListenerEngine is an engine that takes a breaker-pause listener. The
// live engine does, through EngineAdapter.
type pauseListenerEngine interface {
	SetAuthoredPauseListener(name string, listener memql.AuthoredPauseListener)
}

// SetAuthoredPauseListener hands the live engine a breaker-pause listener.
func (a EngineAdapter) SetAuthoredPauseListener(name string, listener memql.AuthoredPauseListener) {
	a.Engine.SetAuthoredPauseListener(name, listener)
}

// recordBreakerPause marks the email rule whose bundle a breaker paused:
// status paused, BreakerStoppedSentence, and the paused bundle kept on the row
// -- turning the rule on again supersedes it, and turning it off retires it.
//
// Only a bundle shaped like a rule's is read: every rule bundle id is derived
// from the rule's id and version (bundleIDFor), which is also how the bundle
// is traced back to its rule (Store.RuleOfBundle). The rule is marked only
// while its row is active and names this bundle; a rule that moved on -- an
// edit armed a newer version, or it was turned off -- is left as it is. The
// reads and the write run as the rule's owner -- the bundle's owner, which
// activation requires to be the same person -- and the write with internal
// origin, as every write of the engine's account of a rule does.
func recordBreakerPause(ctx context.Context, engine Engine, owner, bundleID string) error {
	if !strings.HasPrefix(memql.BareShortId(bundleID), ruleBundlePrefix) {
		return nil
	}
	ownerCtx := auth.ContextWithUserActor(ctx, owner)
	store := NewStore(engine)
	ruleID, ok, err := store.RuleOfBundle(ownerCtx, bundleID)
	if err != nil || !ok {
		return err
	}
	state, found, err := store.RuleStateByID(ownerCtx, ruleID)
	if err != nil || !found {
		return err
	}
	if state.Status != "active" || memql.BareShortId(state.BundleID) != memql.BareShortId(bundleID) {
		return nil
	}
	return store.RecordGeneration(auth.ContextWithInternalOrigin(ownerCtx), ruleID, "paused", state.BundleID, state.ConstructName, BreakerStoppedSentence)
}
