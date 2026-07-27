package automations

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/events"
)

// originCapturingRegistry records the origin of the context each step is
// dispatched with.
//
// `called` exists because the zero value of auth.CallOrigin IS OriginClient.
// A negative assertion spelled `if reg.got.IsInternal()` therefore passes when
// NOTHING RAN -- if ExecuteWithEvent bails before dispatch (stricter arg
// binding, a precondition, a concept that stops resolving), the test reports
// "it ran and was untrusted" having executed nothing at all. Every assertion
// below checks `called` first. For a negative security assertion, "nothing
// happened" must not read as "it happened and was safe".
type originCapturingRegistry struct {
	got    auth.CallOrigin
	called bool
}

func (r *originCapturingRegistry) Execute(ctx context.Context, step *Step, stepCtx *StepContext) (*StepResult, error) {
	// FIRST dispatch only. Overwriting on every call meant a fixture with two
	// steps silently asserted on the LAST one -- and the resume tests below run
	// a multi-step loop. The app-side twin already did this; the two disagreed.
	if !r.called {
		r.got = auth.OriginFromContext(ctx)
		r.called = true
	}
	return &StepResult{Status: "success"}, nil
}

// TestStepDispatchCarriesInternalOrigin pins the fix for the regression that
// made memql#2800's first attempt ship a broken kill switch.
//
// The internal stamp was originally applied to executeInput -- the automation's
// `input:` block. No STEP goes through that path: every step type dispatches
// via stepRegistry.Execute, which received an unstamped context and therefore
// OriginClient. killSwitchSuspendsRunningPlans reads runningPlansForUser
// (@serverOnly) from a step, so the decide step was refused as a client call
// and no plan was suspended when a user tripped the computer-use kill switch.
//
// The DSL comment asserted the opposite ("the automation path stamps ... so it
// keeps working"), and nothing tested it, so the security control failed
// closed-looking but open -- exactly what the issue's park comment predicted.
//
// Asserting on the STEP DISPATCH BOUNDARY rather than on the kill switch
// specifically is deliberate: it holds for every step type and every
// @serverOnly construct any automation reaches later, instead of pinning one
// automation that happens to exercise it today.
func TestStepDispatchCarriesInternalOrigin(t *testing.T) {
	reg := &originCapturingRegistry{}
	e := &Executor{
		logger:       slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		stepRegistry: reg,
	}

	step := &Step{ID: "s1", Name: "decide", Type: StepTypeFunction}
	stepCtx := &StepContext{Execution: NewExecution("killSwitchSuspendsRunningPlans", "test")}
	stepCtx.Execution.SourceTrusted = true // loaded from the registered DSL tree
	if _, err := e.executeStep(context.Background(), step, stepCtx); err != nil {
		t.Fatalf("executeStep: %v", err)
	}

	if !reg.got.IsInternal() {
		t.Fatalf("step dispatched with origin %v, want internal.\n"+
			"An automation step is server-side by construction -- an AUTHORED body "+
			"dispatched from a graph event -- so it must be able to reach @serverOnly "+
			"constructs. Without this the computer-use kill switch is refused as a "+
			"client call and silently suspends nothing.", reg.got)
	}
}

// TestUnstampedExecutorContextIsStillClient guards the other direction: the
// stamp must come from executeStep, not from something ambient. If the origin
// were internal before executeStep applied it, the assertion above would pass
// for the wrong reason and could not detect the stamp being removed.
func TestUnstampedExecutorContextIsStillClient(t *testing.T) {
	if auth.OriginFromContext(context.Background()).IsInternal() {
		t.Fatal("a bare context reported internal origin")
	}
}

