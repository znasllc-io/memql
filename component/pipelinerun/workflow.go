package pipelinerun

// The DSL owns sequence, parallelism and failure policy. This host binds its
// bounded operations to ONE claimed run. It uses the ordinary automation
// interpreter and authored-action binder, with the pipeline's existing required
// journal as the authority for effects. Control frames are reconstructed from
// immutable input and saved receipts on recovery; no second work run is opened.

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/actions"
	"github.com/znasllc-io/memql/component/actions/pin"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/steps"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/core/id"
)

const defaultPipelineWorkflow = "runPipelineStages"

type workflowProgram struct {
	root        string
	definitions map[string]*automations.Automation
	actions     *actions.Registry
	fingerprint string
}

var installedPipelineWorkflows = sync.OnceValues(func() (map[string]*automations.Automation, error) {
	loaded, err := automations.NewLoader(automations.LoaderOptions{}).LoadAll()
	if err != nil {
		return nil, err
	}
	byName := make(map[string]*automations.Automation, len(loaded))
	for _, a := range loaded {
		byName[a.Name] = a
	}
	return byName, nil
})

func loadPipelineWorkflow(name string) (*automations.Automation, error) {
	all, err := installedPipelineWorkflows()
	if err != nil {
		return nil, err
	}
	a := all[name]
	if a == nil {
		return nil, fmt.Errorf("pipeline workflow %q is not installed", name)
	}
	return a, nil
}

func (dr *runDriver) prepareWorkflow(name string) error {
	if name == "" {
		name = defaultPipelineWorkflow
	}
	load := dr.d.LoadWorkflow
	if load == nil {
		load = loadPipelineWorkflow
	}
	reg := actions.Default()
	if err := actions.DefaultLoadError(); err != nil {
		return err
	}
	p := &workflowProgram{root: name, definitions: map[string]*automations.Automation{}, actions: reg}
	identities := map[string]any{}
	visiting := map[string]bool{}
	var visit func(string) error
	var walk func([]*automations.Step) error
	walk = func(list []*automations.Step) error {
		for _, step := range list {
			switch step.Type {
			case automations.StepTypeExpression, automations.StepTypeReturn:
			case automations.StepTypeFunction:
				if step.Function == nil || (step.Function.Name != "pipelineWorkflowFacts" && step.Function.Name != "pipelineSkipStep") {
					return fmt.Errorf("pipeline workflow step %q calls a function outside its run-scoped contract", step.ID)
				}
			case automations.StepTypeAction:
				if step.Action == nil || step.Action.Surface != "" {
					return fmt.Errorf("pipeline workflow actions use the claimed run's execution surface")
				}
				ref, err := pin.Parse(step.Action.Ref)
				if err != nil {
					return err
				}
				var a *actions.Action
				if ref.Floating {
					a, _ = reg.LookupLatest(ref.ID)
				} else {
					a, _ = reg.Lookup(ref.ID, ref.Version)
				}
				if a == nil || (a.Capability != "integration.pipelines.executeStep" && a.Capability != "integration.pipelines.reportProgress") {
					return fmt.Errorf("pipeline action %q is outside its run-scoped contract", step.Action.Ref)
				}
				identities["action:"+step.Action.Ref] = a
			case automations.StepTypeAutomation:
				if step.Automation == nil || step.Automation.Async {
					return fmt.Errorf("pipeline child workflows must be synchronous")
				}
				if err := visit(step.Automation.Name); err != nil {
					return err
				}
			case automations.StepTypeForEach:
				if step.ForEach == nil {
					return fmt.Errorf("pipeline workflow loop has no body")
				}
				if err := walk(step.ForEach.Do); err != nil {
					return err
				}
			case automations.StepTypeParallel:
				if step.Parallel == nil {
					return fmt.Errorf("pipeline workflow parallel has no branches")
				}
				if step.Parallel.Wait != "" && step.Parallel.Wait != "all" {
					return fmt.Errorf("pipeline workflow must wait for every parallel branch")
				}
				if err := walk(step.Parallel.Branches); err != nil {
					return err
				}
			case automations.StepTypeBlock:
				if step.Block == nil {
					return fmt.Errorf("pipeline workflow block has no body")
				}
				if err := walk(step.Block.Steps); err != nil {
					return err
				}
			default:
				return fmt.Errorf("pipeline workflow step kind %q is outside its run-scoped contract", step.Type)
			}
		}
		return nil
	}
	visit = func(n string) error {
		if visiting[n] {
			return fmt.Errorf("pipeline workflow recursion at %q", n)
		}
		if p.definitions[n] != nil {
			return nil
		}
		a, err := load(n)
		if err != nil {
			return err
		}
		if a == nil || !a.Template || !a.IsEnabled() || a.BeforeWrite != nil || a.IsScheduled() || a.IsEventTriggered() || a.JournalRequired || a.Mode != nil || a.Loop != nil {
			return fmt.Errorf("pipeline workflow %q is not an enabled callable automation", n)
		}
		visiting[n] = true
		if err := walk(a.Steps); err != nil {
			return err
		}
		delete(visiting, n)
		p.definitions[n] = a
		identities["automation:"+n] = a.DefinitionFingerprint(id.New())
		return nil
	}
	if err := visit(name); err != nil {
		return err
	}
	// Notification policy can be reached through a native step, so its pure
	// recipes are part of the immutable execution contract as well.
	for _, helper := range []string{"pipelineNotificationCopy", "pipelineNotificationOutcome"} {
		if err := visit(helper); err != nil {
			return err
		}
	}
	p.fingerprint = string(id.New().MustFromMap(identities))
	dr.workflow = p
	return nil
}

