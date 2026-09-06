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
	"github.com/znasllc-io/memql/core/id"
)

// patchProposer is the seam onto component/healing. An interface rather
// than the concrete *healing.RepairLoop so this can be tested without a
// structured-output provider -- the point under test is what happens to
// the patches, not how they were produced.
type patchProposer interface {
	Propose(ctx context.Context, miss healing.PreconditionMiss, base map[string]any) ([]healing.Patch, error)
}

// approvalWriter is the narrow write seam. Kept separate from the engine
// so the subscriber can be exercised against a recorder.
type approvalWriter interface {
	Execute(ctx context.Context, query string) (any, error)
}

// WorkHealer turns a precondition miss into a planReview approval.
type WorkHealer struct {
	proposer patchProposer
	writer   approvalWriter
	loop     *PlannerAgentLoop
	// ttl is how long a proposed repair stays answerable. A patch set
	// proposed against a world that has since moved is worse than none,
	// and an approval nobody answered should lapse rather than sit.
	ttl time.Duration
}

// NewWorkHealer builds the subscriber. A nil proposer or writer yields a
// nil healer, and every method on a nil healer is a no-op, so the caller
// can wire it unconditionally.
func NewWorkHealer(proposer patchProposer, writer approvalWriter, loop *PlannerAgentLoop, ttl time.Duration) *WorkHealer {
	if proposer == nil || writer == nil {
		return nil
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &WorkHealer{proposer: proposer, writer: writer, loop: loop, ttl: ttl}
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
	args := map[string]any{
		"approvalId":   id.NewShortId(),
		"runId":        req.RunId,
		"stepKey":      req.StepKey,
		"kind":         req.Kind,
		"subject":      req.Subject,
		"artifactHash": req.ArtifactHash,
		"evidence": map[string]any{
			"tier":   req.Evidence.Tier,
			"reason": req.Evidence.Reason,
			"ruleId": req.Evidence.RuleId,
			"source": req.Evidence.Source,
		},
		"requestedAt": req.RequestedAt.Format(time.RFC3339Nano),
	}
	if !req.ExpiresAt.IsZero() {
		args["expiresAt"] = req.ExpiresAt.Format(time.RFC3339Nano)
	}
	// Named-args form, and every nil argument DROPPED rather than rendered.
	// Both halves are load-bearing and both fail silently: the parser refuses
	// `name({...})` outright (#2335), and an optional `object` field given
	// `null` fails the concept's type check, which refuses the WHOLE row --
	// so an approval nobody can see reads as "the healer never fired".
	_, err := h.writer.Execute(ctx, "createWorkApproval("+encodeArgs(dropNil(args))+")")
	return err
}

// dropNil removes absent values so they are never rendered as `null`.
// json.Marshal(nil) is "null", and a concept's optional object field
// refuses it, taking the whole insert with it.
func dropNil(args map[string]any) map[string]any {
	out := make(map[string]any, len(args))
	for k, v := range args {
		if v == nil {
			continue
		}
		if s, ok := v.(string); ok && s == "" {
			// An empty string is a real value for a string field, but for
			// the optional ones here it is indistinguishable from absent
			// and costs nothing to omit.
			continue
		}
		out[k] = v
	}
	return out
}
