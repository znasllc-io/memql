package airoute

import "strings"

// apps.go -- the CLOSED set of apps the engine can drive, and the one place it
// is written down (epic memql#5391, design D8).
//
// IT LIVES HERE RATHER THAN IN component/worker, and the reason is a module
// direction. The DSL parser refuses a policy entry `app:<id>` for an id outside
// this set at LOAD, and component/language cannot import component/worker --
// component/worker imports component/language. core/airoute is the shared
// routing vocabulary both already require, and `app:<id>` is exactly that: a
// routing word.
//
// GROWING THE SET IS A VALUE CHANGE HERE AND NOWHERE ELSE. component/worker's
// IsKnownAppId and KnownAppIds derive from this, the registry resolves
// `app:<id>` against it, and the parser's refusal message is built from it --
// so a third app is one line, and a restated list cannot drift out of step
// with the one the engine actually drives.
const (
	// AppClaudeCode is Claude Code's app id.
	AppClaudeCode = "claude-code"
	// AppCodex is Codex's app id.
	AppCodex = "codex"
)

// runnableApps is the set, in sorted order, which is the order every message
// built from it lists.
var runnableApps = []string{AppClaudeCode, AppCodex}

// RunnableApps returns the closed set, sorted. The slice is a COPY: a caller
// that sorted or appended to a shared one would be editing the set for every
// later reader.
func RunnableApps() []string {
	out := make([]string, len(runnableApps))
	copy(out, runnableApps)
	return out
}

// IsRunnableApp reports whether id is in the closed set.
//
// The WILDCARD IS NOT A MEMBER. `app:*` is a selector over this set -- "any
// signed-in app that can run the call" -- and admitting it here would let it
// through every check that asks whether a concrete app exists.
func IsRunnableApp(id string) bool {
	id = strings.TrimSpace(id)
	for _, known := range runnableApps {
		if id == known {
			return true
		}
	}
	return false
}

// What a machine measured about an app, the closed set (component/worker's
// AppInfo.Readiness carries it). Empty is a Cockpit that measures nothing.
const (
	AppReadinessReady      = "ready"
	AppReadinessSignedOut  = "signed_out"
	AppReadinessOutOfUsage = "out_of_usage"
	AppReadinessUnknown    = "unknown"
)

// AppRuns reports whether an app a machine reported can be DRIVEN now: the id
// is in the closed set, the machine allows it, it is signed in, and the
// machine measured it ready (or measures nothing, a Cockpit that predates the
// checks). It is component/worker's AppInfo.Runnable, written once here so the
// routing side and AppIsSetUp below read one set of tests.
func AppRuns(id string, allowed, signedIn bool, readiness string) bool {
	return IsRunnableApp(id) && allowed && signedIn &&
		(readiness == "" || readiness == AppReadinessReady)
}

// AppIsSetUp reports whether a person can be said to have set an app up: it
// is AppRuns with the tests about being ready right now dropped. "Not checked
// yet" and "out of usage" count, because they come back; only a machine that
// measured the app signed out does not. Desktop access reads this, so the two
// differ in nothing but what is temporary.
func AppIsSetUp(id string, allowed, signedIn bool, readiness string) bool {
	return IsRunnableApp(id) && allowed && signedIn && readiness != AppReadinessSignedOut
}
