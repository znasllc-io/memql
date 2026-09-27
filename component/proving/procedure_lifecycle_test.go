package proving

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/proving/figure"
	"github.com/znasllc-io/memql/component/proving/scenario"
	"github.com/znasllc-io/memql/component/proving/scorecard"
	"github.com/znasllc-io/memql/component/proving/world"
	"github.com/znasllc-io/memql/component/work"
	workerservice "github.com/znasllc-io/memql/component/worker"
)

// The committed lifecycle scenarios, by id. Named here so a test that means
// one of them cannot silently start running another.
const (
	scnTrusted        = "amortizedCost.a-trusted-procedure-replays-without-a-model"
	scnShadowOnly     = "amortizedCost.control-a-shadow-procedure-serves-no-goal"
	scnFreshGoal      = "amortizedCost.control-a-fresh-goal-reaches-a-model"
	scnDivergence     = "durability.a-divergence-duplicates-no-side-effect"
	scnDivergeControl = "durability.control-a-divergence-without-guidance-duplicates"
)

func lifecycleProv() figure.Provenance {
	return figure.Provenance{Commit: "abc1234", Date: "2026-09-26", Tier: figure.TierCI, Runner: "test"}
}

// lifecycles returns the committed corpus's lifecycle scenarios, by id.
func lifecycles(t *testing.T) map[string]scenario.Scenario {
	t.Helper()
	out := map[string]scenario.Scenario{}
	for _, s := range loadCorpus(t).Scenarios {
		if s.Procedure != nil {
			out[s.Id] = s
		}
	}
	for _, id := range []string{scnTrusted, scnShadowOnly, scnFreshGoal, scnDivergence, scnDivergeControl} {
		if _, ok := out[id]; !ok {
			t.Fatalf("the committed corpus has no lifecycle scenario %s", id)
		}
	}
	return out
}

func runBoth(t *testing.T, r *Runner, s scenario.Scenario) (platform, baseline ArmResult) {
	t.Helper()
	ctx := context.Background()
	platform = r.RunScenario(ctx, s, figure.ArmPlatform)
	baseline = r.RunScenario(ctx, s, figure.ArmBaseline)
	for _, res := range []ArmResult{platform, baseline} {
		if res.Err != nil {
			t.Fatalf("%s/%s could not run: %v", s.Id, res.Arm, res.Err)
		}
	}
	return platform, baseline
}

// figureOf returns the one median a scenario published for a metric on an arm.
func figureOf(t *testing.T, entries []scorecard.Entry, m figure.Metric, arm figure.Arm) float64 {
	t.Helper()
	for _, e := range entries {
		if e.Figure.Metric == m && e.Arm == arm {
			if !e.Figure.IsMeasured() {
				t.Fatalf("%s on %s is unmeasured (%s)", m, arm, e.Figure.Absent)
			}
			return e.Figure.Stat.Median
		}
	}
	t.Fatalf("no %s figure on the %s arm", m, arm)
	return 0
}

// --- The committed lifecycles, end to end ----------------------------------------

