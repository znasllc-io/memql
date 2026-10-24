package installation

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/argocd"
)

const rollbackContract = "installation.rollback.v1"

type rollbackWorkflowScope struct {
	revision *revisionWorkflowScope
	snapshot *workflowhost.Snapshot
	observed bool
}

// RollbackSelected reverses only this original operator's started selection.
// The saved recipe derives each reversal from its owned terminal controller
// operation. It cannot downgrade a migrated database or replace a pending sync.
func (p *Preparer) RollbackSelected(ctx context.Context, installationID, planID string) (SelectionResult, error) {
	ctx, err := p.selectionContext(ctx)
	if err != nil {
		return SelectionResult{}, err
	}
	if !identifier.MatchString(installationID) || !internalDigest.MatchString(planID) {
		return SelectionResult{}, errors.New("exact installation and plan identities are required")
	}
	actor, err := preparationActor(ctx)
	if err != nil {
		return SelectionResult{}, err
	}
	journal := &revisionJournal{db: p.host.journal.db}
	record, err := journal.get(ctx, installationID, planID)
	if err != nil {
		return SelectionResult{}, err
	}
	if record.Plan.RequestedBy != actor {
		return SelectionResult{}, errors.New("only the original operator may reverse this installation")
	}
	if record.State == "rolled_back" {
		return selectionResult(ctx, record), nil
	}
	if record.State == "complete" {
		record, err = journal.reacquireRollback(ctx, installationID, planID)
		if err != nil {
			return SelectionResult{}, err
		}
	}
	if !record.rollbackActive() || record.Plan.Preparation == nil || record.Plan.HealthBaseline == nil || record.Plan.RollbackWorkflow == nil {
		return SelectionResult{}, errors.New("rollback requires the exact started selection and its retained recipe")
	}
	client, err := argocd.New(p.host.api)
	if err != nil {
		return SelectionResult{}, err
	}
	revision := &revisionWorkflowScope{workflow: &revisionWorkflow{digest: record.Plan.ExecutionWorkflowDigest, artifacts: &artifactWorkflow{digest: record.Plan.Preparation.ArtifactWorkflowDigest}}, host: p.host, journal: journal, client: client, installation: installationID, key: planID, operator: actor, mode: revisionStart, rollbackExecution: true, renders: map[string]argocd.RenderedRevision{}, sources: map[string]verifiedSourceCapture{}}
	s := &rollbackWorkflowScope{revision: revision, snapshot: record.Plan.RollbackWorkflow}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	_, err = workflowhost.RunSnapshot(ctx, s.snapshot, rollbackContract, nil, workflowhost.Options{Operations: s.operations()})
	if err != nil {
		return selectionResult(ctx, record), err
	}
	if !s.observed {
		return SelectionResult{}, errors.New("rollback recipe returned no native observation")
	}
	record, err = journal.get(ctx, installationID, planID)
	return selectionResult(ctx, record), err
}
func (s *rollbackWorkflowScope) operations() map[string]workflowhost.Operation {
	ops := s.revision.operations()
	for _, name := range []string{"installationRevisionRead", "installationRevisionConfiguration", "installationRevisionReobserveConfig", "installationRevisionBegin", "installationRevisionApply", "installationRevisionObserve"} {
		delete(ops, name)
	}
	ops["installationRollbackRead"] = s.read
	ops["installationRollbackConfiguration"] = s.configuration
	ops["installationRollbackReopenSource"] = s.revision.reopen
	ops["installationRollbackRecheckResources"] = s.revision.recheckResources
	ops["installationRollbackReobserveConfiguration"] = s.reobserve
	ops["installationRollbackReserve"] = s.reserve
	ops["installationRollbackBegin"] = s.begin
	ops["installationRollbackApply"] = s.apply
	ops["installationRollbackObserve"] = s.observe
	// These public DSL names predate the receiver host. Their capabilities stay
	// bounded to the same selected plan; they never invoke preparation again.
	ops["installationResolvePublications"] = s.revision.resolve
	ops["installationCheckCompatibility"] = s.revision.compatibility
	ops["installationRenderSource"] = s.revision.render
	ops["installationVerifyResources"] = s.revision.verifyResources
	ops["installationVerifyStorage"] = s.revision.verifyStorage
	ops["installationVerifyProtected"] = s.revision.verifyProtected
	for name, operation := range ops {
		ops[name] = func(ctx context.Context, args map[string]any) (any, error) {
			actor, err := preparationActor(ctx)
			if err != nil || actor != s.revision.operator {
				return nil, errors.New("rollback operation is outside its original operator scope")
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return operation(ctx, args)
		}
	}
	return ops
}
func (s *rollbackWorkflowScope) record(ctx context.Context) (revisionRecord, error) {
	r, err := s.revision.writable(ctx)
	if err != nil {
		return r, err
	}
	if !r.rollbackActive() || r.Plan.RollbackWorkflow == nil || s.snapshot == nil || r.Plan.RollbackWorkflow.Version != s.snapshot.Version {
		return r, errors.New("rollback selected recipe changed")
	}
	return r, nil
}
func (s *rollbackWorkflowScope) read(ctx context.Context, args map[string]any) (any, error) {
	if err := noRevisionArguments(args); err != nil {
		return nil, err
	}
	r, err := s.record(ctx)
	if err != nil {
		return nil, err
	}
	needs := r.Rollback == nil || !r.Rollback.Started
	if r.Rollback != nil {
		for _, app := range r.Plan.applicationMembers() {
			reverse, ok := r.Rollback.application(app.Key)
			if !ok {
				continue
			}
			facts, e := s.revision.client.Observe(ctx, reverse.Intent)
			if e == nil {
				if !reverse.Started || !facts.IntentObserved {
					return nil, errors.New("rollback effect preceded its durable start")
				}
			} else if errors.Is(e, argocd.ErrChanged) {
				needs = true
			} else {
				return nil, e
			}
		}
	}
	return map[string]any{"needsWrite": needs}, nil
}
func (s *rollbackWorkflowScope) configuration(ctx context.Context, args map[string]any) (any, error) {
	if err := noRevisionArguments(args); err != nil {
		return nil, err
	}
	r, err := s.record(ctx)
	if err != nil {
		return nil, err
	}
	cfg, err := readSelectedReceiver(ctx, s.revision.host, r)
	if err != nil {
		return nil, err
	}
	s.revision.configuration = cfg
	return nil, nil
}
func (s *rollbackWorkflowScope) reobserve(ctx context.Context, args map[string]any) (any, error) {
	if err := noRevisionArguments(args); err != nil {
		return nil, err
	}
	r, err := s.record(ctx)
	if err != nil {
		return nil, err
	}
	fresh, err := readSelectedReceiver(ctx, s.revision.host, r)
	if err != nil {
		return nil, err
	}
	if s.revision.configuration == nil || fresh.digest != s.revision.configuration.digest {
		return nil, errors.New("configuration changed during rollback qualification")
	}
	s.revision.configuration = fresh
	s.revision.preflightComplete = true
	s.revision.reobserved = true
	return nil, nil
}

// A reverse sync is never a database downgrade. Even if the new application is
// unhealthy, the authenticated original session must prove unchanged migrations.
func (s *rollbackWorkflowScope) continuityGuard(ctx context.Context, r revisionRecord) error {
	cfg := s.revision.configuration
	if cfg == nil || cfg.configuration.Health == nil || cfg.healthCredential == "" || s.revision.host.health == nil || r.Plan.HealthBaseline == nil {
		return errors.New("rollback needs authenticated migration continuity")
	}
	h := cfg.configuration.Health
	nonce := id.NewShortId()
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	now, err := s.revision.host.health(probeCtx, strings.TrimSuffix(h.Endpoint, "/"), cfg.healthCredential, cfg.healthRoots, nonce)
	before := r.Plan.HealthBaseline
	if err != nil || now.validate() != nil || now.Nonce != nonce || now.UserID != before.UserID || now.IdentityID != before.IdentityID || now.MigrationsDigest != before.MigrationsDigest {
		return errors.New("rollback cannot prove unchanged migrations and original authenticated session; human intervention is required")
	}
	allowed := false
	for _, pub := range []struct{ commit string }{{before.EngineCommit}} {
		allowed = allowed || now.EngineCommit == pub.commit
	}
	publication, err := s.revision.candidate.Release()
	if err != nil {
		return err
	}
	for _, component := range publication.Components {
		if component.Name == h.Component && component.Commit == now.EngineCommit {
			allowed = true
		}
	}
	if !allowed {
		return errors.New("rollback serving engine is outside the selected release pair")
	}
	return nil
}
func (s *rollbackWorkflowScope) reserve(ctx context.Context, args map[string]any) (any, error) {
	return s.revision.journal.withEffectLock(ctx, s.revision.key, func(ctx context.Context) (any, error) { return s.reserveLocked(ctx, args) })
}
func (s *rollbackWorkflowScope) reserveLocked(ctx context.Context, args map[string]any) (any, error) {
	if err := noRevisionArguments(args); err != nil {
		return nil, err
	}
	r, err := s.record(ctx)
	if err != nil {
		return nil, err
	}
	if _, err = s.revision.freshWriteEvidence(r); err != nil {
		return nil, err
	}
	if err = s.continuityGuard(ctx, r); err != nil {
		return nil, err
	}
	if r.Rollback != nil {
		return nil, nil
	}
	p := rollbackPlan{ParentID: r.ID, RequestedBy: s.revision.operator, WorkflowDigest: artifactHash("rollback-workflow", s.snapshot.Version), ConfigurationDigest: s.revision.configuration.digest, ArtifactDigest: s.revision.artifacts.digest, StorageDigest: s.revision.storage.digest, SensitiveDigest: s.revision.sensitive.digest, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), ArtifactExpiresAt: s.revision.artifacts.expires.UTC().Format(time.RFC3339Nano)}
	if r.Plan.FormatVersion == 2 {
		p.FormatVersion = 2
	}
	for _, app := range r.Plan.applicationMembers() {
		if r.Plan.FormatVersion == 2 && !r.Applications[app.Key].Started {
			continue
		}
		request := "rollback-" + string(id.NewUntracked().FromString(r.ID+":"+app.Key))
		// Exclude live writers while classifying the current baseline. The
		// recorded baseline sync CAS fences even an uncertain delayed request.
		baseline, baselineErr := s.revision.client.Read(ctx, app.Intent.Target)
		intent := argocd.Intent{}
		untouched := false
		if baselineErr == nil {
			intent, baselineErr = argocd.PlanUnappliedRollback(baseline, app.Intent, request)
			untouched = baselineErr == nil
		}
		if !untouched {
			intent, err = s.revision.client.PlanRollback(ctx, app.Intent, request)
			if err != nil {
				return nil, err
			}
		}

		if p.FormatVersion == 2 {
			p.Applications = append(p.Applications, rollbackApplication{Key: app.Key, Intent: intent, BaselineFence: untouched})
		} else {
			p.Intent = intent
			p.BaselineFence = untouched
		}
	}
	if len(p.Applications) > 0 {
		p.Intent = p.Applications[0].Intent
	}
	if _, err = s.revision.freshWriteEvidence(r); err != nil {
		return nil, err
	}
	_, err = s.revision.journal.reserveRollback(ctx, r.Plan.InstallationID, r.ID, p)
	return nil, err
}
func (s *rollbackWorkflowScope) begin(ctx context.Context, args map[string]any) (any, error) {
	if err := noRevisionArguments(args); err != nil {
		return nil, err
	}
	r, err := s.record(ctx)
	if err != nil {
		return nil, err
	}
	if _, err = s.revision.freshWriteEvidence(r); err != nil {
		return nil, err
	}
	_, err = s.revision.journal.beginRollback(ctx, r.Plan.InstallationID, r.ID, artifactHash("rollback-workflow", s.snapshot.Version))
	return nil, err
}
func (s *rollbackWorkflowScope) apply(ctx context.Context, args map[string]any) (any, error) {
	return s.revision.journal.withEffectLock(ctx, s.revision.key, func(ctx context.Context) (any, error) { return s.applyLocked(ctx, args) })
}
func (s *rollbackWorkflowScope) applyLocked(ctx context.Context, args map[string]any) (any, error) {
	application, err := applicationArgument(args)
	if err != nil {
		return nil, err
	}
	r, err := s.record(ctx)
	if err != nil {
		return nil, err
	}
	if _, err = r.Plan.application(application); err != nil {
		return nil, err
	}
	if r.Rollback == nil || !r.Rollback.Started {
		return nil, errors.New("rollback needs its durable start")
	}
	reverse, ok := r.Rollback.application(application)
	if !ok {
		return nil, nil
	}
	if _, err = s.revision.client.Observe(ctx, reverse.Intent); err == nil {
		if !reverse.Started {
			return nil, errors.New("rollback effect preceded its start")
		}
		return nil, nil
	} else if !errors.Is(err, argocd.ErrChanged) {
		return nil, err
	}
	if reverse.BaselineFence {
		forward, _ := r.Plan.application(application)
		if facts, e := s.revision.client.Observe(ctx, forward.Intent); e == nil && facts.IntentObserved && !facts.OperationPending {
			replacement, e := s.revision.client.PlanRollback(ctx, forward.Intent, "reconciled-"+string(id.NewUntracked().FromString(r.ID+":"+application)))
			if e != nil {
				return nil, e
			}
			r, e = s.revision.journal.reconcileRollbackApplication(ctx, r.Plan.InstallationID, r.ID, application, replacement)
			if e != nil {
				return nil, e
			}
			reverse, _ = r.Rollback.application(application)
		}
	}
	admission, err := s.revision.freshWriteEvidence(r)
	if err != nil {
		return nil, err
	}
	if _, err = readSelectedReceiver(ctx, s.revision.host, r); err != nil {
		return nil, err
	}
	if err = s.continuityGuard(ctx, r); err != nil {
		return nil, err
	}
	if r.Plan.FormatVersion == 2 {
		r, err = s.revision.journal.beginRollbackApplication(ctx, r.Plan.InstallationID, r.ID, application, artifactHash("rollback-workflow", s.snapshot.Version), admission)
		if err != nil {
			return nil, err
		}
	}
	if _, err = s.revision.freshWriteEvidence(r); err != nil {
		return nil, err
	}
	effect, cancel := context.WithDeadline(ctx, admission.freshUntil)
	defer cancel()
	if _, err = s.revision.client.Apply(effect, reverse.Intent); err != nil {
		return nil, errors.New("rollback effect is unconfirmed; resume its recorded intent")
	}
	return nil, nil
}
func (s *rollbackWorkflowScope) observe(ctx context.Context, args map[string]any) (any, error) {
	if err := noRevisionArguments(args); err != nil {
		return nil, err
	}
	r, err := s.record(ctx)
	if err != nil {
		return nil, err
	}
	if r.Rollback == nil || !r.Rollback.Started {
		return nil, errors.New("rollback observation needs a durable reversal")
	}
	if _, err = readSelectedReceiver(ctx, s.revision.host, r); err != nil {
		return nil, err
	}
	if r.Plan.FormatVersion == 2 {
		for _, app := range r.Rollback.Plan.Applications {
			prior := r.Rollback.Applications[app.Key]
			if !prior.Started {
				return nil, errors.New("rollback member is not started")
			}
			effective, _ := r.Rollback.application(app.Key)
			facts, err := s.revision.client.Observe(ctx, effective.Intent)
			if err != nil {
				return nil, err
			}
			r, err = s.revision.journal.observeRollbackApplication(ctx, r.Plan.InstallationID, r.ID, app.Key, prior.Version, facts)
			if err != nil {
				return nil, err
			}
		}
	} else {
		effective, _ := r.Rollback.application("")
		facts, err := s.revision.client.Observe(ctx, effective.Intent)
		if err != nil {
			return nil, err
		}
		r, err = s.revision.journal.observeRollback(ctx, r.Plan.InstallationID, r.ID, r.Rollback.ObservationVersion, facts)
		if err != nil {
			return nil, err
		}
	}
	s.observed = true
	return nil, nil
}
