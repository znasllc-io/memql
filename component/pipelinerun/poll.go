package pipelinerun

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strconv"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/core/logger"
)

// poll.go -- the polling delivery (D4, D11), for a cluster GitHub cannot
// reach: every minute, each delivery=poll pipeline's default-branch head and
// open pull-request heads, against what the pipeline row says was seen.
//
// THE FIRST POLL RECORDS A BASELINE AND OPENS NOTHING (decision 13), so
// connecting a pipeline never floods a repository's history with runs; runs
// start with the next change, which is how webhook delivery behaves by
// construction. A later poll synthesizes the event a webhook would have
// carried -- `push` for the default branch, `pull_request` for a pull
// request's head -- so a poll and a webhook for one head compute one run key
// and open one run.

// pollPageLimit is pipelinesPolled's page. A page that comes back full is
// said out loud: the poll is then not seeing every pipeline.
const pollPageLimit = 500

// PollResult is what one poll did.
type PollResult struct {
	Pipelines int
	Baselines int
	Opened    []Run
	Existing  []Run
	Failed    []SkippedPipeline
	// RecoverError is the recovery hook's failure, when it ran and failed.
	RecoverError string
}

func (i *Integration) handlePoll(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	res, err := i.Poll(handlerContext(ctx))
	if err != nil {
		return nil, err
	}
	answer := map[string]any{
		"pipelines": res.Pipelines,
		"baselines": res.Baselines,
		"opened":    runIDs(res.Opened),
		"existing":  runIDs(res.Existing),
		"failed":    res.Failed,
	}
	if res.RecoverError != "" {
		answer["recoverError"] = res.RecoverError
	}
	return resultNode(answer), nil
}

// Poll walks every polled pipeline, then calls the recovery hook. One
// pipeline that cannot be polled -- a revoked grant, GitHub down -- is
// reported and does not stop the others. It is the schedule's alone: a call
// that did not arrive with internal origin is refused (ErrClientOrigin)
// before anything is read.
func (i *Integration) Poll(ctx context.Context) (PollResult, error) {
	if err := requireInternalOrigin(ctx, "poll"); err != nil {
		return PollResult{}, err
	}
	d := i.snapshot()
	if d.Store == nil {
		return PollResult{}, errNoStore
	}
	polled, err := d.Store.PipelinesPolled(ctx)
	if err != nil {
		return PollResult{}, err
	}
	if len(polled) >= pollPageLimit {
		d.Logger.Warn("pipelines: the poll read a full page of pipelines; any beyond it are not polled this minute",
			"component", "pipelinerun", "pipelines", len(polled))
	}
	res := PollResult{Pipelines: len(polled)}
	for _, p := range polled {
		outcome, err := i.pollOne(ctx, d, p)
		res.Opened = append(res.Opened, outcome.opened...)
		res.Existing = append(res.Existing, outcome.existing...)
		if outcome.baseline {
			res.Baselines++
		}
		if err != nil {
			d.Logger.Warn("pipelines: a pipeline could not be polled",
				"component", "pipelinerun", logger.Subject(PipelineConcept, p.ID), "repository", p.Repository, "error", err)
			res.Failed = append(res.Failed, SkippedPipeline{PipelineID: p.ID, Reason: err.Error()})
		}
	}
	if d.Recover != nil {
		if rerr := d.Recover(ctx); rerr != nil {
			d.Logger.Warn("pipelines: recovering unclaimed and stranded runs failed",
				"component", "pipelinerun", "error", rerr)
			res.RecoverError = rerr.Error()
		}
	}
	return res, nil
}

type pollOutcome struct {
	opened   []Run
	existing []Run
	baseline bool
}

