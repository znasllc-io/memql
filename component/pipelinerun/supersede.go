package pipelinerun

import (
	"context"
	"errors"
	"strings"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/core/logger"
)

// supersede.go -- a new push to a pull request stops the pull request's
// earlier runs (design record D11; epic memql#5478, memql#5496).
//
// When a pull request's head moves, the run of the old head answers a
// question nobody is asking any more, while it holds a runner and its check
// run sits on a commit the pull request has left. So opening the new head's
// run asks every OTHER run of the same pipeline and pull request that has not
// finished, in affected mode, to stop -- through RequestCancel, as
// "superseded by <the new run's id>". A queued run nobody drives is concluded
// cancelled there and then, its check run with it; a run an agent drives is
// asked, and its driver stops what is executing and concludes it.
//
// WHAT IS NEVER STOPPED. A full-mode run: the merge queue's, a push to the
// default branch's, a release's -- each runs to its end, whatever pull request
// it carries. A run of another pull request, or of another pipeline's pull
// request of the same number. And nothing at all is stopped by:
//
//   - a second sighting of a head (a redelivered webhook; a webhook and the
//     poll for one head): it opens nothing, and the run that answers it
//     superseded what it superseded when it opened;
//   - a re-run, which is not a push: re-running an earlier head asks for that
//     commit's answer again, and stopping the pull request's current run for
//     it would leave the head the pull request shows with a cancelled check;
//   - an opening that is not the pull request's CURRENT head (below).
//
// ONLY THE PULL REQUEST'S CURRENT HEAD SUPERSEDES (ruling R34). The order in
// which openings are processed is not the order the pushes happened: two
// replicas take deliveries at once, a trigger retries after a gate timeout, a
// redelivery arrives late, a poll can read the heads just before a newer push.
// A late opening for an OLDER head would otherwise stop the run of the head
// the pull request actually shows. So before anything is stopped, GitHub is
// asked for the pull request's head -- by its number, never through the open
// list, which is one page of a hundred and misses an older pull request in a
// busy repository -- and an opening whose commit is not that head stops
// nothing: the newer head's own opening supersedes. Whatever keeps the head
// from being KNOWN stops nothing too -- no installation token to ask with, a
// read GitHub refused, an answer naming no head -- and the log says which.
// GitHub is asked only when some run would be stopped, so the common case,
// nothing unfinished, costs no call.
//
// THE RUNS ARE READ BEFORE THE NEW RUN EXISTS, and so before GitHub is asked,
// and only those are stopped. A newer push that lands while this opening is
// under way -- after GitHub named this opening's head -- opens a run this
// opening never read, so it can never be this opening's to stop. Of two runs
// of one pull request opened together, only the current head's can stop
// anything, and it stops only what was visible before its own run existed;
// when that misses the other, both run, which costs a runner and nothing else.
//
// AND NOTHING HAPPENS UNTIL THE OPEN GATE IS RELEASED. The head read is a call
// to GitHub, RequestCancel takes each run's own gate and concludes a queued
// run with a write to GitHub; under the open gate those are calls to GitHub
// under a gate and a nested gate, which the gate discipline forbids (ids.go).
// A read or an ask that fails is logged and fails nothing: the new head's run
// is open, and an earlier run left going costs a runner, never a wrong answer.
//
// A SUPERSEDED RUN DOES NOT ANSWER ITS OWN HEAD COMING BACK (ruling R37).
// Push X, push Y (which supersedes X's run), force-push back to X: X's key
// already has a run, the superseded one, and a second sighting of a head is
// answered by its run -- which would leave the head the pull request shows
// with a cancelled check and no run coming, while Y's run, a commit no longer
// in the pull request, keeps its runner. So when the key's newest attempt was
// stopped as superseded (stoppedBySupersede), the dedup asks GitHub whether
// the opening's commit is the pull request's head again: if it is, the key
// opens its NEXT attempt, the way a fork's refusal is opened past, and that
// run supersedes Y in the ordinary way above. The question is asked BEFORE the
// open gate and only in that case; the gated dedup read reuses the answer, so
// nothing of GitHub's is asked under the gate for it. Anything short of a yes
// -- another head (a late redelivery of a superseded head), no token, a
// refused read -- leaves the superseded run answering, as it always has.

// supersedes reports whether opening o, in mode, stops its pull request's
// earlier runs: a new head of a pull request, in affected mode, delivered by
// a webhook or seen by the poll -- anything but a re-run.
func supersedes(o Opening, mode pipelines.Mode) bool {
	return mode == pipelines.ModeAffected && o.PullRequest > 0 && o.Trigger != TriggerRerun
}

