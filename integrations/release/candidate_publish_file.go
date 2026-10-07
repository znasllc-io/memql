package release

import (
	"context"
	"errors"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/integrations/githubrelease"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

func (s *candidatePublishScope) verifyFile(ctx context.Context, _ map[string]any) (any, error) {
	if !s.checked || s.intent.EffectID == "" || s.intent.Artifact.Kind != "file" {
		return nil, errors.New("file verification requires its approved file publication intent")
	}
	if s.intent.State == "complete" || s.file != nil {
		return nil, nil
	}
	a := s.intent.Artifact
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
	file, verifyErr := githubrelease.Verify(ctx, body, githubrelease.Expected{SHA256: a.Digest, Size: a.Size}, githubrelease.Limits{})
	err = errors.Join(verifyErr, body.Close())
	if err != nil {
		if file != nil {
			err = errors.Join(err, file.Close())
		}
		return nil, err
	}
	s.file = file
	return nil, nil
}

func (s *candidatePublishScope) writeFile(ctx context.Context, _ map[string]any) (any, error) {
	if !s.checked || s.intent.EffectID == "" || s.intent.ApprovalID != s.request.ApprovalID || s.intent.Artifact.Kind != "file" {
		return nil, errors.New("release asset write requires its verified file approval and durable intent")
	}
	if s.intent.State == "complete" || s.proof != nil {
		return nil, nil
	}
	if s.file == nil {
		return nil, errors.New("release asset write requires verified immutable bytes")
	}
	plan, err := s.p.targetReader.draftPlan(ctx, s.record, s.intent.Target.TargetID)
	if err != nil {
		return nil, err
	}
	binding, err := s.p.ledger.beginDraft(ctx, s.record.ID, s.request.ApprovalID, plan)
	if err != nil {
		return nil, err
	}
	var receipt githubrelease.Receipt
	err = s.p.ledger.withDraftUpload(ctx, s.record.ID, s.request.ApprovalID, plan, func(releaseID int64) error {
		var lifecycle *githubrelease.Lifecycle
		marker := ""
		if plan.Target.ReleaseID == 0 {
			marker = binding.IntentID
		}
		if plan.Target.Draft != nil {
			var err error
			lifecycle, err = s.p.targetReader.fileLifecycle(ctx, s.intent.Target)
			if err != nil {
				return err
			}
			if err := lifecycle.CheckDraft(ctx, releaseID, *plan.Target.Draft, marker); err != nil {
				return err
			}
		}
		publisher, err := s.p.targetReader.boundFilePublisher(ctx, s.intent.Target, releaseID)
		if err != nil {
			return err
		}
		receipt, err = publisher.Publish(ctx, s.file)
		if err != nil {
			return err
		}
		if receipt.Size != s.intent.Artifact.Size || receipt.SHA256 != s.intent.Artifact.Digest {
			return errors.New("release asset readback differs from approved bytes")
		}
		if lifecycle != nil {
			if err := lifecycle.CheckDraft(ctx, releaseID, *plan.Target.Draft, marker); err != nil {
				return githubrelease.ErrUncertain
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.proof = &candidatePublicationReceipt{TargetDigest: s.intent.Target.TargetDigest, ArchiveDigest: receipt.SHA256, Platform: s.intent.Artifact.Platform}
	return nil, nil
}
