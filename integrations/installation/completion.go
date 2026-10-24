package installation

import (
	"context"
	"errors"
	"time"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/integrations/argocd"
)

const completionContract = "installation.completion.v1"

type completionScope struct {
	revision              *revisionWorkflowScope
	record                revisionRecord
	snapshot              *workflowhost.Snapshot
	workloads             *workloadWorkflowScope
	health                *HealthObservation
	observationVersion    int64
	converged, reobserved bool
}

func (s *completionScope) operations() map[string]workflowhost.Operation {
	ops := s.revision.operations()
	// A completion scope has no authority to apply or start any controller effect.
	for _, name := range []string{"installationRevisionBegin", "installationRevisionApply", "installationRevisionRead", "installationRevisionConfiguration", "installationRevisionReobserveConfig", "installationRevisionObserve"} {
		delete(ops, name)
	}
	ops["installationCompletionRead"] = s.read
	ops["installationCompletionConfiguration"] = s.configuration
	ops["installationCompletionPrepareWorkloads"] = s.prepareWorkloads
	ops["installationCompletionContinuity"] = s.continuity
	ops["installationCompletionReobserve"] = s.reobserve
	ops["installationCompletionRecord"] = s.complete
	for _, name := range []string{"installationWorkloadRequirements", "installationWorkloadCheck", "installationWorkloadsComplete"} {
		ops[name] = func(ctx context.Context, args map[string]any) (any, error) {
			if s.workloads == nil {
				return nil, errors.New("completion workloads have no native scope")
			}
			return s.workloads.operations()[name](ctx, args)
		}
	}
	return ops
}
func (p *Preparer) completeSelected(ctx context.Context, installationID, planID string) (revisionRecord, error) {
	actor, err := preparationActor(ctx)
	if err != nil {
		return revisionRecord{}, err
	}
	journal := &revisionJournal{db: p.host.journal.db}
	record, err := journal.get(ctx, installationID, planID)
	if err != nil {
		return record, err
	}
	if record.State == "complete" || record.State == "rolled_back" {
		return record, nil
	}
	if !record.rollbackActive() || record.Plan.RequestedBy != actor || record.Plan.Preparation == nil || record.Plan.HealthBaseline == nil || record.Plan.CompletionWorkflow == nil {
		return record, errors.New("installation completion requires the original selected plan and continuity baseline")
	}
	if record.State == "reverting" && record.Rollback == nil {
		return record, errors.New("resume the selected rollback before observing its completion")
	}
	client, err := argocd.New(p.host.api)
	if err != nil {
		return record, err
	}
	// Only read/evidence operations use this reconstructed identity. The write
	// operations are absent, and the source closure comes from the native journal.
	revision := &revisionWorkflowScope{workflow: &revisionWorkflow{digest: record.Plan.ExecutionWorkflowDigest, artifacts: &artifactWorkflow{digest: record.Plan.Preparation.ArtifactWorkflowDigest}}, host: p.host, journal: journal, client: client, installation: installationID, key: planID, operator: actor, mode: revisionStart, rollbackExecution: record.Rollback != nil, renders: map[string]argocd.RenderedRevision{}, sources: map[string]verifiedSourceCapture{}}
	s := &completionScope{revision: revision, record: record, snapshot: record.Plan.CompletionWorkflow}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	_, err = workflowhost.RunSnapshot(ctx, s.snapshot, completionContract, nil, workflowhost.Options{Operations: s.operations()})
	if err != nil {
		return record, err
	}
	return journal.get(ctx, installationID, planID)
}
func (s *completionScope) read(ctx context.Context, args map[string]any) (any, error) {
	if err := noRevisionArguments(args); err != nil {
		return nil, err
	}
	r, err := s.revision.journal.get(ctx, s.record.Plan.InstallationID, s.record.ID)
	if err != nil {
		return nil, err
	}
	if r.Rollback != nil {
		rollback := &rollbackWorkflowScope{revision: s.revision, snapshot: r.Plan.RollbackWorkflow}
		if _, err = rollback.observe(ctx, nil); err != nil {
			return nil, err
		}
		r, err = s.revision.journal.get(ctx, r.Plan.InstallationID, r.ID)
		if err != nil {
			return nil, err
		}
		s.record = r
		s.observationVersion = r.Rollback.ObservationVersion
		s.converged = r.Rollback.Started && ((r.Rollback.Observation != nil && completedController(*r.Rollback.Observation)) || r.Rollback.noStartedApplications())
		return map[string]any{"converged": s.converged}, nil
	}
	if r.Plan.FormatVersion == 2 {
		for _, app := range r.Plan.Applications {
			prior := r.Applications[app.Key]
			if !prior.Started {
				return nil, errors.New("completion requires every application start")
			}
			facts, err := s.revision.client.Observe(ctx, app.Intent)
			if err != nil {
				return nil, err
			}
			r, err = s.revision.journal.observeApplication(ctx, r.Plan.InstallationID, r.ID, app.Key, prior.Version, facts)
			if err != nil {
				return nil, err
			}
		}
		s.record = r
		s.observationVersion = r.ObservationVersion
		s.converged = r.Observation != nil && completedController(*r.Observation)
		return map[string]any{"converged": s.converged}, nil
	}
	facts, err := s.revision.client.Observe(ctx, r.Plan.Intent)
	if err != nil {
		return nil, err
	}
	r, err = s.revision.journal.observe(ctx, r.Plan.InstallationID, r.ID, r.ObservationVersion, facts)
	if err != nil {
		return nil, err
	}
	s.record = r
	s.observationVersion = r.ObservationVersion
	s.converged = completedController(facts)
	return map[string]any{"converged": s.converged}, nil
}
func completedController(f argocd.Facts) bool {
	return f.IntentObserved && !f.OperationPending && f.OperationSucceeded && f.RevisionObserved && f.Healthy && f.Synced
}
func (s *completionScope) configuration(ctx context.Context, args map[string]any) (any, error) {
	if err := noRevisionArguments(args); err != nil {
		return nil, err
	}
	if !s.converged {
		return nil, errors.New("completion requires the exact converged controller intent")
	}
	h := s.revision.host
	if s.record.Plan.FormatVersion == 2 || s.record.Rollback != nil {
		snapshot, err := readSelectedReceiver(ctx, h, s.record)
		if err != nil {
			return nil, err
		}
		s.revision.configuration = snapshot
		return nil, nil
	}
	evidence, err := readReceiverContinuation(ctx, h.api, h.namespace, h.configurationName, h.catalog, s.record.Plan, s.record.Plan.Intent)
	if err != nil {
		return nil, err
	}
	s.revision.configuration = evidence.snapshot
	if h.rendererAddressOverride != "" {
		s.revision.configuration.rendererAddress = h.rendererAddressOverride
	}
	return nil, nil
}
func (s *completionScope) prepareWorkloads(ctx context.Context, args map[string]any) (any, error) {
	if err := noRevisionArguments(args); err != nil {
		return nil, err
	}
	r := s.revision
	if r.artifacts == nil || r.resources.digest != s.record.Plan.ResourceDiffDigest {
		return nil, errors.New("completion requires complete fresh artifacts and resources")
	}
	admission, err := newWorkloadAdmission(ctx, r.host.api, r.inventory(s.targetRole()), *r.artifacts)
	if err != nil {
		return nil, err
	}
	s.workloads = &workloadWorkflowScope{admission: admission, operator: r.operator, observations: map[string]workloadObservation{}}
	return nil, nil
}
func (s *completionScope) continuity(ctx context.Context, args map[string]any) (any, error) {
	if err := noRevisionArguments(args); err != nil {
		return nil, err
	}
	r := s.revision
	if r.configuration == nil || s.workloads == nil || s.workloads.result == nil {
		return nil, errors.New("serving continuity requires verified runtime workload images")
	}
	publication := r.candidate
	if s.record.Rollback != nil {
		publication = r.rollback
	}
	current, err := r.configuration.healthCheck(ctx, r.host.health, publication)
	if err != nil {
		return nil, err
	}
	before := s.record.Plan.HealthBaseline
	if before == nil || current.UserID != before.UserID || current.IdentityID != before.IdentityID || (s.record.Rollback != nil && current.MigrationsDigest != before.MigrationsDigest) {
		return nil, errors.New("original authenticated session did not survive the installation")
	}
	s.health = &current
	return nil, nil
}
func (s *completionScope) reobserve(ctx context.Context, args map[string]any) (any, error) {
	if err := noRevisionArguments(args); err != nil {
		return nil, err
	}
	if s.health == nil || s.workloads == nil || s.workloads.result == nil {
		return nil, errors.New("completion requires session, migration and workload evidence")
	}
	r := s.revision
	h := r.host
	if s.record.Plan.FormatVersion == 2 || s.record.Rollback != nil {
		snapshot, err := readSelectedReceiver(ctx, h, s.record)
		if err != nil {
			return nil, err
		}
		if snapshot.digest != r.configuration.digest {
			return nil, errors.New("group configuration changed during completion")
		}
		for _, app := range s.record.Plan.applicationMembers() {
			intent := app.Intent
			if s.record.Rollback != nil {
				reverse, ok := s.record.Rollback.application(app.Key)
				if !ok {
					continue
				}
				intent = reverse.Intent
			}
			facts, err := r.client.Observe(ctx, intent)
			if err != nil || !completedController(facts) {
				return nil, errors.New("application changed during group verification")
			}
		}
		r.configuration = snapshot
		s.reobserved = true
		return nil, nil
	}
	fresh, err := readReceiverContinuation(ctx, h.api, h.namespace, h.configurationName, h.catalog, s.record.Plan, s.record.Plan.Intent)
	if err != nil {
		return nil, err
	}
	if fresh.snapshot.digest != r.configuration.digest {
		return nil, errors.New("receiving configuration changed during completion")
	}
	facts, err := r.client.Observe(ctx, s.record.Plan.Intent)
	if err != nil || !completedController(facts) {
		return nil, errors.New("controller changed during installation verification")
	}
	r.configuration = fresh.snapshot
	s.reobserved = true
	return nil, nil
}
func (s *completionScope) complete(ctx context.Context, args map[string]any) (any, error) {
	if err := noRevisionArguments(args); err != nil {
		return nil, err
	}
	proof, err := s.evidence()
	if err != nil {
		return nil, err
	}
	_, err = s.revision.journal.complete(ctx, s.record.Plan.InstallationID, s.record.ID, s.observationVersion, proof)
	return nil, err
}
func (s *completionScope) evidence() (completionEvidence, error) {
	r := s.revision
	now := time.Now()
	if !s.reobserved || s.health == nil || s.health.validate() != nil || r.configuration == nil || !freshConfigurationObservation(r.configuration.observed, now) || r.artifacts == nil || !freshArtifactObservation(r.artifacts.observed, r.artifacts.expires, now) || s.workloads == nil || s.workloads.result == nil || !freshPreservationObservation(s.workloads.result.observed, now) || !s.workloads.result.expires.After(now) || !freshPreservationObservation(r.storage.observed, now) || !freshPreservationObservation(r.sensitive.observed, now) || !freshPreservationObservation(r.resources.observed, now) {
		return completionEvidence{}, errors.New("installation completion evidence is incomplete or stale")
	}
	workload := *s.workloads.result
	workload.workflow = artifactHash("completion-workflow", s.snapshot.Version)
	workload.operator = r.operator
	workload.digest = artifactHash("workflow-evidence", []string{workload.digest, workload.workflow, r.operator})
	if err := workload.require(r.inventory(s.targetRole()), *r.artifacts, workload.workflow, r.operator); err != nil {
		return completionEvidence{}, err
	}
	if r.storage.digest != s.record.Plan.Preparation.StorageDigest || r.sensitive.digest != s.record.Plan.Preparation.SensitiveDigest || r.resources.digest != s.record.Plan.ResourceDiffDigest {
		return completionEvidence{}, errors.New("installation preservation proofs differ from selected plan")
	}
	expires := now.Add(time.Minute)
	for _, v := range []time.Time{r.configuration.observed.Add(time.Minute), r.resources.observed.Add(time.Minute), r.storage.observed.Add(time.Minute), r.sensitive.observed.Add(time.Minute), workload.observed.Add(time.Minute), workload.expires, r.artifacts.expires} {
		if v.Before(expires) {
			expires = v
		}
	}
	return completionEvidence{plan: s.record.ID, operator: r.operator, expires: expires, receipt: completionReceipt{Direction: s.direction(), PlanID: s.record.ID, Workflow: s.snapshot.Version, Artifacts: r.artifacts.digest, Workloads: workload.digest, Storage: r.storage.digest, Protected: r.sensitive.digest, Health: *s.health}}, nil
}

func (s *completionScope) targetRole() string {
	if s.record.Rollback != nil {
		return "rollback"
	}
	return "candidate"
}
func (s *completionScope) direction() string {
	if s.record.Rollback != nil {
		return "rollback"
	}
	return ""
}
