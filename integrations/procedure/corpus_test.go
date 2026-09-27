package procedure

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/work"
)

// corpus_test.go -- rows to recordings (gaps G2 and G3, epic memql#5408).

func loadFixtureCorpus(t *testing.T, recs ...recFixture) ([]recording, *fakeEngine) {
	t.Helper()
	eng := newFakeEngine()
	seedCorpus(t, eng, recs...)
	got, err := newTestIntegration(eng).loadCorpus(context.Background(), corpusKeyFor())
	if err != nil {
		t.Fatalf("loadCorpus: %v", err)
	}
	return got, eng
}

// TestTheCorpusReadsArgumentsFromTheObservation is gap G3. The session writer
// never wrote step.input: an action's arguments live on its tool_result
// observation, as a JSON STRING, and until the loader read them there every
// recorded action canonicalized to an empty call -- so no two recordings ever
// differed where they really differed, and no template ever had the hole it
// needed. The observables a replay is compared on come from the same row.
func TestTheCorpusReadsArgumentsFromTheObservation(t *testing.T) {
	recs, _ := loadFixtureCorpus(t, twoRecordings()...)
	if len(recs) != 2 {
		t.Fatalf("loaded %d recordings, want 2", len(recs))
	}
	exec := recs[0].Steps[0]
	if exec.StepType != "exec" {
		t.Fatalf("first step is %q, want the exec (steps must be in seq order)", exec.StepType)
	}
	if got, want := exec.Input["command"], "mkdir -p out && echo hello > a.txt"; got != want {
		t.Fatalf("exec arguments = %v, want the observation's %q", exec.Input, want)
	}
	// The observation's file_path, written as the WORKSPACE (relativize.go):
	// the recording ran in testWorkspace, and a replay runs in its own.
	if got := recs[0].Steps[1].Input["file_path"]; got != relativeReportPath {
		t.Fatalf("fs_write arguments = %v, want the observation's file_path relative to the workspace", recs[0].Steps[1].Input)
	}
	ev := recs[0].Evidence[exec.Key].Observation
	if ev.ExitCode == nil || *ev.ExitCode != 0 || ev.IsError == nil || *ev.IsError || ev.ResultType != "string" {
		t.Fatalf("exec evidence = %+v, want exit 0, no error, a string result", ev)
	}
	if len(ev.Contents) != 0 {
		t.Fatalf("an exec's file effects must not be expected -- a dispatcher cannot see them; got %+v", ev.Contents)
	}
	if recs[0].Workspace != testWorkspace || recs[0].Fingerprint == nil {
		t.Fatalf("the session's fingerprint (gap G4) must be read off its first step; workspace %q, fingerprint %v",
			recs[0].Workspace, recs[0].Fingerprint)
	}
}

// TestTheCorpusResolvesContentDigestsFromTheLibrary: a written file is
// compared by its bytes, and the bytes' digest is the Library row's sha256.
// The row is CONTENT-ADDRESSED -- both recordings wrote the same bytes, so
// both reference the one row the FIRST session filed, under that session's
// name -- and the path therefore comes from THIS action's arguments, written
// relative to the workspace so a replay in another workspace is comparable.
func TestTheCorpusResolvesContentDigestsFromTheLibrary(t *testing.T) {
	recs, eng := loadFixtureCorpus(t, twoRecordings()...)
	want := []work.ContentDigest{{Op: "write", Path: "out/report.txt", Digest: helloDigest}}
	for n, rec := range recs {
		write := rec.Steps[1]
		got := rec.Evidence[write.Key].Observation.Contents
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("recording %d: contents = %+v, want %+v", n, got, want)
		}
		paths := rec.Evidence[write.Key].Paths
		if len(paths) == 0 || paths[len(paths)-1] != relativeReportPath {
			t.Fatalf("recording %d: the footprint's paths are %v, want the recorded %s relative to the workspace", n, paths, reportPath)
		}
		if ws := rec.Evidence[write.Key].Workspace; ws != testWorkspace {
			t.Fatalf("recording %d: the evidence was read against %q, want the fingerprint's workspace", n, ws)
		}
	}
	for _, c := range eng.callsTo("libraryFileById") {
		if c.Actor != testOwner || c.Internal {
			t.Fatalf("the Library row was read as %q (internal %v); it is the owner's and needs no stamp", c.Actor, c.Internal)
		}
	}
}