// TestCallerSuppliedAutomationDoesNotGetInternalOrigin closes a working BYPASS
// that a previous revision of this file's fix introduced.
//
// The stamp was applied in executeStep unconditionally, justified by
// "executeStep is reachable only from automation execution and resume". That
// is true and is not a security argument, because AUTOMATION EXECUTION
// INCLUDES AUTOMATIONS WHOSE BODY THE CALLER SUPPLIED: RunBundleDryRun
// compiles submitted source and drives this very Executor. So MCP
// run_inline_automation and the planner's LLM-emitted bundle could each wrap a
// @serverOnly read in a step and have it execute with internal origin --
// laundering client origin into trusted, and handing a caller the full user
// row that userById's admin gate denies them.
//
// It also falsified the headline claim that "a cluster owner over the wire is
// still refused", since the MCP path is reachable by an owner.
//
// Trust now rides on the automation's SOURCE (Automation.Trusted, granted only
// by the unified tree loader) rather than on which function does the
// dispatching. False is the zero value, so anything compiled from submitted
// source is untrusted without having to be recognised as such.
func TestCallerSuppliedAutomationDoesNotGetInternalOrigin(t *testing.T) {
	reg := &originCapturingRegistry{}
	e := &Executor{
		logger:       slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		stepRegistry: reg,
	}

	step := &Step{ID: "s1", Name: "steal", Type: StepTypeFunction}
	// An execution whose source was NOT the registered tree -- exactly what
	// RunBundleDryRun produces from caller-submitted bundle source.
	stepCtx := &StepContext{Execution: NewExecution("attackerSubmitted", "dryrun")}
	if stepCtx.Execution.SourceTrusted {
		t.Fatal("NewExecution defaulted to trusted; caller-supplied source would be trusted by default")
	}

	if _, err := e.executeStep(context.Background(), step, stepCtx); err != nil {
		t.Fatalf("executeStep: %v", err)
	}

	if reg.got.IsInternal() {
		t.Fatalf("a caller-supplied automation dispatched with origin %v -- "+
			"this is a client-to-@serverOnly bypass: submit a bundle whose step "+
			"reads userByIdSystem and the gate is skipped", reg.got)
	}
}

// TestAutomationTrustIsGrantedOnlyByTheTreeLoader pins where trust comes from.
// A second grant site added later would reopen the bypass, and the grep that
// would catch it is easy to skip.
func TestAutomationTrustIsGrantedOnlyByTheTreeLoader(t *testing.T) {
	if (&Automation{}).Trusted {
		t.Error("Automation.Trusted is not false by default; caller-supplied " +
			"automations would be trusted unless explicitly marked, which is the " +
			"wrong direction for this flag")
	}
	if NewExecution("x", "y").SourceTrusted {
		t.Error("AutomationExecution.SourceTrusted is not false by default")
	}
}

// TestTreeLoadedAutomationReachesDispatchTrusted is the END-TO-END link the
// two tests above do not cover, and its absence was the review finding that
// mattered most on this change.
//
// Those tests set SourceTrusted BY HAND, so the chain that actually produces
// it -- unified loader sets Automation.Trusted, ExecuteWithEvent mirrors it
// onto AutomationExecution.SourceTrusted, executeStep reads it -- had zero
// coverage at any link. All three production lines could be deleted with the
// full suite green, and deleting either of the first two silently reproduces
// the original defect: every tree-loaded automation reverts to untrusted, the
// stamp stops, and the kill switch is refused as a client call while the DSL
// comment says it works.
//
// This drives a REAL automation from the registered tree through the REAL
// ExecuteWithEvent and asserts on the origin the step dispatch actually
// receives, so each link is load-bearing for the assertion.
func TestTreeLoadedAutomationReachesDispatchTrusted(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	loader := NewLoader(LoaderOptions{Logger: logger})

	all, err := loader.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(all) == 0 {
		t.Fatal("no automations loaded from the registered tree")
	}
	for _, a := range all {
		if !a.Trusted {
			t.Errorf("automation %q loaded from the registered tree is NOT trusted; "+
				"every tree-loaded automation must be, or it silently loses the "+
				"ability to reach @serverOnly constructs", a.Name)
		}
	}

	// Drive the MOTIVATING automation through the real execution path, by name.
	//
	// This used to take all[0] -- whatever the tree walk happened to yield
	// first (bootstrapCluster). That pins an arbitrary fixture: adding a domain
	// that sorts earlier, or giving that automation an input: block or a
	// required arg, breaks this test for reasons unrelated to origin. Worse,
	// the automation it did NOT cover is the only one that actually reaches a
	// @serverOnly construct.
	subject := automationNamed(t, all, "killSwitchSuspendsRunningPlans")

	reg := &originCapturingRegistry{}
	e := NewExecutor(ExecutorOptions{Logger: logger, StepRegistry: reg})
	defer e.Close()

	if _, err := e.ExecuteWithEvent(context.Background(), subject, "test", nil); err != nil {
		t.Logf("ExecuteWithEvent returned %v (fine -- the origin is what is under test)", err)
	}
	if !reg.called {
		t.Fatal("no step was dispatched, so this test asserted nothing about origin")
	}
	if !reg.got.IsInternal() {
		t.Fatalf("a tree-loaded automation dispatched its step with origin %v, want internal. "+
			"One of the three links is broken: unified_loader sets Automation.Trusted, "+
			"ExecuteWithEvent mirrors it to SourceTrusted, executeStep reads it.", reg.got)
	}
}

