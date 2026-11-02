package release

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/znasllc-io/memql/integrations/argocd"

	pl "github.com/znasllc-io/memql/component/pipelines"
)

// train_deploy.go -- what a deployment asks of the release integration and the cluster
// (design record docs/superpowers/specs/2026-11-02-nightly-release-from-main-design.md,
// section 7). The coordinator in component/pipelinerun decides; this only does.

// ConfigureDeployCluster binds the installation's ArgoCD. A node without one answers
// each cluster step with a refusal, and the deployment's deadlines say so.
func (i *Integration) ConfigureDeployCluster(client *argocd.Client) error {
	if i == nil || client == nil || !i.deployCluster.CompareAndSwap(nil, client) {
		return errors.New("deploy cluster client is absent or already configured")
	}
	return nil
}

func (i *Integration) deployClient() (*argocd.Client, error) {
	if i == nil {
		return nil, errors.New("the release integration is unavailable")
	}
	client := i.deployCluster.Load()
	if client == nil {
		return nil, errors.New("this node has no cluster API to deploy with")
	}
	return client, nil
}

// NewestRelease answers a release in the signed catalog: the named version, or the
// newest of the profile's component. The catalog is the only source, so a deploy can
// name nothing that was not published and sealed.
func (i *Integration) NewestRelease(ctx context.Context, owner string, authority map[string]any, profile, version string) (pl.DeployRelease, error) {
	var none pl.DeployRelease
	owned, _, err := i.standingOwner(ctx, owner, authority)
	if err != nil {
		return none, err
	}
	selected, _, _, err := i.candidateProfile(owned, profile)
	if err != nil {
		return none, err
	}
	if selected.Build == nil {
		return none, fmt.Errorf("release profile %s builds nothing", profile)
	}
	ledger, cfg, trust, err := i.catalog(owned)
	if err != nil {
		return none, err
	}
	db, err := ledger.database()
	if err != nil {
		return none, err
	}
	rows, err := db.QueryContext(owned, `SELECT candidate_id FROM release_catalog WHERE publisher=$1 ORDER BY created_at DESC LIMIT 50`, cfg.Publisher)
	if err != nil {
		return none, err
	}
	var candidates []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return none, err
		}
		candidates = append(candidates, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return none, err
	}
	var best pl.DeployRelease
	for _, id := range candidates {
		verified, err := PublishedCatalog(owned, db, id, cfg.Publisher, trust)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return none, err
		}
		component, err := overlayPublishedComponent(verified, selected.Build.Component)
		if err != nil {
			continue // Another component's release.
		}
		if version != "" && component.Version != version {
			continue
		}
		if best.Version == "" || pl.VersionAfter(component.Version, best.Version) {
			best = pl.DeployRelease{
				Version: component.Version, Tag: selected.Build.TagPrefix + component.Version, SHA: component.Commit,
				CandidateID: id, CatalogDigest: verified.Digest(),
			}
		}
	}
	if best.Version == "" {
		if version != "" {
			return none, fmt.Errorf("version %s of %s is not in the release catalog", version, selected.Build.Component)
		}
		return none, fmt.Errorf("the release catalog holds no release of %s", selected.Build.Component)
	}
	return best, nil
}

// ProposeDeployPin makes the release's pin on its own branch of the installation's
// repository, through the operator's overlay and recipe. It refuses to invent a fact:
// every byte comes from the signed publication or the file it replaces.
func (i *Integration) ProposeDeployPin(ctx context.Context, r pl.DeployPinRequest) (pl.DeployPin, error) {
	var none pl.DeployPin
	owned, _, err := i.standingOwner(ctx, r.OwnerUserID, r.Authority)
	if err != nil {
		return none, err
	}
	cfg, err := i.overlayConfig(owned)
	if err != nil {
		return none, err
	}
	stored, err := i.proposeOverlay(owned, cfg, overlayRequest{cfg.InstallationID, r.RequestID, r.CandidateID, r.CatalogDigest, r.ExpectedBase}, true)
	if err != nil {
		return none, err
	}
	if stored.PullRequest < 1 || !installationCommit.MatchString(stored.BranchCommit) {
		return none, errors.New("the pin has no confirmed pull request")
	}
	return pl.DeployPin{ProposalID: stored.Record.ID, PullRequest: stored.PullRequest, PullRequestURL: stored.PullRequestURL, Commit: stored.BranchCommit}, nil
}

func deployFacts(uid string, facts argocd.SyncFacts) pl.DeployApplicationFacts {
	return pl.DeployApplicationFacts{
		UID: uid, Revision: facts.Revision, Phase: facts.Phase, Message: facts.Message, Health: facts.Health, Mine: facts.Mine,
		Pending: facts.Pending, Succeeded: facts.Succeeded, Partial: facts.Partial, Healthy: facts.Healthy, Settled: facts.Settled,
	}
}

// ReadApplication reads one Application by name.
func (i *Integration) ReadApplication(ctx context.Context, namespace, name string) (pl.DeployApplicationFacts, error) {
	client, err := i.deployClient()
	if err != nil {
		return pl.DeployApplicationFacts{}, err
	}
	target, facts, err := client.ReadApplication(ctx, namespace, name)
	if err != nil {
		return pl.DeployApplicationFacts{}, err
	}
	return deployFacts(target.UID, facts), nil
}

// StartSync starts or reports one Application's sync at an exact commit.
func (i *Integration) StartSync(ctx context.Context, r pl.DeployRequest) (pl.DeployApplicationFacts, error) {
	client, err := i.deployClient()
	if err != nil {
		return pl.DeployApplicationFacts{}, err
	}
	facts, err := client.StartSync(ctx, argocd.SyncRequest{
		Target: argocd.Target{Namespace: r.Namespace, Name: r.Name, UID: r.UID}, RequestID: r.RequestID, Revision: r.Revision,
	})
	if errors.Is(err, argocd.ErrBusy) {
		return pl.DeployApplicationFacts{}, pl.ErrDeployBusy
	}
	if err != nil {
		return pl.DeployApplicationFacts{}, err
	}
	return deployFacts(r.UID, facts), nil
}

// TerminateSync stops a running operation on one Application.
func (i *Integration) TerminateSync(ctx context.Context, r pl.DeployRequest) error {
	client, err := i.deployClient()
	if err != nil {
		return err
	}
	return client.TerminateSync(ctx, argocd.Target{Namespace: r.Namespace, Name: r.Name, UID: r.UID})
}
