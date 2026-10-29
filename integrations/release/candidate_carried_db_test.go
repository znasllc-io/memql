package release

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/component/workjournal"
	"github.com/znasllc-io/memql/core/id"
)

// carriedDB writes real native release attempts (pipeline run rows) and their
// journal work runs, so candidate coverage proves carried steps through the
// same owner queries production reads.
type carriedDB struct {
	t        *testing.T
	engine   *memql.MemQLEngine
	reader   candidateEvidenceReader
	journal  *workjournal.Journal
	owner    string
	pipeline string
	runKey   string
}

const carriedRepo = "acme/engine"

var (
	carriedSHA        = strings.Repeat("b", 40)
	carriedDefinition = "work-definition-v2:" + strings.Repeat("d", 64)
	carriedImage      = strings.Repeat("c", 64)
)

// carriedStep is one declared step of an attempt and how it ended.
type carriedStep struct {
	definition string
	packages   []string
	carried    bool
	failed     bool
	artifacts  []string
}

func newCarriedDB(t *testing.T) *carriedDB {
	t.Helper()
	db := candidateTestDB(t)
	engine := assemblyEngine(t, db)
	owner, err := candidateOwner(ownerCtx())
	if err != nil {
		t.Fatal(err)
	}
	f := &carriedDB{t: t, engine: engine, reader: candidateEvidenceReader{engine: evidenceFreshRead{t, engine}}, owner: owner,
		pipeline: id.NewShortId(), runKey: id.NewShortId()}
	f.journal = workjournal.New(workjournal.ExecutorFunc(func(ctx context.Context, q string) (any, error) { return engine.Execute(ctx, q) }), slog.New(slog.DiscardHandler), "carried-producer")
	profileMutation(t, engine, "createPipeline", map[string]any{"pipelineId": f.pipeline, "packageId": id.NewShortId(), "name": "Carried fixture", "repository": carriedRepo, "defaultBranch": "main", "installationId": "1", "credentialId": id.NewShortId(), "delivery": "webhook"})
	return f
}

