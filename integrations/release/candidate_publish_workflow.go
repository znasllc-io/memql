package release

import (
	"context"
	"errors"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/githubrelease"
	"github.com/znasllc-io/memql/integrations/ociregistry"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

// Approval names an already reviewed candidate. It does not accept a manifest,
// a passed flag or a destination. Fresh verification runs before the native
// ledger commits the owner's separate, durable approval.
func (p *candidatePreparer) approve(ctx context.Context, key string) (candidateRecord, error) {
	if _, err := candidateOwner(ctx); err != nil {
		return candidateRecord{}, err
	}
	if p == nil || p.ledger == nil {
		return candidateRecord{}, errors.New("candidate approval is unavailable")
	}
	record, err := p.ledger.get(ctx, key)
	if err != nil {
		return candidateRecord{}, err
	}
	if record.State != "ready" && record.State != "approved" {
		return candidateRecord{}, errors.New("candidate is not ready for owner approval")
	}
	definition, err := workflowhost.Load(candidatePrepareWorkflow)
	if err != nil {
		return candidateRecord{}, err
	}
	if _, err := p.prepareWithExpectedIdentity(ctx, record.Manifest, definition, key, nil); err != nil {
		return candidateRecord{}, err
	}
	return p.ledger.approve(ctx, key)
}

type candidatePublishRequest struct {
	CandidateID, ApprovalID, TargetID, Component, Artifact string
}

// The owner entry selects one already approved effect using native app wiring;
// the DSL composes its protocol
// operations. Registry credentials never enter DSL arguments or journal rows.
func (p *candidatePreparer) publish(ctx context.Context, request candidatePublishRequest) (candidatePublication, error) {
	if _, err := candidateOwner(ctx); err != nil {
		return candidatePublication{}, err
	}
	definition, err := workflowhost.Load(candidatePublishWorkflow)
	if err != nil {
		return candidatePublication{}, err
	}
	return p.publishWithWorkflow(ctx, request, definition)
}

func (p *candidatePreparer) publishWithWorkflow(ctx context.Context, request candidatePublishRequest, definition *automations.Automation) (out candidatePublication, err error) {
	if _, err := candidateOwner(ctx); err != nil {
		return out, err
	}
	if p == nil || p.ledger == nil || p.library == nil || p.targetReader == nil {
		return out, errors.New("candidate publication is unavailable")
	}
	record, err := p.ledger.get(ctx, request.CandidateID)
	if err != nil {
		return out, err
	}
	if record.State != "approved" || request.ApprovalID == "" || request.ApprovalID != record.ApprovalID {
		return out, errors.New("publication requires a separate exact candidate approval")
	}
	if _, err := candidatePublicationFor(record, request.TargetID, request.Component, request.Artifact); err != nil {
		return out, err
	}
	if definition == nil || !definition.Trusted {
		return out, errors.New("publication requires its installed workflow")
	}
	definition, err = automations.NewLoader(automations.LoaderOptions{Logger: p.logger}).Snapshot(definition)
	if err != nil || !definition.Trusted {
		return out, errors.New("publication requires its installed workflow")
	}
	scope := &candidatePublishScope{p: p, request: request, record: record, definition: definition}
	defer func() {
		if scope.image != nil {
			err = errors.Join(err, scope.image.Close())
		}
		if scope.file != nil {
			err = errors.Join(err, scope.file.Close())
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, time.Hour)
	defer cancel()
	_, err = workflowhost.Run(ctx, definition.Name, nil, workflowhost.Options{Logger: p.logger, Operations: scope.operations(),
		Load: func(name string) (*automations.Automation, error) {
			if name != definition.Name {
				return nil, errors.New("publication cannot load an unbound child")
			}
			return definition, nil
		},
		LoadLogic: func(string) (*memql.Function, error) { return nil, errors.New("publication cannot load unbound logic") },
	})
	if err != nil {
		return out, err
	}
	if scope.out.State != "complete" || scope.out.CandidateID != request.CandidateID {
		return out, errors.New("publication workflow did not record verified completion")
	}
	return scope.out, nil
}

type candidatePublishScope struct {
	p           *candidatePreparer
	request     candidatePublishRequest
	record      candidateRecord
	definition  *automations.Automation
	checked     bool
	intent, out candidatePublication
	image       *ociregistry.VerifiedImage
	file        *githubrelease.VerifiedFile
	proof       *candidatePublicationReceipt
}

func (s *candidatePublishScope) operations() map[string]workflowhost.Operation {
	return map[string]workflowhost.Operation{
		"releasePublicationCheckCandidate": s.check,
		"releasePublicationBegin":          s.begin,
		"releasePublicationVerifyImage":    s.verifyImage,
		"releasePublicationWriteImage":     s.write,
		"releasePublicationVerifyFile":     s.verifyFile,
		"releasePublicationWriteFile":      s.writeFile,
		"releasePublicationRecordReceipt":  s.complete,
	}
}

func (s *candidatePublishScope) check(ctx context.Context, _ map[string]any) (any, error) {
	s.checked = false
	definition, err := workflowhost.Load(candidatePrepareWorkflow)
	if err != nil {
		return nil, err
	}
	record, err := s.p.prepareWithExpectedIdentity(ctx, s.record.Manifest, definition, s.request.CandidateID, s.definition)
	if err != nil {
		return nil, err
	}
	if record.State != "approved" {
		return nil, errors.New("candidate approval no longer exists")
	}
	s.checked = true
	return nil, nil
}

func (s *candidatePublishScope) begin(ctx context.Context, _ map[string]any) (any, error) {
	if !s.checked {
		return nil, errors.New("publication intent requires fresh candidate verification")
	}
	intent, err := s.p.ledger.beginPublication(ctx, s.request.CandidateID, s.request.ApprovalID, s.request.TargetID, s.request.Component, s.request.Artifact)
	if err != nil {
		return nil, err
	}
	s.intent = intent
	if intent.Artifact.Kind == "file" {
		plan, err := s.p.targetReader.draftPlan(ctx, s.record, intent.Target.TargetID)
		if err != nil {
			return nil, err
		}
		if _, err := s.p.ledger.beginDraft(ctx, s.record.ID, s.request.ApprovalID, plan); err != nil {
			return nil, err
		}
	}
	if intent.State == "complete" {
		s.out = intent
	}
	return map[string]any{"complete": intent.State == "complete", "kind": intent.Artifact.Kind}, nil
}

func (s *candidatePublishScope) verifyImage(ctx context.Context, _ map[string]any) (any, error) {
	if !s.checked || s.intent.EffectID == "" {
		return nil, errors.New("image verification requires its approved publication intent")
	}
	if s.intent.State == "complete" || s.image != nil {
		return nil, nil
	}
	a := s.intent.Artifact
	if a.Kind != "oci" {
		return nil, errors.New("registry publication requires an OCI artifact")
	}
	scope := pipelinesteps.RunFileReceiptScope{OwnerUserID: s.record.Manifest.OwnerUserID, WorkRunID: a.Receipt.WorkRunID, StepKey: a.Receipt.StepKey, Attempt: a.Receipt.Attempt}
	receipt, body, err := s.p.library.OpenRunFileReceipt(auth.ContextWithInternalOrigin(ctx), scope, a.Receipt.IntentID)
	if err != nil {
		return nil, err
	}
	if body == nil {
		return nil, errors.New("publication artifact returned no stream")
	}
	if receiptScope(receipt) != scope || receipt.IntentID != a.Receipt.IntentID || receipt.Size != a.Size || "sha256:"+receipt.SHA256 != a.Digest || receipt.ETag == "" {
		return nil, errors.Join(errors.New("publication artifact differs from candidate"), body.Close())
	}
	image, verifyErr := ociregistry.Verify(ctx, body, ociregistry.Expected{ArchiveSHA256: a.Digest, ArchiveSize: a.Size, ImageDigest: a.ImageDigest, Platform: a.Platform}, ociregistry.Limits{})
	err = errors.Join(verifyErr, body.Close())
	if err != nil {
		if image != nil {
			err = errors.Join(err, image.Close())
		}
		return nil, err
	}
	s.image = image
	return nil, nil
}

func (s *candidatePublishScope) write(ctx context.Context, _ map[string]any) (any, error) {
	if !s.checked || s.intent.EffectID == "" || s.intent.ApprovalID != s.request.ApprovalID {
		return nil, errors.New("registry write requires its verified approval and durable intent")
	}
	if s.intent.State == "complete" || s.proof != nil {
		return nil, nil
	}
	if s.image == nil {
		return nil, errors.New("registry write requires verified immutable image bytes")
	}
	publisher, err := s.p.targetReader.publisher(ctx, s.intent.Target)
	if err != nil {
		return nil, err
	}
	receipt, err := publisher.Publish(ctx, s.image)
	if err != nil {
		return nil, err
	} // uncertain outcomes keep the intent and every pin
	if receipt.ArchiveSize != s.intent.Artifact.Size {
		return nil, errors.New("publication readback size differs")
	}
	s.proof = &candidatePublicationReceipt{TargetDigest: s.intent.Target.TargetDigest, ArchiveDigest: receipt.ArchiveSHA256, ImageDigest: receipt.ImageDigest, Platform: receipt.Platform}
	return nil, nil
}

func (s *candidatePublishScope) complete(ctx context.Context, _ map[string]any) (any, error) {
	if s.intent.State == "complete" {
		s.out = s.intent
		return nil, nil
	}
	if s.proof == nil || s.intent.EffectID == "" {
		return nil, errors.New("publication completion requires verified destination readback")
	}
	if err := s.p.ledger.finishPublication(ctx, s.intent, *s.proof); err != nil {
		return nil, err
	}
	s.out = s.intent
	s.out.State = "complete"
	return nil, nil
}
