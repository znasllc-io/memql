package proving

// procedure_lifecycle.go -- the learned-procedure lifecycle driver (epic
// memql#5408, task memql#5413; the program record's section 7).
//
// A lifecycle scenario is a sequence of goals on one fixture. The driver
// serves each goal the way the platform would -- the goal is opened, the serve
// decision reads the learned procedure's CURRENT rung, and whatever that
// decision says happens: the app serves and is recorded, the app serves with
// the procedure compared beside it, or the procedure replays -- and it records
// what every goal cost. The scenario says only what the goals are and where
// the ladder must end up; it never says which rung serves a goal, because a
// lifecycle that dictated "now replay it trusted" would measure the driver's
// obedience rather than the ladder's.
//
// THE SERVE DECISION IS THE PLATFORM'S OWN FUNCTION. component/work.DecideServe
// is what compile's exact tier applies to a learned procedure it finds, and
// the driver asks it rather than re-deriving it: a second copy of "canary
// means the construct serves with the app standing by" is a copy that drifts.
//
// EVERYTHING ELSE GOES THROUGH A SMALL INTERFACE. ProcedureLadder is what the
// driver needs from the learned-procedure runner (integrations/procedure) and
// WorkSpine what it needs from the work spine (integrations/work). Both are
// declared here, so the sequencing below is testable with no engine, and so a
// change in the runner's shape is one adapter's problem rather than the
// driver's.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/proving/figure"
	"github.com/znasllc-io/memql/component/proving/scenario"
	"github.com/znasllc-io/memql/component/proving/world"
	"github.com/znasllc-io/memql/component/work"
	workerservice "github.com/znasllc-io/memql/component/worker"
)

// --- The runner, as the driver needs it ---------------------------------------

// ProcedureLadder is what the lifecycle driver needs from the learned-procedure
// runner. The first four are the runner's own entry points, by the names the
// plan gives them (docs/superpowers/plans/2026-09-23-procedure-certification-
// replay.md, section 1.4); the last two are the reads compile and the verifier
// make. Every call names the owner, and the adapter decides which actor makes
// it -- the person, or the cluster's maintenance principal an automation runs
// as -- because that is a property of the runner's gates, not of the lifecycle.
type ProcedureLadder interface {
	// LearnFromRun mines the corpus a succeeded recording belongs to and
	// lifts what it finds -- what the learnFromSucceededRun automation does
	// when a recording run succeeds.
	LearnFromRun(ctx context.Context, ownerUserId, recordingRunId string) (LearnReport, error)
	// ShadowCompare replays every procedure on shadow for the recording's
	// goal beside the app's own actions, in a sandbox, and moves the ladder
	// on the comparison.
	ShadowCompare(ctx context.Context, ownerUserId, recordingRunId string) ([]ReplayReport, error)
	// DecidePromotion applies a DECIDED procedurePromotion approval to the
	// ladder -- what the onProcedurePromotionDecided automation does.
	DecidePromotion(ctx context.Context, ownerUserId, approvalId string) (LadderMove, error)
	// Replay serves one goal from a construct on a serving rung.
	Replay(ctx context.Context, order ReplayOrder) (ReplayReport, error)

	// ProcedureFor is compile's exact tier for one goal signature: the learned
	// construct on the ladder for it, at its CURRENT rung. ok is false when
	// there is none.
	ProcedureFor(ctx context.Context, ownerUserId, goalSignature string) (ConstructState, bool, error)
	// Constructs reads back every learned construct the owner has.
	Constructs(ctx context.Context, ownerUserId string) ([]ConstructState, error)
}

// WorkSpine is what the driver needs from the work spine.
type WorkSpine interface {
	// OpenGoal opens the goal a person asked for and the run that serves it.
	// The run carries the goal signature and the goal's input as its
	// variables -- what compile writes on a run it decided, and what an app
	// session's recording inherits from the run it was delegated from.
	OpenGoal(ctx context.Context, g GoalOrder) (goalId, runId string, err error)
	// DecideApproval is the person's decision, through the work spine's OWN
	// decide handler: the owner check and the artifact-hash gate included.
	DecideApproval(ctx context.Context, ownerUserId, approvalId, decision string) error
	// PromotionApprovals reads back the owner's procedurePromotion approvals.
	PromotionApprovals(ctx context.Context, ownerUserId string) ([]ApprovalState, error)
}

