package installation

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/integrations/argocd"
)

// Preparation refuses a candidate that rewrites the receiver's own
// configuration, but an engine up to 0.31.28 never checked it, so a plan that
// engine prepared reaches this engine's start fence unchecked. The fence
// (revisionWorkflowScope.begin) refuses it after full write admission and
// before the durable start, so the plan stays prepared and can be cancelled;
// an unchanged configuration starts as before.
func TestRevisionStartRefusesAnOlderPlanThatChangesTheReceiverConfiguration(t *testing.T) {
	db, _ := journalDB(t)
	preparation := preparationConnection(db)
	ctx, prepared, plan, evidence := promotionFixture(t, preparation)
	r, err := preparation.promote(ctx, plan.InstallationID, prepared.ID, prepared.Scope.WorkflowDigest, prepared.Scope.ConfigurationDigest, plan, evidence)
	require.NoError(t, err)
	journal := &revisionJournal{db: func() *sql.DB { return db }}
	binding := r.Plan.Preparation
	f := newReceiverFixture(t)
	live := observeReceiver(t, f)
	installation := resourceText(resourceMap(f.objects[receiverConfigPath], "data"), "installation.json")
	edited := strings.Replace(installation, `"platform":"linux/arm64"`, `"platform":"linux/amd64"`, 1)
	require.NotEqual(t, installation, edited)
	scope := func(candidate string) *revisionWorkflowScope {
		now := time.Now().UTC()
		return &revisionWorkflowScope{
			workflow: &revisionWorkflow{digest: r.Plan.ExecutionWorkflowDigest, artifacts: &artifactWorkflow{digest: binding.ArtifactWorkflowDigest}},
			journal:  journal, installation: plan.InstallationID, key: r.ID, operator: plan.RequestedBy, mode: revisionStart,
			needsWrite: true, preflightComplete: true, reobserved: true,
			configuration: &receiverSnapshot{digest: binding.ConfigurationDigest, invariantDigest: binding.ConfigurationInvariantDigest, observed: now, own: live.own},
			renders: map[string]argocd.RenderedRevision{
				"rollback":  renderInventoryFixture(t, strings.Repeat("a", 40), append(resourceManifests(oldImage), receiverConfigManifest(installation, "memql", nil))),
				"candidate": renderInventoryFixture(t, strings.Repeat("b", 40), append(resourceManifests(nextImage), receiverConfigManifest(candidate, "memql", nil))),
			},
			resources: resourceEvidence{digest: r.Plan.ResourceDiffDigest, publication: r.Plan.PublicationDigest, before: r.Plan.RollbackRenderDigest, after: r.Plan.RenderDigest, observed: now},
			artifacts: &artifactEvidence{workflow: binding.ArtifactWorkflowDigest, operator: plan.RequestedBy, configuration: binding.ConfigurationDigest, candidate: r.Plan.PublicationDigest,
				rollback: binding.RollbackPublicationDigest, resources: r.Plan.ResourceDiffDigest, before: r.Plan.RollbackRenderDigest, after: r.Plan.RenderDigest, observed: now, expires: now.Add(20 * time.Minute)},
			storage:   storageEvidence{digest: binding.StorageDigest, before: r.Plan.RollbackRenderDigest, after: r.Plan.RenderDigest, observed: now},
			sensitive: sensitiveEvidence{digest: binding.SensitiveDigest, before: r.Plan.RollbackRenderDigest, after: r.Plan.RenderDigest, observed: now},
		}
	}

	_, err = scope(edited).begin(ctx, nil)
	require.ErrorContains(t, err, "changes the installation's own update configuration")
	require.ErrorContains(t, err, "cancel this preparation")
	current, err := journal.get(ctx, plan.InstallationID, r.ID)
	require.NoError(t, err)
	require.Equal(t, "prepared", current.State, "refused before the durable start, so the plan can still be cancelled")

	_, err = scope(installation).begin(ctx, nil)
	require.NoError(t, err)
	current, err = journal.get(ctx, plan.InstallationID, r.ID)
	require.NoError(t, err)
	require.Equal(t, "applying", current.State)
}
