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
	gh, token, err := trainAccess(ctx, d, p)
	if err != nil {
		return err
	}
	open, merged, err := gh.PullRequestOpen(ctx, token, p.Repository, t.PullRequest)
	if err != nil {
		return err
	}
	switch {
	case merged:
		// Merged by hand: what landed is the owner's own doing.
		_, _, err := i.casTrain(ctx, d, p, t, TrainPatch{State: ptr(TrainLanded)})
		return err
	case !open:
		_, _, err := i.casTrain(ctx, d, p, t, TrainPatch{State: ptr(TrainCancelled), HeldReason: ptr("The train pull request was closed.")})
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
	gh, token, err := trainAccess(ctx, d, p)
	if err != nil {
		return err
	}
	open, merged, err := gh.PullRequestOpen(ctx, token, p.Repository, t.PullRequest)
	if err != nil {
		return err
	}
	landed := t.LandedSHA
	if !merged {
		if !open {
			_, _, err := i.casTrain(ctx, d, p, t, TrainPatch{State: ptr(TrainCancelled), HeldReason: ptr("The train pull request was closed before it landed.")})
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
			if status := githubapp.StatusOf(err); status == http.StatusConflict || status == http.StatusMethodNotAllowed {
				return i.hold(ctx, d, s, p, t, fmt.Sprintf("GitHub would not merge the approved train (%d). Its head or main moved, or a rule refused it.", status), nil)
			}
			return err
		}
		landed = sha
	}
	updated, ok, err := i.casTrain(ctx, d, p, t, TrainPatch{State: ptr(TrainLanded), LandedSHA: ptr(landed)})
	if err != nil || !ok {
		return err
	}
	for _, car := range t.OnBoard {
		_ = gh.CommentOnPullRequest(ctx, token, p.Repository, car.Number, fmt.Sprintf(
			"Landed on main in %s at `%s` (merged as `%s`).", t.Branch, shortCommit(car.SHA), shortCommit(landed)))
	}
	_ = gh.DeleteBranchRef(ctx, token, p.Repository, t.Branch)
	return i.notifyTrain(ctx, d, s, p, updated, pipelinenotify.TrainLanded,
		fmt.Sprintf("Landed %s on main as %s.", trainCount(len(t.OnBoard)), shortCommit(landed)), nil)
}

// TrainExpress starts an express train carrying only the named pull requests
// (spec 4.2). Only the pipeline's owner may: it is their repository's main.
// It goes ahead of a train that is waiting for approval or held, which is
// cancelled (its pull requests board the next train); it refuses while a
// train is building or running, rather than abandoning a run in flight.
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
	now := d.Now().UTC()
	day := now.In(loc)
	startOfDay := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, loc)
	var created Train
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
		return d.Store.CreateTrain(gctx, p.OwnerUserID, created)
	})
	if err != nil {
		return Train{}, err
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
func intListArg(args map[string]any, key string) []int {
	list, _ := args[key].([]any)
	var out []int
	for _, v := range list {
		switch n := v.(type) {
		case float64:
			if n == float64(int(n)) {
				out = append(out, int(n))
			}
		case int:
			out = append(out, n)
		case int64:
			out = append(out, int(n))
		}
	}
	return out
}
