package pipelinerun

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelinenotify"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/core/num"
)

// train_land.go -- the owner's approval and the landing (design record,
// section 7), and express trains (section 4.2).
//
// WHAT LANDS IS WHAT WAS APPROVED. A train lands only when the pipeline
// owner's latest deciding review approves the train pull request's CURRENT
// head (pipelines.ApprovalLands), main is still the commit the train was built
// on, and GitHub merges with that head as its guard. Any of those moving sends
// the train back to building, and the owner is asked again.

// awaitApproval watches a train whose run passed.
func (i *Integration) awaitApproval(ctx context.Context, d Deps, s pipelines.TrainSettings, p Pipeline, t Train, now time.Time) error {
	gh, token, _, err := trainAccess(ctx, d, p)
	if err != nil {
		return err
	}
	if done, err := i.settleClosedTrain(ctx, d, s, p, t, gh, token); done || err != nil {
		return err
	}
	rebuilt, err := i.rebuildIfAsked(ctx, d, s, p, t, gh, token)
	if err != nil || rebuilt {
		return err
	}
	login, err := gh.OwnerLogin(ctx, p.CredentialID, p.OwnerUserID, p.Repository)
	if err != nil {
		return err
	}
	reviews, err := gh.PullRequestReviews(ctx, token, p.Repository, t.PullRequest)
	if err != nil {
		return err
	}
	decided := make([]pipelines.TrainReview, 0, len(reviews))
	for _, r := range reviews {
		decided = append(decided, pipelines.TrainReview{Login: r.Login, State: r.State, CommitID: r.CommitID, SubmittedAt: r.SubmittedAt})
	}
	if pipelines.ApprovalLands(decided, login, t.HeadSHA) {
		approver := login
		for _, r := range decided {
			if strings.EqualFold(r.Login, login) && r.State == "APPROVED" && r.CommitID == t.HeadSHA {
				approver = r.Login
			}
		}
		updated, ok, err := i.casTrain(ctx, d, p, t, TrainPatch{State: ptr(TrainLanding), ApprovedBy: ptr(approver), ApprovedSHA: ptr(t.HeadSHA)})
		if err != nil || !ok {
			return err
		}
		return i.land(ctx, d, s, p, updated)
	}
	if _, due := pipelines.DepartureDue(s, t.DepartedAt, now); due {
		return i.notifyTrain(ctx, d, s, p, t, pipelinenotify.TrainReminder,
			fmt.Sprintf("This train passed and has waited since %s for your approval.", t.DepartedAt.UTC().Format("Jan 2 15:04 MST")), nil)
	}
	return nil
}

// land merges an approved train and tells the pull requests it carried. A
// train found in `landing` on a later tick -- its replica died between the
// approval and the merge -- resumes here, and one already merged is only
// recorded.
func (i *Integration) land(ctx context.Context, d Deps, s pipelines.TrainSettings, p Pipeline, t Train) error {
	gh, token, _, err := trainAccess(ctx, d, p)
	if err != nil {
		return err
	}
	if done, err := i.settleClosedTrain(ctx, d, s, p, t, gh, token); done || err != nil {
		return err
	}
	head, _, err := d.GitHub.BranchHead(ctx, token, p.Repository, p.DefaultBranch)
	if err != nil {
		return err
	}
	if head != t.BaseSHA {
		updated, ok, err := i.casTrain(ctx, d, p, t, TrainPatch{State: ptr(TrainBuilding), Rebuilds: ptr(t.Rebuilds + 1), ApprovedBy: ptr(""), ApprovedSHA: ptr("")})
		if err != nil || !ok {
			return err
		}
		return i.build(ctx, d, s, p, updated)
	}
	sha, err := gh.MergePullRequest(ctx, token, p.Repository, t.PullRequest, t.HeadSHA)
	if err != nil {
		status := githubapp.StatusOf(err)
		if status != http.StatusConflict && status != http.StatusMethodNotAllowed {
			return err
		}
		// GitHub refused the merge. It may have refused it because the
		// pull request is already merged -- another tick, or the owner, got
		// there first -- and then the train landed.
		if done, err := i.settleClosedTrain(ctx, d, s, p, t, gh, token); done || err != nil {
			return err
		}
		return i.hold(ctx, d, s, p, t, fmt.Sprintf("GitHub would not merge the approved train (%d). Its head or main moved, or a rule refused it.", status), nil, "")
	}
	return i.recordLanded(ctx, d, s, p, t, gh, token, sha)
}

