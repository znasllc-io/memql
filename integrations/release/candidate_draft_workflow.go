package release

import (
	"context"
	"errors"
	"time"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/githubrelease"
)

const candidateDraftWorkflow = "releaseCreateDraftWorkflow"
const candidatePromotionWorkflow = "releasePromoteDraftWorkflow"

type candidateDraftRequest struct{ CandidateID, ApprovalID, TargetID string }

func (p *candidatePreparer) draft(ctx context.Context, request candidateDraftRequest, promote bool) (candidateDraftRecord, error) {
	if _, err := candidateOwner(ctx); err != nil {
		return candidateDraftRecord{}, err
	}
	if p == nil || p.ledger == nil || p.targetReader == nil {
		return candidateDraftRecord{}, errors.New("release lifecycle is unavailable")
	}
	record, err := p.ledger.get(ctx, request.CandidateID)
	if err != nil {
		return candidateDraftRecord{}, err
	}
	if record.State != "approved" || request.ApprovalID == "" || record.ApprovalID != request.ApprovalID {
		return candidateDraftRecord{}, errors.New("release lifecycle requires a separate exact candidate approval")
	}
	plan, err := p.targetReader.draftPlan(ctx, record, request.TargetID)
	if err != nil {
		return candidateDraftRecord{}, err
	}
	if plan.Target.Draft == nil {
		return candidateDraftRecord{}, errors.New("release lifecycle requires exact reviewed name, body and prerelease metadata")
	}
	name := candidateDraftWorkflow
	if promote {
		name = candidatePromotionWorkflow
	}
	definition, err := workflowhost.Load(name)
	if err != nil {
		return candidateDraftRecord{}, err
	}
	definition, err = automations.NewLoader(automations.LoaderOptions{Logger: p.logger}).Snapshot(definition)
	if err != nil || definition == nil || !definition.Trusted {
		return candidateDraftRecord{}, errors.New("release lifecycle requires its immutable installed recipe")
	}
	s := &candidateDraftScope{p: p, request: request, record: record, plan: plan, promote: promote, definition: definition}
	ctx, cancel := context.WithTimeout(ctx, time.Hour)
	defer cancel()
	_, err = workflowhost.Run(ctx, name, nil, workflowhost.Options{Logger: p.logger, Operations: s.operations(),
		Load: func(child string) (*automations.Automation, error) {
			if child != name {
				return nil, errors.New("release lifecycle cannot load an unbound child")
			}
			return definition, nil
		},
		LoadLogic: func(string) (*memql.Function, error) {
			return nil, errors.New("release lifecycle cannot load unbound logic")
		},
	})
	if err != nil {
		return candidateDraftRecord{}, err
	}
	want := "ready"
	if promote {
		want = "published"
	}
	if s.out.CandidateID != request.CandidateID || s.out.State != want {
		return candidateDraftRecord{}, errors.New("release recipe did not record verified completion")
	}
	return s.out, nil
}

type candidateDraftScope struct {
	p                *candidatePreparer
	request          candidateDraftRequest
	record           candidateRecord
	plan             candidateDraftPlan
	promote          bool
	definition       *automations.Automation
	checked, claimed bool
	binding, out     candidateDraftRecord
	proof            *githubrelease.ReleaseState
}

func (s *candidateDraftScope) operations() map[string]workflowhost.Operation {
	ops := map[string]workflowhost.Operation{
		"releaseDraftCheckCandidate":   s.check,
		"releaseDraftBegin":            s.begin,
		"releaseDraftObserveBound":     s.observeBound,
		"releaseDraftEnsureTag":        s.ensureTag,
		"releaseDraftFind":             s.find,
		"releaseDraftClaimCreate":      s.claimCreate,
		"releaseDraftCreate":           s.create,
		"releaseDraftRecord":           s.recordDraft,
		"releasePromotionPublications": s.publications,
		"releasePromotionObserve":      s.observe,
		"releasePromotionClaim":        s.claimPromotion,
		"releasePromotionWrite":        s.writePromotion,
		"releasePromotionRecord":       s.recordPromotion,
		"releaseDraftUncertain":        func(context.Context, map[string]any) (any, error) { return nil, githubrelease.ErrUncertain },
		"releasePromotionRefuse": func(context.Context, map[string]any) (any, error) {
			return nil, errors.New("release promotion requires every approved destination to be complete")
		},
	}
	for name, operation := range ops {
		ops[name] = func(ctx context.Context, args map[string]any) (any, error) {
			owner, err := candidateOwner(ctx)
			if err != nil || owner != s.record.Manifest.OwnerUserID {
				return nil, errors.New("release operation requires its original owner")
			}
			return operation(ctx, args)
		}
	}
	return ops
}