func TestEveryCommittedLifecyclePassesItsVerifierAgainstAWellBehavedLadder(t *testing.T) {
	// The corpus files run through the real driver, the real fixture app, the
	// real world and the real ladder arithmetic (component/work.Advance), with
	// only the runner's rows faked. A lifecycle file that could not pass here
	// -- a verifier count that is wrong, a goal the ladder cannot reach -- would
	// otherwise be found only in the database lane.
	all := lifecycles(t)
	var zeroControls, nonZeroControls, headlines int
	for id, s := range all {
		t.Run(id, func(t *testing.T) {
			r := &Runner{Lifecycle: &fakeHarness{}, Prov: lifecycleProv()}
			p, b := runBoth(t, r, s)
			if !p.Passed || len(p.Failures) > 0 {
				t.Fatalf("the platform arm failed its verifier: %s\nthe lifecycle: %s", strings.Join(p.Failures, "; "), p.lifecycle.narrative())
			}
			entries, _, err := Figures(s, map[figure.Arm]ArmResult{figure.ArmPlatform: p, figure.ArmBaseline: b}, nil, r.Prov)
			if err != nil {
				t.Fatalf("Figures: %v", err)
			}
			if msg := checkNegativeControl(s, entries); msg != "" {
				t.Fatalf("the control failed: %s", msg)
			}
		})
		switch spec, _ := figure.MetricSpec(s.NegativeControlFor); {
		case s.NegativeControlFor == "":
			headlines++
		case spec.Control() == figure.ControlZero:
			zeroControls++
		case spec.Control() == figure.ControlNonZero:
			nonZeroControls++
		}
	}
	// A uniform set would exercise one side of the control rule only.
	if headlines == 0 || zeroControls == 0 || nonZeroControls == 0 {
		t.Fatalf("the lifecycle corpus lacks a side (headlines %d, zero-reading controls %d, non-zero controls %d)", headlines, zeroControls, nonZeroControls)
	}
}

func TestTheLifecycleFiguresReadWhatTheRecordSays(t *testing.T) {
	// The numbers the scorecard will carry, pinned: each figure is the one
	// the program record says the scenario measures, on both arms.
	all := lifecycles(t)
	for _, tc := range []struct {
		scenario           string
		metric             figure.Metric
		platform, baseline float64
	}{
		{scnTrusted, figure.MetricReplaysWithoutModel, 1, 0},
		{scnTrusted, figure.MetricProviderCalls, 0, 1},
		{scnShadowOnly, figure.MetricReplaysWithoutModel, 0, 0},
		{scnFreshGoal, figure.MetricProviderCalls, 1, 1},
		{scnDivergence, figure.MetricDuplicatedAcrossDivergence, 0, 1},
		{scnDivergeControl, figure.MetricDuplicatedAcrossDivergence, 0, 1},
	} {
		t.Run(tc.scenario+"/"+string(tc.metric), func(t *testing.T) {
			s := all[tc.scenario]
			r := &Runner{Lifecycle: &fakeHarness{}, Prov: lifecycleProv()}
			p, b := runBoth(t, r, s)
			entries, _, err := Figures(s, map[figure.Arm]ArmResult{figure.ArmPlatform: p, figure.ArmBaseline: b}, nil, r.Prov)
			if err != nil {
				t.Fatalf("Figures: %v", err)
			}
			if got := figureOf(t, entries, tc.metric, figure.ArmPlatform); got != tc.platform {
				t.Errorf("platform = %v, want %v (the lifecycle: %s)", got, tc.platform, p.lifecycle.narrative())
			}
			if got := figureOf(t, entries, tc.metric, figure.ArmBaseline); got != tc.baseline {
				t.Errorf("baseline = %v, want %v (the lifecycle: %s)", got, tc.baseline, b.lifecycle.narrative())
			}
		})
	}
}

