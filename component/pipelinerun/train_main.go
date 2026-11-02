package pipelinerun

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelinenotify"
	"github.com/znasllc-io/memql/component/pipelines"
)

// train_main.go -- a train from main (design record
// docs/superpowers/specs/2026-11-02-nightly-release-from-main-design.md, sections
// 4 and 5). Changes land on main directly, so a train carries main as it stands,
// plus its version: it has no pull requests on board, nothing to drop, and no reason
// to rebuild. Everything else is the train's: the record, the lease, the
// compare-and-swap and the messages.

const (
	// trainMainCommitsListed bounds the commits a train's pull request names, and
	// how many a held train looks through for a place to start.
	trainMainCommitsListed = 50
	// trainLastLandedWindow is how far back a train looks for the last release. A
	// landed train older than this reads as none: the next train releases main.
	trainLastLandedWindow = 90 * 24 * time.Hour
)

// lastLandedTrain is the newest train that landed, or nil.
func lastLandedTrain(ctx context.Context, d Deps, p Pipeline) (*Train, error) {
	trains, err := d.Store.TrainsSince(memql.ContextWithFreshRead(ctx), p.ID, d.Now().Add(-trainLastLandedWindow))
	if err != nil {
		return nil, err
	}
	for n := range trains {
		if trains[n].State == TrainLanded && trains[n].LandedSHA != "" {
			return &trains[n], nil
		}
	}
	return nil, nil
}

// buildFromMain makes the train branch from main's head and the version, and
// opens (or adopts) its pull request. Like build it works on a scratch ref and
// moves the train branch once, and it is safe to run again: a build abandoned at
// any point is claimed by a later tick and starts over from main.
func (i *Integration) buildFromMain(ctx context.Context, d Deps, s pipelines.TrainSettings, p Pipeline, t Train) error {
	gh, token, _, err := trainAccess(ctx, d, p)
	if err != nil {
		return err
	}
	base, _, err := d.GitHub.BranchHead(ctx, token, p.Repository, p.DefaultBranch)
	if err != nil {
		return err
	}
	last, err := lastLandedTrain(ctx, d, p)
	if err != nil {
		return err
	}
	if last != nil && last.LandedSHA == base {
		return i.cancelBuild(ctx, d, s, gh, token, p, t, base, nil, "Nothing new on "+p.DefaultBranch+" since the last release.")
	}

	scratch := pipelines.TrainScratchBranch(t.Branch)
	if err := moveBranch(ctx, gh, token, p, scratch, base); err != nil {
		return err
	}
	defer func() {
		if err := gh.DeleteBranchRef(ctx, token, p.Repository, scratch); err != nil && githubapp.StatusOf(err) != http.StatusUnprocessableEntity && githubapp.StatusOf(err) != http.StatusNotFound {
			trainLog(d).Warn("pipelines: a train's scratch ref was not deleted", "repository", p.Repository, "ref", scratch, "error", err.Error())
		}
	}()
	version, head, err := trainVersionFromMain(ctx, gh, token, p, t, scratch, base, last)
	if err != nil {
		return err
	}
	if err := moveBranch(ctx, gh, token, p, t.Branch, head); err != nil {
		return err
	}

	var commits []githubapp.ComparedCommit
	total := 0
	if last != nil {
		// The list is for the reader. A comparison GitHub would not give is no
		// reason to stop a release.
		if commits, total, err = gh.CompareCommits(ctx, token, p.Repository, last.LandedSHA, base); err != nil {
			trainLog(d).Warn("pipelines: a train's commits were not listed", "repository", p.Repository, "branch", t.Branch, "error", err.Error())
			commits, total = nil, 0
		}
	}
	body := trainMainPullRequestBody(p, t, base, version, commits, total, s.LandsAutomatically())
	number, url := t.PullRequest, t.PullRequestURL
	if number == 0 {
		// A build that died after GitHub opened its pull request left one open
		// for this branch: adopt it rather than fail to open a second.
		prs, err := gh.TrainPullRequests(ctx, token, p.Repository, p.DefaultBranch)
		if err != nil {
			return err
		}
		for _, pr := range prs {
			if pr.HeadRef == t.Branch {
				number, url = pr.Number, pr.URL
			}
		}
	}
	if number == 0 {
		number, url, err = gh.CreateTrainPullRequest(ctx, token, p.Repository, t.Branch, p.DefaultBranch, "Train "+t.Branch, body)
	} else {
		err = gh.EditPullRequestBody(ctx, token, p.Repository, number, body)
	}
	if err != nil {
		return err
	}
	updated, ok, err := i.casTrain(ctx, d, p, t, TrainPatch{
		State: ptr(TrainRunning), BaseSHA: ptr(base), HeadSHA: ptr(head), Version: ptr(version), PullRequest: ptr(number),
		PullRequestURL: ptr(url), RunID: ptr(""), Retried: ptr(false), HeldReason: ptr(""),
	})
	if err != nil || !ok {
		return err
	}
	return i.notifyTrain(ctx, d, s, p, updated, pipelinenotify.TrainDeparted, "", func(m *pipelinenotify.TrainMessage) { m.Commits = total })
}