// automationNamed finds one automation by name, failing loudly if the tree no
// longer carries it. A silent fallback to "some other automation" would make
// the caller assert against a fixture that does not exercise the property.
func automationNamed(t *testing.T, all []*Automation, name string) *Automation {
	t.Helper()
	for _, a := range all {
		if a.Name == name {
			return a
		}
	}
	names := make([]string, 0, len(all))
	for _, a := range all {
		names = append(names, a.Name)
	}
	t.Fatalf("automation %q is not in the loaded tree -- it is the one that reaches a "+
		"@serverOnly construct, so this test needs it or an equivalent. Loaded: %v", name, names)
	return nil
}

// TestResumedAutomationReachesDispatchTrusted covers the THIRD site that
// mirrors Automation.Trusted onto the execution, and the one #2879's first
// round claimed to have covered and had not.
//
// resume.go:78 (`exec.SourceTrusted = automation.Trusted`) could be deleted
// with ./component/automations/... entirely green, because both end-to-end
// tests drive ExecuteWithEvent and nothing drove ResumeFrom.
//
// The failure it guards is the same closed-looking-but-broken shape as the
// original defect: a checkpointed kill-switch automation resumes,
// SourceTrusted is false, executeStep does not stamp, runningPlansForUser is
// refused as a client call, and the resumed run silently suspends nothing.
func TestResumedAutomationReachesDispatchTrusted(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	automation := &Automation{
		Name:    "zzResumeTrustProbe",
		Trusted: true, // as the unified tree loader sets it
		Steps: []*Step{
			{ID: "s1", Name: "first", Type: StepTypeFunction},
			{ID: "s2", Name: "second", Type: StepTypeFunction},
		},
	}

	reg := &originCapturingRegistry{}
	e := NewExecutor(ExecutorOptions{Logger: logger, StepRegistry: reg})
	defer e.Close()

	// AutomationFingerprint is left empty on purpose: ValidateCheckpoint only
	// compares it when set, and pinning a fingerprint here would couple this
	// origin test to the definition-hashing scheme.
	checkpoint := &ExecutionCheckpoint{
		ExecutionId:    "exec-resume-probe",
		AutomationName: automation.Name,
		StepIndex:      1,
		FailedAt:       &StepFailure{StepId: "s2"},
	}

	if _, err := e.ResumeFrom(context.Background(), checkpoint, automation, &ResumeOptions{AllowSideEffects: true}); err != nil {
		t.Logf("ResumeFrom returned %v (fine -- the origin is what is under test)", err)
	}
	if !reg.called {
		t.Fatal("resume dispatched no step, so this test asserted nothing about origin. " +
			"Check the checkpoint fixture against ValidateCheckpoint rather than deleting this guard")
	}
	if !reg.got.IsInternal() {
		t.Fatalf("a RESUMED tree-loaded automation dispatched with origin %v, want internal. "+
			"resume.go's `exec.SourceTrusted = automation.Trusted` is the link; without it a "+
			"checkpointed kill-switch run silently suspends nothing.", reg.got)
	}
}