func TestTheSuiteOverTheLifecyclesBlocksNothing(t *testing.T) {
	// Through Run, so the corpus rule, the figures and both directions of
	// checkNegativeControl are asked exactly as the lane asks them.
	var subset []scenario.Scenario
	for _, s := range loadCorpus(t).Scenarios {
		if s.Procedure != nil {
			subset = append(subset, s)
		}
	}
	r := &Runner{Lifecycle: &fakeHarness{}, Prov: lifecycleProv()}
	res, err := r.Run(context.Background(), scenario.Corpus{Scenarios: subset, Fingerprint: "test"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if b := res.Blocking(); len(b) > 0 {
		t.Fatalf("the lifecycles block the suite: %s", strings.Join(b, "\n"))
	}
}

func TestTheHarnessIsGivenTheScenariosValuesAndPutsThemBack(t *testing.T) {
	// The scenario's ladder values are an OVERLAY: what it writes, and zero
	// for what it leaves to the deployment. And every platform run puts the
	// row back, so a shared database is not left on a benchmark's values.
	h := &fakeHarness{}
	r := &Runner{Lifecycle: h, Prov: lifecycleProv()}
	p, _ := runBoth(t, r, lifecycles(t)[scnTrusted])
	if !p.Passed {
		t.Fatalf("the platform arm failed: %v", p.Failures)
	}
	want := work.LadderPolicy{ShadowMatches: 2, DistinctBindings: 2, CanaryMatches: 1}
	if len(h.opened) != 1 || h.opened[0] != want {
		t.Fatalf("Open was given %+v, want exactly one overlay %+v", h.opened, want)
	}
	if h.closed != 1 {
		t.Fatalf("the platform was put back %d time(s), want 1", h.closed)
	}
}

func TestEveryGoalRunIsClosedAndEveryReplayKnowsItsGoal(t *testing.T) {
	// A goal run left `running` reads as work still in flight to every sweep
	// and every reader; and a replay that is not told its goal, its statement
	// and the statement that asked for it cannot key its own run, so a
	// resumed goal would replay twice.
	h := &fakeHarness{}
	r := &Runner{Lifecycle: h, Prov: lifecycleProv()}
	if p, _ := runBoth(t, r, lifecycles(t)[scnDivergence]); !p.Passed {
		t.Fatalf("the platform arm failed: %v", p.Failures)
	}
	if len(h.ladders) != 1 {
		t.Fatalf("%d ladders were built, want 1", len(h.ladders))
	}
	l := h.ladders[0]
	opened, closed := len(l.spine.goals), len(l.spine.closed)
	if opened == 0 || opened != closed {
		t.Fatalf("%d goal runs opened and %d closed; every one must be closed", opened, closed)
	}
	for run, served := range l.spine.closed {
		if !served {
			t.Errorf("goal run %s was closed as not served, and every goal of this lifecycle was served -- the last by the app's repair", run)
		}
	}
	if len(l.orders) == 0 {
		t.Fatal("no replay was asked for, so this test checked nothing")
	}
	for _, o := range l.orders {
		if o.StepKey != replayStatementKey || o.GoalId == "" || o.Statement == "" || o.GoalRunId == "" {
			t.Errorf("a replay was asked for as %+v; it needs its goal, its statement and the statement key %q", o, replayStatementKey)
		}
		if g := l.spine.goals[o.GoalRunId]; g.construct != o.ConstructId {
			t.Errorf("goal run %s names construct %q and was replayed from %q; the run must name what serves it", o.GoalRunId, g.construct, o.ConstructId)
		}
	}
}

// --- Negative controls, run on the TEST ------------------------------------------

func TestGuidanceThatNamesNoCompletedStepIsADuplicateTheFigureAndTheCheckBothSee(t *testing.T) {
	// The durability zero must be BECAUSE of the guidance. A runner that
	// hands the goal back without naming the step it delivered makes the app
	// redo it, and both the figure and the named check must say so.
	s := lifecycles(t)[scnDivergence]
	r := &Runner{Lifecycle: &fakeHarness{knobs: fakeKnobs{dropGuidance: true}}, Prov: lifecycleProv()}
	p := r.RunScenario(context.Background(), s, figure.ArmPlatform)
	if p.Err != nil {
		t.Fatalf("run: %v", p.Err)
	}
	if p.DuplicatedAcrossDivergence != 1 {
		t.Fatalf("DuplicatedAcrossDivergence = %d, want 1: the app redid the delivered step and the world must have seen it", p.DuplicatedAcrossDivergence)
	}
	joined := strings.Join(p.Failures, "\n")
	for _, want := range []string{"does not name it", "machine.duplicates = 1, want exactly 0"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the verifier did not say %q; it said:\n%s", want, joined)
		}
	}
}

func TestAPromotionThatDoesNotMoveTheLadderLeavesTheLastGoalWithTheApp(t *testing.T) {
	// The headline's one is the ladder's doing: a runner that records the
	// person's approval and never moves the construct must leave the last
	// goal with the app, and the figure must read zero.
	s := lifecycles(t)[scnTrusted]
	r := &Runner{Lifecycle: &fakeHarness{knobs: fakeKnobs{ignoreDecisions: true}}, Prov: lifecycleProv()}
	p := r.RunScenario(context.Background(), s, figure.ArmPlatform)
	if p.Err != nil {
		t.Fatalf("run: %v", p.Err)
	}
	if p.ReplaysServedWithoutModel != 0 || p.AppCalls != 1 {
		t.Fatalf("ReplaysServedWithoutModel = %d, AppCalls = %d; want 0 and 1 when the ladder never promoted", p.ReplaysServedWithoutModel, p.AppCalls)
	}
	if joined := strings.Join(p.Failures, "\n"); !strings.Contains(joined, "was not replayed at all") {
		t.Errorf("the named check did not say the goal was never replayed:\n%s", joined)
	}
}

func TestAReplayThatReportsAModelCallIsNotServedWithoutOne(t *testing.T) {
	s := lifecycles(t)[scnTrusted]
	r := &Runner{Lifecycle: &fakeHarness{knobs: fakeKnobs{replayModelCalls: 1}}, Prov: lifecycleProv()}
	p := r.RunScenario(context.Background(), s, figure.ArmPlatform)
	if p.Err != nil {
		t.Fatalf("run: %v", p.Err)
	}
	if p.ReplaysServedWithoutModel != 0 {
		t.Fatalf("a replay reporting a model call was counted as served without one")
	}
	if joined := strings.Join(p.Failures, "\n"); !strings.Contains(joined, "reported 1 model call(s)") {
		t.Errorf("the named check did not name the model call:\n%s", joined)
	}
}

func TestTheZeroReadingControlCatchesACounterThatReadsItsClaimWithoutAReplay(t *testing.T) {
	// The second direction's whole reason. A procedure still on shadow that
	// is SERVED -- a rung misread, a serve decision bypassed -- makes the
	// shadow-only control read one, and the control must fail rather than
	// pass: that one is a replay nobody certified.
	s := lifecycles(t)[scnShadowOnly]
	r := &Runner{Lifecycle: &fakeHarness{knobs: fakeKnobs{serveShadow: true}}, Prov: lifecycleProv()}
	p, b := runBoth(t, r, s)
	entries, _, err := Figures(s, map[figure.Arm]ArmResult{figure.ArmPlatform: p, figure.ArmBaseline: b}, nil, r.Prov)
	if err != nil {
		t.Fatalf("Figures: %v", err)
	}
	msg := checkNegativeControl(s, entries)
	if !strings.Contains(msg, "reads its claim whether or not the event occurred") {
		t.Fatalf("checkNegativeControl = %q, want the zero-reading control to fail", msg)
	}
}

// --- The driver's own rules ---------------------------------------------------------

func TestALifecycleWithNoHarnessIsARunnerErrorAndNotASkip(t *testing.T) {
	// A lifecycle figure from a lifecycle nothing drove is the one thing the
	// driver must never publish. The baseline needs no platform and runs.
	s := lifecycles(t)[scnTrusted]
	r := &Runner{Prov: lifecycleProv()}
	p := r.RunScenario(context.Background(), s, figure.ArmPlatform)
	if !errors.Is(p.Err, errNoLifecycleHarness) {
		t.Fatalf("Err = %v, want errNoLifecycleHarness", p.Err)
	}
	if b := r.RunScenario(context.Background(), s, figure.ArmBaseline); b.Err != nil {
		t.Fatalf("the baseline needs no platform and failed: %v", b.Err)
	}
}

func TestADecisionWithNothingProposedIsAVerifierFailure(t *testing.T) {
	// The scenario ran, and the platform's answer was wrong: that is a
	// verifier failure on the platform arm, not a runner error.
	s := lifecycles(t)[scnTrusted]
	s.Procedure = &scenario.Procedure{Policy: s.Procedure.Policy, Goals: []scenario.ProcedureGoal{
		{Variables: map[string]string{"account": "acme"}},
		{Decide: scenario.DecideApproved},
		{Variables: map[string]string{"account": "globex"}, Measure: true},
	}}
	s.Verify = []scenario.Check{{Effects: "machine.duplicates", Count: intPtr(0)}}
	s.Claims = []figure.Metric{figure.MetricProviderCalls}
	if err := s.Validate(); err != nil {
		t.Fatalf("the test's scenario is invalid: %v", err)
	}
	r := &Runner{Lifecycle: &fakeHarness{}, Prov: lifecycleProv()}
	p := r.RunScenario(context.Background(), s, figure.ArmPlatform)
	if p.Err != nil {
		t.Fatalf("run: %v", p.Err)
	}
	if joined := strings.Join(p.Failures, "\n"); !strings.Contains(joined, "the ladder proposed none") {
		t.Fatalf("the failures do not name the missing proposal:\n%s", joined)
	}
}

func TestServedWithoutModelNeedsEveryOneOfItsConditions(t *testing.T) {
	good := func() *goalRecord {
		return &goalRecord{Rung: string(work.RungTrusted), ServedBy: servedByProcedure, Replay: &ReplayReport{Served: true}}
	}
	if !servedWithoutModel(good()) {
		t.Fatal("the fixture itself does not count, so every case below would pass for the wrong reason")
	}
	for name, breakIt := range map[string]func(*goalRecord){
		"no replay":           func(g *goalRecord) { g.Replay = nil },
		"a canary replay":     func(g *goalRecord) { g.Rung = string(work.RungCanary) },
		"served by the app":   func(g *goalRecord) { g.ServedBy = servedByApp },
		"a model call":        func(g *goalRecord) { g.Replay.ModelCalls = 1 },
		"the app was reached": func(g *goalRecord) { g.AppCalls = 1 },
	} {
		g := good()
		breakIt(g)
		if servedWithoutModel(g) {
			t.Errorf("%s still counts as served without a model", name)
		}
	}
}

func TestCheckNegativeControlReadsEachDirectionOnItsOwnArm(t *testing.T) {
	entry := func(m figure.Metric, arm figure.Arm, v float64) scorecard.Entry {
		f, err := figure.Measured(m, []float64{v}, lifecycleProv())
		if err != nil {
			t.Fatalf("Measured: %v", err)
		}
		return scorecard.Entry{Scenario: "x", Arm: arm, Figure: f}
	}
	lower := scenario.Scenario{Id: "d.control", NegativeControlFor: figure.MetricDuplicatedAcrossDivergence}
	higher := scenario.Scenario{Id: "a.control", NegativeControlFor: figure.MetricReplaysWithoutModel}
	for _, tc := range []struct {
		name    string
		s       scenario.Scenario
		entries []scorecard.Entry
		fails   string
	}{
		// Lower is better: the baseline must rise.
		{"lower, baseline rose", lower, []scorecard.Entry{entry(lower.NegativeControlFor, figure.ArmBaseline, 1), entry(lower.NegativeControlFor, figure.ArmPlatform, 0)}, ""},
		{"lower, baseline stayed at zero", lower, []scorecard.Entry{entry(lower.NegativeControlFor, figure.ArmBaseline, 0), entry(lower.NegativeControlFor, figure.ArmPlatform, 1)}, "measured ZERO"},
		// Higher is better: the PLATFORM must stay at zero, whatever the
		// baseline reads.
		{"higher, platform stayed at zero", higher, []scorecard.Entry{entry(higher.NegativeControlFor, figure.ArmPlatform, 0), entry(higher.NegativeControlFor, figure.ArmBaseline, 1)}, ""},
		{"higher, platform read its claim", higher, []scorecard.Entry{entry(higher.NegativeControlFor, figure.ArmPlatform, 1), entry(higher.NegativeControlFor, figure.ArmBaseline, 0)}, "reads its claim"},
		{"higher, no platform figure", higher, []scorecard.Entry{entry(higher.NegativeControlFor, figure.ArmBaseline, 0)}, "produced no"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := checkNegativeControl(tc.s, tc.entries)
			switch {
			case tc.fails == "" && msg != "":
				t.Fatalf("checkNegativeControl = %q, want a pass", msg)
			case tc.fails != "" && !strings.Contains(msg, tc.fails):
				t.Fatalf("checkNegativeControl = %q, want it to contain %q", msg, tc.fails)
			}
		})
	}
	// The existing lower-is-better exemptions still read the platform.
	for _, m := range []figure.Metric{figure.MetricCompileCallsExact, figure.MetricRecoveryCalls} {
		if got := controlArm(m); got != figure.ArmPlatform {
			t.Errorf("%s's control is read on %s, want platform", m, got)
		}
	}
	if got := controlArm(figure.MetricDuplicatedEffects); got != figure.ArmBaseline {
		t.Errorf("durability.duplicatedSideEffects's control is read on %s, want baseline", got)
	}
}

