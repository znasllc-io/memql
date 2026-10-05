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

func (i *Integration) parkHumanDecision(ctx context.Context, owner, runID, approvalKind string, subject map[string]any, question string, options []map[string]any, expires time.Time) (string, error) {
	rc, ok := common.RunFromContext(ctx)
	if !ok || memql.BareShortId(rc.RunId) != memql.BareShortId(runID) || memql.BareShortId(rc.OwnerUserId) != memql.BareShortId(owner) {
		return "", fmt.Errorf("human decision requires the current owned work run")
	}
	hash := workstate.ArtifactHash(subject)
	id := "question-" + workstate.ArtifactHash(map[string]any{"run": memql.BareShortId(runID), "step": rc.StepKey, "question": hash, "kind": approvalKind})[:32]
	if i.decisionGate == nil {
		return "", fmt.Errorf("feedback coordination is unavailable")
	}
	release, err := i.decisionGate(ctx, id)
	if err != nil {
		return "", err
	}
	defer release()
	ctx = memql.ContextWithFreshRead(ownerActor(ctx, owner))
	run, err := i.store().runForOwner(ctx, runID)
	if err != nil {
		return "", err
	}
	if run == nil || run["cancelRequested"] == true || (rowString(run, "status") != runStatusRunning && rowString(run, "status") != runStatusWaiting) {
		return "", fmt.Errorf("feedback run is no longer active")
	}
	existing, err := i.store().query(ctx, "query "+call("workApprovalForOwner", map[string]any{"approvalId": id}))
	if err != nil {
		return "", err
	}
	// Renewal names the expired receipt, so every replica discovers the same
	// successor. Hold that successor's decision lock too: an answer arriving
	// while it is created must never be overwritten by the park below.
	for renewals := 0; approvalKind == "scopeElevation" && len(existing) > 0 && rowString(existing[0], "decision") != "rejected"; renewals++ {
		until, valid := rowTime(existing[0], "expiresAt")
		if valid && i.clock().Before(until) {
			break
		}
		if renewals >= 100 {
			return "", fmt.Errorf("computer access has expired too many times; start fresh work")
		}
		id = "question-" + workstate.ArtifactHash(map[string]any{"renewalOf": id})[:32]
		unlock, lockErr := i.decisionGate(ctx, id)
		if lockErr != nil {
			return "", lockErr
		}
		defer unlock()
		existing, err = i.store().query(ctx, "query "+call("workApprovalForOwner", map[string]any{"approvalId": id}))
		if err != nil {
			return "", err
		}
	}
	// Re-read after acquiring a successor lock, which may have waited behind
	// a person's decision on another replica.
	run, err = i.store().runForOwner(ctx, runID)
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
		_, err = i.RaiseApproval(ctx, owner, ApprovalSeed{ApprovalId: id, RunId: runID, StepKey: rc.StepKey, Kind: approvalKind, Subject: subject, ArtifactHash: hash, Question: question, Options: options, ExpiresAt: expires})
		if err != nil {
			return "", err
		}
	}
	if rowString(run, "status") == runStatusWaiting {
		return id, nil
	}
	if err := i.store().updateRun(ctx, runID, map[string]any{"versionTime": rfc(workRowVersionAfter(run["createdAt"], i.clock())), "status": runStatusWaiting, "waitingOn": map[string]any{"kind": "approval", "subject": id, "approvalKind": approvalKind, "since": rfc(i.clock().UTC())}}); err != nil {
		return "", err
	}
	return id, nil
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