// TestOriginForSource covers the automation `input:` block's trust decision --
// the fourth #2800 production line that was deletable with a green suite.
//
// Both end-to-end tests drive automations with NO input: block, so the branch
// was never entered and collapsing it to an unconditional `ctx` left
// ./component/automations/... and ./component/memql/... entirely green.
//
// Direction matters in both ways here, so both are asserted: stamping an
// UNTRUSTED body is the round-2 bypass (a caller wraps a @serverOnly read in a
// submitted bundle's input block), and failing to stamp a TRUSTED one is the
// original defect (a tree automation silently loses the ability to read).
func TestOriginForSource(t *testing.T) {
	if got := auth.OriginFromContext(originForSource(context.Background(), true)); !got.IsInternal() {
		t.Errorf("a TRUSTED automation's input block got origin %v, want internal -- "+
			"a tree automation would silently lose access to @serverOnly constructs", got)
	}
	if got := auth.OriginFromContext(originForSource(context.Background(), false)); got.IsInternal() {
		t.Errorf("an UNTRUSTED automation's input block got origin %v, want client -- "+
			"that is the bypass: caller-submitted source reaching a @serverOnly construct", got)
	}
	// An inherited internal mark must not survive an untrusted body. Without
	// this, a caller-submitted automation executed on a context descended from
	// server-side Go inherits the trust it was denied.
	inherited := auth.ContextWithInternalOrigin(context.Background())
	if got := auth.OriginFromContext(originForSource(inherited, false)); got.IsInternal() {
		t.Errorf("an UNTRUSTED automation on an internally-stamped parent context got origin %v, "+
			"want client -- origin must not be inheritable into untrusted source", got)
	}
}

// TestCompiledSourceAutomationIsUntrustedEndToEnd is the same chain from the
// other side: source compiled at runtime -- which is what RunBundleDryRun does
// with a caller-submitted bundle -- must NOT come out trusted.
func TestCompiledSourceAutomationIsUntrustedEndToEnd(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	loader := NewLoader(LoaderOptions{Logger: logger})

	src := `@trigger(event="node.created", concept="v1:identity:user")
automation zzAttackerSubmitted {
  step steal {
    query userByIdSystem(userId: "v1:identity:user:victim")
  }
}`
	a, err := loader.CompileSource(src, "attacker:inline")
	if err != nil {
		// NOT a t.Skipf. Skipping on compile failure turns a security test
		// into a silent no-op the first time the parser tightens -- and this
		// probe uses `query userByIdSystem(...)` inside a step, which is
		// exactly the kind of thing that gets tightened. If the probe stops
		// compiling, the fixture needs updating, not muting.
		t.Fatalf("CompileSource rejected the probe: %v\nThe trust assertion below needs a "+
			"compiling body. Update the probe source rather than skipping -- a skipped "+
			"security test asserts nothing and looks green.", err)
	}
	if a.Trusted {
		t.Fatal("an automation compiled from submitted SOURCE came out trusted -- " +
			"this is the round-2 bypass: a caller wraps a @serverOnly read in a " +
			"bundle and its steps run with internal origin")
	}

	reg := &originCapturingRegistry{}
	e := NewExecutor(ExecutorOptions{Logger: logger, StepRegistry: reg})
	defer e.Close()

	// Driven from an INTERNALLY-STAMPED parent context on purpose. The
	// untrusted path used to pass ctx through unchanged, so an untrusted body
	// executed on a context descended from server-side Go inherited the trust
	// it had been denied. With a plain Background() parent this test passes
	// either way and proves nothing about laundering.
	parent := auth.ContextWithInternalOrigin(context.Background())
	if _, err := e.ExecuteWithEvent(parent, a, "dryrun", nil); err != nil {
		t.Logf("ExecuteWithEvent returned %v (fine -- the origin is what is under test)", err)
	}
	if !reg.called {
		t.Fatal("no step was dispatched, so this test asserted nothing. The zero value of " +
			"auth.CallOrigin IS OriginClient, so the assertion below would have passed on " +
			"an execution that never reached dispatch")
	}
	if reg.got.IsInternal() {
		t.Fatalf("caller-supplied automation dispatched with origin %v, want client -- "+
			"an untrusted body inherited internal origin from its parent context", reg.got)
	}
}

