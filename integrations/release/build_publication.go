package release

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

func (s *releaseBuildScope) publish(ctx context.Context, approval string) error {
	if _, err := s.gate(ctx); err != nil {
		return err
	}
	r := &s.lease.record
	if r.CandidateID == "" || approval == "" {
		return errors.New("publication requires the saved candidate's separate approval")
	}
	c, err := s.lease.ledger.get(ctx, r.CandidateID)
	if err != nil {
		return err
	}
	if c.State != "approved" || c.ApprovalID != approval || r.ApprovalID != "" && r.ApprovalID != approval {
		return errors.New("publication approval differs from the exact saved candidate")
	}
	r.ApprovalID, r.PublicationStarted, r.Phase = approval, true, "publishing"
	if err := s.lease.save(ctx); err != nil {
		return err
	}
	_, err = workflowhost.RunSnapshot(ctx, r.Selection.Publication, buildPublicationContract, map[string]any{"candidateId": r.CandidateID, "approvalId": approval}, workflowhost.Options{Logger: s.i.logger, Operations: s.publicationOperations()})
	if err != nil {
		return err
	}
	c, err = s.lease.ledger.get(ctx, r.CandidateID)
	if err != nil {
		return err
	}
	catalog := s.i.candidateCatalogStatus(ctx, c)
	if catalog.State != "sealed" || catalog.ApprovalID != approval {
		return errors.New("publication recipe has not produced a verified signed catalog entry")
	}
	r.Phase, r.ReasonCode, r.Reason = "published", "", ""
	return s.lease.save(ctx)
}

func (s *releaseBuildScope) publicationOperations() map[string]workflowhost.Operation {
	type handler func(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error)
	handlers := map[string]handler{
		"releaseGetCandidate":           s.i.handleCandidateGet,
		"releaseCandidateTargets":       s.i.handleCandidateTargets,
		"releaseCandidateDrafts":        s.i.handleCandidateDrafts,
		"releaseCandidatePublications":  s.i.handleCandidateEffects,
		"releaseCreateCandidateDraft":   s.i.handleCandidateDraftCreate,
		"releasePublishCandidate":       s.i.handleCandidatePublish,
		"releasePromoteCandidateDraft":  s.i.handleCandidateDraftPromote,
		"releaseSealPublishedCandidate": s.i.handleCatalogSeal,
	}
	out := map[string]workflowhost.Operation{}
	for name, call := range handlers {
		out[name] = func(ctx context.Context, args map[string]any) (any, error) {
			if _, err := s.gate(ctx); err != nil {
				return nil, err
			}
			r := s.lease.record
			if asString(args["candidateId"]) != r.CandidateID {
				return nil, errors.New("publication recipe selected a different candidate")
			}
			if name == "releaseCreateCandidateDraft" || name == "releasePublishCandidate" || name == "releasePromoteCandidateDraft" || name == "releaseSealPublishedCandidate" {
				if !r.PublicationStarted || asString(args["approvalId"]) != r.ApprovalID {
					return nil, errors.New("publication recipe requires the original separate approval")
				}
			}
			rows, err := call(ctx, args, 0)
			if err != nil {
				return nil, err
			}
			if len(rows) != 1 || len(rows[0].Payload) > 4<<20 {
				return nil, errors.New("publication operation returned an invalid bounded record")
			}
			var result any
			if err := json.Unmarshal(rows[0].Payload, &result); err != nil {
				return nil, err
			}
			return result, nil
		}
	}
	return out
}
