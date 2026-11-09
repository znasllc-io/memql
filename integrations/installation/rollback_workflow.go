package installation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	// qualifying is set while Restore previous release admits a COMPLETED
	// update: every operation runs against the completed attempt without its
	// installation head, and the head is taken back only together with the
	// reversal, in installationRollbackReserve (reserveCompletedRollback). A
	// check that refuses before then leaves the attempt complete and the head
	// free, as before the request. It is cleared once the reversal is stored.
	qualifying bool
}

// RollbackSelected reverses only this original operator's started selection.
// The saved recipe derives each reversal from its owned terminal controller
// operation. It cannot downgrade a migrated database or replace a pending sync.
//
// Restoring a completed update first qualifies it without the installation
// head (rollbackWorkflowScope.qualifying). Any refusal from then until a
// reverse write has a durable start returns the attempt to complete, releasing
// the head (withdrawRefusedRestoration); before this, a refused check could
// leave the installation held by a restoration nothing could end.
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
	if record.State == "closed" {
		return SelectionResult{}, errors.New("a closed installation plan cannot be restored; its close ended the selection without completion proof")
	}
	qualifying := record.State == "complete"
	if qualifying && !record.RollbackEligible {
		return selectionResult(ctx, record), errRestoreIneligible
	}
	if (!qualifying && !record.rollbackActive()) || record.Plan.Preparation == nil || record.Plan.HealthBaseline == nil || record.Plan.RollbackWorkflow == nil {
		return SelectionResult{}, errors.New("rollback requires the exact started selection and its retained recipe")
	}
	client, err := argocd.New(p.host.api)
	if err != nil {
		return SelectionResult{}, err
	}
	revision := &revisionWorkflowScope{workflow: &revisionWorkflow{digest: record.Plan.ExecutionWorkflowDigest, artifacts: &artifactWorkflow{digest: record.Plan.Preparation.ArtifactWorkflowDigest}}, host: p.host, journal: journal, client: client, installation: installationID, key: planID, operator: actor, mode: revisionStart, rollbackExecution: true, renders: map[string]argocd.RenderedRevision{}, sources: map[string]verifiedSourceCapture{}}
	s := &rollbackWorkflowScope{revision: revision, snapshot: record.Plan.RollbackWorkflow, qualifying: qualifying}
	runCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	_, err = workflowhost.RunSnapshot(runCtx, s.snapshot, rollbackContract, nil, workflowhost.Options{Operations: s.operations()})
	if err != nil {
		return s.refused(ctx, record, err)
	}
	if !s.observed {
		return SelectionResult{}, errors.New("rollback recipe returned no native observation")
	}
	record, err = journal.get(ctx, installationID, planID)
	return selectionResult(ctx, record), err
}

// refused answers a restoration the recipe stopped. When nothing of it reached
// the cluster it is withdrawn: the attempt is complete again and the
// installation free, so the refusal ends the request rather than stranding it.
// A restoration with a recorded reverse start stays as it is for its operator
// to resume. The withdrawal gets its own short context, because a refusal can
// be the run's own deadline.
//
// A withdrawal that fails (its effect lock or the database unavailable) is
// logged and leaves the attempt as it is, reverting if this request reserved
// it; the restoration's next refusal, or its operator's close, withdraws it.
// The result is read again so it reports that state rather than the one
// before the request.
//
// The withdrawal cannot tell which request reserved the reversal: a refusal
// here also withdraws another request's reservation that has not started yet,
// whose next step then refuses because the attempt is complete again.
func (s *rollbackWorkflowScope) refused(ctx context.Context, before revisionRecord, cause error) (SelectionResult, error) {
	withdrawCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	withdrawn, err := s.revision.journal.withdrawRefusedRestoration(withdrawCtx, s.revision.installation, s.revision.key)
	if err != nil {
		slog.WarnContext(ctx, "installation restoration refused and could not be withdrawn", "installation", s.revision.installation, "plan", s.revision.key, "refusal", cause.Error(), "error", err.Error())
	}
	if err != nil || withdrawn == nil {
		if current, readErr := s.revision.journal.get(withdrawCtx, s.revision.installation, s.revision.key); readErr == nil {
			before = current
		}
		return selectionResult(ctx, before), cause
	}
	return selectionResult(ctx, *withdrawn), fmt.Errorf("%w; the restoration changed no application, so the update stays complete and can be restored again", cause)
}

