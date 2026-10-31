package release

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/core/id"
)

// candidate_reused_db_test.go -- coverage proves a reused release step against the
// run that tested the same tree, through the same owner queries production reads,
// and the candidate records where the receipt came from
// (docs/superpowers/specs/2026-10-29-release-same-tree-reuse-design.md).

var (
	reusedTree        = strings.Repeat("7", 40)
	otherTree         = strings.Repeat("8", 40)
	reusedPullCommit  = strings.Repeat("9", 40)
	reusedFingerprint = "work-definition-v2:" + strings.Repeat("f", 64)
	// A source's own definition binds its commit, event and mode, so it never
	// equals the release's. The tree fingerprint is what the two share.
	sourceDefinition = "work-definition-v2:" + strings.Repeat("a", 64)
	reusedPackages   = []string{"./engine/..."}
)

// reusedPass is the receipt a pull request left for a release to stand on.
func reusedPass() carriedStep {
	return carriedStep{definition: sourceDefinition, packages: reusedPackages, treeFingerprint: reusedFingerprint}
}

func reusedOf(source string) carriedStep {
	return carriedStep{definition: carriedDefinition, packages: reusedPackages, evidenceRun: source, treeFingerprint: reusedFingerprint}
}

// pullRequest writes a successful affected run of the fixture's tree at another
// commit, an hour before now, as an up-to-date pull request leaves it.
func (f *carriedDB) pullRequest(now time.Time, change func(*carriedRunSpec)) (string, string) {
	f.t.Helper()
	spec := carriedRunSpec{event: "pull_request", mode: "affected", sha: reusedPullCommit, runKey: id.NewShortId(), succeeded: true, tree: reusedTree,
		queuedAt: now.Add(-2 * time.Hour), finishedAt: now.Add(-time.Hour), steps: map[string]carriedStep{"test.full": reusedPass()}}
	if change != nil {
		change(&spec)
	}
	return f.run(spec)
}

// release writes a successful release of the fixture's tree that reused step from
// source and built its own image.
func (f *carriedDB) release(now time.Time, step carriedStep, change func(*carriedRunSpec)) (string, string) {
	f.t.Helper()
	spec := carriedRunSpec{succeeded: true, tree: reusedTree, queuedAt: now.Add(-time.Minute), startedAt: now, steps: map[string]carriedStep{"build.image": image, "test.full": step}}
	if change != nil {
		change(&spec)
	}
	return f.run(spec)
}

// provenance is the record a candidate must carry for source's test.full.
func (f *carriedDB) provenance(t *testing.T, sourceRun, sourceWork, event, commit string, attempt int) *pl.ReleaseReusedFrom {
	t.Helper()
	rows := func(name string, args map[string]any) map[string]any {
		got, err := f.reader.rows(ownerCtx(), name, args, 1)
		if err != nil || len(got) != 1 {
			t.Fatal(name, err)
		}
		return got[0]
	}
	receipt, err := pl.InspectReleaseStepReceipt(f.owner, sourceWork, "test.full",
		rows("workRunForOwner", map[string]any{"runId": sourceWork}), rows("workStepForOwnerRun", map[string]any{"runId": sourceWork, "stepKey": "test.full"}))
	if err != nil {
		t.Fatal(err)
	}
	return &pl.ReleaseReusedFrom{PipelineRunID: sourceRun, WorkRunID: sourceWork, Attempt: attempt, Event: event, Commit: commit, TreeHash: reusedTree, ReceiptDigest: receipt.ReceiptDigest}
}

// A release of a merge commit stands on its pull request: another commit with one
// tree. Coverage proves it through the owner's reads and reports the source.
func TestReusedReceiptVerifiesAgainstTheRunThatTestedItsTree(t *testing.T) {
	for _, tc := range []struct{ event, mode string }{{"pull_request", "affected"}, {"push", "full"}, {"merge_group", "full"}} {
		t.Run(tc.event, func(t *testing.T) {
			f := newCarriedDB(t)
			now := time.Now()
			source, sourceWork := f.pullRequest(now, func(s *carriedRunSpec) { s.event, s.mode = tc.event, tc.mode })
			_, work := f.release(now, reusedOf(source), nil)
			want := f.provenance(t, source, sourceWork, tc.event, reusedPullCommit, 1)
			got, err := f.coverageRecording(work, map[string]*pl.ReleaseReusedFrom{"test.full": want})
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Skipped) != 1 || got.Skipped[0].ReusedFrom == nil || got.Skipped[0].CarriedFrom != nil {
				t.Fatalf("the reused step has no proof: %+v", got.Skipped)
			}
			if *got.Skipped[0].ReusedFrom != *want {
				t.Fatalf("reused from %+v, want %+v", *got.Skipped[0].ReusedFrom, *want)
			}
		})
	}
}