// TestExecuteWithClientEventDoesNotGrantInternalOrigin is the memql#2888 rule,
// pinned at the executor rather than at the MCP runner.
//
// THAT PLACEMENT IS THE POINT. The seam the bug lives on is
// app/mcp_automation_runner.go, behind `//go:build mcp`, and CI never EXECUTES
// that file: the tagged test lane runs `-tags agent` and `-tags voice` only.
//
// Precisely: it is COMPILED and type-check-gated -- ci.yml's build-node-tags
// matrix includes mcp and runs `go vet -tags mcp ./...`, which type-checks test
// files -- but no `go test -tags mcp` ever runs. An earlier version of this
// comment said the file "is never compiled by CI", which was false and is
// corrected here rather than quietly dropped. A test that compiles but never
// runs asserts nothing, so the conclusion stands: this one runs untagged, on
// the function that actually computes the trust, so it holds for every caller
// of ExecuteWithClientEvent rather than for the one that exists today. The lane
// gap is tracked in memql#2903.
func TestExecuteWithClientEventDoesNotGrantInternalOrigin(t *testing.T) {
	reg := &originCapturingRegistry{}
	e := NewExecutor(ExecutorOptions{
		Logger:       slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		StepRegistry: reg,
	})

	// Trusted: loaded from the registered DSL tree, exactly as LoadByName
	// returns it on the MCP run_automation path.
	auto := &Automation{
		Name:    "zzTreeLoadedAutomation",
		Trusted: true,
		Steps:   []*Step{{ID: "s1", Name: "decide", Type: StepTypeFunction}},
	}
	// The half the caller controls.
	ev := &events.Event{Topic: "mcp.run.zzTreeLoadedAutomation", Payload: map[string]any{
		"node": map[string]any{"id": "v1:identity:user:victim"},
	}}

	if _, err := e.ExecuteWithClientEvent(context.Background(), auto, "mcp", ev); err != nil {
		t.Fatalf("ExecuteWithClientEvent: %v", err)
	}
	if !reg.called {
		t.Fatal("no step was dispatched, so this asserts nothing -- OriginClient is the zero " +
			"value, so a negative origin check passes when nothing ran")
	}
	if reg.got.IsInternal() {
		t.Fatalf("a caller-parameterised run of a TRUSTED automation dispatched with origin %v, "+
			"want client.\n\nThe client cannot choose which construct the authored body invokes, "+
			"but it chooses that construct's ARGUMENT -- and @serverOnly exempts a construct from "+
			"carrying any caller-scope filter, so origin is the entire authorization decision. "+
			"Live chain: run_automation(killSwitchSuspendsRunningPlans, {node:{id:<victim>}}) "+
			"reaches runningPlansForUser under an attacker-chosen userId and then transitions the "+
			"plans it returns via updatePlanStatus, which stamps id: args.planId with no owner "+
			"predicate (memql#2888).", reg.got)
	}
}

