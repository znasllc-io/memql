package release

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// releasePublicationOperationLimit is one recipe operation's watchdog, the action's
// only time limit. It sits above the hour draft and publish give themselves, so
// their own bound reports first. A breach ends the action as uncertain; receipts
// already recorded keep the next publish from repeating that work.
const releasePublicationOperationLimit = 70 * time.Minute

func (s *releaseBuildScope) publish(ctx context.Context, approval string) error {
	if err := s.openPublication(ctx, approval); err != nil {
		return err
	}
	r := &s.lease.record
	_, err := workflowhost.RunSnapshot(ctx, r.Selection.Publication, buildPublicationContract, map[string]any{"candidateId": r.CandidateID, "approvalId": approval}, workflowhost.Options{Logger: s.i.logger, Operations: s.publicationOperations()})
	if err != nil {
		return err
	}
	return s.closePublication(ctx, approval)
}

func (s *releaseBuildScope) promotionStarted(ctx context.Context) bool {
	drafts, err := s.lease.ledger.drafts(ctx, s.lease.record.CandidateID)
	return err == nil && slices.ContainsFunc(drafts, func(d candidateDraftRecord) bool { return d.State == "promoting" })
}

func (s *releaseBuildScope) openPublication(ctx context.Context, approval string) error {
	ctx, cancel := context.WithTimeout(ctx, buildActionLimit)
	defer cancel()
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
	r.ReasonCode, r.Reason = "", ""
	return s.lease.save(ctx)
}

func (s *releaseBuildScope) closePublication(ctx context.Context, approval string) error {
	ctx, cancel := context.WithTimeout(ctx, buildActionLimit)
	defer cancel()
	r := &s.lease.record
	c, err := s.lease.ledger.get(ctx, r.CandidateID)
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

// publicationAction is what one publish call shares across its recipe's operations.
// A recipe calls each effect once per target, and captured recipes cannot be changed
// after the fact, so this is where the cost of a many-file release is kept linear. It
// dies with the call and survives nothing; a retry on any replica starts over from
// the durable journals. Direct owner capabilities never see it, so each of those
// keeps its full, fresh verification.
type publicationAction struct {
	scope    *releaseBuildScope
	mu       sync.Mutex
	prepared *candidatePreparer
}

// preparer configures once for the private work of the action. fresh starts over,
// re-reading the run facts and forgetting the verification, for an effect that goes
// public: the one kind of effect that cannot be taken back must not rest on what the
// call read at its start. Without this a changed run record went unnoticed until
// sealing refused it, after the release was already public.
//
// Each operation inside an effect runs the preparer's authority check first, so the
// lease and the stored owner are also re-checked there. One promotion can read bytes
// for minutes before its claim and its PATCH, which is too long for the recipe-level
// gate alone.
func (a *publicationAction) preparer(ctx context.Context, candidateID string, fresh bool) (*candidatePreparer, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.prepared != nil && !fresh {
		return a.prepared, nil
	}
	p, err := a.scope.i.configuredCandidateFor(ctx, candidateID)
	if err != nil {
		return nil, err
	}
	configured := p.authority
	p.authority = func(ctx context.Context) error {
		if configured != nil {
			if err := configured(ctx); err != nil {
				return err
			}
		}
		return a.scope.held(ctx)
	}
	p.verified = &candidateVerification{}
	a.prepared = p
	return p, nil
}

// draft answers from the durable journal once the target's group has settled. One
// release is shared by all its targets, so the first call settles all of them, and
// a recipe that read the journal once at its top calls again for each sibling. A
// repeated promotion read every asset's bytes twice more, per file.
//
// Settled means no further effect of this kind is possible: creation is done from
// ready onward, promotion only at published, whose verified receipt it returns.
// A promoting draft is not settled and goes on to reconcile natively. The caller's
// approval is checked before this runs, and the upload and the promotion that
// follow still verify the remote draft themselves.
func (a *publicationAction) draft(promote bool) func(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error) {
	return func(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
		key, target := asString(args["candidateId"]), asString(args["targetId"])
		drafts, err := a.scope.lease.ledger.drafts(ctx, key)
		if err != nil {
			return nil, err
		}
		for _, rec := range drafts {
			if slices.Contains(rec.Targets, target) && (rec.State == "published" || !promote && (rec.State == "ready" || rec.State == "promoting")) {
				return resultNode("draft", rec.IntentID, rec)
			}
		}
		p, err := a.preparer(ctx, key, promote)
		if err != nil {
			return nil, err
		}
		return draftResult(ctx, p, args, promote)
	}
}

// publish reuses the action's verification only for an upload to a private draft. A
// registry or npm write is public the moment it lands, so it verifies afresh, as it
// did when every effect did.
func (a *publicationAction) publish(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	key := asString(args["candidateId"])
	p, err := a.preparer(ctx, key, false)
	if err != nil {
		return nil, err
	}
	if _, upload := p.targetReader.files[asString(args["targetId"])]; !upload {
		if p, err = a.preparer(ctx, key, true); err != nil {
			return nil, err
		}
	}
	return publishCandidateResult(ctx, p, args)
}

func (s *releaseBuildScope) publicationOperations() map[string]workflowhost.Operation {
	type handler func(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error)
	action := &publicationAction{scope: s}
	handlers := map[string]handler{
		"releaseGetCandidate":           s.i.handleCandidateGet,
		"releaseCandidateTargets":       s.i.handleCandidateTargets,
		"releaseCandidateDrafts":        s.i.handleCandidateDrafts,
		"releaseCandidatePublications":  s.i.handleCandidateEffects,
		"releaseCreateCandidateDraft":   action.draft(false),
		"releasePublishCandidate":       action.publish,
		"releasePromoteCandidateDraft":  action.draft(true),
		"releaseSealPublishedCandidate": s.i.handleCatalogSeal,
	}
	out := map[string]workflowhost.Operation{}
	for name, call := range handlers {
		out[name] = func(ctx context.Context, args map[string]any) (any, error) {
			ctx, cancel := context.WithTimeout(ctx, releasePublicationOperationLimit)
			defer cancel()
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
