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
	"errors"
	"fmt"
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
// is traced back to its rule (Store.RuleOfBundle). Everything it acts on is
// read fresh -- the listener runs after the pause went out, and the rule or
// the bundle may have moved on since:
//
//   - the bundle must still read paused: one turned on again since is not
//     this pause's to mark;
//   - the rule is marked only while its row reads active and names this
//     bundle: a rule that moved on -- an edit armed a newer version, or it was
//     turned off -- is left as it is;
//   - the write is conditional on the version of the row read (a
//     compare-and-set). Another write landing in between -- a firing's count,
//     an edit -- makes it fail with memql.ErrRowVersionChanged, and the row is
//     read and decided again.
//
// The reads and the write run as the rule's owner -- the bundle's owner, which
// activation requires to be the same person -- and the write with internal
// origin, as every write of the engine's account of a rule does.
func recordBreakerPause(ctx context.Context, engine Engine, owner, bundleID string) error {
	if !strings.HasPrefix(memql.BareShortId(bundleID), ruleBundlePrefix) {
		return nil
	}
	ownerCtx := auth.ContextWithUserActor(ctx, owner)
	store := NewStore(engine)
	ruleID, status, ok, err := store.RuleOfBundle(memql.ContextWithFreshRead(ownerCtx), bundleID)
	if err != nil || !ok || status != "paused" {
		return err
	}
	for attempt := 0; attempt < recordAttempts; attempt++ {
		state, found, err := store.RuleStateByID(memql.ContextWithFreshRead(ownerCtx), ruleID)
		if err != nil || !found {
			return err
		}
		if state.Status != "active" || !sameBundle(state.BundleID, bundleID) {
			return nil
		}
		err = store.RecordGeneration(auth.ContextWithInternalOrigin(fencedOnRule(ownerCtx, ruleID, state.VersionAt)),
			ruleID, "paused", state.BundleID, state.ConstructName, BreakerStoppedSentence)
		if !errors.Is(err, memql.ErrRowVersionChanged) {
			return err
		}
	}
	return fmt.Errorf("emailrules: rule %q kept changing, so the breaker's pause of %q was not recorded on it", ruleID, bundleID)
}
