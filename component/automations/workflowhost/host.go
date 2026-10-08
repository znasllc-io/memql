// Package workflowhost binds a native execution scope to installed MemQL
// templates. The ordinary automation interpreter owns control flow; the host
// supplies bounded operations and retains the caller's authority and journal.
// It is not a second language or an external-effect retry engine.
package workflowhost

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"sort"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/actions"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/steps"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

type Operation = memql.WorkflowOperation
type Loader func(string) (*automations.Automation, error)

type Options struct {
	Logger *slog.Logger
	// AmbientEngine supplies config/partition bindings only, never a step executor.
	AmbientEngine *memql.MemQLEngine
	Load          Loader
	LoadLogic     func(string) (*memql.Function, error)
	LoadAction    func(string) (*actions.Action, error)
	// Operations binds builtin names and fully qualified action capabilities.
	// No action can escape this allowlist to the ambient engine or local shell.
	Operations map[string]Operation
}

var installed = sync.OnceValues(func() (map[string]*automations.Automation, error) {
	all, err := automations.NewLoader(automations.LoaderOptions{}).LoadAll()
	if err != nil {
		return nil, err
	}
	out := make(map[string]*automations.Automation, len(all))
	for _, a := range all {
		out[a.Name] = a
	}
	return out, nil
})

func Load(name string) (*automations.Automation, error) {
	all, err := installed()
	if err != nil {
		return nil, err
	}
	if a := all[name]; a != nil {
		return a, nil
	}
	return nil, fmt.Errorf("workflow %q is not installed", name)
}

// ScopedCapabilities makes a scoped operation visible to the engine's boot
// audit without making its authority forgeable by a direct DSL/wire caller.
// Only Run supplies the actual handlers, for one already-authorized scope.
func ScopedCapabilities(operations map[string]Operation) []memql.IntegrationCapability {
	names := make([]string, 0, len(operations))
	for name := range operations {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]memql.IntegrationCapability, 0, len(names))
	for _, name := range names {
		out = append(out, memql.IntegrationCapability{Name: name,
			Description: "Bounded operation available only inside its authorized workflow scope.",
			Handler: func(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error) {
				return nil, fmt.Errorf("%s requires its authorized workflow scope", name)
			},
		})
	}
	return out
}

