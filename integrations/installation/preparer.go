package installation

import (
	"context"
	"database/sql"
	"errors"

	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/integrations/argocd"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

// PreparationDependencies are native wiring, never DSL or client parameters.
// The caller supplies the installed engine revision and an authenticated API;
// every preparation reconstructs its configuration from the named ConfigMap.
type PreparationDependencies struct {
	API      argocd.API
	Database func() *sql.DB
	Executor interface {
		pipelines.Executor
		pipelines.ReceiptAcknowledger
	}
	Files interface {
		pipelinesteps.LibraryReceiptOpener
		pipelinesteps.LibraryArtifactLifecycle
	}
	Tokens                                       pipelinesteps.TokenMinter
	Catalog                                      CatalogFactory
	Namespace, ConfigurationName, EngineRevision string
}

// Preparer is a native admission host. It registers no integration capabilities
// and accepts only an already-admitted internal operator context.
type Preparer struct{ host *preparationHost }

// PrepareSelection selects a publication and reviewed overlay, not authority,
// connections, credentials, rendered resources or evidence fingerprints.
type PrepareSelection struct {
	InstallationID  string `json:"installationId"`
	RequestID       string `json:"requestId"`
	CandidateID     string `json:"candidateId"`
	CatalogDigest   string `json:"catalogDigest"`
	OverlayRevision string `json:"overlayRevision"`
	Prune           bool   `json:"prune"`
}

// PreparationResult is a summary of the durable journal, never a transferable
// permission to apply. A revision operation must reload and requalify it.
type PreparationResult struct {
	InstallationID    string `json:"installationId"`
	PreparationID     string `json:"preparationId"`
	PlanID            string `json:"planId"`
	State             string `json:"state"`
	ArtifactExpiresAt string `json:"artifactExpiresAt"`
}

func NewPreparer(deps PreparationDependencies) (*Preparer, error) {
	if deps.API == nil || deps.Database == nil || deps.Executor == nil || deps.Files == nil || deps.Tokens == nil || deps.Catalog == nil {
		return nil, errors.New("installation preparation requires all native execution and lifecycle ports")
	}
	if _, err := receiverPath("v1", "configmaps", deps.Namespace, deps.ConfigurationName); err != nil {
		return nil, err
	}
	host := &preparationHost{api: deps.API, journal: &preparationJournal{db: deps.Database}, executor: deps.Executor, files: deps.Files, tokens: deps.Tokens, catalog: deps.Catalog, namespace: deps.Namespace, configurationName: deps.ConfigurationName}
	if err := host.bindWorkflow(deps.EngineRevision); err != nil {
		return nil, err
	}
	return &Preparer{host: host}, nil
}

func (p *Preparer) Prepare(ctx context.Context, selection PrepareSelection) (PreparationResult, error) {
	if p == nil || p.host == nil {
		return PreparationResult{}, errors.New("installation preparation is unavailable")
	}
	record, err := p.host.prepare(ctx, preparationRequest{InstallationID: selection.InstallationID, RequestID: selection.RequestID, CandidateID: selection.CandidateID, CatalogDigest: selection.CatalogDigest, OverlayRevision: selection.OverlayRevision, Prune: selection.Prune})
	if err != nil {
		return PreparationResult{}, err
	}
	binding := record.Plan.Preparation
	if binding == nil {
		return PreparationResult{}, errors.New("installation revision has no admitted preparation")
	}
	return PreparationResult{InstallationID: record.Plan.InstallationID, PreparationID: binding.ID, PlanID: record.ID, State: record.State, ArtifactExpiresAt: binding.ArtifactExpiresAt}, nil
}
