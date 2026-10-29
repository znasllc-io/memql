package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	pl "github.com/znasllc-io/memql/component/pipelines"
)

// candidateCarriedFrom names the executed receipt a carried release step
// stands on: the attempt of the same release that ran the step, and that
// step's receipt digest (pl.InspectCarriedReleaseReceipt). The engine recipe
// (releasePrepareCandidateWorkflow) accepts a pipeline_passed_earlier skip
// only together with this proof.
type candidateCarriedFrom struct {
	PipelineRunID string `json:"pipelineRunId"`
	WorkRunID     string `json:"workRunId"`
	Attempt       int    `json:"attempt"`
	ReceiptDigest string `json:"receiptDigest"`
}

// carriedRun is the part of a native pipeline run row a carried proof reads.
type carriedRun struct {
	ID, OwnerUserID, PipelineID, Repository, SHA, Mode, Event string
	RunKey, RerunOf, Version, ReleaseTag, WorkRunID           string
	Attempt                                                   int
	QueuedAt                                                  time.Time
}

const (
	// carriedReleaseWindow and maxCarriedAttempts mirror the runner's
	// releaseCarryWindow and maxReleaseAttempts (component/pipelinerun). The
	// runner carries nothing past them, and coverage refuses what the runner
	// would not have carried.
	carriedReleaseWindow = 24 * time.Hour
	maxCarriedAttempts   = 50
)

// sameRelease: two attempts of one release share every identity a release
// run key is built from, and the version and tag the release was cut at.
func sameRelease(a, b carriedRun) bool {
	return a.OwnerUserID == b.OwnerUserID && memql.BareShortId(a.PipelineID) == memql.BareShortId(b.PipelineID) &&
		a.Repository == b.Repository && a.SHA == b.SHA && a.Mode == b.Mode && a.Event == b.Event &&
		a.RunKey == b.RunKey && a.Version == b.Version && a.ReleaseTag == b.ReleaseTag
}

// carriedChain walks carrying's rerunOf links back to attempt 1 and returns
// the earlier attempts, newest first. Only a full release re-run carries a
// pass. Every link must stay inside the same release and name a strictly
// earlier attempt, so the walk ends and cannot cycle. The chain is refused
// when it is longer than maxCarriedAttempts, does not reach attempt 1, or its
// first attempt was opened more than carriedReleaseWindow before the carrying
// attempt: every carried pass is younger than attempt 1, so that one
// comparison bounds them all.
func carriedChain(carrying carriedRun, read func(id string) (carriedRun, error)) ([]carriedRun, error) {
	if carrying.Event != string(pl.EventRelease) || carrying.Mode != string(pl.ModeFull) {
		return nil, errors.New("only a full release run carries a release pass")
	}
	if carrying.Attempt <= 1 || carrying.Attempt > maxCarriedAttempts {
		return nil, fmt.Errorf("attempt %d of a release cannot carry a pass", carrying.Attempt)
	}
	var chain []carriedRun
	current := carrying
	for current.Attempt > 1 {
		prev, err := read(memql.BareShortId(strings.TrimSpace(current.RerunOf)))
		if err != nil {
			return nil, fmt.Errorf("attempt %d re-runs an attempt that cannot be read: %w", current.Attempt, err)
		}
		if !sameRelease(prev, carrying) || prev.Attempt >= current.Attempt {
			return nil, fmt.Errorf("attempt %d re-runs a run outside its release", current.Attempt)
		}
		chain = append(chain, prev)
		current = prev
	}
	if current.Attempt != 1 {
		return nil, errors.New("the release's attempt chain does not reach its first attempt")
	}
	if current.QueuedAt.IsZero() || carrying.QueuedAt.IsZero() || current.QueuedAt.Before(carrying.QueuedAt.Add(-carriedReleaseWindow)) {
		return nil, fmt.Errorf("the release's first attempt was opened more than 24 hours before attempt %d", carrying.Attempt)
	}
	return chain, nil
}