// TestExecuteWithEventStillGrantsInternalOriginForTrustedSource is the other
// direction, and it matters as much: #2800's first attempt shipped a BROKEN
// KILL SWITCH by getting this wrong. A server-originated trigger -- a graph
// event, a cron tick -- must still reach @serverOnly constructs, or
// killSwitchSuspendsRunningPlans silently suspends nothing when a user trips
// the computer-use kill switch.
func TestExecuteWithEventStillGrantsInternalOriginForTrustedSource(t *testing.T) {
	reg := &originCapturingRegistry{}
	e := NewExecutor(ExecutorOptions{
		Logger:       slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		StepRegistry: reg,
	})
	auto := &Automation{
		Name:    "zzTreeLoadedAutomation",
		Trusted: true,
		Steps:   []*Step{{ID: "s1", Name: "decide", Type: StepTypeFunction}},
	}
	ev := &events.Event{Topic: "graph.node.updated.v1:identity:user"}

	if _, err := e.ExecuteWithEvent(context.Background(), auto, "event", ev); err != nil {
		t.Fatalf("ExecuteWithEvent: %v", err)
	}
	if !reg.called {
		t.Fatal("no step was dispatched")
	}
	if !reg.got.IsInternal() {
		t.Fatalf("a graph-event-triggered run of a TRUSTED automation dispatched with origin %v, "+
			"want internal. Downgrading this path breaks the computer-use kill switch: its decide "+
			"step reads runningPlansForUser (@serverOnly) and would be refused as a client call, "+
			"suspending nothing (memql#2800).", reg.got)
	}
}

// TestExecuteWithClientEventOnUntrustedSourceIsStillClient closes the
// combination: caller-supplied source AND caller-supplied payload. Without it,
// the && could be written as either operand alone and one of the two tests
// above would still pass.
func TestExecuteWithClientEventOnUntrustedSourceIsStillClient(t *testing.T) {
	reg := &originCapturingRegistry{}
	e := NewExecutor(ExecutorOptions{
		Logger:       slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		StepRegistry: reg,
	})
	auto := &Automation{ // Trusted false: compiled from submitted source.
		Name:  "zzCallerSubmitted",
		Steps: []*Step{{ID: "s1", Name: "steal", Type: StepTypeFunction}},
	}
	if _, err := e.ExecuteWithClientEvent(context.Background(), auto, "mcp", &events.Event{Topic: "t"}); err != nil {
		t.Fatalf("ExecuteWithClientEvent: %v", err)
	}
	if !reg.called {
		t.Fatal("no step was dispatched")
	}
	if reg.got.IsInternal() {
		t.Fatalf("caller-supplied source AND caller-supplied payload dispatched with origin %v, "+
			"want client", reg.got)
	}
}

// TestResumeDoesNotLaunderClientOriginBackToInternal closes the bypass the
// #2902 review found, which is sharper than the original bug because the fix
// itself handed the attacker the token.
//
//  1. MCP run_automation with a chosen payload -> client origin. Correct.
//  2. The body reaches a @serverOnly construct -> refused -> the step errors.
//  3. ErrorStrategyStop saves a CHECKPOINT, and the checkpoint persists
//     TriggerContext.Event -- the attacker's payload.
//  4. POST /automations/resume replays it. resume.go read automation.Trusted
//     alone and restored SourceTrusted = true.
//  5. Every remaining step re-dispatches at INTERNAL origin with that payload,
//     and the resume loop runs to the end, so the WRITE step executes too.
//
// So the refusal produced in (2) is what mints the token used in (3). The
// checkpoint now carries CallerSuppliedPayload and resume applies the same rule
// as the live path.
func TestResumeDoesNotLaunderClientOriginBackToInternal(t *testing.T) {
	reg := &originCapturingRegistry{}
	e := NewExecutor(ExecutorOptions{
		Logger:       slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		StepRegistry: reg,
	})
	auto := &Automation{
		Name:    "zzTreeLoadedAutomation",
		Trusted: true, // tree-loaded, exactly as LoadByName returns it
		Steps:   []*Step{{ID: "s1", Name: "decide", Type: StepTypeFunction}},
	}

	// A checkpoint written by a run whose payload the CALLER supplied.
	cp := &ExecutionCheckpoint{
		ExecutionId:           "exec-1",
		AutomationName:        auto.Name,
		AutomationFingerprint: auto.DefinitionFingerprint(fingerprintEngine),
		StepIndex:             0,
		CallerSuppliedPayload: true,
		FailedAt:              &StepFailure{StepId: "s1", Error: "refused: @serverOnly"},
		StepResults:           map[string]*MinimalStepResult{},
		TriggerContext: &TriggerContext{
			Event: map[string]any{
				"topic":   "mcp.run.zzTreeLoadedAutomation",
				"payload": map[string]any{"node": map[string]any{"id": "v1:identity:user:victim"}},
			},
		},
	}

	if _, err := e.ResumeFrom(context.Background(), cp, auto, &ResumeOptions{FromStep: "s1"}); err != nil {
		t.Fatalf("ResumeFrom: %v", err)
	}
	if !reg.called {
		t.Fatal("no step was dispatched on resume, so this asserts nothing")
	}
	if reg.got.IsInternal() {
		t.Fatalf("RESUME re-granted INTERNAL origin (%v) to a run whose payload the caller "+
			"supplied.\n\nThat makes the whole origin downgrade bypassable, and the fix supplies "+
			"the bypass: the @serverOnly refusal is what causes the checkpoint to be written, and "+
			"the checkpoint carries the attacker's payload straight back in through "+
			"POST /automations/resume (memql#2888).", reg.got)
	}
}

