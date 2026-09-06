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

// Serving sources.
const (
	// ServeJournal means a recorded response answers the request.
	ServeJournal = "journal"
	// ServeLive means a provider is called.
	ServeLive = "live"
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
}

// ServeVerdict is what to do about one model request.
type ServeVerdict struct {
	// Source is ServeJournal or ServeLive.
	Source string
	// Diverged is set when a strict replay could not be served.
	Diverged bool
	// Reason explains a divergence.
	Reason string
}

// DecideServe answers one model request.
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
	default:
		// live, and anything unrecognised. Serving a journal row to a
		// run that did not ask to be replayed is the one mistake here
		// that produces a confidently wrong answer.
		return ServeVerdict{Source: ServeLive}
	}
}
