package installation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"

	"github.com/znasllc-io/memql/component/auth"
)

// WorkflowNames are operator configuration, never fields in a selection request.
// Names must resolve to installed templates. Native evidence gates remain mandatory.
type WorkflowNames struct {
	Preparation string `json:"preparation"`
	Revision    string `json:"revision"`
	Artifacts   string `json:"artifacts"`
	Completion  string `json:"completion"`
	Rollback    string `json:"rollback"`
}

func (n WorkflowNames) defaults() WorkflowNames {
	if n.Preparation == "" {
		n.Preparation = installationPrepareWorkflow
	}
	if n.Revision == "" {
		n.Revision = installationRevisionWorkflow
	}
	if n.Artifacts == "" {
		n.Artifacts = installationArtifactWorkflow
	}
	if n.Completion == "" {
		n.Completion = "installationCompletionWorkflow"
	}
	if n.Rollback == "" {
		n.Rollback = "installationRollbackWorkflow"
	}
	return n
}
func (n WorkflowNames) validate() error {
	valid := regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,127}$`)
	for _, v := range []string{n.Preparation, n.Revision, n.Artifacts, n.Completion, n.Rollback} {
		if !valid.MatchString(v) {
			return errors.New("installation workflow name is invalid")
		}
	}
	return nil
}
func sourceRevision(source json.RawMessage) string {
	var v struct {
		Revision string `json:"targetRevision"`
	}
	if json.Unmarshal(source, &v) != nil {
		return ""
	}
	return v.Revision
}
func beforeRevision(spec json.RawMessage) string {
	var v struct {
		Source json.RawMessage `json:"source"`
	}
	if json.Unmarshal(spec, &v) != nil {
		return ""
	}
	return sourceRevision(v.Source)
}

// SelectionResult is public review data; it cannot authorize an effect.
type SelectionApplication struct {
	Key             string         `json:"key"`
	Name            string         `json:"name"`
	Namespace       string         `json:"namespace"`
	CurrentRevision string         `json:"currentRevision,omitempty"`
	Started         bool           `json:"started"`
	Observation     map[string]any `json:"observation,omitempty"`
}

type SelectionResult struct {
	Direction               string                 `json:"direction,omitempty"`
	Applications            []SelectionApplication `json:"applications,omitempty"`
	InstallationID          string                 `json:"installationId"`
	PreparationID           string                 `json:"preparationId"`
	PlanID                  string                 `json:"planId"`
	State                   string                 `json:"state"`
	CandidateID             string                 `json:"candidateId"`
	CatalogDigest           string                 `json:"catalogDigest"`
	OverlayRevision         string                 `json:"overlayRevision"`
	ExpectedCurrentRevision string                 `json:"expectedCurrentRevision"`
	ArtifactExpiresAt       string                 `json:"artifactExpiresAt"`
	Observation             map[string]any         `json:"observation,omitempty"`
	CanManage               bool                   `json:"canManage"`
	CanRollback             bool                   `json:"canRollback"`
}
type SelectionStatus struct {
	Applications    []SelectionApplication `json:"applications,omitempty"`
	InstallationID  string                 `json:"installationId"`
	CurrentRevision string                 `json:"currentRevision"`
	Active          *SelectionResult       `json:"active,omitempty"`
	Latest          *SelectionResult       `json:"latest,omitempty"`
	Preparation     *SelectionPreparation  `json:"preparation,omitempty"`
}

// SelectionPreparation exposes the durable identity of unfinished work, never
// its source credentials, manifests or proof. Only its original operator may
// explicitly resume it; reloading a browser cannot invent a new reservation.
type SelectionPreparation struct {
	PrepareSelection
	PreparationID string `json:"preparationId"`
	State         string `json:"state"`
	CanResume     bool   `json:"canResume"`
}

func selectedPreparation(r preparationRecord, actor string) SelectionPreparation {
	return SelectionPreparation{PrepareSelection: PrepareSelection{InstallationID: r.Scope.InstallationID, RequestID: r.Scope.RequestID, CandidateID: r.Scope.CandidateID, CatalogDigest: r.Scope.PublicationDigest, OverlayRevision: r.Scope.Intent.Revision, ExpectedCurrentRevision: beforeRevision(r.Scope.Intent.BeforeSpec), Prune: r.Scope.Intent.Prune}, PreparationID: r.ID, State: r.State, CanResume: actor == r.Scope.RequestedBy && r.State == "preparing"}
}
func selectionResult(ctx context.Context, r revisionRecord) SelectionResult {
	out := selectedResult(r)
	actor, err := installationActor(ctx)
	out.CanManage = err == nil && actor == r.Plan.RequestedBy
	out.CanRollback = out.CanManage && r.RollbackEligible
	return out
}

// ResumeSelected reconstructs the original selection from the native journal.
// No mutable client draft, new request identity or supplied authority is used.
func (p *Preparer) ResumeSelected(ctx context.Context, installationID, preparationID string) (SelectionResult, error) {
	ctx, err := p.selectionContext(ctx)
	if err != nil {
		return SelectionResult{}, err
	}
	r, err := p.host.journal.get(ctx, installationID, preparationID)
	if err != nil {
		return SelectionResult{}, err
	}
	actor, err := installationActor(ctx)
	if err != nil || actor != r.Scope.RequestedBy {
		return SelectionResult{}, errors.New("only the original operator may resume this preparation")
	}
	if r.State != "preparing" && r.State != "promoted" {
		return SelectionResult{}, errors.New("this preparation can no longer be resumed")
	}
	return p.PrepareSelected(ctx, selectedPreparation(r, actor).PrepareSelection)
}

// CancelSelected releases only an unstarted plan. The journal locks the current
// head and original operator together; an applying plan can never be cancelled.
func (p *Preparer) CancelSelected(ctx context.Context, installationID, planID string) (SelectionResult, error) {
	ctx, err := p.selectionContext(ctx)
	if err != nil {
		return SelectionResult{}, err
	}
	r, err := (&revisionJournal{db: p.host.journal.db}).cancel(ctx, installationID, planID)
	return selectionResult(ctx, r), err
}

func selectedResult(r revisionRecord) SelectionResult {
	out := SelectionResult{InstallationID: r.Plan.InstallationID, PlanID: r.ID, State: r.State, CandidateID: r.Plan.CandidateID, CatalogDigest: r.Plan.PublicationDigest, OverlayRevision: r.Plan.Intent.Revision, ExpectedCurrentRevision: r.Plan.RollbackRevision}
	if b := r.Plan.Preparation; b != nil {
		out.PreparationID = b.ID
		out.ArtifactExpiresAt = b.ArtifactExpiresAt
	}
	if r.State == "reverting" {
		out.State = "applying"
	}
	if r.Rollback != nil || r.State == "reverting" {
		out.Direction = "rollback"
	}
	for _, app := range r.Plan.Applications {
		member := r.Applications[app.Key]
		if r.Rollback != nil {
			member = r.Rollback.Applications[app.Key]
		}
		item := SelectionApplication{Key: app.Key, Namespace: app.Intent.Target.Namespace, Name: app.Intent.Target.Name, Started: member.Started}
		if member.Facts != nil {
			f := member.Facts
			item.Observation = map[string]any{"intentObserved": f.IntentObserved, "operationPending": f.OperationPending, "operationPhase": f.OperationPhase, "operationSucceeded": f.OperationSucceeded, "revisionObserved": f.RevisionObserved, "healthy": f.Healthy, "synced": f.Synced}
		}
		out.Applications = append(out.Applications, item)
	}
	facts := r.Observation
	if r.Rollback != nil {
		facts = r.Rollback.Observation
	}
	if f := facts; f != nil {
		out.Observation = map[string]any{"intentObserved": f.IntentObserved, "operationPending": f.OperationPending, "operationPhase": f.OperationPhase, "operationSucceeded": f.OperationSucceeded, "revisionObserved": f.RevisionObserved, "healthy": f.Healthy, "synced": f.Synced}
	}
	return out
}
func (p *Preparer) selectionContext(ctx context.Context) (context.Context, error) {
	if _, err := installationActor(ctx); err != nil {
		return nil, err
	}
	if p == nil || p.host == nil {
		return nil, errors.New("installation selection is not configured")
	}
	// Only the exact native selection host receives internal origin. The resolved
	// actor and all credential ceilings are preserved; no scope token escapes.
	return auth.ContextWithInternalOrigin(ctx), nil
}

// PrepareSelected is the public manual-selection gate. It creates a durable
// review plan; it never applies it. Recovery keeps the same request identity.
func (p *Preparer) PrepareSelected(ctx context.Context, selection PrepareSelection) (SelectionResult, error) {
	ctx, err := p.selectionContext(ctx)
	if err != nil {
		return SelectionResult{}, err
	}
	if !commitDigest.MatchString(selection.ExpectedCurrentRevision) {
		return SelectionResult{}, errors.New("selection requires the exact reviewed current revision")
	}
	cfg, err := readReceiver(ctx, p.host.api, p.host.namespace, p.host.configurationName, p.host.catalog)
	if err != nil {
		return SelectionResult{}, err
	}
	if cfg.configuration.Health == nil || p.host.health == nil {
		return SelectionResult{}, errors.New("installation selection requires authenticated continuity verification")
	}
	result, err := p.Prepare(ctx, selection)
	if err != nil {
		return SelectionResult{}, err
	}
	r, err := (&revisionJournal{db: p.host.journal.db}).get(ctx, result.InstallationID, result.PlanID)
	return selectionResult(ctx, r), err
}

// StartSelected authorizes only the exact prepared plan, under its original
// operator and recipe. The native host rechecks every mutable observation.
func (p *Preparer) StartSelected(ctx context.Context, installationID, planID string) (SelectionResult, error) {
	return p.selectedPass(ctx, installationID, planID, revisionStart)
}

// ObserveSelected runs the captured completion recipe. It cannot apply an
// intent, and completion requires fresh images, continuity and preservation.
func (p *Preparer) ObserveSelected(ctx context.Context, installationID, planID string) (SelectionResult, error) {
	ctx, err := p.selectionContext(ctx)
	if err != nil {
		return SelectionResult{}, err
	}
	if !identifier.MatchString(installationID) || !internalDigest.MatchString(planID) {
		return SelectionResult{}, errors.New("exact installation and plan identities are required")
	}
	r, err := p.completeSelected(ctx, installationID, planID)
	return selectionResult(ctx, r), err
}
func (p *Preparer) selectedPass(ctx context.Context, installationID, planID string, mode revisionMode) (SelectionResult, error) {
	ctx, err := p.selectionContext(ctx)
	if err != nil {
		return SelectionResult{}, err
	}
	if !identifier.MatchString(installationID) || !internalDigest.MatchString(planID) {
		return SelectionResult{}, errors.New("exact installation and plan identities are required")
	}
	r, err := p.host.revisions.run(ctx, &revisionJournal{db: p.host.journal.db}, installationID, planID, mode, nil, p.host)
	return selectionResult(ctx, r), err
}

// SelectionStatus reads trusted receiving configuration and the current durable
// selection. It never returns endpoints, secrets, rendered manifests or grants.
func (p *Preparer) SelectionStatus(ctx context.Context) (SelectionStatus, error) {
	ctx, err := p.selectionContext(ctx)
	if err != nil {
		return SelectionStatus{}, err
	}
	db, err := p.host.journal.database()
	if err != nil {
		return SelectionStatus{}, err
	}
	// Status needs only named configuration and Application metadata. It works
	// during an active rollout and never reads catalog or registry credentials.
	reads := newReceiverReads(p.host.api)
	cm, err := reads.object(ctx, "v1", "ConfigMap", "configmaps", p.host.namespace, p.host.configurationName)
	if err != nil {
		return SelectionStatus{}, err
	}
	var cfg receiverConfiguration
	if receiverJSON([]byte(resourceText(resourceMap(cm, "data"), "installation.json")), &cfg) != nil || (cfg.FormatVersion != 1 && cfg.FormatVersion != 2) || !identifier.MatchString(cfg.InstallationID) {
		return SelectionStatus{}, errors.New("installation receiving configuration is invalid")
	}
	apps, err := cfg.applicationNames()
	if err != nil {
		return SelectionStatus{}, err
	}
	out := SelectionStatus{InstallationID: cfg.InstallationID}
	common := ""
	for index, application := range apps {
		app, err := reads.object(ctx, "argoproj.io/v1alpha1", "Application", "applications", application.Namespace, application.Name)
		if err != nil {
			return out, err
		}
		current := resourceText(resourceMap(resourceMap(app, "spec"), "source"), "targetRevision")
		if !commitDigest.MatchString(current) {
			return out, errors.New("installation requires immutable current application revisions")
		}
		if index == 0 {
			common = current
		} else if current != common {
			common = ""
		}
		out.Applications = append(out.Applications, SelectionApplication{Key: application.Key, Namespace: application.Namespace, Name: application.Name, CurrentRevision: current})
	}
	out.CurrentRevision = common
	if _, err := reads.finish(ctx); err != nil {
		return out, err
	}

	var key, preparation sql.NullString
	err = db.QueryRowContext(ctx, `SELECT active_plan_id,active_preparation_id FROM installation_revision_heads WHERE installation_id=$1`, out.InstallationID).Scan(&key, &preparation)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if preparation.Valid {
		r, e := p.host.journal.get(ctx, out.InstallationID, preparation.String)
		if e != nil {
			return out, e
		}
		actor, e := installationActor(ctx)
		if e != nil {
			return out, e
		}
		if r.State != "preparing" && r.State != "retiring" {
			return out, errChanged
		}
		v := selectedPreparation(r, actor)
		out.Preparation = &v
	}
	if key.Valid {
		r, e := (&revisionJournal{db: p.host.journal.db}).get(ctx, out.InstallationID, key.String)
		if e != nil {
			return out, e
		}
		v := selectionResult(ctx, r)
		out.Active = &v
	}
	var latest string
	err = db.QueryRowContext(ctx, `SELECT plan_id FROM installation_revision_attempts WHERE installation_id=$1 AND state IN ('complete','rolled_back') ORDER BY COALESCE(rollback_completed_at,completed_at) DESC,plan_id LIMIT 1`, out.InstallationID).Scan(&latest)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	if err == nil {
		r, e := (&revisionJournal{db: p.host.journal.db}).get(ctx, out.InstallationID, latest)
		if e != nil {
			return out, e
		}
		v := selectionResult(ctx, r)
		out.Latest = &v
	}
	return out, nil
}
