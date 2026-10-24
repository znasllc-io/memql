package pipelinerun

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/pipelines"
)

// Observe the real driver's provider writes; the test does not publish a
// synthetic progress report or replace the installed pipeline recipe.
type progressGitHub struct {
	*fakeGitHub
	reports chan githubapp.CheckRun
}

func (g *progressGitHub) UpdateCheckRun(ctx context.Context, token, repository string, id int64, run githubapp.CheckRun) error {
	if err := g.fakeGitHub.UpdateCheckRun(ctx, token, repository, id, run); err != nil {
		return err
	}
	g.reports <- run
	return nil
}

func TestDefaultWorkflowReportsCompletedStepBeforeItsSiblingFinishes(t *testing.T) {
	for _, failed := range []bool{false, true} {
		name := "passed"
		if failed {
			name = "failed"
		}
		t.Run(name, func(t *testing.T) {
			dh := newDriveHarness(t, `formatVersion: 1
name: shop
pipeline:
  image: ghcr.io/acme/toolchain@sha256:abc
  stages:
    - name: checks
      steps:
        - name: fast
          run: check fast
        - name: held
          run: check held
`)
			provider := &progressGitHub{fakeGitHub: dh.github, reports: make(chan githubapp.CheckRun, 32)}
			dh.integ.Configure(func(d *Deps) { d.GitHub = provider })
			heldStarted, releaseHeld := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(releaseHeld) }) }
			t.Cleanup(func() {
				release()
				waitDrives(t, dh.integ)
			})
			dh.exec.answer = func(ctx context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
				if req.StepKey == "checks.held" {
					close(heldStarted)
					select {
					case <-releaseHeld:
						return passed(req), nil
					case <-ctx.Done():
						return pipelines.StepResult{}, ctx.Err()
					}
				}
				select {
				case <-heldStarted:
				case <-ctx.Done():
					return pipelines.StepResult{}, ctx.Err()
				}
				result := passed(req)
				if failed {
					result.Status, result.ExitCode = pipelines.OutcomeFailed, 1
				}
				return result, nil
			}
			run := dh.openRun(t, prOpening())
			dh.integ.HandleRunEvent(graphEvent(events.TopicGraphNodeCreated, run))
			dh.awaitEntered(t, "checks.fast", "checks.held")
			wantSummary := "1 " + name + ", 1 running"
			deadline := time.NewTimer(5 * time.Second)
			defer deadline.Stop()
			observed := false
			for !observed {
				select {
				case report := <-provider.reports:
					if report.Status == StatusCompleted {
						t.Fatal("the check concluded while its sibling was still running")
					}
					observed = report.Output != nil && strings.Contains(report.Output.Summary, wantSummary)
				case <-deadline.C:
					t.Fatalf("completed step was not reported while its sibling was held; wanted %q", wantSummary)
				}
			}
			current, _ := dh.store.run(run.ID)
			wantFailed := 0
			if failed {
				wantFailed = 1
			}
			if current.Status != StatusInProgress || current.Conclusion != "" || len(current.Stages) != 1 ||
				current.Stages[0].Status != StageRunning || current.Stages[0].Failed != wantFailed {
				t.Fatalf("saved stage table disagrees with the live report: %+v", current)
			}
			if len(dh.work.receiptsOf("checks.fast")) != 1 || len(dh.work.receiptsOf("checks.held")) != 0 {
				t.Fatal("progress did not follow the finished receipt or invented a sibling result")
			}
			release()
			waitDrives(t, dh.integ)
			final, _ := dh.store.run(run.ID)
			wantConclusion := ConclusionSuccess
			if failed {
				wantConclusion = ConclusionFailure
			}
			if final.Status != StatusCompleted || final.Conclusion != wantConclusion || len(dh.exec.sentKeys()) != 2 {
				t.Fatalf("progress changed execution or final verdict: %+v; executed %v", final, dh.exec.sentKeys())
			}
		})
	}
}