// pollOne polls one pipeline, and HOLDS NO GATE WHILE IT TALKS TO GITHUB:
// a fresh read of the row, then the token, the default branch's head and the
// open pull requests asked with no gate held; then the runs a moved head asks
// for, each opened under its own run key's gate and no other; then the heads
// written back under the repository's gate as one short compare-and-swap
// (writeHeads). A head whose run could not be opened keeps its PREVIOUS
// value, so the next poll asks again rather than the change being lost.
func (i *Integration) pollOne(ctx context.Context, d Deps, listed Pipeline) (pollOutcome, error) {
	var out pollOutcome
	p, err := d.Store.PipelineByID(memql.ContextWithFreshRead(ctx), listed.ID)
	if err != nil {
		return out, err
	}
	if p == nil || !p.Active() || p.Delivery != DeliveryPoll {
		return out, nil // disconnected or switched to webhooks since the list was read
	}
	if d.GitHub == nil {
		return out, errNoGitHub
	}
	token, _, err := d.GitHub.InstallationToken(ctx, p.CredentialID, p.OwnerUserID, p.Repository)
	if err != nil {
		return out, err
	}
	head, err := readDefaultHead(ctx, d, *p, token)
	if err != nil {
		return out, err
	}
	// The pull requests are a separate question with an answer of their
	// own: a list GitHub would not give does not stop the default branch,
	// whose head is already read. It is reported with the poll's other
	// failures, and every pull request's head stays as the last poll left
	// it -- a read that failed says nothing about which are closed.
	var failures []error
	pulls, pullsErr := d.GitHub.OpenPullRequests(ctx, token, p.Repository)
	if pullsErr != nil {
		failures = append(failures, fmt.Errorf("reading open pull requests: %w", pullsErr))
		pulls = nil
	}

	branchKey := headKeyBranch(head.branch)
	seen := map[string]string{branchKey: head.sha}
	for _, pr := range pulls {
		if pr.Number <= 0 || strings.TrimSpace(pr.HeadSHA) == "" {
			continue
		}
		seen[headKeyPR(pr.Number)] = strings.ToLower(strings.TrimSpace(pr.HeadSHA))
	}
	// branchPatch records a default branch that moved on GitHub since the
	// row was written, so the next poll asks for the right one.
	var branchPatch *string
	if head.renamed {
		branchPatch = ptr(head.branch)
	}

	if len(p.Heads) == 0 {
		// The baseline: record, open nothing -- and only when BOTH reads
		// answered. A baseline missing the pull requests would make every
		// open one look new at the next poll, and open a run for each: the
		// flood the baseline exists to prevent.
		if pullsErr != nil {
			return out, errors.Join(failures...)
		}
		written, err := i.writeHeads(ctx, d, *p, seen, branchPatch)
		out.baseline = written
		return out, err
	}

	next := maps.Clone(seen) // closed pull requests' keys drop out here
	if pullsErr != nil {
		for key, sha := range p.Heads {
			if strings.HasPrefix(key, headPRPrefix) {
				next[key] = sha
			}
		}
	}
	openOrKeep := func(key string, o Opening) {
		res, err := i.openWithToken(ctx, d, *p, o, token)
		if err != nil {
			failures = append(failures, err)
			if prev, ok := p.Heads[key]; ok {
				next[key] = prev
			} else {
				delete(next, key)
			}
			return
		}
		if res.Opened {
			out.opened = append(out.opened, res.Run)
		} else {
			out.existing = append(out.existing, res.Run)
		}
	}

	// The default branch: a head that MOVED is a push. A branch key the
	// heads do not hold yet -- the default branch was renamed since the
	// last poll -- is a baseline for that branch, which is what a webhook
	// does too: a rename is not a push.
	if prev, ok := p.Heads[branchKey]; ok && prev != seen[branchKey] {
		openOrKeep(branchKey, Opening{
			Event: pipelines.EventPush, SHA: seen[branchKey], BaseSHA: prev, Branch: head.branch,
			Title: firstLine(head.message), Trigger: TriggerPoll,
		})
	}
	// Pull requests: a NEW one, or one whose head moved, is what a
	// webhook's opened or synchronize would have carried.
	for _, pr := range pulls {
		key := headKeyPR(pr.Number)
		sha, ok := seen[key]
		if !ok || p.Heads[key] == sha {
			continue
		}
		headRepo := strings.TrimSpace(pr.HeadRepository)
		openOrKeep(key, Opening{
			Event: pipelines.EventPullRequest, SHA: sha, BaseSHA: pr.BaseSHA, Branch: pr.HeadRef,
			Title: firstLine(pr.Title), PullRequest: pr.Number,
			// D6, exactly as the delivery reads it: a head in another
			// repository, or one GitHub reports deleted, is a fork.
			Fork:           headRepo == "" || !strings.EqualFold(headRepo, p.Repository),
			HeadRepository: headRepo,
			Trigger:        TriggerPoll,
		})
	}

	if !maps.Equal(next, p.Heads) || branchPatch != nil {
		if _, err := i.writeHeads(ctx, d, *p, next, branchPatch); err != nil {
			failures = append(failures, err)
		}
	}
	return out, errors.Join(failures...)
}

