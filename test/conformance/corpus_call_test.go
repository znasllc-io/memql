package conformance

// corpus_call_test.go -- the corpus runs a logic (epic memql#5370, task
// memql#5374; the `call` field agreed with the corpus runner's owner, epic
// memql#5356).
//
// An evaluate case with `call` names a logic its file declares. The file,
// with its directory's fixture beside it, has already loaded like a load case
// (corpusLoad), and a refusal there is the case's failure. Here the logic's
// body compiles the way the function loader compiles it (compiler.CompileBody
// over the parsed statements) and runs through the automations LogicRunner --
// the runner a node calls a logic through -- with the case's args. What it
// returns is compared with expect, as an expression's value is.
//
// DB-FREE BY CONSTRUCTION. A statement cell is literals, args and names. The
// step registry is the production one with every step that could reach the
// outside refused, so a `for` or a parallel runs as it does on a node. A
// construct call runs only two ways: to another logic the case file or its
// fixture declares, which runs the same way, or answered by the case's
// `calls` ("<kind> <name>": the value it returns), as an expression case's
// calls are. A case that needs the database belongs in scenarios/.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/steps"
	"github.com/znasllc-io/memql/component/language/compiler"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// corpusCall runs every call case whose file loaded.
func corpusCall(t *testing.T, runs []*corpusRun) {
	t.Helper()
	for _, r := range runs {
		if r.parseErr != nil || len(r.loadDiags) > 0 {
			continue // judged as the load failure it is
		}
		logics, err := corpusLogics(r.line.Edition, r.fixture, r.src)
		if err != nil {
			r.gotErr = err
			continue
		}
		body, ok := logics[r.c.Call]
		if !ok {
			r.gotErr = fmt.Errorf("neither the file nor its fixture declares a logic %q", r.c.Call)
			continue
		}
		calls := &corpusLogicCalls{logics: logics, answers: r.c.Calls}
		reg := corpusStepRegistry(calls)
		calls.runner = automations.NewLogicRunner(nil, reg, nil).WithoutJournal()
		r.got, r.gotErr = calls.runner.RunLogicBody(context.Background(), r.c.Call, body, r.c.Args)
	}
}

// corpusStepRegistry is the production registry with every step that could
// reach the outside refused: the statements that hold lists -- a `for`, a
// parallel and its branches -- run as they do on a node, and a construct call
// reaches calls, which runs a declared logic and refuses anything else.
func corpusStepRegistry(calls *corpusLogicCalls) *steps.Registry {
	reg := steps.NewRegistry()
	reg.Register(automations.StepTypeFunction, calls)
	for _, t := range []automations.StepType{
		automations.StepTypeEvent, automations.StepTypeAction, automations.StepTypeAutomation,
	} {
		reg.Register(t, corpusRefused{})
	}
	return reg
}

// corpusRefused refuses a step a corpus cell cannot run.
type corpusRefused struct{}

func (corpusRefused) Execute(_ context.Context, step *automations.Step, _ *automations.StepContext) (*automations.StepResult, error) {
	err := fmt.Errorf("a corpus cell runs no %s step (%q)", step.Type, step.ID)
	now := time.Now()
	return &automations.StepResult{StepId: step.ID, Status: "failed", Error: err.Error(), StartedAt: now, CompletedAt: now}, err
}

// corpusLogics compiles every logic the fixture and the case file declare,
// by name.
func corpusLogics(edition string, sources ...string) (map[string][]map[string]any, error) {
	fe, err := langparser.FrontEndFor(edition)
	if err != nil {
		return nil, err
	}
	out := map[string][]map[string]any{}
	for _, src := range sources {
		if src == "" {
			continue
		}
		prepared, err := fe.Prepare(src)
		if err != nil {
			return nil, err
		}
		file, err := compiler.ParseFileSource(prepared)
		if err != nil {
			return nil, err
		}
		for _, def := range file.Definitions {
			fn, ok := def.(*langparser.FunctionDef)
			if !ok || fn.Type != langparser.FunctionTypeLogic {
				continue
			}
			auto, ok := fn.Body.(*langparser.AutomationDef)
			if !ok || auto.Body == nil {
				return nil, fmt.Errorf("logic %s is not a statement body", fn.Name)
			}
			var args []string
			if fn.ArgsSchema != nil {
				for _, f := range fn.ArgsSchema.Fields {
					args = append(args, f.Name)
				}
			}
			body, problems := compiler.CompileBody("logic", fn.Name, args, auto.Body)
			if len(problems) > 0 {
				return nil, fmt.Errorf("logic %s: %w", fn.Name, compiler.BodyProblems(problems))
			}
			out[fn.Name] = body
		}
	}
	return out, nil
}

// corpusLogicCalls runs a corpus logic's construct calls: a call to a logic
// the case declares runs it, one the case's `calls` answers returns that
// answer, and any other is refused.
type corpusLogicCalls struct {
	logics  map[string][]map[string]any
	answers map[string]json.RawMessage
	runner  *automations.LogicRunner
}

func (g *corpusLogicCalls) Execute(ctx context.Context, step *automations.Step, stepCtx *automations.StepContext) (*automations.StepResult, error) {
	now := time.Now()
	res := &automations.StepResult{StepId: step.ID, StartedAt: now}
	if step.Function != nil && step.Function.Kind == "logic" {
		if body, ok := g.logics[step.Function.Name]; ok {
			args, err := stepCtx.Evaluator.ResolveV1Map(ctx, step.Function.Args)
			if err == nil {
				res.Result, err = g.runner.RunLogicBody(ctx, step.Function.Name, body, args)
			}
			res.CompletedAt = time.Now()
			if err != nil {
				res.Status, res.Error = "failed", err.Error()
				return res, err
			}
			res.Status = "success"
			return res, nil
		}
	}
	if step.Function != nil {
		if answer, ok := g.answers[step.Function.Kind+" "+step.Function.Name]; ok {
			var v any
			if err := json.Unmarshal(answer, &v); err != nil {
				return nil, fmt.Errorf("calls[%q]: %w", step.Function.Kind+" "+step.Function.Name, err)
			}
			res.Status, res.Result, res.CompletedAt = "success", v, time.Now()
			return res, nil
		}
	}
	err := fmt.Errorf("a corpus cell's construct call runs another logic the case declares or is answered by its calls; step %q is neither", step.ID)
	res.Status, res.Error, res.CompletedAt = "failed", err.Error(), time.Now()
	return res, err
}
