package pipelines

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"
)

// A release run may skip an artifact-free verification step when another
// successful run of the same pipeline executed it on the same git tree
// (docs/superpowers/specs/2026-10-29-release-same-tree-reuse-design.md).
//
// Two halves judge that, at different times and from different reads: the driver
// when it plans the run (component/pipelinerun/evidence.go), and candidate
// coverage when it proves the skip (integrations/release/candidate_reused.go). A
// successful release run is never replaced, so a skip the runner plans and the
// proof refuses costs the release its version, which is how 0.31.4 and 0.31.5
// were lost. The rules below are therefore one piece of code that both halves
// call, rather than two readings of one rule. They live here, over injected reads,
// because those two halves are separate modules: the driver's tests can run the
// whole proof over rows the driver really wrote
// (TestAReleaseReusesItsTreeOverRealRows).

// ReleaseReuseWindow is how old a source may be when a release reuses it, from
// the source's finish to the start of the attempt that reused it. It is the same
// length as the failed-only carry window, and the two add up: a re-run may carry a
// pass that was already a day old when its first attempt reused it.
const ReleaseReuseWindow = 24 * time.Hour

var treeHashPattern = regexp.MustCompile(`^(?:[a-f0-9]{40}|[a-f0-9]{64})$`)

// ValidTreeHash reports a full lower-case git object id, of either hash
// function. Anything else is not a tree hash a run may be compared by: a hash
// that was never read must not compare equal to another that was never read.
func ValidTreeHash(value string) bool { return treeHashPattern.MatchString(value) }

// SameTreeRun is the part of a pipeline run the reuse rule reads. Ids are bare,
// so a comparison here is between two spellings of nothing.
type SameTreeRun struct {
	ID, OwnerUserID, PipelineID, Repository, SHA string
	Mode, Event, TreeHash, Status, Conclusion    string
	WorkRunID                                    string
	Attempt                                      int
	CancelRequested                              bool
	StartedAt, FinishedAt                        time.Time
}

// SameTreeRunFromRow reads a v1:pipelines:run row as the rule does. A row spells
// some ids canonically and bare is the caller's to supply, because this package
// imports nothing beyond the standard library.
func SameTreeRunFromRow(row map[string]any, bare func(string) string) (SameTreeRun, error) {
	var decoded SameTreeRun
	body, err := json.Marshal(row)
	if err != nil {
		return decoded, err
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return decoded, err
	}
	decoded.ID, decoded.OwnerUserID, decoded.PipelineID = bare(decoded.ID), bare(decoded.OwnerUserID), bare(decoded.PipelineID)
	decoded.WorkRunID = bare(decoded.WorkRunID)
	return decoded, nil
}

// SameTreeJudgedAt is the clock the runner judges a source at: the later of now
// and the run's start. The proof judges at the run's start, the only clock still
// true when a candidate is read again at publication, so a source the runner accepts
// is one the proof accepts, even if the replica planning the run has a clock behind
// the one that started it (TestSameTreeWindowOfTheRunnerImpliesTheProofs).
func SameTreeJudgedAt(now, started time.Time) time.Time {
	if started.After(now) {
		return started
	}
	return now
}