// TestResumeStillGrantsInternalOriginToAServerOriginatedRun is the other
// direction. A checkpoint from a graph-event or cron run must resume at
// internal origin, or every legitimate retry silently loses access to the
// @serverOnly constructs its body reads.
func TestResumeStillGrantsInternalOriginToAServerOriginatedRun(t *testing.T) {
	reg := &originCapturingRegistry{}
	e := NewExecutor(ExecutorOptions{
		Logger:       slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		StepRegistry: reg,
	})
	auto := &Automation{
		Name:    "zzTreeLoadedAutomation",
		Trusted: true,
		Steps:   []*Step{{ID: "s1", Name: "decide", Type: StepTypeFunction}},
	}
	cp := &ExecutionCheckpoint{
		ExecutionId:           "exec-2",
		AutomationName:        auto.Name,
		AutomationFingerprint: auto.DefinitionFingerprint(fingerprintEngine),
		StepIndex:             0,
		CallerSuppliedPayload: false, // graph event / cron
		FailedAt:              &StepFailure{StepId: "s1", Error: "transient"},
		StepResults:           map[string]*MinimalStepResult{},
	}

	if _, err := e.ResumeFrom(context.Background(), cp, auto, &ResumeOptions{FromStep: "s1"}); err != nil {
		t.Fatalf("ResumeFrom: %v", err)
	}
	if !reg.called {
		t.Fatal("no step was dispatched on resume")
	}
	if !reg.got.IsInternal() {
		t.Fatalf("resume of a SERVER-originated run dispatched at %v, want internal -- every "+
			"legitimate retry would lose access to the @serverOnly constructs its body reads.",
			reg.got)
	}
}

// TestCheckpointCarriesTheCallerSuppliedFlag pins the SAVE half, through the
// real constructor AND the real concept schema.
//
// Two rounds of this test were green for the wrong reason:
//
//  1. it round-tripped a hand-built ExecutionCheckpoint through JSON, so both
//     mutations that drop the flag on the real save path left the suite green.
//     Fixed by extracting newCheckpointFromExecution.
//  2. it then round-tripped the CONSTRUCTOR's output through encoding/json --
//     which is still not the persistence layer. Checkpoints are stored as
//     v1:memql:checkpoint graph rows, and every concept schema is CLOSED
//     (noAdditional defaults true). The field was not declared, so
//     Concept.Create REJECTED the row.
//
// The second one was almost invisible, because `omitempty` drops the field
// when false: a server-originated checkpoint saved fine and only the
// SECURITY-CRITICAL one was refused, leaving a WARN and no checkpoint. The
// posture happened to be closed -- no checkpoint, nothing to resume -- but by
// schema rejection, not by the mechanism, and the mechanism never ran.
//
// So the schema declaration is asserted here directly. It is the input the
// persistence layer actually validates against, and a future edit that drops
// the field silently reopens the laundering path with everything else green.
func TestCheckpointCarriesTheCallerSuppliedFlag(t *testing.T) {
	auto := &Automation{Name: "zzAuto", Trusted: true,
		Steps: []*Step{{ID: "s1", Name: "decide", Type: StepTypeFunction}}}
	step := auto.Steps[0]
	ev := &events.Event{Topic: "mcp.run.zzAuto", Payload: map[string]any{"node": "victim"}}

	for _, tc := range []struct {
		name          string
		callerPayload bool
	}{
		{"caller-supplied payload", true},
		{"server-originated", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exec := NewExecution(auto.Name, "mcp")
			exec.CallerSuppliedPayload = tc.callerPayload

			cp := newCheckpointFromExecution(auto, exec, step, 0, "", errTestStepFailed, ev)
			if cp.CallerSuppliedPayload != tc.callerPayload {
				t.Fatalf("checkpoint.CallerSuppliedPayload = %v, want %v.\n\nResume reads this "+
					"field to decide whether internal origin may be restored, so a checkpoint "+
					"that drops it hands a caller-parameterised run full trust on replay "+
					"(memql#2888).", cp.CallerSuppliedPayload, tc.callerPayload)
			}
			// The attacker payload really is captured -- which is why the flag
			// has to be too.
			if cp.TriggerContext == nil || cp.TriggerContext.Event["payload"] == nil {
				t.Error("the checkpoint did not capture the trigger payload; if that ever stops " +
					"being true this laundering path closes for a different reason and these " +
					"tests should be revisited rather than deleted")
			}
		})
	}
}