func (s *candidateDraftScope) check(ctx context.Context, _ map[string]any) (any, error) {
	s.checked = false
	definition, err := workflowhost.Load(candidatePrepareWorkflow)
	if err != nil {
		return nil, err
	}
	// Preparation fingerprints both lifecycle recipes. Its loader snapshot is
	// pinned to the definition executing here to detect reloads before effects.
	bound := *s.p
	bound.draftDefinitions = map[string]*automations.Automation{s.definition.Name: s.definition}
	rec, err := bound.prepareWithExpectedIdentity(ctx, s.record.Manifest, definition, s.record.ID, nil)
	if err != nil {
		return nil, err
	}
	if rec.State != "approved" {
		return nil, errors.New("release candidate approval changed")
	}
	rec, err = s.p.ledger.get(ctx, s.record.ID)
	if err != nil {
		return nil, err
	}
	if rec.State != "approved" || rec.ApprovalID != s.request.ApprovalID {
		return nil, errors.New("release candidate approval changed")
	}
	s.checked = true
	return nil, nil
}

func (s *candidateDraftScope) begin(ctx context.Context, _ map[string]any) (any, error) {
	if !s.checked {
		return nil, errors.New("release intent requires fresh candidate verification")
	}
	rec, err := s.p.ledger.beginDraft(ctx, s.record.ID, s.request.ApprovalID, s.plan)
	if err != nil {
		return nil, err
	}
	s.binding = rec
	return map[string]any{"state": rec.State, "ready": rec.State == "ready", "releaseId": rec.ReleaseID}, nil
}

func (s *candidateDraftScope) observeBound(ctx context.Context, _ map[string]any) (any, error) {
	if s.promote || s.binding.State != "ready" || s.binding.ReleaseID <= 0 {
		return nil, errors.New("draft observation requires its native ready binding")
	}
	c, err := s.client(ctx)
	if err != nil {
		return nil, err
	}
	if err := c.CheckDraft(ctx, s.binding.ReleaseID, *s.plan.Target.Draft, s.marker()); err != nil {
		return nil, err
	}
	s.out = s.binding
	return nil, nil
}

func (s *candidateDraftScope) client(ctx context.Context) (*githubrelease.Lifecycle, error) {
	if !s.checked || s.binding.IntentID == "" {
		return nil, errors.New("release protocol operation requires its native intent")
	}
	return s.p.targetReader.fileLifecycle(ctx, s.plan.Assets[0].Destination)
}
func (s *candidateDraftScope) marker() string {
	if s.plan.Target.ReleaseID == 0 {
		return s.binding.IntentID
	}
	return ""
}

