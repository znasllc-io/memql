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
	by := "superseded by " + opened.ID
	for _, r := range stale {
		if _, err := i.requestCancel(ctx, d, r.ID, by); err != nil {
			if errors.Is(err, ErrRunFinished) {
				continue
			}
			d.Logger.Warn("pipelines: a run a newer push to its pull request superseded could not be cancelled",
				"component", "pipelinerun", logger.Subject(RunConcept, r.ID), "supersededBy", opened.ID,
				"pullRequest", opened.PullRequest, "error", err)
			continue
		}
		d.Logger.Info("pipelines: a newer push to the pull request superseded this run",
			"component", "pipelinerun", logger.Subject(RunConcept, r.ID), "supersededBy", opened.ID,
			"pullRequest", opened.PullRequest)
	}
}

// isPullRequestHead reports whether opened's commit is its pull request's head
// as GitHub reports it now. Anything short of a yes is a no, and the log says
// which: a head that has moved on at Info -- the newer head's own opening
// supersedes -- and anything that kept the head from being known at Warn. SHAs
// are compared as this package compares them everywhere: trimmed and
// lower-cased, then equal.
func isPullRequestHead(ctx context.Context, d Deps, opened Run, token string) bool {
	attrs := []any{"component", "pipelinerun", logger.Subject(RunConcept, opened.ID),
		"repository", opened.Repository, "pullRequest", opened.PullRequest}
	if d.GitHub == nil || strings.TrimSpace(token) == "" {
		d.Logger.Warn("pipelines: there is no installation token to ask GitHub for the pull request's head, so this push stops none of its earlier runs", attrs...)
		return false
	}
	pr, err := d.GitHub.PullRequestHead(ctx, token, opened.Repository, opened.PullRequest)
	if err != nil {
		d.Logger.Warn("pipelines: the pull request's head could not be read from GitHub, so this push stops none of its earlier runs",
			append(attrs, "error", err)...)
		return false
	}
	head := strings.ToLower(strings.TrimSpace(pr.HeadSHA))
	if head == "" {
		d.Logger.Warn("pipelines: GitHub named no head for the pull request, so this push stops none of its earlier runs", attrs...)
		return false
	}
	if head != strings.ToLower(strings.TrimSpace(opened.SHA)) {
		d.Logger.Info("pipelines: the pull request's head has moved on since this push, so it stops none of its earlier runs; the newer head's own opening supersedes",
			append(attrs, "sha", opened.SHA, "head", head)...)
		return false
	}
	return true
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