// Coverage refuses, naming the step, every reuse it cannot prove, each for its own
// reason. The reason is pinned because a candidate with no record is refused too,
// and that later refusal must not pass for this one.
func TestReusedReceiptRefusals(t *testing.T) {
	sourceStep := func(mutate func(*carriedStep)) func(*carriedRunSpec) {
		return func(s *carriedRunSpec) {
			step := s.steps["test.full"]
			mutate(&step)
			s.steps["test.full"] = step
		}
	}
	for name, tc := range map[string]struct {
		want    string
		source  func(*carriedRunSpec)
		release func(*carriedRunSpec)
		reused  func(*carriedStep)
	}{
		"another tree":                  {want: "tested a different tree", source: func(s *carriedRunSpec) { s.tree = otherTree }},
		"the source's tree was unread":  {want: "tree hash was not read", source: func(s *carriedRunSpec) { s.tree = "" }},
		"the release's tree was unread": {want: "tree hash was not read", release: func(s *carriedRunSpec) { s.tree = "" }},
		// The row and the journal must agree, and the tree a receipt's digest covers
		// is the journal's.
		"the source's row disagrees with its journal": {want: "the source run's own", source: func(s *carriedRunSpec) { s.workTree = otherTree }},
		"the source's journal has no tree":            {want: "on a known tree", source: func(s *carriedRunSpec) { s.noWorkTree = true }},
		"the source failed":                           {want: "did not succeed", source: func(s *carriedRunSpec) { s.succeeded = false }},
		"the source was told to stop":                 {want: "asked to stop", source: func(s *carriedRunSpec) { s.cancelRequested = true }},
		"the source is a scheduled scan":              {want: "scheduled scan", source: func(s *carriedRunSpec) { s.event = "schedule" }},
		"the source has no work run":                  {want: "no work run", source: func(s *carriedRunSpec) { s.noWorkRun = true }},
		"the source never finished":                   {want: "no recorded time", source: func(s *carriedRunSpec) { s.finishedAt = time.Time{} }},
		"the source is older than a day": {want: "older than the reuse window", source: func(s *carriedRunSpec) {
			s.finishedAt = time.Now().Add(-25 * time.Hour)
		}},
		"the source's step failed": {want: "did not leave a pass", source: sourceStep(func(s *carriedStep) { s.failed = true })},
		// Only a directly executed receipt counts.
		"the source's step was itself carried": {want: "executed, artifact-free", source: sourceStep(func(s *carriedStep) { s.carried = true })},
		"the source's step produced artifacts": {want: "executed, artifact-free", source: sourceStep(func(s *carriedStep) { s.artifacts = []string{carriedImage} })},
		"another check": {want: "different check", source: sourceStep(func(s *carriedStep) {
			s.treeFingerprint = "work-definition-v2:" + strings.Repeat("9", 64)
		})},
		"no check to compare":             {want: "different check", source: sourceStep(func(s *carriedStep) { s.treeFingerprint = "" })},
		"other packages":                  {want: "different packages", source: sourceStep(func(s *carriedStep) { s.packages = []string{"./other/..."} })},
		"the run it names does not exist": {want: "cannot be read", reused: func(s *carriedStep) { s.evidenceRun = id.NewShortId() }},
		"the release never began":         {want: "no recorded time", release: func(s *carriedRunSpec) { s.startedAt = time.Time{} }},
		// Reuse across runs is a release's alone.
		"the reuser is not a release": {want: "only a full release", release: func(s *carriedRunSpec) { s.event = "push"; s.runKey = id.NewShortId() }},
	} {
		t.Run(name, func(t *testing.T) {
			f := newCarriedDB(t)
			now := time.Now()
			source, _ := f.pullRequest(now, tc.source)
			step := reusedOf(source)
			if tc.reused != nil {
				tc.reused(&step)
			}
			_, work := f.release(now, step, tc.release)
			got, err := f.coverage(work)
			if err == nil || !strings.Contains(err.Error(), "test.full") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %+v, %v; want a refusal of test.full saying %q", got, err, tc.want)
			}
		})
	}
}

