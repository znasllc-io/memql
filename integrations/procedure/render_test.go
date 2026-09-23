package procedure

import (
	"strings"
	"testing"

	// The REAL Gate 1 automation compiler: component/automations registers
	// it from init(), production binaries always link it, and this test
	// binary would otherwise report the automation kind as SKIPPED -- a
	// compile assertion that passes having compiled nothing (memql#1366).
	_ "github.com/znasllc-io/memql/component/automations"

	"github.com/znasllc-io/memql/component/memql"
	proc "github.com/znasllc-io/memql/component/procedure"
	"github.com/znasllc-io/memql/integrations/planner"
)

// render_test.go -- a learned procedure written as MemQL (gap G8).

// twoStepTemplate is the fixture corpus's template: an exec whose file name is
// a free parameter, and a fixed write.
func twoStepTemplate() proc.Template {
	a := proc.Canonicalize([]proc.Step{
		{StepType: "exec", Input: map[string]any{"command": "mkdir -p out && echo hello > a.txt"}},
		{StepType: "fs_write", Input: map[string]any{"file_path": "/w/project/out/report.txt", "content": "hello\n"}},
	})
	b := proc.Canonicalize([]proc.Step{
		{StepType: "exec", Input: map[string]any{"command": "mkdir -p out && echo hello > b.txt"}},
		{StepType: "fs_write", Input: map[string]any{"file_path": "/w/project/out/report.txt", "content": "hello\n"}},
	})
	t := proc.Generalize([][]proc.Action{a, b})
	t.Holes = proc.Classify(t, [][]proc.Action{a, b})
	return t
}

const goldenSource = `// Learned procedure (epic memql#5402). Generalized by component/procedure;
// no model was spent on the induction.
// Recorded from app claude-code, model claude-sonnet-4-6, session s1, s2.
// From runs: r1, r2.
// Accepted on 2 use(s), net compression 9.
/// Procedure learned from the recorded runs of: Write the "greeting" file
automation learnedProcedure_abc_l1 {
  args {
    command_70 string
  }

  call0 := builtin procedureStep(step: 0, tool: "exec", args: {command: ["mkdir", "-p", "out", "&&", "echo", "hello", ">", args.command_70]})
  call1 := builtin procedureStep(step: 1, tool: "fs_write", args: {content: "hello\n", file_path: ["w", "project", "out", "report.txt"]})
}
`

// TestRenderProcedureSourceWritesEveryStepAsAProcedureStep is the golden: the
// provenance stamp, the description as a doc comment, the free parameter as
// the args block, and every step one procedureStep statement with its holes
// spelled -- where every step used to be a comment line.
func TestRenderProcedureSourceWritesEveryStepAsAProcedureStep(t *testing.T) {
	tmpl := twoStepTemplate()
	src, written := renderProcedureSource("learnedProcedure_abc_l1", `Write the "greeting" file`, tmpl, freeHoles(tmpl),
		planner.TemplateProvenance{App: "claude-code", Model: "claude-sonnet-4-6", SessionId: "s1, s2",
			RunIds: []string{"r1", "r2"}, Uses: 2, Net: 9})
	if !written {
		t.Fatal("every step has a spelling, so every step must be written")
	}
	if src != goldenSource {
		t.Fatalf("source drifted from the golden.\n--- got ---\n%s\n--- want ---\n%s", src, goldenSource)
	}
}

// TestAStepWhoseArgumentsHaveNoSpellingIsACommentLine: a map key that is not a
// MemQL name has no spelling in a statement, so that step stays a comment and
// the source reports it -- which is what keeps the procedure from being
// recorded as re-runnable.
func TestAStepWhoseArgumentsHaveNoSpellingIsACommentLine(t *testing.T) {
	// An exec, not a read: an unconsumed pure read is dropped as noise by
	// canonicalization, and would leave no step to render at all.
	one := func() []proc.Action {
		return proc.Canonicalize([]proc.Step{{StepType: "exec", Input: map[string]any{
			"command": "make", "env": map[string]any{"MAKE-FLAGS": "-j4"},
		}}})
	}
	tmpl := proc.Generalize([][]proc.Action{one(), one()})
	if len(tmpl.Steps) != 1 {
		t.Fatalf("the fixture template has %d steps, want 1", len(tmpl.Steps))
	}
	src, written := renderProcedureSource("p", "", tmpl, nil, planner.TemplateProvenance{})
	if written {
		t.Fatalf("a key that is not a name cannot be written:\n%s", src)
	}
	if !strings.Contains(src, "  // call0: step 0, tool exec -- an argument has no MemQL spelling") {
		t.Fatalf("the unwritten step must be a comment naming it:\n%s", src)
	}
	if strings.Contains(src, "procedureStep(") {
		t.Fatalf("an unwritten step was written anyway:\n%s", src)
	}
}

