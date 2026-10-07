package installation

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/argocd"
)

const installationRevisionWorkflow = "installationRevisionWorkflow"

type revisionMode uint8

const (
	revisionStart revisionMode = iota + 1
	revisionObserve
)

type revisionWorkflow struct {
	definition *automations.Automation
	artifacts  *artifactWorkflow
	digest     string
}

// revisionWriteAdmission is an ephemeral, call-local proof handle. It is never
// serialized and is minted only after the sealed recipe completes every fresh
// receiver, artifact, render, resource and preservation check.
type revisionWriteAdmission struct {
	planID, workflow string
	freshUntil       time.Time
}

func loadRevisionWorkflow(engineRevision string) (*revisionWorkflow, error) {
	definition, err := workflowhost.Load(installationRevisionWorkflow)
	if err != nil {
		return nil, err
	}
	return newRevisionWorkflow(definition, engineRevision)
}

func newRevisionWorkflow(definition *automations.Automation, engineRevision string) (*revisionWorkflow, error) {
	if definition == nil || !definition.Trusted || !commitDigest.MatchString(engineRevision) {
		return nil, errors.New("installation execution requires an installed recipe and immutable engine revision")
	}
	owned, err := automations.NewLoader(automations.LoaderOptions{}).Snapshot(definition)
	if err != nil {
		return nil, err
	}
	artifacts, err := loadArtifactWorkflow(engineRevision)
	if err != nil {
		return nil, err
	}
	digest := "memql-id:" + string(id.NewUntracked().FromString("installation-execution-native-v2:"+engineRevision+":"+owned.DefinitionFingerprint(id.NewUntracked())+":"+artifacts.digest))
	return &revisionWorkflow{definition: owned, artifacts: artifacts, digest: digest}, nil
}