// Another pipeline's run of one tree stands for nothing here, whatever it passed.
func TestReusedReceiptRefusesAnotherPipeline(t *testing.T) {
	f := newCarriedDB(t)
	now := time.Now()
	other := id.NewShortId()
	profileMutation(t, f.engine, "createPipeline", map[string]any{"pipelineId": other, "packageId": id.NewShortId(), "name": "Other fixture", "repository": "acme/other", "defaultBranch": "main", "installationId": "1", "credentialId": id.NewShortId(), "delivery": "webhook"})
	for name, tc := range map[string]struct {
		want   string
		change func(*carriedRunSpec)
	}{
		"another pipeline":   {"another pipeline", func(s *carriedRunSpec) { s.pipeline = other }},
		"another repository": {"another repository", func(s *carriedRunSpec) { s.repository = "acme/other" }},
	} {
		t.Run(name, func(t *testing.T) {
			source, _ := f.pullRequest(now, tc.change)
			_, work := f.release(now, reusedOf(source), nil)
			got, err := f.coverage(work)
			if err == nil || !strings.Contains(err.Error(), "test.full") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %+v, %v; want a refusal saying %q", got, err, tc.want)
			}
		})
	}
}

// The candidate's record is what the journal proves. A record that is missing,
// changed, or claimed for a step that ran is refused, so client JSON cannot assert
// where a receipt came from.
func TestCandidateRecordOfAReuseIsWhatTheJournalProves(t *testing.T) {
	f := newCarriedDB(t)
	now := time.Now()
	source, sourceWork := f.pullRequest(now, nil)
	_, work := f.release(now, reusedOf(source), nil)
	honest := f.provenance(t, source, sourceWork, "pull_request", reusedPullCommit, 1)
	if _, err := f.coverageRecording(work, map[string]*pl.ReleaseReusedFrom{"test.full": honest}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.coverageRecording(work, nil); err == nil {
		t.Fatal("a candidate that does not record where a reused receipt came from was accepted")
	}
	for name, change := range map[string]func(*pl.ReleaseReusedFrom){
		"run":     func(r *pl.ReleaseReusedFrom) { r.PipelineRunID = "another" },
		"work":    func(r *pl.ReleaseReusedFrom) { r.WorkRunID = "another" },
		"attempt": func(r *pl.ReleaseReusedFrom) { r.Attempt = 2 },
		"event":   func(r *pl.ReleaseReusedFrom) { r.Event = "push" },
		"commit":  func(r *pl.ReleaseReusedFrom) { r.Commit = strings.Repeat("6", 40) },
		"tree":    func(r *pl.ReleaseReusedFrom) { r.TreeHash = otherTree },
		"receipt": func(r *pl.ReleaseReusedFrom) { r.ReceiptDigest = "sha256:" + strings.Repeat("6", 64) },
	} {
		t.Run("changed "+name, func(t *testing.T) {
			record := *honest
			change(&record)
			if _, err := f.coverageRecording(work, map[string]*pl.ReleaseReusedFrom{"test.full": &record}); err == nil {
				t.Fatal("a candidate that changed where a receipt came from was accepted")
			}
		})
	}
	t.Run("claimed for a step that ran", func(t *testing.T) {
		_, plain := f.release(now, executed, nil)
		if _, err := f.coverageRecording(plain, map[string]*pl.ReleaseReusedFrom{"test.full": honest}); err == nil {
			t.Fatal("a candidate that invented a reuse was accepted")
		}
	})
}

// #6369 still works over a reuse. A failed-only re-run carries what its attempt
// reused, the proof walks the chain to the attempt that reused it, and the source
// is what the candidate records. The bound is that attempt's start: a re-run's own
// time is bounded by the chain's window, as every carried pass is.
func TestACarriedReuseProvesAgainstItsSource(t *testing.T) {
	carriedReuse := carriedStep{definition: carriedDefinition, packages: reusedPackages, carried: true, treeFingerprint: reusedFingerprint}
	failedImage := carriedStep{definition: carriedDefinition, failed: true}
	for name, tc := range map[string]struct {
		first carriedStep
		age   time.Duration // of the source when the first attempt started
	}{
		"it settled as reused":            {first: carriedStep{}, age: time.Hour},
		"a failed stage blocked it first": {first: carriedStep{blocked: true}, age: time.Hour},
		"the source was nearly a day old": {first: carriedStep{}, age: 23 * time.Hour},
	} {
		t.Run(name, func(t *testing.T) {
			f := newCarriedDB(t)
			now := time.Now()
			firstStart := now.Add(-time.Hour)
			source, sourceWork := f.pullRequest(now, func(s *carriedRunSpec) { s.finishedAt = firstStart.Add(-tc.age) })
			firstStep := reusedOf(source)
			firstStep.blocked = tc.first.blocked
			first, _ := f.run(carriedRunSpec{attempt: 1, queuedAt: firstStart, startedAt: firstStart, tree: reusedTree, steps: map[string]carriedStep{"build.image": failedImage, "test.full": firstStep}})
			second, _ := f.run(carriedRunSpec{attempt: 2, rerunOf: first, queuedAt: now.Add(-time.Minute), startedAt: now.Add(-time.Minute), tree: reusedTree, steps: map[string]carriedStep{"build.image": failedImage, "test.full": carriedReuse}})
			_, third := f.run(carriedRunSpec{attempt: 3, rerunOf: second, queuedAt: now, startedAt: now, succeeded: true, tree: reusedTree, steps: map[string]carriedStep{"build.image": image, "test.full": carriedReuse}})
			want := f.provenance(t, source, sourceWork, "pull_request", reusedPullCommit, 1)
			got, err := f.coverageRecording(third, map[string]*pl.ReleaseReusedFrom{"test.full": want})
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Skipped) != 1 || got.Skipped[0].ReusedFrom == nil || *got.Skipped[0].ReusedFrom != *want || got.Skipped[0].CarriedFrom != nil {
				t.Fatalf("the carried reuse is not proven against its source: %+v", got.Skipped)
			}
		})
	}
	for name, tc := range map[string]struct {
		want   string
		change func(*carriedStep)
	}{
		"another definition": {"different step definition", func(s *carriedStep) { s.definition = "work-definition-v2:" + strings.Repeat("9", 64) }},
		"other packages":     {"different packages", func(s *carriedStep) { s.packages = []string{"./other/..."} }},
	} {
		t.Run("refused: "+name, func(t *testing.T) {
			f := newCarriedDB(t)
			now := time.Now()
			source, _ := f.pullRequest(now, nil)
			first, _ := f.run(carriedRunSpec{attempt: 1, queuedAt: now.Add(-time.Hour), startedAt: now.Add(-time.Hour), tree: reusedTree, steps: map[string]carriedStep{"build.image": failedImage, "test.full": reusedOf(source)}})
			step := carriedReuse
			tc.change(&step)
			_, second := f.run(carriedRunSpec{attempt: 2, rerunOf: first, queuedAt: now, startedAt: now, succeeded: true, tree: reusedTree, steps: map[string]carriedStep{"build.image": image, "test.full": step}})
			got, err := f.coverage(second)
			if err == nil || !strings.Contains(err.Error(), "test.full") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %+v, %v; want a refusal saying %q", got, err, tc.want)
			}
		})
	}
}