// attempt writes release attempt n: its pipeline run row, a journal work run
// with the given steps, and the run's ending.
func (f *carriedDB) attempt(n int, rerunOf string, queuedAt time.Time, succeeded bool, steps map[string]carriedStep) (string, string) {
	t := f.t
	t.Helper()
	runID := id.NewShortId()
	conclusion := "failure"
	if succeeded {
		conclusion = "success"
	}
	args := map[string]any{"runId": runID, "pipelineId": f.pipeline, "repository": carriedRepo, "sha": carriedSHA, "mode": "full", "event": "release", "runKey": f.runKey,
		"attempt": n, "trigger": "manual", "releaseTag": "v1.2.3", "version": "v1.2.3", "status": "completed", "conclusion": conclusion, "queuedAt": queuedAt.UTC().Format(time.RFC3339)}
	if rerunOf != "" {
		args["rerunOf"] = rerunOf
	}
	profileMutation(t, f.engine, "createPipelineRun", args)
	keys := make([]string, 0, len(steps))
	for key := range steps {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	decls := make([]workjournal.StepDecl, 0, len(keys))
	for _, key := range keys {
		s := steps[key]
		call := map[string]any{"construct": "pipeline", "pipelineKind": "command", "definitionFingerprint": s.definition, "packages": s.packages}
		if s.carried {
			call["skip"] = map[string]any{"code": pl.CodePassedEarlier, "reason": "Passed in attempt 1."}
		}
		decls = append(decls, workjournal.StepDecl{Key: key, Kind: workjournal.KindDeterministic, StepType: "exec", Call: call})
	}
	work := workjournal.Work{OwnerUserID: f.owner, Template: "pipeline:carried", GoalKey: id.NewShortId(), RunKey: "1", Statement: "Carried release fixture", TriggeredBy: pl.WorkTriggerPrefix + "full",
		Input: map[string]any{"repository": carriedRepo, "sha": carriedSHA, "mode": "full", "event": "release", "pipelineId": f.pipeline, "pipelineRunId": runID, "attempt": n},
		Steps: decls}
	_, workRunID, err := workjournal.IDs(work)
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.journal.Begin(ownerCtx(), work)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range keys {
		s := steps[key]
		step, err := run.Step(ownerCtx(), key)
		if err != nil {
			t.Fatal(err)
		}
		receipt := workjournal.Receipt{Status: "done", Result: map[string]any{"status": "succeeded", "metadata": map[string]any{"exitCode": 0}}}
		switch {
		case s.carried:
			receipt = workjournal.Receipt{Status: "skipped", Code: pl.CodePassedEarlier, Result: map[string]any{"code": pl.CodePassedEarlier, "reason": "Passed in attempt 1."}}
		case s.failed:
			receipt = workjournal.Receipt{Status: "failed", Code: "pipeline_step_failed", Message: "exit 1", Result: map[string]any{"status": "failed", "metadata": map[string]any{"exitCode": 1}}}
		case len(s.artifacts) > 0:
			receipt.Result["metadata"] = map[string]any{"exitCode": 0, "artifactIntentIds": s.artifacts}
		}
		if err := step.Finish(ownerCtx(), receipt); err != nil {
			t.Fatal(key, err)
		}
	}
	if succeeded {
		err = run.Succeeded(ownerCtx(), map[string]any{"status": "succeeded"})
	} else {
		err = run.Failed(ownerCtx(), "pipeline_failed", "A step failed.")
	}
	if err != nil {
		t.Fatal(err)
	}
	profileMutation(t, f.engine, "updatePipelineRun", map[string]any{"runId": runID, "workRunId": workRunID})
	return runID, workRunID
}

// coverage asks candidate coverage about a candidate whose evidence is the
// complete work run workRunID and whose artifact build.image produced.
func (f *carriedDB) coverage(workRunID string) (candidateCoverage, error) {
	t := f.t
	t.Helper()
	built, err := f.reader.read(ownerCtx(), workRunID, "build.image")
	if err != nil {
		t.Fatal(err)
	}
	c := candidateTestManifest()
	c.OwnerUserID = f.owner
	a := &c.Components[0].Artifacts[0]
	a.Receipt = pl.ReleaseReceiptReference{WorkRunID: workRunID, StepKey: "build.image", Attempt: built.Attempt, IntentID: carriedImage, DefinitionDigest: built.DefinitionDigest, ReceiptDigest: built.ReceiptDigest}
	c.Evidence = nil
	for _, key := range []string{"build.image", "test.full"} {
		receipt, err := f.reader.readReceipt(ownerCtx(), workRunID, key, true)
		if err != nil {
			t.Fatal(key, err)
		}
		c.Evidence = append(c.Evidence, pl.ReleaseEvidence{Name: key, Component: "engine", WorkRunID: workRunID, StepKey: key, Attempt: receipt.Attempt,
			ReceiptID: receipt.ReceiptID, ReceiptDigest: receipt.ReceiptDigest, DefinitionDigest: receipt.DefinitionDigest, ArtifactIntentIDs: receipt.ArtifactIntentIDs})
	}
	return f.reader.coverage(ownerCtx(), c)
}

// executed is an artifact-free pass of test.full; image re-builds build.image.
var (
	executed = carriedStep{definition: carriedDefinition, packages: []string{"./engine/..."}}
	image    = carriedStep{definition: carriedDefinition, artifacts: []string{carriedImage}}
	carried  = carriedStep{definition: carriedDefinition, packages: []string{"./engine/..."}, carried: true}
)

// TestCarriedReceiptVerifiesAgainstItsExecutingAttempt: the pass a failed-only
// re-run carried is proven against the failed attempt that executed it.
func TestCarriedReceiptVerifiesAgainstItsExecutingAttempt(t *testing.T) {
	f := newCarriedDB(t)
	now := time.Now()
	first, firstWork := f.attempt(1, "", now.Add(-time.Hour), false, map[string]carriedStep{"build.image": {definition: carriedDefinition, failed: true}, "test.full": executed})
	_, rerun := f.attempt(2, first, now, true, map[string]carriedStep{"build.image": image, "test.full": carried})
	got, err := f.coverage(rerun)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Skipped) != 1 || got.Skipped[0].CarriedFrom == nil {
		t.Fatalf("carried step has no proof: %+v", got.Skipped)
	}
	from := got.Skipped[0].CarriedFrom
	want := carriedExecuted(t, f, firstWork)
	if from.Attempt != 1 || from.PipelineRunID != first || from.WorkRunID != firstWork || from.ReceiptDigest != want {
		t.Fatalf("carried from %+v, want attempt 1 %s/%s %s", from, first, firstWork, want)
	}
}