func TestALifecyclesProviderCallsAreTheAppsSessions(t *testing.T) {
	// A lifecycle plays no cassette, so its recorded-response count is not a
	// model count at all; the app's sessions are.
	s := lifecycles(t)[scnTrusted]
	res := map[figure.Arm]ArmResult{
		figure.ArmPlatform: {Scenario: s.Id, Arm: figure.ArmPlatform, AppCalls: 0, RecordedResponses: 5, ReplaysServedWithoutModel: 1},
		figure.ArmBaseline: {Scenario: s.Id, Arm: figure.ArmBaseline, AppCalls: 1, RecordedResponses: 5},
	}
	entries, _, err := Figures(s, res, nil, lifecycleProv())
	if err != nil {
		t.Fatalf("Figures: %v", err)
	}
	if got := figureOf(t, entries, figure.MetricProviderCalls, figure.ArmPlatform); got != 0 {
		t.Errorf("platform providerCalls = %v, want the app's 0 sessions, not the 5 recorded responses", got)
	}
	if got := figureOf(t, entries, figure.MetricProviderCalls, figure.ArmBaseline); got != 1 {
		t.Errorf("baseline providerCalls = %v, want 1", got)
	}
}

func TestEveryLifecycleRowTheLoaderAdmitsIsAnswered(t *testing.T) {
	// The loader's vocabulary and the runner's answers are two lists; this
	// holds them together, so a key the loader admits can never reach the
	// lane and answer "cannot answer".
	rec := &lifecycleRecord{
		Constructs: []ConstructState{{ConstructId: "c1", Rung: "trusted"}},
		Approvals:  []ApprovalState{{ApprovalId: "a1", Decision: ""}, {ApprovalId: "a2", Decision: "approved"}},
	}
	for row, keys := range scenario.LifecycleRows() {
		for key, values := range keys {
			for _, v := range values {
				if _, ok := lifecycleRowCount(scenario.Check{Rows: row, Where: map[string]string{key: v}}, rec); !ok {
					t.Errorf("the loader admits %s where %s=%s and the runner cannot answer it", row, key, v)
				}
			}
		}
	}
	if n, _ := lifecycleRowCount(scenario.Check{Rows: scenario.RowApproval, Where: map[string]string{"decision": scenario.DecisionPending}}, rec); n != 1 {
		t.Errorf("pending approvals = %d, want 1: an undecided approval is spelled pending", n)
	}
	if _, ok := lifecycleRowCount(scenario.Check{Rows: "v1:work:run", Where: map[string]string{"status": "succeeded"}}, rec); ok {
		t.Error("a lifecycle answered a journal row it does not keep")
	}
}