type pipelineWorkflowHost struct {
	dr         *runDriver
	slots      chan struct{}
	locks      map[string]*sync.Mutex
	byKey      map[string]*stepTrack
	registry   *steps.Registry
	progressMu sync.Mutex
}

// This value is never accepted from a caller or carried over a wire. The
// owning driver binds it after claiming the run; every write still fences the
// lease in the database. An ordinary builtin/action call has no such scope.
type pipelineWorkflowContextKey struct{}

func workflowCapability(name string) func(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error) {
	return func(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
		h, _ := ctx.Value(pipelineWorkflowContextKey{}).(*pipelineWorkflowHost)
		if h == nil {
			return nil, fmt.Errorf("pipeline workflow operation %s requires an active claimed run", name)
		}
		var value any
		var err error
		switch name {
		case "workflowFacts":
			value = h.facts()
		case "skipStep":
			value, err = h.skip(ctx, args)
		default:
			value, err = h.Invoke(ctx, "integration.pipelines."+name, args)
		}
		if err != nil {
			return nil, err
		}
		if payload, ok := value.(map[string]any); ok {
			return resultNode(payload), nil
		}
		return resultNode(map[string]any{}), nil
	}
}

func newPipelineWorkflowHost(dr *runDriver) *pipelineWorkflowHost {
	h := &pipelineWorkflowHost{dr: dr, slots: make(chan struct{}, maxConcurrentSteps), locks: map[string]*sync.Mutex{}, byKey: map[string]*stepTrack{}}
	for _, t := range dr.tracks {
		h.locks[t.step.Key] = &sync.Mutex{}
		h.byKey[t.step.Key] = t
	}
	h.registry = steps.NewRegistry()
	h.registry.Register(automations.StepTypeFunction, h)
	h.registry.Register(automations.StepTypeAction, &steps.ActionExecutor{Registry: dr.workflow.actions, Dispatcher: h})
	return h
}

func (h *pipelineWorkflowHost) TriggerAutomation(ctx context.Context, name string) (*automations.AutomationExecution, error) {
	return h.TriggerAutomationWithArgs(ctx, name, nil)
}

func (h *pipelineWorkflowHost) TriggerAutomationWithArgs(ctx context.Context, name string, args map[string]any) (*automations.AutomationExecution, error) {
	a := h.dr.workflow.definitions[name]
	if a == nil {
		return nil, fmt.Errorf("pipeline workflow %q was not pinned before execution", name)
	}
	ctx = context.WithValue(ctx, pipelineWorkflowContextKey{}, h)
	return automations.ExecuteInScope(ctx, a, args, automations.ExecutorOptions{Logger: h.dr.log, StepRegistry: h.registry, AutomationTrigger: h})
}

func (h *pipelineWorkflowHost) Execute(ctx context.Context, step *automations.Step, sc *automations.StepContext) (*automations.StepResult, error) {
	started := time.Now()
	if step.Function == nil {
		return nil, fmt.Errorf("pipeline runtime function is missing")
	}
	args, err := sc.Evaluator.ResolveV1Map(ctx, step.Function.Args)
	var value any
	if err == nil {
		switch step.Function.Name {
		case "pipelineWorkflowFacts":
			value = h.facts()
		case "pipelineSkipStep":
			value, err = h.skip(ctx, args)
		default:
			err = fmt.Errorf("pipeline runtime function %q is unavailable", step.Function.Name)
		}
	}
	result := &automations.StepResult{StepId: step.ID, StartedAt: started, CompletedAt: time.Now(), Status: "success", Result: value}
	if err != nil {
		result.Status = "failed"
		result.Error = err.Error()
	}
	return result, err
}

