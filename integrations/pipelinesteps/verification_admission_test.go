package pipelinesteps

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	pl "github.com/znasllc-io/memql/component/pipelines"
)

// Exercise the real request adaptation and wire rather than inventing an Env
// that verification commands intentionally never receive.
func TestVerificationAdmissionPreservesTrustedEventAcrossHop(t *testing.T) {
	for _, event := range []pl.Event{pl.EventPullRequest, pl.EventMergeGroup, pl.EventRelease} {
		t.Run(string(event), func(t *testing.T) {
			cfg := testConfig()
			cfg.QueueName, cfg.NodePool = "background", "builds"
			cfg.QueuePriorityClasses = `{"pull_request":"pull-requests","merge_group":"merge-gates","release":"releases"}`
			req := testRequest()
			req.Event, req.Step.Verification = event, true
			run := stepRunFor(req, 600, pl.CodeRunCeiling)
			run.RunDeadline = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
			wire, err := json.Marshal(run)
			require.NoError(t, err)
			var peer StepRun
			require.NoError(t, json.Unmarshal(wire, &peer))
			require.NotContains(t, peer.Env, eventVar)
			require.Equal(t, "true", peer.Env["MEMQL_VERIFICATION"])
			require.Equal(t, CacheUntrusted, cacheTrustOf(peer))
			job, err := BuildJob(cfg, peer, testJobName)
			require.NoError(t, err, "verification admission must use trusted control-plane event")
			classes := map[pl.Event]string{pl.EventPullRequest: "pull-requests", pl.EventMergeGroup: "merge-gates", pl.EventRelease: "releases"}
			require.Equal(t, classes[event], job.Metadata.Labels[queuePriorityLabel])
			require.Equal(t, "background", job.Metadata.Labels[queueLabel])
			require.True(t, *job.Spec.Suspend)
			require.Empty(t, job.Spec.Template.Metadata.Labels[queuePriorityLabel])
			mounted, prepared := cacheOf(t, job)
			want := CacheSubPath(req.OwnerUserID, req.Repository, CacheUntrusted)
			require.Equal(t, want, mounted)
			require.Equal(t, want, prepared)
			for _, container := range job.Spec.Template.Spec.Containers {
				for _, env := range container.Env {
					require.NotEqual(t, eventVar, env.Name)
				}
			}
		})
	}
}

func TestAdmissionPriorityNeverBorrowsCommandEvent(t *testing.T) {
	cfg := testConfig()
	cfg.QueueName, cfg.NodePool = "background", "builds"
	cfg.QueuePriorityClasses = `{"merge_group":"merge-gates","pull_request":"pull-requests"}`
	for _, event := range []pl.Event{"", "forged"} {
		t.Run("invalid-"+string(event), func(t *testing.T) {
			req := testRequest()
			req.Event = event
			run := stepRunFor(req, 600, pl.CodeStepTimeout)
			run.RunDeadline = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
			// Even a plausible command value cannot supply missing native evidence.
			run.Env[eventVar] = string(pl.EventMergeGroup)
			_, err := BuildJob(cfg, run, testJobName)
			require.ErrorContains(t, err, "trusted pipeline event")
		})
	}
	req := testRequest()
	run := stepRunFor(req, 600, pl.CodeStepTimeout)
	run.Env[eventVar] = string(pl.EventMergeGroup)
	got, err := queuePriorityClass(cfg, run)
	require.NoError(t, err)
	require.Equal(t, "pull-requests", got, "command metadata cannot select a different admission class")
	// Compiler-invalid reserved secrets remain refused at the runner boundary.
	req.Step.Verification = true
	req.Secrets[eventVar] = string(pl.EventMergeGroup)
	run = stepRunFor(req, 600, pl.CodeStepTimeout)
	run.RunDeadline = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	_, err = BuildJob(cfg, run, testJobName)
	require.ErrorContains(t, err, "MEMQL_")
}

func TestDefaultAdmissionPriorityRetainsResourceQueue(t *testing.T) {
	cfg := testConfig()
	cfg.QueueName, cfg.NodePool = "background", "builds"
	req := testRequest()
	req.Step.Verification = true
	run := stepRunFor(req, 600, pl.CodeStepTimeout)
	run.RunDeadline = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	// Omitted priority mapping is the documented default-order mode, not a
	// change to resource admission, placement, execution bounds or cache trust.
	job, err := BuildJob(cfg, run, testJobName)
	require.NoError(t, err)
	require.Empty(t, job.Metadata.Labels[queuePriorityLabel])
	require.Equal(t, "background", job.Metadata.Labels[queueLabel])
	require.True(t, *job.Spec.Suspend)
	require.Equal(t, CacheUntrusted, cacheTrustOf(run))
	cfg.NodePool = ""
	_, err = BuildJob(cfg, run, testJobName)
	require.ErrorContains(t, err, "dedicated build pool")
	cfg.NodePool = "builds"
	run.RunDeadline = ""
	_, err = BuildJob(cfg, run, testJobName)
	require.ErrorContains(t, err, "deadline")
	cfg.QueuePriorityClasses, cfg.QueueName = `{"pull_request":"pull-requests"}`, ""
	_, err = BuildJob(cfg, run, testJobName)
	require.ErrorContains(t, err, "require a queue")
}
