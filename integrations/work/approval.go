package work

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/safety"
	work "github.com/znasllc-io/memql/component/work"
)

// approval.go -- the human gate, from both ends (design record section D,
// "approval", and section E, "Govern").
//
//	handleDecideApproval  a person decides, and the run parked on it resumes
//	Sink                  the safety gate's Ask, as a v1:work:approval row
//
// ONE CONCEPT, SIX KINDS, ONE INBOX. v1:work:approval replaces the plan's
// feedbackRequest/feedbackResponse fields, the canvas cards the planner
// emitted onto a cognition space, AND component/safety/approval's own
// v1:safety:approvalRequest sink. That third replacement is why Sink lives
// here: a human gate raised by the safety classifier and a human gate raised
// by the loop were two inboxes, and in an engine-only cluster the canvas half
// was not registered at all -- so every planner approval was already invisible.

const approvalConcept = "v1:work:approval"

// handleDecideApproval records a decision and resumes the run.
func (i *Integration) handleDecideApproval(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ac, err := requirePrincipal(ctx)
	if err != nil {
		return nil, err
	}
	approvalId := argString(args, "approvalId")
	if approvalId == "" {
		return nil, fmt.Errorf("work: decideApproval needs an approvalId")
	}
	decision := argString(args, "decision")
	switch decision {
	case "approved", "rejected", "answered":
	default:
		return nil, fmt.Errorf("work: decision %q is not approved, rejected or answered", decision)
	}

	st := i.store()

	// Resolve the approval through the CALLER's own pending list. That list
	// filters ownerUserId==actor.userId && decision=="", so finding the row
	// in it establishes three things at once: the approval exists, it is
	// this caller's, and nobody has decided it yet. The id-addressed read
	// (workApprovalById) is @serverOnly and would establish only the first.
	pending, err := st.pendingApprovalsForOwner(ctx)
	if err != nil {
		return nil, err
	}
	var approval map[string]any
	for _, row := range pending {
		if rowString(row, "id") == approvalId {
			approval = row
			break
		}
	}
	if approval == nil {
		return nil, fmt.Errorf("work: no pending approval %q is readable by this caller -- it may already have been decided, or it belongs to somebody else", approvalId)
	}

	kind := rowString(approval, "kind")
	storedHash := rowString(approval, "artifactHash")
	runId := rowString(approval, "runId")
	owner := rowString(approval, "ownerUserId")

	// THE ARTIFACT-HASH GATE. An approval is a decision about a specific
	// thing -- this command, this patch, this draft -- and it never carries
	// to a modified one.
	//
	// IT IS ASKED ONLY OF A DECISION THAT WOULD LET THE WORK PROCEED.
	// work.ResumeAllowed answers "may the run act on this", and for a
	// REJECTION the answer is no by definition -- which is not a reason to
	// refuse RECORDING the rejection. Running the gate over `rejected` would
	// make a person's "no" unrecordable and leave the approval pending
	// forever, waiting for a decision the system had already refused to take.
	// So the hash is compared for approved and answered, and a rejection is
	// recorded unconditionally: saying no to a modified artifact is still no.
	//
	// currentArtifactHash below explains why the recompute is conditional on
	// KIND; ResumeAllowed is the one implementation of the comparison, and
	// its typed error is what the refusal re-raises verbatim so a caller can
	// tell "the artifact changed" from anything else.
	if decision != "rejected" {
		current := currentArtifactHash(kind, storedHash, rowMap(approval, "subject"))
		if ok, rerr := work.ResumeAllowed(storedHash, current, decision); !ok {
			if errors.Is(rerr, work.ErrArtifactChanged) {
				return nil, fmt.Errorf("work: %w -- approve it again against what it is now", rerr)
			}
			return nil, fmt.Errorf("work: %w", rerr)
		}
	}

	now := i.clock().UTC()
	answer := argMap(args, "answer")
	if decision == "answered" && len(answer) == 0 {
		return nil, fmt.Errorf("work: an `answered` decision needs an answer -- a feedback approval with no answer resumes a run with nothing to act on")
	}

	// Borrowed authority on both writes: the write guard ignores the
	// clusterOwner arm, so an owned row is written AS its owner. The owner
	// came off the approval row this caller already read under their own
	// actor.
	writeCtx := ownerActor(ctx, owner)
	if err := st.decideApprovalRow(writeCtx, approvalId, decision, strings.TrimSpace(ac.UserId), now, answer); err != nil {
		return nil, err
	}

	resumed, err := i.resumeParkedRun(writeCtx, runId, approvalId, decision, now)
	if err != nil {
		// The DECISION landed. Failing the whole call now would tell the
		// caller their answer was not recorded when it was, and a retry
		// would find the approval no longer pending and refuse. So the
		// resume failure is reported beside the recorded decision instead.
		i.log().Warn("work: the decision was recorded but the run could not be resumed",
			"component", "work.approval", "approval", approvalId, "run", runId, "err", err)
		return i.resultNode(map[string]any{
			"approvalId":  approvalId,
			"runId":       runId,
			"decision":    decision,
			"runResumed":  false,
			"resumeError": err.Error(),
		}), nil
	}

	return i.resultNode(map[string]any{
		"approvalId": approvalId,
		"runId":      runId,
		"decision":   decision,
		"runResumed": resumed,
	}), nil
}

