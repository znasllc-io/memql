package proving

import (
	"sort"
	"strings"
	"testing"
	"time"

	proc "github.com/znasllc-io/memql/component/procedure"
	"github.com/znasllc-io/memql/component/proving/scenario"
)

// TestEveryCommittedLifecycleFixtureIsLearnable holds the committed lifecycle
// fixtures to the one property every figure they publish rests on, with no
// database: the learner, given the fixture app's first two recordings, lifts
// ONE procedure covering every action, and every free parameter of it is bound
// by a goal input -- so a trusted replay can bind it from the next goal and
// never refuses to start for want of a value.
//
// It is not decorative. The first fixture tried here -- two commands of the
// form `reconcile.sh {{account}}` -- lifts NOTHING: its body is so small that
// the two free parameters cost exactly what the abstraction saves, and D14's
// floor refuses a net gain of zero. In the database lane that reads as a
// lifecycle whose procedure never appears, several goals away from the cause.
//
// The pipeline below is integrations/procedure's runPipeline, in its order,
// over the actions the fixture app records (a command per action, arguments
// whole). It is a mirror kept for this purpose only: the lane's real learner is
// the ground truth, and this is what makes a fixture that could never be
// learned fail here instead of there.
func TestEveryCommittedLifecycleFixtureIsLearnable(t *testing.T) {
	for id, s := range lifecycles(t) {
		t.Run(id, func(t *testing.T) {
			bindings := recordingBindings(t, s)
			var (
				corpus [][]proc.Action
				inputs []map[string]any
				fps    []map[string]any
			)
			pw := newProcedureWorld(s, newWorld(s))
			for r, vars := range bindings {
				steps := make([]proc.Step, 0, len(s.Steps))
				for i, st := range s.Steps {
					steps = append(steps, proc.Step{
						RunId: "run" + string(rune('a'+r)), Key: st.Key, Seq: i + 1, StepType: st.Type,
						Call:  proc.Call{Construct: "app", Name: st.Type},
						Input: map[string]any{"command": scenario.Render(st.Target, vars)},
					})
				}
				acts := proc.Canonicalize(steps)
				proc.SortActions(acts)
				corpus = append(corpus, acts)
				in := map[string]any{}
				for k, v := range vars {
					in[k] = v
				}
				inputs = append(inputs, in)
				fps = append(fps, pw.fingerprint("/workspace/"+id, time.Unix(0, 0)))
			}

			win, instances := minePipeline(corpus)
			if win == nil {
				t.Fatalf("the learner lifts NOTHING from %d recordings of this fixture; its actions do not clear D14's compression floor", len(corpus))
			}
			if len(win.Steps) != len(s.Steps) {
				t.Fatalf("the procedure covers %d of the fixture's %d actions; a replay of it would not serve the goal", len(win.Steps), len(s.Steps))
			}
			inputMap := proc.LearnInputMap(*win, instances, inputs[:len(instances)])
			for _, h := range win.Holes {
				if h.Class != proc.HoleFree {
					continue
				}
				if _, ok := inputMap[h.Id]; !ok {
					t.Errorf("free parameter %s is bound by no goal input; a trusted replay would refuse to start for want of it", h.Id)
				}
			}
			if len(inputMap) == 0 {
				t.Error("the procedure has no free parameter mapped to a goal input, so the lifecycle's distinct-binding rule would have nothing to count")
			}

			// The preconditions the recordings teach are the ones the fake
			// machine must answer at replay: its platform, each script at its
			// version, an empty workspace.
			pre := proc.LearnPreconditions(fps, proc.UsedTools(*win))
			if pre.Platform["os"] != provingMachineOS || pre.Platform["arch"] != provingMachineArch {
				t.Errorf("learned platform %v, want %s/%s", pre.Platform, provingMachineOS, provingMachineArch)
			}
			var tools []string
			for name, version := range pre.Tools {
				if version != provingToolVersion {
					t.Errorf("learned %s at %q, want %q", name, version, provingToolVersion)
				}
				tools = append(tools, name)
			}
			sort.Strings(tools)
			var scripts []string
			for _, st := range s.Steps {
				scripts = append(scripts, st.Script())
			}
			sort.Strings(scripts)
			if strings.Join(tools, ",") != strings.Join(scripts, ",") {
				t.Errorf("learned tools %v, want every script the fixture runs %v", tools, scripts)
			}
			if pre.EmptyWorkspace == nil || !*pre.EmptyWorkspace {
				t.Error("the recordings did not teach an empty workspace, though every session starts in one")
			}
		})
	}
}