// --- The fixture app ----------------------------------------------------------------

func fixtureScenario(t *testing.T) scenario.Scenario {
	t.Helper()
	return lifecycles(t)[scnDivergence]
}

func TestTheFixtureAppRecordsEveryActionAsTheCockpitWould(t *testing.T) {
	// The recording is the learner's only input, so its shape is the
	// contract: the session opens beneath the goal's run (the goal signature
	// and input ride down from there), every action is one event with its
	// arguments whole and its exit code, the fingerprint rides on the first
	// action only, and the close carries the app's model and its accounting.
	s := fixtureScenario(t)
	rec := &fakeRecorder{runs: map[string]*fakeRecording{}}
	pw := newProcedureWorld(s, newWorld(s))
	app := newFixtureApp(pw, s.Steps, rec, "owner-1")
	sess, err := app.Serve(context.Background(), appGoal{OwnerUserId: "owner-1", GoalRunId: "v1:work:run:goal-1", Statement: s.Goal, Variables: map[string]string{"account": "acme"}})
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	r, ok := rec.recording(sess.RunId)
	if !ok {
		t.Fatalf("no recording %q", sess.RunId)
	}
	if r.open.ParentRunId != "v1:work:run:goal-1" || r.open.OwnerUserId != "owner-1" || r.open.App != fixtureAppId {
		t.Errorf("the recording opened as %+v", r.open)
	}
	if len(r.actions) != len(s.Steps) {
		t.Fatalf("%d actions recorded, want %d", len(r.actions), len(s.Steps))
	}
	for i, a := range r.actions {
		want := scenario.Render(s.Steps[i].Target, map[string]string{"account": "acme"})
		switch {
		case a.Seq != i+1 || a.Action.Seq != uint64(i+1):
			t.Errorf("action %d carries seq %d/%d", i, a.Seq, a.Action.Seq)
		case a.Action.Tool != "exec" || a.Action.Args["command"] != want:
			t.Errorf("action %d = %s %v, want exec %q", i, a.Action.Tool, a.Action.Args, want)
		case a.Action.ExitCode == nil || *a.Action.ExitCode != 0 || a.Action.IsError:
			t.Errorf("action %d reported exit %v error %v", i, a.Action.ExitCode, a.Action.IsError)
		case a.Action.ResultType == "" || a.Action.ResultDigest == "":
			t.Errorf("action %d reported no result type or digest", i)
		case (i == 0) != (len(a.Fingerprint) > 0):
			t.Errorf("action %d fingerprint present = %v; it belongs on the first action only", i, len(a.Fingerprint) > 0)
		}
	}
	if fp := r.actions[0].Fingerprint; fp["cwdEntries"] != 0 || fp["cwd"] != r.actions[0].Action.Cwd {
		t.Errorf("the fingerprint does not describe the session's empty workspace: %v", fp)
	}
	if c := r.close; c == nil || c.Status != workerservice.AppSessionStatusEnded || c.RecordedActions != len(s.Steps) || c.Model != fixtureModel {
		t.Errorf("the recording closed as %+v", c)
	}
	if app.Calls() != 1 {
		t.Errorf("one session counted %d model reaches, want 1", app.Calls())
	}
}

