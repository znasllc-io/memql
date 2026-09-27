package proving

// procedure_correction_test.go -- a person's correction of one step, driven
// with no engine (epic memql#5414, task memql#5420): the goals' runs executed,
// the verdicts and the re-run, the checks that read them, and the negative
// controls run on the TEST -- each knob breaks one rule the database lane
// relies on, and the verifier must say which.

import (
	"context"
	"strings"
	"testing"

	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/proving/figure"
	"github.com/znasllc-io/memql/component/proving/scenario"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

// --- The committed correction, end to end --------------------------------------

func TestOnlyALifecycleThatStepsInExecutesItsGoalsRuns(t *testing.T) {
	// Epic D's lifecycles keep their committed path, where nothing executes a
	// goal's run; a lifecycle that intervenes executes every goal the app
	// serves, and leaves every run closed exactly once -- by the executor,
	// never a second time by the driver.
	for _, tc := range []struct {
		scenario string
		executed int
	}{
		{scnTrusted, 0},
		{scnDivergence, 0},
		// acme, globex, initech and umbrella go to the app; hooli and stark
		// are replays, whose runs the replay serves.
		{scnCorrected, 4},
		{scnDislikedControl, 6},
	} {
		t.Run(tc.scenario, func(t *testing.T) {
			h := &fakeHarness{}
			r := &Runner{Lifecycle: h, Prov: lifecycleProv()}
			p := r.RunScenario(context.Background(), lifecycles(t)[tc.scenario], figure.ArmPlatform)
			if p.Err != nil || !p.Passed {
				t.Fatalf("the platform arm: err %v, failures %v", p.Err, p.Failures)
			}
			spine := h.ladders[0].spine
			if len(spine.steps) != tc.executed {
				t.Fatalf("%d goal runs were executed, want %d", len(spine.steps), tc.executed)
			}
			if len(spine.goals) != len(spine.closed) {
				t.Fatalf("%d goal runs opened and %d closed; every one must be closed", len(spine.goals), len(spine.closed))
			}
			executed := 0
			for _, g := range p.lifecycle.Goals {
				if g.Executed {
					executed++
				}
			}
			if executed != tc.executed {
				t.Fatalf("the record says %d goals were executed, want %d", executed, tc.executed)
			}
		})
	}
}

func TestTheReRunsSessionIsToldTheDislikeInTheDelegatesWords(t *testing.T) {
	// D23's repair half, end to end over the fakes: the re-run carries the
	// newest dislike on the version it replaces, the app is told it in the
	// words the session delegate uses, the session is asked for the level the
	// person chose, it runs in the re-run's fresh workspace, and it is
	// recorded on the step's row.
	h := &fakeHarness{}
	r := &Runner{Lifecycle: h, Prov: lifecycleProv()}
	p := r.RunScenario(context.Background(), lifecycles(t)[scnCorrected], figure.ArmPlatform)
	if p.Err != nil || !p.Passed {
		t.Fatalf("the platform arm: err %v, failures %v", p.Err, p.Failures)
	}
	if len(p.lifecycle.Interventions) != 1 {
		t.Fatalf("%d goals were stepped into, want 1", len(p.lifecycle.Interventions))
	}
	iv := p.lifecycle.Interventions[0]
	if len(iv.Reruns) != 1 {
		t.Fatalf("%d re-runs recorded, want 1", len(iv.Reruns))
	}
	rr := iv.Reruns[0]
	want := memqlengine.StepOverrideGuidance(&common.StepOverride{
		GuidanceAxes: []string{"product"}, GuidanceReason: "It reconciled the monthly ledger; this goal is the weekly one.",
	})
	if !strings.Contains(rr.Prompt, want) || rr.SessionLevel != "strong" {
		t.Fatalf("the re-run's session was started at level %q with the prompt %q; want level strong and the guidance %q", rr.SessionLevel, rr.Prompt, want)
	}
	rec, ok := h.ladders[0].rec.recording(rr.Recording)
	if !ok {
		t.Fatalf("the re-run's recording %s was never opened", rr.Recording)
	}
	if rec.open.ParentStepId != iv.GoalRunId+"-"+appStepKey || !strings.HasSuffix(rec.open.Workspace, "-v2") {
		t.Fatalf("the re-run's recording opened as %+v; it belongs to the step's row, in the re-run's fresh workspace", rec.open)
	}
	// The first session gave the wrong answer; the re-run gave the right one.
	first, _ := h.ladders[0].rec.recording(iv.Recordings[1])
	if got, _ := first.actions[0].Action.Args["command"].(string); got != "reconcile.sh --account acme --period monthly" {
		t.Errorf("the first session ran %q, want the goal's wrong answer", got)
	}
	if got, _ := rec.actions[0].Action.Args["command"].(string); got != "reconcile.sh --account acme --period weekly" {
		t.Errorf("the re-run's session ran %q, want the action as written", got)
	}
}

// --- Negative controls, run on the TEST ------------------------------------------

func TestACorpusThatIgnoresADislikeIsAProcedureTheControlCounts(t *testing.T) {
	// The control's whole point: were the corpus to keep a recording a person
	// disliked, the second goal's lift would find two, and the row assertion
	// that no construct exists is what goes red. The database lane was run with
	// the platform's own rule broken the same way, and failed the same way.
	r := &Runner{Lifecycle: &fakeHarness{knobs: fakeKnobs{ignoreDislikes: true}}, Prov: lifecycleProv()}
	p := r.RunScenario(context.Background(), lifecycles(t)[scnDislikedControl], figure.ArmPlatform)
	if p.Err != nil {
		t.Fatalf("run: %v", p.Err)
	}
	if joined := strings.Join(p.Failures, "\n"); !strings.Contains(joined, "v1:authoring:construct = 1, want exactly 0") {
		t.Fatalf("the control did not see the procedure a disliked corpus teaches:\n%s", joined)
	}
}

func TestAProcedureLearnedFromTheDislikedRecordingFailsTheCorrection(t *testing.T) {
	// With both of the corpus's rules broken -- the replaced version and the
	// disliked one both kept -- the procedure is lifted at the re-run from the
	// wrong recording and the right one, and the correction's check names the
	// wrong one. (Either rule alone keeps the first recording out: it was both
	// replaced and disliked.)
	r := &Runner{Lifecycle: &fakeHarness{knobs: fakeKnobs{ignoreDislikes: true, ignoreSuperseded: true}}, Prov: lifecycleProv()}
	p := r.RunScenario(context.Background(), lifecycles(t)[scnCorrected], figure.ArmPlatform)
	if p.Err != nil {
		t.Fatalf("run: %v", p.Err)
	}
	if joined := strings.Join(p.Failures, "\n"); !strings.Contains(joined, "the recording of version 1, which the person disliked") {
		t.Fatalf("the correction's check did not name the disliked recording:\n%s", joined)
	}
}

func TestAReRunThatLosesTheDislikeIsAProblem(t *testing.T) {
	r := &Runner{Lifecycle: &fakeHarness{knobs: fakeKnobs{dropRerunGuidance: true}}, Prov: lifecycleProv()}
	p := r.RunScenario(context.Background(), lifecycles(t)[scnCorrected], figure.ArmPlatform)
	if p.Err != nil {
		t.Fatalf("run: %v", p.Err)
	}
	if joined := strings.Join(p.Failures, "\n"); !strings.Contains(joined, "was not told what the person disliked about version 1") {
		t.Fatalf("a re-run started without the person's reason passed:\n%s", joined)
	}
}

func TestAVersionTheStoreRewroteIsNotReadable(t *testing.T) {
	r := &Runner{Lifecycle: &fakeHarness{knobs: fakeKnobs{forgetReplacedVersions: true}}, Prov: lifecycleProv()}
	p := r.RunScenario(context.Background(), lifecycles(t)[scnCorrected], figure.ArmPlatform)
	if p.Err != nil {
		t.Fatalf("run: %v", p.Err)
	}
	if joined := strings.Join(p.Failures, "\n"); !strings.Contains(joined, "is not among the versions the step reads back") {
		t.Fatalf("a store that kept only the current version passed:\n%s", joined)
	}
}

func TestAReRunThatLeavesTheHeadWhereItWasIsSeen(t *testing.T) {
	r := &Runner{Lifecycle: &fakeHarness{knobs: fakeKnobs{headStays: true}}, Prov: lifecycleProv()}
	p := r.RunScenario(context.Background(), lifecycles(t)[scnCorrected], figure.ArmPlatform)
	if p.Err != nil {
		t.Fatalf("run: %v", p.Err)
	}
	if joined := strings.Join(p.Failures, "\n"); !strings.Contains(joined, "still reads as current") {
		t.Fatalf("a re-run that did not move the step's head passed:\n%s", joined)
	}
}

// --- The driver's own rules -----------------------------------------------------

func TestAVerdictOnAGoalAProcedureServedIsAProblemNotACrash(t *testing.T) {
	// The loader refuses this wherever it can see it; the driver must still
	// say what happened when the platform serves a goal it did not expect to,
	// rather than judge a step nobody handed to the app.
	s := lifecycles(t)[scnTrusted]
	goals := append([]scenario.ProcedureGoal{}, s.Procedure.Goals...)
	last := goals[len(goals)-1]
	goals = append(goals[:len(goals)-1], scenario.ProcedureGoal{Feedback: &scenario.Feedback{Verdict: scenario.VerdictLike}}, last)
	s.Procedure = &scenario.Procedure{Policy: s.Procedure.Policy, Goals: goals}
	r := &Runner{Lifecycle: &fakeHarness{}, Prov: lifecycleProv()}
	p := r.RunScenario(context.Background(), s, figure.ArmPlatform)
	if p.Err != nil {
		t.Fatalf("run: %v", p.Err)
	}
	if joined := strings.Join(p.Failures, "\n"); !strings.Contains(joined, "it was served by a procedure") {
		t.Fatalf("a verdict on a procedure-served goal was not reported:\n%s", joined)
	}
}

func TestABareLoopHearsAReRunAsTheGoalAskedAgain(t *testing.T) {
	// The baseline has no store for a verdict and no versions to run again,
	// so a re-run is one more session of the goal, told nothing -- and the
	// measured goal is still the app's, which is the comparison.
	s := lifecycles(t)[scnCorrected]
	b := (&Runner{Prov: lifecycleProv()}).RunScenario(context.Background(), s, figure.ArmBaseline)
	if b.Err != nil {
		t.Fatalf("baseline: %v", b.Err)
	}
	sessions := 0
	for _, g := range b.lifecycle.Goals {
		sessions += g.AppCalls
	}
	if want := 7; sessions != want { // six goals and the one re-run
		t.Fatalf("the bare loop ran %d sessions, want %d: every goal and the re-run", sessions, want)
	}
	if b.AppCalls != 1 || b.ReplaysServedWithoutModel != 0 {
		t.Fatalf("the baseline's measured goal: %d session(s), %d served without a model; want 1 and 0", b.AppCalls, b.ReplaysServedWithoutModel)
	}
}

// --- The fixture app -------------------------------------------------------------

func TestTheWrongAnswerIsTheGoalsFirstSessionOnly(t *testing.T) {
	s := lifecycles(t)[scnCorrected]
	rec := &fakeRecorder{runs: map[string]*fakeRecording{}}
	app := newFixtureApp(newProcedureWorld(s, newWorld(s)), s.Steps, rec, "owner-1")
	g := appGoal{OwnerUserId: "owner-1", GoalRunId: "run-1", Statement: s.Goal, Variables: map[string]string{"account": "acme"},
		Answer: map[string]string{"reconcile": "reconcile.sh --account {{account}} --period monthly"}}
	for i, want := range []string{"reconcile.sh --account acme --period monthly", "reconcile.sh --account acme --period weekly"} {
		sess, err := app.Serve(context.Background(), g)
		if err != nil {
			t.Fatalf("session %d: %v", i, err)
		}
		r, _ := rec.recording(sess.RunId)
		if got, _ := r.actions[0].Action.Args["command"].(string); got != want {
			t.Errorf("session %d ran %q, want %q", i, got, want)
		}
		if got, _ := r.actions[1].Action.Args["command"].(string); got != "notify.sh --to ops --account acme" {
			t.Errorf("session %d changed an action the answer does not name: %q", i, got)
		}
	}
	// Another goal's first session gives its own answer, whatever this one's did.
	other := g
	other.GoalRunId = "run-2"
	sess, _ := app.Serve(context.Background(), other)
	r, _ := rec.recording(sess.RunId)
	if got, _ := r.actions[0].Action.Args["command"].(string); got != "reconcile.sh --account acme --period monthly" {
		t.Errorf("a second goal's first session ran %q; the answer is per goal", got)
	}
}

func TestServeStepReadsTheStepsContextAsTheDelegateDoes(t *testing.T) {
	s := lifecycles(t)[scnCorrected]
	rec := &fakeRecorder{runs: map[string]*fakeRecording{}}
	app := newFixtureApp(newProcedureWorld(s, newWorld(s)), s.Steps, rec, "owner-1")
	g := appGoal{OwnerUserId: "owner-1", GoalRunId: "v1:work:run:goal-1", Statement: s.Goal, Variables: map[string]string{"account": "acme"}}
	step := AppStep{RunId: g.GoalRunId, StepKey: appStepKey, StepId: "goal-1-" + appStepKey}

	// No override: the goal as it was asked, on the step's row, in the
	// session's own workspace.
	plain, err := app.ServeStep(common.ContextWithRun(context.Background(), common.RunContext{RunId: g.GoalRunId}), g, step)
	if err != nil {
		t.Fatalf("ServeStep: %v", err)
	}
	r, _ := rec.recording(plain.RunId)
	if plain.Prompt != "Reconcile the weekly ledger for acme and notify the operators" || plain.Level != "" ||
		r.open.ParentStepId != step.StepId || !strings.HasPrefix(r.open.Workspace, "/workspace/fixture-") {
		t.Fatalf("an ordinary step was served as %+v, recorded as %+v", plain, r.open)
	}

	// A re-run's override: the level, the person's instructions, the
	// guidance, and the fresh workspace.
	ov := &common.StepOverride{Level: "strong", Prompt: "Use the weekly period.", GuidanceAxes: []string{"product"}, GuidanceReason: "It was monthly."}
	over, err := app.ServeStep(common.ContextWithRun(context.Background(), common.RunContext{RunId: g.GoalRunId, Override: ov, Workspace: "goal-1-v2"}), g, step)
	if err != nil {
		t.Fatalf("ServeStep: %v", err)
	}
	r, _ = rec.recording(over.RunId)
	for _, want := range []string{"Reconcile the weekly ledger for acme", memqlengine.StepOverrideInstructions(ov), memqlengine.StepOverrideGuidance(ov)} {
		if !strings.Contains(over.Prompt, want) {
			t.Errorf("the overridden session's prompt %q does not carry %q", over.Prompt, want)
		}
	}
	if over.Level != "strong" || r.open.Workspace != "/workspace/goal-1-v2" || r.open.Prompt != over.Prompt {
		t.Errorf("the overridden session: level %q, recorded as %+v", over.Level, r.open)
	}

	// A whole prompt the person wrote replaces the goal's.
	ov = &common.StepOverride{Prompt: "Reconcile the weekly ledger for acme, weekly.", WholePrompt: true}
	whole, _ := app.ServeStep(common.ContextWithRun(context.Background(), common.RunContext{RunId: g.GoalRunId, Override: ov}), g, step)
	if whole.Prompt != ov.Prompt {
		t.Errorf("a whole prompt was not the session's prompt: %q", whole.Prompt)
	}
}

// --- The platform's vocabulary ----------------------------------------------------

func TestTheLoadersVocabularyIsThePlatforms(t *testing.T) {
	// The loader is pure and cannot import component/work, so it keeps its
	// own copy of the verdicts, the axes, the levels and the reason bound;
	// this is what holds the copy to the acts that apply them. A loader that
	// admitted a value the act refuses would find out in the database lane.
	dislikeAxes := work.Axes{Product: true}
	for _, v := range scenario.FeedbackVerdicts() {
		verdict := work.ParseVerdict(v)
		axes := work.Axes{}
		if verdict == work.VerdictDislike {
			axes = dislikeAxes
		}
		if verdict == work.VerdictUnseen || work.ValidateFeedback(verdict, axes, "") != nil {
			t.Errorf("the loader admits the verdict %q and the platform does not", v)
		}
	}
	if work.ValidateFeedback(work.VerdictDislike, work.Axes{}, "") == nil {
		t.Error("the platform accepts a dislike with no axis, and the loader refuses one")
	}
	if got, want := strings.Join(scenario.FeedbackAxes(), ","), strings.Join(work.Axes{Product: true, Process: true, Performance: true}.Names(), ","); got != want {
		t.Errorf("the loader's axes are %s and the platform's %s", got, want)
	}
	for _, a := range scenario.FeedbackAxes() {
		if names := work.AxesFromNames([]string{a}).Names(); len(names) != 1 || names[0] != a {
			t.Errorf("the platform does not read the axis %q the loader admits", a)
		}
	}
	for _, l := range scenario.RerunLevels() {
		if err := work.ValidateOverride(work.Override{Level: l}); err != nil {
			t.Errorf("the loader admits the level %q and the platform refuses it: %v", l, err)
		}
	}
	if work.ValidateOverride(work.Override{Level: "embeddings"}) == nil {
		t.Error("the platform accepts a re-run at the embeddings level, and the loader refuses one")
	}
	fits := strings.Repeat("x", scenario.MaxFeedbackReasonBytes)
	if work.ValidateFeedback(work.VerdictLike, work.Axes{}, fits) != nil || work.ValidateFeedback(work.VerdictLike, work.Axes{}, fits+"x") == nil {
		t.Errorf("the platform's reason bound is not the loader's %d bytes", scenario.MaxFeedbackReasonBytes)
	}
}

// --- The correction's two checks, over hand-built records -------------------------

// correctedRecord is a correction every part of which holds: version 1
// disliked and replaced, version 2 liked and current, and a procedure learned
// from version 2's recording and another goal's, whose steps are the actions
// as written. Built fresh per call, for one test to break one part of.
func correctedRecord() (*lifecycleRecord, scenario.Scenario) {
	s := scenario.Scenario{Steps: []scenario.Step{
		{Key: "reconcile", Type: "exec", Target: "reconcile.sh --account {{account}} --period weekly", Effect: scenario.FacetMachine},
		{Key: "notify", Type: "exec", Target: "notify.sh --to ops --account {{account}}", Effect: scenario.FacetMachine},
	}}
	goal := appGoal{Statement: "Reconcile the weekly ledger for {{account}}", Variables: map[string]string{"account": "acme"},
		Answer: map[string]string{"reconcile": "reconcile.sh --account {{account}} --period monthly"}}
	iv := &interventionRecord{
		Goal: 0, GoalRunId: "v1:work:run:goal-1", goal: goal, Current: 2,
		Recordings: map[int]string{1: "v1:work:run:rec-1", 2: "v1:work:run:rec-2"},
		Verdicts: []verdictSeen{
			{Version: 1, Verdict: scenario.VerdictDislike, Axes: []string{"product"}, Reason: "It was monthly."},
			{Version: 2, Verdict: scenario.VerdictLike},
		},
		Reruns: []rerunSeen{{From: 1, Version: 2, Level: "strong"}},
		Versions: []StepVersionState{
			{StepKey: appStepKey, Version: 1, Status: "done", ChildRunId: "v1:work:run:rec-1"},
			// The bare spelling, as a row may carry it.
			{StepKey: appStepKey, Version: 2, Status: "done", Current: true, ChildRunId: "rec-2"},
		},
	}
	rec := &lifecycleRecord{Measured: -1, Interventions: []*interventionRecord{iv}, Constructs: []ConstructState{{
		ConstructId: "v1:authoring:construct:c1", Rung: "trusted",
		GoalSignature: work.GoalSignature(goal.Statement, inputKeys(goal.Variables)),
		RecordedFrom:  []string{"rec-2", "v1:work:run:rec-3"},
		// Quoted as the runner's writer quotes a parameter.
		Steps: []string{"reconcile.sh --account '{{account}}' --period weekly", "notify.sh --to ops --account {{account}}"},
	}}}
	return rec, s
}

func TestALiftedProcedureCameFromTheLikedVersionNamesWhatDidNotHold(t *testing.T) {
	rec, s := correctedRecord()
	if msg := checkLiftedFromTheLikedVersion(rec, s); msg != "" {
		t.Fatalf("the fixture correction itself fails: %s", msg)
	}
	for _, tc := range []struct {
		name   string
		break_ func(*lifecycleRecord)
		want   string
	}{
		{"nothing liked", func(r *lifecycleRecord) { r.Interventions[0].Verdicts[1].Verdict = scenario.VerdictNeutral }, "no liked version for a procedure to have come from"},
		{"the disliked version never replaced", func(r *lifecycleRecord) { r.Interventions[0].Current = 1 }, "no liked version for a procedure to have come from"},
		{"no procedure for the goal", func(r *lifecycleRecord) { r.Constructs[0].GoalSignature = "another goal" }, "no learned procedure exists"},
		{"a procedure nobody could read", func(r *lifecycleRecord) { r.Constructs[0].Unreadable = "decode failed" }, "could not be read"},
		{"not learned from the liked recording", func(r *lifecycleRecord) {
			r.Constructs[0].RecordedFrom = []string{"v1:work:run:rec-3", "v1:work:run:rec-4"}
		}, "was not learned from v1:work:run:rec-2, the recording of version 2, which the person liked"},
		{"learned from the disliked recording", func(r *lifecycleRecord) {
			r.Constructs[0].RecordedFrom = []string{"rec-1", "v1:work:run:rec-2"}
		}, "the recording of version 1, which the person disliked"},
		{"a step short", func(r *lifecycleRecord) { r.Constructs[0].Steps = r.Constructs[0].Steps[:1] }, "has 1 step(s) and the fixture app performs 2"},
		{"the wrong answer in a step", func(r *lifecycleRecord) {
			r.Constructs[0].Steps[0] = "reconcile.sh --account {{account}} --period monthly"
		}, "is the wrong answer the person disliked"},
		{"a step that is some other action", func(r *lifecycleRecord) {
			r.Constructs[0].Steps[0] = "reconcile.sh --account {{account}} --period {{?h2}}"
		}, "the corrected action is reconcile.sh --account acme --period weekly"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, s := correctedRecord()
			tc.break_(rec)
			msg := checkLiftedFromTheLikedVersion(rec, s)
			if !strings.Contains(msg, tc.want) {
				t.Fatalf("check = %q\nwant it to contain %q", msg, tc.want)
			}
		})
	}
	if msg := checkLiftedFromTheLikedVersion(nil, s); msg == "" {
		t.Fatal("the check passed with no lifecycle at all")
	}
}