// carriedFrom proves one step that a failed-only re-run of a release carried
// (skip code pipeline_passed_earlier) against the attempt that executed it
// (docs/superpowers/specs/2026-10-28-release-failed-only-rerun-design.md):
//
//   - The carrying work run names its native pipeline run, which must be the
//     same attempt, owned by the same person.
//   - The attempt chain must hold (carriedChain).
//   - Walking back, attempts that carried the step themselves are passed
//     over. The first attempt that did not carry it must have executed it: a
//     done command with exit zero and no artifact intents, inside an attempt
//     that ended in any terminal state (pl.InspectCarriedReleaseReceipt).
//   - That receipt must have the carried step's definition digest and
//     package slice.
//
// Every read is the owner's, through the same fresh queries as the rest of
// candidate evidence. An error names what failed; nothing is assumed.
func (r candidateEvidenceReader) carriedFrom(ctx context.Context, owner, workRunID string, carried pl.ReleaseWorkReceipt) (candidateCarriedFrom, error) {
	one := func(name string, args map[string]any) (map[string]any, error) {
		rows, err := r.rows(ctx, name, args, 1)
		if err != nil {
			return nil, err
		}
		if len(rows) != 1 {
			return nil, fmt.Errorf("%s answered %d rows, want exactly one", name, len(rows))
		}
		return rows[0], nil
	}
	decode := func(row map[string]any, dst any) error {
		body, err := json.Marshal(row)
		if err != nil {
			return err
		}
		return json.Unmarshal(body, dst)
	}
	readRun := func(id string) (carriedRun, error) {
		var run carriedRun
		row, err := one("pipelineRunForOwner", map[string]any{"runId": id})
		if err != nil {
			return run, err
		}
		if err := decode(row, &run); err != nil {
			return run, err
		}
		if memql.BareShortId(run.ID) != id {
			return run, errors.New("pipeline run identity changed")
		}
		return run, nil
	}
	workRow, err := one("workRunForOwner", map[string]any{"runId": workRunID})
	if err != nil {
		return candidateCarriedFrom{}, err
	}
	var work struct {
		Input struct {
			PipelineRunID string
			Attempt       int
		}
	}
	if err := decode(workRow, &work); err != nil {
		return candidateCarriedFrom{}, err
	}
	carrying, err := readRun(memql.BareShortId(work.Input.PipelineRunID))
	if err != nil {
		return candidateCarriedFrom{}, err
	}
	if carrying.OwnerUserID != owner || memql.BareShortId(carrying.WorkRunID) != workRunID || carrying.Attempt != work.Input.Attempt {
		return candidateCarriedFrom{}, errors.New("the carrying work run is not its release attempt's")
	}
	chain, err := carriedChain(carrying, readRun)
	if err != nil {
		return candidateCarriedFrom{}, err
	}
	carriedRow, err := one("workStepForOwnerRun", map[string]any{"runId": workRunID, "stepKey": carried.StepKey})
	if err != nil {
		return candidateCarriedFrom{}, err
	}
	for _, earlier := range chain {
		earlierWork := memql.BareShortId(earlier.WorkRunID)
		if earlierWork == "" {
			return candidateCarriedFrom{}, fmt.Errorf("attempt %d has no work run to prove the pass", earlier.Attempt)
		}
		stepRow, err := one("workStepForOwnerRun", map[string]any{"runId": earlierWork, "stepKey": carried.StepKey})
		if err != nil {
			return candidateCarriedFrom{}, fmt.Errorf("attempt %d: %w", earlier.Attempt, err)
		}
		if carriedSkip(stepRow) {
			continue
		}
		runRow, err := one("workRunForOwner", map[string]any{"runId": earlierWork})
		if err != nil {
			return candidateCarriedFrom{}, fmt.Errorf("attempt %d: %w", earlier.Attempt, err)
		}
		executed, err := pl.InspectCarriedReleaseReceipt(owner, earlierWork, carried.StepKey, runRow, stepRow)
		if err != nil {
			return candidateCarriedFrom{}, fmt.Errorf("attempt %d did not leave a pass to carry: %w", earlier.Attempt, err)
		}
		if executed.DefinitionDigest != carried.DefinitionDigest {
			return candidateCarriedFrom{}, fmt.Errorf("attempt %d ran a different step definition", earlier.Attempt)
		}
		if !slices.Equal(callPackages(stepRow), callPackages(carriedRow)) {
			return candidateCarriedFrom{}, fmt.Errorf("attempt %d ran the step over different packages", earlier.Attempt)
		}
		return candidateCarriedFrom{PipelineRunID: memql.BareShortId(earlier.ID), WorkRunID: earlierWork, Attempt: earlier.Attempt, ReceiptDigest: executed.ReceiptDigest}, nil
	}
	return candidateCarriedFrom{}, errors.New("no earlier attempt of the release executed the step")
}

// carriedSkip reports a step row an attempt settled as carried from an
// earlier one.
func carriedSkip(step map[string]any) bool {
	call, _ := step["call"].(map[string]any)
	skip, _ := call["skip"].(map[string]any)
	return step["status"] == "skipped" && skip["code"] == pl.CodePassedEarlier
}

// callPackages is the package slice the plan gave a step (call.packages).
func callPackages(step map[string]any) []string {
	call, _ := step["call"].(map[string]any)
	raw, _ := call["packages"].([]any)
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		if s, ok := p.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