// settleClosedTrain reads the train pull request and finishes a train whose
// pull request is no longer open: merged, it landed -- by the App, by the
// owner on GitHub, or on a tick that died before recording it -- and closed,
// it is cancelled. It reports whether it finished the train.
func (i *Integration) settleClosedTrain(ctx context.Context, d Deps, s pipelines.TrainSettings, p Pipeline, t Train, gh trainGitHub, token string) (bool, error) {
	if t.PullRequest <= 0 {
		return false, nil
	}
	st, err := gh.PullRequestState(ctx, token, p.Repository, t.PullRequest)
	if err != nil {
		return false, err
	}
	switch {
	case st.Merged:
		return true, i.recordLanded(ctx, d, s, p, t, gh, token, st.MergeCommitSHA)
	case st.State != "open":
		return true, i.cancelTrain(ctx, d, s, p, t, gh, token, "The train pull request was closed.", false)
	}
	return false, nil
}

// recordLanded records a merged train and tells everyone it carried: a
// comment on each pull request on board naming the commit that boarded and
// the merge commit, the branch deleted, and the landed message.
func (i *Integration) recordLanded(ctx context.Context, d Deps, s pipelines.TrainSettings, p Pipeline, t Train, gh trainGitHub, token, mergeSHA string) error {
	updated, ok, err := i.casTrain(ctx, d, p, t, TrainPatch{State: ptr(TrainLanded), LandedSHA: ptr(mergeSHA)})
	if err != nil || !ok {
		return err
	}
	as := ""
	if mergeSHA != "" {
		as = fmt.Sprintf(" (merged as `%s`)", shortCommit(mergeSHA))
	}
	for _, car := range t.OnBoard {
		_ = gh.CommentOnPullRequest(ctx, token, p.Repository, car.Number, fmt.Sprintf(
			"Landed on main in %s at `%s`%s.", t.Branch, shortCommit(car.SHA), as))
	}
	_ = gh.DeleteBranchRef(ctx, token, p.Repository, t.Branch)
	reason := "Landed " + trainCount(len(t.OnBoard)) + " on main."
	if mergeSHA != "" {
		reason = fmt.Sprintf("Landed %s on main as %s.", trainCount(len(t.OnBoard)), shortCommit(mergeSHA))
	}
	return i.notifyTrain(ctx, d, s, p, updated, pipelinenotify.TrainLanded, reason, nil)
}

// cancelTrain records a train cancelled, then retires what it left on GitHub
// and tells the owner. comment says whether the train pull request is told
// why: not when the owner closed it themselves.
func (i *Integration) cancelTrain(ctx context.Context, d Deps, s pipelines.TrainSettings, p Pipeline, t Train, gh trainGitHub, token, reason string, comment bool) error {
	cancelled, ok, err := i.casTrain(ctx, d, p, t, TrainPatch{State: ptr(TrainCancelled), HeldReason: ptr(reason)})
	if err != nil || !ok {
		return err
	}
	i.retireCancelled(ctx, d, s, p, cancelled, gh, token, reason, comment)
	return nil
}