// currentArtifactHash answers "what does the thing being approved hash to
// NOW", and the answer depends on where the stored hash came from.
//
//   - For kinds whose hash is DERIVED from the subject (budget, feedback, and
//     a planReview with no explicit hash), recomputing over the stored subject
//     is the check: a subject edited since the approval was raised hashes
//     differently and the resume refuses.
//
//   - For `sideEffect` the stored hash is safety.ApprovalCorrelationKey over
//     the redacted DESCRIPTOR, which is not a function of the stored subject
//     and cannot be recomputed from a row. It is passed through unchanged, and
//     the modified-artifact protection for that kind lives where the artifact
//     actually is: the NEXT dispatch of a changed command computes a DIFFERENT
//     correlation key, finds no approved row under it, and raises a fresh
//     pending approval. Approving one command can therefore never run another
//     -- which is the guarantee, arrived at by construction rather than by a
//     comparison this function is not in a position to make.
//
// Returning storedHash for that case is deliberate and is what makes the
// distinction visible: the alternative -- recomputing over the subject for
// every kind -- would make every sideEffect decision fail as "changed", every
// time, and the safety gate's inbox would be undecidable.
func currentArtifactHash(kind, storedHash string, subject map[string]any) string {
	switch kind {
	case work.ApprovalKindSideEffect, work.ApprovalKindScopeElevation, work.ApprovalKindSkillMint:
		return storedHash
	}
	if len(subject) == 0 {
		// No subject to recompute over. Treating an absent subject as a
		// mismatch would refuse every approval raised before the subject was
		// recorded; treating it as a match is what the stored hash already
		// asserts.
		return storedHash
	}
	return work.ArtifactHash(subject)
}

// resumeParkedRun takes the run off its wait, or stops it.
//
// Three states, and the third is the one worth stating:
//
//   - approved / answered -> the run returns to `running` with waitingOn
//     cleared. Re-dispatch is the executor's; this records the transition so
//     the run is claimable again and the sweep stops treating it as parked.
//   - rejected -> the run FAILS with errorCode approval_rejected. Leaving it
//     `waiting` would park it forever: a rejected approval has no timer, so
//     the timer sweep never resumes it and the abandoned sweep deliberately
//     leaves waiting runs alone. Someone said no, and the run stops.
//   - the run is not actually parked on THIS approval -> nothing is written.
//     A stale decision must not un-park a run that has since moved on.
//
// When the A2 executor lands, `rejected` becomes a failed STEP with the
// `human` symptom and the loop decides what the run does about it; the run's
// terminal status here is the honest executor-free reading of "deny fails the
// step as human" (design section E).
func (i *Integration) resumeParkedRun(ctx context.Context, runId, approvalId, decision string, now time.Time) (bool, error) {
	if runId == "" {
		return false, nil
	}
	st := i.store()
	run, err := st.runForOwner(ctx, runId)
	if err != nil {
		return false, err
	}
	if run == nil {
		return false, fmt.Errorf("work: run %q is not readable", runId)
	}
	if rowString(run, "status") != runStatusWaiting {
		return false, nil
	}
	// The wait must name THIS approval. waitingOn.subject is the id the run
	// parked on; a run waiting on something else is not this decision's to
	// release.
	if waiting := rowMap(run, "waitingOn"); waiting != nil {
		if subject, ok := waiting["subject"].(string); ok && trim(subject) != "" && trim(subject) != approvalId {
			return false, nil
		}
	}

	fields := map[string]any{
		// An EMPTY OBJECT, not an omitted argument: updateWorkRun is a
		// read-merge, so leaving waitingOn out would keep the stale wait and
		// the run would read as parked while running.
		"waitingOn": map[string]any{},
	}
	if decision == "rejected" {
		fields["status"] = runStatusFailed
		fields["errorCode"] = "approval_rejected"
		fields["errorMessage"] = "a person rejected the approval this run was parked on"
		fields["finishedAt"] = rfc(now)
	} else {
		fields["status"] = runStatusRunning
		fields["heartbeatAt"] = rfc(now)
	}
	if err := st.updateRun(ctx, runId, fields); err != nil {
		return false, err
	}
	return true, nil
}

