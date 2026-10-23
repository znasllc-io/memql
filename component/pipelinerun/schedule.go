package pipelinerun

import (
	"context"
	"fmt"
	"slices"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/packages"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/core/logger"
)

// Schedule policy is read from each repository's current default-branch
// manifest, so changing cadence takes effect without reconnecting the source.

const scheduledScanPageLimit = 500

func (i *Integration) handleSchedule(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	res, err := i.Schedule(handlerContext(ctx))
	if err != nil {
		return nil, err
	}
	answer := map[string]any{
		"pipelines": res.Pipelines,
		"opened":    runIDs(res.Opened),
		"existing":  runIDs(res.Existing),
		"failed":    res.Failed,
	}
	return resultNode(answer), nil
}

// Schedule opens each due calendar period for active pipelines whose current
// default-branch manifest opts in. The saved slot makes repeated, delayed and
// concurrent invocations idempotent; weekly scans retain their first pinned SHA.
func (i *Integration) Schedule(ctx context.Context) (PollResult, error) {
	if err := requireInternalOrigin(ctx, "schedule"); err != nil {
		return PollResult{}, err
	}
	d := i.snapshot()
	if d.Store == nil {
		return PollResult{}, errNoStore
	}
	if d.GitHub == nil {
		return PollResult{}, errNoGitHub
	}
	active, err := d.Store.PipelinesForScheduledScan(ctx)
	if err != nil {
		return PollResult{}, err
	}
	if len(active) >= scheduledScanPageLimit {
		d.Logger.Warn("pipelines: scheduled scan read a full page of active pipelines; later rows are not scanned today",
			"component", "pipelinerun", "pipelines", len(active))
	}
	res := PollResult{Pipelines: len(active)}
	now := d.now()
	for _, listed := range active {
		p, err := d.Store.PipelineByID(memql.ContextWithFreshRead(ctx), listed.ID)
		if err != nil {
			scheduledFailure(d, &res, listed, err)
			continue
		}
		if p == nil || !p.Active() {
			continue
		}
		token, _, err := d.GitHub.InstallationToken(ctx, p.CredentialID, p.OwnerUserID, p.Repository)
		if err != nil {
			scheduledFailure(d, &res, *p, err)
			continue
		}
		head, err := readDefaultHead(ctx, d, *p, token)
		if err != nil {
			scheduledFailure(d, &res, *p, err)
			continue
		}
		tree, err := d.GitHub.Tree(ctx, token, p.Repository, head.sha,
			func(name string) bool { return name == pipelines.ManifestPath }, maxManifestBytes)
		if err != nil {
			scheduledFailure(d, &res, *p, fmt.Errorf("reading %s at %s: %w", pipelines.ManifestPath, head.sha, err))
			continue
		}
		manifest, err := packages.ReadManifest(tree)
		if err != nil {
			scheduledFailure(d, &res, *p, err)
			continue
		}
		if manifest.Pipeline == nil || manifest.Pipeline.Schedule == "" {
			continue
		}
		if refusal := pipelines.Validate(manifest.Pipeline); refusal != nil {
			scheduledFailure(d, &res, *p, refusal)
			continue
		}
		// The manifest declares cadence; the saved slot is the authority when
		// the pinned DSL selects stages, including on a later manual retry.
		seen := map[string]bool{}
		for _, stage := range manifest.Pipeline.Stages {
			if !slices.Contains(stage.On, string(pipelines.EventSchedule)) {
				continue
			}
			cadence := stage.Schedule
			if cadence == "" {
				cadence = manifest.Pipeline.Schedule
			}
			slot, err := pipelines.ScheduleSlot(cadence, now)
			if err != nil {
				scheduledFailure(d, &res, *p, err)
				break
			}
			if seen[slot] {
				continue
			}
			seen[slot] = true
			opened, err := i.openWithToken(ctx, d, *p, Opening{
				Event: pipelines.EventSchedule, SHA: head.sha, Branch: head.branch,
				Title: firstLine(head.message), Trigger: TriggerSchedule, ScheduleSlot: slot,
			}, token)
			if err != nil {
				scheduledFailure(d, &res, *p, err)
				continue
			}
			if opened.Opened {
				res.Opened = append(res.Opened, opened.Run)
			} else if opened.Ignored == "" {
				res.Existing = append(res.Existing, opened.Run)
			}
		}
	}
	return res, nil
}

func scheduledFailure(d Deps, res *PollResult, p Pipeline, err error) {
	d.Logger.Warn("pipelines: the scheduled scan could not evaluate a pipeline",
		"component", "pipelinerun", logger.Subject(PipelineConcept, p.ID), "repository", p.Repository, "error", err)
	res.Failed = append(res.Failed, SkippedPipeline{PipelineID: p.ID, Reason: err.Error()})
}
