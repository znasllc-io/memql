package proving

// procedure_fakes_test.go -- a WELL-BEHAVED stand-in for the learned-procedure
// half of the platform, so the lifecycle driver, its figures and its verifier
// can be exercised with no engine and no database.
//
// It behaves as the runner does (integrations/procedure: replay.go's stages
// and seams.go's contract; design record
// docs/superpowers/specs/2026-09-13-app-session-recording-and-learning-program-design.md,
// section 4 epic D): a construct per goal signature, lifted on its
// second succeeded recording, moved ONLY by component/work.Advance -- the real
// ladder, not a copy of it -- a shadow comparison replaying in a sandbox, a
// promotion raised once and decided through a separate spine, a replay that
// stops at the first step that disagrees with the recordings and hands the
// goal back with the steps it completed.
//
// WHERE IT CHEATS, AND WHY THAT IS SAFE. It does not learn: the procedure it
// replays is the scenario's own action script with the goal's input bound in,
// which is what the real learner derives from the recordings. Learning is the
// runner's claim and is measured in the database lane; what these tests
// measure is the DRIVER -- that it serves each goal as the ladder decides,
// counts what the goal cost, and publishes the right number.
//
// A person stepping in (epic memql#5414) is faked the same way: a goal run's
// step keeps its versions and verdicts in memory, a re-run puts the dislike it
// replaces on the step's context as the executor does, and the corpus a
// procedure is lifted from keeps the platform's two rules -- a recording whose
// parent version was replaced, or disliked, is left out. What the fake calls a
// lifted procedure's steps is the recordings it was lifted from, written back
// token by token with each goal's input as a placeholder: enough for the
// correction's check to see WHICH recordings a procedure came from, never a
// claim about the real learner's templates.
//
// Each knob below breaks one behaviour on purpose, for a negative control run
// on the TEST: the figure that depends on the behaviour must move.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/znasllc-io/memql/component/proving/scenario"
	"github.com/znasllc-io/memql/component/work"
	workerservice "github.com/znasllc-io/memql/component/worker"
	"github.com/znasllc-io/memql/core/common"
)

// fakeKnobs break one behaviour each.
type fakeKnobs struct {
	// dropGuidance hands a diverged goal back with no completed steps.
	dropGuidance bool
	// ignoreDecisions leaves the ladder where it is when a promotion is
	// decided.
	ignoreDecisions bool
	// replayModelCalls is what every canary or trusted replay reports
	// reaching.
	replayModelCalls int
	// serveShadow serves a goal from a procedure on SHADOW -- the counter lie
	// the zero-reading control exists to catch.
	serveShadow bool

	// ignoreDislikes keeps a recording a person disliked in the corpus, and
	// ignoreSuperseded one whose version a re-run replaced.
	ignoreDislikes   bool
	ignoreSuperseded bool
	// dropRerunGuidance runs a step again without telling the app what the
	// person disliked about the version it replaces.
	dropRerunGuidance bool
	// forgetReplacedVersions reads back only each step's current version, as
	// a store that rewrote a version in place would.
	forgetReplacedVersions bool
	// headStays leaves a re-run's step head on the version it replaced.
	headStays bool
}

// fakeHarness is the LifecycleHarness the tests install.
type fakeHarness struct {
	knobs fakeKnobs
	// opened records every overlay Open was given, and closed how many times
	// the platform was put back.
	mu      sync.Mutex
	opened  []work.LadderPolicy
	closed  int
	ladders []*fakeLadder
}