// trainVersionFromMain gives the train its one commit and its version.
//
// The usual case is the patch bump. A VERSION a person changed on main since the
// last release is theirs, and is kept: that is how a minor version happens. So is
// a repository with no VERSION, or one that is not MAJOR.MINOR.PATCH: a train
// never guesses over a version a person chose. Those two have no file to change,
// so the train's commit is an empty one, because GitHub opens no pull request
// for a branch that is where its base is.
func trainVersionFromMain(ctx context.Context, gh trainGitHub, token string, p Pipeline, t Train, scratch, base string, last *Train) (version, head string, err error) {
	raw, err := gh.FileAtRef(ctx, token, p.Repository, base, "VERSION")
	if err != nil && githubapp.StatusOf(err) != http.StatusNotFound {
		return "", "", err
	}
	current := ""
	next := ""
	if err == nil {
		current = strings.TrimSpace(string(raw))
		if n, perr := pipelines.NextPatchVersion(current); perr == nil {
			next = n
		}
	}
	chosen := last != nil && last.Version != "" && current != last.Version
	if next == "" || chosen {
		sha, err := gh.AddEmptyCommit(ctx, token, p.Repository, scratch, "chore: release "+t.Branch)
		return current, sha, err
	}
	sha, err := gh.PutBranchFile(ctx, token, p.Repository, scratch, "VERSION", "chore: prepare "+next+" for "+t.Branch, []byte(next+"\n"))
	return next, sha, err
}

// trainMainPullRequestBody is the consolidated pull request's description. A
// commit subject is the repository's text, so it goes in a code span: GitHub
// reads no mention inside one, and a dependency's "@types/node" pings nobody.
func trainMainPullRequestBody(p Pipeline, t Train, base, version string, commits []githubapp.ComparedCommit, total int, automatic bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Release built by MemQL from `%s` at `%s`.", p.DefaultBranch, shortCommit(base))
	if automatic {
		b.WriteString(" It lands by itself when its run passes.")
	} else {
		b.WriteString(" Approve this pull request to land it; nothing lands without that approval.")
	}
	b.WriteString("\n\nA failure stops it here. Push the fix to this branch and its run starts again. Close this pull request to cancel the release.\n")
	if total > 0 {
		b.WriteString("\n**Commits since the last release**\n\n")
		shown := min(len(commits), trainMainCommitsListed)
		for _, c := range commits[:shown] {
			fmt.Fprintf(&b, "- `%s` `%s`\n", shortCommit(c.SHA), strings.ReplaceAll(oneLine(c.Subject), "`", "'"))
		}
		if total > shown {
			fmt.Fprintf(&b, "- and %d more\n", total-shown)
		}
	}
	if version != "" {
		fmt.Fprintf(&b, "\nVersion: `%s`\n", version)
	}
	return b.String()
}

// landAutomatically takes a train whose run passed straight to landing, recording
// the setting that allowed it where a person's approval would be.
func (i *Integration) landAutomatically(ctx context.Context, d Deps, s pipelines.TrainSettings, p Pipeline, t Train, runID string) error {
	updated, ok, err := i.casTrain(ctx, d, p, t, TrainPatch{State: ptr(TrainLanding), RunID: ptr(runID),
		ApprovedBy: ptr(pipelines.TrainAutomaticApprover), ApprovedSHA: ptr(t.HeadSHA)})
	if err != nil || !ok {
		return err
	}
	return i.land(ctx, d, s, p, updated)
}