// recordingBindings are the inputs of the goals the app records before any
// procedure exists: the first two goals of the scenario's own statement.
func recordingBindings(t *testing.T, s scenario.Scenario) []map[string]string {
	t.Helper()
	var out []map[string]string
	for _, g := range s.Procedure.Goals {
		if g.Serves() && g.Goal == "" && len(out) < 2 {
			out = append(out, g.Variables)
		}
	}
	if len(out) < 2 {
		t.Fatalf("%s records fewer than two goals of its own statement before anything else; D14's floor is two uses", s.Id)
	}
	return out
}

// minePipeline is integrations/procedure's runPipeline over values: one symbol
// table across the corpus, mine, generalize each pattern over its occurrences,
// classify its holes (an unexplained one demoted to free, as it is when no
// deriver is installed), then select by compression. It returns the first
// accepted template and the instances it was generalized from.
func minePipeline(corpus [][]proc.Action) (*proc.Template, [][]proc.Action) {
	params := proc.DefaultParams()
	var flat []proc.Action
	offsets := make([]int, len(corpus))
	for i, run := range corpus {
		offsets[i] = len(flat)
		flat = append(flat, run...)
	}
	all := proc.SymbolSequence(flat, proc.Symbolize(flat, params))
	sequences := make([][]string, len(corpus))
	for r := range corpus {
		sequences[r] = all[offsets[r] : offsets[r]+len(corpus[r])]
	}
	instancesOf := func(occs []proc.Occurrence) [][]proc.Action {
		var out [][]proc.Action
		for _, occ := range occs {
			var acts []proc.Action
			for _, pos := range occ.Positions {
				acts = append(acts, corpus[occ.Sequence][pos])
			}
			out = append(out, acts)
		}
		return out
	}
	var candidates []proc.Candidate
	for _, p := range proc.Mine(sequences, params) {
		instances := instancesOf(p.Occurrences)
		if len(instances) < 2 {
			continue
		}
		tmpl := proc.Generalize(instances)
		tmpl.Holes = proc.Classify(tmpl, instances)
		for i := range tmpl.Holes {
			if tmpl.Holes[i].Class == proc.HoleUnexplained {
				tmpl.Holes[i].Class = proc.HoleFree
			}
		}
		candidates = append(candidates, proc.Candidate{Template: tmpl, Occurrences: p.Occurrences})
	}
	accepted := proc.Select(candidates, corpus, params)
	if len(accepted) == 0 {
		return nil, nil
	}
	win := accepted[0].Candidate.Template
	return &win, instancesOf(accepted[0].Candidate.Occurrences)
}

// TestTheLearnabilityMirrorCanSayNo is the mirror's own negative control: the
// fixture shape recorded above as lifting nothing must lift nothing here too,
// or the test above could never have failed.
func TestTheLearnabilityMirrorCanSayNo(t *testing.T) {
	var corpus [][]proc.Action
	for _, account := range []string{"acme", "globex"} {
		acts := proc.Canonicalize([]proc.Step{
			{Key: "a", Seq: 1, StepType: "exec", Input: map[string]any{"command": "reconcile.sh " + account}},
			{Key: "b", Seq: 2, StepType: "exec", Input: map[string]any{"command": "notify.sh " + account}},
		})
		corpus = append(corpus, acts)
	}
	if win, _ := minePipeline(corpus); win != nil {
		t.Fatalf("the mirror lifted a procedure from a corpus D14's floor refuses; the learnability test above could never fail")
	}
}