// TestCheckpointConceptDeclaresTheFlag is the persistence half.
//
// v1:memql:checkpoint is a CLOSED schema (noAdditional defaults to true for
// every concept), so an undeclared field does not round-trip -- it makes
// Concept.Create reject the entire row. Combined with `omitempty`, that meant
// only the caller-supplied case was refused: the mechanism was dead in
// production and no test could see it, because encoding/json is not the
// validator.
func TestCheckpointConceptDeclaresTheFlag(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "dsl", "memql", "concepts.memql"))
	if err != nil {
		t.Fatalf("read the checkpoint concept: %v", err)
	}
	if !strings.Contains(string(src), "callerSuppliedPayload") {
		t.Fatal("v1:memql:checkpoint does not declare callerSuppliedPayload.\n\n" +
			"Concept schemas are closed, so the field is not merely dropped -- SaveCheckpoint's " +
			"Concept.Create REJECTS the whole row, and because the field is `omitempty` that " +
			"happens only for a CALLER-SUPPLIED run. Resume then has no checkpoint at all for " +
			"exactly the case the flag exists to mark, and the designed mechanism never runs " +
			"(memql#2888).")
	}
}

// TestExecutionRecordsTheCallerSuppliedFlag pins the other half: the flag has
// to be ON the execution before the checkpoint can copy it.
func TestExecutionRecordsTheCallerSuppliedFlag(t *testing.T) {
	reg := &originCapturingRegistry{}
	e := NewExecutor(ExecutorOptions{
		Logger:       slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		StepRegistry: reg,
	})
	auto := &Automation{Name: "zzAuto", Trusted: true,
		Steps: []*Step{{ID: "s1", Name: "decide", Type: StepTypeFunction}}}
	ev := &events.Event{Topic: "mcp.run.zzAuto"}

	client, err := e.ExecuteWithClientEvent(context.Background(), auto, "mcp", ev)
	if err != nil {
		t.Fatalf("ExecuteWithClientEvent: %v", err)
	}
	if !client.CallerSuppliedPayload {
		t.Error("a caller-parameterised run did not record CallerSuppliedPayload, so any " +
			"checkpoint it writes resumes at internal origin (memql#2888)")
	}

	server, err := e.ExecuteWithEvent(context.Background(), auto, "event", ev)
	if err != nil {
		t.Fatalf("ExecuteWithEvent: %v", err)
	}
	if server.CallerSuppliedPayload {
		t.Error("a server-originated run recorded CallerSuppliedPayload, which would downgrade " +
			"every legitimate resume")
	}
}

// errTestStepFailed is the stand-in failure a checkpoint is built from.
var errTestStepFailed = errors.New("refused: @serverOnly")
