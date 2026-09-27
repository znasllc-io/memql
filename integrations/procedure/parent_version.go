package procedure

import (
	"context"
	"fmt"
	"strings"

	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
)

// parent_version.go -- what a recording's PARENT step version says about it
// (epic memql#5414, design D18 and D23's learning half; plan section 1.5).
//
// A delegated session is a recording of ONE version of the goal-run step that
// delegated it: the parent step's version whose childRunId is the recording.
// Two things about that version decide the recording's place in the corpus.
//
// A VERSION THAT IS NO LONGER CURRENT WAS REPLACED. A person re-ran the step,
// or moved the run's head off it, so the recording answers a question the run
// no longer asks; it leaves the corpus as `superseded`. Mining it would teach a
// procedure the behaviour the person stepped in to change.
//
// THE VERSION'S NEWEST VERDICT JUDGES THE RECORDING. A dislike takes it out
// (`disliked`), and a like makes it weigh more -- which RANKS it and never
// counts it twice (component/procedure.MineWeighted).
//
// The versions are read through workStepVersions, the one read of every
// version of a run's steps, under the owner's actor like every other corpus
// read; the verdicts are feedback observations on the PARENT run naming
// {stepKey, version}, read through readFeedback as the recording's own are.

// Why a recording left the corpus.
const (
	exclusionSuperseded = "superseded"
	exclusionDisliked   = "disliked"
)

// likedWeight is a liked recording's weight in the corpus; every other
// recording weighs one.
const likedWeight = 2

// parentJudgment is what a recording's parent step version says about it.
type parentJudgment struct {
	// Excluded names why the recording leaves the corpus, or is empty.
	Excluded string
	// Liked: the version's newest verdict is a like.
	Liked bool
	// StepKey and Version name the parent's version that delegated the
	// recording; empty when no version names it.
	StepKey string
	Version int
}

// parentFacts are one parent run's versions and feedback, read once per corpus
// load: several recordings share a parent when its step was re-run, and each
// of them is judged against the same rows.
type parentFacts struct {
	versions []map[string]any
	feedback *feedback
	// unreadable: the versions could not be read, so nothing about this
	// parent is known -- and it is said once, not once per recording.
	unreadable bool
}

// judgeByParent reads the recording's parent step version and judges the
// recording by it. A run no step delegated -- no parentRunId, or a parent none
// of whose versions names it -- is judged by nothing and stays.
//
// A PARENT THAT CANNOT BE READ JUDGES NOTHING either, and never fails the
// load. An unreadable version is no evidence that the recording was replaced
// or disliked -- the same reading an unmarked `current` gets below -- and
// failing the whole corpus for it would let one recording whose parent the
// read cannot reach (a transient error, a parent that is not the owner's, a
// node that registers no work integration) stop every procedure of the goal
// from being learned. The recording is judged by its own run's verdicts, and
// the failure is logged.
func (i *Integration) judgeByParent(ctx context.Context, run map[string]any, cache map[string]*parentFacts) parentJudgment {
	parent := strings.TrimSpace(str(run, "parentRunId"))
	runId := strings.TrimSpace(str(run, "id"))
	if parent == "" || runId == "" {
		return parentJudgment{}
	}
	facts := cache[parent]
	if facts == nil {
		facts = &parentFacts{}
		versions, err := i.store.runStepVersions(ctx, parent)
		if err != nil {
			facts.unreadable = true
			// Info, not Warn: in this package a WARN says a write was refused,
			// and nothing here was written -- the lift goes on with what it
			// can see.
			i.log().Info("procedure: a recording's parent run could not be read; its recordings are judged by their own verdicts alone",
				"parentRunId", parent, "error", err)
		}
		facts.versions = versions
		if cache != nil {
			cache[parent] = facts
		}
	}
	if facts.unreadable {
		return parentJudgment{}
	}
	delegating := delegatingVersion(facts.versions, runId)
	if delegating == nil {
		return parentJudgment{}
	}
	j := parentJudgment{StepKey: str(delegating, "key"), Version: intOf(delegating, "version")}
	// Only an explicit false excludes. A version the read did not mark either
	// way is not evidence that it was replaced, and reading silence as "no
	// longer current" would empty the corpus of every recording at once.
	if current, marked := delegating["current"].(bool); marked && !current {
		j.Excluded = exclusionSuperseded
		return j
	}
	if facts.feedback == nil {
		fb := feedback{}
		observations, err := i.store.query(ctx, "query "+call("workObservationsForOwnerRun", map[string]any{"runId": parent}))
		if err != nil {
			i.log().Info("procedure: the feedback on a recording's parent run could not be read; its version's verdict is unseen",
				"parentRunId", parent, "error", err)
		} else {
			fb = readFeedback(observations)
		}
		facts.feedback = &fb
	}
	switch newestVerdictOn(facts.feedback.steps[j.StepKey], j.Version) {
	case work.VerdictDislike:
		j.Excluded = exclusionDisliked
	case work.VerdictLike:
		j.Liked = true
	}
	return j
}

// delegatingVersion is the version whose childRunId is the recording. Each
// version opens its own child, so one version names it; if several do, the
// LOWEST is the one that opened it -- a later version can only carry the id
// by a read-merge that failed to reset it.
func delegatingVersion(versions []map[string]any, runId string) map[string]any {
	var found map[string]any
	for _, v := range versions {
		if !sameRunId(str(v, "childRunId"), runId) {
			continue
		}
		if found == nil || intOf(v, "version") < intOf(found, "version") {
			found = v
		}
	}
	return found
}

// newestVerdictOn is the person's current judgment of one version: the newest
// verdict naming it (D21: a verdict is never overwritten, so the newest row
// is the judgment). A verdict this build cannot read is unseen and has no
// vote, so it cannot hide a readable one.
func newestVerdictOn(rows []stepVerdict, version int) work.Verdict {
	var newest *stepVerdict
	for n := range rows {
		r := rows[n]
		if r.Version != version || r.Verdict == work.VerdictUnseen {
			continue
		}
		if newest == nil || newerVerdict(r, *newest) {
			newest = &rows[n]
		}
	}
	if newest == nil {
		return work.VerdictUnseen
	}
	return newest.Verdict
}

// newerVerdict orders two verdicts by when they were given, the later row in
// the read breaking a tie.
func newerVerdict(a, b stepVerdict) bool {
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.After(b.CreatedAt)
	}
	return a.Order > b.Order
}

// sameRunId compares two run ids that may be spelled canonically or bare:
// childRunId and parentRunId are stored exactly as their writers passed them.
func sameRunId(a, b string) bool {
	a, b = memqlengine.BareShortId(a), memqlengine.BareShortId(b)
	return a != "" && a == b
}

// runStepVersions reads every version of every step of one run through the
// workStepVersions builtin, under the actor in ctx -- the run's owner, whom
// the builtin requires. A builtin answers one node set keyed by id rather
// than a row set, which memqlRows does not unwrap; MaterializeRows does, with
// each node's payload laid over it.
func (s *store) runStepVersions(ctx context.Context, runId string) ([]map[string]any, error) {
	if s == nil || s.engine == nil {
		return nil, fmt.Errorf("procedure: engine not configured")
	}
	q := "builtin " + call("workStepVersions", map[string]any{"runId": runId})
	res, err := s.engine.Execute(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("workStepVersions: %w", err)
	}
	return memqlengine.MaterializeRows(res), nil
}
