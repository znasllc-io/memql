package procedure

import (
	"context"
	"testing"

	proc "github.com/znasllc-io/memql/component/procedure"
)

// failingDeriver fails the test if it is ever asked. It is the instrument for
// D6's headline claim -- "induction spends no model" -- which is only
// checkable if something would notice a call.
type failingDeriver struct{ t *testing.T }

func (d failingDeriver) ProposeDerivation(context.Context, proc.Hole, [][]proc.Action) (string, error) {
	d.t.Fatal("a model was asked to explain a hole the rules already explained: " +
		"induction is supposed to spend no model")
	return "", nil
}

// recordingDeriver answers a fixed expression and counts the calls.
type recordingDeriver struct {
	expr  string
	calls int
}

func (d *recordingDeriver) ProposeDerivation(context.Context, proc.Hole, [][]proc.Action) (string, error) {
	d.calls++
	return d.expr, nil
}

func holeSet(classes ...proc.HoleClass) []proc.Hole {
	out := make([]proc.Hole, 0, len(classes))
	for i, c := range classes {
		out = append(out, proc.Hole{
			Id: string(rune('a' + i)), StepIndex: 1, Path: []string{string(rune('a' + i))},
			Type: "string", Class: c,
		})
	}
	return out
}

// TestExplainUnexplained_TheModelIsAskedOnlyForAnUnexplainedHole is the
// NEGATIVE CONTROL for D6. Every hole the rules explained must reach no
// provider at all.
func TestExplainUnexplained_TheModelIsAskedOnlyForAnUnexplainedHole(t *testing.T) {
	i := New(nil, nil)
	i.SetDeriver(failingDeriver{t: t})
	got := i.explainUnexplained(context.Background(),
		holeSet(proc.HoleDataFlow, proc.HoleConstant, proc.HoleFree), nil)
	if len(got) != 3 {
		t.Fatalf("got %d holes, want 3", len(got))
	}
}

func TestExplainUnexplained_WithNoDeriverAnUnexplainedHoleBecomesFree(t *testing.T) {
	// The ordinary path: no deriver installed at all, so nothing reaches a
	// provider and the procedure is still learned with one more parameter.
	i := New(nil, nil)
	got := i.explainUnexplained(context.Background(), holeSet(proc.HoleUnexplained), nil)
	if got[0].Class != proc.HoleFree {
		t.Fatalf("class = %q, want %q", got[0].Class, proc.HoleFree)
	}
}

func TestExplainUnexplained_AtMostOneCallPerTemplate(t *testing.T) {
	// D6 allows ONE bounded model call. Two unexplained holes must not become
	// two calls.
	d := &recordingDeriver{expr: ""}
	i := New(nil, nil)
	i.SetDeriver(d)
	i.explainUnexplained(context.Background(),
		holeSet(proc.HoleUnexplained, proc.HoleUnexplained), nil)
	if d.calls != 1 {
		t.Fatalf("the deriver was called %d times; D6 allows one bounded call", d.calls)
	}
}

func TestExplainUnexplained_AProposalThatDoesNotHoldLeavesTheHoleFree(t *testing.T) {
	instances := [][]proc.Action{
		{
			{Tool: "exec", Args: proc.Obj(map[string]*proc.Node{"c": proc.Lit("x")}), Seq: 0,
				ResultValue: map[string]any{"path": "/srv/alpha.txt"}},
			{Tool: "fs_write", Args: proc.Obj(map[string]*proc.Node{"a": proc.Lit("alpha.txt")}), Seq: 1},
		},
		{
			{Tool: "exec", Args: proc.Obj(map[string]*proc.Node{"c": proc.Lit("x")}), Seq: 0,
				ResultValue: map[string]any{"path": "/srv/beta.txt"}},
			{Tool: "fs_write", Args: proc.Obj(map[string]*proc.Node{"a": proc.Lit("unrelated")}), Seq: 1},
		},
	}
	d := &recordingDeriver{expr: `basename(ref(0, "path"))`}
	i := New(nil, nil)
	i.SetDeriver(d)
	got := i.explainUnexplained(context.Background(), holeSet(proc.HoleUnexplained), instances)
	if got[0].Class != proc.HoleFree {
		t.Fatalf("class = %q, want %q -- the proposal explains one instance of two", got[0].Class, proc.HoleFree)
	}
	if got[0].Derivation != "" {
		t.Fatalf("a rejected derivation must not be recorded: %q", got[0].Derivation)
	}
}

