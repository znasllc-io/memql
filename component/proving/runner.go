package proving

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/automations"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/proving/cassette"
	"github.com/znasllc-io/memql/component/proving/figure"
	"github.com/znasllc-io/memql/component/proving/scenario"
	"github.com/znasllc-io/memql/component/proving/world"
	"github.com/znasllc-io/memql/component/work"
)

// Runner drives the corpus.
type Runner struct {
	Engine    *memqlengine.MemQLEngine
	Cassettes cassette.Set
	Logger    *slog.Logger
	Prov      figure.Provenance
	// Lifecycle opens the learned-procedure half of the platform for a
	// procedure lifecycle scenario's platform arm (procedure_lifecycle.go).
	// Nil fails such a scenario loudly rather than skipping it.
	Lifecycle LifecycleHarness
}

// ArmResult is one arm's run of one scenario: what happened, and everything
// counted while it happened.
type ArmResult struct {
	Scenario string
	Arm      figure.Arm

	// Passed is the verifier's answer.
	Passed bool
	// Failures are the verifier's complaints, in its own words.
	Failures []string

	// RecordedResponses is how many cassette turns the run consumed. In the
	// CI tier this is the honest stand-in for "provider calls": a run that
	// consumed none made none.
	RecordedResponses int
	// StepsExecuted is how many step bodies ran, including repeats.
	StepsExecuted int
	// StepsReExecuted counts bodies that ran more than once. On a resumed
	// platform run this must be zero.
	StepsReExecuted int
	// Resumed reports that the run was resumed from its journal.
	Resumed bool
	// SameRunId reports that the resumed run kept the original run's id --
	// the property that makes "what happened to run X" one story rather than
	// two executions sharing a prefix.
	SameRunId bool
	// Effects and Duplicates are the fake world's counters.
	Effects    int
	Duplicates int
	// DuplicateDetail names what repeated, for a failure message.
	DuplicateDetail string
	// WallClock is how long the arm took IN THIS PROCESS. It is recorded and
	// deliberately NOT published as a CI figure -- a shared runner's
	// wall-clock is the runner's, not the product's (design P1).
	WallClock time.Duration
	// RunId is the journal run this arm produced, empty for the baseline arm
	// which keeps no journal by construction.
	RunId string
	// Err is a runner-level failure: the scenario could not be run at all,
	// which is different from a scenario that ran and failed its verifier.
	Err error

	// The procedure lifecycle's counters, all about its ONE measured goal
	// (procedure_lifecycle.go):
	//
	// ReplaysServedWithoutModel is 1 when a TRUSTED procedure served the goal
	// itself with no model and no app call, else 0.
	ReplaysServedWithoutModel int
	// AppCalls is the fixture app's sessions during the goal. The app is the
	// lifecycle's only intelligence, so a session is a model reach.
	AppCalls int
	// DuplicatedAcrossDivergence is the world's duplicates during the goal --
	// whose failure the scenario injects, so that a replay stops part-way and
	// the app takes over.
	DuplicatedAcrossDivergence int
	// lifecycle is everything the driver saw, for the verifier.
	lifecycle *lifecycleRecord

	// unrecovered records that the run ended failed even after its resume.
	// It is what a verifier asserting `status: failed` reads, and it is a
	// legitimate expected outcome: a scenario may exist to show that an
	// unrecoverable failure STAYS failed rather than being papered over.
	unrecovered bool
}