func TestALikedVersionRunAgainIsNotOneAProcedureMustComeFrom(t *testing.T) {
	// A person may like a version and run the step again anyway. The corpus
	// leaves the replaced version out whatever its verdict (D18), so the liked
	// version a procedure must have come from is the CURRENT one -- asking
	// for every liked version would fail a platform that did what it should.
	rec, s := correctedRecord()
	iv := rec.Interventions[0]
	iv.Current = 3
	iv.Recordings[3] = "v1:work:run:rec-2b"
	iv.Verdicts = append(iv.Verdicts, verdictSeen{Version: 3, Verdict: scenario.VerdictLike})
	iv.Versions = append(iv.Versions, StepVersionState{StepKey: appStepKey, Version: 3, Status: "done", Current: true, ChildRunId: "v1:work:run:rec-2b"})
	iv.Versions[1].Current = false
	rec.Constructs[0].RecordedFrom = []string{"v1:work:run:rec-2b", "v1:work:run:rec-3"}
	if msg := checkLiftedFromTheLikedVersion(rec, s); msg != "" {
		t.Fatalf("a procedure learned from the current, liked version failed: %s", msg)
	}
	rec.Constructs[0].RecordedFrom = []string{"v1:work:run:rec-2", "v1:work:run:rec-3"}
	if msg := checkLiftedFromTheLikedVersion(rec, s); !strings.Contains(msg, "the recording of version 3, which the person liked") {
		t.Fatalf("a procedure learned from the replaced version and not the current one passed: %q", msg)
	}
}

