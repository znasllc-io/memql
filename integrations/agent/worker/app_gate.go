//go:build agent

package worker

// app_gate.go -- the consent gate in front of every APP SESSION: a step handed
// over through the session door (design D7), and one turn through the chat /
// structured app door. Both front doors ask it, so an app session has one
// consent story whichever way it was reached.
//
// IT IS NOT THE COMPUTER-USE GATE, and that is the owner's decision.
// preDispatchCheck answers "may this AGENT run this command on this user's
// computer": per-task approval, the agent's standing computerUseScope, the
// classifier over the command. An app session is a different act, and its
// consent is two things together:
//
//   - THE MACHINE: the app is in apps.allow of that machine's policy.yaml and
//     somebody signed into it. That is what "allowed and signed in" on the
//     registration reports, what the `app:<id>` routing label means, and what
//     appMachineRefusal asks of the live registration -- which must also be
//     the session owner's own.
//   - A DECISION NAMING THE APP. The router reaches an app door two ways, and
//     only one of them is a decision by construction:
//       - A ROUTING RULE'S CHAIN. The chain names the app because the cluster's
//         routing configuration wrote it there. That configuration is ONE
//         document for the whole cluster (PolicyRegistry.SnapshotRouting has
//         no owner key), not a per-owner policy: what makes the session the
//         owner's is that the door resolves only against their own machines,
//         where their own apps.allow consented to it.
//       - AN EXPLICIT PIN (ResolveRequest.ExplicitProvider). A pin skips every
//         rule, so it is consent only when the person the session runs for
//         made it -- their own request naming the app, their own step
//         override. A pin anybody else made (another user's agent, a prompt
//         author's @defaultProvider, a deploy-time env var) would open this
//         person's machine for a choice they never made, and
//         appDoorPinRefusal refuses it by name. The router says which of the
//         two the door was (memql.AppDoorPin).
//
// Requiring standing scope `full` on top refused every session on a cluster
// where no agent had been granted a shell -- for work the owner had consented
// to on the machine and in the configuration -- so no agentAuthorization row
// is read.
//
// And the one switch the owner can throw from MemQL itself: the computer-use
// kill switch closes app sessions when it is EXPLICITLY engaged and not
// otherwise. A switch that cannot be read is neither, and is refused under its
// own name.

import (
	"context"
	"log/slog"
	"strings"

	memqlengine "github.com/znasllc-io/memql/component/memql"
	workerservice "github.com/znasllc-io/memql/component/worker"
)

// PreferencesReader is the one read the app gate makes. The dispatcher's Store
// satisfies it, so both doors read the switch from the graph the tool path
// reads it from.
type PreferencesReader interface {
	UserPreferences(ctx context.Context, userId string) (Preferences, error)
}

// admitAppSession is the whole gate short of the machine: whether owner may
// open an app session reached through pin, right now. Nil admits.
//
// It is asked BEFORE a machine is selected, and on the session door before a
// child run is opened, so a refused call never reveals which machines the
// owner has online and never leaves anything behind. The pin is asked first
// because it needs no read.
func admitAppSession(ctx context.Context, prefs PreferencesReader, logger *slog.Logger, owner string, pin memqlengine.AppDoorPin) *appGateRefusal {
	if refusal := appDoorPinRefusal(pin, owner); refusal != nil {
		return refusal
	}
	return appSessionConsent(ctx, prefs, logger, owner)
}

// appDoorPinRefusal refuses an app door reached through a pin that owner did
// not make. A rule's chain (the zero pin) is not a pin and is not refused.
func appDoorPinRefusal(pin memqlengine.AppDoorPin, owner string) *appGateRefusal {
	if !pin.Pinned || sameSubject(pin.By, owner) {
		return nil
	}
	by := "no person"
	if strings.TrimSpace(pin.By) != "" {
		by = strings.TrimSpace(pin.By)
	}
	return &appGateRefusal{
		code: AppGateNotNamedByOwner,
		message: "this app was pinned by " + by + ", not by " + strings.TrimSpace(owner) +
			", and an app session opens on " + strings.TrimSpace(owner) + "'s own machine only for a routing rule " +
			"or their own pin",
	}
}

// appSessionConsent is the kill switch: whether owner has switched computer use
// off. Nil admits.
//
// A nil reader admits: it is a node with no store, which has no switch to read
// -- the tool path's preDispatchCheck skips the switch on the same node.
func appSessionConsent(ctx context.Context, prefs PreferencesReader, logger *slog.Logger, owner string) *appGateRefusal {
	if prefs == nil {
		return nil
	}
	p, err := prefs.UserPreferences(ctx, owner)
	if err != nil {
		if logger == nil {
			logger = slog.Default()
		}
		logger.Warn("app gate: user preferences lookup failed; the app session is refused rather than opened past a switch nobody could read",
			"owner_user_id", owner, "error", err)
		return &appGateRefusal{
			code: AppGateKillSwitchUnreadable,
			message: "the computer-use kill switch for this user could not be read (" + err.Error() + "), " +
				"so whether they switched computer use off is unknown",
		}
	}
	if p.KillSwitchEngaged {
		return &appGateRefusal{
			code: AppGateKillSwitchEngaged,
			message: "computer use is switched off for this user (preferences.computerUseEnabled is false), " +
				"which closes app sessions until it is switched back on",
		}
	}
	return nil
}

// appMachineRefusal says why this LIVE registration cannot carry owner's
// session of appId, or "" when it can.
//
// OWNED, NOT SHARED. App sessions have no sharing opt-in: the session's
// back-channel credential names a person, and a machine lent to the cluster
// for model calls was not lent for somebody else's agent to work on. The fleet
// read is already the owner's machines; this asks the live registration too,
// because that is the thing the session actually opens on.
//
// ALLOWED AND SIGNED IN is RunsApp, the same test the `app:` label derivation
// applies, so the router's answer and this one cannot disagree.
func appMachineRefusal(w *workerservice.Worker, owner, appId string) string {
	if w == nil {
		return "its stream is held by another replica"
	}
	if !sameSubject(w.OwnerUserId, owner) {
		return "it is not " + strings.TrimSpace(owner) + "'s machine; an app session runs only on its owner's machines"
	}
	if !w.RunsApp(appId) {
		return appId + " is not allowed and signed in"
	}
	return ""
}