// TestTheSourceOutsideItsCommentsIsTheBehaviourAlone: the source is hashed
// without its comment lines, so whatever varies with the CORPUS rather than
// with the procedure -- the run count, the sessions, and the goal statement,
// read off whichever recording is oldest -- must live in a comment. Outside
// one, a reworded goal or one more agreeing recording would be a new version.
func TestTheSourceOutsideItsCommentsIsTheBehaviourAlone(t *testing.T) {
	tmpl := twoStepTemplate()
	two, _ := renderProcedureSource("p", "Write the greeting file", tmpl, freeHoles(tmpl),
		planner.TemplateProvenance{RunIds: []string{"a", "b"}, SessionId: "s1"})
	three, _ := renderProcedureSource("p", "write the greeting file!", tmpl, freeHoles(tmpl),
		planner.TemplateProvenance{RunIds: []string{"a", "b", "c"}, SessionId: "s1, s2"})
	if sourceWithoutComments(two) != sourceWithoutComments(three) {
		t.Fatalf("the source outside its comments changed with the corpus:\n%s\n---\n%s", two, three)
	}
	if !strings.Contains(two, "/// Procedure learned from the recorded runs of: Write the greeting file\n") {
		t.Fatalf("the goal statement must still be READ in the source, as its doc comment:\n%s", two)
	}
}

// TestTheRenderedProcedureCompilesThroughTheRealGate1: the source must pass
// the compile gate the lift runs -- the REAL sandbox, bound against a
// DSL-loaded engine so `builtin procedureStep` resolves to the declared
// builtin -- or no procedure is ever re-runnable. The automation must have
// been COMPILED, not skipped: a skipped construct does not fail the bundle.
func TestTheRenderedProcedureCompilesThroughTheRealGate1(t *testing.T) {
	tmpl := twoStepTemplate()
	src, written := renderProcedureSource("learnedProcedure_abc_l1", "Write the greeting file", tmpl, freeHoles(tmpl),
		planner.TemplateProvenance{RunIds: []string{"r1", "r2"}, Uses: 2})
	if !written {
		t.Fatal("the fixture must render every step")
	}
	report := memql.SandboxCompileBundleWithEngine(
		[]memql.SandboxConstruct{{Kind: "automation", Name: "learnedProcedure_abc_l1", Source: src}}, realDSLEngine(t))
	if !compiledAutomation(report, "learnedProcedure_abc_l1") || !report.OK {
		t.Fatalf("the rendered procedure does not pass Gate 1: %+v\n%s", report.Diagnostics, src)
	}

	// The control: the same source reading a parameter it never declared
	// fails the same gate, so a pass above is the gate agreeing, not the gate
	// absent. (Gate 1 checks the automation's own names and syntax; it does
	// NOT resolve a statement's callee -- measured: a call to an undeclared
	// builtin compiles -- which is why the builtin's contract is held to the
	// loaded registry below instead.)
	broken := strings.Replace(src, "args.command_70]", "args.notDeclared]", 1)
	bad := memql.SandboxCompileBundleWithEngine(
		[]memql.SandboxConstruct{{Kind: "automation", Name: "learnedProcedure_abc_l1", Source: broken}}, realDSLEngine(t))
	if bad.OK {
		t.Fatalf("Gate 1 accepted an undeclared parameter, so it is not checking the source:\n%s", broken)
	}
}

// TestEveryStepCallsTheDeclaredProcedureStepBuiltin: Gate 1 does not resolve
// a statement's callee, so the contract every rendered step relies on is held
// here, against the LOADED registry: procedureStep is a builtin of the
// procedure integration, and it declares exactly the three arguments the
// renderer names -- step and tool required, args optional.
func TestEveryStepCallsTheDeclaredProcedureStepBuiltin(t *testing.T) {
	fn, err := realDSLEngine(t).Functions().Get("procedureStep")
	if err != nil || fn == nil {
		t.Fatalf("procedureStep is not in the loaded registry: %v", err)
	}
	if fn.Executor != "integration.procedure.step" {
		t.Fatalf("procedureStep executes %q, want integration.procedure.step", fn.Executor)
	}
	// A builtin's body IS its schema, and the loader keeps it as the
	// builtin's argument contract: the declared properties, and which of them
	// are required.
	if fn.BuiltinArgs == nil || len(fn.BuiltinArgs.Properties) == 0 {
		t.Fatalf("procedureStep declares no fields: %+v", fn.BuiltinArgs)
	}
	got := map[string]bool{}
	for name := range fn.BuiltinArgs.Properties {
		got[name] = false
	}
	for _, name := range fn.BuiltinArgs.Required {
		got[name] = true
	}
	want := map[string]bool{"step": true, "tool": true, "args": false}
	if len(got) != len(want) {
		t.Fatalf("procedureStep declares %v, want exactly step, tool and args", got)
	}
	for name, required := range want {
		if r, ok := got[name]; !ok || r != required {
			t.Fatalf("procedureStep field %s: declared %v (required %v), want required %v", name, ok, r, required)
		}
	}
	tmpl := twoStepTemplate()
	src, _ := renderProcedureSource("p", "", tmpl, freeHoles(tmpl), planner.TemplateProvenance{})
	if strings.Count(src, "builtin procedureStep(step: ") != len(tmpl.Steps) {
		t.Fatalf("every step must call procedureStep with its step index:\n%s", src)
	}
}