// TestAnOmittedContentStillNamesItsPathAndDigest: a content above the cap is
// referenced by digest only (design D5), and the entry that says so is the
// only record of which file it was.
func TestAnOmittedContentStillNamesItsPathAndDigest(t *testing.T) {
	p, d := parseOmitted("/w/project/big.bin: 9000000 bytes is above the 8388608-byte per-content cap (sha256 ABCDEF)")
	if p != "/w/project/big.bin" || d != "abcdef" {
		t.Fatalf("parseOmitted = %q, %q", p, d)
	}
	if p, d := parseOmitted("/w/x: this node stores no content"); p != "/w/x" || d != "" {
		t.Fatalf("an omission with no digest: %q, %q", p, d)
	}
}

// TestARunWithNoActionsAtTheLevelIsSkipped: the goal's own run carries the
// signature too, and holds statements rather than actions. Mined as a
// recording it would be a sequence of nothing -- and counted toward D14's
// floor of two, so one real recording beside it would look like a corpus.
func TestARunWithNoActionsAtTheLevelIsSkipped(t *testing.T) {
	eng := newFakeEngine()
	recs := twoRecordings()
	seedCorpus(t, eng, recs[0])
	goalRun := recs[1].runRow()
	goalRun["id"] = "v1:work:run:the-goal"
	eng.reply("workRunsForOwnerGoalSignature", recs[0].runRow(), goalRun)
	eng.replyWhen("workStepsForOwnerRun", `"v1:work:run:the-goal"`,
		map[string]any{"key": "call0", "seq": float64(0), "stepType": "builtin"},
		map[string]any{"key": "call1", "seq": float64(1), "stepType": "function"})
	got, err := newTestIntegration(eng).loadCorpus(context.Background(), corpusKeyFor())
	if err != nil {
		t.Fatalf("loadCorpus: %v", err)
	}
	if len(got) != 1 || got[0].RunId != recs[0].runId {
		t.Fatalf("loaded %d recordings (%v); the goal's statement-only run must be skipped", len(got), got)
	}
}

// TestAReplayRunIsNotARecording is the coordinator's decision for epic
// memql#5408: a run a procedure's replay opened (triggeredBy procedure:<mode>)
// never enters a corpus. Learning from one would teach a procedure its own
// output, and a shadow comparison of one would compare it with itself.
func TestAReplayRunIsNotARecording(t *testing.T) {
	recs := twoRecordings()
	replay := recording1("c.txt", testNow.Add(-30*time.Minute))
	replay.triggeredBy = "procedure:trusted"
	got, _ := loadFixtureCorpus(t, recs[0], recs[1], replay)
	for _, rec := range got {
		if rec.RunId == replay.runId {
			t.Fatal("a replay run was loaded as a recording")
		}
	}
	if len(got) != 2 {
		t.Fatalf("loaded %d recordings, want the two real ones", len(got))
	}
}

// TestARunLevelDislikeExcludesTheRecordingAndAStepLevelOneDoesNot (D21, D23):
// a dislike naming the RUN takes the recording out of the corpus; a verdict
// naming a STEP is the candidate gate's evidence, and excluding the recording
// for it would hide the very step the gate is asked about.
func TestARunLevelDislikeExcludesTheRecordingAndAStepLevelOneDoesNot(t *testing.T) {
	recs := twoRecordings()
	disliked := recording1("c.txt", testNow.Add(-30*time.Minute))
	disliked.runDisliked = true
	stepHeld := recs[1]
	stepHeld.stepFeedback = []stepFeedback{{version: 1, verdict: "dislike", at: testNow}}
	got, _ := loadFixtureCorpus(t, recs[0], stepHeld, disliked)
	if len(got) != 2 {
		t.Fatalf("loaded %d recordings, want 2: the run-level dislike out, the step-level one in", len(got))
	}
	held := got[1]
	if held.RunId != stepHeld.runId || len(held.Verdicts[stepHeld.writeKey()]) != 1 {
		t.Fatalf("the step-level verdict must ride on its recording: %+v", held.Verdicts)
	}
}

