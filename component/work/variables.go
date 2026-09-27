package work

// variables.go -- the variables a replay reads and a goal never supplied
// (epic memql#5408).
//
// When a learned procedure serves a goal, compile lays its construct id over
// the goal's input so the one embedded replay template knows which procedure
// to run. When the replay diverges, the app takes the goal back in a session
// opened from that same run, and the session's recording inherits the run's
// variables AS THE GOAL'S INPUT: the lift maps a procedure's parameters onto
// them and lists their keys as the goal's inputs. A recording that kept a
// replay-only key would teach the next lift an input nobody gave.
//
// It lives HERE, in the leaf, because both writers of a recording need it --
// integrations/work's session writer and integrations/agent/worker's app
// session delegate -- and the second cannot import the first without a cycle
// under the agent build tag.

// ProcedureConstructVariable is the variable compile adds to the run of a goal
// a learned procedure serves (integrations/planner, work_compile.go): the one
// argument of replayLearnedProcedure, naming the procedure.
const ProcedureConstructVariable = "procedureConstructId"

// replayOnlyVariables are every key compile lays over a goal's input when a
// learned procedure serves it. A key compile adds there belongs here too.
var replayOnlyVariables = []string{ProcedureConstructVariable}

// GoalInput is a run's variables less the replay-only ones. It copies, so the
// row read is left as it was, and answers nil when nothing is left -- an empty
// object is not an input.
func GoalInput(variables map[string]any) map[string]any {
	out := make(map[string]any, len(variables))
	for k, v := range variables {
		out[k] = v
	}
	for _, k := range replayOnlyVariables {
		delete(out, k)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
