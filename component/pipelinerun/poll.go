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
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/packages/githubapp"
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

// pollOne polls one pipeline under its gate: a fresh read of the row, the
// heads GitHub reports, the runs a moved head asks for, and the heads written
// back. A head whose run could not be opened keeps its PREVIOUS value, so the
// next poll asks again rather than the change being lost.
func (i *Integration) pollOne(ctx context.Context, d Deps, listed Pipeline) (pollOutcome, error) {
	var out pollOutcome
	err := d.gate(ctx, PipelineGateKey(listed.ID), func(gctx context.Context) error {
		p, err := d.Store.PipelineByID(memql.ContextWithFreshRead(gctx), listed.ID)
		if err != nil {
			return err
		}
		if p == nil || !p.Active() || p.Delivery != DeliveryPoll {
			return nil // disconnected or switched to webhooks since the list was read
		}
		if d.GitHub == nil {
			return errNoGitHub
		}
		token, _, err := d.GitHub.InstallationToken(gctx, p.CredentialID, p.OwnerUserID, p.Repository)
		if err != nil {
			return err
		}
		branch := strings.TrimSpace(p.DefaultBranch)
		if branch == "" {
			info, err := d.GitHub.Repository(gctx, token, p.Repository)
			if err != nil {
				return err
			}
			branch = strings.TrimSpace(info.DefaultBranch)
		}
		if branch == "" {
			return fmt.Errorf("GitHub names no default branch for %s", p.Repository)
		}
		// branchPatch records a default branch that moved on GitHub since the
		// row was written, so the next poll asks for the right one.
		var branchPatch *string
		headSHA, headMessage, err := d.GitHub.BranchHead(gctx, token, p.Repository, branch)
		if githubapp.StatusOf(err) == http.StatusNotFound {
			// The stored branch is gone: renamed since connect. Ask GitHub
			// which branch is the default now, rather than failing every
			// minute until somebody reconnects. Its key is new to the heads,
			// so it is a baseline -- a rename is not a push.
			if info, rerr := d.GitHub.Repository(gctx, token, p.Repository); rerr == nil {
				if renamed := strings.TrimSpace(info.DefaultBranch); renamed != "" && renamed != branch {
					branch, branchPatch = renamed, ptr(renamed)
					headSHA, headMessage, err = d.GitHub.BranchHead(gctx, token, p.Repository, branch)
				}
			}
		}
		if err != nil {
			return fmt.Errorf("reading %s's head: %w", branch, err)
		}
		pulls, err := d.GitHub.OpenPullRequests(gctx, token, p.Repository)
		if err != nil {
			return fmt.Errorf("reading open pull requests: %w", err)
		}

		branchKey := headKeyBranch(branch)
		seen := map[string]string{branchKey: strings.ToLower(strings.TrimSpace(headSHA))}
		for _, pr := range pulls {
			if pr.Number <= 0 || strings.TrimSpace(pr.HeadSHA) == "" {
				continue
			}
			seen[headKeyPR(pr.Number)] = strings.ToLower(strings.TrimSpace(pr.HeadSHA))
		}

		if len(p.Heads) == 0 {
			// The baseline: record, open nothing.
			out.baseline = true
			return d.Store.UpdatePipeline(gctx, p.OwnerUserID, p.ID, PipelinePatch{Heads: ptr(seen), DefaultBranch: branchPatch})
		}

		next := maps.Clone(seen) // closed pull requests' keys drop out here
		var failures []error
		openOrKeep := func(key string, o Opening) {
			res, err := i.open(gctx, d, *p, o)
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
				Event: pipelines.EventPush, SHA: seen[branchKey], BaseSHA: prev, Branch: branch,
				Title: firstLine(headMessage), Trigger: TriggerPoll,
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
			head := strings.TrimSpace(pr.HeadRepository)
			openOrKeep(key, Opening{
				Event: pipelines.EventPullRequest, SHA: sha, BaseSHA: pr.BaseSHA, Branch: pr.HeadRef,
				Title: firstLine(pr.Title), PullRequest: pr.Number,
				// D6, exactly as the delivery reads it: a head in another
				// repository, or one GitHub reports deleted, is a fork.
				Fork:           head == "" || !strings.EqualFold(head, p.Repository),
				HeadRepository: head,
				Trigger:        TriggerPoll,
			})
		}

		if !maps.Equal(next, p.Heads) || branchPatch != nil {
			if err := d.Store.UpdatePipeline(gctx, p.OwnerUserID, p.ID, PipelinePatch{Heads: ptr(next), DefaultBranch: branchPatch}); err != nil {
				failures = append(failures, err)
			}
		}
		return errors.Join(failures...)
	})
	return out, err
}

func headKeyBranch(branch string) string { return "branch:" + branch }
func headKeyPR(number int) string        { return "pr:" + strconv.Itoa(number) }

// firstLine is a message's subject line.
func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return strings.TrimSpace(line)
}
