package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/node"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
	workspine "github.com/znasllc-io/memql/integrations/work"
)

func TestExecutionHopRestoresOwnerGoalAndReplay(t *testing.T) {
	j := &automations.RunJournal{RunId: "r", GoalId: "g", OwnerUserId: "u", Mode: "replay", ReplayPolicy: "strict", ForkedFromRunId: "source"}
	source := &automations.RunJournal{RunId: "source", GoalId: "g", OwnerUserId: "u", StepOrder: []string{"draft", "file"}}
	ctx, err := workExecutionContext(context.Background(), j, source, nil)
	if err != nil {
		t.Fatal(err)
	}
	run, ok := common.RunFromContext(ctx)
	if !ok || run.RunId != "r" || run.GoalId != "g" || run.OwnerUserId != "u" || run.Mode != "replay" || run.SourceRunId != "source" || run.SourceGoalId != "g" || len(run.StepOrder) != 2 {
		t.Fatalf("lost run context: %+v", run)
	}
	ac, _ := auth.AccessFromContext(ctx)
	if ac == nil || ac.UserId != "u" || ac.Synthetic {
		t.Fatalf("lost owner: %+v", ac)
	}
}

func TestExecutionHopRefusesAnotherGoalsJournal(t *testing.T) {
	j := &automations.RunJournal{RunId: "r", GoalId: "g", OwnerUserId: "u", Mode: "replay", ForkedFromRunId: "source"}
	_, err := workExecutionContext(context.Background(), j, &automations.RunJournal{RunId: "source", GoalId: "other", OwnerUserId: "u"}, nil)
	if err == nil {
		t.Fatal("replay accepted a different goal's model journal")
	}
}