// RunScenario runs one scenario on one arm.
func (r *Runner) RunScenario(ctx context.Context, s scenario.Scenario, arm figure.Arm) (res ArmResult) {
	res = ArmResult{Scenario: s.Id, Arm: arm}
	started := time.Now()
	// A NAMED return, deliberately. With a plain local the deferred write
	// lands after the return value has been copied, so every WallClock
	// reached the caller as zero -- and a zero here is not visibly wrong: it
	// silently turns the journal-overhead ratio into `belowFloor` and the
	// scorecard reports an absence where a measurement belongs.
	defer func() { res.WallClock = time.Since(started) }()

	w := world.New(world.Config{
		Scripts:   scriptsOf(s),
		Addresses: addressesOf(s),
		Routes:    routesOf(s),
	})

	var player *cassette.Player
	if needsModel(s) {
		c, ok := r.Cassettes.For(s.Id, string(arm))
		if !ok {
			res.Err = fmt.Errorf(
				"no cassette for %s/%s. The CI tier has no provider, so BOTH arms replay: "+
					"record one with `memql-bench record --scenario=%s`", s.Id, arm, s.Id)
			return res
		}
		player = cassette.NewPlayer(c)
	}

	switch {
	case s.Procedure != nil:
		// A learned-procedure lifecycle: a sequence of goals on the fixture
		// app, served however the platform's ladder decides.
		r.runLifecycle(ctx, s, arm, w, &res)
	case arm == figure.ArmPlatform:
		r.runPlatform(ctx, s, w, player, &res)
	case arm == figure.ArmBaseline:
		r.runBaseline(s, w, player, &res)
	default:
		res.Err = fmt.Errorf("unknown arm %q", arm)
		return res
	}

	res.Effects = totalEffects(w)
	res.Duplicates = w.Duplicates()
	res.DuplicateDetail = w.DuplicateDetail()
	if player != nil {
		res.RecordedResponses = player.Reads()
	}
	if res.Err == nil {
		// THE VERIFIER IS THE PLATFORM'S CONTRACT. The baseline arm is
		// MEASURED, not verified: it is the comparison, not the thing under
		// test, and asserting the platform's postconditions against it would
		// report the very difference the suite exists to show as a corpus
		// failure. A bare loop that re-delivers is not a broken scenario --
		// it is the finding.
		//
		// What the baseline is still asked is whether it finished at all,
		// which is a real property: a loop with no journal can exhaust its
		// restarts.
		if arm == figure.ArmPlatform {
			res.Failures = verify(s, w, res)
			res.Passed = len(res.Failures) == 0
		}
	}
	return res
}

// runPlatform executes the scenario through the REAL automation executor
// against a REAL database, then -- if the run failed -- resumes it from its own
// journal.
//
// Everything measured here is measured about the platform rather than about a
// model of it: the journal rows are written by component/automations/journal.go,
// the resume reads them back through LoadRunJournal, and the step registry is
// the only fake in the path.
func (r *Runner) runPlatform(ctx context.Context, s scenario.Scenario, w *world.World, player *cassette.Player, res *ArmResult) {
	reg := newStepRegistry(s, w, player)
	exec := automations.NewExecutor(automations.ExecutorOptions{
		Engine:       r.Engine,
		StepRegistry: reg,
		Logger:       r.Logger,
	})
	// THE FAILURE PATH'S ONE MODEL CALL, wired so the recovery family's count
	// is a measurement rather than an assumption. With no classifier installed
	// the path never reaches a model on ANY path, so the zero-claim would pass
	// for the wrong reason and its control could never produce a one. See
	// classifier.go.
	exec.SetSymptomClassifier(&provingClassifier{player: player})
	defer exec.Close()

	auto := buildAutomation(s, automationName(s))
	run, err := exec.Execute(ctx, auto, "proving")
	if err != nil && run == nil {
		res.Err = fmt.Errorf("executing %s: %w", s.Id, err)
		return
	}
	res.RunId = run.ID

	if run.Status == "failed" {
		// The claim under test: a failed run is resumed FROM ITS OWN JOURNAL,
		// on the same run id, and the steps that already completed are served
		// back rather than re-run. Anything the resume re-executes shows up in
		// the registry's counter, and anything it re-delivers shows up in the
		// world's.
		journal, jerr := automations.LoadRunJournal(ctx, r.Engine, run.ID)
		if jerr != nil {
			res.Err = fmt.Errorf("loading the journal for %s: %w", run.ID, jerr)
			return
		}
		// ResumeFrom validates the journal against the automation's own
		// fingerprint before it does anything, so a changed template refuses
		// rather than resuming into a different plan. That check is the
		// executor's, not the runner's -- re-deriving the fingerprint here
		// would be a second implementation that can disagree with the one
		// that matters.
		resumed, rerr := exec.ResumeFrom(ctx, journal, auto, &automations.ResumeOptions{AllowSideEffects: true})
		if rerr != nil && resumed == nil {
			res.Err = fmt.Errorf("resuming %s: %w", run.ID, rerr)
			return
		}
		res.Resumed = true
		res.SameRunId = resumed.ID == run.ID
		res.unrecovered = resumed.Status == "failed"
	}

	res.StepsExecuted = totalExecuted(reg)
	res.StepsReExecuted = reg.ReExecuted()
}

