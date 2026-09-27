package proving

// procedure_fakes_test.go -- a WELL-BEHAVED stand-in for the learned-procedure
// half of the platform, so the lifecycle driver, its figures and its verifier
// can be exercised with no engine and no database.
//
// It behaves as the plan says the runner does
// (docs/superpowers/plans/2026-09-23-procedure-certification-replay.md,
// section 1.4 and Task 5): a construct per goal signature, lifted on its
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
// Each knob below breaks one behaviour on purpose, for a negative control run
// on the TEST: the figure that depends on the behaviour must move.

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/znasllc-io/memql/component/proving/scenario"
	"github.com/znasllc-io/memql/component/work"
	workerservice "github.com/znasllc-io/memql/component/worker"
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
	spine := &fakeSpine{goals: map[string]fakeGoalRun{}, approvals: map[string]*ApprovalState{}, closed: map[string]bool{}}
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
	recordings map[string]int            // succeeded recordings per signature
	replays    int
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
	if l.recordings == nil {
		l.recordings = map[string]int{}
	}
	l.recordings[g.sig]++
	c, ok := l.constructs[g.sig]
	switch {
	case ok:
		c.recordings++
		return LearnReport{ConstructId: c.id, Lift: LiftUnchanged, Rung: string(c.state.Rung)}, nil
	case l.recordings[g.sig] < 2:
		return LearnReport{Reason: "fewer than two recorded runs for this signature"}, nil
	}
	rung, reason := work.EntryRung(work.CandidateEvidence{Uses: l.recordings[g.sig]})
	c = &fakeConstruct{id: fmt.Sprintf("v1:authoring:construct:%d", len(l.constructs)+1), sig: g.sig,
		state: work.LadderState{Rung: rung, DistinctBindings: map[string][]string{}}, recordings: l.recordings[g.sig]}
	l.constructs[g.sig] = c
	return LearnReport{ConstructId: c.id, Lift: LiftCreated, Rung: string(rung), Reason: reason}, nil
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
		out = append(out, ConstructState{ConstructId: c.id, GoalSignature: c.sig, Rung: string(c.state.Rung), PromotionApprovalId: c.state.PromotionApprovalId})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ConstructId < out[j].ConstructId })
	return out, nil
}