func (h *fakeHarness) Open(_ context.Context, owner string, overlay work.LadderPolicy) (*LifecyclePlatform, error) {
	if owner == "" {
		return nil, fmt.Errorf("no owner")
	}
	h.mu.Lock()
	h.opened = append(h.opened, overlay)
	h.mu.Unlock()
	// The deployment's row, in the fake, is the default policy: an overlay
	// field left zero keeps it.
	policy := work.DefaultLadderPolicy()
	for _, f := range []struct {
		dst *int
		v   int
	}{
		{&policy.ShadowMatches, overlay.ShadowMatches}, {&policy.DistinctBindings, overlay.DistinctBindings},
		{&policy.CanaryMatches, overlay.CanaryMatches}, {&policy.FailuresToDemote, overlay.FailuresToDemote},
		{&policy.InsufficientToDemote, overlay.InsufficientToDemote}, {&policy.RetireAfterDays, overlay.RetireAfterDays},
	} {
		if f.v > 0 {
			*f.dst = f.v
		}
	}
	spine := &fakeSpine{goals: map[string]fakeGoalRun{}, approvals: map[string]*ApprovalState{}, closed: map[string]bool{},
		steps: map[string]*fakeStep{}, knobs: h.knobs}
	rec := &fakeRecorder{runs: map[string]*fakeRecording{}}
	return &LifecyclePlatform{
		Recorder: rec,
		Spine:    spine,
		NewLadder: func(w *ProcedureWorld, app AppHandover) (ProcedureLadder, error) {
			l := &fakeLadder{knobs: h.knobs, policy: policy, world: w, app: app, spine: spine, rec: rec,
				constructs: map[string]*fakeConstruct{}}
			h.mu.Lock()
			h.ladders = append(h.ladders, l)
			h.mu.Unlock()
			return l, nil
		},
		Close: func(context.Context) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.closed++
			return nil
		},
	}, nil
}

// --- The spine ---------------------------------------------------------------------

type fakeGoalRun struct {
	sig       string
	variables map[string]string
	construct string
}

type fakeSpine struct {
	mu        sync.Mutex
	n         int
	goals     map[string]fakeGoalRun
	approvals map[string]*ApprovalState
	order     []string
	// closed records every goal run closed, and whether it was served.
	closed map[string]bool

	knobs fakeKnobs
	// steps are the executed goal runs' one step each, by run.
	steps map[string]*fakeStep
	// verdicts are every verdict given, in order; a later one is newer.
	verdicts []fakeVerdict
}

// fakeStep is one goal run's step: its versions, oldest first, and the one
// that is current.
type fakeStep struct {
	key      string
	versions []fakeStepVersion
	head     int
}

type fakeStepVersion struct {
	version    int
	status     string
	childRunId string
	level      string
}

type fakeVerdict struct {
	id, runId, stepKey string
	version            int
	verdict            string
	axes               []string
	reason             string
}

func (s *fakeSpine) OpenGoal(_ context.Context, g GoalOrder) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	vars := map[string]string{}
	for k, v := range g.Input {
		vars[k] = fmt.Sprint(v)
	}
	runId := fmt.Sprintf("v1:work:run:goal-%d", s.n)
	s.goals[runId] = fakeGoalRun{sig: g.GoalSignature, variables: vars, construct: g.ProcedureConstructId}
	return fmt.Sprintf("v1:work:goal:%d", s.n), runId, nil
}

func (s *fakeSpine) CloseGoal(_ context.Context, _ string, runId string, served bool, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.goals[runId]; !ok {
		return fmt.Errorf("no goal run %q", runId)
	}
	if _, done := s.closed[runId]; done {
		return fmt.Errorf("goal run %q closed twice", runId)
	}
	s.closed[runId] = served
	return nil
}

func (s *fakeSpine) DecideApproval(_ context.Context, _ string, approvalId, decision string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.approvals[approvalId]
	if !ok || a.Decision != "" {
		return fmt.Errorf("no pending approval %q", approvalId)
	}
	a.Decision = decision
	return nil
}

func (s *fakeSpine) PromotionApprovals(_ context.Context, _ string, known []string) ([]ApprovalState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Every pending one, and each known one whatever its decision -- the
	// engine's read-back answers the same union.
	want := map[string]bool{}
	for _, id := range known {
		want[id] = true
	}
	out := make([]ApprovalState, 0, len(s.order))
	for _, id := range s.order {
		if a := s.approvals[id]; a.Decision == "" || want[id] {
			out = append(out, *a)
		}
	}
	return out, nil
}