// AppHandover is the app's side of a fallback: a goal a replay could not
// finish, handed back with what the replay had already done. The fixture app
// implements it, and the runner's AppFallback seam is adapted onto it.
type AppHandover interface {
	Handover(ctx context.Context, h HandoverOrder) (HandoverResult, error)
}

// LifecycleHarness opens the learned-procedure half of the platform for one
// platform-arm run of one lifecycle scenario. cmd/memql-bench installs one
// over the real engine; the db-free tests install fakes. A Runner with none
// fails a lifecycle scenario at RUN time, naming what is missing, rather than
// publishing a figure for a lifecycle nothing drove.
type LifecycleHarness interface {
	// Open writes the ladder's values for the run -- overlay's non-zero
	// fields over the deployment's current row, a zero field keeping the
	// deployment's value -- and returns what the run records through and
	// decides with.
	Open(ctx context.Context, overlay work.LadderPolicy) (*LifecyclePlatform, error)
}

// LifecyclePlatform is what Open returns.
type LifecyclePlatform struct {
	// Recorder writes the fixture app's sessions into the work spine:
	// integrations/work's SessionWriter in the lane.
	Recorder workerservice.SessionRecorder
	Spine    WorkSpine
	// NewLadder builds the runner with its seams bound to THIS run's world and
	// app: the dispatcher and the prober over the world, the app as the
	// fallback. Built after the app, because the app records through Recorder.
	NewLadder func(world *ProcedureWorld, app AppHandover) (ProcedureLadder, error)
	// Close puts back what Open changed -- the ladder's values. Nil when
	// nothing needs putting back.
	Close func(ctx context.Context) error
}

// LearnReport is what one learning pass did.
type LearnReport struct {
	ConstructId string
	// Lift is what the pass did to the catalog: "created", "relifted",
	// "unchanged", or "" when nothing cleared the floor.
	Lift string
	// Rung is the construct's rung after the pass.
	Rung   string
	Reason string
}

// The lift outcomes the driver branches on (integrations/procedure.LiftOutcome).
const (
	LiftCreated   = "created"
	LiftRelifted  = "relifted"
	LiftUnchanged = "unchanged"
)

// ReplayOrder serves one goal from a construct.
type ReplayOrder struct {
	OwnerUserId string
	ConstructId string
	// Mode is "canary" or "trusted" -- the rung the serve decision read.
	Mode string
	// GoalRunId is the run of the goal the replay serves.
	GoalRunId string
	// Input is the goal's input, which binds the procedure's free parameters.
	Input map[string]any
}

// ReplayReport is what one replay -- canary, trusted or shadow -- did.
type ReplayReport struct {
	ConstructId string
	Mode        string
	// Served: a canary or trusted replay answered the goal itself.
	Served bool
	// Match: a shadow replay matched the app on every step.
	Match        bool
	Diverged     bool
	DivergedStep int
	Diagnosis    string
	// StartRefused: nothing ran -- a precondition did not hold, a parameter
	// could not be bound, or the rung had moved since the serve decision.
	StartRefused bool
	// FellBack: the goal was handed to the app.
	FellBack bool
	// ModelCalls is what the replay ITSELF reached. A fallback's calls are
	// the app's, and are counted by the app.
	ModelCalls int
	// Completed are the steps the replay ran, with their idempotency keys.
	Completed []CompletedReport
	// Proposed: this comparison proposed a promotion, and
	// PromotionApprovalId is the approval raised for it.
	Proposed            bool
	PromotionApprovalId string
	// From and To are the ladder's move.
	From, To    string
	ReplayRunId string
}

// CompletedReport is one step a replay ran before it stopped.
type CompletedReport struct {
	// Index is the procedure's step index.
	Index          int
	Tool           string
	IdempotencyKey string
	SideEffect     bool
}

// LadderMove is one move of the ladder.
type LadderMove struct {
	From, To, Reason string
}

// ConstructState is where one learned construct stands.
type ConstructState struct {
	ConstructId         string
	GoalSignature       string
	Rung                string
	PromotionApprovalId string
}