// TestARecordingWhoseArgumentsCannotBeReadBackIsSkipped: an action with no
// tool_result, or whose arguments were cut at the observation ceiling, is a
// call nobody can reproduce -- and a template generalized over it replays a
// call nobody made.
func TestARecordingWhoseArgumentsCannotBeReadBackIsSkipped(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  func(*recFixture)
	}{
		{"no tool_result", func(r *recFixture) { r.noToolResult = true }},
		{"arguments truncated", func(r *recFixture) { r.argsTruncated = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recs := twoRecordings()
			tc.mod(&recs[1])
			got, _ := loadFixtureCorpus(t, recs...)
			if len(got) != 1 || got[0].RunId != recs[0].runId {
				t.Fatalf("loaded %d recordings; the unreadable one must be skipped", len(got))
			}
		})
	}
}

// TestTheAppsAnswerIsNotAnAction: every session ends with an app_answer step,
// which is the app's intelligence and carries no tool_result. Mined, it ended
// every procedure with a step no replay can perform, so none could ever be
// promoted.
func TestTheAppsAnswerIsNotAnAction(t *testing.T) {
	if levelAccepts(LevelAction, "app_answer") {
		t.Fatal("app_answer is not an action a procedure can replay")
	}
	recs, _ := loadFixtureCorpus(t, twoRecordings()...)
	for _, rec := range recs {
		for _, s := range rec.Steps {
			if s.StepType == "app_answer" {
				t.Fatalf("recording %s kept its app_answer step", rec.RunId)
			}
		}
		if len(rec.Steps) != 2 {
			t.Fatalf("recording %s has %d steps, want its two actions", rec.RunId, len(rec.Steps))
		}
	}
}

// TestTheCorpusIsReadOldestFirstUnderTheOwner: the corpus read runs as the
// OWNER (the composite tier would serve a sweep everybody's runs), and the
// corpus is ordered oldest first -- Generalize reads a template's hints off
// its first instance, so a corpus that put each new recording first would
// re-spell an unchanged procedure on every recording.
func TestTheCorpusIsReadOldestFirstUnderTheOwner(t *testing.T) {
	recs, eng := loadFixtureCorpus(t, twoRecordings()...)
	if recs[0].RunId != twoRecordings()[0].runId {
		t.Fatalf("first recording is %s, want the oldest", recs[0].RunId)
	}
	for _, c := range eng.recorded() {
		if c.Actor != testOwner {
			t.Fatalf("%s ran as %q, want the owner", c.Name(), c.Actor)
		}
		if strings.HasPrefix(c.Query, "mutation ") {
			t.Fatalf("loading a corpus wrote %s", c.Name())
		}
	}
	read := eng.callTo(t, "workRunsForOwnerGoalSignature")
	if !strings.Contains(read.Query, `goalSignature: "`+testSignature+`"`) {
		t.Fatalf("the corpus read is %s, want it pushed down on the signature", read.Query)
	}
}

// TestABlankSignatureReadsNothing: grouping runs of different goals mines
// across unrelated work.
func TestABlankSignatureReadsNothing(t *testing.T) {
	eng := newFakeEngine()
	got, err := newTestIntegration(eng).loadCorpus(context.Background(), corpusKey{OwnerUserId: testOwner, Level: LevelAction})
	if err != nil || len(got) != 0 || len(eng.recorded()) != 0 {
		t.Fatalf("a blank signature read %d rows with %d calls (err %v)", len(got), len(eng.recorded()), err)
	}
}

// TestStepVersionsKeepsTheNewestVerdictPerVersion (D21): a verdict is never
// overwritten, so a version's newest row is the person's current judgment of
// it, and versions are handed over oldest first.
func TestStepVersionsKeepsTheNewestVerdictPerVersion(t *testing.T) {
	t0 := testNow
	got := stepVersions([]stepVerdict{
		{Version: 2, Verdict: work.VerdictLike, CreatedAt: t0.Add(time.Minute)},
		{Version: 1, Verdict: work.VerdictLike, CreatedAt: t0},
		{Version: 1, Verdict: work.VerdictDislike, CreatedAt: t0.Add(2 * time.Minute)},
	})
	want := []work.Verdict{work.VerdictDislike, work.VerdictLike}
	if !reflect.DeepEqual(got.Verdicts, want) {
		t.Fatalf("verdicts = %v, want %v", got.Verdicts, want)
	}
}
