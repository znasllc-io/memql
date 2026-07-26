package automations

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
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
	r.got = auth.OriginFromContext(ctx)
	r.called = true
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
