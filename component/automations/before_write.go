package automations

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	logSubject "github.com/znasllc-io/memql/core/logger"
)

const StepTypeFieldWrite StepType = "fieldWrite"

type FieldWriteConfig struct {
	Field string `json:"field"`
	Value any    `json:"value"`
}
type BeforeWriteConfig struct {
	On      string `json:"on"`
	Concept string `json:"concept"`
	Filter  string `json:"filter,omitempty"`
	filter  *ast.LambdaExpr
}

var beforeWritesTotal = promauto.NewCounterVec(prometheus.CounterOpts{Name: "memql_automation_before_writes_total", Help: "Before-write automation bodies applied to incoming rows."}, []string{"automation"})

func (l *Loader) prepareBeforeWrite(a *Automation) error {
	b := a.BeforeWrite
	if b == nil {
		for _, s := range a.Steps {
			if s.Type == StepTypeFieldWrite {
				return fmt.Errorf("automation %q: field write requires before trigger [before_write_outside]", a.Name)
			}
		}
		return nil
	}
	if (b.On != "create" && b.On != "update" && b.On != "write") || b.Concept == "" || a.Trigger != nil || a.Schedule != "" || a.Template {
		return fmt.Errorf("automation %q: invalid before trigger [before_write_trigger]", a.Name)
	}
	var properties map[string]json.RawMessage
	if l.registry != nil && len(l.registry.List()) > 0 {
		c, err := l.registry.Get(b.Concept)
		if err != nil {
			return fmt.Errorf("automation %q: unknown before-write concept %q [before_write_field]", a.Name, b.Concept)
		}
		schema, err := c.DefinitionSchema()
		if err != nil {
			return err
		}
		var obj struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(schema, &obj); err != nil {
			return err
		}
		properties = obj.Properties
	}
	reg := newFunctionSource(l.functions, l.registry).Registry()
	var pure func(string, map[string]bool) bool
	pure = func(name string, seen map[string]bool) bool {
		if seen[name] {
			return true
		}
		seen[name] = true
		t, ok := reg[name]
		if !ok {
			return false
		}
		if t.ConstructKind != work.ConstructQuery && t.ConstructKind != work.ConstructLogic {
			return false
		}
		for _, call := range t.Calls {
			if !pure(call, seen) {
				return false
			}
		}
		return true
	}
	for _, s := range a.Steps {
		switch s.Type {
		case StepTypeFieldWrite:
			if s.FieldWrite == nil {
				return fmt.Errorf("missing field write config [before_write_field]")
			}
			f := s.FieldWrite.Field
			if f == "" || strings.Contains(f, ".") || rowIntrinsics[f] || f == "firstVersion" || f == "updatedAt" || properties != nil && properties[f] == nil {
				return fmt.Errorf("automation %q: %q is not a declared payload field [before_write_field]", a.Name, f)
			}
		case StepTypeFunction:
			if s.Function == nil || (s.Function.Kind != "logic" && s.Function.Kind != "query") || !pure(s.Function.Name, map[string]bool{}) {
				return fmt.Errorf("automation %q: call %v may write [before_write_writes]", a.Name, s.Function)
			}
		case StepTypeExpression, StepTypeReturn:
		default:
			return fmt.Errorf("automation %q: %s is not permitted before write [before_write_writes]", a.Name, s.Type)
		}
	}
	if b.Filter != "" {
		t := &TriggerConfig{Event: "graph.node.created." + b.Concept, Filter: b.Filter}
		p := &exprPreparer{automation: a.Name, concepts: l.registry}
		if err := p.triggerFilter(t); err != nil {
			return err
		}
		b.filter = t.FilterLambda
	}
	return nil
}

// InstallBeforeWriteHooks registers the compiled bodies on the writing engine.
// The normal sequence runner executes them with no work journal or lifecycle
// events. Adjustment attribution goes to the log store; no second row exists.
func InstallBeforeWriteHooks(engine *memql.MemQLEngine, autos []*Automation, registry StepExecutorRegistry, logger *slog.Logger) {
	engine.SetBeforeWriteHooks(BuildBeforeWriteHooks(engine, autos, registry, logger))
}

// BuildBeforeWriteHooks builds the same hooks used at boot and by the corpus runner.
func BuildBeforeWriteHooks(engine *memql.MemQLEngine, autos []*Automation, registry StepExecutorRegistry, logger *slog.Logger) map[string][]memql.BeforeWriteHook {
	hooks := map[string][]memql.BeforeWriteHook{}
	for _, a := range autos {
		if a.BeforeWrite == nil || !a.IsEnabled() {
			continue
		}
		a := a
		hooks[a.BeforeWrite.Concept] = append(hooks[a.BeforeWrite.Concept], memql.BeforeWriteHook{Name: a.Name, On: a.BeforeWrite.On, Apply: func(ctx context.Context, row map[string]any) error {
			ctx = memql.ContextWithBeforeWrite(ctx)
			runner := NewLogicRunner(engine, registry, logger)
			evaluator := runner.newEvaluatorForLogic(ctx, row)
			rowValue := memql.ExprRow{ID: memql.BeforeWriteRowID(ctx), Concept: a.BeforeWrite.Concept, Payload: row}
			evaluator.SetCustom("row", rowValue)
			evaluator.enterStatements()
			if lam := a.BeforeWrite.filter; lam != nil {
				filtered := evaluator.childFrame()
				filtered.names.bind(lam.Params[0], rowValue)
				ok, err := filtered.EvalV1Condition(ctx, lam.Body)
				if err != nil {
					return err
				}
				if !ok {
					return nil
				}
			}
			fields := []string{}
			ex := &Executor{engine: engine, logger: runner.logger, stepRegistry: beforeWriteRegistry{base: registry, row: row, fields: &fields}}
			execution := NewExecution(a.Name, "before-write")
			execution.SourceTrusted = a.Trusted && auth.OriginFromContext(ctx).IsInternal()
			run := &sequenceRun{stepCtx: &StepContext{Engine: engine, Logger: runner.logger, Evaluator: evaluator, Execution: execution}}
			_, err := ex.runSequence(ctx, a.Steps, run)
			if err != nil {
				return err
			}
			beforeWritesTotal.WithLabelValues(a.Name).Inc()
			runner.logger.Info("before-write row adjusted", "automation", a.Name, "concept", a.BeforeWrite.Concept, logSubject.Subject(a.BeforeWrite.Concept, memql.BeforeWriteRowID(ctx)), "fields", fields)
			return nil
		}})
	}
	return hooks
}

type beforeWriteRegistry struct {
	base   StepExecutorRegistry
	row    map[string]any
	fields *[]string
}

func (r beforeWriteRegistry) Execute(ctx context.Context, s *Step, sc *StepContext) (*StepResult, error) {
	if s.Type != StepTypeFieldWrite {
		return r.base.Execute(ctx, s, sc)
	}
	value, err := sc.Evaluator.ResolveV1Value(ctx, s.FieldWrite.Value)
	if err != nil {
		return nil, err
	}
	if memql.IsAbsent(value) {
		delete(r.row, s.FieldWrite.Field)
	} else {
		r.row[s.FieldWrite.Field] = value
	}
	*r.fields = append(*r.fields, s.FieldWrite.Field)
	return &StepResult{StepId: s.ID, Status: "success", Result: value}, nil
}
