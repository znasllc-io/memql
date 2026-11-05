package pipelinerun

import (
	"context"
	"errors"
	"strings"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/core/logger"
)

// supersede.go -- a new push stops the runs it makes pointless: a pull
// request's earlier runs (design record D11; epic memql#5478, memql#5496), and,
// where the installation opted in, the default branch's waiting push runs
// (design record 2026-11-05 D4; the push lane, push_lane.go).
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
// A PUSH TO THE DEFAULT BRANCH supersedes differently, and only where the
// installation opted in through the `supersession` policy role (push_lane.go
// says who is in the lane and why). It stops only push runs that are still
// WAITING -- queued, claimed by no agent -- never one already executing, which
// finishes; and only those whose commit GitHub proves a PROPER ancestor of the
// new commit, when the new commit is the default branch's head as GitHub
// reports it then (coalescePush). The default branch only moves forward, so the
// newer commit's run tests everything the waiting one carried; a commit that
// is not proven an ancestor -- a rewritten history, another branch's commit, a
// compare GitHub would not answer -- is not superseded. Each waiting run is
// concluded by cancelIfWaiting, under its own gate, only if it is STILL
// waiting: an agent may claim it while GitHub is asked, and then it finishes.
//
// WHAT IS NEVER STOPPED. A run of a full mode other than the push lane's: the
// merge queue's, a release's -- each runs to its end, whatever pull request it
// carries; and a push run already executing, or any push run of an
// installation that did not opt in. A run of another pull request, or of
// another pipeline's pull request of the same number. A re-run, a manual run
// and a scheduled scan are never superseded and supersede nothing. And nothing
// at all is stopped by:
//
//   - a second sighting of a head (a redelivered webhook; a webhook and the
//     poll for one head): it opens nothing, and the run that answers it
//     superseded what it superseded when it opened;
//   - a re-run, which is not a push: re-running an earlier head asks for that
//     commit's answer again, and stopping the pull request's current run for
//     it would leave the head the pull request shows with a cancelled check;
//   - an opening that is not the CURRENT head (below).
//
// ONLY THE CURRENT HEAD SUPERSEDES (ruling R34). The order in which openings
// are processed is not the order the pushes happened: two replicas take
// deliveries at once, a trigger retries after a gate timeout, a redelivery
// arrives late, a poll can read the heads just before a newer push. A late
// opening for an OLDER head would otherwise stop the run of the head the pull
// request actually shows. So before anything is stopped, GitHub is asked for
// the pull request's head -- by its number, never through the open list, which
// is one page of a hundred and misses an older pull request in a busy
// repository -- and an opening whose commit is not that head stops nothing:
// the newer head's own opening supersedes. A push to the default branch reads
// the branch's head the same way (defaultBranchHead), with the same answer
// when it has moved on. Whatever keeps the head from being KNOWN stops nothing
// too -- no installation token to ask with, a read GitHub refused, an answer
// naming no head -- and the log says which. GitHub is asked only when some run
// would be stopped, so the common case, nothing unfinished, costs no call.
//
// THE RUNS ARE READ BEFORE THE NEW RUN EXISTS, and so before GitHub is asked,
// and only those are stopped. A newer push that lands while this opening is
// under way -- after GitHub named this opening's head -- opens a run this
// opening never read, so it can never be this opening's to stop. Of two runs
// of one pull request opened together, only the current head's can stop
// anything, and it stops only what was visible before its own run existed;
// when that misses the other, both run, which costs a runner and nothing else.
// The same holds for two pushes to the default branch, and for a late push
// whose own opening superseded nothing: recovery's lane pass supersedes it
// toward the head's run (push_lane.go).
//
// AND NOTHING HAPPENS UNTIL THE OPEN GATE IS RELEASED. The head read and the
// ancestry compares are calls to GitHub, and RequestCancel and cancelIfWaiting
// take each run's own gate and conclude a queued run with a write to GitHub;
// under the open gate those are calls to GitHub under a gate and a nested
// gate, which the gate discipline forbids (ids.go). A read or an ask that
// fails is logged and fails nothing: the new head's run is open, and an
// earlier run left going costs a runner, never a wrong answer.
//
// A SUPERSEDED RUN DOES NOT ANSWER ITS OWN HEAD COMING BACK (ruling R37).
// Push X, push Y (which supersedes X's run), force-push back to X: X's key
// already has a run, the superseded one, and a second sighting of a head is
// answered by its run -- which would leave the head with a cancelled check and
// no run coming, while Y's run, a commit no longer there, keeps its runner. So
// when the key's newest attempt was stopped as superseded (stoppedBySupersede),
// the dedup asks GitHub whether the opening's commit is the head again -- the
// pull request's head, or for a push the default branch's: if it is, the key
// opens its NEXT attempt, the way a fork's refusal is opened past, and that
// run supersedes in the ordinary way above. For a push that means Y's run is
// superseded only if Y is proven an ancestor of X, which after a force-push
// back it is not: Y's run, if still waiting, waits its turn, and costs a
// runner. The installation's ruleset refuses force-pushes to the default
// branch, but nothing here relies on that. The question is asked BEFORE the
// open gate and only in that case; the gated dedup read reuses the answer, so
// nothing of GitHub's is asked under the gate for it. Anything short of a yes
// -- another head (a late redelivery of a superseded head), no token, a
// refused read -- leaves the superseded run answering, as it always has.

