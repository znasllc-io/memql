package planner

// work_heal.go -- the heal arm of the work spine's miss path (epic
// memql#4966, design record section E "Miss", decision D5).
//
// THIS IS WIRING, NOT A NEW LOOP. component/healing already proposes the
// four typed patches (relativize the literal, add the precondition,
// insert the guard, rebind the param), and the executor has emitted
// `healing.precondition.missed` on every precondition miss since epic 4,
// with a broadcast routing rule carrying it across the mesh. What was
// missing is that NOTHING SUBSCRIBED TO IT: grep found the emitter, the
// routing rule and the tests, and `NewRepairLoop` had no non-test caller.
// So the mechanism was complete, never ran, and looked complete.
//
// D5 -- NEVER A SILENT EDIT. Propose does not write, by design, and this
// subscriber does not either. It carries the patches to a person as an
// approval of kind planReview, hashed over the exact patch set, so the
// person approves THESE patches and resume refuses if they changed. That
// holds even for the run's own draft template, which is the case where a
// silent edit would be most tempting and least visible.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/healing"
	"github.com/znasllc-io/memql/component/work"
	workintegration "github.com/znasllc-io/memql/integrations/work"
)

// patchProposer is the seam onto component/healing. An interface rather
// than the concrete *healing.RepairLoop so this can be tested without a
// structured-output provider -- the point under test is what happens to
// the patches, not how they were produced.
type patchProposer interface {
	Propose(ctx context.Context, miss healing.PreconditionMiss, base map[string]any) ([]healing.Patch, error)
}

// approvalRaiser is the narrow write seam. The planner does NOT write the
// approval row itself: createWorkApproval is @serverOnly, an unstamped write
// is refused with one WARN nothing above it hears, and stamping is allowlisted
// per PACKAGE -- integrations/planner is large and request-derived and must
// not be admitted. integrations/work holds the allowlist entry and exactly one
// stamping site, so the row is raised there and this delegates.
type approvalRaiser interface {
	RaiseApproval(ctx context.Context, ownerUserId string, a workintegration.ApprovalSeed) (string, error)
}

// WorkHealer turns a precondition miss into a planReview approval.
type WorkHealer struct {
	proposer patchProposer
	raiser   approvalRaiser
	loop     *PlannerAgentLoop
	// ttl is how long a proposed repair stays answerable. A patch set
	// proposed against a world that has since moved is worse than none,
	// and an approval nobody answered should lapse rather than sit.
	ttl time.Duration
}

// NewWorkHealer builds the subscriber. A nil proposer or writer yields a
// nil healer, and every method on a nil healer is a no-op, so the caller
// can wire it unconditionally.
func NewWorkHealer(proposer patchProposer, raiser approvalRaiser, loop *PlannerAgentLoop, ttl time.Duration) *WorkHealer {
	if proposer == nil || raiser == nil {
		return nil
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &WorkHealer{proposer: proposer, raiser: raiser, loop: loop, ttl: ttl}
}

// HandlePreconditionMissed is the events.TopicPreconditionMissed handler.
func (h *WorkHealer) HandlePreconditionMissed(ev events.Event) {
	if h == nil {
		return
	}
	// The bus hands this to us on its own goroutine with no deadline; a
	// proposal is one model call, so bound it rather than letting a hung
	// provider hold a subscriber slot forever.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := h.handle(ctx, ev); err != nil && h.loop != nil && h.loop.logger != nil {
		h.loop.logger.Warn("work heal: precondition miss not carried to an approval",
			"topic", ev.Topic, "error", err)
	}
}

