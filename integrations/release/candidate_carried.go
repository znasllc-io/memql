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

// carriedRun is the part of a native pipeline run row a carried or reused proof
// reads.
type carriedRun struct {
	ID, OwnerUserID, PipelineID, Repository, SHA, Mode, Event string
	RunKey, RerunOf, Version, ReleaseTag, WorkRunID           string
	Attempt                                                   int
	QueuedAt                                                  time.Time
	// row is the run as read, for the rule a reuse is proven under.
	row map[string]any
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

// candidateStepProof is what one pipeline_passed_earlier skip stands on: a receipt
// an earlier attempt of the release executed, or one another run executed on the
// same tree. Exactly one is set.
type candidateStepProof struct {
	CarriedFrom *candidateCarriedFrom
	ReusedFrom  *pl.ReleaseReusedFrom
}

// oneRow is the one row a named owner query answers, or an error saying otherwise.
// Within one pass (forOnePass) each row is read once.
func (r candidateEvidenceReader) oneRow(ctx context.Context, name string, args map[string]any) (map[string]any, error) {
	key := renderCall(name, args)
	if row, ok := r.pass[key]; ok {
		return row, nil
	}
	rows, err := r.rows(ctx, name, args, 1)
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 {
		return nil, fmt.Errorf("%s answered %d rows, want exactly one", name, len(rows))
	}
	if r.pass != nil {
		r.pass[key] = rows[0]
	}
	return rows[0], nil
}

// forOnePass is this reader for one proof of a complete run. Twenty-seven reused
// steps name one source run, and a publication repeats coverage for each artifact, so
// reading that run and its work run for every step would double what verifying the
// candidate costs. A pass reads each row once, still fresh from the database, and
// keeps it no longer than the pass.
func (r candidateEvidenceReader) forOnePass() candidateEvidenceReader {
	r.pass = map[string]map[string]any{}
	return r
}

// pipelineRun reads one of the owner's native pipeline runs.
func (r candidateEvidenceReader) pipelineRun(ctx context.Context, id string) (carriedRun, error) {
	var run carriedRun
	row, err := r.oneRow(ctx, "pipelineRunForOwner", map[string]any{"runId": id})
	if err != nil {
		return run, err
	}
	if err := decodeRow(row, &run); err != nil {
		return run, err
	}
	if memql.BareShortId(run.ID) != id {
		return run, errors.New("pipeline run identity changed")
	}
	run.row = row
	return run, nil
}

func decodeRow(row map[string]any, dst any) error {
	body, err := json.Marshal(row)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, dst)
}