func (s *candidateDraftScope) ensureTag(ctx context.Context, _ map[string]any) (any, error) {
	if s.promote || (s.binding.State != "prepared" && s.binding.State != "creating") {
		return nil, errors.New("tag creation is outside this draft scope")
	}
	c, err := s.client(ctx)
	if err != nil {
		return nil, err
	}
	return nil, c.EnsureTag(ctx)
}
func (s *candidateDraftScope) find(ctx context.Context, _ map[string]any) (any, error) {
	if s.promote || (s.binding.State != "prepared" && s.binding.State != "creating") {
		return nil, errors.New("draft discovery is outside this creation scope")
	}
	c, err := s.client(ctx)
	if err != nil {
		return nil, err
	}
	proof, found, err := c.FindDraft(ctx, *s.plan.Target.Draft, s.marker())
	if err != nil {
		return nil, err
	}
	if found {
		if !proof.Draft {
			return nil, errors.New("created release is no longer a draft")
		}
		s.proof = &proof
	}
	return map[string]any{"found": found}, nil
}
func (s *candidateDraftScope) claimCreate(ctx context.Context, _ map[string]any) (any, error) {
	if !s.checked || s.promote || s.binding.IntentID == "" {
		return nil, errors.New("draft creation claim is outside this scope")
	}
	var err error
	s.binding, s.claimed, err = s.p.ledger.claimDraftEffect(ctx, s.record.ID, s.request.ApprovalID, s.plan, false)
	return map[string]any{"claimed": s.claimed}, err
}
func (s *candidateDraftScope) create(ctx context.Context, _ map[string]any) (any, error) {
	if !s.claimed || s.promote || s.binding.State != "creating" {
		return nil, errors.New("draft POST requires the unique durable attempt")
	}
	s.claimed = false // even a repeated call in one malformed recipe cannot POST twice
	c, err := s.client(ctx)
	if err != nil {
		return nil, err
	}
	proof, err := c.CreateDraft(ctx, *s.plan.Target.Draft, s.marker())
	if err != nil {
		return nil, err
	}
	s.proof = &proof
	return nil, nil
}
func (s *candidateDraftScope) recordDraft(ctx context.Context, _ map[string]any) (any, error) {
	if !s.checked || s.promote || s.proof == nil {
		return nil, errors.New("draft recording requires verified native readback")
	}
	var err error
	s.out, err = s.p.ledger.recordDraft(ctx, s.record.ID, s.request.ApprovalID, s.plan, *s.proof)
	return nil, err
}

func (s *candidateDraftScope) publications(ctx context.Context, _ map[string]any) (any, error) {
	if !s.checked || !s.promote {
		return nil, errors.New("publication inspection is outside promotion scope")
	}
	entries, err := s.p.ledger.effects(ctx, s.record.ID)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, d := range s.record.Manifest.Destinations {
		complete := false
		for _, e := range entries {
			if e.Target == d && e.State == "complete" {
				complete = true
			}
		}
		out = append(out, map[string]any{"targetId": d.TargetID, "complete": complete})
	}
	return out, nil
}
func (s *candidateDraftScope) observe(ctx context.Context, _ map[string]any) (any, error) {
	if !s.promote || s.binding.ReleaseID <= 0 {
		return nil, errors.New("promotion requires an existing native draft binding")
	}
	c, err := s.client(ctx)
	if err != nil {
		return nil, err
	}
	proof, err := c.Observe(ctx, s.binding.ReleaseID, *s.plan.Target.Draft, s.marker(), s.plan.expected())
	if err != nil {
		return nil, err
	}
	s.proof = &proof
	return map[string]any{"draft": proof.Draft}, nil
}
func (s *candidateDraftScope) claimPromotion(ctx context.Context, _ map[string]any) (any, error) {
	if !s.checked || !s.promote || s.proof == nil || s.binding.ReleaseID <= 0 {
		return nil, errors.New("promotion claim requires native asset readback")
	}
	var err error
	s.binding, s.claimed, err = s.p.ledger.claimDraftEffect(ctx, s.record.ID, s.request.ApprovalID, s.plan, true)
	return map[string]any{"claimed": s.claimed, "state": s.binding.State}, err
}
func (s *candidateDraftScope) writePromotion(ctx context.Context, _ map[string]any) (any, error) {
	if !s.promote || !s.claimed || s.binding.State != "promoting" || s.proof == nil {
		return nil, errors.New("release promotion requires its unique durable attempt")
	}
	s.claimed = false
	c, err := s.client(ctx)
	if err != nil {
		return nil, err
	}
	proof, err := c.Promote(ctx, s.binding.ReleaseID, *s.plan.Target.Draft, s.marker(), s.plan.expected())
	if err != nil {
		return nil, err
	}
	s.proof = &proof
	return nil, nil
}
func (s *candidateDraftScope) recordPromotion(ctx context.Context, _ map[string]any) (any, error) {
	if !s.checked || !s.promote || s.proof == nil || s.proof.Draft {
		return nil, errors.New("promotion recording requires verified published readback")
	}
	var err error
	s.out, err = s.p.ledger.recordPromotion(ctx, s.record.ID, s.request.ApprovalID, s.plan, *s.proof)
	return nil, err
}