func (h *WorkHealer) handle(ctx context.Context, ev events.Event) error {
	payload := ev.Payload
	if payload == nil {
		return fmt.Errorf("precondition miss carried no payload")
	}
	miss := healing.MissFromEventPayload(payload)
	if strings.TrimSpace(miss.AutomationName) == "" {
		return fmt.Errorf("precondition miss names no automation")
	}
	runId := getString(payload, "executionId")
	if runId == "" {
		// Every miss the executor emits carries executionId, and without
		// one there is no run to park -- an approval with no run is a
		// question nobody can answer, so refuse rather than orphan it.
		return fmt.Errorf("precondition miss for %s carries no executionId, so there is no run to park", miss.AutomationName)
	}

	patches, err := h.proposer.Propose(ctx, miss, nil)
	if err != nil {
		return fmt.Errorf("propose patches for %s: %w", miss.AutomationName, err)
	}
	if len(patches) == 0 {
		// A spine construct, or nothing the model could suggest. Not an
		// error: the run stays failed with its environment symptom, and a
		// person sees that rather than an empty approval.
		return nil
	}

	subject := map[string]any{
		"automationName": miss.AutomationName,
		"preconditionId": miss.PreconditionId,
		"check":          miss.Check,
		"literal":        miss.Literal,
		"description":    miss.Description,
		"patches":        patchObjects(patches),
	}
	req := work.PlanReviewApproval(runId, miss.PreconditionId, "", patchObjects(patches),
		work.Evidence{
			Tier:   "environment",
			Reason: healReason(miss),
			RuleId: "environment.literal",
			Source: work.EvidenceSourceRules,
		}, time.Now().UTC(), h.ttl)
	// The hash covers the WHOLE subject rather than the patches alone, so
	// approving a repair to one precondition cannot be replayed against a
	// different one that happens to want the same edits.
	req.Subject = subject
	req.ArtifactHash = work.ArtifactHash(subject)

	return h.writeApproval(ctx, req)
}

func healReason(miss healing.PreconditionMiss) string {
	if miss.Description != "" {
		return miss.Description
	}
	if miss.Literal != "" {
		return fmt.Sprintf("precondition %s does not hold here: %s", miss.PreconditionId, miss.Literal)
	}
	return fmt.Sprintf("precondition %s does not hold on this machine", miss.PreconditionId)
}

// patchObjects renders the typed patches for the approval subject. The
// person deciding sees the patches themselves, not a count: "3 proposed
// repairs" is not something anyone can approve.
func patchObjects(patches []healing.Patch) []map[string]any {
	out := make([]map[string]any, 0, len(patches))
	for _, p := range patches {
		b, err := json.Marshal(p)
		if err != nil {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			continue
		}
		out = append(out, m)
	}
	return out
}

func (h *WorkHealer) writeApproval(ctx context.Context, req work.ApprovalRequest) error {
	// THE OWNER IS DELIBERATELY EMPTY, and it is right for the only path that
	// reaches here today. `healing.precondition.missed` is emitted by the
	// automation executor, whose runs are the DEPLOYMENT's -- their
	// ownerUserId is present-and-empty -- so an approval raised under no
	// actor lands on the same row tier the run itself did, readable through
	// the composite tier's cluster-owner escape.
	//
	// A GOAL-OWNED run reaching this path would put its approval on the
	// deployment rather than on the person, which is visible to an operator
	// and not to them. The miss event carries no owner, so closing that needs
	// the owner threaded onto the event or looked up from the run -- named
	// here rather than papered over, because the failure is quiet.
	_, err := h.raiser.RaiseApproval(ctx, "", workintegration.ApprovalSeed{
		RunId:        req.RunId,
		StepKey:      req.StepKey,
		Kind:         req.Kind,
		Subject:      req.Subject,
		ArtifactHash: req.ArtifactHash,
		Question:     req.Question,
		Options:      req.Options,
		Evidence: map[string]any{
			"tier":   req.Evidence.Tier,
			"reason": req.Evidence.Reason,
			"ruleId": req.Evidence.RuleId,
			"source": req.Evidence.Source,
		},
		RequestedAt: req.RequestedAt,
		ExpiresAt:   req.ExpiresAt,
	})
	return err
}