// passedEarlier proves one step a release skipped as pipeline_passed_earlier:
// carried from the attempt of the same release that executed it
// (docs/superpowers/specs/2026-10-28-release-failed-only-rerun-design.md), or
// reused from the run that tested the same tree (candidate_reused.go).
//
//   - The carrying work run names its native pipeline run, which must be the
//     same attempt, owned by the same person.
//   - A row that names an evidence run was reused here, and reusedFrom proves it.
//     This attempt need not be a re-run: a release's first attempt reuses.
//   - Otherwise the attempt chain must hold (carriedChain).
//   - Walking back, the first attempt that reused the step is proven the same
//     way, against that attempt's own start. A failed-only re-run therefore keeps
//     what its attempt reused, inside the chain's 24-hour window, without the row
//     having to have settled as reused: a failed stage may have blocked it first.
//   - Attempts that carried the step themselves are passed over. The first attempt
//     that did not carry it must have executed it: a done command with exit zero
//     and no artifact intents, inside an attempt that ended in any terminal state
//     (pl.InspectCarriedReleaseReceipt).
//   - That receipt, or that reuse, must have the carried step's definition digest
//     and package slice.
//
// Every read is the owner's, through the same fresh queries as the rest of
// candidate evidence. An error names what failed; nothing is assumed.
func (r candidateEvidenceReader) passedEarlier(ctx context.Context, owner, workRunID string, carried pl.ReleaseWorkReceipt) (candidateStepProof, error) {
	workRow, err := r.oneRow(ctx, "workRunForOwner", map[string]any{"runId": workRunID})
	if err != nil {
		return candidateStepProof{}, err
	}
	var work struct {
		Input struct {
			PipelineRunID string
			Attempt       int
		}
	}
	if err := decodeRow(workRow, &work); err != nil {
		return candidateStepProof{}, err
	}
	readRun := func(id string) (carriedRun, error) { return r.pipelineRun(ctx, id) }
	carrying, err := readRun(memql.BareShortId(work.Input.PipelineRunID))
	if err != nil {
		return candidateStepProof{}, err
	}
	if carrying.OwnerUserID != owner || memql.BareShortId(carrying.WorkRunID) != workRunID || carrying.Attempt != work.Input.Attempt {
		return candidateStepProof{}, errors.New("the carrying work run is not its release attempt's")
	}
	carriedRow, err := r.oneRow(ctx, "workStepForOwnerRun", map[string]any{"runId": workRunID, "stepKey": carried.StepKey})
	if err != nil {
		return candidateStepProof{}, err
	}
	reused := func(reuser carriedRun, row map[string]any) (candidateStepProof, error) {
		from, err := r.reusedFrom(ctx, owner, reuser, row, carried.StepKey)
		if err != nil {
			return candidateStepProof{}, err
		}
		return candidateStepProof{ReusedFrom: &from}, nil
	}
	if reusedSkip(carriedRow) != "" {
		return reused(carrying, carriedRow)
	}
	chain, err := carriedChain(carrying, readRun)
	if err != nil {
		return candidateStepProof{}, err
	}
	for _, earlier := range chain {
		earlierWork := memql.BareShortId(earlier.WorkRunID)
		if earlierWork == "" {
			return candidateStepProof{}, fmt.Errorf("attempt %d has no work run to prove the pass", earlier.Attempt)
		}
		stepRow, err := r.oneRow(ctx, "workStepForOwnerRun", map[string]any{"runId": earlierWork, "stepKey": carried.StepKey})
		if err != nil {
			return candidateStepProof{}, fmt.Errorf("attempt %d: %w", earlier.Attempt, err)
		}
		if reusedSkip(stepRow) != "" {
			if stepDefinitionDigest(stepRow) != carried.DefinitionDigest {
				return candidateStepProof{}, fmt.Errorf("attempt %d reused a different step definition", earlier.Attempt)
			}
			if !slices.Equal(callPackages(stepRow), callPackages(carriedRow)) {
				return candidateStepProof{}, fmt.Errorf("attempt %d reused the step over different packages", earlier.Attempt)
			}
			return reused(earlier, stepRow)
		}
		if carriedSkip(stepRow) {
			continue
		}
		runRow, err := r.oneRow(ctx, "workRunForOwner", map[string]any{"runId": earlierWork})
		if err != nil {
			return candidateStepProof{}, fmt.Errorf("attempt %d: %w", earlier.Attempt, err)
		}
		executed, err := pl.InspectCarriedReleaseReceipt(owner, earlierWork, carried.StepKey, runRow, stepRow)
		if err != nil {
			return candidateStepProof{}, fmt.Errorf("attempt %d did not leave a pass to carry: %w", earlier.Attempt, err)
		}
		if executed.DefinitionDigest != carried.DefinitionDigest {
			return candidateStepProof{}, fmt.Errorf("attempt %d ran a different step definition", earlier.Attempt)
		}
		if !slices.Equal(callPackages(stepRow), callPackages(carriedRow)) {
			return candidateStepProof{}, fmt.Errorf("attempt %d ran the step over different packages", earlier.Attempt)
		}
		return candidateStepProof{CarriedFrom: &candidateCarriedFrom{PipelineRunID: memql.BareShortId(earlier.ID), WorkRunID: earlierWork, Attempt: earlier.Attempt, ReceiptDigest: executed.ReceiptDigest}}, nil
	}
	return candidateStepProof{}, errors.New("no earlier attempt of the release executed the step")
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