// ApprovalState is one procedurePromotion approval.
type ApprovalState struct {
	ApprovalId  string
	ConstructId string
	// Decision is "approved", "rejected", or "" while it waits for a person.
	Decision string
}

// GoalOrder is a goal a person asks for.
type GoalOrder struct {
	OwnerUserId   string
	Statement     string
	Input         map[string]any
	GoalSignature string
}

// HandoverOrder is a goal handed back to the app.
type HandoverOrder struct {
	OwnerUserId string
	// GoalRunId is the run the goal is being served in.
	GoalRunId string
	// Completed are the steps the replay ran. The app must not redo them.
	Completed []CompletedReport
	Diagnosis string
	// Prompt is the guidance as the app reads it.
	Prompt string
}

// HandoverResult is what the app's session produced.
type HandoverResult struct {
	// ChildRunId is the session's recording: the repaired run.
	ChildRunId string
	SessionId  string
}

// --- What the lifecycle did ---------------------------------------------------

// The two things a goal can be served by, as the verifier's row form spells
// them (`{"rows": "v1:work:run", "where": {"servedBy": ...}}`).
const (
	servedByProcedure = "procedure"
	servedByApp       = "app"
)

// lifecycleRecord is everything the driver saw, for the figures, the
// verifier's row and named checks, and a failure message that has to say what
// actually happened.
type lifecycleRecord struct {
	Owner string
	Goals []goalRecord
	// Measured indexes Goals; -1 until the measured goal has been served.
	Measured int
	// Constructs and Approvals are read back after the last goal.
	Constructs []ConstructState
	Approvals  []ApprovalState
	// Problems are the platform not doing what the lifecycle needed -- a
	// decision with no proposal to decide, an app session that failed. They
	// are verifier failures on the platform arm, not runner errors: the
	// scenario RAN, and the platform's answer was wrong.
	Problems []string
}

// goalRecord is one entry of the lifecycle.
type goalRecord struct {
	Index    int
	Decide   string
	ServedBy string
	// Rung is the rung the serve decision read, "" when there was no
	// procedure for the goal.
	Rung      string
	Learn     *LearnReport
	Shadow    []ReplayReport
	Replay    *ReplayReport
	Decision  *LadderMove
	AppCalls  int
	Duplicate int
	Handovers []handoverSeen
	Narrative string
}

func (r *lifecycleRecord) measured() *goalRecord {
	if r == nil || r.Measured < 0 || r.Measured >= len(r.Goals) {
		return nil
	}
	return &r.Goals[r.Measured]
}

func (r *lifecycleRecord) problem(format string, a ...any) {
	r.Problems = append(r.Problems, fmt.Sprintf(format, a...))
}

// narrative is the lifecycle in one line per goal, for a failure message.
func (r *lifecycleRecord) narrative() string {
	if r == nil {
		return "(no lifecycle ran)"
	}
	lines := make([]string, 0, len(r.Goals))
	for _, g := range r.Goals {
		lines = append(lines, fmt.Sprintf("goal %d: %s", g.Index, g.Narrative))
	}
	return strings.Join(lines, "; ")
}

// --- The arms ------------------------------------------------------------------

// errNoLifecycleHarness is what a lifecycle scenario gets from a Runner nothing
// installed a harness on. A runner-level error, so the whole suite stops and
// says why: a lifecycle figure from a lifecycle nothing drove is the one thing
// this file must never publish.
var errNoLifecycleHarness = errors.New("this runner has no learned-procedure harness (Runner.Lifecycle), so a lifecycle scenario cannot run; cmd/memql-bench installs it over the engine")

// runLifecycle runs a lifecycle scenario on one arm, against the scenario's
// world.
func (r *Runner) runLifecycle(ctx context.Context, s scenario.Scenario, arm figure.Arm, w *world.World, res *ArmResult) {
	pw := newProcedureWorld(s, w)
	owner := lifecycleOwner(arm)
	switch arm {
	case figure.ArmPlatform:
		r.runLifecyclePlatform(ctx, s, pw, owner, res)
	case figure.ArmBaseline:
		runLifecycleBaseline(ctx, s, pw, owner, res)
	default:
		res.Err = fmt.Errorf("unknown arm %q", arm)
	}
}

