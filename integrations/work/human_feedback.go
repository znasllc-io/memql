package work

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

// AskFeedback creates and parks under the same cross-replica decision lock
// used by the answer path. A retry cannot create a second inbox item or park
// over a decision already made by the person.
func (i *Integration) AskFeedback(ctx context.Context, owner, runID, question, kind string, options []map[string]any, expires time.Time) (string, error) {
	if strings.TrimSpace(question) == "" || len(question) > 8000 {
		return "", fmt.Errorf("feedback needs a concise, nonempty question")
	}
	if kind != "text" && kind != "choice" && kind != "multi" {
		return "", fmt.Errorf("feedback kind must be text, choice or multi")
	}
	if kind != "text" && (len(options) == 0 || len(options) > 20) {
		return "", fmt.Errorf("feedback choices need between 1 and 20 options")
	}
	seen := map[string]bool{}
	for _, option := range options {
		value := rowString(option, "value")
		if value == "" || seen[value] || strings.TrimSpace(rowString(option, "label")) == "" {
			return "", fmt.Errorf("feedback options need unique values and readable labels")
		}
		seen[value] = true
	}
	subject := map[string]any{"question": question, "kind": kind, "options": options}
	return i.parkHumanDecision(ctx, owner, runID, "feedback", subject, question, options, expires)
}

func (i *Integration) AskComputerScope(ctx context.Context, owner, runID string, subject map[string]any) (string, error) {
	return i.parkHumanDecision(ctx, owner, runID, "scopeElevation", subject, rowString(subject, "summary"), nil, i.clock().UTC().Add(time.Hour))
}

// AskPlanReview parks the current run on an exact, immutable proposal. The DSL
// chooses the question and placement; shared native machinery owns the receipt.
func (i *Integration) AskPlanReview(ctx context.Context, owner, runID string, subject map[string]any, question string) (string, error) {
	if len(subject) == 0 || strings.TrimSpace(question) == "" || len(question) > 8000 {
		return "", fmt.Errorf("review needs a proposal and question")
	}
	return i.parkHumanDecision(ctx, owner, runID, workstate.ApprovalKindPlanReview, subject, question, nil, time.Time{})
}

func (i *Integration) parkHumanDecision(ctx context.Context, owner, runID, approvalKind string, subject map[string]any, question string, options []map[string]any, expires time.Time) (string, error) {
	rc, ok := common.RunFromContext(ctx)
	if !ok || memql.BareShortId(rc.RunId) != memql.BareShortId(runID) || memql.BareShortId(rc.OwnerUserId) != memql.BareShortId(owner) {
		return "", fmt.Errorf("human decision requires the current owned work run")
	}
	hash := workstate.ArtifactHash(subject)
	first := "question-" + workstate.ArtifactHash(map[string]any{"run": memql.BareShortId(runID), "step": rc.StepKey, "question": hash, "kind": approvalKind})[:32]
	if i.decisionGate == nil {
		return "", fmt.Errorf("feedback coordination is unavailable")
	}
	read := memql.ContextWithFreshRead(ownerActor(ctx, owner))
	// THE CHAIN'S LOCKS ARE ONE ACQUISITION. Renewal names the expired receipt,
	// so every replica discovers the same successor, and an answer arriving at
	// any receipt of the chain while it is walked must never be overwritten by
	// the park below: every receipt the chain runs through is a decision lock.
	// Taken one inside another they were a gate connection each, and a chain
	// longer than the gate pool could never be renewed (memql#6022). So: walk the
	// chain without its locks, take every key it ran through in chain order --
	// the same order for every replica, since the chain is a function of its
	// first receipt -- in one transaction on one connection, and walk it again
	// under them. The locked walk is the answer; one that went past the keys
	// held (a grant expired in between) is walked and locked again.
	var (
		id       string
		existing []map[string]any
	)
	for attempt := 1; ; attempt++ {
		// Only computer access renews; every other question is its first receipt.
		walked := []string{first}
		if approvalKind == "scopeElevation" {
			var err error
			if walked, _, err = i.renewalChain(read, approvalKind, first); err != nil {
				return "", err
			}
		}
		release, err := i.decisionGate(ctx, walked...)
		if err != nil {
			return "", err
		}
		locked, rows, err := i.renewalChain(read, approvalKind, first)
		if err != nil {
			release()
			return "", err
		}
		if len(locked) <= len(walked) {
			defer release()
			id, existing = locked[len(locked)-1], rows
			break
		}
		release()
		if attempt == renewalLockAttempts {
			return "", fmt.Errorf("computer access kept expiring while it was being renewed; try again")
		}
	}
	// Read under the locks, which may have waited behind a person's decision on
	// another replica.
	run, err := i.store().runForOwner(read, runID)
	if err != nil {
		return "", err
	}
	if run == nil || run["cancelRequested"] == true || (rowString(run, "status") != runStatusRunning && rowString(run, "status") != runStatusWaiting) {
		return "", fmt.Errorf("feedback run is no longer active")
	}
	if waiting := rowMap(run, "waitingOn"); rowString(run, "status") == runStatusWaiting && memql.BareShortId(rowString(waiting, "subject")) != id {
		return "", fmt.Errorf("this run is already waiting for another decision")
	}
	for _, row := range existing {
		if rowString(row, "decision") != "" {
			return id, nil
		}
	}
	if len(existing) == 0 {
		_, err = i.RaiseApproval(read, owner, ApprovalSeed{ApprovalId: id, RunId: runID, StepKey: rc.StepKey, Kind: approvalKind, Subject: subject, ArtifactHash: hash, Question: question, Options: options, ExpiresAt: expires})
		if err != nil {
			return "", err
		}
	}
	if rowString(run, "status") == runStatusWaiting {
		return id, nil
	}
	if err := i.store().updateRun(read, runID, map[string]any{"versionTime": rfc(workRowVersionAfter(run["createdAt"], i.clock())), "status": runStatusWaiting, "waitingOn": map[string]any{"kind": "approval", "subject": id, "approvalKind": approvalKind, "since": rfc(i.clock().UTC())}}); err != nil {
		return "", err
	}
	return id, nil
}

