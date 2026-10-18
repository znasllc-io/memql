package pipelinesteps

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	pl "github.com/znasllc-io/memql/component/pipelines"
)

func TestResourceQueueCreatesSuspendedJobAndReservesSidecars(t *testing.T) {
	cfg, run := testConfig(), testRun()
	cfg.QueueName, cfg.NodePool = "background", "builds"
	run.RunDeadline = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	run.MemoryMiB = 8192
	job, err := BuildJob(cfg, run, testJobName)
	require.NoError(t, err)
	require.Equal(t, "background", job.Metadata.Labels[queueLabel])
	require.NotNil(t, job.Spec.Suspend)
	require.True(t, *job.Spec.Suspend, "a missing queue controller must never start work")
	for _, list := range [][]Container{job.Spec.Template.Spec.Containers, job.Spec.Template.Spec.InitContainers} {
		for _, c := range list {
			if c.Resources == nil {
				continue
			}
			for _, key := range []string{"memory", "ephemeral-storage"} {
				if limit := c.Resources.Limits[key]; limit != "" {
					require.Equal(t, limit, c.Resources.Requests[key], c.Name+" "+key)
				}
			}
		}
	}
	cfg.NodePool = ""
	_, err = BuildJob(cfg, run, testJobName)
	require.ErrorContains(t, err, "dedicated build pool")
}

func TestQueuedJobFailureIsTerminalBeforeAdmission(t *testing.T) {
	job := Job{Metadata: ObjectMeta{Labels: map[string]string{queueLabel: "background"}}}
	job.Status.Conditions = []JobCondition{{Type: "Failed", Status: "True", Reason: "Rejected"}}
	obs := Classify(job, nil, time.Now(), time.Now(), testConfig(), "")
	require.Equal(t, PhaseFailed, obs.Phase)
}

func TestSuspendedJobStopsAtRunCeilingOnAnotherReplica(t *testing.T) {
	h := newRunnerHarness(t)
	run := rtRun()
	run.RunDeadline = rtRunDeadline(time.Minute)
	job := h.existingJob(t, run, map[string]string{AnnotRunner: rtStamp(rtOther, rtT0.Add(-time.Hour))})
	job.Metadata.Labels[queueLabel] = "background"
	job.Spec.Suspend = ptr(true)
	job.Status.StartTime = time.Time{}
	h.c.putJob(job, nil)
	h.clock.Advance(2 * time.Minute)
	res := h.run(t, run)
	rtWantCode(t, res, pl.OutcomeFailed, pl.CodeRunCeiling)
	// The new replica must settle and retire the suspended Job so it cannot
	// start later when capacity frees. It must not create a replacement.
	require.Empty(t, h.c.requestsFor("POST", kubeJobs))
	require.False(t, h.c.hasJob(testJobName))
	require.True(t, h.c.hasSecret(testJobName+retirementSuffix))
}

func TestQueuedJobStatusSurvivesReplicaHopWithoutStartingTimeout(t *testing.T) {
	h := newRunnerHarness(t)
	job := h.existingJob(t, rtRun(), map[string]string{AnnotRunner: rtStamp(rtOther, rtT0)})
	job.Metadata.Labels[queueLabel] = "background"
	job.Spec.Suspend = ptr(true)
	job.Status.StartTime = time.Time{}
	h.c.putJob(job, nil)
	// This runner never held the originating request. The shared Job, rather
	// than local memory or creation time, decides whether execution began.
	got := h.r.Status(context.Background(), StatusRequest{JobName: testJobName})
	require.Empty(t, got.ExecutionStartedAt)
	job.Spec.Suspend = ptr(false)
	job.Status.StartTime = rtT0
	h.c.putJob(job, nil)
	got = h.r.Status(context.Background(), StatusRequest{JobName: testJobName})
	require.Equal(t, rtT0.Format(time.RFC3339), got.ExecutionStartedAt)
}

func TestQueueWaitingDoesNotConsumeSchedulingAllowance(t *testing.T) {
	cfg := testConfig()
	job := Job{Metadata: ObjectMeta{Labels: map[string]string{queueLabel: "background"}}, Spec: JobSpec{Suspend: ptr(true)}}
	now := time.Now()
	obs := Classify(job, nil, now.Add(-time.Hour), now, cfg, "")
	require.Equal(t, PhasePending, obs.Phase)
	require.Equal(t, "waiting for background resources", obs.Detail)
	job.Spec.Suspend = ptr(false)
	job.Status.StartTime = now
	pod := &Pod{Status: PodStatus{Conditions: []PodCondition{{Type: "PodScheduled", Status: "False", Reason: "Unschedulable"}}}}
	obs = Classify(job, pod, now.Add(-time.Hour), now, cfg, "")
	require.Equal(t, PhasePending, obs.Phase, "the scheduling clock starts at admission, not when queued")
}