// runLifecyclePlatform drives the lifecycle through the platform: the goals
// on the work spine, the app recorded, the ladder moving on its own evidence.
func (r *Runner) runLifecyclePlatform(ctx context.Context, s scenario.Scenario, pw *ProcedureWorld, owner string, res *ArmResult) {
	if r.Lifecycle == nil {
		res.Err = errNoLifecycleHarness
		return
	}
	platform, err := r.Lifecycle.Open(ctx, ladderPolicyOf(s.Procedure.Policy))
	if err != nil {
		res.Err = fmt.Errorf("opening the learned-procedure platform: %w", err)
		return
	}
	if platform.Close != nil {
		defer func() {
			// Putting the ladder's values back must not turn a measured run
			// into a failed one, so a failure here is logged -- loudly, since
			// a database left on this run's values would change every later
			// reader's ladder.
			closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			if cerr := platform.Close(closeCtx); cerr != nil && r.Logger != nil {
				r.Logger.Error("proving: could not put the ladder's values back after a lifecycle", "scenario", s.Id, "error", cerr)
			}
		}()
	}
	app := newFixtureApp(pw, s.Steps, platform.Recorder, owner)
	ladder, err := platform.NewLadder(pw, app)
	if err != nil {
		res.Err = fmt.Errorf("building the learned-procedure runner: %w", err)
		return
	}
	d := &lifecycleDriver{s: s, owner: owner, world: pw, app: app, spine: platform.Spine, ladder: ladder,
		rec: &lifecycleRecord{Owner: owner, Measured: -1}}
	res.lifecycle = d.rec
	if err := d.run(ctx); err != nil {
		res.Err = fmt.Errorf("the lifecycle of %s could not be run: %w (so far: %s)", s.Id, err, d.rec.narrative())
		return
	}
	d.publish(res)
}

// baselineRestarts is the retry allowance a bare loop gets: the same three the
// scripted baseline gets (runBaseline), because a baseline given fewer would be
// the strawman this comparison must not be.
const baselineRestarts = 3

// runLifecycleBaseline is the same goals, the same app and the same world with
// the platform switched off: no recording, no ladder, no replay, no guidance.
// Every goal goes to the app, and a goal whose app session fails is started
// again from its first action -- a loop with no memory of what it already did
// has nowhere else to start.
func runLifecycleBaseline(ctx context.Context, s scenario.Scenario, pw *ProcedureWorld, owner string, res *ArmResult) {
	app := newFixtureApp(pw, s.Steps, nil, owner)
	rec := &lifecycleRecord{Owner: owner, Measured: -1}
	res.lifecycle = rec
	completed := true
	for i, g := range s.Procedure.Goals {
		if g.Decide != "" {
			// No ladder, so nothing was proposed and there is nothing to
			// decide.
			rec.Goals = append(rec.Goals, goalRecord{Index: i, Decide: g.Decide, Narrative: "no ladder, nothing to decide"})
			continue
		}
		pw.armGoal(g.Inject)
		callsBefore, dupBefore := app.Calls(), pw.world.Duplicates()
		ag := appGoal{OwnerUserId: owner, GoalRunId: fmt.Sprintf("baseline-goal-%d", i), Statement: goalStatement(s, g), Variables: g.Variables}
		done := false
		for attempt := 0; attempt <= baselineRestarts && !done; attempt++ {
			sess, err := app.Serve(ctx, ag)
			if err != nil {
				res.Err = fmt.Errorf("baseline goal %d: %w", i, err)
				return
			}
			done = !sess.Failed
		}
		completed = completed && done
		gr := goalRecord{Index: i, ServedBy: servedByApp, AppCalls: app.Calls() - callsBefore,
			Duplicate: pw.world.Duplicates() - dupBefore}
		gr.Narrative = fmt.Sprintf("the app served it in %d session(s), %d duplicate(s)", gr.AppCalls, gr.Duplicate)
		rec.Goals = append(rec.Goals, gr)
		if g.Measure {
			rec.Measured = len(rec.Goals) - 1
		}
	}
	res.Passed = completed
	if m := rec.measured(); m != nil {
		res.AppCalls = m.AppCalls
		res.DuplicatedAcrossDivergence = m.Duplicate
		// A bare loop has no ladder, so no goal of it is ever served by a
		// procedure: zero by construction, which is exactly what the
		// comparison is against.
		res.ReplaysServedWithoutModel = 0
	}
}