// maxScopeRenewals bounds a computer-access question's renewal chain: past it the
// person is told to start fresh work rather than renew again.
const maxScopeRenewals = 100

// renewalLockAttempts is how many times a renewal walks its chain and locks it
// before it gives up: a second walk differs from the first only when a grant
// expired between them, so a third is already rare.
const renewalLockAttempts = 3

// renewalChain walks a question's receipts from its first: every receipt the
// question runs through now -- for computer access, each expired grant and its
// successor in turn -- ending with the one that answers for it, and that
// receipt's rows (none when it does not exist yet). Every other kind of question
// is its first receipt alone.
func (i *Integration) renewalChain(ctx context.Context, approvalKind, first string) ([]string, []map[string]any, error) {
	chain := []string{first}
	existing, err := i.store().query(ctx, "query "+call("workApprovalForOwner", map[string]any{"approvalId": first}))
	if err != nil {
		return nil, nil, err
	}
	for renewals := 0; approvalKind == "scopeElevation" && len(existing) > 0 && rowString(existing[0], "decision") != "rejected"; renewals++ {
		until, valid := rowTime(existing[0], "expiresAt")
		if valid && i.clock().Before(until) {
			break
		}
		if renewals >= maxScopeRenewals {
			return nil, nil, fmt.Errorf("computer access has expired too many times; start fresh work")
		}
		next := "question-" + workstate.ArtifactHash(map[string]any{"renewalOf": chain[len(chain)-1]})[:32]
		chain = append(chain, next)
		existing, err = i.store().query(ctx, "query "+call("workApprovalForOwner", map[string]any{"approvalId": next}))
		if err != nil {
			return nil, nil, err
		}
	}
	return chain, existing, nil
}

func validateFeedbackAnswer(subject map[string]any, decision string, answer map[string]any) error {
	kind := rowString(subject, "kind")
	if kind != "text" && kind != "choice" && kind != "multi" {
		return nil
	} // Other approval producers have their own contracts.
	if decision != "answered" {
		return fmt.Errorf("work: this question needs an answer")
	}
	// Every human question accepts an ad hoc answer as well as its suggestions.
	// A supplied invalid option is still rejected rather than hidden by prose.
	if text := strings.TrimSpace(rowString(answer, "text")); len(text) > 8000 {
		return fmt.Errorf("work: the answer exceeds 8000 characters")
	} else if text != "" && rowString(answer, "value") == "" && len(rowStringSlice(answer, "values")) == 0 {
		return nil
	}
	if kind == "text" {
		if strings.TrimSpace(rowString(answer, "text")) == "" {
			return fmt.Errorf("work: enter an answer before continuing")
		}
		return nil
	}
	allowed := map[string]bool{}
	raw, _ := json.Marshal(subject["options"])
	var options []map[string]any
	_ = json.Unmarshal(raw, &options)
	for _, option := range options {
		allowed[rowString(option, "value")] = true
	}
	if kind == "choice" {
		if !allowed[rowString(answer, "value")] {
			return fmt.Errorf("work: choose an option offered by this question")
		}
		return nil
	}
	values := rowStringSlice(answer, "values")
	if len(values) == 0 {
		return fmt.Errorf("work: choose at least one option")
	}
	seen := map[string]bool{}
	for _, v := range values {
		if !allowed[v] || seen[v] {
			return fmt.Errorf("work: answers must be distinct options offered by this question")
		}
		seen[v] = true
	}
	return nil
}