// ---------------------------------------------------------------------------
// The safety gate's sink
// ---------------------------------------------------------------------------

// Sink is the safety.ApprovalSink that writes v1:work:approval rows.
//
// It REPLACES component/safety/approval's v1:safety:approvalRequest sink
// (design section D: the approval concept "replaces the plan's feedbackRequest
// and feedbackResponse, the canvas cards and the safety gate's ask sink").
// Wire it in app/safety_llm.go's buildSafetyApprovalSink.
//
// # The contract this must not break
//
// safety.ApprovalSink says: errors are recovered, logged internally, and
// returned as Unconfigured -- a dead approval sink must not crash live
// traffic. Every failure path below therefore answers Unconfigured, which
// leaves the Gate's pre-approval Ask refusal exactly as it was.
//
// # Why the correlation key IS the artifact hash
//
// safety.ApprovalCorrelationKey is a stable digest of the descriptor's
// identifying surface over the REDACTED payload, and it is already the key the
// gate uses to collapse retries of the same action onto one row. Reusing it as
// artifactHash means a MODIFIED command hashes differently, finds no approved
// row, and raises a fresh approval -- which is the artifact-hash guarantee,
// obtained by construction. Computing a second, different hash here would give
// the row two identities and make "is this the thing I approved" answerable
// two ways.
type Sink struct {
	integ  *Integration
	logger *slog.Logger
	ttl    time.Duration
}

var _ safety.ApprovalSink = (*Sink)(nil)

// SinkOptions configure NewSink.
type SinkOptions struct {
	Logger *slog.Logger
	// TTL is how long an approved row acts as a bypass. Zero takes
	// DefaultApprovalTTL.
	TTL time.Duration
}

// DefaultApprovalTTL matches the sink this replaces: long enough that a person
// who approves in the morning can run the same workflow that afternoon, short
// enough that a forgotten approval does not outlast the awareness of why it
// was given.
const DefaultApprovalTTL = 24 * time.Hour

// NewSink builds the work-backed approval sink.
func (i *Integration) NewSink(opts SinkOptions) *Sink {
	logger := opts.Logger
	if logger == nil {
		logger = i.log()
	}
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = DefaultApprovalTTL
	}
	return &Sink{integ: i, logger: logger, ttl: ttl}
}