// Run preflights the entire call tree before any operation, then executes it.
// Operations are call-local closures, never serializable tokens or global
// capabilities. A public builtin calling this host must enforce its own gate
// before constructing the scope. Completed-effect recovery remains with the
// operation's existing journal; errors are not implicitly retried here.
func Run(ctx context.Context, name string, args map[string]any, opts Options) (any, error) {
	if opts.Load == nil {
		opts.Load = Load
	}
	if opts.LoadLogic == nil {
		opts.LoadLogic = loadLogic
	}
	if opts.LoadAction == nil {
		opts.LoadAction = loadAction
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	opts.Operations = maps.Clone(opts.Operations)
	h := &host{opts: opts, definitions: map[string]*automations.Automation{}, logics: map[string]*preparedLogic{}}
	if err := h.prepare(name, map[string]bool{}); err != nil {
		return nil, err
	}
	h.registry = steps.NewRegistry()
	h.registry.Register(automations.StepTypeFunction, h)
	h.registry.Register(automations.StepTypeAction, h)
	execution, err := h.TriggerAutomationWithArgs(ctx, name, args)
	if err != nil {
		// The interpreter stores an error's printable text for its journal.
		// Native callers also need the original typed refusal for errors.As.
		return nil, err
	}
	return execution.Output, nil
}

type host struct {
	opts        Options
	definitions map[string]*automations.Automation
	logics      map[string]*preparedLogic
	registry    *steps.Registry
	actions     map[string]*actions.Registry
	mu          sync.Mutex
}

func (h *host) prepare(name string, visiting map[string]bool) error {
	if visiting[name] {
		return fmt.Errorf("workflow recursion at %q", name)
	}
	if h.definitions[name] != nil {
		return nil
	}
	a, err := h.opts.Load(name)
	if err != nil {
		return err
	}
	if a == nil || a.Name != name || !a.Template || !a.IsEnabled() || a.BeforeWrite != nil || a.IsScheduled() || a.IsEventTriggered() || a.JournalRequired || a.Mode != nil || a.Loop != nil {
		return fmt.Errorf("workflow %q is not an enabled callable template", name)
	}
	a, err = automations.NewLoader(automations.LoaderOptions{Logger: h.opts.Logger}).Snapshot(a)
	if err != nil {
		return err
	}
	visiting[name] = true
	var walk func([]*automations.Step) error
	walk = func(body []*automations.Step) error {
		for _, s := range body {
			switch s.Type {
			case automations.StepTypeExpression, automations.StepTypeReturn:
			case automations.StepTypeAction:
				if err := h.prepareAction(s); err != nil {
					return err
				}
			case automations.StepTypeFunction:
				if s.Function != nil && s.Function.Kind == "logic" {
					if err := h.prepareLogic(s.Function.Name, visiting); err != nil {
						return err
					}
					continue
				}
				if s.Function == nil || h.opts.Operations[s.Function.Name] == nil {
					return fmt.Errorf("workflow %q step %q has no scoped operation", name, s.ID)
				}
			case automations.StepTypeAutomation:
				if s.Automation == nil || s.Automation.Async {
					return fmt.Errorf("workflow children must be synchronous")
				}
				if err := h.prepare(s.Automation.Name, visiting); err != nil {
					return err
				}
			case automations.StepTypeForEach:
				if s.ForEach == nil {
					return fmt.Errorf("workflow loop has no body")
				}
				if err := walk(s.ForEach.Do); err != nil {
					return err
				}
			case automations.StepTypeParallel:
				if s.Parallel == nil || (s.Parallel.Wait != "" && s.Parallel.Wait != "all") {
					return fmt.Errorf("workflow parallel must join all branches")
				}
				if err := walk(s.Parallel.Branches); err != nil {
					return err
				}
			case automations.StepTypeBlock:
				if s.Block == nil {
					return fmt.Errorf("workflow block has no body")
				}
				if err := walk(s.Block.Steps); err != nil {
					return err
				}
			default:
				return fmt.Errorf("workflow %q step kind %q is outside its scoped contract", name, s.Type)
			}
		}
		return nil
	}
	if err := walk(a.Steps); err != nil {
		return err
	}
	delete(visiting, name)
	h.definitions[name] = a
	return nil
}

func (h *host) TriggerAutomation(ctx context.Context, name string) (*automations.AutomationExecution, error) {
	return h.TriggerAutomationWithArgs(ctx, name, nil)
}

func (h *host) TriggerAutomationWithArgs(ctx context.Context, name string, args map[string]any) (*automations.AutomationExecution, error) {
	a := h.definitions[name]
	if a == nil {
		return nil, fmt.Errorf("workflow %q was not preflighted", name)
	}
	return automations.ExecuteInScope(ctx, a, args, automations.ExecutorOptions{Logger: h.opts.Logger, Engine: h.opts.AmbientEngine, StepRegistry: h.registry, AutomationTrigger: h})
}

func (h *host) Execute(ctx context.Context, step *automations.Step, sc *automations.StepContext) (*automations.StepResult, error) {
	if step.Type == automations.StepTypeAction {
		return h.executeAction(ctx, step, sc)
	}
	started := time.Now()
	args, err := sc.Evaluator.ResolveV1Map(ctx, step.Function.Args)
	var value any
	if err == nil {
		if step.Function.Kind == "logic" {
			fn := h.logics[step.Function.Name]
			if fn == nil {
				err = fmt.Errorf("logic %q was not preflighted", step.Function.Name)
			} else if err = fn.validate(args); err == nil {
				value, err = automations.NewLogicRunner(h.opts.AmbientEngine, h.registry, h.opts.Logger).WithoutJournal().RunPreparedLogicBody(ctx, fn.body, args)
			}
		} else {
			h.mu.Lock()
			if err = ctx.Err(); err == nil {
				value, err = h.opts.Operations[step.Function.Name](ctx, args)
			}
			h.mu.Unlock()
		}
	}
	r := &automations.StepResult{StepId: step.ID, StartedAt: started, CompletedAt: time.Now(), Status: "success", Result: value}
	if err != nil {
		r.Status, r.Error = "failed", err.Error()
	}
	return r, err
}