// supersedesPullRequest reports whether opening o, in mode, stops its pull
// request's earlier runs: a new head of a pull request, in affected mode,
// delivered by a webhook or seen by the poll -- anything but a re-run.
func supersedesPullRequest(o Opening, mode pipelines.Mode) bool {
	return mode == pipelines.ModeAffected && o.PullRequest > 0 && !o.isRerun()
}

// supersedesWaitingPushes reports whether opening o, in mode, under the policy
// its run is opened with, is a push-lane member (push_lane.go) and so
// supersedes the pipeline's waiting push runs it covers: a push to the default
// branch in full mode that a webhook or the poll delivered, which that policy
// opts in. The policy is asked last, and only of a push of the lane's shape.
func supersedesWaitingPushes(ctx context.Context, o Opening, mode pipelines.Mode, policy *workflowhost.Snapshot) bool {
	return o.Event == pipelines.EventPush && mode == pipelines.ModeFull && o.PullRequest <= 0 && !o.isRerun() &&
		o.Qualification == nil && (o.Trigger == TriggerWebhook || o.Trigger == TriggerPoll) &&
		pushLaneAdmits(ctx, policy, o.Trigger)
}

// supersedable is what opening o would supersede, read fresh BEFORE the run
// is created (above): every unfinished run of p's pull request, or, for a
// push-lane member, every unfinished push run of p -- which supersede narrows
// once the run exists. Nil when o supersedes nothing, or when the read failed
// -- which is logged, and opens the run regardless.
func supersedable(ctx context.Context, d Deps, p Pipeline, o Opening, mode pipelines.Mode, policy *workflowhost.Snapshot) []Run {
	switch {
	case supersedesPullRequest(o, mode):
		runs, err := d.Store.RunsUnfinishedForPullRequest(memql.ContextWithFreshRead(ctx), p.ID, o.PullRequest)
		if err != nil {
			d.Logger.Warn("pipelines: the pull request's earlier runs could not be read, so this push stops none of them",
				"component", "pipelinerun", logger.Subject(PipelineConcept, p.ID), "repository", p.Repository,
				"pullRequest", o.PullRequest, "error", err)
			return nil
		}
		return runs
	case supersedesWaitingPushes(ctx, o, mode, policy):
		runs, err := d.Store.RunsUnfinishedForPipelineEvent(memql.ContextWithFreshRead(ctx), p.ID, pipelines.EventPush)
		if err != nil {
			d.Logger.Warn("pipelines: the pipeline's waiting push runs could not be read, so this push supersedes none of them",
				"component", "pipelinerun", logger.Subject(PipelineConcept, p.ID), "repository", p.Repository, "error", err)
			return nil
		}
		return runs
	}
	return nil
}