func (s *fakeSpine) raise(constructId string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := fmt.Sprintf("v1:work:approval:%d", len(s.order)+1)
	s.approvals[id] = &ApprovalState{ApprovalId: id, ConstructId: constructId}
	s.order = append(s.order, id)
	return id
}

func (s *fakeSpine) decision(approvalId string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.approvals[approvalId]; ok {
		return a.Decision
	}
	return ""
}

func (s *fakeSpine) goal(runId string) (fakeGoalRun, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.goals[runId]
	return g, ok
}

// ExecuteAppGoal is the executor's side of a goal the app serves: one step,
// version 1, handed to the app with a run context naming the run and the step,
// and the run closed as the session ended.
func (s *fakeSpine) ExecuteAppGoal(ctx context.Context, step GoalStep, serve AppStepServer) (StepRun, error) {
	s.mu.Lock()
	_, opened := s.goals[step.RunId]
	_, started := s.steps[step.RunId]
	_, closed := s.closed[step.RunId]
	s.mu.Unlock()
	switch {
	case !opened:
		return StepRun{}, fmt.Errorf("no goal run %q", step.RunId)
	case started || closed:
		return StepRun{}, fmt.Errorf("goal run %q executed twice", step.RunId)
	}
	ans, err := serve(common.ContextWithRun(ctx, common.RunContext{
		RunId: step.RunId, StepKey: step.StepKey, OwnerUserId: step.OwnerUserId, Mode: common.RunModeLive,
	}), AppStep{RunId: step.RunId, StepKey: step.StepKey, StepId: step.RunId + "-" + step.StepKey})
	if err != nil {
		return StepRun{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.steps[step.RunId] = &fakeStep{key: step.StepKey, head: 1,
		versions: []fakeStepVersion{{version: 1, status: fakeStepStatus(ans), childRunId: ans.ChildRunId}}}
	s.closed[step.RunId] = !ans.Failed
	return StepRun{Version: 1, Status: fakeRunStatus(ans)}, nil
}

// RerunStep is the act and the executor together: a new version, one past
// every version the step has, run with the level asked for and the newest
// dislike on the version it replaces as guidance -- on the step's context
// alone, as the executor puts it there.
func (s *fakeSpine) RerunStep(ctx context.Context, o RerunOrder, serve AppStepServer) (StepRun, error) {
	s.mu.Lock()
	st, ok := s.steps[o.RunId]
	if !ok || st.key != o.StepKey {
		s.mu.Unlock()
		return StepRun{}, fmt.Errorf("run %q has no step %q to run again", o.RunId, o.StepKey)
	}
	override := &common.StepOverride{Level: o.Level, RequestedBy: o.OwnerUserId}
	if v := s.newestVerdictLocked(o.RunId, o.StepKey, st.head); v != nil && v.verdict == scenario.VerdictDislike && !s.knobs.dropRerunGuidance {
		override.GuidanceAxes, override.GuidanceReason, override.FeedbackId = v.axes, v.reason, v.id
	}
	version := len(st.versions) + 1
	s.mu.Unlock()
	if override.Empty() {
		override = nil
	}
	ans, err := serve(common.ContextWithRun(ctx, common.RunContext{
		RunId: o.RunId, StepKey: o.StepKey, OwnerUserId: o.OwnerUserId, Mode: common.RunModeLive,
		Override: override, Workspace: fmt.Sprintf("%s-v%d", strings.TrimPrefix(o.RunId, "v1:work:run:"), version),
	}), AppStep{RunId: o.RunId, StepKey: o.StepKey, StepId: o.RunId + "-" + o.StepKey})
	if err != nil {
		return StepRun{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st.versions = append(st.versions, fakeStepVersion{version: version, status: fakeStepStatus(ans), childRunId: ans.ChildRunId, level: o.Level})
	if !s.knobs.headStays {
		st.head = version
	}
	s.closed[o.RunId] = !ans.Failed
	return StepRun{Version: version, Status: fakeRunStatus(ans)}, nil
}

// RecordFeedback keeps a verdict as the act does, refusing what it refuses: a
// version the step never had, and a dislike naming no axis.
func (s *fakeSpine) RecordFeedback(_ context.Context, o FeedbackOrder) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.steps[o.RunId]
	switch {
	case !ok || st.key != o.StepKey || o.Version < 1 || o.Version > len(st.versions):
		return "", fmt.Errorf("feedback_target_not_found: run %q records no version %d of %q", o.RunId, o.Version, o.StepKey)
	case o.Verdict == scenario.VerdictDislike && len(o.Axes) == 0:
		return "", fmt.Errorf("feedback_axis_required")
	}
	id := fmt.Sprintf("v1:work:observation:%d", len(s.verdicts)+1)
	s.verdicts = append(s.verdicts, fakeVerdict{id: id, runId: o.RunId, stepKey: o.StepKey, version: o.Version,
		verdict: o.Verdict, axes: append([]string(nil), o.Axes...), reason: o.Reason})
	return id, nil
}

// StepVersions answers every version of the run's step, the current one
// marked.
func (s *fakeSpine) StepVersions(_ context.Context, _ string, runId string) ([]StepVersionState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.steps[runId]
	if !ok {
		return nil, fmt.Errorf("run %q has no step", runId)
	}
	var out []StepVersionState
	for _, v := range st.versions {
		if s.knobs.forgetReplacedVersions && v.version != st.head {
			continue
		}
		out = append(out, StepVersionState{StepKey: st.key, Version: v.version, Status: v.status,
			Current: v.version == st.head, ChildRunId: v.childRunId, Level: v.level})
	}
	return out, nil
}

// newestVerdictLocked is the newest verdict on one version of one step.
func (s *fakeSpine) newestVerdictLocked(runId, stepKey string, version int) *fakeVerdict {
	var newest *fakeVerdict
	for i := range s.verdicts {
		v := &s.verdicts[i]
		if v.runId == runId && v.stepKey == stepKey && v.version == version {
			newest = v
		}
	}
	return newest
}

// excludes is the corpus's judgment of a recording by its parent step version
// (integrations/procedure parent_version.go): left out when that version is no
// longer the step's current one, or when its newest verdict is a dislike. A
// recording no step delegated is judged by nothing.
func (s *fakeSpine) excludes(parentRunId, recordingRunId string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.steps[parentRunId]
	if !ok {
		return false
	}
	for _, v := range st.versions {
		if v.childRunId != recordingRunId {
			continue
		}
		if v.version != st.head && !s.knobs.ignoreSuperseded {
			return true
		}
		if nv := s.newestVerdictLocked(parentRunId, st.key, v.version); nv != nil && nv.verdict == scenario.VerdictDislike && !s.knobs.ignoreDislikes {
			return true
		}
		return false
	}
	return false
}

func fakeStepStatus(a AppStepAnswer) string {
	if a.Failed {
		return "failed"
	}
	return "done"
}

func fakeRunStatus(a AppStepAnswer) string {
	if a.Failed {
		return "failed"
	}
	return "succeeded"
}

// --- The recorder ------------------------------------------------------------------

type fakeRecording struct {
	open    workerservice.RecordingOpen
	actions []workerservice.RecordedAction
	close   *workerservice.RecordingClose
}

// fakeRecorder is component/worker.SessionRecorder over memory.
type fakeRecorder struct {
	mu   sync.Mutex
	n    int
	runs map[string]*fakeRecording
	ids  []string
}

func (r *fakeRecorder) OpenRecording(_ context.Context, o workerservice.RecordingOpen) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if o.OwnerUserId == "" {
		return "", fmt.Errorf("no owner")
	}
	r.n++
	id := fmt.Sprintf("v1:work:run:recording-%d", r.n)
	r.runs[id] = &fakeRecording{open: o}
	r.ids = append(r.ids, id)
	return id, nil
}

func (r *fakeRecorder) RecordAction(_ context.Context, a workerservice.RecordedAction) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.runs[a.RunId]
	if !ok {
		return fmt.Errorf("no recording %q", a.RunId)
	}
	rec.actions = append(rec.actions, a)
	return nil
}

func (r *fakeRecorder) RecordGap(context.Context, workerservice.RecordedGap) error { return nil }

func (r *fakeRecorder) CloseRecording(_ context.Context, c workerservice.RecordingClose) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.runs[c.RunId]
	if !ok {
		return fmt.Errorf("no recording %q", c.RunId)
	}
	rec.close = &c
	return nil
}

