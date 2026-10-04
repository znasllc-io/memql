package work

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/safety"
	work "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
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
	if run, running := common.RunFromContext(ctx); ac.Synthetic || ac.Unranked || (running && run.RunId != "") {
		return nil, fmt.Errorf("work: an approval requires a person's interactive decision, not a running job")
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

	if i.decisionGate == nil {
		return nil, fmt.Errorf("work: approval coordination is unavailable")
	}
	release, err := i.decisionGate(ctx, approvalId)
	if err != nil {
		return nil, err
	}
	defer release()
	ctx = memql.ContextWithFreshRead(ctx)
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
		if memql.BareShortId(rowString(row, "id")) == memql.BareShortId(approvalId) {
			approval = row
			break
		}
	}
	if approval == nil {
		if recovered, err := i.recoverReviewDecision(ctx, approvalId, decision); err != nil {
			return nil, err
		} else if recovered != nil {
			return i.resultNode(recovered), nil
		}
		return nil, fmt.Errorf("work: no pending approval %q is readable by this caller -- it may already have been decided, or it belongs to somebody else", approvalId)
	}

	kind := rowString(approval, "kind")
	if kind == work.ApprovalKindPlanReview && rowString(rowMap(approval, "subject"), "reviewType") != "" && decision == "answered" {
		return nil, fmt.Errorf("work: this draft request needs an approval or a rejection")
	}
	storedHash := rowString(approval, "artifactHash")
	runId := rowString(approval, "runId")
	owner := rowString(approval, "ownerUserId")

	// A PROMOTION TAKES TWO DECISIONS, its two options: approved moves the
	// procedure from shadow to canary and rejected keeps it in shadow
	// (integrations/procedure's DecidePromotion). An `answered` one would be
	// recorded -- spending the approval -- and then move nothing, leaving the
	// construct waiting on an approval nobody can decide again. So it is
	// refused before anything is read or written.
	if kind == work.ApprovalKindProcedurePromotion && decision == "answered" {
		return nil, fmt.Errorf("work: a procedure promotion is decided approved (the procedure moves from shadow to canary) or rejected (it stays in shadow); it takes no answer")
	}

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
		if err := i.validatePlanReview(ctx, kind, rowMap(approval, "subject")); err != nil {
			return nil, err
		}
		current := currentArtifactHash(kind, storedHash, rowMap(approval, "subject"))
		if current != storedHash && work.IsFailureQuestion(approval["options"]) && legacyFailureHash(storedHash, rowMap(approval, "subject"), runId) {
			// A failure question raised before memql#5664, whose hash was
			// over another map (work.LegacyFailureHashes): the same failure,
			// still unedited, so the decision may land.
			current = storedHash
		}
		if kind == work.ApprovalKindProcedurePromotion {
			// The one kind whose artifact is a ROW that keeps changing after
			// the approval is raised: a re-lift rewrites the construct's
			// source, template and preconditions, and D15 says a changed
			// construct is a new candidate. So "what does it hash to now" is
			// the construct's CURRENT procedureHash, read here -- recomputing
			// over the stored subject would only prove the subject was not
			// edited, which nobody does.
			current, err = st.currentProcedureHash(ctx, rowMap(approval, "subject"))
			if err != nil {
				return nil, err
			}
		}
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
	if err := st.decideApprovalRow(writeCtx, approvalId, decision, strings.TrimSpace(ac.UserId), now, answer, approvalVersionAfter(approval["createdAt"], now)); err != nil {
		return nil, err
	}

	// WHAT AN APPROVAL CAUSES, beyond releasing the run that raised it
	// (memql#5063). Approval-driven specialist training lost its trigger when
	// the planner's decision loop was deleted -- the MECHANISM survived and
	// only the gate that reached it went. This is that gate, and it sits here
	// because this is where a decision becomes an action for every other kind
	// too.
	//
	// AFTER the decision is recorded and BEFORE the resume, deliberately: the
	// idempotency is the approval's own pending-list resolution above, so the
	// work must not start until the row that guarantees "exactly once" has
	// been written. Ordering it after the resume instead would leave a window
	// in which a crash loses the training with the approval already spent.
	var trainingGoalId, trainingRunId string
	var trainingEscalated bool
	if decision == "approved" {
		trainingGoalId, trainingRunId, trainingEscalated =
			i.startApprovedTraining(writeCtx, owner, runId, kind, rowMap(approval, "subject"))
	}

	if kind == work.ApprovalKindProcedurePromotion {
		// NOTHING IS PARKED ON A PROMOTION. Its runId names the SHADOW run
		// whose comparison met the threshold -- a finished run, kept on the
		// approval so a person can open the evidence. resumeParkedRun would
		// leave a succeeded run alone anyway (it acts only on `waiting`), but
		// its rejection branch FAILS the run it names, and a person saying
		// "not yet" must never rewrite the evidence they were shown. The
		// ladder move itself is integrations/procedure's: the
		// onProcedurePromotionDecided automation reads this decision and
		// moves shadow to canary exactly once.
		return i.resultNode(map[string]any{
			"approvalId": approvalId,
			"runId":      runId,
			"decision":   decision,
			"runResumed": false,
		}), nil
	}

	// AN ANSWER CAN BE A STOP. A failure-path question offers Retry and
	// Abandon (work.FailureApproval) and is decided `answered`, the choice
	// riding the answer -- so reading the decision alone would carry on with
	// work the person had just chosen to stop.
	runDecision := decision
	if decision == "answered" && work.AnswerAbandons(approval["options"], answer) {
		runDecision = decisionAbandoned
	}
	var retry *failureRetry
	if work.IsFailureQuestion(approval["options"]) {
		retry = &failureRetry{stepKey: rowString(approval, "stepKey"), decidedBy: strings.TrimSpace(ac.UserId)}
	}
	resumed, err := i.resumeParkedRun(writeCtx, runId, approvalId, runDecision, retry, now)
	if err != nil {
		// The DECISION landed. Failing the whole call now would tell the
		// caller their answer was not recorded when it was, and a retry
		// would find the approval no longer pending and refuse. So the
		// resume failure is reported beside the recorded decision instead.
		i.log().Warn("work: the decision was recorded but the run could not be resumed",
			"component", "work.approval", "approval", approvalId, "run", runId, "err", err)
		return withTrainingOutcome(map[string]any{
			"approvalId":  approvalId,
			"runId":       runId,
			"decision":    decision,
			"runResumed":  false,
			"resumeError": err.Error(),
		}, trainingGoalId, trainingRunId, trainingEscalated, i), nil
	}

	return withTrainingOutcome(map[string]any{
		"approvalId": approvalId,
		"runId":      runId,
		"decision":   decision,
		"runResumed": resumed,
	}, trainingGoalId, trainingRunId, trainingEscalated, i), nil
}