func TestExecutionHopBindsOnlyThePersistedOwnersForwardedAuthority(t *testing.T) {
	parent := auth.ContextWithInternalOrigin(auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "system:maintenance", Role: auth.RoleOwner, Synthetic: true}))
	j := &automations.RunJournal{RunId: "run", GoalId: "goal", OwnerUserId: "v1:identity:user:alice", Mode: "live"}
	ctx, err := workExecutionContext(parent, j, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertion, ok := auth.ForwardedAuthorityFromContext(ctx)
	if !ok {
		t.Fatal("execution cannot forward model calls: no forwarded authority for the persisted owner")
	}
	// Exercise the wire conversion and the same verifier as the worker
	// model-call receiver, rather than trusting the producer's local actor.
	wire := node.ForwardedAuthorityToProto(assertion, "agent-origin", "agent")
	verified, err := auth.VerifyForwardedAuthority(node.ForwardedAuthorityFromProto(wire), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if verified.UserId != "v1:identity:user:alice" || verified.Role != auth.RoleWriter || verified.Synthetic || verified.Unranked || verified.IsClusterOwner() {
		t.Fatalf("execution forwarded authority beyond the persisted owner: %+v", verified)
	}
	if auth.OriginFromContext(ctx) != auth.OriginClient {
		t.Fatal("borrowed owner inherited internal call origin")
	}
	claims := assertion.Principal().Claims
	if claims["sub"] != verified.UserId || claims["role"] != string(verified.Role) {
		t.Fatalf("forwarded attribution differs from authorization: %+v / %+v", claims, verified)
	}
}

func TestExecutionHopRefusesAMissingPersistedOwner(t *testing.T) {
	for _, owner := range []string{"", "  "} {
		_, err := workExecutionContext(context.Background(), &automations.RunJournal{RunId: "run", GoalId: "goal", OwnerUserId: owner}, nil, nil)
		if err == nil {
			t.Fatalf("execution accepted missing persisted owner %q", owner)
		}
	}
}

// The receiver starts with no browser session; the journal and current identity
// read are the only authority allowed to survive this hop.
func TestExecutionHopCarriesIntakeCeilingToModelReceiver(t *testing.T) {
	for _, ceiling := range []auth.Role{auth.RoleOwner, auth.RoleReader} {
		resolver := auth.NewIdentityResolver(auth.QueryRunnerFunc(func(context.Context, string) (any, error) {
			return map[string]any{"role": "owner"}, nil
		}), nil)
		journal := &automations.RunJournal{RunId: "r", GoalId: "g", OwnerUserId: "v1:identity:user:alice", ExecutionAuthority: map[string]any{"roleCeiling": string(ceiling), "credentialClass": auth.ForwardedClassUser}}
		ctx, err := workExecutionContext(context.Background(), journal, nil, resolver)
		if err != nil {
			t.Fatal(err)
		}
		assertion, _ := auth.ForwardedAuthorityFromContext(ctx)
		wire := node.ForwardedAuthorityToProto(assertion, "receiving-agent", "agent")
		received, err := auth.VerifyForwardedAuthority(node.ForwardedAuthorityFromProto(wire), time.Now())
		if err != nil || received.Role != ceiling || received.UserId != journal.OwnerUserId {
			t.Fatalf("lost or expanded authority: %+v %v", received, err)
		}
	}
}

// rerunDispatchTemplate is a three-step statement template, compiled the way
// the dispatcher's loader compiles one.
func rerunDispatchTemplate(t *testing.T) *automations.Automation {
	t.Helper()
	a, err := automations.NewLoader(automations.LoaderOptions{}).CompileSource(`@trigger(event="probe.fired")
automation drafts {
  a := builtin fetch()
  b := builtin draft(from: a)
  builtin publish(text: b)
}`, "drafts.memql")
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// finishedDispatchJournal is a goal run whose three steps finished, carrying
// the request the person's act wrote.
func finishedDispatchJournal(spec *automations.RerunSpec) *automations.RunJournal {
	done := automations.StepState{Status: "done", Attempt: 1, Version: 1}
	return &automations.RunJournal{
		RunId: "r1", GoalId: "v1:work:goal:g1", OwnerUserId: "u1", AutomationName: "drafts", Status: "running", Mode: "live",
		StepOrder: []string{"a", "b", "publish"},
		Steps: map[string]*automations.MinimalStepResult{
			"a": {StepId: "a", Status: "success", Value: "A1"}, "b": {StepId: "b", Status: "success", Value: "B1"}, "publish": {StepId: "publish", Status: "success"},
		},
		StepStates: map[string]automations.StepState{"a": done, "b": done, "publish": done},
		MaxAttempt: map[string]int{"a": 1, "b": 1, "publish": 1},
		Rerun:      spec,
	}
}

// The person's act refuses a nested target before it writes anything; this is
// the same refusal where the request is served, for a request that reached the
// row some other way. The code is the act's own, and nothing is prepared to run.
func TestARerunOfANestedStepIsRefusedAtDispatch(t *testing.T) {
	auto := rerunDispatchTemplate(t)
	for _, key := range []string{"for_x/0/touch", "b/draft"} {
		j := finishedDispatchJournal(&automations.RerunSpec{RequestId: "req-1", Reason: automations.RerunReasonRerun, StepKey: key})
		resume, opts, code, err := workRerunResumption(j, nil, auto)
		if code != workRerunStepNested || err == nil || !strings.Contains(err.Error(), key) {
			t.Fatalf("nested target %q: code %q, err %v -- want %s naming the step", key, code, err, workRerunStepNested)
		}
		if resume != nil || opts != nil {
			t.Fatalf("nested target %q: a refused request was still prepared to run", key)
		}
	}
	j := finishedDispatchJournal(&automations.RerunSpec{RequestId: "req-1", Reason: automations.RerunReasonRerun, StepKey: "nowhere"})
	if _, _, code, _ := workRerunResumption(j, nil, auto); code != workRerunStepNotInRun {
		t.Fatalf("unknown target: code %q, want %s", code, workRerunStepNotInRun)
	}
}

func TestARerunIsPreparedFromTheRowAtDispatch(t *testing.T) {
	auto := rerunDispatchTemplate(t)
	j := finishedDispatchJournal(&automations.RerunSpec{RequestId: "req-1", Reason: automations.RerunReasonRerun, StepKey: "b"})
	resume, opts, code, err := workRerunResumption(j, nil, auto)
	if err != nil || code != "" {
		t.Fatalf("refused: %s %v", code, err)
	}
	if opts.FromStep != "b" || !opts.AllowSideEffects || opts.Rerun == nil || opts.Rerun.RequestId != "req-1" {
		t.Fatalf("options = %+v", opts)
	}
	if !resume.CallerSuppliedPayload {
		t.Fatal("a goal's re-run lost the caller-supplied mark; its steps would reach internal origin on the goal caller's arguments")
	}
	if j.Rerun == nil || resume.RunId != "r1" {
		t.Fatal("the prepared journal is not the run's")
	}
}

func TestAGoallessRerunIsRefusedAtDispatch(t *testing.T) {
	j := finishedDispatchJournal(&automations.RerunSpec{RequestId: "req-1", Reason: automations.RerunReasonRerun, StepKey: "b"})
	j.GoalId, j.OwnerUserId = "", ""
	if _, _, code, err := workRerunResumption(j, nil, rerunDispatchTemplate(t)); code != workRerunNeedsGoal || err == nil {
		t.Fatalf("code %q, err %v, want %s", code, err, workRerunNeedsGoal)
	}
}

func TestARerunWithAnUnknownReasonIsRefusedAtDispatch(t *testing.T) {
	auto := rerunDispatchTemplate(t)
	j := finishedDispatchJournal(&automations.RerunSpec{RequestId: "req-1", Reason: "rewind", StepKey: "b"})
	if _, _, code, _ := workRerunResumption(j, nil, auto); code != workRerunReasonInvalid {
		t.Fatalf("unknown reason: code %q, want %s", code, workRerunReasonInvalid)
	}
	// A branch request on a run that is not a fork is not a branch.
	j = finishedDispatchJournal(&automations.RerunSpec{RequestId: "req-1", Reason: automations.RerunReasonBranch, StepKey: "b"})
	if _, _, code, _ := workRerunResumption(j, nil, auto); code != workRerunReasonInvalid {
		t.Fatalf("branch on a live run: code %q, want %s", code, workRerunReasonInvalid)
	}
}

func TestABranchIsPreparedFromItsSourceAtDispatch(t *testing.T) {
	auto := rerunDispatchTemplate(t)
	source := finishedDispatchJournal(nil)
	source.RunId = "src1"
	fork := &automations.RunJournal{
		RunId: "fork1", GoalId: "v1:work:goal:g1", OwnerUserId: "u1", AutomationName: "drafts", Status: "running",
		Mode: "fork", ForkedFromRunId: "src1", ForkAtStepKey: "b",
		Head:       work.Head{"a": {Version: 1, RunId: "src1"}},
		Steps:      map[string]*automations.MinimalStepResult{},
		StepStates: map[string]automations.StepState{},
		MaxAttempt: map[string]int{},
		Rerun:      &automations.RerunSpec{RequestId: "req-b", Reason: automations.RerunReasonBranch, StepKey: "b"},
	}
	if got := automations.RerunSources(fork, auto); len(got) != 1 || got[0] != "src1" {
		t.Fatalf("RerunSources = %v, want the run the prefix lives in", got)
	}
	resume, opts, code, err := workRerunResumption(fork, []*automations.RunJournal{source}, auto)
	if err != nil {
		t.Fatalf("refused: %s %v", code, err)
	}
	if opts.FromStep != "b" {
		t.Fatalf("the branch resumes at %q, want its fork step", opts.FromStep)
	}
	if m := resume.Steps["a"]; m == nil || m.Value != "A1" {
		t.Fatalf("the prefix was not rehydrated from the source: %+v", resume.Steps["a"])
	}
	if resume.RunId != "fork1" {
		t.Fatalf("the prepared journal is %s's, want the branch's own", resume.RunId)
	}

	// A source of another goal, or another owner, serves no prefix.
	stranger := finishedDispatchJournal(nil)
	stranger.RunId, stranger.GoalId = "src1", "v1:work:goal:other"
	if _, _, code, _ := workRerunResumption(fork, []*automations.RunJournal{stranger}, auto); code != workRerunSourceRefused {
		t.Fatalf("another goal's source: code %q, want %s", code, workRerunSourceRefused)
	}
	// And a prefix the source cannot serve refuses the branch rather than
	// running the prefix in it.
	if _, _, code, _ := workRerunResumption(fork, nil, auto); code != workRerunPrefixUnserved {
		t.Fatalf("no source loaded: code %q, want %s", code, workRerunPrefixUnserved)
	}
}

// A re-run is claimed under its own request id; the dispatch serves the row's
// request only under that claim, or under the sweep's recovery.
func TestADispatchServesOnlyTheRerunItsClaimIsFor(t *testing.T) {
	pending := finishedDispatchJournal(&automations.RerunSpec{RequestId: "req-2", Reason: automations.RerunReasonRerun, StepKey: "b"})
	plain := finishedDispatchJournal(nil)
	for _, tc := range []struct {
		name string
		req  workspine.DispatchRequest
		j    *automations.RunJournal
		want bool
	}{
		{"the request's own claim", workspine.DispatchRequest{RerunRequestId: "req-2"}, pending, true},
		{"a claim for an earlier request", workspine.DispatchRequest{RerunRequestId: "req-1"}, pending, false},
		{"a late event from the run's previous execution", workspine.DispatchRequest{}, pending, false},
		{"the sweep's recovery", workspine.DispatchRequest{Recovery: true}, pending, true},
		{"an ordinary run", workspine.DispatchRequest{}, plain, true},
		{"a claim for a request the row no longer carries", workspine.DispatchRequest{RerunRequestId: "req-2"}, plain, false},
	} {
		if got := workRerunServable(tc.req, tc.j); got != tc.want {
			t.Errorf("%s: servable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A replay runs each step with the override its source recorded; nothing else
// the dispatcher executes does.
func TestAReplayRunsWithTheOverridesItsSourceRecorded(t *testing.T) {
	source := &automations.RunJournal{RunId: "src1", StepOverrides: map[string]*common.StepOverride{"b": {Prompt: "Name the regions."}}}
	replay := &automations.RunJournal{RunId: "r2", Mode: common.RunModeReplay, ForkedFromRunId: "v1:work:run:src1"}
	if got := workReplayOverrides(replay, source); got["b"] == nil || got["b"].Prompt != "Name the regions." {
		t.Fatalf("a replay's overrides = %+v, want its source's", got)
	}
	for _, j := range []*automations.RunJournal{
		{RunId: "r3", Mode: common.RunModeLive, ForkedFromRunId: "src1"},
		{RunId: "r4", Mode: common.RunModeFork, ForkedFromRunId: "src1"},
		{RunId: "r5", Mode: common.RunModeReplay, ForkedFromRunId: "another"},
	} {
		if got := workReplayOverrides(j, source); got != nil {
			t.Errorf("run %s (mode %s, from %s) took the source's overrides: %+v", j.RunId, j.Mode, j.ForkedFromRunId, got)
		}
	}
}
