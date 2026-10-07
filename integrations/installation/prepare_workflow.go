package installation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/argocd"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

const installationPrepareWorkflow = "installationPrepareWorkflow"

// A request chooses identifiers, never endpoints, trust, rendered resources,
// credentials or evidence digests. OverlayRevision is a selection to verify,
// not proof: every image and resource in that commit must pass native checks.
type preparationRequest struct {
	InstallationID, RequestID, CandidateID, CatalogDigest, OverlayRevision string
	Prune                                                                  bool
}
type preparationHost struct {
	api                          argocd.API
	journal                      *preparationJournal
	executor                     sourceCaptureExecutor
	files                        sourceCaptureFiles
	tokens                       pipelinesteps.TokenMinter
	catalog                      CatalogFactory
	namespace, configurationName string
	definition                   *automations.Automation
	revisions                    *revisionWorkflow
	artifacts                    *artifactWorkflow
	digest                       string
}

func (h *preparationHost) bindWorkflow(engineRevision string) error {
	if h == nil || !commitDigest.MatchString(engineRevision) {
		return errors.New("installation preparation requires its installed engine revision")
	}
	definition, err := workflowhost.Load(installationPrepareWorkflow)
	if err != nil {
		return err
	}
	if definition == nil || !definition.Trusted {
		return errors.New("installation preparation requires its installed workflow")
	}
	h.definition, err = automations.NewLoader(automations.LoaderOptions{}).Snapshot(definition)
	if err != nil {
		return err
	}
	h.artifacts, err = loadArtifactWorkflow(engineRevision)
	if err != nil {
		return err
	}
	h.revisions, err = loadRevisionWorkflow(engineRevision)
	if err != nil {
		return err
	}
	h.digest = artifactHash("preparation-workflow-v1", []string{engineRevision, h.definition.DefinitionFingerprint(id.NewUntracked()), h.artifacts.digest, h.revisions.digest})
	return nil
}

func (h *preparationHost) prepare(ctx context.Context, request preparationRequest) (revisionRecord, error) {
	actor, err := preparationActor(ctx)
	if err != nil {
		return revisionRecord{}, err
	}
	if h == nil || h.api == nil || h.journal == nil || h.executor == nil || h.files == nil || h.tokens == nil || h.catalog == nil || h.definition == nil || h.artifacts == nil || h.revisions == nil || !internalDigest.MatchString(h.digest) {
		return revisionRecord{}, errors.New("installation preparation host is incomplete")
	}
	if !identifier.MatchString(request.InstallationID) || !identifier.MatchString(request.RequestID) || !artifactDigest.MatchString(request.CandidateID) || !artifactDigest.MatchString(request.CatalogDigest) || !commitDigest.MatchString(request.OverlayRevision) {
		return revisionRecord{}, errors.New("installation preparation selection is invalid")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Hour)
	defer cancel()
	prior, err := h.journal.getByRequest(ctx, request.InstallationID, request.RequestID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return revisionRecord{}, err
	}
	if err == nil {
		if prior.Scope.RequestedBy != actor || prior.Scope.WorkflowDigest != h.digest || prior.Scope.CandidateID != request.CandidateID || prior.Scope.PublicationDigest != request.CatalogDigest || prior.Scope.Intent.Revision != request.OverlayRevision || prior.Scope.Intent.Prune != request.Prune {
			return revisionRecord{}, errors.New("installation preparation request was reused with changed inputs or authority")
		}
		if prior.State == "promoted" {
			// Historical recovery is a read, never fresh readiness or permission to
			// apply. The revision start gate must requalify a now-expired preparation.
			return (&revisionJournal{db: h.journal.db}).get(ctx, request.InstallationID, prior.PromotedPlanID)
		}
		if prior.State != "preparing" {
			return revisionRecord{}, errChanged
		}
	}
	cfg, err := readReceiver(ctx, h.api, h.namespace, h.configurationName, h.catalog)
	if err != nil {
		return revisionRecord{}, err
	}
	if cfg.configuration.InstallationID != request.InstallationID || (prior.ID != "" && (prior.Scope.ConfigurationDigest != cfg.digest || prior.Scope.ConfigurationInvariantDigest != cfg.invariantDigest)) {
		return revisionRecord{}, errors.New("installation receiving configuration changed")
	}
	s := &preparationWorkflowScope{host: h, request: request, operator: actor, configuration: cfg, record: prior, sources: map[string]verifiedSourceCapture{}, renders: map[string]argocd.RenderedRevision{}}
	return s.run(ctx)
}