// followTrainHead notices a person's push to a train from main's branch: the fix
// for what stopped it, or main merged in so it can land. GitHub's synchronize
// opens the new head's run, and the train reads it from running. It may retry once
// again: that head has not failed yet. It answers whether the train moved.
//
// A branch that is gone says nothing here: settleClosedTrain, which runs first,
// reads what became of its pull request.
func (i *Integration) followTrainHead(ctx context.Context, d Deps, s pipelines.TrainSettings, p Pipeline, t Train, token string) (bool, error) {
	if !s.FromMain() {
		return false, nil
	}
	head, _, err := d.GitHub.BranchHead(ctx, token, p.Repository, t.Branch)
	if err != nil {
		if githubapp.StatusOf(err) == http.StatusNotFound {
			return false, nil
		}
		return false, err
	}
	if head == "" || head == t.HeadSHA {
		return false, nil
	}
	_, ok, err := i.casTrain(ctx, d, p, t, TrainPatch{
		State: ptr(TrainRunning), HeadSHA: ptr(head), RunID: ptr(""), Retried: ptr(false), HeldReason: ptr(""),
		ApprovedBy: ptr(""), ApprovedSHA: ptr(""),
	})
	return ok, err
}

// triageMain decides a failed run of a train from main.
func (i *Integration) triageMain(ctx context.Context, d Deps, s pipelines.TrainSettings, p Pipeline, t Train, run Run, failed []pipelines.TrainFailedStep, keys []string) error {
	verdict := pipelines.TriageMainTrainFailure(failed, t.Retried)
	if verdict.Action == "retry" {
		// The same failed-only re-run a person gets (see triage).
		if _, err := i.open(ctx, d, p, rerunOf(run, "", true)); err != nil {
			return err
		}
		updated, ok, err := i.casTrain(ctx, d, p, t, TrainPatch{Retried: ptr(true), RunID: ptr(run.ID)})
		if err != nil || !ok {
			return err
		}
		return i.notifyTrain(ctx, d, s, p, updated, pipelinenotify.TrainRetrying, verdict.Reason, func(m *pipelinenotify.TrainMessage) { m.Failed = keys })
	}
	suspects := i.trainSuspects(ctx, d, p, t, failed)
	return i.hold(ctx, d, s, p, t, verdict.Reason, keys, run.ID, func(m *pipelinenotify.TrainMessage) { m.Suspects = suspects })
}

// trainSuspects names the commits since the last release that can reach what
// failed, as a place for a person to start. It is a courtesy: anything it cannot
// read costs the message a line, never the hold it is part of.
func (i *Integration) trainSuspects(ctx context.Context, d Deps, p Pipeline, t Train, failed []pipelines.TrainFailedStep) []string {
	note := func(err error) []string {
		trainLog(d).Warn("pipelines: a held train's commits were not examined", "repository", p.Repository, "branch", t.Branch, "error", err.Error())
		return nil
	}
	gh, token, _, err := trainAccess(ctx, d, p)
	if err != nil {
		return note(err)
	}
	last, err := lastLandedTrain(ctx, d, p)
	if err != nil {
		return note(err)
	}
	if last == nil {
		return nil
	}
	commits, total, err := gh.CompareCommits(ctx, token, p.Repository, last.LandedSHA, t.BaseSHA)
	if err != nil {
		return note(err)
	}
	if total > trainMainCommitsListed || len(commits) > trainMainCommitsListed {
		return []string{fmt.Sprintf("%d commits since the last release: too many to examine", total)}
	}
	graph, fullGlobs := trainGraph(ctx, d, token, p, t)
	examined := make([]pipelines.TrainCommit, 0, len(commits))
	for _, c := range commits {
		files, complete, err := gh.CompareFiles(ctx, token, p.Repository, c.Parent, c.SHA)
		if err != nil {
			return note(err)
		}
		examined = append(examined, pipelines.TrainCommit{SHA: c.SHA, Subject: c.Subject, Files: files, Complete: complete})
	}
	var out []string
	for _, c := range pipelines.SuspectCommits(graph, examined, failed, fullGlobs) {
		out = append(out, shortCommit(c.SHA)+" "+c.Subject)
	}
	return out
}
