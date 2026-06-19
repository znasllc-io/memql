// Package actionplan implements Phase 3 retrieval-augmented composition for
// the action library (#1738, epic #1734): the planner's per-sub-goal decision
// against the action library, mirroring the agent route/upgrade/provision
// thresholds one level down (over actions instead of agents).
//
// It is pure of engine/DB types -- the planner supplies a library search
// result (a best-fit action + cosine fitScore) and this package returns the
// REUSE / ADAPT / SYNTHESIZE decision plus the dedup verdict for a newly
// minted action. The live pgvector search + the emission of action steps into
// the plan are the planner-side integration that consumes these decisions.
package actionplan

// Decision is the per-sub-goal verdict against the action library.
type Decision string

const (
	// Reuse: a high-fit library action -> emit an action step, bind params
	// from the sub-goal/input, NO LLM step at runtime.
	Reuse Decision = "reuse"
	// Adapt: a partial-fit action -> emit it but flag for LLM arg-adjustment
	// at execution if the bind is incomplete.
	Adapt Decision = "adapt"
	// Synthesize: no fit -> keep the LLM step; on success, trace + contribute
	// a new action to the library.
	Synthesize Decision = "synthesize"
)

// Thresholds mirror the planner's existing agent thresholds (planner.go):
// Route 0.82, Upgrade 0.60, Dedup 0.90. Configurable so the planner can tune
// them, with sensible library defaults.
type Thresholds struct {
	Reuse float64 // >= -> REUSE (default 0.82)
	Adapt float64 // >= -> ADAPT (default 0.60)
	Dedup float64 // >= -> a newly minted action near-duplicates an existing one (default 0.90)
}

// DefaultThresholds returns the library's default decision thresholds,
// matching the agent route/upgrade/dedup values.
func DefaultThresholds() Thresholds {
	return Thresholds{Reuse: 0.82, Adapt: 0.60, Dedup: 0.90}
}

// Fit is a single library search hit: the action id + its cosine fitScore
// against the sub-goal embedding. CanBind reports whether the planner could
// fully bind the action's params from the sub-goal/input (an incomplete bind
// downgrades a REUSE to ADAPT).
type Fit struct {
	ActionID string
	Score    float64
	CanBind  bool
}

// Decide returns the per-sub-goal decision for the best library hit (or a
// zero Fit when the search returned nothing). Preference is for the
// highest-level fit; the planner is responsible for preferring composites
// over primitives when both clear the bar.
func Decide(best Fit, t Thresholds) Decision {
	if best.ActionID == "" {
		return Synthesize
	}
	if best.Score >= t.Reuse && best.CanBind {
		return Reuse
	}
	if best.Score >= t.Adapt {
		// A high score with an incomplete bind still adapts (LLM fixes the
		// args at execution); a mid score adapts by definition.
		return Adapt
	}
	return Synthesize
}

// ShouldDedup reports whether a newly minted action is a near-duplicate of an
// existing library action (>= Dedup), in which case the planner reinforces the
// existing action instead of landing a new row (preventing library bloat,
// mirroring agent dedup).
func ShouldDedup(bestExisting Fit, t Thresholds) bool {
	return bestExisting.ActionID != "" && bestExisting.Score >= t.Dedup
}

// PreferCompositeBest returns the best fit, preferring a composite over a
// primitive when both clear the reuse bar (so the planner reuses the
// highest-level prior composition possible, per the doc). Composite ids are
// flagged by the caller via the IsComposite field.
type RankedFit struct {
	Fit
	IsComposite bool
}

// BestFit picks the winning fit from a search result set: among hits at or
// above the reuse bar, a composite wins; otherwise the highest score wins.
func BestFit(hits []RankedFit, t Thresholds) Fit {
	var best RankedFit
	var bestComposite RankedFit
	for _, h := range hits {
		if h.Score > best.Score {
			best = h
		}
		if h.IsComposite && h.Score >= t.Reuse && h.Score > bestComposite.Score {
			bestComposite = h
		}
	}
	if bestComposite.ActionID != "" {
		return bestComposite.Fit
	}
	return best.Fit
}