// supersedable is what opening o would supersede, read fresh BEFORE the run
// is created (above): every unfinished run of p's pull request, which
// supersede narrows once the run exists. Nil when o supersedes nothing, or
// when the read failed -- which is logged, and opens the run regardless.
func supersedable(ctx context.Context, d Deps, p Pipeline, o Opening, mode pipelines.Mode) []Run {
	if !supersedes(o, mode) {
		return nil
	}
	runs, err := d.Store.RunsUnfinishedForPullRequest(memql.ContextWithFreshRead(ctx), p.ID, o.PullRequest)
	if err != nil {
		d.Logger.Warn("pipelines: the pull request's earlier runs could not be read, so this push stops none of them",
			"component", "pipelinerun", logger.Subject(PipelineConcept, p.ID), "repository", p.Repository,
			"pullRequest", o.PullRequest, "error", err)
		return nil
	}
	return runs
}

// supersede asks each run of earlier that opened supersedes to stop -- when
// opened is its pull request's current head on GitHub. It is called with NO
// GATE HELD, once opened exists, with the installation token the opening
// minted ("" when it could not mint one). A run that finished since it was
// read has nothing to stop and is passed over; any other failure is logged,
// and the rest are still asked.
func (i *Integration) supersede(ctx context.Context, d Deps, opened Run, earlier []Run, token string) {
	var stale []Run
	for _, r := range earlier {
		if supersededBy(r, opened) {
			stale = append(stale, r)
		}
	}
	if len(stale) == 0 || !isPullRequestHead(ctx, d, opened, token) {
		return
	}
	by := supersededPrefix + opened.ID
	for _, r := range stale {
		attrs := []any{"component", "pipelinerun", logger.Subject(RunConcept, r.ID), "supersededBy", opened.ID,
			"pullRequest", opened.PullRequest}
		if _, err := i.requestCancel(ctx, d, r.ID, by); err != nil {
			if errors.Is(err, ErrRunFinished) {
				d.Logger.Info("pipelines: a run a newer push superseded had finished before it could be asked to stop; its answer stands", attrs...)
				continue
			}
			d.Logger.Warn("pipelines: a run a newer push to its pull request superseded could not be cancelled",
				append(attrs, "error", err)...)
			continue
		}
		d.Logger.Info("pipelines: a newer push to the pull request superseded this run", attrs...)
	}
}

// supersededPrefix begins the cancelledBy of every run a newer push stopped:
// "superseded by <run id>".
const supersededPrefix = "superseded by "

// headVerdict is what GitHub says of one commit and a pull request's head.
type headVerdict int

const (
	// headUnknown: no token to ask with, a read GitHub refused, or an answer
	// naming no head -- the error says which.
	headUnknown headVerdict = iota
	// headElsewhere: GitHub names another commit.
	headElsewhere
	// headHere: GitHub names this commit.
	headHere
)

var (
	errNoHeadToken = errors.New("there is no installation token to ask GitHub with")
	errNoHeadNamed = errors.New("GitHub named no head for the pull request")
)

// pullRequestHead asks GitHub for pull request number's head in repository and
// says whether it is sha, and what it is. The ONE place either decision -- a
// supersede (R34) or a superseded head coming back (R37) -- reads the head, so
// both compare SHAs as this package compares them everywhere: trimmed and
// lower-cased, then equal.
func pullRequestHead(ctx context.Context, d Deps, repository string, number int, sha, token string) (headVerdict, string, error) {
	if d.GitHub == nil || strings.TrimSpace(token) == "" {
		return headUnknown, "", errNoHeadToken
	}
	pr, err := d.GitHub.PullRequestHead(ctx, token, repository, number)
	if err != nil {
		return headUnknown, "", err
	}
	head := strings.ToLower(strings.TrimSpace(pr.HeadSHA))
	if head == "" {
		return headUnknown, "", errNoHeadNamed
	}
	if head != strings.ToLower(strings.TrimSpace(sha)) {
		return headElsewhere, head, nil
	}
	return headHere, head, nil
}