// The whole path, through the installed recipe: assembly resolves the receipts,
// writes the provenance into the candidate, and preparation proves it and pins the
// artifact only from the run the candidate names.
func TestCandidateAssemblyRecordsAReusedReceipt(t *testing.T) {
	db := candidateTestDB(t)
	f := newCarriedDB(t)
	p, c, library, _ := candidatePreparationFixture(t, db)
	now := time.Now()
	metadataID := strings.Repeat("f", 64)
	source, sourceWork := f.pullRequest(now, nil)
	imageStep := carriedStep{definition: carriedDefinition, artifacts: []string{library.receipt.IntentID, metadataID}}
	_, work := f.release(now, reusedOf(source), func(s *carriedRunSpec) { s.steps["build.image"] = imageStep })

	library.receipt.WorkRunID, library.receipt.StepKey, library.receipt.Path = work, "build.image", "build/image.oci.tar"
	l := &assemblyLibraryFixture{candidateLibraryFixture: library, metadata: library.receipt}
	l.metadata.IntentID, l.metadata.Path, l.metadata.ETag = metadataID, "build/metadata.json", "metadata-v1"
	metadata, _ := json.Marshal(map[string]any{"containerimage.digest": c.Components[0].Artifacts[0].ImageDigest})
	l.setMetadata(metadata)
	p.library = l
	p.evidence = f.reader
	p.assemblyPlans = []candidateAssemblyPlan{assemblyTestPlan()}

	record, err := p.assemble(ownerCtx(), "coordinated", map[string]string{"engine_arm64": memql.BareShortId(work)})
	if err != nil {
		t.Fatal(err)
	}
	cleanupCandidate(t, db, record.ID)
	if record.State != "ready" {
		t.Fatalf("the candidate is %q", record.State)
	}
	want := f.provenance(t, source, sourceWork, "pull_request", reusedPullCommit, 1)
	found := 0
	for _, evidence := range record.Manifest.Evidence {
		switch evidence.StepKey {
		case "test.full":
			if evidence.ReusedFrom == nil || *evidence.ReusedFrom != *want {
				t.Errorf("the candidate records %+v, want %+v", evidence.ReusedFrom, want)
			}
			found++
		default:
			if evidence.ReusedFrom != nil {
				t.Errorf("%s ran, and the candidate says it was reused: %+v", evidence.StepKey, evidence.ReusedFrom)
			}
		}
	}
	if found != 1 {
		t.Fatalf("the candidate has %d records of the reused step", found)
	}
	// Every artifact still comes from the run the candidate names.
	for _, artifact := range record.Manifest.Components[0].Artifacts {
		if artifact.Receipt.WorkRunID != work {
			t.Errorf("artifact %s comes from %s, not the release %s", artifact.Name, artifact.Receipt.WorkRunID, work)
		}
	}
}