// polledHead is a pipeline's default branch as GitHub reports it now.
type polledHead struct {
	branch, sha, message string
	// renamed: the branch is not the one the row names -- it was renamed on
	// GitHub since connect -- so the row's defaultBranch moves with it.
	renamed bool
}

// readDefaultHead asks GitHub for the default branch's head, following a
// rename: when the stored branch is gone (404), it asks which branch is the
// default now rather than failing every minute until somebody reconnects. A
// network call throughout, made with no gate held.
func readDefaultHead(ctx context.Context, d Deps, p Pipeline, token string) (polledHead, error) {
	branch := strings.TrimSpace(p.DefaultBranch)
	renamed := false
	if branch == "" {
		info, err := d.GitHub.Repository(ctx, token, p.Repository)
		if err != nil {
			return polledHead{}, err
		}
		branch = strings.TrimSpace(info.DefaultBranch)
	}
	if branch == "" {
		return polledHead{}, fmt.Errorf("GitHub names no default branch for %s", p.Repository)
	}
	sha, message, err := d.GitHub.BranchHead(ctx, token, p.Repository, branch)
	if githubapp.StatusOf(err) == http.StatusNotFound {
		// The stored branch is gone: renamed since connect. Its key is new to
		// the heads, so it is a baseline -- a rename is not a push.
		if info, rerr := d.GitHub.Repository(ctx, token, p.Repository); rerr == nil {
			if now := strings.TrimSpace(info.DefaultBranch); now != "" && now != branch {
				branch, renamed = now, true
				sha, message, err = d.GitHub.BranchHead(ctx, token, p.Repository, branch)
			}
		}
	}
	if err != nil {
		return polledHead{}, fmt.Errorf("reading %s's head: %w", branch, err)
	}
	return polledHead{branch: branch, sha: strings.ToLower(strings.TrimSpace(sha)), message: message, renamed: renamed}, nil
}

// writeHeads records what a poll saw, under the repository's gate -- the
// one every writer of the pipeline row takes -- as a COMPARE-AND-SWAP against
// the row the poll read: a fresh read must still show it active, polled, on
// the same repository, holding the very heads the poll diffed against.
// Anything else is another writer since -- a reconnect elsewhere, a
// disconnect, a switch to webhooks -- and the write is skipped, reported as
// not written: the next poll starts from the row as it is, and a run this one
// opened is the run key the next one computes, so nothing opens twice. One
// fresh read and one write; GitHub was asked before.
func (i *Integration) writeHeads(ctx context.Context, d Deps, read Pipeline, heads map[string]string, branch *string) (bool, error) {
	written := false
	err := d.gate(ctx, RepositoryGateKey(read.Repository), func(gctx context.Context) error {
		current, err := d.Store.PipelineByID(memql.ContextWithFreshRead(gctx), read.ID)
		if err != nil {
			return err
		}
		if current == nil || !current.Active() || current.Delivery != DeliveryPoll ||
			current.Repository != read.Repository || !maps.Equal(current.Heads, read.Heads) {
			d.Logger.Info("pipelines: the pipeline changed while it was polled; the next poll starts from it as it is",
				"component", "pipelinerun", logger.Subject(PipelineConcept, read.ID), "repository", read.Repository)
			return nil
		}
		if err := d.Store.UpdatePipeline(gctx, current.OwnerUserID, current.ID, PipelinePatch{Heads: ptr(heads), DefaultBranch: branch}); err != nil {
			return err
		}
		written = true
		return nil
	})
	return written, err
}

// headPRPrefix begins every pull request's key in a pipeline's heads.
const headPRPrefix = "pr:"

func headKeyBranch(branch string) string { return "branch:" + branch }
func headKeyPR(number int) string        { return headPRPrefix + strconv.Itoa(number) }

// firstLine is a message's subject line.
func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return strings.TrimSpace(line)
}