// withTrainingOutcome adds the training keys ONLY when there was training to
// report. An absent key and a zero are different answers: a caller deciding
// whether to show "training started" must not read an empty string on every
// budget approval as "training failed".
func withTrainingOutcome(payload map[string]any, goalId, runId string, escalated bool, i *Integration) []memorynodes.MemoryNode {
	if goalId != "" {
		payload["trainingGoalId"] = goalId
		payload["trainingRunId"] = runId
	}
	if escalated {
		payload["trainingEscalated"] = true
	}
	return i.resultNode(payload)
}

// currentArtifactHash answers "what does the thing being approved hash to
// NOW". The rule is component/work's CurrentArtifactHash, beside the builders
// every approval is raised with, so the raise side and this side cannot drift
// apart again: the failure path's approvals were raised with a hash over one
// map and checked here against another, and every approve and every answer on
// them was refused as "artifact changed" (memql#5664).
//
// `sideEffect` is the case worth restating here, because this is where it
// would be broken: its stored hash is safety.ApprovalCorrelationKey over the
// redacted DESCRIPTOR, not a function of the stored subject, so it is passed
// through, and the modified-artifact protection for it lives where the
// artifact is -- the next dispatch of a changed command computes a different
// key, finds no approved row under it, and raises a fresh approval.
// Recomputing over the subject for every kind would make every sideEffect
// decision fail as "changed" and the safety gate's inbox undecidable.
func currentArtifactHash(kind, storedHash string, subject map[string]any) string {
	return work.CurrentArtifactHash(kind, storedHash, subject)
}