// SameTreeProblem says why source's receipts cannot stand for reuser's steps, or
// "" when they can. The caller decides at: the runner passes SameTreeJudgedAt, and
// the proof passes the release's start.
//
// There is deliberately no lower bound on the age: a source that finished just
// after the release started is fresh, and the proof must not refuse it for
// looking impossible. The tree is what makes the two runs comparable, and the
// step's fingerprint is compared separately, row by row.
//
// A scheduled scan is refused because its run key carries its UTC day, not a
// change anyone reviewed. It is also the one event evidence reuse has always
// excluded on both sides.
func SameTreeProblem(reuser, source SameTreeRun, at time.Time) string {
	switch {
	case source.ID == "" || source.ID == reuser.ID:
		return "the source is not another run"
	case reuser.OwnerUserID == "" || source.OwnerUserID != reuser.OwnerUserID:
		return "the source belongs to another owner"
	case reuser.PipelineID == "" || source.PipelineID != reuser.PipelineID:
		return "the source belongs to another pipeline"
	case reuser.Repository == "" || source.Repository != reuser.Repository:
		return "the source belongs to another repository"
	case !ValidTreeHash(reuser.TreeHash) || !ValidTreeHash(source.TreeHash):
		return "a run's tree hash was not read"
	case source.TreeHash != reuser.TreeHash:
		return "the source tested a different tree"
	case source.Status != "completed" || source.Conclusion != "success":
		return "the source did not succeed"
	case source.CancelRequested:
		return "the source was asked to stop"
	case source.Event == string(EventSchedule):
		return "a scheduled scan is not evidence for a release"
	case reuser.StartedAt.IsZero() || source.FinishedAt.IsZero():
		return "a run has no recorded time"
	case at.Sub(source.FinishedAt) > ReleaseReuseWindow:
		return "the source is older than the reuse window"
	}
	return ""
}

// InspectReusedReleaseReceipt admits the receipt a release reuses from another
// run: a command that actually ran and passed, exit zero, with no artifact
// intents and a tree hash, inside a work run that succeeded and was never asked
// to stop. A source run failing elsewhere has no such receipt, unlike a carried
// pass inside one release (InspectCarriedReleaseReceipt), because nothing but its
// success ties a pull request's run to the release.
//
// It uses the release identity, so one step receipt has one digest whichever way
// it is read. An artifact-bearing receipt is refused because every artifact a
// candidate pins must come from the run the candidate names.
//
// The receipt must also be the source run's own: the journal names the pipeline
// run it executed for, and that run's pipeline, repository, commit and tree must
// agree with the source row SameTreeProblem judged. So the tree comes from the
// journal, which every receipt's digest covers, and a row that disagrees with the
// journal is refused rather than believed.
func InspectReusedReleaseReceipt(owner, runID, stepKey string, run, step map[string]any, source SameTreeRun) (ReleaseWorkReceipt, error) {
	out, err := inspectPipelineStepReceipt(owner, runID, stepKey, run, step, admitReleased)
	if err != nil {
		return ReleaseWorkReceipt{}, err
	}
	if out.StepKind != string(StepCommand) || out.Status != "done" || len(out.ArtifactIntentIDs) != 0 || !ValidTreeHash(out.TreeHash) {
		return ReleaseWorkReceipt{}, errors.New("a reused release receipt requires an executed, artifact-free successful command on a known tree")
	}
	input, _ := run["input"].(map[string]any)
	if input["pipelineRunId"] != source.ID || input["pipelineId"] != source.PipelineID ||
		out.Repository != source.Repository || out.Commit != source.SHA || out.TreeHash != source.TreeHash {
		return ReleaseWorkReceipt{}, errors.New("a reused release receipt must be the source run's own, at its commit and tree")
	}
	return out, nil
}

// ReusedEvidenceRun is the run a step row reused, or "" for a row that names none.
// It reads the plan's declaration rather than how the row settled, because a failed
// stage may have blocked the step before it could settle as reused, and a failed-only
// re-run still carries it.
func ReusedEvidenceRun(step map[string]any) string {
	call, _ := step["call"].(map[string]any)
	skip, _ := call["skip"].(map[string]any)
	if skip["code"] != CodePassedEarlier {
		return ""
	}
	run, _ := call["evidenceRunId"].(string)
	return run
}

// ReusedStepReads are the journal reads one proof makes. Each caller supplies its own
// owner-scoped, fresh reads, so the proof sees only what that owner may see.
type ReusedStepReads struct {
	PipelineRun func(id string) (SameTreeRun, error)
	WorkRun     func(id string) (map[string]any, error)
	WorkStep    func(workRunID, stepKey string) (map[string]any, error)
}

