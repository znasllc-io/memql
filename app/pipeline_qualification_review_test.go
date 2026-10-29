package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/pipelinerun"
	"github.com/znasllc-io/memql/component/pipelines"
)

func qualifiedReviewFixture(t *testing.T) (*independentCaptureFixture, pipelinerun.Run, string) {
	t.Helper()
	f := newIndependentCaptureFixture(t)
	f.p.Status = pipelinerun.PipelineQualification
	f.p.Compute = pipelines.ComputeCluster
	f.p.SecretNames = []string{}
	phases := map[string]string{}
	for _, name := range []string{"runPipelineStages", "pipelineStageIncluded", "pipelinePackageSelection", "pipelinePackageStepSelection", "pipelineBucketIncluded", "pipelineModeForEvent", "pipelineVersionForEvent", "pipelineNotificationCopy", "pipelineNotificationOutcome", "pipelineArtifactRetentionPolicy"} {
		phases[name] = "qualificationReviewFixture"
	}
	snapshot, err := workflowhost.CapturePhases("qualificationReviewFixture", "pipelines.delivery.v1", phases, nil, func(kind, name string) (string, error) {
		if kind != "automation" || name != "qualificationReviewFixture" {
			return "", fmt.Errorf("unexpected fixture source %s %s", kind, name)
		}
		return "@template\nautomation qualificationReviewFixture { return true }", nil
	}, nil)
	require.NoError(t, err)
	r := f.run(2)
	r.Trigger = pipelinerun.TriggerQualification
	r.PolicySnapshot = snapshot.Map()
	q := pipelinerun.PullRequestQualification{Version: 1, RequestID: "review-request", PackageID: f.p.PackageID, CredentialID: f.p.CredentialID, OwnerUserID: f.p.OwnerUserID, AccountID: f.p.AccountID, Repository: f.p.Repository, DefaultBranch: f.p.DefaultBranch, Name: f.p.Name, InstallationID: f.p.InstallationID, PullRequest: r.PullRequest, HeadSHA: r.SHA, HeadBranch: r.HeadBranch, BaseSHA: r.BaseSHA, ManifestDigest: strings.Repeat("a", 64), Compute: f.p.Compute, SecretNames: f.p.SecretNames, ExpiresAt: time.Now().UTC().Add(time.Hour)}
	// Match the production constructor's immutable scope digest and request-derived
	// ID. This fixture is evidence data only; it makes no provider grant or write.
	q.Name = "product"
	q.AppID = 42
	q.RepositoryID = f.source.RepositoryID
	f.p.Name = q.Name
	q.RequestDigest = pipelineReviewDigest(q)
	q.PolicyVersion = snapshot.Version
	raw, err := json.Marshal(q)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &r.Qualification))
	sum := sha256.Sum256([]byte("pipelines.qualification\x00" + f.p.ID + "\x00" + q.RequestID))
	r.ID = hex.EncodeToString(sum[:16])
	require.NoError(t, pipelinerun.ValidateQualification(f.p, r, time.Now().UTC()))
	return f, r, snapshot.Version
}

func TestIndependentReviewCaptureAcceptsOnlyExactQualifiedInactiveRun(t *testing.T) {
	for _, change := range []string{"valid", "ordinary disconnected", "missing scope", "copied run", "different owner", "different account", "different PR", "different compute", "active restatement", "expired", "moved head", "moved base"} {
		t.Run(change, func(t *testing.T) {
			f, r, version := qualifiedReviewFixture(t)
			switch change {
			case "ordinary disconnected":
				f.p.Status = pipelinerun.PipelineDisconnected
				r.Trigger = pipelinerun.TriggerManual
				r.Qualification = nil
			case "missing scope":
				r.Qualification = nil
			case "copied run":
				r.ID = "copied-run"
			case "different owner":
				r.OwnerUserID = "someone-else"
			case "different account":
				r.AccountID = "another-account"
			case "different PR":
				r.PullRequest++
			case "different compute":
				f.p.Compute = pipelines.ComputeClusterAndFleet
			case "active restatement":
				f.p.Status = pipelinerun.PipelineActive
			case "expired":
				// Keep a valid scope digest: this exercises the expiry fence itself,
				// not the separate defense against tampered persisted scope.
				raw, err := json.Marshal(r.Qualification)
				require.NoError(t, err)
				var scope pipelinerun.PullRequestQualification
				require.NoError(t, json.Unmarshal(raw, &scope))
				scope.ExpiresAt = time.Now().UTC().Add(-time.Minute)
				scope.RequestDigest, scope.PolicyVersion = "", ""
				scope.RequestDigest = pipelineReviewDigest(scope)
				scope.PolicyVersion = version
				raw, err = json.Marshal(scope)
				require.NoError(t, err)
				require.NoError(t, json.Unmarshal(raw, &r.Qualification))
			case "moved head":
				f.movedHead = true
			case "moved base":
				f.base = reviewSHA(2)
			}
			reads := f.reads()
			sourceReads := 0
			source := reads.source
			reads.source = func(ctx context.Context, p pipelinerun.Pipeline, run pipelinerun.Run, fence func(context.Context) error) (pipelinerun.ReviewSource, *githubapp.Client, string, error) {
				sourceReads++
				require.Equal(t, r.ID, run.ID)
				return source(ctx, p, run, fence)
			}
			got, err := capturePipelineIndependentReview(t.Context(), f.p, r, "independentPrompt", version, func(c context.Context) error { return c.Err() }, reads)
			if change == "valid" {
				require.NoError(t, err)
				require.Equal(t, "ordinary", got.Kind)
				require.Equal(t, r.SHA, got.PullRequest.HeadSHA)
				require.GreaterOrEqual(t, sourceReads, 2)
			} else {
				require.Error(t, err)
				if change == "moved base" {
					require.ErrorContains(t, err, "qualified review base no longer matches")
				}
				if change != "moved head" && change != "moved base" {
					require.Zero(t, sourceReads, "invalid persisted scope reached provider source adapter")
				}
			}
			require.Zero(t, f.writes, "review capture wrote provider state")
			require.False(t, f.p.Active() && change != "active restatement", "qualification activated pipeline")
		})
	}
}