// --- The driver ----------------------------------------------------------------

type lifecycleDriver struct {
	s      scenario.Scenario
	owner  string
	world  *ProcedureWorld
	app    *FixtureApp
	spine  WorkSpine
	ladder ProcedureLadder
	rec    *lifecycleRecord
}

func (d *lifecycleDriver) run(ctx context.Context) error {
	for i, g := range d.s.Procedure.Goals {
		var err error
		if g.Decide != "" {
			err = d.decide(ctx, i, g.Decide)
		} else {
			err = d.serve(ctx, i, g)
		}
		if err != nil {
			return fmt.Errorf("goal %d: %w", i, err)
		}
	}
	var err error
	if d.rec.Constructs, err = d.ladder.Constructs(ctx, d.owner); err != nil {
		return fmt.Errorf("reading the learned constructs back: %w", err)
	}
	if d.rec.Approvals, err = d.spine.PromotionApprovals(ctx, d.owner); err != nil {
		return fmt.Errorf("reading the promotion approvals back: %w", err)
	}
	return nil
}

// serve serves one goal however the platform's serve decision says.
func (d *lifecycleDriver) serve(ctx context.Context, i int, g scenario.ProcedureGoal) error {
	statement := goalStatement(d.s, g)
	sig := work.GoalSignature(statement, inputKeys(g.Variables))
	input := make(map[string]any, len(g.Variables))
	for k, v := range g.Variables {
		input[k] = v
	}
	_, goalRunId, err := d.spine.OpenGoal(ctx, GoalOrder{OwnerUserId: d.owner, Statement: statement, Input: input, GoalSignature: sig})
	if err != nil {
		return fmt.Errorf("opening the goal: %w", err)
	}
	ag := appGoal{OwnerUserId: d.owner, GoalRunId: goalRunId, Statement: statement, Variables: g.Variables}
	d.app.expect(ag)
	d.world.armGoal(g.Inject)
	defer d.world.armGoal(nil)

	callsBefore, dupBefore, handoversBefore := d.app.Calls(), d.world.world.Duplicates(), d.app.handoverCount()
	gr := goalRecord{Index: i}

	cs, found, err := d.ladder.ProcedureFor(ctx, d.owner, sig)
	if err != nil {
		return fmt.Errorf("reading the procedure for the goal: %w", err)
	}
	rung := work.RungNone
	if found {
		// An unknown spelling is RungNone, which serves nothing: a rung this
		// build cannot read is never a reason to skip the app.
		rung, _ = work.ParseRung(cs.Rung)
	}
	gr.Rung = string(rung)
	verdict := work.DecideServe(work.ReplayContext{Mode: "live", ConstructRung: rung})

	if verdict.Source == work.ServeConstruct {
		mode := string(work.RungTrusted)
		if verdict.Standby {
			mode = string(work.RungCanary)
		}
		rep, err := d.ladder.Replay(ctx, ReplayOrder{OwnerUserId: d.owner, ConstructId: cs.ConstructId, Mode: mode, GoalRunId: goalRunId, Input: input})
		if err != nil {
			return fmt.Errorf("replaying %s on %s: %w", cs.ConstructId, mode, err)
		}
		gr.Replay = &rep
		gr.ServedBy = servedByApp
		if rep.Served && !rep.FellBack {
			gr.ServedBy = servedByProcedure
		}
		gr.Narrative = fmt.Sprintf("%s replay of %s: served=%v diverged=%v fellBack=%v modelCalls=%d, ladder %s -> %s",
			mode, cs.ConstructId, rep.Served, rep.Diverged, rep.FellBack, rep.ModelCalls, rep.From, rep.To)
	} else {
		sess, err := d.app.Serve(ctx, ag)
		if err != nil {
			return err
		}
		gr.ServedBy = servedByApp
		gr.Narrative = fmt.Sprintf("the app served it (rung %q)", rung)
		if sess.Failed {
			// Not learned from: a failed recording is not a success, and the
			// learner fires only on one.
			d.rec.problem("goal %d: the app's session failed at action %d, so nothing was recorded to learn from", i, sess.FailedAt)
			gr.Narrative += "; its session failed"
		} else {
			learn, err := d.ladder.LearnFromRun(ctx, d.owner, sess.RunId)
			if err != nil {
				return fmt.Errorf("learning from the recording %s: %w", sess.RunId, err)
			}
			gr.Learn = &learn
			gr.Narrative += fmt.Sprintf(", learned: lift=%q rung=%q", learn.Lift, learn.Rung)
			// A comparison beside the app is meaningful only against a
			// version this recording did NOT change: one it changed was
			// partly derived from it and has just re-entered the ladder.
			if verdict.Shadow && learn.Lift == LiftUnchanged {
				reps, err := d.ladder.ShadowCompare(ctx, d.owner, sess.RunId)
				if err != nil {
					return fmt.Errorf("comparing beside the recording %s: %w", sess.RunId, err)
				}
				gr.Shadow = reps
				for _, rep := range reps {
					gr.Narrative += fmt.Sprintf(", shadow match=%v proposed=%v ladder %s -> %s", rep.Match, rep.Proposed, rep.From, rep.To)
				}
			}
		}
	}

	gr.AppCalls = d.app.Calls() - callsBefore
	gr.Duplicate = d.world.world.Duplicates() - dupBefore
	gr.Handovers = d.app.handoversSince(handoversBefore)
	d.rec.Goals = append(d.rec.Goals, gr)
	if g.Measure {
		d.rec.Measured = len(d.rec.Goals) - 1
	}
	return nil
}

