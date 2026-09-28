//go:build agent

package worker

// app_gate.go -- the consent gate in front of every APP SESSION: a step handed
// over through the session door (design D7), a delegated Task, and one turn
// through the chat / structured app door. Both front doors ask it, so an app
// session has one consent story whichever way it was reached.
//
// IT IS NOT THE COMPUTER-USE GATE, and that is the owner's decision.
// preDispatchCheck answers "may this AGENT run this command on this user's
// computer": per-task approval, the agent's standing computerUseScope, the
// classifier over the command. An app session is a different act, and its
// consent is given in two places the owner already controls:
//
//   - THE MACHINE: the app is in apps.allow of that machine's policy.yaml and
//     somebody signed into it. That is what "allowed and signed in" on the
//     registration reports, and what the `app:<id>` routing label means.
//   - THE OWNER'S POLICY: their routing policy names `app:<id>` (the session
//     and chat doors), or their delegation policy lets a Task kind go to an
//     app. That is decided before a call gets here -- the router never
//     resolves an app door a policy does not name.
//
// Requiring standing scope `full` on top refused every session on a cluster
// where no agent had been granted a shell -- for work the owner had consented
// to on the machine and in the policy -- so no agentAuthorization row is read.
//
// What this file adds is the one switch the owner can throw from MemQL itself:
// the computer-use kill switch. It closes app sessions when it is EXPLICITLY
// engaged and not otherwise.

import (
	"context"
	"log/slog"
	"strings"

	workerservice "github.com/znasllc-io/memql/component/worker"
)

// AppGateKillSwitchEngaged is the refusal code when the owner's computer-use
// kill switch is engaged. The same code the tool path uses, because it is the
// same switch.
const AppGateKillSwitchEngaged = "kill_switch_engaged"

// PreferencesReader is the one read the app gate makes. The dispatcher's Store
// satisfies it, so both doors read the switch from the graph the tool path
// reads it from.
type PreferencesReader interface {
	UserPreferences(ctx context.Context, userId string) (Preferences, error)
}

// appGateRefusal is a named refusal from the app gate.
type appGateRefusal struct {
	Code    string
	Message string
}

func (r *appGateRefusal) Error() string { return r.Code + ": " + r.Message }

// appSessionConsent decides whether owner may open an app session right now.
// Nil admits.
//
// It is asked BEFORE a machine is selected, so a refused call never reveals
// which machines the owner has online, and never opens anything on one.
//
// A preference read that FAILS admits, with a warning. The switch closes app
// sessions only when the owner engaged it, and a read that failed says nothing
// about what they did; refusing on it would turn a database hiccup into "you
// switched computer use off", which they did not.
func appSessionConsent(ctx context.Context, prefs PreferencesReader, logger *slog.Logger, owner string) *appGateRefusal {
	if prefs == nil {
		return nil
	}
	p, err := prefs.UserPreferences(ctx, owner)
	if err != nil {
		if logger == nil {
			logger = slog.Default()
		}
		logger.Warn("app gate: user preferences lookup failed; the kill switch reads as not engaged",
			"owner_user_id", owner, "error", err)
		return nil
	}
	if p.KillSwitchEngaged {
		return &appGateRefusal{
			Code: AppGateKillSwitchEngaged,
			Message: "computer use is switched off for this user (preferences.computerUseEnabled is false), " +
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
