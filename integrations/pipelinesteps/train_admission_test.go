package pipelinesteps

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/integrations/workbench"
)

// The source driver's identity crosses the actual SYSTEM-gated forward
// handler into a runner with no originating process state. The train must
// fund both its probe and command, even when verification omits event env.
func TestTrainCloudAdmissionSurvivesForwardAndFundsIsolation(t *testing.T) {
	var trainReads, ordinaryReads atomic.Int32
	h := newIsoHarness(t, isoScript(probeExitIsolated, isoSaid[probeExitIsolated]), func(c *Config) {
		c.AdmitCloud = func(_ time.Time, train bool) (bool, error) {
			if train {
				trainReads.Add(1)
			} else {
				ordinaryReads.Add(1)
			}
			return train, nil
		}
	})
	req := exRequest()
	req.Event, req.HeadBranch = pl.EventPullRequest, "train/2026-10-28-1"
	req.RunStartedAt = h.clock.Now().Add(-time.Minute).Format(time.RFC3339)
	req.Step.Services, req.Step.Caches, req.Step.Artifacts = nil, nil, nil
	req.Step.Verification = true
	name := JobName(req.RunID, req.StepKey, req.Attempt)
	h.c.script(name, rtFinishingScript(name, 0, ""))
	mesh := newHopMesh(t, func(string) workbench.PipelineRunner { return hopRunnerAdapter{h.r} }, rtNode)
	executor := newTestExecutor(mesh, nil)
	executor.now = h.clock.Now
	done := make(chan pl.StepResult, 1)
	go func() {
		result, _ := executor.Execute(t.Context(), req)
		done <- result
	}()
	if result := h.await(t, done); result.Status != pl.OutcomeSucceeded {
		t.Fatalf("train did not reach its allowed command: %+v", result)
	}
	if trainReads.Load() < 3 || ordinaryReads.Load() != 0 || createsOf(h.c, kubeJobs, isoName()) != 1 || createsOf(h.c, kubeJobs, name) != 1 {
		t.Fatalf("train identity did not reach both allocation boundaries: train=%d ordinary=%d requests=%s", trainReads.Load(), ordinaryReads.Load(), h.c.summary())
	}
	isoLeftNothing(t, h)
}

func TestOrdinaryCloudWorkCannotBorrowTrainEnvironmentOrCachedProof(t *testing.T) {
	for _, identity := range []struct {
		name, branch string
		event        pl.Event
	}{
		{"ordinary pull request", "feature/ordinary", pl.EventPullRequest},
		{"train-named push", "train/2026-10-28-1", pl.EventPush},
		{"environment only", "", ""},
	} {
		t.Run(identity.name, func(t *testing.T) {
			var reads atomic.Int32
			h := newRunnerHarness(t, func(c *Config) {
				c.AdmitCloud = func(_ time.Time, train bool) (bool, error) {
					reads.Add(1)
					if train {
						t.Error("ordinary work acquired train admission")
					}
					return train, nil
				}
			}) // includes a previously successful, shared isolation verdict
			run := rtRun()
			run.Event, run.HeadBranch = identity.event, identity.branch
			run.Env["MEMQL_EVENT"], run.Env["MEMQL_HEAD_BRANCH"] = "pull_request", "train/2026-10-28-1"
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := h.start(ctx, run)
			rtWaitUntil(t, "ordinary spending hold", func() bool { return reads.Load() > 0 })
			cancel()
			h.await(t, done)
			if len(h.c.requestsFor(http.MethodPost, kubeJobs)) != 0 || len(h.tokens.called()) != 0 {
				t.Fatal("ordinary request borrowed train permission from environment or cached isolation")
			}
		})
	}
}