// decide is a person answering the promotion the ladder proposed: through the
// work spine's own decide handler, then the ladder's own reaction to it.
func (d *lifecycleDriver) decide(ctx context.Context, i int, decision string) error {
	gr := goalRecord{Index: i, Decide: decision}
	defer func() { d.rec.Goals = append(d.rec.Goals, gr) }()

	sig := work.GoalSignature(d.s.Goal, d.primaryKeys())
	cs, found, err := d.ladder.ProcedureFor(ctx, d.owner, sig)
	if err != nil {
		return fmt.Errorf("reading the procedure to decide: %w", err)
	}
	if !found || cs.PromotionApprovalId == "" {
		d.rec.problem("goal %d decides a promotion (%s), and the ladder proposed none -- the procedure is %s", i, decision, describeConstruct(cs, found))
		gr.Narrative = "nothing was proposed to decide"
		return nil
	}
	if err := d.spine.DecideApproval(ctx, d.owner, cs.PromotionApprovalId, decision); err != nil {
		return fmt.Errorf("deciding %s: %w", cs.PromotionApprovalId, err)
	}
	move, err := d.ladder.DecidePromotion(ctx, d.owner, cs.PromotionApprovalId)
	if err != nil {
		return fmt.Errorf("applying the decision on %s: %w", cs.PromotionApprovalId, err)
	}
	gr.Decision = &move
	gr.Narrative = fmt.Sprintf("the person %s %s; ladder %s -> %s", decision, cs.PromotionApprovalId, move.From, move.To)
	return nil
}

// primaryKeys are the input keys of the scenario's own goal statement: the
// first goal that uses it binds them, and the loader holds every other goal
// that uses it to the same set.
func (d *lifecycleDriver) primaryKeys() []string {
	for _, g := range d.s.Procedure.Goals {
		if g.Decide == "" && g.Goal == "" {
			return inputKeys(g.Variables)
		}
	}
	return nil
}

// publish turns the measured goal into the arm's counters.
func (d *lifecycleDriver) publish(res *ArmResult) {
	m := d.rec.measured()
	if m == nil {
		// The loader refuses a lifecycle with no measured goal, so this is
		// the driver's own defect -- and a zero published for it would be a
		// figure nothing measured.
		res.Err = fmt.Errorf("the lifecycle finished with no measured goal recorded")
		return
	}
	res.AppCalls = m.AppCalls
	res.DuplicatedAcrossDivergence = m.Duplicate
	if servedWithoutModel(m) {
		res.ReplaysServedWithoutModel = 1
	}
	// Passed is not set here: on the platform arm it is the VERIFIER's answer,
	// which RunScenario asks next and which reads the lifecycle's problems.
}

