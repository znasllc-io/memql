package work

// replay.go -- the three replay modes (design record
// docs/superpowers/specs/2026-09-05-work-spine-design.md, section D
// "Replay has three modes").
//
//   live    serves nothing from the journal. Its DETERMINISTIC steps are
//           the reuse; its reasoning steps call a model.
//   replay  serves every model call from the journal. A hash miss raises
//           a divergence pinned to the first step that differs, unless
//           the run's replayPolicy is permissive.
//   fork    serves the shared prefix from the journal and runs live from
//           the fork step.
//
// JOURNAL SERVING NEVER CROSSES GOALS. A reasoning step is not memoized
// across goals: cross-goal reuse of an ANSWER is not what replayable
// means -- the template's deterministic steps are the reuse, and serving
// one goal's answer to another would silently make the system confidently
// wrong. The SameGoal field is that rule, checked before mode.
//
// A LEARNED PROCEDURE IS SERVED BY ITS RUNG, AND ONLY IN A LIVE RUN (epic
// memql#5408; design record
// docs/superpowers/specs/2026-09-13-app-session-recording-and-learning-program-design.md,
// section 4 epic D). When compile's exact tier finds a learned procedure for
// the goal, the ladder decides: trusted serves the construct's steps, canary
// serves them with the app standing by, shadow lets the app serve and replays
// the construct beside it, and nothing else serves at all. A replay or a fork
// keeps the journal decision whatever the ladder says -- it must REPRODUCE the
// recorded run, and serving today's construct inside it would report a
// reproduction that never happened.

import "fmt"

// Serving sources.
const (
	// ServeJournal means a recorded response answers the request.
	ServeJournal = "journal"
	// ServeLive means a provider is called.
	ServeLive = "live"
	// ServeConstruct means a learned procedure's steps answer the goal,
	// with no model.
	ServeConstruct = "construct"
)

// ReplayContext is everything the serving decision reads.
type ReplayContext struct {
	// Mode is the run's mode: live, replay or fork.
	Mode string
	// ReplayPolicy is strict (default) or permissive.
	ReplayPolicy string
	// JournalHit reports whether a modelCall row with this requestHash
	// exists for the run being served from.
	JournalHit bool
	// SameGoal reports whether that row belongs to the same goal.
	SameGoal bool
	// BeforeForkPoint reports whether this step is in the fork's shared
	// prefix. Meaningless outside fork mode.
	BeforeForkPoint bool
	// ConstructRung is the ladder rung of the learned procedure compile's
	// exact tier found for this goal; RungNone when it found none, which
	// leaves every decision above exactly as it was.
	ConstructRung Rung
}

// ServeVerdict is what to do about one model request.
type ServeVerdict struct {
	// Source is ServeJournal, ServeLive or ServeConstruct.
	Source string
	// Diverged is set when a strict replay could not be served.
	Diverged bool
	// Reason explains a divergence, or why a learned procedure the goal
	// matched does not serve it.
	Reason string
	// Shadow: the app serves, and the construct replays beside it in a
	// sandbox so its steps can be compared with the app's.
	Shadow bool
	// Standby: the construct serves, and the app stands by to take over on
	// a divergence.
	Standby bool
}

// DecideServe answers one model request -- or, with ConstructRung set, one
// goal a learned procedure matched.
func DecideServe(rc ReplayContext) ServeVerdict {
	// The cross-goal rule outranks the mode. A row from another goal is
	// not a hit at all.
	hit := rc.JournalHit && rc.SameGoal

	switch rc.Mode {
	case "replay":
		if hit {
			return ServeVerdict{Source: ServeJournal}
		}
		if rc.ReplayPolicy == "permissive" {
			return ServeVerdict{Source: ServeLive}
		}
		return ServeVerdict{
			Source:   ServeLive,
			Diverged: true,
			Reason:   "no journaled model call matches this request hash -- the prompt, the model or the settings changed since the recorded run; re-run with replayPolicy=permissive to make a fresh call and journal it",
		}
	case "fork":
		if hit && rc.BeforeForkPoint {
			return ServeVerdict{Source: ServeJournal}
		}
		return ServeVerdict{Source: ServeLive}
	case "", "live":
		if rc.ConstructRung != RungNone {
			return serveByRung(rc.ConstructRung)
		}
		return ServeVerdict{Source: ServeLive}
	default:
		// Anything unrecognised. Serving a journal row -- or a learned
		// procedure -- to a run that did not ask for it is the one mistake
		// here that produces a confidently wrong answer.
		return ServeVerdict{Source: ServeLive}
	}
}

// serveByRung is the ladder's half of the decision, for a live run.
func serveByRung(r Rung) ServeVerdict {
	switch r {
	case RungTrusted:
		return ServeVerdict{Source: ServeConstruct}
	case RungCanary:
		return ServeVerdict{Source: ServeConstruct, Standby: true}
	case RungShadow:
		return ServeVerdict{Source: ServeLive, Shadow: true}
	case RungCandidate:
		return ServeVerdict{Source: ServeLive, Reason: "the learned procedure is a candidate that has not been compared beside the app, so the app serves"}
	case RungRetired:
		return ServeVerdict{Source: ServeLive, Reason: "the learned procedure is retired, so the app serves"}
	}
	return ServeVerdict{Source: ServeLive, Reason: fmt.Sprintf("the learned procedure's rung %q is not one this build knows, so the app serves", r)}
}