// isPullRequestHead reports whether opened's commit is its pull request's head
// as GitHub reports it now. Anything short of a yes is a no, and the log says
// which: a head that has moved on at Info -- the newer head's own opening
// supersedes -- and anything that kept the head from being known at Warn.
func isPullRequestHead(ctx context.Context, d Deps, opened Run, token string) bool {
	attrs := []any{"component", "pipelinerun", logger.Subject(RunConcept, opened.ID),
		"repository", opened.Repository, "pullRequest", opened.PullRequest}
	verdict, head, err := pullRequestHead(ctx, d, opened.Repository, opened.PullRequest, opened.SHA, token)
	switch {
	case verdict == headHere:
		return true
	case verdict == headElsewhere:
		d.Logger.Info("pipelines: the pull request's head has moved on since this push, so it stops none of its earlier runs; the newer head's own opening supersedes",
			append(attrs, "sha", opened.SHA, "head", head)...)
	case errors.Is(err, errNoHeadToken):
		d.Logger.Warn("pipelines: there is no installation token to ask GitHub for the pull request's head, so this push stops none of its earlier runs", attrs...)
	case errors.Is(err, errNoHeadNamed):
		d.Logger.Warn("pipelines: GitHub named no head for the pull request, so this push stops none of its earlier runs", attrs...)
	default:
		d.Logger.Warn("pipelines: the pull request's head could not be read from GitHub, so this push stops none of its earlier runs",
			append(attrs, "error", err)...)
	}
	return false
}

// stoppedBySupersede reports whether r was stopped -- or is being stopped --
// because a newer push superseded it: asked by a supersede (its cancelledBy
// says so, and only a supersede writes that), and either concluded cancelled
// or not concluded yet. A run a PERSON cancelled is not one: that cancel was a
// decision. Nor is a superseded run that finished with an answer of its own
// before its driver read the ask: that answer stands.
func stoppedBySupersede(r Run) bool {
	return r.CancelRequested && strings.HasPrefix(r.CancelledBy, supersededPrefix) &&
		(!r.Finished() || r.Conclusion == ConclusionCancelled)
}

// supersededNewest is the key's newest attempt when it was stopped as
// superseded and o is an opening its head coming back could rerun: a push --
// not a re-run, which answers through its own rule -- of a pull request,
// from this repository (a fork's head runs nothing either way). It is the one
// case in which the dedup asks GitHub anything (R37).
func supersededNewest(runs []Run, o Opening) (Run, bool) {
	if o.Trigger == TriggerRerun || o.Fork || o.PullRequest <= 0 {
		return Run{}, false
	}
	newest, attempts := newestAttempt(runs)
	if attempts == 0 || !stoppedBySupersede(newest) {
		return Run{}, false
	}
	return newest, true
}

// headCameBack reports whether GitHub names o's commit as its pull request's
// head NOW -- a force-push back to a commit whose run a newer push superseded
// -- in which case that superseded run does not answer the delivery and the
// key opens its next attempt (R37). It is asked before the open gate, under
// the token the opening minted (tokenErr: the mint that failed). Anything
// short of a yes leaves the superseded run answering, and the log says why.
func headCameBack(ctx context.Context, d Deps, p Pipeline, o Opening, sha, token string, tokenErr error, superseded Run) bool {
	attrs := []any{"component", "pipelinerun", logger.Subject(RunConcept, superseded.ID), "repository", p.Repository,
		"pullRequest", o.PullRequest, "sha", sha, "cancelledBy", superseded.CancelledBy}
	if tokenErr != nil {
		token = ""
	}
	verdict, head, err := pullRequestHead(ctx, d, p.Repository, o.PullRequest, sha, token)
	switch {
	case verdict == headHere:
		d.Logger.Info("pipelines: the pull request's head comes back to a commit whose run a newer push superseded, so it runs again", attrs...)
		return true
	case verdict == headElsewhere:
		d.Logger.Info("pipelines: a delivery for a head a newer push superseded is answered by its superseded run; the pull request's head is elsewhere",
			append(attrs, "head", head)...)
	case errors.Is(err, errNoHeadToken):
		d.Logger.Warn("pipelines: there is no installation token to ask GitHub whether the pull request's head came back to this commit, so its superseded run answers the delivery", attrs...)
	case errors.Is(err, errNoHeadNamed):
		d.Logger.Warn("pipelines: GitHub named no head for the pull request, so the superseded run answers the delivery", attrs...)
	default:
		d.Logger.Warn("pipelines: the pull request's head could not be read from GitHub, so the superseded run answers the delivery",
			append(attrs, "error", err)...)
	}
	return false
}

// supersededBy reports whether opened stops r: ANOTHER run of the same
// pipeline and pull request, in affected mode, not finished. The read has
// narrowed to the pipeline, the pull request and the unfinished already; they
// are asked again here so the rule is whole in one place. The MODE is decided
// only here, where it is tested: a full run is never stopped by a push,
// whatever pull request it carries.
func supersededBy(r, opened Run) bool {
	return !sameID(r.ID, opened.ID) &&
		sameID(r.PipelineID, opened.PipelineID) &&
		opened.PullRequest > 0 && r.PullRequest == opened.PullRequest &&
		r.Mode == pipelines.ModeAffected &&
		!r.Finished()
}