func TestTheFixtureAppHonoursOnlyGuidanceNamingItsOwnSteps(t *testing.T) {
	// A guidance entry is honoured when its index AND tool are this app's
	// action; one naming some other step is not honoured by accident -- the
	// app redoes the action, and the world counts it.
	s := fixtureScenario(t)
	pw := newProcedureWorld(s, newWorld(s))
	app := newFixtureApp(pw, s.Steps, nil, "owner-1")
	g := appGoal{OwnerUserId: "owner-1", GoalRunId: "run-1", Statement: s.Goal, Variables: map[string]string{"account": "acme"}}
	app.expect(g)
	if _, err := app.Handover(context.Background(), HandoverOrder{GoalRunId: "run-1", Completed: []CompletedReport{{Index: 0, Tool: "exec"}}}); err != nil {
		t.Fatalf("Handover: %v", err)
	}
	if got := app.handoversSince(0)[0].Session.Performed; len(got) != len(s.Steps)-1 || got[0] != 1 {
		t.Fatalf("performed %v, want every action but the first", got)
	}
	if _, err := app.Handover(context.Background(), HandoverOrder{GoalRunId: "run-1", Completed: []CompletedReport{{Index: 0, Tool: "fs_write"}}}); err != nil {
		t.Fatalf("Handover: %v", err)
	}
	if got := app.handoversSince(1)[0].Session.Performed; len(got) != len(s.Steps) {
		t.Fatalf("a guidance naming another tool was honoured: performed %v", got)
	}
	if _, err := app.Handover(context.Background(), HandoverOrder{GoalRunId: "never-given"}); err == nil {
		t.Fatal("the app accepted a goal it was never given")
	}
}