func (s *preparationWorkflowScope) run(ctx context.Context) (revisionRecord, error) {
	h := s.host
	_, err := workflowhost.Run(ctx, h.definition.Name, nil, workflowhost.Options{Operations: s.operations(), Load: func(name string) (*automations.Automation, error) {
		switch name {
		case h.definition.Name:
			return h.definition, nil
		case h.artifacts.definition.Name:
			return h.artifacts.definition, nil
		}
		return nil, errors.New("installation preparation cannot load an unbound workflow")
	}, LoadLogic: func(string) (*memql.Function, error) {
		return nil, errors.New("installation preparation cannot load unbound logic")
	}})
	if err != nil {
		return revisionRecord{}, err
	}
	if ctx.Err() != nil {
		return revisionRecord{}, ctx.Err()
	}
	if s.result == nil {
		return revisionRecord{}, errors.New("installation recipe did not complete native preparation")
	}
	return *s.result, nil
}

type preparationWorkflowScope struct {
	// Native preparation proofs mutate together. Artifact checks share a read
	// lock and retain their own bounded concurrency inside the child scope.
	mu                  sync.RWMutex
	host                *preparationHost
	request             preparationRequest
	operator            string
	configuration       *receiverSnapshot
	record              preparationRecord
	candidate, rollback pipelines.VerifiedPublishedRelease
	sources             map[string]verifiedSourceCapture
	renders             map[string]argocd.RenderedRevision
	resources           resourceEvidence
	storage             storageEvidence
	sensitive           sensitiveEvidence
	artifacts           *artifactWorkflowScope
	reobserved          bool
	result              *revisionRecord
}