func (h *pipelineWorkflowHost) facts() map[string]any {
	states := []any{}
	for index, stage := range h.dr.stages {
		for _, t := range stage.tracks {
			s := t.snapshot()
			states = append(states, map[string]any{"key": t.step.Key, "stage": stage.name, "stageIndex": index, "status": string(s.Status), "kind": string(t.step.Kind)})
		}
	}
	return map[string]any{"stopped": h.dr.lease.isLost() || h.dr.lease.isCancelled(), "steps": states}
}

func (h *pipelineWorkflowHost) skip(ctx context.Context, args map[string]any) (any, error) {
	key, code, reason := stringArg(args, "stepKey"), stringArg(args, "code"), stringArg(args, "reason")
	t := h.byKey[key]
	if t == nil {
		return nil, fmt.Errorf("step %q does not belong to this run", key)
	}
	// Workflow skips can never impersonate a carried successful receipt.
	if code != pipelines.CodeStageBlocked || reason == "" {
		return nil, fmt.Errorf("a workflow skip requires pipeline_stage_blocked and a reason")
	}
	h.locks[key].Lock()
	defer h.locks[key].Unlock()
	if !t.finished() && !h.dr.lease.isLost() && !h.dr.lease.isCancelled() {
		if !h.dr.settle(ctx, t, skipReceipt(code, reason)) {
			return nil, fmt.Errorf("pipeline skip receipt could not be confirmed")
		}
	}
	return map[string]any{"status": string(t.snapshot().Status)}, nil
}

func (h *pipelineWorkflowHost) Invoke(ctx context.Context, capability string, args map[string]any) (any, error) {
	if h.dr.lease.isLost() || h.dr.lease.isCancelled() {
		return nil, nil
	}
	switch capability {
	case "integration.pipelines.reportProgress":
		h.progressMu.Lock()
		defer h.progressMu.Unlock()
		h.dr.progress(ctx)
		return nil, nil
	case "integration.pipelines.executeStep":
		key := stringArg(args, "stepKey")
		t := h.byKey[key]
		if t == nil {
			return nil, fmt.Errorf("step %q does not belong to this run", key)
		}
		h.locks[key].Lock()
		defer h.locks[key].Unlock()
		if t.finished() {
			return map[string]any{"status": string(t.snapshot().Status)}, nil
		}
		select {
		case h.slots <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-h.dr.lease.stop:
			return nil, nil
		}
		defer func() { <-h.slots }()
		h.dr.runStep(ctx, t)
		if h.dr.journalFailed.Load() || h.dr.cleanupFailed.Load() {
			return nil, fmt.Errorf("pipeline operation did not confirm its receipt or cleanup")
		}
		return map[string]any{"status": string(t.snapshot().Status)}, nil
	default:
		return nil, fmt.Errorf("capability %q is outside this pipeline run", capability)
	}
}

func (dr *runDriver) execute(ctx context.Context) error {
	h := newPipelineWorkflowHost(dr)
	stages := []any{}
	for index, stage := range dr.stages {
		items := []any{}
		for _, t := range stage.tracks {
			items = append(items, map[string]any{"key": t.step.Key, "kind": string(t.step.Kind)})
		}
		stages = append(stages, map[string]any{"name": stage.name, "stageIndex": index, "steps": items})
	}
	_, err := h.TriggerAutomationWithArgs(ctx, dr.workflow.root, map[string]any{"stages": stages})
	if dr.lease.isCancelled() {
		for _, t := range dr.tracks {
			if !t.finished() {
				dr.settle(ctx, t, cancelReceipt())
			}
		}
	}
	return err
}

func (dr *runDriver) workflowIdentity() string {
	if dr.workflow == nil {
		return ""
	}
	return dr.workflow.fingerprint
}

func modeForEvent(ctx context.Context, event pipelines.Event) (pipelines.Mode, error) {
	value, err := workflowhost.Run(ctx, "pipelineModeForEvent", map[string]any{"event": string(event)}, workflowhost.Options{})
	if err != nil {
		return "", err
	}
	mode, _ := value.(string)
	if mode != "" && mode != string(pipelines.ModeAffected) && mode != string(pipelines.ModeFull) {
		return "", fmt.Errorf("pipeline workflow selected unsupported mode %q", mode)
	}
	return pipelines.Mode(mode), nil
}

func versionForEvent(ctx context.Context, event pipelines.Event, sha, tag string) (string, error) {
	value, err := workflowhost.Run(ctx, "pipelineVersionForEvent", map[string]any{"eventName": string(event), "sha": sha, "tag": tag}, workflowhost.Options{})
	if err != nil {
		return "", err
	}
	version, ok := value.(string)
	if !ok || version == "" {
		return "", fmt.Errorf("pipeline workflow returned no version")
	}
	return version, nil
}