// HeartbeatRecording is a no-op: a recording in memory cannot be abandoned.
func (r *fakeRecorder) HeartbeatRecording(context.Context, workerservice.RecordingHeartbeat) error {
	return nil
}

func (r *fakeRecorder) recording(id string) (*fakeRecording, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.runs[id]
	return rec, ok
}

// --- The ladder --------------------------------------------------------------------

type fakeConstruct struct {
	id    string
	sig   string
	state work.LadderState
	// recordings are the succeeded recordings learned from.
	recordings int
	// recordedFrom are the recordings the construct was lifted from, and
	// steps their actions as the fake writes them back (lifted).
	recordedFrom []string
	steps        []string
}

type fakeLadder struct {
	knobs  fakeKnobs
	policy work.LadderPolicy
	world  *ProcedureWorld
	app    AppHandover
	spine  *fakeSpine
	rec    *fakeRecorder

	mu         sync.Mutex
	constructs map[string]*fakeConstruct // by goal signature
	// learned are the succeeded recordings the driver asked to learn from, in
	// order, by signature: the corpus, before the platform's rules leave any
	// out.
	learned map[string][]string
	replays int
	// orders are every replay the driver asked for, as it asked.
	orders []ReplayOrder
}

// recordingGoal resolves a recording to the goal it served.
func (l *fakeLadder) recordingGoal(runId string) (*fakeRecording, fakeGoalRun, error) {
	rec, ok := l.rec.recording(runId)
	if !ok {
		return nil, fakeGoalRun{}, fmt.Errorf("no recording %q", runId)
	}
	g, ok := l.spine.goal(rec.open.ParentRunId)
	if !ok {
		return nil, fakeGoalRun{}, fmt.Errorf("recording %q names no goal run the spine opened", runId)
	}
	return rec, g, nil
}