// ProveReusedStep proves one step a release reused from another run, and answers the
// provenance the candidate records (the design record's D7 and D8). reuser is the
// attempt that declared the reuse, and reuserStep its row: the attempt a candidate
// names, or an earlier one it carries the step from.
//
//   - Trigger: coverage met a pipeline_passed_earlier skip whose row names an
//     evidence run.
//   - Evidence: the journal, read fresh on every pass, including at publication and
//     overlay time. Nothing is taken from the candidate: coverage compares its record
//     with this answer and refuses a difference, so client JSON cannot assert where a
//     receipt came from.
//   - The rule is the runner's own: SameTreeProblem and InspectReusedReleaseReceipt,
//     the two calls the driver made before it planned the skip. The age is judged at
//     the reusing attempt's start.
//   - Both rows must declare the same treeFingerprint, which binds the tree, the
//     command, the image, the engine, the workflow and the packages
//     (component/pipelinerun/definition.go, treeFingerprintOf), and the same slice.
//   - Fallback: an error, naming what failed, refuses the candidate. It does not ask
//     GitHub again. Each run's driver read its tree from the commit object, and the
//     journal is already the trust root for whether a test passed.
//
// Only a full release reuses across runs: every other event compares commits, and its
// skips never become candidates.
func ProveReusedStep(reads ReusedStepReads, owner string, reuser SameTreeRun, reuserStep map[string]any, stepKey string) (ReleaseReusedFrom, error) {
	var none ReleaseReusedFrom
	if reuser.Event != string(EventRelease) || reuser.Mode != string(ModeFull) {
		return none, errors.New("only a full release reuses another run's receipt")
	}
	source, err := reads.PipelineRun(ReusedEvidenceRun(reuserStep))
	if err != nil {
		return none, fmt.Errorf("the run it reused cannot be read: %w", err)
	}
	if problem := SameTreeProblem(reuser, source, reuser.StartedAt); problem != "" {
		return none, fmt.Errorf("run %s cannot stand for this release: %s", source.ID, problem)
	}
	if source.WorkRunID == "" {
		return none, fmt.Errorf("run %s has no work run to prove the pass", source.ID)
	}
	runRow, err := reads.WorkRun(source.WorkRunID)
	if err != nil {
		return none, fmt.Errorf("run %s: %w", source.ID, err)
	}
	stepRow, err := reads.WorkStep(source.WorkRunID, stepKey)
	if err != nil {
		return none, fmt.Errorf("run %s: %w", source.ID, err)
	}
	receipt, err := InspectReusedReleaseReceipt(owner, source.WorkRunID, stepKey, runRow, stepRow, source)
	if err != nil {
		return none, fmt.Errorf("run %s did not leave a pass to reuse: %w", source.ID, err)
	}
	declared, _ := stepCall(reuserStep)["treeFingerprint"].(string)
	if declared == "" || declared != stepCall(stepRow)["treeFingerprint"] {
		return none, fmt.Errorf("run %s ran a different check", source.ID)
	}
	if !slices.Equal(stepCallStrings(reuserStep, "packages"), stepCallStrings(stepRow, "packages")) {
		return none, fmt.Errorf("run %s ran the step over different packages", source.ID)
	}
	return ReleaseReusedFrom{PipelineRunID: source.ID, WorkRunID: source.WorkRunID, Attempt: source.Attempt, Event: source.Event,
		Commit: source.SHA, TreeHash: receipt.TreeHash, ReceiptDigest: receipt.ReceiptDigest}, nil
}

func stepCall(step map[string]any) map[string]any {
	call, _ := step["call"].(map[string]any)
	return call
}

// stepCallStrings is a list a step's call declares, however it was decoded.
func stepCallStrings(step map[string]any, key string) []string {
	var out []string
	switch values := stepCall(step)[key].(type) {
	case []string:
		out = append(out, values...)
	case []any:
		for _, value := range values {
			if text, ok := value.(string); ok {
				out = append(out, text)
			}
		}
	}
	return out
}