// runBaseline is the same model, the same tools, the same scenario, in a plain
// bounded tool loop with the platform's machinery switched off.
//
// It is a REAL implementation, not a strawman: it gets the same steps, the same
// cassette, the same retry allowance and the same world. What it does not get
// is a journal -- so its only recovery from a mid-run failure is to start the
// whole sequence again, which is exactly what a loop with no memory of having
// done this before must do.
//
// That is also why the baseline arm is the corpus's natural negative control
// for `durability.duplicatedSideEffects`: restarting re-delivers, and a
// counter that never went above zero on ANY path would read as zero forever.
func (r *Runner) runBaseline(s scenario.Scenario, w *world.World, player *cassette.Player, res *ArmResult) {
	// The same retry allowance the platform arm gets from its resume: three
	// further attempts. Giving the baseline fewer would be the strawman this
	// comparison must not be.
	const maxRestarts = 3
	reg := newStepRegistry(s, w, player)

	completed := false
	for attempt := 0; attempt <= maxRestarts && !completed; attempt++ {
		failed := false
		for _, st := range s.Steps {
			step := &automations.Step{ID: st.Key, Type: automations.StepTypeFunction}
			if _, err := reg.Execute(context.Background(), step, nil); err != nil {
				failed = true
				break
			}
		}
		completed = !failed
		// No journal, so there is nowhere to resume from: the loop starts the
		// whole sequence again, from the first step, with everything it
		// already did still done in the outside world.
	}

	res.Passed = completed
	res.StepsExecuted = totalExecuted(reg)
	res.StepsReExecuted = reg.ReExecuted()
}

// verify runs the scenario's deterministic verifier.
func verify(s scenario.Scenario, w *world.World, res ArmResult) []string {
	var failures []string
	if res.lifecycle != nil {
		// What the lifecycle needed and the platform did not do -- a decision
		// with nothing proposed, a session that failed -- is the platform's
		// answer being wrong, so it fails the verifier with the lifecycle's own
		// account of what happened beside it.
		for _, p := range res.lifecycle.Problems {
			failures = append(failures, fmt.Sprintf("%s (the lifecycle: %s)", p, res.lifecycle.narrative()))
		}
	}
	for i, c := range s.Verify {
		form, err := c.Form()
		if err != nil {
			failures = append(failures, fmt.Sprintf("verifier %d is malformed: %v", i, err))
			continue
		}
		switch form {
		case scenario.FormEffects:
			got, ok := w.Counter(c.Effects)
			if !ok {
				failures = append(failures, fmt.Sprintf(
					"verifier %d reads counter %q, which the world does not keep (it keeps: %s)",
					i, c.Effects, strings.Join(world.KnownCounters(), ", ")))
				continue
			}
			if msg := compareCount(c, got, c.Effects); msg != "" {
				failures = append(failures, msg)
			}
		case scenario.FormRows:
			// A row assertion is answered from the run's own journal, which
			// is the only graph state a proving scenario writes. It is scoped
			// to the platform arm: the baseline keeps no journal, so asserting
			// on rows there would be asserting that a thing which by design
			// does not happen did not happen.
			if res.Arm != figure.ArmPlatform {
				continue
			}
			got, ok := rowCount(c, res)
			if !ok {
				failures = append(failures, fmt.Sprintf(
					"verifier %d asserts on %s, which the runner cannot answer from a proving run", i, c.Rows))
				continue
			}
			if msg := compareCount(c, got, c.Rows); msg != "" {
				failures = append(failures, msg)
			}
		case scenario.FormNamed:
			if msg := runNamedCheck(c.Named, s, w, res); msg != "" {
				failures = append(failures, fmt.Sprintf("verifier %d (%s): %s", i, c.Named, msg))
			}
		}
	}
	return failures
}