func TestTheDislikedVersionIsStillReadableNamesWhatDidNotHold(t *testing.T) {
	rec, _ := correctedRecord()
	if msg := checkDislikedVersionReadable(rec); msg != "" {
		t.Fatalf("the fixture correction itself fails: %s", msg)
	}
	for _, tc := range []struct {
		name   string
		break_ func(*lifecycleRecord)
		want   string
	}{
		{"nothing disliked and replaced", func(r *lifecycleRecord) { r.Interventions[0].Verdicts[0].Verdict = scenario.VerdictLike }, "no replaced version was left to read"},
		{"the disliked version gone", func(r *lifecycleRecord) { r.Interventions[0].Versions = r.Interventions[0].Versions[1:] }, "version 1 of goal 0's step is not among the versions"},
		{"the disliked version unfinished", func(r *lifecycleRecord) { r.Interventions[0].Versions[0].Status = "running" }, "reads back running, not done"},
		{"the disliked version still current", func(r *lifecycleRecord) { r.Interventions[0].Versions[0].Current = true }, "still reads as current"},
		{"the disliked version naming another session", func(r *lifecycleRecord) {
			r.Interventions[0].Versions[0].ChildRunId = "v1:work:run:rec-2"
		}, "as the session that answered it, not v1:work:run:rec-1"},
		{"the re-run's version gone", func(r *lifecycleRecord) { r.Interventions[0].Versions = r.Interventions[0].Versions[:1] }, "the re-run's, is not among the versions"},
		{"the re-run's version not current", func(r *lifecycleRecord) { r.Interventions[0].Versions[1].Current = false }, "the re-run's, is not the current one"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, _ := correctedRecord()
			tc.break_(rec)
			msg := checkDislikedVersionReadable(rec)
			if !strings.Contains(msg, tc.want) {
				t.Fatalf("check = %q\nwant it to contain %q", msg, tc.want)
			}
		})
	}
	if msg := checkDislikedVersionReadable(nil); msg == "" {
		t.Fatal("the check passed with no lifecycle at all")
	}
}
