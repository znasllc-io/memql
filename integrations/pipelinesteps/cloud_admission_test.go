package pipelinesteps

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	pl "github.com/znasllc-io/memql/component/pipelines"
)

func TestCloudAdmissionWaitSurvivesReplicaAndLateHold(t *testing.T) {
	for _, when := range []string{"before preparation", "before Job POST", "unreadable decision"} {
		t.Run(when, func(t *testing.T) {
			reads := 0
			h := newRunnerHarness(t, func(c *Config) {
				c.AdmitCloud = func(time.Time, bool) (bool, error) {
					reads++
					if when == "unreadable decision" {
						return true, errors.New("unavailable")
					}
					return when == "before Job POST" && reads == 1, nil
				}
			})
			run := rtRun()
			first := &step{r: h.r, run: run, jobName: testJobName, ctx: context.Background(), log: h.r.log}
			defer first.discardCapture()
			if res, job, done := first.create(); done || job.Metadata.UID != "" {
				t.Fatalf("held create ended or allocated: %+v %+v", res, job)
			}
			meta, err := h.c.kube.SecretMetadata(context.Background(), testSecretName)
			if err != nil || meta.Annotations[annotCreation] != creationQueued || len(h.c.requestsFor(http.MethodPost, kubeJobs)) != 0 {
				t.Fatalf("hold lacks durable no-execution proof: %+v %v", meta, err)
			}
			if when != "before Job POST" && len(h.tokens.called()) != 0 {
				t.Fatal("held work minted a clone token")
			}
			// Simulate a stopped creator without running its cancellation path.
			// A fresh replica owns no memory or capture from that creator.
			cfg := h.cfg
			cfg.NodeID = rtOther
			cfg.AdmitCloud = func(time.Time, bool) (bool, error) { return true, nil }
			second := h.newRunner(cfg)
			h.c.script(testJobName, rtFinishingScript(testJobName, 0, ""))
			run.RecoverOnly = true
			res := second.Run(context.Background(), run)
			if res.Status != pl.OutcomeSucceeded || len(h.c.requestsFor(http.MethodPost, kubeJobs)) != 1 {
				t.Fatalf("queued work did not resume exactly once: %+v", res)
			}
		})
	}
}

func TestCloudAdmissionDoesNotBlockAdoptionOrCancellation(t *testing.T) {
	t.Run("adoption", func(t *testing.T) {
		h := newRunnerHarness(t, func(c *Config) {
			c.AdmitCloud = func(time.Time, bool) (bool, error) {
				t.Fatal("existing work must not request a new spending allowance")
				return false, nil
			}
		})
		run := rtRun()
		h.c.putSecret(BuildSecret(h.cfg, run, testJobName, rtCloneToken))
		h.c.putJob(h.existingJob(t, run, nil), rtFinishingScript(testJobName, 0, ""))
		run.RecoverOnly = true
		if res := h.run(t, run); res.Status != pl.OutcomeSucceeded {
			t.Fatalf("adoption blocked: %+v", res)
		}
		if len(h.c.requestsFor(http.MethodPost, kubeJobs)) != 0 {
			t.Fatal("adoption allocated new work")
		}
	})
	t.Run("cancel waiting", func(t *testing.T) {
		h := newRunnerHarness(t, func(c *Config) { c.AdmitCloud = func(time.Time, bool) (bool, error) { return false, nil } })
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := h.start(ctx, rtRun())
		rtWaitUntil(t, "queued admission record", func() bool { return h.c.hasSecret(testSecretName) })
		cancel()
		rtWantCode(t, h.await(t, done), pl.OutcomeCancelled, pl.CodeStepCancelled)
		if h.c.hasSecret(testSecretName) || len(h.c.requestsFor(http.MethodPost, kubeJobs)) != 0 {
			t.Fatal("cancelled wait left work behind")
		}
	})
}

func TestCloudAdmissionRechecksIsolationProbeAllocation(t *testing.T) {
	h := newIsoHarness(t, isoScript(probeExitIsolated, isoSaid[probeExitIsolated]))
	reads := 0
	h.r.cfg.AdmitCloud = func(time.Time, bool) (bool, error) {
		reads++
		if reads == 1 {
			return true, nil
		} // step enters isolation
		if reads < 5 {
			if createsOf(h.c, kubeJobs, isoName()) != 0 {
				t.Error("isolation allocated while spending was held")
			}
			return false, nil
		}
		return true, nil
	}
	if res := h.run(t, isoStep(h, rtRun().StepKey)); res.Status != pl.OutcomeSucceeded {
		t.Fatalf("probe wait did not resume: %+v", res)
	}
	if reads < 5 || createsOf(h.c, kubeJobs, isoName()) != 1 {
		t.Fatal("probe did not use live admission")
	}
	isoLeftNothing(t, h)
}

func TestCloudAdmissionExpiredArrivalStartsNoAdmissionOrCleanup(t *testing.T) {
	h := newIsoHarness(t, nil)
	h.r.cfg.AdmitCloud = func(time.Time, bool) (bool, error) {
		t.Error("an expired arrival must not ask for a spending decision")
		return false, nil
	}
	run := rtRun()
	run.RunDeadline = rtRunDeadline(-5 * time.Minute)
	rtWantCode(t, h.run(t, run), pl.OutcomeFailed, pl.CodeRunCeiling)
	if len(h.tokens.called()) != 0 || h.c.hasSecret(testSecretName) {
		t.Fatal("expired arrival acquired credentials or queued state")
	}
	for _, request := range h.c.requests() {
		if request.Method != http.MethodGet {
			t.Fatalf("expired arrival issued an effect: %s", h.c.summary())
		}
	}
}