func compareCount(c scenario.Check, got int, what string) string {
	switch {
	case c.Count != nil && got != *c.Count:
		return fmt.Sprintf("%s = %d, want exactly %d", what, got, *c.Count)
	case c.AtLeast != nil && got < *c.AtLeast:
		return fmt.Sprintf("%s = %d, want at least %d", what, got, *c.AtLeast)
	}
	return ""
}

// rowCount answers a row assertion from what the run produced. It supports
// exactly what the corpus asserts on today and reports ok=false for anything
// else -- a verifier that silently answered zero for an unsupported concept
// would make `count: 0` pass on a scenario that asserts nothing.
func rowCount(c scenario.Check, res ArmResult) (int, bool) {
	if res.lifecycle != nil {
		// A lifecycle keeps no journal run of its own to answer the
		// automation rows from; it answers the ladder's rows, read back from
		// the platform after its last goal.
		return lifecycleRowCount(c, res.lifecycle)
	}
	switch c.Rows {
	case "v1:work:run":
		switch c.Where["status"] {
		case "succeeded", "":
			return boolCount(res.RunId != "" && res.Err == nil && !res.unrecovered), true
		case "failed":
			return boolCount(res.unrecovered), true
		case "resumed":
			return boolCount(res.Resumed), true
		}
		return 0, false
	case "v1:work:step":
		if c.Where["status"] == "reExecuted" {
			return res.StepsReExecuted, true
		}
		return 0, false
	}
	return 0, false
}

// runNamedCheck resolves the closed registry of Go checks. An unregistered
// name cannot reach here -- the loader refuses it -- so a name arriving with no
// implementation is a defect in THIS function, and it says so rather than
// passing.
func runNamedCheck(name string, s scenario.Scenario, w *world.World, res ArmResult) string {
	switch name {
	case "scriptHashMatchesWhatWasAsked":
		if w == nil {
			// Unreachable from a real run -- the loader refuses a scenario
			// whose check reaches a facet its world does not declare -- but a
			// benchmark runner that PANICS is strictly worse than one that
			// says what it could not check.
			return "the scenario declares no fake machine, so there is no recorded hash to compare"
		}
		hashes := w.ScriptHashes()
		if len(hashes) == 0 {
			return "the fake machine was never asked to run a script"
		}
		for i, h := range hashes {
			if len(h) == 0 {
				return fmt.Sprintf("script %d recorded no hash", i)
			}
		}
		return ""
	case "resumedRunKeepsItsRunId":
		if !res.Resumed {
			return "the run was not resumed, so there is nothing to compare"
		}
		if !res.SameRunId {
			return "the resumed run took a new id, so a reader asking what happened to this run gets two stories"
		}
		return ""
	case "journalIsCompleteForEveryStep":
		if res.RunId == "" {
			return "no journal run was opened"
		}
		return ""
	case "aTrustedReplayReachedNoModel":
		return checkTrustedReplayReachedNoModel(res.lifecycle)
	case "aDivergedReplayHandedOverItsCompletedSteps":
		return checkDivergedReplayHandedOver(res.lifecycle)
	case "approvalRefusesAChangedArtifact":
		// The hash comparison itself is a property of component/work, proved
		// there over values. What this checks is that the scenario reached the
		// point of having an artifact at all.
		if res.StepsExecuted == 0 {
			return "no step ran, so no artifact was produced to approve"
		}
		return ""
	}
	return fmt.Sprintf("the check is registered in the corpus loader but has no implementation in the runner (%s)", name)
}

// --- scenario helpers ------------------------------------------------------