func (l *fakeLadder) LearnFromRun(_ context.Context, _ string, runId string) (LearnReport, error) {
	rec, g, err := l.recordingGoal(runId)
	if err != nil {
		return LearnReport{}, err
	}
	if rec.close == nil || rec.close.Status != workerservice.AppSessionStatusEnded {
		return LearnReport{Reason: "not a succeeded recording"}, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.learned == nil {
		l.learned = map[string][]string{}
	}
	l.learned[g.sig] = append(l.learned[g.sig], runId)
	corpus := l.corpusLocked(g.sig)
	c, ok := l.constructs[g.sig]
	switch {
	case ok:
		c.recordings++
		return LearnReport{ConstructId: c.id, Lift: LiftUnchanged, Rung: string(c.state.Rung)}, nil
	case len(corpus) < 2:
		return LearnReport{Reason: "fewer than two recorded runs for this signature"}, nil
	}
	rung, reason := work.EntryRung(work.CandidateEvidence{Uses: len(corpus)})
	c = &fakeConstruct{id: fmt.Sprintf("v1:authoring:construct:%d", len(l.constructs)+1), sig: g.sig,
		state: work.LadderState{Rung: rung, DistinctBindings: map[string][]string{}}, recordings: len(corpus),
		recordedFrom: corpus, steps: l.lifted(corpus)}
	l.constructs[g.sig] = c
	return LearnReport{ConstructId: c.id, Lift: LiftCreated, Rung: string(rung), Reason: reason}, nil
}

// corpusLocked is one signature's corpus as it stands NOW: every recording
// learned from, less those the platform's rules leave out -- the corpus is read
// afresh at every lift, so a verdict given after a recording was learned from
// still decides whether the next lift sees it.
func (l *fakeLadder) corpusLocked(sig string) []string {
	var out []string
	for _, id := range l.learned[sig] {
		rec, ok := l.rec.recording(id)
		if !ok || l.spine.excludes(rec.open.ParentRunId, id) {
			continue
		}
		out = append(out, id)
	}
	return out
}

// lifted is the fake's account of a lifted procedure's steps: each
// recording's actions, the goal's input values written back as placeholders,
// and a position the recordings disagree on written {{?}} -- which is where
// the real learner would open a hole no goal input binds.
func (l *fakeLadder) lifted(corpus []string) []string {
	var steps []string
	for n, id := range corpus {
		rec, g, err := l.recordingGoal(id)
		if err != nil {
			return nil
		}
		mine := make([]string, 0, len(rec.actions))
		for _, a := range rec.actions {
			cmd, _ := a.Action.Args["command"].(string)
			argv := splitCommand(cmd)
			for i, word := range argv {
				for k, v := range g.variables {
					if word == v {
						argv[i] = "{{" + k + "}}"
					}
				}
			}
			mine = append(mine, strings.Join(argv, " "))
		}
		if n == 0 {
			steps = mine
			continue
		}
		if len(mine) < len(steps) {
			steps = steps[:len(mine)]
		}
		for i := range steps {
			a, b := strings.Fields(steps[i]), strings.Fields(mine[i])
			if len(a) != len(b) {
				steps[i] = "{{?}}"
				continue
			}
			for j := range a {
				if a[j] != b[j] {
					a[j] = "{{?}}"
				}
			}
			steps[i] = strings.Join(a, " ")
		}
	}
	return steps
}

// freeParameters are the fixture's placeholders: the fake's stand-in for the
// learned template's free holes, each bound from the goal input of that name.
func (l *fakeLadder) freeParameters() []string {
	seen := map[string]bool{}
	var out []string
	for _, st := range l.world.steps {
		for _, ph := range scenario.Placeholders(st.Target) {
			if !seen[ph] {
				seen[ph] = true
				out = append(out, ph)
			}
		}
	}
	sort.Strings(out)
	return out
}

func (l *fakeLadder) ShadowCompare(_ context.Context, _ string, runId string) ([]ReplayReport, error) {
	rec, g, err := l.recordingGoal(runId)
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	c, ok := l.constructs[g.sig]
	l.mu.Unlock()
	if !ok || c.state.Rung != work.RungShadow {
		return nil, nil
	}
	match := len(rec.actions) == len(l.world.steps)
	for i, st := range l.world.steps {
		if !match {
			break
		}
		// SANDBOXED: the comparison never reaches the world the app did.
		ans := l.world.exec(scenario.Render(st.Target, g.variables), fmt.Sprintf("shadow:%s:%d", runId, i), true)
		app := rec.actions[i].Action
		match = app.ExitCode != nil && *app.ExitCode == ans.ExitCode && app.IsError == ans.IsError
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	bindings := map[string]string{}
	for _, p := range l.freeParameters() {
		bindings[p] = g.variables[p]
	}
	t := work.Advance(c.state, work.LadderEvent{Kind: work.EventShadowCompared, Match: match, Bindings: bindings, FreeParameters: l.freeParameters()}, l.policy)
	rep := ReplayReport{ConstructId: c.id, Mode: "shadow", Match: match, From: string(t.From), To: string(t.To), Rung: string(t.From)}
	if t.Propose {
		t.State.PromotionApprovalId = l.spine.raise(c.id)
		rep.Proposed, rep.PromotionApprovalId = true, t.State.PromotionApprovalId
	}
	c.state = t.State
	return []ReplayReport{rep}, nil
}

func (l *fakeLadder) DecidePromotion(_ context.Context, _ string, approvalId string) (LadderMove, error) {
	decision := l.spine.decision(approvalId)
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, c := range l.constructs {
		if c.state.PromotionApprovalId != approvalId {
			continue
		}
		if l.knobs.ignoreDecisions {
			return LadderMove{From: string(c.state.Rung), To: string(c.state.Rung), Reason: "ignored"}, nil
		}
		t := work.Advance(c.state, work.LadderEvent{Kind: work.EventPromotionDecided, Approved: decision == scenario.DecideApproved}, l.policy)
		c.state = t.State
		return LadderMove{From: string(t.From), To: string(t.To), Reason: t.Reason}, nil
	}
	return LadderMove{}, fmt.Errorf("no construct carries approval %q", approvalId)
}

func (l *fakeLadder) Replay(ctx context.Context, o ReplayOrder) (ReplayReport, error) {
	l.mu.Lock()
	var c *fakeConstruct
	for _, x := range l.constructs {
		if x.id == o.ConstructId {
			c = x
		}
	}
	l.replays++
	l.orders = append(l.orders, o)
	replayRun := fmt.Sprintf("v1:work:run:replay-%d", l.replays)
	l.mu.Unlock()
	if c == nil {
		return ReplayReport{}, fmt.Errorf("no construct %q", o.ConstructId)
	}
	rep := ReplayReport{ConstructId: c.id, Mode: o.Mode, ReplayRunId: replayRun, ModelCalls: l.knobs.replayModelCalls, From: string(c.state.Rung), Rung: string(c.state.Rung)}
	if l.knobs.serveShadow && c.state.Rung == work.RungShadow {
		// The lie carried through: the runner reports the rung it was told.
		rep.Rung = string(work.RungTrusted)
	}
	vars := map[string]string{}
	for k, v := range o.Input {
		vars[k] = fmt.Sprint(v)
	}
	for i, st := range l.world.steps {
		key := fmt.Sprintf("%s:step%d:1", replayRun, i)
		ans := l.world.exec(scenario.Render(st.Target, vars), key, false)
		if ans.IsError || ans.ExitCode != 0 {
			rep.Diverged, rep.DivergedStep = true, i
			rep.Diagnosis = fmt.Sprintf("step %d (%s) exited %d where every recording exited 0: %s", i, st.Key, ans.ExitCode, ans.Error)
			break
		}
		rep.Completed = append(rep.Completed, CompletedReport{Index: i, Tool: st.Type, IdempotencyKey: key, SideEffect: ans.Delivered})
	}
	l.mu.Lock()
	t := work.Advance(c.state, work.LadderEvent{Kind: work.EventReplayed, Match: !rep.Diverged, Insufficient: rep.Diverged}, l.policy)
	c.state = t.State
	rep.To = string(t.To)
	l.mu.Unlock()
	if !rep.Diverged {
		rep.Served = true
		return rep, nil
	}
	rep.Code = "procedure_diverged"
	guidance := rep.Completed
	if l.knobs.dropGuidance {
		guidance = nil
	}
	if _, err := l.app.Handover(ctx, HandoverOrder{OwnerUserId: o.OwnerUserId, GoalRunId: o.GoalRunId, Completed: guidance, Diagnosis: rep.Diagnosis}); err != nil {
		return rep, err
	}
	rep.FellBack = true
	return rep, nil
}

func (l *fakeLadder) ProcedureFor(_ context.Context, _ string, sig string) (ConstructState, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	c, ok := l.constructs[sig]
	if !ok {
		return ConstructState{}, false, nil
	}
	cs := ConstructState{ConstructId: c.id, GoalSignature: c.sig, Rung: string(c.state.Rung), PromotionApprovalId: c.state.PromotionApprovalId}
	if l.knobs.serveShadow && c.state.Rung == work.RungShadow {
		// The lie: a shadow procedure reported as trusted.
		cs.Rung = string(work.RungTrusted)
	}
	return cs, true, nil
}

func (l *fakeLadder) Constructs(context.Context, string) ([]ConstructState, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]ConstructState, 0, len(l.constructs))
	for _, c := range l.constructs {
		out = append(out, ConstructState{ConstructId: c.id, GoalSignature: c.sig, Rung: string(c.state.Rung), PromotionApprovalId: c.state.PromotionApprovalId,
			RecordedFrom: append([]string(nil), c.recordedFrom...), Steps: append([]string(nil), c.steps...)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ConstructId < out[j].ConstructId })
	return out, nil
}