func (s *rollbackWorkflowScope) operations() map[string]workflowhost.Operation {
	ops := s.revision.operations()
	for _, name := range []string{"installationRevisionRead", "installationRevisionConfiguration", "installationRevisionReobserveConfig", "installationRevisionBegin", "installationRevisionApply", "installationRevisionObserve"} {
		delete(ops, name)
	}
	ops["installationRollbackRead"] = s.read
	ops["installationRollbackConfiguration"] = s.configuration
	ops["installationRollbackReopenSource"] = s.reopen
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

// record reads the attempt for one operation. While qualifying it also admits
// the completed attempt itself, and only while it stays eligible for
// restoration; otherwise the attempt must be applying or reverting.
func (s *rollbackWorkflowScope) record(ctx context.Context) (revisionRecord, error) {
	r, err := s.revision.writable(ctx)
	if err != nil {
		return r, err
	}
	if r.Plan.RollbackWorkflow == nil || s.snapshot == nil || r.Plan.RollbackWorkflow.Version != s.snapshot.Version {
		return r, errors.New("rollback selected recipe changed")
	}
	if s.qualifying && r.State == "complete" {
		if !r.RollbackEligible {
			return r, errRestoreIneligible
		}
		return r, nil
	}
	if !r.rollbackActive() {
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
		evidence := recordedSuccessContext(ctx, r)
		for _, app := range r.Plan.applicationMembers() {
			reverse, ok := r.Rollback.application(app.Key)
			if !ok {
				continue
			}
			facts, e := s.revision.client.Observe(evidence, reverse.Intent)
			if e == nil {
				if !reverse.Started || !facts.IntentObserved {
					return nil, errors.New("rollback effect preceded its durable start")
				}
				// Every read that sees the reversal's own success keeps it, so a
				// later sync before Check progress cannot strand the restoration.
				if r, e = s.recordSuccess(ctx, r, app.Key, reverse.Intent, facts); e != nil {
					return nil, e
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
	cfg, err := readSelectedReceiver(recordedSuccessContext(ctx, r), s.revision.host, r)
	if err != nil {
		return nil, err
	}
	s.revision.configuration = cfg
	return nil, nil
}

// reopen is the retained-source operation. Under restore admission of a
// completed update the head is still free, so the source is reopened through
// reopenCompleted; once the attempt is active it is the revision scope's
// reopen under the head, as before.
func (s *rollbackWorkflowScope) reopen(ctx context.Context, args map[string]any) (any, error) {
	if !s.qualifying {
		return s.revision.reopen(ctx, args)
	}
	r, err := s.record(ctx)
	if err != nil {
		return nil, err
	}
	if r.State != "complete" {
		// An identical request on another replica already took the
		// installation back; its head fences this read as it fences theirs.
		return s.revision.reopen(ctx, args)
	}
	return s.reopenCompleted(ctx, r, args)
}
func (s *rollbackWorkflowScope) reobserve(ctx context.Context, args map[string]any) (any, error) {
	if err := noRevisionArguments(args); err != nil {
		return nil, err
	}
	r, err := s.record(ctx)
	if err != nil {
		return nil, err
	}
	fresh, err := readSelectedReceiver(recordedSuccessContext(ctx, r), s.revision.host, r)
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
	ctx = recordedSuccessContext(ctx, r)
	if _, err = s.revision.freshWriteEvidence(r); err != nil {
		return nil, err
	}
	if err = s.continuityGuard(ctx, r); err != nil {
		return nil, err
	}
	// A started reversal is kept as recorded. One reserved but never started
	// is planned again below and replaced (replaceUnstartedRollback): nothing
	// of it was written, and its saved boundary may be one Kubernetes no longer
	// retains, or a strict fence Argo's status writes have since passed.
	if r.Rollback != nil && r.Rollback.Started {
		s.qualifying = false
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
		// Read through the name-filtered list so a baseline fence is planned
		// against a history boundary too, and its own patch tolerates Argo's
		// status-only generation advances (argocd.BoundaryProof).
		baseline, baselineErr := s.revision.client.ReadPlanning(ctx, app.Intent.Target)
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
	if r.State == "complete" {
		// Restore admission ends here: every check above passed without the
		// installation head, which is taken back with the reversal at once.
		_, err = s.revision.journal.reserveCompletedRollback(ctx, r.Plan.InstallationID, r.ID, p)
	} else {
		_, err = s.revision.journal.replaceUnstartedRollback(ctx, r.Plan.InstallationID, r.ID, p)
	}
	if err != nil {
		return nil, err
	}
	s.qualifying = false
	return nil, nil
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
	ctx = recordedSuccessContext(ctx, r)
	forward, err := r.Plan.application(application)
	if err != nil {
		return nil, err
	}
	if r.Rollback == nil || !r.Rollback.Started {
		return nil, errors.New("rollback needs its durable start")
	}
	reverse, ok := r.Rollback.application(application)
	if !ok {
		return nil, nil
	}
	if facts, err := s.revision.client.Observe(ctx, reverse.Intent); err == nil {
		if !reverse.Started {
			return nil, errors.New("rollback effect preceded its start")
		}
		// Keep an applied reversal's own success whenever a read sees it.
		_, err = s.recordSuccess(ctx, r, application, reverse.Intent, facts)
		return nil, err
	} else if !errors.Is(err, argocd.ErrChanged) {
		return nil, err
	}
	if reverse.BaselineFence {
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
	// Admit the reversal BEFORE the member's reverse start: a history-backed
	// one against its complete history (or, with that history gone, its
	// unchanged planning generation), a format 1 one (planned by engines up to
	// 0.31.28) against its strict fence. A refusal here leaves nothing of the
	// member written, so a restoration with no other start recorded is still
	// withdrawn (restorationUnwritten). A single-Application reversal whose
	// format 1 intent such an engine may have written is the exception: its
	// reversal start already counts as a possible write (perApplyStarts). A
	// saved reversal that can no longer be admitted but provably never reached
	// the cluster is planned again (replan) rather than strand the installation.
	if _, err = s.revision.client.Admit(ctx, reverse.Intent); err != nil {
		if r, reverse, err = s.replan(ctx, r, application, forward, reverse, err); err != nil {
			return nil, err
		}
	}
	if r.Plan.FormatVersion == 2 || r.Rollback.perApplyStarts() {
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
	// Admitted again for the exact current object, which the patch tests. A
	// format 2 reversal needs a fresh proof that, since it was planned, Argo
	// changed only status: its status writes advance the generation of an
	// Application CRD without a status subresource, which refused the strict
	// fence on every retry. A format 1 reversal keeps its strict generation
	// fence (nil proof). Any spec, marker, operation or identity change still
	// refuses, as does missing history.
	proof, err := s.revision.client.Admit(effect, reverse.Intent)
	if err != nil {
		return nil, err
	}
	if _, err = s.revision.client.ApplyWithContinuity(effect, reverse.Intent, proof); err != nil {
		return nil, errors.New("rollback effect is unconfirmed; resume its recorded intent")
	}
	return nil, nil
}

// replan plans one member's reversal again when its saved intent can no
// longer be admitted (cause) and nothing of it can still reach the cluster,
// and returns the member with the new intent admitted. Only these causes
// qualify:
//
//   - a member with no reverse start whose history-backed intent's Kubernetes
//     history is gone and whose generation moved since planning
//     (ErrHistoryUnavailable; argocd.BoundaryProof still proves an unmoved
//     generation), the normal state of a restoration resumed after the watch
//     window, because Argo's status refresh moves the generation;
//   - a member with no reverse start whose format 1 intent fails its strict
//     fence (ErrChanged), which cannot tell Argo's status writes from an edit;
//   - a started member whose format 1 intent the Application proves
//     superseded (argocd.Superseded), as engines up to 0.31.28 left a
//     restoration whose strict fence one status write had passed.
//
// History that positively shows another writer's edit refuses as before, and
// so does every other failure. A started history-backed member is never
// planned again: the resource version its request tested is not retained, so
// nothing proves that request can no longer land (a known limit, recorded in
// installation-revision-effects.md). PlanRollback then requires the
// Application to hold the forward intent's exact marker and spec, be idle,
// and show the forward operation's terminal outcome; this pass already reran
// every configuration, migration and preservation check.
func (s *rollbackWorkflowScope) replan(ctx context.Context, r revisionRecord, application string, forward preparedApplication, reverse rollbackApplicationState, cause error) (revisionRecord, rollbackApplicationState, error) {
	eligible := false
	switch {
	case !reverse.Started && reverse.Intent.FormatVersion == 2:
		eligible = errors.Is(cause, argocd.ErrHistoryUnavailable)
	case !reverse.Started:
		eligible = errors.Is(cause, argocd.ErrChanged)
	case reverse.Intent.FormatVersion != 2 && errors.Is(cause, argocd.ErrChanged):
		superseded, err := s.revision.client.Superseded(ctx, reverse.Intent)
		if err != nil {
			return r, reverse, err
		}
		eligible = superseded
	}
	if !eligible {
		return r, reverse, cause
	}
	replacement, err := s.revision.client.PlanRollback(ctx, forward.Intent, "replanned-"+string(id.NewUntracked().FromString(r.ID+":"+application)))
	if err != nil {
		return r, reverse, err
	}
	if r, err = s.revision.journal.replanRollbackApplication(ctx, r.Plan.InstallationID, r.ID, application, replacement); err != nil {
		return r, reverse, err
	}
	reverse, _ = r.Rollback.application(application)
	if _, err = s.revision.client.Admit(ctx, reverse.Intent); err != nil {
		return r, reverse, err
	}
	return r, reverse, nil
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
	evidence := recordedSuccessContext(ctx, r)
	if _, err = readSelectedReceiver(evidence, s.revision.host, r); err != nil {
		return nil, err
	}
	if r.Plan.FormatVersion == 2 {
		for _, app := range r.Rollback.Plan.Applications {
			prior := r.Rollback.Applications[app.Key]
			if !prior.Started {
				return nil, errors.New("rollback member is not started")
			}
			effective, _ := r.Rollback.application(app.Key)
			facts, err := s.revision.client.Observe(evidence, effective.Intent)
			if err != nil {
				return nil, err
			}
			if r, err = s.recordSuccess(ctx, r, app.Key, effective.Intent, facts); err != nil {
				return nil, err
			}
			r, err = s.revision.journal.observeRollbackApplication(ctx, r.Plan.InstallationID, r.ID, app.Key, prior.Version, facts)
			if err != nil {
				return nil, err
			}
		}
	} else {
		effective, _ := r.Rollback.application("")
		// Captured before the external read: a late observer cannot overwrite
		// newer facts (observeRollback).
		version := r.Rollback.ObservationVersion
		facts, err := s.revision.client.Observe(evidence, effective.Intent)
		if err != nil {
			return nil, err
		}
		if r, err = s.recordSuccess(ctx, r, "", effective.Intent, facts); err != nil {
			return nil, err
		}
		r, err = s.revision.journal.observeRollback(ctx, r.Plan.InstallationID, r.ID, version, facts)
		if err != nil {
			return nil, err
		}
	}
	s.observed = true
	return nil, nil
}

// recordSuccess keeps the reversal's own observed success durably, so a later
// sync of the Application cannot strand its completion (completion_journal.go).
func (s *rollbackWorkflowScope) recordSuccess(ctx context.Context, r revisionRecord, application string, intent argocd.Intent, facts argocd.Facts) (revisionRecord, error) {
	if !observedSuccess(facts) {
		return r, nil
	}
	return s.revision.journal.recordOwnedSuccess(ctx, r.Plan.InstallationID, r.ID, application, intent)
}