// needsModel reports whether a scenario reaches a model at all, and therefore
// whether it needs a cassette.
//
// THERE ARE TWO WAYS TO REACH ONE, and the second arrived with the work
// spine's failure path (epic memql#5127, design D12). A reasoning STEP is the
// obvious one. The other is a run that FAILS with a symptom the deterministic
// rules table has no opinion about: the executor then makes exactly one
// classifySymptom call, which is the whole point of the recovery family's
// count and the thing its negative control exists to produce.
//
// Deriving the answer from the steps alone was correct until that path was
// wired. It is now incomplete in the direction that hurts: a control scenario
// whose model call comes from the classifier would be told it needs no
// cassette, its cassette would be reported as an orphan, and deleting it --
// which is what that message asks for -- would make the control fail at run
// time with "no recorded response", read as a cassette problem rather than as
// the deletion it was.
func needsModel(s scenario.Scenario) bool {
	for _, st := range s.Steps {
		if st.Reasoning {
			return true
		}
	}
	return reachesTheClassifier(s)
}

// reachesTheClassifier reports whether this scenario exists to make the
// classifier fire, which it declares by being the negative control for the
// recovery call count.
//
// IT IS A DECLARATION RATHER THAN A DERIVATION, and the alternative is worth
// recording because it was tried. "The run ends failed" over-approximates:
// most failing scenarios in this corpus fail with a message the rules table
// classifies -- a stalled loop, an unrecoverable environment fault -- so they
// reach no model, and requiring a cassette for each would demand files that
// are never read and would go stale unnoticed.
//
// The cost of the declaration is that a scenario which reaches the classifier
// WITHOUT saying so fails at run time with "no recorded response for ...". That
// failure names the cassette and the scenario, and the fix is one field, so it
// is a loud wrong rather than a quiet one.
func reachesTheClassifier(s scenario.Scenario) bool {
	return s.NegativeControlFor == figure.MetricRecoveryCalls
}

func scriptsOf(s scenario.Scenario) map[string]string {
	if s.World.Machine == nil {
		return nil
	}
	return s.World.Machine.Scripts
}

func addressesOf(s scenario.Scenario) []string {
	if s.World.Mailbox == nil {
		return nil
	}
	return s.World.Mailbox.Addresses
}

func routesOf(s scenario.Scenario) map[string]string {
	if s.World.HTTP == nil {
		return nil
	}
	return s.World.HTTP.Routes
}

func totalEffects(w *world.World) int {
	return w.Count("machine") + w.Count("mailbox") + w.Count("http")
}

func totalExecuted(r *stepRegistry) int {
	n := 0
	for _, v := range r.Executed() {
		n += v
	}
	return n
}

// automationName is unique per run so two scenarios never share a journal's
// automation name, and so a re-run does not read as a resume of the last one.
func automationName(s scenario.Scenario) string {
	safe := strings.NewReplacer(".", "_", "-", "_").Replace(s.Id)
	return fmt.Sprintf("proving_%s_%d", safe, time.Now().UnixNano())
}

// CompileCallsOnCatalogHit measures the epic's cheapest and most checkable
// claim: a goal that exactly matches the catalog reaches no model at all.
//
// It calls component/work.Decide directly, which is the honest measurement --
// the claim IS a property of that function's return value, and routing it
// through a provider stub would measure the stub. The negative control is the
// companion below: without it, a counter that is never incremented on ANY path
// reads as zero forever.
func CompileCallsOnCatalogHit(statement string, inputKeys []string) int {
	d := work.Decide(work.CompileInput{
		Statement: statement,
		InputKeys: inputKeys,
		Exact: []work.CatalogCandidate{{
			ConstructId: "v1:authoring:construct:proving",
			Name:        "provingTemplate",
			Signature:   work.GoalSignature(statement, inputKeys),
		}},
	})
	return modelCalls(d)
}

// CompileCallsOnCatalogMiss is the negative control for the figure above. A
// miss must reach a model; if this ever returns zero, the counter is dead and
// the headline claim means nothing.
func CompileCallsOnCatalogMiss(statement string, inputKeys []string) int {
	d := work.Decide(work.CompileInput{Statement: statement, InputKeys: inputKeys})
	return modelCalls(d)
}

func modelCalls(d work.Decision) int {
	n := 0
	if d.NeedsTriage {
		n++
	}
	if d.NeedsModel && !d.NeedsTriage {
		n++
	}
	return n
}

// boolCount renders a predicate as the count a row assertion compares against:
// one row, or none.
func boolCount(b bool) int {
	if b {
		return 1
	}
	return 0
}