// countedRead counts each query a reader makes.
type countedRead struct {
	inner interface {
		Execute(context.Context, string) (*memql.ExecuteResult, error)
	}
	seen map[string]int
}

func (c *countedRead) Execute(ctx context.Context, q string) (*memql.ExecuteResult, error) {
	c.seen[q]++
	return c.inner.Execute(ctx, q)
}

// Many reused steps name one source, and a publication repeats coverage for each
// artifact. One pass reads each row once, and the next pass reads it fresh.
func TestOneProofPassReadsEachRowOnce(t *testing.T) {
	f := newCarriedDB(t)
	now := time.Now()
	source, _ := f.pullRequest(now, func(s *carriedRunSpec) { s.steps["test.other"] = reusedPass() })
	_, work := f.release(now, reusedOf(source), func(s *carriedRunSpec) { s.steps["test.other"] = reusedOf(source) })

	counted := &countedRead{inner: f.reader.engine, seen: map[string]int{}}
	reader := candidateEvidenceReader{engine: counted}
	plan, err := reader.completeRun(ownerCtx(), work)
	if err != nil {
		t.Fatal(err)
	}
	reads := func() int {
		n := 0
		for query, count := range counted.seen {
			if strings.Contains(query, "pipelineRunForOwner") && strings.Contains(query, source) {
				n += count
			}
		}
		return n
	}
	got, err := reader.reusedProvenance(ownerCtx(), work, plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["test.full"] == nil || got["test.other"] == nil {
		t.Fatalf("both reused steps must be proven: %+v", got)
	}
	if n := reads(); n != 1 {
		t.Fatalf("one pass read the source run %d times", n)
	}
	if _, err := reader.reusedProvenance(ownerCtx(), work, plan); err != nil {
		t.Fatal(err)
	}
	if n := reads(); n != 2 {
		t.Fatalf("the next pass did not read the source run again: %d reads", n)
	}
}