// Check implements safety.ApprovalSink.
func (s *Sink) Check(ctx context.Context, desc safety.ActionDescriptor, cls safety.Classification) safety.ApprovalVerdict {
	if s == nil || s.integ == nil || s.integ.engine == nil {
		return safety.ApprovalVerdict{State: safety.ApprovalStateUnconfigured}
	}
	key := safety.ApprovalCorrelationKey(desc)

	// The run is the descriptor's PlanID, which is what every safety caller
	// populates today and is the field the work spine's run replaces.
	//
	// A blank one answers Unconfigured rather than inventing a run: runId is
	// required on v1:work:approval, the row would be refused, and a refused
	// row reported as "pending" would park a step on an approval nobody can
	// ever see. Unconfigured keeps the Gate's own refusal, which is the
	// behaviour a cluster with no sink has always had.
	runId := trim(desc.Caller.PlanID)
	if runId == "" {
		s.logger.Warn("work: a side-effect approval has no run to attach to; the safety gate keeps its own refusal",
			"component", "work.approval_sink", "surface", string(desc.Surface), "action", string(desc.Action))
		return safety.ApprovalVerdict{State: safety.ApprovalStateUnconfigured}
	}
	owner := trim(desc.Caller.OwnerUserID)

	// Borrowed authority on BOTH the read and the write. The read gate has
	// no internal-origin bypass, so an unstamped lookup would answer zero
	// rows -- and a sink that can never find an existing approval raises a
	// new one on every dispatch, which turns one decision into an unbounded
	// inbox.
	actorCtx := ownerActor(ctx, owner)

	existing, err := s.integ.store().pendingApprovalsForOwner(actorCtx)
	if err != nil {
		s.logger.Warn("work: the approval lookup failed; falling back to unconfigured",
			"component", "work.approval_sink", "correlationKey", key, "err", err)
		return safety.ApprovalVerdict{State: safety.ApprovalStateUnconfigured}
	}
	for _, row := range existing {
		if rowString(row, "artifactHash") != key {
			continue
		}
		// workApprovalsForOwner returns only undecided rows, so a hit here
		// is a pending one. Reuse its id rather than raising a duplicate.
		return safety.ApprovalVerdict{
			State:             safety.ApprovalStatePending,
			ApprovalRequestID: rowString(row, "id"),
		}
	}

	now := s.integ.clock().UTC()
	approvalId := newRowId(approvalConcept)
	req := work.SideEffectApproval(runId, trim(desc.Caller.TaskID), key,
		evidenceFrom(cls), subjectFrom(desc), now, s.ttl)

	if err := s.integ.store().createApprovalRow(actorCtx, approvalSeed{
		ApprovalId:   approvalId,
		RunId:        req.RunId,
		StepKey:      req.StepKey,
		Kind:         req.Kind,
		Subject:      req.Subject,
		ArtifactHash: req.ArtifactHash,
		Evidence:     evidenceMap(req.Evidence),
		RequestedAt:  req.RequestedAt,
		ExpiresAt:    req.ExpiresAt,
	}); err != nil {
		s.logger.Warn("work: the approval could not be raised; falling back to unconfigured",
			"component", "work.approval_sink", "correlationKey", key, "err", err)
		return safety.ApprovalVerdict{State: safety.ApprovalStateUnconfigured}
	}
	return safety.ApprovalVerdict{State: safety.ApprovalStatePending, ApprovalRequestID: approvalId}
}

// evidenceFrom carries the classifier's verdict onto the row, in the shape
// v1:work:approval.evidence declares: {tier, reason, ruleId, source}. A person
// deciding a gate needs to know WHY it was raised and whether a rule or a
// model said so, because the two have different costs and different trust.
func evidenceFrom(cls safety.Classification) work.Evidence {
	return work.Evidence{
		Tier:   cls.Tier.String(),
		Reason: cls.Reason,
		RuleId: cls.RuleID,
		Source: string(cls.Source),
	}
}

func evidenceMap(e work.Evidence) map[string]any {
	return map[string]any{
		"tier":   e.Tier,
		"reason": e.Reason,
		"ruleId": e.RuleId,
		"source": e.Source,
	}
}

// subjectFrom renders what is being approved, from the REDACTED payload.
//
// Redacted, always: the subject is shown to a person and stored in the graph,
// and the raw payload can carry a credential. safety.RedactedPayload is the
// one place that decision is made, and calling it here rather than reaching
// for desc.Payload is the whole of the difference.
func subjectFrom(desc safety.ActionDescriptor) map[string]any {
	p := safety.RedactedPayload(desc.Payload)
	subject := map[string]any{
		"surface": string(desc.Surface),
		"action":  string(desc.Action),
	}
	if p.Command != "" {
		subject["command"] = p.Command
	}
	if p.URL != "" {
		subject["url"] = p.URL
	}
	if p.Method != "" {
		subject["method"] = p.Method
	}
	if p.ToolName != "" {
		subject["tool"] = p.ToolName
	}
	if len(p.Paths) > 0 {
		subject["paths"] = p.Paths
	}
	if len(p.Args) > 0 {
		subject["args"] = p.Args
	}
	return subject
}