func TestExplainUnexplained_AProposalThatHoldsIsKeptAsDataFlow(t *testing.T) {
	mk := func(n string) []proc.Action {
		return []proc.Action{
			{Tool: "exec", Args: proc.Obj(map[string]*proc.Node{"c": proc.Lit("x")}), Seq: 0,
				ResultValue: map[string]any{"path": "/srv/" + n + ".txt"}},
			{Tool: "fs_write", Args: proc.Obj(map[string]*proc.Node{"a": proc.Lit(n + ".txt")}), Seq: 1},
		}
	}
	d := &recordingDeriver{expr: `basename(ref(0, "path"))`}
	i := New(nil, nil)
	i.SetDeriver(d)
	got := i.explainUnexplained(context.Background(),
		holeSet(proc.HoleUnexplained), [][]proc.Action{mk("alpha"), mk("beta")})
	if got[0].Class != proc.HoleDataFlow {
		t.Fatalf("class = %q, want %q", got[0].Class, proc.HoleDataFlow)
	}
	if got[0].Derivation != `basename(ref(0, "path"))` {
		t.Fatalf("the kept derivation must be recorded; got %q", got[0].Derivation)
	}
}

func TestExplainUnexplained_AProposalOutsideTheGrammarIsRejected(t *testing.T) {
	d := &recordingDeriver{expr: `exec("rm -rf /")`}
	i := New(nil, nil)
	i.SetDeriver(d)
	got := i.explainUnexplained(context.Background(), holeSet(proc.HoleUnexplained), nil)
	if got[0].Class != proc.HoleFree {
		t.Fatalf("class = %q, want %q -- the grammar is the safety property", got[0].Class, proc.HoleFree)
	}
}

func TestLevelArg_DefaultsToActionsAndAcceptsTwo(t *testing.T) {
	if got := levelArg(nil); got != LevelAction {
		t.Fatalf("default level = %v, want %v", got, LevelAction)
	}
	if got := levelArg(map[string]any{"level": float64(2)}); got != LevelAutomation {
		t.Fatalf("level 2 = %v, want %v", got, LevelAutomation)
	}
	if got := levelArg(map[string]any{"level": float64(9)}); got != LevelAction {
		t.Fatalf("an unknown level must fall back to actions rather than mining nothing; got %v", got)
	}
}

func TestLevelAccepts_SeparatesTheTwoCorpusLevels(t *testing.T) {
	if !levelAccepts(LevelAction, "exec") || levelAccepts(LevelAction, "automation") {
		t.Fatal("level 1 is the app-session actions")
	}
	if !levelAccepts(LevelAutomation, "automation") || levelAccepts(LevelAutomation, "exec") {
		t.Fatal("level 2 is the automation invocations")
	}
}

func TestMarkConsumed_SetsConsumedWhenALaterStepReferencesTheResult(t *testing.T) {
	steps := []proc.Step{
		{StepType: "fs_read", Seq: 0, ResultValue: map[string]any{"body": "token-123"}},
		{StepType: "exec", Seq: 1, Input: map[string]any{"command": "deploy --key token-123"}},
		{StepType: "fs_read", Seq: 2, ResultValue: map[string]any{"body": "never-used"}},
	}
	markConsumed(steps)
	if !steps[0].Consumed {
		t.Error("a read whose value a later argument carries is consumed")
	}
	if steps[2].Consumed {
		t.Error("a read nothing referenced is not consumed")
	}
}

func TestMarkConsumed_ADigestOnlyResultIsStillRecognised(t *testing.T) {
	// A large result degrades to a digest. A step whose result was too big to
	// keep must still be recognised as consumed when a later argument carries
	// that digest, or every big read looks like noise.
	steps := []proc.Step{
		{StepType: "fetch", Seq: 0, ResultDigest: "sha256:abc"},
		{StepType: "exec", Seq: 1, Input: map[string]any{"command": "verify sha256:abc"}},
	}
	markConsumed(steps)
	if !steps[0].Consumed {
		t.Error("a digest carried into a later argument is a reference")
	}
}