func (s *preparationWorkflowScope) operations() map[string]workflowhost.Operation {
	operations := map[string]workflowhost.Operation{
		"installationResolvePublications":    s.resolve,
		"installationCheckCompatibility":     s.compatibility,
		"installationReservePreparation":     s.reserve,
		"installationCaptureSource":          s.capture,
		"installationVerifySource":           s.verify,
		"installationAcknowledgeSource":      s.acknowledge,
		"installationRenderSource":           s.render,
		"installationVerifyResources":        s.verifyResources,
		"installationVerifyStorage":          s.verifyStorage,
		"installationVerifyProtected":        s.verifyProtected,
		"installationReobserveConfiguration": s.reobserve,
		"installationPromotePreparation":     s.promote,
	}
	for _, name := range []string{"installationArtifactRequirements", "installationArtifactCheck", "installationArtifactsComplete"} {
		operations[name] = func(ctx context.Context, args map[string]any) (any, error) {
			if s.artifacts == nil {
				return nil, errors.New("installation artifact scope is not prepared")
			}
			return s.artifacts.operations()[name](ctx, args)
		}
	}
	for name, operation := range operations {
		artifactRead := name == "installationArtifactRequirements" || name == "installationArtifactCheck" || name == "installationArtifactsComplete"
		operations[name] = func(ctx context.Context, args map[string]any) (any, error) {
			if artifactRead {
				s.mu.RLock()
				defer s.mu.RUnlock()
			} else {
				s.mu.Lock()
				defer s.mu.Unlock()
			}
			actor, err := preparationActor(ctx)
			if err != nil || actor != s.operator || s.result != nil {
				return nil, errors.New("installation operation is outside its admitted scope")
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return operation(ctx, args)
		}
	}
	return operations
}
func noPreparationArguments(args map[string]any) error {
	if len(args) != 0 {
		return errors.New("installation operation accepts no caller evidence")
	}
	return nil
}
func preparationRole(args map[string]any) (string, error) {
	role, ok := args["role"].(string)
	if !ok || len(args) != 1 || (role != "candidate" && role != "rollback") {
		return "", errors.New("installation operation requires one scoped source role")
	}
	return role, nil
}
func (s *preparationWorkflowScope) resolve(ctx context.Context, args map[string]any) (any, error) {
	if err := noPreparationArguments(args); err != nil {
		return nil, err
	}
	cfg := s.configuration.configuration
	next, err := s.configuration.catalog.Get(ctx, cfg.Catalog.ID, s.request.CandidateID, s.request.CatalogDigest)
	if err != nil {
		return nil, err
	}
	before, err := s.configuration.catalog.Get(ctx, cfg.Catalog.ID, cfg.Rollback.CandidateID, cfg.Rollback.CatalogDigest)
	if err != nil {
		return nil, err
	}
	s.candidate, s.rollback = next, before
	return nil, nil
}
func (s *preparationWorkflowScope) compatibility(_ context.Context, args map[string]any) (any, error) {
	if err := noPreparationArguments(args); err != nil {
		return nil, err
	}
	return nil, pipelines.CheckPublishedReleaseOverlap(s.rollback, s.candidate, s.configuration.dependencies)
}
func (s *preparationWorkflowScope) reserve(ctx context.Context, args map[string]any) (any, error) {
	if err := noPreparationArguments(args); err != nil {
		return nil, err
	}
	if _, err := s.compatibility(ctx, nil); err != nil {
		return nil, err
	}
	cfg := s.configuration.configuration
	started := time.Now().UTC().Format(time.RFC3339Nano)
	if s.record.ID != "" {
		started = s.record.Scope.Captures["candidate"].RunStartedAt
	}
	intent, err := argocd.PlanRevision(s.configuration.application, s.request.RequestID, s.request.OverlayRevision, s.request.Prune)
	if err != nil {
		return nil, err
	}
	scope := preparationScope{FormatVersion: 1, InstallationID: cfg.InstallationID, RequestID: s.request.RequestID, RequestedBy: s.operator, WorkflowDigest: s.host.digest, ExecutionWorkflowDigest: s.host.revisions.digest, ConfigurationDigest: s.configuration.digest, ConfigurationInvariantDigest: s.configuration.invariantDigest, CandidateID: s.request.CandidateID, PublicationDigest: s.request.CatalogDigest, Intent: intent, Captures: map[string]sourceCaptureSpec{}}
	run := preparationSourceRun(cfg.InstallationID, s.request.RequestID, s.operator)
	for _, role := range []string{"candidate", "rollback"} {
		render := s.configuration.render
		var source map[string]any
		if json.Unmarshal(render.Source, &source) != nil {
			return nil, errors.New("installation source is malformed")
		}
		if role == "candidate" {
			source["targetRevision"] = s.request.OverlayRevision
		}
		render.Source, _ = json.Marshal(source)
		repository := resourceText(source, "repoURL")
		parts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(repository, "https://github.com/"), ".git"), "/")
		if len(parts) != 2 {
			return nil, errors.New("installation capture requires a canonical GitHub repository")
		}
		spec := sourceCaptureSpec{OwnerUserID: s.operator, RunID: run, WorkRunID: run, StepKey: "source-" + role, Attempt: 1, RunStartedAt: started, Repository: pipelines.Repository{Owner: parts[0], Name: parts[1], CloneURL: repository}, CloneInstallationID: cfg.Collector.CloneInstallationID, CollectorImage: cfg.Collector.Image, Platform: cfg.Platform, Render: render}
		if cfg.Collector.PullCredential != nil {
			spec.ImagePullSecret = "INSTALLATION_PULL_CREDENTIAL"
		}
		scope.Captures[role] = spec
	}
	record, err := s.host.journal.reserve(ctx, scope)
	if err == nil {
		s.record = record
	}
	return nil, err
}
func (s *preparationWorkflowScope) capture(ctx context.Context, args map[string]any) (any, error) {
	role, err := preparationRole(args)
	if err != nil {
		return nil, err
	}
	record, err := s.host.journal.capture(ctx, s.request.InstallationID, s.record.ID, s.host.digest, role, s.host.executor, s.configuration.pullCredential)
	if err == nil {
		s.record = record
	}
	return nil, err
}
func (s *preparationWorkflowScope) verify(ctx context.Context, args map[string]any) (any, error) {
	role, err := preparationRole(args)
	if err != nil {
		return nil, err
	}
	delete(s.sources, role)
	proof, err := s.host.journal.verifyCapture(ctx, s.request.InstallationID, s.record.ID, s.host.digest, role, s.host.files)
	if err != nil {
		return nil, err
	}
	s.sources[role] = proof
	return nil, nil
}
func (s *preparationWorkflowScope) acknowledge(ctx context.Context, args map[string]any) (any, error) {
	role, err := preparationRole(args)
	if err != nil {
		return nil, err
	}
	record, err := s.host.journal.acknowledgeCapture(ctx, s.request.InstallationID, s.record.ID, s.host.digest, role, s.host.executor)
	if err == nil {
		s.record = record
	}
	return nil, err
}
func (s *preparationWorkflowScope) render(ctx context.Context, args map[string]any) (any, error) {
	role, err := preparationRole(args)
	if err != nil {
		return nil, err
	}
	delete(s.renders, role)
	source, ok := s.sources[role]
	if !ok || source.source.Digest() == "" {
		return nil, errors.New("installation render requires fresh private source verification")
	}
	spec := s.record.Scope.Captures[role]
	token, err := s.host.tokens.CloneToken(ctx, spec.CloneInstallationID, spec.Repository.Owner, spec.Repository.Name)
	if err != nil {
		return nil, errors.New("installation render repository credential is unavailable")
	}
	credentials := argocd.RepositoryCredentials{}
	if spec.CloneInstallationID != 0 {
		if token == "" {
			return nil, errors.New("installation render repository credential is missing")
		}
		credentials = argocd.RepositoryCredentials{Username: "x-access-token", Password: token}
	}
	result, err := s.configuration.renderRevision(ctx, source.source.Spec(), credentials)
	if err != nil {
		return nil, err
	}
	s.renders[role] = result
	return nil, nil
}
func (s *preparationWorkflowScope) verifyResources(ctx context.Context, args map[string]any) (any, error) {
	if err := noPreparationArguments(args); err != nil {
		return nil, err
	}
	s.resources, s.artifacts = resourceEvidence{}, nil
	cfg := s.configuration
	evidence, err := verifyImagesAndDiff(ctx, s.host.api, s.candidate, s.renders["rollback"], s.renders["candidate"], cfg.configuration.Platform, cfg.bindings)
	if err != nil {
		return nil, err
	}
	scope, err := newArtifactAdmission(ctx, s.host.api, artifactAdmissionConfig{ConfigurationDigest: cfg.digest, Platform: cfg.configuration.Platform, Bindings: cfg.bindings, Dependencies: cfg.dependencies, Registries: cfg.registries}, s.rollback, s.candidate, s.renders["rollback"], s.renders["candidate"])
	if err != nil {
		return nil, err
	}
	s.resources = evidence
	s.artifacts = &artifactWorkflowScope{admission: scope, operator: s.operator, observations: map[string]artifactObservation{}}
	return nil, nil
}
func (s *preparationWorkflowScope) verifyStorage(ctx context.Context, args map[string]any) (any, error) {
	if err := noPreparationArguments(args); err != nil {
		return nil, err
	}
	var err error
	s.storage, err = verifyStoragePreservation(ctx, s.host.api, s.renders["rollback"], s.renders["candidate"])
	return nil, err
}
func (s *preparationWorkflowScope) verifyProtected(ctx context.Context, args map[string]any) (any, error) {
	if err := noPreparationArguments(args); err != nil {
		return nil, err
	}
	var err error
	s.sensitive, err = verifySensitivePreservation(ctx, s.host.api, s.renders["rollback"], s.renders["candidate"], s.configuration.protected)
	return nil, err
}
func (s *preparationWorkflowScope) reobserve(ctx context.Context, args map[string]any) (any, error) {
	if err := noPreparationArguments(args); err != nil {
		return nil, err
	}
	s.reobserved = false
	fresh, err := readReceiver(ctx, s.host.api, s.host.namespace, s.host.configurationName, s.host.catalog)
	if err != nil {
		return nil, err
	}
	if fresh.digest != s.configuration.digest {
		return nil, errors.New("installation configuration changed during preparation")
	}
	s.reobserved = true
	s.configuration.observed = fresh.observed
	return nil, nil
}
func (s *preparationWorkflowScope) promote(ctx context.Context, args map[string]any) (any, error) {
	if err := noPreparationArguments(args); err != nil {
		return nil, err
	}
	if !s.reobserved || time.Since(s.configuration.observed) > time.Minute || s.artifacts == nil || s.artifacts.result == nil || !s.artifacts.result.expires.After(time.Now()) {
		return nil, errors.New("installation promotion requires fresh configuration and complete artifact evidence")
	}
	artifact := *s.artifacts.result
	if artifact.configuration != s.configuration.digest || artifact.candidate != s.candidate.Digest() || artifact.rollback != s.rollback.Digest() || artifact.resources != s.resources.digest || artifact.before != s.renders["rollback"].Digest() || artifact.after != s.renders["candidate"].Digest() {
		return nil, errors.New("installation artifact evidence belongs to another preparation")
	}
	release, err := s.candidate.Release()
	if err != nil {
		return nil, err
	}
	var before struct{ TargetRevision string }
	_ = json.Unmarshal(s.renders["rollback"].Spec().Source, &before)
	plan := preparedPlan{FormatVersion: 1, InstallationID: s.request.InstallationID, RequestedBy: s.operator, WorkflowDigest: s.host.digest, ExecutionWorkflowDigest: s.host.revisions.digest, CandidateID: s.request.CandidateID, CandidateApprovalID: release.ApprovalID, PublicationDigest: s.request.CatalogDigest, RenderDigest: s.renders["candidate"].Digest(), RollbackRenderDigest: s.renders["rollback"].Digest(), ResourceDiffDigest: s.resources.digest, RollbackRevision: before.TargetRevision, Intent: s.record.Scope.Intent}
	artifact.workflow, artifact.operator = s.host.artifacts.digest, s.operator
	artifact.digest = artifactHash("workflow-evidence", []string{artifact.digest, artifact.workflow, artifact.operator})
	evidence := promotionEvidence{configuration: s.configuration, artifacts: artifact, published: s.candidate, candidateSource: s.sources["candidate"], rollbackSource: s.sources["rollback"], candidateRender: s.renders["candidate"], rollbackRender: s.renders["rollback"], resources: s.resources, storage: s.storage, sensitive: s.sensitive}
	result, err := s.host.journal.promote(ctx, s.request.InstallationID, s.record.ID, s.host.digest, s.configuration.digest, plan, evidence)
	if err != nil {
		return nil, err
	}
	s.result = &result
	return nil, nil
}