// retireCancelled is what follows any cancellation: its branch deleted (which
// closes a pull request still open), and the cancelled message when the owner
// has seen the train's pull request, so a train that left or asked for an
// approval never goes quiet.
func (i *Integration) retireCancelled(ctx context.Context, d Deps, s pipelines.TrainSettings, p Pipeline, cancelled Train, gh trainGitHub, token, reason string, comment bool) {
	target := cancelled
	if !comment {
		target.PullRequest = 0
	}
	retireTrain(ctx, d, gh, token, p, target, reason)
	if cancelled.PullRequest > 0 {
		_ = i.notifyTrain(ctx, d, s, p, cancelled, pipelinenotify.TrainCancelled, reason, nil)
	}
}

// TrainExpress starts an express train carrying only the named pull requests
// (spec 4.2). Only the pipeline's owner may: it is their repository's main.
// It refuses, changing nothing, when none of them can board. It goes ahead of
// a train that is waiting for approval or held: that train is cancelled, and
// recorded as the one the express train Replaces, so its pull requests leave
// on a new train as soon as the express train finishes (departScheduled). It
// refuses while a train is building or running, rather than abandoning a run
// in flight.
func (i *Integration) TrainExpress(ctx context.Context, repository string, numbers []int) (Train, error) {
	caller, err := personFrom(ctx)
	if err != nil {
		return Train{}, err
	}
	d := i.snapshot()
	if d.Store == nil {
		return Train{}, errNoStore
	}
	requested := uniquePositive(numbers)
	if len(requested) == 0 {
		return Train{}, fmt.Errorf("pipelines: an express train needs at least one pull request number")
	}
	raw := ""
	if d.SystemVariable != nil {
		if raw, err = d.SystemVariable(ctx, pipelines.TrainSettingsVariable); err != nil {
			return Train{}, err
		}
	}
	settings, err := pipelines.ParseTrainSettings(raw)
	if err != nil {
		return Train{}, err
	}
	s, ok := settings[strings.ToLower(strings.TrimSpace(repository))]
	if !ok {
		return Train{}, fmt.Errorf("pipelines: release trains are not configured for %s; add it to %s", repository, pipelines.TrainSettingsVariable)
	}
	p, err := trainPipeline(ctx, d, s.Repository)
	if err != nil {
		return Train{}, err
	}
	if !sameID(p.OwnerUserID, caller) {
		return Train{}, fmt.Errorf("pipelines: only the owner of %s's pipeline can start an express train", p.Repository)
	}
	loc, err := time.LoadLocation(s.TimeZone)
	if err != nil {
		return Train{}, err
	}
	// Board before anything is cancelled: an express train that could carry
	// nothing must leave the waiting train as it was, and say why for each
	// number named.
	gh, token, appID, err := trainAccess(ctx, d, p)
	if err != nil {
		return Train{}, err
	}
	prs, err := gh.TrainPullRequests(ctx, token, p.Repository, p.DefaultBranch)
	if err != nil {
		return Train{}, err
	}
	board, skipped, err := boardTrain(ctx, gh, token, appID, p, Train{Requested: requested}, prs)
	if err != nil {
		return Train{}, err
	}
	if len(board) == 0 {
		reasons := make([]string, 0, len(skipped))
		for _, sk := range skipped {
			reasons = append(reasons, fmt.Sprintf("#%d %s", sk.Number, sk.Reason))
		}
		return Train{}, fmt.Errorf("pipelines: no pull request named can board an express train: %s", strings.Join(reasons, "; "))
	}
	now := d.Now().UTC()
	day := now.In(loc)
	startOfDay := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, loc)
	var created Train
	var replaced *Train
	err = d.gate(ctx, TrainGateKey(p.Repository), func(gctx context.Context) error {
		latest, err := d.Store.LatestTrain(memql.ContextWithFreshRead(gctx), p.ID)
		if err != nil {
			return err
		}
		if latest != nil && !latest.Finished() {
			if latest.State != TrainAwaitingApproval && latest.State != TrainHeld {
				return fmt.Errorf("pipelines: %s is %s; start the express train when it has landed or is held", latest.Branch, latest.State)
			}
			at := now
			if !at.After(latest.UpdatedAt) {
				at = latest.UpdatedAt.Add(time.Microsecond)
			}
			if err := d.Store.UpdateTrain(gctx, latest.OwnerUserID, latest.ID, TrainPatch{
				State: ptr(TrainCancelled), HeldReason: ptr("Replaced by an express train."), UpdatedAt: &at,
			}); err != nil {
				return err
			}
			replaced = latest
		}
		today, err := d.Store.TrainsSince(memql.ContextWithFreshRead(gctx), p.ID, startOfDay)
		if err != nil {
			return err
		}
		branch := pipelines.TrainBranch(day, len(today)+1)
		created = Train{
			ID: TrainIDFor(p.ID, branch), OwnerUserID: p.OwnerUserID, AccountID: p.AccountID, PipelineID: p.ID,
			Repository: p.Repository, Branch: branch, Departure: TrainDepartureExpress, State: TrainBuilding,
			Requested: requested, DepartedAt: now, UpdatedAt: now,
		}
		if replaced != nil {
			created.Replaces = replaced.ID
		}
		return d.Store.CreateTrain(gctx, p.OwnerUserID, created)
	})
	if err != nil {
		return Train{}, err
	}
	if replaced != nil {
		// The replaced train's pull request would otherwise stay open beside
		// the express train's, for the owner to approve a train that can no
		// longer land.
		view := *replaced
		view.State = TrainCancelled
		i.retireCancelled(ctx, d, s, p, view, gh, token, "Replaced by express train "+created.Branch+". Its pull requests leave on a new train as soon as the express train finishes.", true)
	}
	if err := i.build(ctx, d, s, p, created); err != nil {
		return Train{}, err
	}
	built, err := d.Store.LatestTrain(memql.ContextWithFreshRead(ctx), p.ID)
	if err != nil || built == nil {
		return created, err
	}
	return *built, nil
}