// supersede asks each run of earlier that opened supersedes to stop -- when
// opened is the current head on GitHub. It is called with NO GATE HELD, once
// opened exists, with the installation token the opening minted ("" when it
// could not mint one). A run that finished since it was read has nothing to
// stop and is passed over; any other failure is logged, and the rest are still
// asked. A push to the default branch goes its own way (coalescePush).
func (i *Integration) supersede(ctx context.Context, d Deps, p Pipeline, opened Run, earlier []Run, token string) {
	if opened.Event == pipelines.EventPush {
		i.coalescePush(ctx, d, p, opened, earlier, token)
		return
	}
	var stale []Run
	for _, r := range earlier {
		if supersededBy(r, opened) {
			if opened.PolicySnapshot != nil {
				policy, err := readPipelinePolicy(opened.PolicySnapshot)
				if err != nil {
					continue
				}
				value, err := runPolicyPhase(ctx, policy, "pipelineSupersessionPolicy", map[string]any{
					"event": string(opened.Event), "mode": string(opened.Mode), "previousMode": string(r.Mode), "trigger": opened.Trigger,
				}, workflowhost.Options{})
				allowed, _ := value.(bool)
				if err != nil || !allowed {
					continue
				}
			}
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

// coalescePush supersedes the waiting push runs of earlier that opened -- a new
// push to the default branch, a push-lane member -- covers (D4): each run
// coalescedBy names, when opened's commit is the default branch's head and
// GitHub proves the waiting run's commit a proper ancestor of it. Called with
// NO GATE HELD after the open gate is released. GitHub is asked for the head
// only when something waits, and for each waiting run's ancestry only once the
// head is known to be opened's; anything short of a proof supersedes nothing
// and says why.
func (i *Integration) coalescePush(ctx context.Context, d Deps, p Pipeline, opened Run, earlier []Run, token string) {
	if !pushLaneShaped(opened) {
		return
	}
	var waiting []Run
	for _, r := range earlier {
		if coalescedBy(ctx, r, opened) {
			waiting = append(waiting, r)
		}
	}
	if len(waiting) == 0 || !isDefaultBranchHead(ctx, d, p, opened, token) {
		return
	}
	head := normalizeSHA(opened.SHA)
	for _, r := range waiting {
		i.supersedeIfAncestor(ctx, d, p, r, opened, head, token)
	}
}

// coalescedBy reports whether opened, a new push to the default branch,
// supersedes r: ANOTHER run of the same pipeline, at another commit, still
// waiting as it was read -- queued, claimed by no agent -- and itself a
// push-lane member under its OWN policy snapshot, so a run opened before the
// installation opted in is never superseded. Whether opened is the head and
// r's commit an ancestor of it is GitHub's to prove (coalescePush). The
// snapshot is evaluated last, after the checks that cost nothing.
func coalescedBy(ctx context.Context, r, opened Run) bool {
	return !sameID(r.ID, opened.ID) && sameID(r.PipelineID, opened.PipelineID) &&
		normalizeSHA(r.SHA) != normalizeSHA(opened.SHA) && waitingRun(r) && inPushLane(ctx, r)
}

// supersedeIfAncestor concludes w -- a waiting push run -- superseded by cover,
// the push run of the default branch's head, when GitHub proves w's commit a
// PROPER ancestor of head; it reports whether w was concluded. Called with NO
// GATE HELD: the compare is asked first, then cancelIfWaiting takes w's gate
// and concludes it only if it is still waiting. Anything short of a proof, or
// a run an agent claimed meanwhile, leaves w alone, and the log says why.
func (i *Integration) supersedeIfAncestor(ctx context.Context, d Deps, p Pipeline, w, cover Run, head, token string) bool {
	attrs := []any{"component", "pipelinerun", logger.Subject(RunConcept, bareID(w.ID)), "sha", w.SHA,
		"head", head, "supersededBy", bareID(cover.ID)}
	if d.GitHub == nil || strings.TrimSpace(token) == "" {
		d.Logger.Warn("pipelines: there is no installation token to compare a waiting push run with the default branch's head, so it is not superseded", attrs...)
		return false
	}
	callCtx, done := context.WithTimeout(ctx, githubCallTimeout)
	ancestry, err := d.GitHub.CompareAncestry(callCtx, token, p.Repository, normalizeSHA(w.SHA), head)
	done()
	switch {
	case err != nil:
		d.Logger.Warn("pipelines: a waiting push run could not be compared with the default branch's head, so it is not superseded",
			append(attrs, "error", err)...)
		return false
	case !ancestry.Ancestor:
		d.Logger.Info("pipelines: a waiting push run is not a proven ancestor of the default branch's head, so it is not superseded", attrs...)
		return false
	}
	_, concluded, err := i.cancelIfWaiting(ctx, d, w.ID, supersededPrefix+bareID(cover.ID))
	switch {
	case errors.Is(err, ErrRunFinished):
		d.Logger.Info("pipelines: a waiting push run had finished before it could be superseded; its answer stands", attrs...)
	case err != nil:
		d.Logger.Warn("pipelines: a waiting push run a newer push superseded could not be cancelled", append(attrs, "error", err)...)
	case concluded:
		d.Logger.Info("pipelines: a newer push to the default branch superseded this run", attrs...)
	}
	return concluded
}

// supersededPrefix begins the cancelledBy of every run a newer push stopped:
// "superseded by <run id>".
const supersededPrefix = "superseded by "

// normalizeSHA is a commit as this package compares commits everywhere:
// trimmed and lower-cased.
func normalizeSHA(sha string) string { return strings.ToLower(strings.TrimSpace(sha)) }

// headVerdict is what GitHub says of one commit and a head.
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
	errNoHeadNamed = errors.New("GitHub named no head")
)

// pullRequestHead asks GitHub for pull request number's head in repository and
// says whether it is sha, and what it is. The ONE place either decision -- a
// supersede (R34) or a superseded head coming back (R37) -- reads a pull
// request's head, so both compare SHAs as this package compares them
// everywhere: trimmed and lower-cased, then equal.
func pullRequestHead(ctx context.Context, d Deps, repository string, number int, sha, token string) (headVerdict, string, error) {
	if d.GitHub == nil || strings.TrimSpace(token) == "" {
		return headUnknown, "", errNoHeadToken
	}
	pr, err := d.GitHub.PullRequestHead(ctx, token, repository, number)
	if err != nil {
		return headUnknown, "", err
	}
	head := normalizeSHA(pr.HeadSHA)
	if head == "" {
		return headUnknown, "", errNoHeadNamed
	}
	if head != normalizeSHA(sha) {
		return headElsewhere, head, nil
	}
	return headHere, head, nil
}

// defaultBranchHead asks GitHub for p's default branch's head, following a
// rename (readDefaultHead), bounded by githubCallTimeout: the ONE place the
// push lane reads it -- a push superseding (coalescePush), recovery's lane pass
// (push_lane.go) and a superseded push's commit coming back (R37) -- so all
// three compare it as this package compares commits.
func defaultBranchHead(ctx context.Context, d Deps, p Pipeline, token string) (string, error) {
	if d.GitHub == nil || strings.TrimSpace(token) == "" {
		return "", errNoHeadToken
	}
	callCtx, done := context.WithTimeout(ctx, githubCallTimeout)
	defer done()
	head, err := readDefaultHead(callCtx, d, p, token)
	if err != nil {
		return "", err
	}
	if head.sha == "" {
		return "", errNoHeadNamed
	}
	return head.sha, nil
}

// defaultBranchHeadVerdict is defaultBranchHead said as pullRequestHead says a
// pull request's: whether the default branch's head is sha, and what it is.
func defaultBranchHeadVerdict(ctx context.Context, d Deps, p Pipeline, sha, token string) (headVerdict, string, error) {
	head, err := defaultBranchHead(ctx, d, p, token)
	if err != nil {
		return headUnknown, "", err
	}
	if head != normalizeSHA(sha) {
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

// isDefaultBranchHead reports whether opened's commit is p's default branch's
// head as GitHub reports it now: isPullRequestHead's rule for a push. Anything
// short of a yes is a no, and the log says which.
func isDefaultBranchHead(ctx context.Context, d Deps, p Pipeline, opened Run, token string) bool {
	attrs := []any{"component", "pipelinerun", logger.Subject(RunConcept, opened.ID),
		"repository", p.Repository, "branch", p.DefaultBranch}
	verdict, head, err := defaultBranchHeadVerdict(ctx, d, p, opened.SHA, token)
	switch {
	case verdict == headHere:
		return true
	case verdict == headElsewhere:
		d.Logger.Info("pipelines: the default branch's head has moved on since this push, so it supersedes none of the waiting push runs; the newer head's own opening supersedes",
			append(attrs, "sha", opened.SHA, "head", head)...)
	case errors.Is(err, errNoHeadToken):
		d.Logger.Warn("pipelines: there is no installation token to ask GitHub for the default branch's head, so this push supersedes none of the waiting push runs", attrs...)
	case errors.Is(err, errNoHeadNamed):
		d.Logger.Warn("pipelines: GitHub named no head for the default branch, so this push supersedes none of the waiting push runs", attrs...)
	default:
		d.Logger.Warn("pipelines: the default branch's head could not be read from GitHub, so this push supersedes none of the waiting push runs",
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
// not a re-run, which answers through its own rule -- of a pull request, from
// this repository (a fork's head runs nothing either way), or to the default
// branch. It is the one case in which the dedup asks GitHub anything (R37).
func supersededNewest(runs []Run, o Opening) (Run, bool) {
	if o.isRerun() || o.Fork || (o.PullRequest <= 0 && o.Event != pipelines.EventPush) {
		return Run{}, false
	}
	newest, attempts := newestAttempt(runs)
	if attempts == 0 || !stoppedBySupersede(newest) {
		return Run{}, false
	}
	return newest, true
}

// currentHead asks GitHub for the head o answers to -- its pull request's, or
// for a push the default branch's -- and says whether it is sha.
func currentHead(ctx context.Context, d Deps, p Pipeline, o Opening, sha, token string) (headVerdict, string, error) {
	if o.Event == pipelines.EventPush {
		return defaultBranchHeadVerdict(ctx, d, p, sha, token)
	}
	return pullRequestHead(ctx, d, p.Repository, o.PullRequest, sha, token)
}

// headCameBack reports whether GitHub names o's commit as its head NOW -- the
// pull request's, or for a push the default branch's: a force-push back to a
// commit whose run a newer push superseded -- in which case that superseded
// run does not answer the delivery and the key opens its next attempt (R37).
// It is asked before the open gate, under the token the opening minted
// (tokenErr: the mint that failed). Anything short of a yes leaves the
// superseded run answering, and the log says why.
func headCameBack(ctx context.Context, d Deps, p Pipeline, o Opening, sha, token string, tokenErr error, superseded Run) bool {
	where, whose := "the pull request's head", "the pull request"
	attrs := []any{"component", "pipelinerun", logger.Subject(RunConcept, superseded.ID), "repository", p.Repository,
		"sha", sha, "cancelledBy", superseded.CancelledBy}
	if o.Event == pipelines.EventPush {
		where, whose = "the default branch's head", "the default branch"
		attrs = append(attrs, "branch", p.DefaultBranch)
	} else {
		attrs = append(attrs, "pullRequest", o.PullRequest)
	}
	if tokenErr != nil {
		token = ""
	}
	verdict, head, err := currentHead(ctx, d, p, o, sha, token)
	switch {
	case verdict == headHere:
		d.Logger.Info("pipelines: "+where+" comes back to a commit whose run a newer push superseded, so it runs again", attrs...)
		return true
	case verdict == headElsewhere:
		d.Logger.Info("pipelines: a delivery for a head a newer push superseded is answered by its superseded run; "+where+" is elsewhere",
			append(attrs, "head", head)...)
	case errors.Is(err, errNoHeadToken):
		// The mint's own error, on this line: the opening returns at once on
		// this path, so no check-run warning records why the mint failed.
		if tokenErr != nil {
			attrs = append(attrs, "error", tokenErr)
		}
		d.Logger.Warn("pipelines: there is no installation token to ask GitHub whether "+where+" came back to this commit, so its superseded run answers the delivery", attrs...)
	case errors.Is(err, errNoHeadNamed):
		d.Logger.Warn("pipelines: GitHub named no head for "+whose+", so the superseded run answers the delivery", attrs...)
	default:
		d.Logger.Warn("pipelines: "+where+" could not be read from GitHub, so the superseded run answers the delivery",
			append(attrs, "error", err)...)
	}
	return false
}

// supersededBy reports whether opened stops r: ANOTHER run of the same
// pipeline and pull request, in affected mode, not finished. The read has
// narrowed to the pipeline, the pull request and the unfinished already; they
// are asked again here so the rule is whole in one place. The MODE is decided
// only here, where it is tested: a full run is never stopped by a push to a
// pull request, whatever pull request it carries.
func supersededBy(r, opened Run) bool {
	return !sameID(r.ID, opened.ID) &&
		sameID(r.PipelineID, opened.PipelineID) &&
		opened.PullRequest > 0 && r.PullRequest == opened.PullRequest &&
		r.Mode == pipelines.ModeAffected &&
		!r.Finished()
}