// run performs one bounded pass. A start pass must be bound to the native
// preparation host that owns the receiver, source files, renderer credentials,
// catalog and artifact workflow. Observation passes need only the journal and
// controller API. A replacement recipe can record facts but cannot inherit
// the original operator's permission to apply an intent.
func (w *revisionWorkflow) run(ctx context.Context, journal *revisionJournal, installation, key string, mode revisionMode, configuration *receiverSnapshot, host *preparationHost) (revisionRecord, error) {
	actor, err := preparationActor(ctx)
	if err != nil {
		return revisionRecord{}, err
	}
	if w == nil || w.definition == nil || journal == nil || (mode != revisionStart && mode != revisionObserve) {
		return revisionRecord{}, errors.New("installation execution has no complete native scope")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	r, err := journal.get(ctx, installation, key)
	if err != nil {
		return revisionRecord{}, err
	}
	if r.Plan.Preparation == nil || (r.State != "prepared" && r.State != "applying") {
		return revisionRecord{}, errors.New("installation execution requires an admitted active preparation")
	}
	if mode == revisionStart && (r.Plan.RequestedBy != actor || r.Plan.ExecutionWorkflowDigest != w.digest) {
		return revisionRecord{}, errors.New("installation execution authority or installed recipe changed")
	}
	if host == nil || host.api == nil {
		if mode == revisionStart {
			return revisionRecord{}, errors.New("installation revision write requires its complete native preparation host")
		}
		return revisionRecord{}, errors.New("installation observation requires its native receiving API")
	}
	if mode == revisionStart && (host.journal == nil || host.files == nil || host.tokens == nil || host.catalog == nil || host.artifacts == nil || host.revisions == nil || host.revisions.digest != w.digest || host.artifacts.digest != w.artifacts.digest) {
		return revisionRecord{}, errors.New("installation revision write requires its complete native preparation host")
	}
	client, err := argocd.New(host.api)
	if err != nil {
		return revisionRecord{}, err
	}
	s := &revisionWorkflowScope{workflow: w, host: host, journal: journal, client: client, installation: installation, key: key, operator: actor, mode: mode, configuration: configuration, renders: map[string]argocd.RenderedRevision{}, sources: map[string]verifiedSourceCapture{}}
	_, err = workflowhost.Run(ctx, w.definition.Name, nil, workflowhost.Options{Operations: s.operations(),
		Load: func(name string) (*automations.Automation, error) {
			switch name {
			case w.definition.Name:
				return w.definition, nil
			case w.artifacts.definition.Name:
				return w.artifacts.definition, nil
			default:
				return nil, errors.New("installation execution cannot load an unbound child")
			}
		},
		LoadLogic: func(string) (*memql.Function, error) {
			return nil, errors.New("installation execution cannot load unbound logic")
		},
	})
	if err != nil {
		return revisionRecord{}, err
	}
	if err := ctx.Err(); err != nil {
		return revisionRecord{}, err
	}
	if !s.observed.Load() {
		return revisionRecord{}, errors.New("installation execution returned no native observation")
	}
	return journal.get(ctx, installation, key)
}

// Call-local proof and observations are owned by workflowhost's serialized
// native callbacks. The DSL controls their order; no callback accepts evidence
// from its caller.
type revisionWorkflowScope struct {
	workflow          *revisionWorkflow
	host              *preparationHost
	journal           *revisionJournal
	client            *argocd.Client
	installation, key string
	operator          string
	mode              revisionMode
	configuration     *receiverSnapshot
	preparation       preparationRecord
	candidate         pipelines.VerifiedPublishedRelease
	rollback          pipelines.VerifiedPublishedRelease
	sources           map[string]verifiedSourceCapture
	renders           map[string]argocd.RenderedRevision
	resources         resourceEvidence
	admission         *artifactAdmission
	artifacts         *artifactEvidence
	storage           storageEvidence
	sensitive         sensitiveEvidence
	artifactScope     *artifactWorkflowScope
	needsWrite        bool
	preflightComplete bool
	reobserved        bool
	observed          atomic.Bool
}

func (s *revisionWorkflowScope) operations() map[string]workflowhost.Operation {
	ops := map[string]workflowhost.Operation{
		"installationRevisionRead":             s.read,
		"installationRevisionConfiguration":    s.readConfiguration,
		"installationRevisionResolve":          s.resolve,
		"installationRevisionCompatibility":    s.compatibility,
		"installationRevisionReopenSource":     s.reopen,
		"installationRevisionRenderSource":     s.render,
		"installationRevisionVerifyResources":  s.verifyResources,
		"installationRevisionRecheckResources": s.recheckResources,
		"installationRevisionVerifyStorage":    s.verifyStorage,
		"installationRevisionVerifyProtected":  s.verifyProtected,
		"installationRevisionReobserveConfig":  s.reobserveConfiguration,
		"installationRevisionBegin":            s.begin,
		"installationRevisionApply":            s.apply,
		"installationRevisionObserve":          s.observe,
	}
	for _, name := range []string{"installationArtifactRequirements", "installationArtifactCheck", "installationArtifactsComplete"} {
		name := name
		ops[name] = func(ctx context.Context, args map[string]any) (any, error) {
			if s.artifactScope == nil {
				return nil, errors.New("installation revision artifact scope is not admitted")
			}
			return s.artifactScope.operations()[name](ctx, args)
		}
	}
	for name, operation := range ops {
		ops[name] = func(ctx context.Context, args map[string]any) (any, error) {
			actor, err := preparationActor(ctx)
			if err != nil || actor != s.operator {
				return nil, errors.New("installation execution operation is outside its native scope")
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return operation(ctx, args)
		}
	}
	return ops
}

func noRevisionArguments(args map[string]any) error {
	if len(args) != 0 {
		return errors.New("installation revision operation accepts no caller evidence")
	}
	return nil
}

func (s *revisionWorkflowScope) writable(ctx context.Context) (revisionRecord, error) {
	r, err := s.journal.get(ctx, s.installation, s.key)
	if err != nil {
		return revisionRecord{}, err
	}
	if s.mode != revisionStart || r.Rollback != nil || r.Plan.Preparation == nil || r.Plan.RequestedBy != s.operator || r.Plan.ExecutionWorkflowDigest != s.workflow.digest {
		return revisionRecord{}, errors.New("installation observation does not grant revision write authority")
	}
	return r, nil
}

func (s *revisionWorkflowScope) read(ctx context.Context, args map[string]any) (any, error) {
	if err := noRevisionArguments(args); err != nil {
		return nil, err
	}
	r, err := s.journal.get(ctx, s.installation, s.key)
	if err != nil {
		return nil, err
	}
	mayApply, needsWrite := false, false
	if s.mode == revisionStart {
		if _, err := s.writable(ctx); err != nil {
			return nil, err
		}
		mayApply = true
		facts, observeErr := s.client.Observe(ctx, r.Plan.Intent)
		if observeErr == nil {
			if r.State != "applying" || !facts.IntentObserved {
				return nil, errors.New("installation intent is observed before its durable start fence")
			}
		} else if errors.Is(observeErr, argocd.ErrChanged) {
			needsWrite = true
		} else {
			return nil, errors.New("installation revision cannot be reconciled before admission")
		}
	}
	s.needsWrite = needsWrite
	return map[string]any{"mayApply": mayApply, "needsWrite": needsWrite, "state": r.State}, nil
}

func (s *revisionWorkflowScope) readConfiguration(ctx context.Context, args map[string]any) (any, error) {
	if err := noRevisionArguments(args); err != nil {
		return nil, err
	}
	r, err := s.writable(ctx)
	if err != nil {
		return nil, err
	}
	cfg, err := readReceiver(ctx, s.host.api, s.host.namespace, s.host.configurationName, s.host.catalog)
	if err != nil {
		return nil, err
	}
	if s.host.rendererAddressOverride != "" {
		cfg.rendererAddress = s.host.rendererAddressOverride
	}
	binding := r.Plan.Preparation
	if binding == nil || cfg.configuration.InstallationID != r.Plan.InstallationID || cfg.digest != binding.ConfigurationDigest || cfg.invariantDigest != binding.ConfigurationInvariantDigest ||
		binding.ArtifactWorkflowDigest != s.workflow.artifacts.digest || !freshConfigurationObservation(cfg.observed, time.Now()) {
		return nil, errors.New("installation revision requires its exact fresh authenticated receiver configuration")
	}
	intent, err := argocd.PlanRevision(cfg.application, r.Plan.Intent.RequestID, r.Plan.Intent.Revision, r.Plan.Intent.Prune)
	if err != nil || !sameIntent(intent, r.Plan.Intent) {
		return nil, errors.New("installation revision intent differs from the current authenticated Application baseline")
	}
	s.configuration = cfg
	s.preparation = preparationRecord{}
	s.candidate, s.rollback = pipelines.VerifiedPublishedRelease{}, pipelines.VerifiedPublishedRelease{}
	s.sources = map[string]verifiedSourceCapture{}
	s.renders = map[string]argocd.RenderedRevision{}
	s.resources, s.admission, s.artifacts = resourceEvidence{}, nil, nil
	s.storage, s.sensitive = storageEvidence{}, sensitiveEvidence{}
	s.artifactScope = nil
	s.preflightComplete, s.reobserved = false, false
	return nil, nil
}

func sameIntent(a, b argocd.Intent) bool {
	left, leftErr := a.Digest()
	right, rightErr := b.Digest()
	return leftErr == nil && rightErr == nil && left == right
}

func (s *revisionWorkflowScope) resolve(ctx context.Context, args map[string]any) (any, error) {
	if err := noRevisionArguments(args); err != nil {
		return nil, err
	}
	r, err := s.writable(ctx)
	if err != nil {
		return nil, err
	}
	if s.configuration == nil {
		return nil, errors.New("installation publications require a fresh receiver configuration")
	}
	binding := r.Plan.Preparation
	candidate, err := s.configuration.catalog.Get(ctx, s.configuration.configuration.Catalog.ID, r.Plan.CandidateID, r.Plan.PublicationDigest)
	if err != nil {
		return nil, err
	}
	rollbackID := s.configuration.configuration.Rollback.CandidateID
	if s.configuration.configuration.Rollback.CatalogDigest != binding.RollbackPublicationDigest {
		return nil, errors.New("installation rollback publication differs from its promoted preparation")
	}
	rollback, err := s.configuration.catalog.Get(ctx, s.configuration.configuration.Catalog.ID, rollbackID, binding.RollbackPublicationDigest)
	if err != nil {
		return nil, err
	}
	s.candidate, s.rollback = candidate, rollback
	return nil, nil
}

func (s *revisionWorkflowScope) compatibility(_ context.Context, args map[string]any) (any, error) {
	if err := noRevisionArguments(args); err != nil {
		return nil, err
	}
	if s.configuration == nil || s.candidate.Digest() == "" || s.rollback.Digest() == "" {
		return nil, errors.New("installation compatibility requires both fresh signed publications")
	}
	return nil, pipelines.CheckPublishedReleaseOverlap(s.rollback, s.candidate, s.configuration.dependencies)
}

func (s *revisionWorkflowScope) reopen(ctx context.Context, args map[string]any) (any, error) {
	role, err := preparationRole(args)
	if err != nil {
		return nil, err
	}
	r, err := s.writable(ctx)
	if err != nil {
		return nil, err
	}
	binding := r.Plan.Preparation
	if s.configuration == nil || binding == nil {
		return nil, errors.New("installation retained source requires fresh admission")
	}
	if s.preparation.ID == "" {
		record, err := s.host.journal.get(ctx, r.Plan.InstallationID, binding.ID)
		if err != nil {
			return nil, err
		}
		if record.State != "promoted" || record.PromotedPlanID != r.ID || record.Scope.InstallationID != r.Plan.InstallationID || record.Scope.RequestedBy != s.operator ||
			record.Scope.ExecutionWorkflowDigest != s.workflow.digest || record.Scope.ConfigurationDigest != binding.ConfigurationDigest || record.Scope.ConfigurationInvariantDigest != binding.ConfigurationInvariantDigest {
			return nil, errors.New("installation retained sources are outside the exact promoted preparation")
		}
		s.preparation = record
	}
	entry, exists := s.preparation.Captures[role]
	wantSource, wantReceipt := binding.CandidateSourceDigest, binding.CandidateReceiptDigest
	if role == "rollback" {
		wantSource, wantReceipt = binding.RollbackSourceDigest, binding.RollbackReceiptDigest
	}
	if !exists || !entry.Acknowledged || entry.Receipt == nil || entry.SourceDigest != wantSource || entry.ReceiptDigest != wantReceipt {
		return nil, errors.New("installation retained source differs from its promoted digest and receipt")
	}
	proof, err := s.host.journal.reopenPromotedCapture(ctx, r.Plan.InstallationID, binding.ID, r.ID, role, s.host.files)
	if err != nil {
		return nil, err
	}
	s.sources[role] = proof
	return nil, nil
}

func (s *revisionWorkflowScope) render(ctx context.Context, args map[string]any) (any, error) {
	role, err := preparationRole(args)
	if err != nil {
		return nil, err
	}
	if s.configuration == nil || s.preparation.ID == "" {
		return nil, errors.New("installation render requires reopened promoted sources")
	}
	source, exists := s.sources[role]
	if !exists || source.source.Digest() == "" {
		return nil, errors.New("installation render requires fresh private source verification")
	}
	spec := s.preparation.Scope.Captures[role]
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
	render, err := s.configuration.renderRevision(ctx, source.source.Spec(), credentials)
	if err != nil {
		return nil, err
	}
	r, err := s.journal.get(ctx, s.installation, s.key)
	if err != nil {
		return nil, err
	}
	want := r.Plan.RollbackRenderDigest
	if role == "candidate" {
		want = r.Plan.RenderDigest
	}
	if render.Digest() == "" || render.Digest() != want {
		return nil, errors.New("installation renderer output differs from the promoted immutable render")
	}
	s.renders[role] = render
	return nil, nil
}

func (s *revisionWorkflowScope) verifyResources(ctx context.Context, args map[string]any) (any, error) {
	if err := noRevisionArguments(args); err != nil {
		return nil, err
	}
	r, err := s.writable(ctx)
	if err != nil {
		return nil, err
	}
	if s.configuration == nil || s.renders["rollback"].Digest() == "" || s.renders["candidate"].Digest() == "" || s.candidate.Digest() == "" || s.rollback.Digest() == "" {
		return nil, errors.New("installation resource verification requires both fresh signed renders")
	}
	resources, err := verifyImagesAndDiff(ctx, s.host.api, s.candidate, s.renders["rollback"], s.renders["candidate"], s.configuration.configuration.Platform, s.configuration.bindings)
	if err != nil {
		return nil, err
	}
	if resources.digest != r.Plan.ResourceDiffDigest || resources.publication != r.Plan.PublicationDigest || resources.before != r.Plan.RollbackRenderDigest || resources.after != r.Plan.RenderDigest {
		return nil, errors.New("installation resource diff differs from the promoted plan")
	}
	admission, err := newArtifactAdmission(ctx, s.host.api, artifactAdmissionConfig{ConfigurationDigest: s.configuration.digest, Platform: s.configuration.configuration.Platform, Bindings: s.configuration.bindings, Dependencies: s.configuration.dependencies, Registries: s.configuration.registries}, s.rollback, s.candidate, s.renders["rollback"], s.renders["candidate"])
	if err != nil {
		return nil, err
	}
	if admission.resources != r.Plan.ResourceDiffDigest || admission.configuration != s.configuration.digest || admission.candidate != r.Plan.PublicationDigest || admission.rollback != r.Plan.Preparation.RollbackPublicationDigest || admission.before != r.Plan.RollbackRenderDigest || admission.after != r.Plan.RenderDigest {
		return nil, errors.New("installation artifact scope differs from the promoted plan")
	}
	s.resources, s.admission = resources, admission
	s.artifactScope = &artifactWorkflowScope{admission: admission, operator: s.operator, observations: map[string]artifactObservation{}}
	return nil, nil
}

func (s *revisionWorkflowScope) recheckResources(ctx context.Context, args map[string]any) (any, error) {
	if err := noRevisionArguments(args); err != nil {
		return nil, err
	}
	r, err := s.writable(ctx)
	if err != nil {
		return nil, err
	}
	if s.configuration == nil || s.candidate.Digest() == "" || s.resources.digest == "" {
		return nil, errors.New("installation resource recheck requires the exact signed plan")
	}
	current, err := verifyImagesAndDiff(ctx, s.host.api, s.candidate, s.renders["rollback"], s.renders["candidate"], s.configuration.configuration.Platform, s.configuration.bindings)
	if err != nil || current.digest != r.Plan.ResourceDiffDigest || current.digest != s.resources.digest || current.publication != s.resources.publication || current.before != s.resources.before || current.after != s.resources.after {
		return nil, errors.New("installation resources changed during immutable artifact verification")
	}
	if s.artifactScope == nil || s.artifactScope.result == nil || !freshArtifactObservation(s.artifactScope.result.observed, s.artifactScope.result.expires, time.Now()) {
		return nil, errors.New("installation artifact recipe did not complete fresh immutable reads")
	}
	s.resources = current
	artifact := *s.artifactScope.result
	artifact.workflow, artifact.operator = s.workflow.artifacts.digest, s.operator
	artifact.digest = artifactHash("workflow-evidence", []string{artifact.digest, s.workflow.artifacts.digest, s.operator})
	s.artifacts = &artifact
	return nil, nil
}

func (s *revisionWorkflowScope) verifyStorage(ctx context.Context, args map[string]any) (any, error) {
	if err := noRevisionArguments(args); err != nil {
		return nil, err
	}
	r, err := s.writable(ctx)
	if err != nil {
		return nil, err
	}
	evidence, err := verifyStoragePreservation(ctx, s.host.api, s.renders["rollback"], s.renders["candidate"])
	if err != nil {
		return nil, err
	}
	if r.Plan.Preparation == nil || evidence.digest != r.Plan.Preparation.StorageDigest || evidence.before != r.Plan.RollbackRenderDigest || evidence.after != r.Plan.RenderDigest || !freshPreservationObservation(evidence.observed, time.Now()) {
		return nil, errors.New("installation storage differs from its fresh promoted preservation evidence")
	}
	s.storage = evidence
	return nil, nil
}

func (s *revisionWorkflowScope) verifyProtected(ctx context.Context, args map[string]any) (any, error) {
	if err := noRevisionArguments(args); err != nil {
		return nil, err
	}
	r, err := s.writable(ctx)
	if err != nil {
		return nil, err
	}
	if s.configuration == nil {
		return nil, errors.New("installation protected resources require authenticated configuration")
	}
	evidence, err := verifySensitivePreservation(ctx, s.host.api, s.renders["rollback"], s.renders["candidate"], s.configuration.protected)
	if err != nil {
		return nil, err
	}
	if r.Plan.Preparation == nil || evidence.digest != r.Plan.Preparation.SensitiveDigest || evidence.before != r.Plan.RollbackRenderDigest || evidence.after != r.Plan.RenderDigest || !freshPreservationObservation(evidence.observed, time.Now()) {
		return nil, errors.New("installation protected resources differ from fresh promoted preservation evidence")
	}
	s.sensitive = evidence
	return nil, nil
}

func (s *revisionWorkflowScope) reobserveConfiguration(ctx context.Context, args map[string]any) (any, error) {
	if err := noRevisionArguments(args); err != nil {
		return nil, err
	}
	r, err := s.writable(ctx)
	if err != nil {
		return nil, err
	}
	s.reobserved = false
	fresh, err := readReceiver(ctx, s.host.api, s.host.namespace, s.host.configurationName, s.host.catalog)
	if err != nil {
		return nil, err
	}
	if s.host.rendererAddressOverride != "" {
		fresh.rendererAddress = s.host.rendererAddressOverride
	}
	if fresh.digest != s.configuration.digest || fresh.invariantDigest != r.Plan.Preparation.ConfigurationInvariantDigest {
		return nil, errors.New("installation configuration changed during revision admission")
	}
	intent, err := argocd.PlanRevision(fresh.application, r.Plan.Intent.RequestID, r.Plan.Intent.Revision, r.Plan.Intent.Prune)
	if err != nil || !sameIntent(intent, r.Plan.Intent) {
		return nil, errors.New("installation Application changed during revision admission")
	}
	s.configuration = fresh
	s.reobserved = true
	s.preflightComplete = true
	return nil, nil
}

func freshConfigurationObservation(observed, now time.Time) bool {
	return !observed.IsZero() && !observed.After(now) && now.Sub(observed) <= time.Minute
}

func freshArtifactObservation(observed, expires, now time.Time) bool {
	return !observed.IsZero() && !observed.After(now) && now.Sub(observed) <= 30*time.Minute && expires.After(now) && !expires.After(observed.Add(30*time.Minute))
}

func (s *revisionWorkflowScope) freshWriteEvidence(r revisionRecord) (revisionWriteAdmission, error) {
	binding := r.Plan.Preparation
	if binding == nil || s.configuration == nil || !s.preflightComplete || !s.reobserved || s.configuration.digest != binding.ConfigurationDigest ||
		s.configuration.invariantDigest != binding.ConfigurationInvariantDigest || binding.ArtifactWorkflowDigest != s.workflow.artifacts.digest || !freshConfigurationObservation(s.configuration.observed, time.Now()) {
		return revisionWriteAdmission{}, errors.New("installation write requires freshly authenticated receiving configuration and completed admission")
	}
	if s.artifacts == nil || s.artifacts.workflow != s.workflow.artifacts.digest || s.artifacts.operator != s.operator ||
		s.artifacts.configuration != binding.ConfigurationDigest || s.artifacts.candidate != r.Plan.PublicationDigest || s.artifacts.rollback != binding.RollbackPublicationDigest ||
		s.artifacts.resources != r.Plan.ResourceDiffDigest || s.artifacts.before != r.Plan.RollbackRenderDigest || s.artifacts.after != r.Plan.RenderDigest || !freshArtifactObservation(s.artifacts.observed, s.artifacts.expires, time.Now()) {
		return revisionWriteAdmission{}, errors.New("installation write requires complete fresh immutable artifact evidence")
	}
	if s.resources.digest != r.Plan.ResourceDiffDigest || s.resources.publication != r.Plan.PublicationDigest || s.resources.before != r.Plan.RollbackRenderDigest || s.resources.after != r.Plan.RenderDigest || !freshPreservationObservation(s.resources.observed, time.Now()) {
		return revisionWriteAdmission{}, errors.New("installation write requires fresh resource evidence")
	}
	if s.storage.digest != binding.StorageDigest || s.storage.before != r.Plan.RollbackRenderDigest || s.storage.after != r.Plan.RenderDigest || !freshPreservationObservation(s.storage.observed, time.Now()) ||
		s.sensitive.digest != binding.SensitiveDigest || s.sensitive.before != r.Plan.RollbackRenderDigest || s.sensitive.after != r.Plan.RenderDigest || !freshPreservationObservation(s.sensitive.observed, time.Now()) {
		return revisionWriteAdmission{}, errors.New("installation write requires fresh storage and protected-resource preservation evidence")
	}
	// The promoted artifact receipt is historical. The sealed artifact child
	// above has just re-read the exact immutable images, so current proof expiry
	// governs the effect; an old receipt's expiration cannot strand recovery.
	freshUntil := s.configuration.observed.Add(time.Minute)
	for _, limit := range []time.Time{s.resources.observed.Add(time.Minute), s.storage.observed.Add(time.Minute), s.sensitive.observed.Add(time.Minute), s.artifacts.expires} {
		if limit.Before(freshUntil) {
			freshUntil = limit
		}
	}
	if !freshUntil.After(time.Now()) {
		return revisionWriteAdmission{}, errors.New("installation write admission expired before the Argo effect")
	}
	return revisionWriteAdmission{planID: r.ID, workflow: s.workflow.digest, freshUntil: freshUntil}, nil
}

func (s *revisionWorkflowScope) begin(ctx context.Context, args map[string]any) (any, error) {
	if err := noRevisionArguments(args); err != nil {
		return nil, err
	}
	r, err := s.writable(ctx)
	if err != nil {
		return nil, err
	}
	if !s.needsWrite {
		return nil, errors.New("installation start requires a controller read that still needs a write")
	}
	admission, err := s.freshWriteEvidence(r)
	if err != nil {
		return nil, err
	}
	_, err = s.journal.beginAdmitted(ctx, s.installation, s.key, s.workflow.digest, admission)
	return nil, err
}

func (s *revisionWorkflowScope) apply(ctx context.Context, args map[string]any) (any, error) {
	if err := noRevisionArguments(args); err != nil {
		return nil, err
	}
	r, err := s.writable(ctx)
	if err != nil {
		return nil, err
	}
	if r.State != "applying" {
		return nil, errors.New("installation effect must be durably started before an Argo write")
	}
	if !s.needsWrite {
		return nil, nil
	}
	admission, err := s.freshWriteEvidence(r)
	if err != nil {
		return nil, err
	}
	// Reconcile once more after the potentially slow admission. The adapter's
	// subsequent JSON Patch also carries the complete Application CAS tests.
	effectCtx, cancel := context.WithDeadline(ctx, admission.freshUntil)
	defer cancel()
	if _, err := s.client.Observe(effectCtx, r.Plan.Intent); err == nil {
		return nil, nil
	} else if !errors.Is(err, argocd.ErrChanged) {
		return nil, errors.New("installation revision cannot be reconciled before applying")
	}
	// Observe itself can use most of a proof's lifetime. Re-evaluate the exact
	// native evidence after that read and retain its original absolute expiry.
	currentAdmission, err := s.freshWriteEvidence(r)
	if err != nil || !currentAdmission.freshUntil.Equal(admission.freshUntil) {
		return nil, errors.New("installation write admission expired during controller reconciliation")
	}
	if _, err := s.client.Apply(effectCtx, r.Plan.Intent); err != nil {
		return nil, errors.New("installation revision effect is unconfirmed; reconcile its recorded intent")
	}
	return nil, nil
}

func (s *revisionWorkflowScope) observe(ctx context.Context, args map[string]any) (any, error) {
	if err := noRevisionArguments(args); err != nil {
		return nil, err
	}
	r, err := s.journal.get(ctx, s.installation, s.key)
	if err != nil {
		return nil, err
	}
	if r.State != "applying" {
		return nil, errors.New("installation observation requires a started intent")
	}
	facts, err := s.client.Observe(ctx, r.Plan.Intent)
	if err != nil {
		return nil, errors.New("installation revision cannot be confirmed from the receiving controller")
	}
	if _, err := s.journal.observe(ctx, s.installation, s.key, r.ObservationVersion, facts); err != nil {
		return nil, err
	}
	s.observed.Store(true)
	return map[string]any{"operationSucceeded": facts.OperationSucceeded, "healthy": facts.Healthy, "synced": facts.Synced}, nil
}