// TestCarriedReceiptThroughTwoAttempts: a pass carried twice is proven against
// the attempt that executed it, not the attempt that carried it first.
func TestCarriedReceiptThroughTwoAttempts(t *testing.T) {
	f := newCarriedDB(t)
	now := time.Now()
	first, firstWork := f.attempt(1, "", now.Add(-2*time.Hour), false, map[string]carriedStep{"build.image": {definition: carriedDefinition, failed: true}, "test.full": executed})
	second, _ := f.attempt(2, first, now.Add(-time.Hour), false, map[string]carriedStep{"build.image": {definition: carriedDefinition, failed: true}, "test.full": carried})
	_, third := f.attempt(3, second, now, true, map[string]carriedStep{"build.image": image, "test.full": carried})
	got, err := f.coverage(third)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Skipped) != 1 || got.Skipped[0].CarriedFrom == nil || got.Skipped[0].CarriedFrom.Attempt != 1 || got.Skipped[0].CarriedFrom.WorkRunID != firstWork {
		t.Fatalf("a pass carried twice is not proven against attempt 1: %+v", got.Skipped)
	}
}

// TestCarriedReceiptRefusals: coverage refuses, naming the step, every carried
// pass it cannot prove.
func TestCarriedReceiptRefusals(t *testing.T) {
	for name, original := range map[string]carriedStep{
		"artifacts on the original": {definition: carriedDefinition, packages: []string{"./engine/..."}, artifacts: []string{strings.Repeat("e", 64)}},
		"definition changed":        {definition: "work-definition-v2:" + strings.Repeat("9", 64), packages: []string{"./engine/..."}},
		"packages changed":          {definition: carriedDefinition, packages: []string{"./other/..."}},
		"original failed":           {definition: carriedDefinition, packages: []string{"./engine/..."}, failed: true},
	} {
		t.Run(name, func(t *testing.T) {
			f := newCarriedDB(t)
			now := time.Now()
			first, _ := f.attempt(1, "", now.Add(-time.Hour), false, map[string]carriedStep{"build.image": {definition: carriedDefinition, failed: true}, "test.full": original})
			_, rerun := f.attempt(2, first, now, true, map[string]carriedStep{"build.image": image, "test.full": carried})
			if got, err := f.coverage(rerun); err == nil || !strings.Contains(err.Error(), "test.full") {
				t.Fatalf("an unproven carried pass was accepted: %+v %v", got, err)
			}
		})
	}
	t.Run("missing link", func(t *testing.T) {
		f := newCarriedDB(t)
		_, rerun := f.attempt(2, id.NewShortId(), time.Now(), true, map[string]carriedStep{"build.image": image, "test.full": carried})
		if got, err := f.coverage(rerun); err == nil {
			t.Fatalf("a carried pass with no earlier attempt was accepted: %+v", got)
		}
	})
}

// carriedExecuted is the carried-identity digest of firstWork's test.full.
func carriedExecuted(t *testing.T, f *carriedDB, workRunID string) string {
	t.Helper()
	rows := func(name string, args map[string]any) map[string]any {
		got, err := f.reader.rows(ownerCtx(), name, args, 1)
		if err != nil || len(got) != 1 {
			t.Fatal(name, err)
		}
		return got[0]
	}
	receipt, err := pl.InspectCarriedReleaseReceipt(f.owner, workRunID, "test.full",
		rows("workRunForOwner", map[string]any{"runId": workRunID}), rows("workStepForOwnerRun", map[string]any{"runId": workRunID, "stepKey": "test.full"}))
	if err != nil {
		t.Fatal(err)
	}
	return receipt.ReceiptDigest
}