func TestTheWorldHearsOneCommandHoweverAReplayQuotesIt(t *testing.T) {
	// A replay materializes a command from its template and may quote it
	// differently from the app (component/procedure.Materialize single-quotes
	// an element that needs it). The world keys a delivery on the argument
	// vector, so the two spellings are one delivery -- a duplicate it missed
	// for a quoting difference would be a zero measured by accident.
	s := fixtureScenario(t)
	w := newWorld(s)
	pw := newProcedureWorld(s, w)
	pw.exec("notify.sh --to ops --account 'acme corp'", "k1", false)
	pw.exec(`notify.sh --to ops --account "acme corp"`, "k2", false)
	pw.exec(`notify.sh --to ops --account acme\ corp`, "k3", false)
	if got := w.Duplicates(); got != 2 {
		t.Fatalf("Duplicates = %d, want 2: three spellings of one command are one delivery made three times", got)
	}
	for in, want := range map[string][]string{
		`a 'b c' 'd'\''e' ''`: {"a", "b c", "d'e", ""},
		`x "y \"z\"" w`:       {"x", `y "z"`, "w"},
		"  spaced   out  ":    {"spaced", "out"},
	} {
		if got := splitCommand(in); strings.Join(got, "|") != strings.Join(want, "|") || len(got) != len(want) {
			t.Errorf("splitCommand(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAShadowsSandboxNeverReachesTheWorld(t *testing.T) {
	s := fixtureScenario(t)
	w := newWorld(s)
	pw := newProcedureWorld(s, w)
	ans := pw.exec("notify.sh --to ops --account acme", "shadow:1", true)
	if ans.IsError || ans.Stdout == "" || ans.Delivered {
		t.Fatalf("a sandboxed command answered %+v; want the machine's answer, not delivered", ans)
	}
	if w.Count("machine") != 0 {
		t.Fatalf("a sandboxed command reached the world")
	}
	// An injection is aimed at the goal's REAL delivery; a sandbox never
	// consumes it.
	pw.armGoal([]scenario.Injection{{At: "notify", Kind: scenario.KindContract, Message: "boom", Once: true}})
	if a := pw.exec("notify.sh --to ops --account acme", "shadow:2", true); a.IsError {
		t.Fatal("a sandboxed command consumed the goal's injection")
	}
	if a := pw.exec("notify.sh --to ops --account acme", "real:1", false); !a.IsError || a.Delivered {
		t.Fatalf("the injection did not fire on the real delivery: %+v", a)
	}
	if a := pw.exec("notify.sh --to ops --account acme", "real:2", false); a.IsError || !a.Delivered {
		t.Fatalf("a once-only injection fired twice: %+v", a)
	}
}

func intPtr(v int) *int { return &v }

// newWorld is the scenario's declared world, as RunScenario builds it.
func newWorld(s scenario.Scenario) *world.World {
	return world.New(world.Config{Scripts: scriptsOf(s), Addresses: addressesOf(s), Routes: routesOf(s)})
}