func TestTrainCloudRecoveryBindsEventAndHeadBranch(t *testing.T) {
	for _, changed := range []string{"unchanged", "branch", "event"} {
		t.Run(changed, func(t *testing.T) {
			h := newRunnerHarness(t, func(c *Config) {
				c.AdmitCloud = func(time.Time, bool) (bool, error) { return false, nil }
			})
			run := rtRun()
			run.Event, run.HeadBranch = pl.EventPullRequest, "train/2026-10-28-1"
			first := &step{r: h.r, run: run, jobName: testJobName, ctx: t.Context(), log: h.r.log}
			defer first.discardCapture()
			if _, _, done := first.create(); done {
				t.Fatal("initial spending hold did not retain the queued identity")
			}
			cfg := h.cfg
			cfg.NodeID = rtOther
			cfg.AdmitCloud = func(time.Time, bool) (bool, error) { return true, nil }
			second := h.newRunner(cfg)
			run.RecoverOnly = true
			switch changed {
			case "branch":
				run.HeadBranch = "feature/ordinary"
			case "event":
				run.Event = pl.EventPush
			}
			h.c.script(testJobName, rtFinishingScript(testJobName, 0, ""))
			result := second.Run(t.Context(), run)
			if changed == "unchanged" {
				if result.Status != pl.OutcomeSucceeded || createsOf(h.c, kubeJobs, testJobName) != 1 {
					t.Fatalf("same train could not recover once admitted: %+v", result)
				}
			} else if result.Failure == nil || result.Failure.Code != pl.CodeExecutionUncertain || createsOf(h.c, kubeJobs, testJobName) != 0 {
				t.Fatalf("changed identity reused a queued effect: %+v", result)
			}
		})
	}
}

// Start with both classes allowed, so ordinary work can already be waiting on
// the same proof when the operator holds it. A departed train must not leave
// that shared proof with permission to POST after the hold changes.
func TestIsolationProbeDropsCancelledTrainAllowance(t *testing.T) {
	var ordinaryAllowed atomic.Bool
	ordinaryAllowed.Store(true)
	h := newIsoHarness(t, isoScript(probeExitIsolated, isoSaid[probeExitIsolated]), func(c *Config) {
		c.AdmitCloud = func(_ time.Time, train bool) (bool, error) { return train || ordinaryAllowed.Load(), nil }
	})
	h.c.with(func(c *rtCluster) { c.quotaJobs = map[string]bool{isoName(): true} })
	train := isoStep(h, "checks/train")
	train.Event, train.HeadBranch = pl.EventPullRequest, "train/2026-10-28-1"
	trainCtx, cancelTrain := context.WithCancel(t.Context())
	defer cancelTrain()
	trainDone := h.start(trainCtx, train)
	rtWaitUntil(t, "train probe waiting for quota", func() bool { return createsOf(h.c, kubeJobs, isoName()) > 0 })
	ordinaryCtx, cancelOrdinary := context.WithCancel(t.Context())
	defer cancelOrdinary()
	ordinaryDone := h.start(ordinaryCtx, isoStep(h, "checks/ordinary"))
	rtWaitUntil(t, "both classes sharing the proof", func() bool {
		h.r.isoMu.Lock()
		defer h.r.isoMu.Unlock()
		return h.r.isoProof != nil && len(h.r.isoProof.waiters) == 2
	})
	ordinaryAllowed.Store(false)
	cancelTrain()
	h.await(t, trainDone)
	rtWaitUntil(t, "train leaving proof", func() bool {
		h.r.isoMu.Lock()
		defer h.r.isoMu.Unlock()
		return h.r.isoProof != nil && len(h.r.isoProof.waiters) == 1
	})
	h.r.isoMu.Lock()
	proof := h.r.isoProof
	h.r.isoMu.Unlock()
	if allowed, _ := h.r.probeCloudAdmission(proof); allowed {
		t.Fatal("the departed train still funds an ordinary waiter's probe")
	}
	h.c.with(func(c *rtCluster) { c.quotaJobs = nil })
	// An allowed live train can join and finish the same proof. Its ordinary
	// sibling remains held even after sharing the successful isolation fact.
	otherTrain := isoStep(h, "checks/next-train")
	otherTrain.Event, otherTrain.HeadBranch = train.Event, train.HeadBranch
	if result := h.run(t, otherTrain); result.Status != pl.OutcomeSucceeded {
		t.Fatalf("new train could not fund shared proof: %+v", result)
	}
	if createsOf(h.c, kubeJobs, JobName(train.RunID, "checks/ordinary", train.Attempt)) != 0 {
		t.Fatal("ordinary waiter borrowed the train's completed isolation proof as admission")
	}
	cancelOrdinary()
	h.await(t, ordinaryDone)
	isoLeftNothing(t, h)
}