// legacyFailureHash reports whether stored is the hash a failure question was
// raised with before memql#5664 (work.LegacyFailureHashes), for any spelling
// of the run's id: the executor hashed the id it ran under, and the row reads
// it back in whatever form the engine returns.
func legacyFailureHash(stored string, subject map[string]any, runId string) bool {
	bare := memql.BareShortId(runId)
	for _, h := range work.LegacyFailureHashes(subject, runId, bare, runConcept+":"+bare) {
		if h == stored {
			return true
		}
	}
	return false
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
//     leaves waiting runs alone. Someone said no, and the run stops. An
//     answer choosing Abandon (decisionAbandoned) is the same no, said
//     through a question's options, and stops the run the same way.
//   - the run is not actually parked on THIS approval -> nothing is written.
//     A stale decision must not un-park a run that has since moved on.
//
// When the A2 executor lands, `rejected` becomes a failed STEP with the
// `human` symptom and the loop decides what the run does about it; the run's
// terminal status here is the honest executor-free reading of "deny fails the
// step as human" (design section E).
//
// A FAILURE-PATH QUESTION'S RETRY IS A REQUEST OF ITS OWN (memql#5664). retry
// is set only for a question the failure path raised (work.IsFailureQuestion),
// and then the run is released under a re-run request on the step it failed
// at (failureRetryRequest), decided by the person who answered. Released bare,
// the run met the claim the failing execution still held -- a lease of four
// minutes from its dispatch, and a person answers within that as a rule --
// every agent lost it, no second event came, and the sweep closed the run as
// abandoned. Every other approval is released bare, as before: what it parked
// is a step waiting on a person, not a failure to run again.
func (i *Integration) resumeParkedRun(ctx context.Context, runId, approvalId, decision string, retry *failureRetry, now time.Time) (bool, error) {
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
	if rowString(run, "status") != runStatusWaiting || run["cancelRequested"] == true {
		return false, nil
	}
	// The wait must name THIS approval. waitingOn.subject is the id the run
	// parked on; a run waiting on something else is not this decision's to
	// release.
	if waiting := rowMap(run, "waitingOn"); waiting != nil {
		if subject, ok := waiting["subject"].(string); ok && trim(subject) != "" && memql.BareShortId(trim(subject)) != memql.BareShortId(approvalId) {
			return false, nil
		}
	}

	fields := map[string]any{
		// An EMPTY OBJECT, not an omitted argument: updateWorkRun is a
		// read-merge, so leaving waitingOn out would keep the stale wait and
		// the run would read as parked while running.
		"waitingOn": map[string]any{},
	}
	switch decision {
	case "rejected", decisionAbandoned:
		fields["status"] = runStatusFailed
		fields["errorCode"] = "approval_rejected"
		fields["errorMessage"] = "a person rejected the approval this run was parked on"
		if decision == decisionAbandoned {
			fields["errorMessage"] = "a person chose to abandon this run when asked about its failure"
		}
		fields["finishedAt"] = rfc(now)
	default:
		fields["status"] = runStatusRunning
		fields["heartbeatAt"] = rfc(now)
		if retry != nil {
			if request, stale, ok := i.failureRetryRequest(ctx, run, runId, *retry, now); ok {
				fields["rerun"] = request
				fields["staleSteps"] = stale
			}
		}
	}
	if err := st.updateRun(ctx, runId, fields); err != nil {
		return false, err
	}
	return true, nil
}

// failureRetry is what releasing a run off a failure-path question says beyond
// `running`: the step the run failed at, and who decided to try it again.
type failureRetry struct {
	stepKey   string
	decidedBy string
}

// failureRetryRequest is the re-run request a failure question's Retry
// releases its run under: the step the run failed at -- the top-level step a
// nested key belongs to -- and every step after it, each as its next version,
// the completed prefix served and never run again. It is the request a
// person's rerunStep writes (rerun.go), so the agents serve it on that path,
// under a once-claim of its own.
//
// ok is false for a run no request can be written for, and the run is then
// released bare, as it was before: a run that is not the executor's to run
// again (actRun.requireExecutable -- a goalless scheduler journal among them,
// which the event path never dispatches), a question naming no step, or one
// naming a step the run never recorded. A version read that fails is the same
// answer, said in the log: the decision has landed, and a run left on a
// decided question would wait for an answer nobody can give again.
func (i *Integration) failureRetryRequest(ctx context.Context, row map[string]any, runId string, retry failureRetry, now time.Time) (map[string]any, []string, bool) {
	run := actRun{row: row, id: runId, owner: rowString(row, "ownerUserId"), order: topLevelOrder(rowStringSlice(row, "stepOrder"))}
	key := topLevelStepKey(retry.stepKey)
	if key == "" || run.requireExecutable() != nil || run.requireTopLevel(key) != nil {
		return nil, nil, false
	}
	versions, err := i.readVersions(ctx, runId, run.order)
	if err != nil {
		i.log().Warn("work: the run's versions could not be read; releasing it without a request of its own",
			"component", "work.approval", "run", runId, "step", key, "err", err)
		return nil, nil, false
	}
	plan, err := work.PlanRerun(run.order, versions.byKey, key)
	if err != nil {
		return nil, nil, false
	}
	return rerunRequest(rerunReasonRerun, key, work.Override{}, plan.Versions, nil, "", retry.decidedBy, now), plan.Stale, true
}

// topLevelStepKey is the run's own step a key belongs to: the key itself, or
// the first segment of a nested one (`for_x/0/touch` belongs to `for_x`).
func topLevelStepKey(key string) string {
	key = strings.TrimSpace(key)
	if before, _, nested := strings.Cut(key, "/"); nested {
		return before
	}
	return key
}

// decisionAbandoned is not a decision anybody records -- the approval row
// says `answered` -- but what an answer choosing work.FailureAnswerAbandon
// means for the run parked on it: the run stops, as it does on a rejection.
const decisionAbandoned = "answered:abandon"

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

	// The run is the descriptor's RunID, which every safety caller populates.
	//
	// A blank one answers Unconfigured rather than inventing a run: runId is
	// required on v1:work:approval, the row would be refused, and a refused
	// row reported as "pending" would park a step on an approval nobody can
	// ever see. Unconfigured keeps the Gate's own refusal, which is the
	// behaviour a cluster with no sink has always had.
	runId := trim(desc.Caller.RunID)
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
	req := work.SideEffectApproval(runId, trim(desc.Caller.StepID), key,
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

// Decisions must sort after the proposal even when another replica's clock is
// behind its author. A shared lock alone cannot repair a non-latest version.
func approvalVersionAfter(value any, now time.Time) time.Time {
	prior, ok := value.(time.Time)
	if !ok {
		prior, _ = time.Parse(time.RFC3339Nano, fmt.Sprint(value))
	}
	return memql.VersionTimeAfter(prior, now)
}