func uniquePositive(numbers []int) []int {
	var out []int
	for _, n := range numbers {
		if n > 0 && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}

func trainCount(n int) string {
	if n == 1 {
		return "1 pull request"
	}
	return fmt.Sprintf("%d pull requests", n)
}

// handleTrainTick is the one-minute automation's step: internal origin only
// (TrainTick checks it).
func (i *Integration) handleTrainTick(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	res, err := i.TrainTick(handlerContext(ctx))
	if err != nil {
		return nil, err
	}
	return resultNode(map[string]any{
		"repositories": res.Repositories,
		"departed":     res.Departed,
		"advanced":     res.Advanced,
		"notes":        res.Notes,
	}), nil
}

// handleTrainExpress starts an express train for the calling owner.
func (i *Integration) handleTrainExpress(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	t, err := i.TrainExpress(handlerContext(ctx), stringArg(args, "repository"), intListArg(args, "pullRequests"))
	if err != nil {
		return nil, err
	}
	var boarded []int
	for _, c := range t.OnBoard {
		boarded = append(boarded, c.Number)
	}
	return resultNode(map[string]any{
		"trainId":     t.ID,
		"branch":      t.Branch,
		"state":       t.State,
		"pullRequest": t.PullRequest,
		"onBoard":     boarded,
	}), nil
}

// intListArg reads a list of whole numbers: the engine hands a JSON number
// as float64, and a fraction or a non-number is dropped rather than rounded.
//
// narrowing: ZERO -- every caller passes the list through uniquePositive,
// which reads 0 as "not a pull request", so a number no int holds is dropped
// with the fractions instead of saturating into a pull request that does not
// exist.
func intListArg(args map[string]any, key string) []int {
	list, _ := args[key].([]any)
	var out []int
	for _, v := range list {
		switch n := v.(type) {
		case float64:
			if whole, ok := num.WholeInt64(n); ok {
				out = append(out, num.Int64OrZero(whole))
			}
		case int:
			out = append(out, n)
		case int64:
			out = append(out, num.Int64OrZero(n))
		}
	}
	return slices.DeleteFunc(out, func(n int) bool { return n == 0 })
}