// servedWithoutModel is the headline's definition, in one place: a goal a
// TRUSTED procedure served itself, whose replay reached no model and during
// which the app -- the only intelligence in the proving world -- was not
// reached either. A canary replay does not count: the app stands by on
// canary, and the figure is about the rung that runs alone.
func servedWithoutModel(g *goalRecord) bool {
	return g != nil && g.Replay != nil &&
		g.Rung == string(work.RungTrusted) &&
		g.ServedBy == servedByProcedure &&
		g.Replay.ModelCalls == 0 &&
		g.AppCalls == 0
}

// --- Helpers ---------------------------------------------------------------------

// goalStatement is the statement one goal asks for: its own, when it names a
// different goal on the same fixture, else the scenario's.
func goalStatement(s scenario.Scenario, g scenario.ProcedureGoal) string {
	if g.Goal != "" {
		return g.Goal
	}
	return s.Goal
}

func inputKeys(vars map[string]string) []string {
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ladderPolicyOf turns the scenario's values into an OVERLAY on the ladder's
// row: a value the scenario did not write stays zero, which the harness reads
// as "keep the deployment's". It is deliberately not normalized here --
// Normalize would turn every zero into component/work's DEFAULT, which is a
// different thing from the deployment's current row the moment an operator has
// edited it.
func ladderPolicyOf(p scenario.LadderPolicy) work.LadderPolicy {
	v := func(n *int) int {
		if n == nil {
			return 0
		}
		return *n
	}
	return work.LadderPolicy{
		ShadowMatches:        v(p.ShadowMatches),
		DistinctBindings:     v(p.DistinctBindings),
		CanaryMatches:        v(p.CanaryMatches),
		FailuresToDemote:     v(p.FailuresToDemote),
		InsufficientToDemote: v(p.InsufficientToDemote),
		RetireAfterDays:      v(p.RetireAfterDays),
	}
}

// lifecycleOwner is the person one arm's lifecycle belongs to: fresh for every
// run, so a corpus is exactly this run's recordings even on a database an
// earlier run -- or another session -- also wrote to. Learning runs on ONE
// owner's corpus, and leftovers under a reused owner would be learned from.
func lifecycleOwner(arm figure.Arm) string {
	return fmt.Sprintf("proving-%s-%s", arm, strconv.FormatInt(time.Now().UnixNano(), 36))
}

func describeConstruct(cs ConstructState, found bool) string {
	if !found {
		return "not on the ladder for this goal"
	}
	return fmt.Sprintf("%s on %q with no open approval", cs.ConstructId, cs.Rung)
}

// --- The verifier's answers --------------------------------------------------------

// lifecycleRowCount answers a lifecycle verifier's row assertion from what the
// driver read back after the last goal. The vocabulary is the loader's
// (scenario.LifecycleRows), and every key it admits is answered here --
// TestEveryLifecycleRowTheLoaderAdmitsIsAnswered holds the two together. A
// row or key outside it answers ok=false, which the verifier reports: an
// unanswerable assertion reading zero would make `count: 0` pass forever.
func lifecycleRowCount(c scenario.Check, rec *lifecycleRecord) (int, bool) {
	if rec == nil {
		return 0, false
	}
	switch c.Rows {
	case scenario.RowConstruct:
		n := 0
		for _, cs := range rec.Constructs {
			if want, ok := c.Where["ladder"]; ok && cs.Rung != want {
				continue
			}
			n++
		}
		return n, onlyKeys(c.Where, "ladder")
	case scenario.RowApproval:
		if kind, ok := c.Where["kind"]; ok && kind != scenario.ApprovalKindPromotion {
			return 0, false
		}
		n := 0
		for _, a := range rec.Approvals {
			if want, ok := c.Where["decision"]; ok && approvalDecisionWord(a.Decision) != want {
				continue
			}
			n++
		}
		return n, onlyKeys(c.Where, "kind", "decision")
	}
	return 0, false
}

// approvalDecisionWord spells an undecided approval the way the verifier does:
// "pending", because an empty value in a where-clause reads as "any".
func approvalDecisionWord(decision string) string {
	if decision == "" {
		return scenario.DecisionPending
	}
	return decision
}

func onlyKeys(where map[string]string, keys ...string) bool {
	allowed := map[string]bool{}
	for _, k := range keys {
		allowed[k] = true
	}
	for k := range where {
		if !allowed[k] {
			return false
		}
	}
	return true
}

// checkTrustedReplayReachedNoModel is the headline's named check: the measured
// goal was served by a TRUSTED procedure whose replay reached no model, and the
// app -- the only other thing in the proving world that could have answered --
// was not reached either. It restates servedWithoutModel as a failure a person
// can act on: which of the four did not hold.
func checkTrustedReplayReachedNoModel(rec *lifecycleRecord) string {
	m := rec.measured()
	switch {
	case m == nil:
		return "no lifecycle goal was measured, so no replay can have been checked"
	case m.Replay == nil:
		return fmt.Sprintf("the measured goal was not replayed at all -- the serve decision read rung %q and the app served it (%s)", m.Rung, rec.narrative())
	case m.Rung != string(work.RungTrusted):
		return fmt.Sprintf("the measured goal was replayed on %q, not trusted: on canary the app stands by, and the claim is about the rung that runs alone (%s)", m.Rung, rec.narrative())
	case m.ServedBy != servedByProcedure:
		return fmt.Sprintf("the trusted replay did not serve the goal itself (served=%v, fellBack=%v, diverged=%v: %s)", m.Replay.Served, m.Replay.FellBack, m.Replay.Diverged, m.Replay.Diagnosis)
	case m.Replay.ModelCalls != 0:
		return fmt.Sprintf("the trusted replay reported %d model call(s) of its own", m.Replay.ModelCalls)
	case m.AppCalls != 0:
		return fmt.Sprintf("the app was reached %d time(s) during a goal the trusted replay reports serving", m.AppCalls)
	}
	return ""
}

// checkDivergedReplayHandedOver is the durability scenario's named check: the
// measured goal's replay diverged AFTER at least one side effect had been
// delivered, the goal was handed to the app exactly once, the guidance named
// every step the replay completed -- by the same index and idempotency key the
// replay reported -- and the app performed none of them again.
//
// Without it, "nothing was delivered twice" would also hold for a replay that
// never diverged, one that stopped before its first side effect, or a fallback
// that never happened: three ways for the durability figure to read zero
// about a divergence that was never measured.
func checkDivergedReplayHandedOver(rec *lifecycleRecord) string {
	m := rec.measured()
	switch {
	case m == nil:
		return "no lifecycle goal was measured"
	case m.Replay == nil:
		return fmt.Sprintf("the measured goal was not replayed, so nothing could diverge (%s)", rec.narrative())
	case !m.Replay.Diverged:
		return fmt.Sprintf("the measured goal's replay did not diverge (served=%v, startRefused=%v): the injected failure was not reached, so a zero here measures nothing", m.Replay.Served, m.Replay.StartRefused)
	case !m.Replay.FellBack:
		return "the replay diverged and did not hand the goal back to the app"
	case len(m.Handovers) != 1:
		return fmt.Sprintf("the app was handed the goal %d time(s), want exactly 1", len(m.Handovers))
	}
	completed := map[int]CompletedReport{}
	sideEffect := false
	for _, c := range m.Replay.Completed {
		completed[c.Index] = c
		sideEffect = sideEffect || c.SideEffect
	}
	if !sideEffect {
		return fmt.Sprintf("the replay diverged at step %d having delivered no side effect, so there was nothing a takeover could deliver twice", m.Replay.DivergedStep)
	}
	h := m.Handovers[0]
	guided := map[int]CompletedReport{}
	for _, c := range h.Order.Completed {
		guided[c.Index] = c
	}
	for idx, c := range completed {
		g, ok := guided[idx]
		switch {
		case !ok:
			return fmt.Sprintf("the replay completed step %d (%s) and the guidance handed to the app does not name it", idx, c.IdempotencyKey)
		case g.IdempotencyKey != c.IdempotencyKey:
			return fmt.Sprintf("the guidance names step %d under idempotency key %q, and the replay ran it under %q", idx, g.IdempotencyKey, c.IdempotencyKey)
		}
	}
	for _, idx := range h.Session.Performed {
		if c, ok := completed[idx]; ok {
			return fmt.Sprintf("the app performed step %d again although the replay had completed it under %s", idx, c.IdempotencyKey)
		}
	}
	return ""
}
