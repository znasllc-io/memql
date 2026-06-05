package steps

// sandbox_registry.go -- the side-effect interception layer for the Gate-2
// behavioral dry-run sandbox (issue #958).
//
// sandboxStepRegistry wraps the real step Registry and satisfies
// automations.StepExecutorRegistry, so the automation Executor drives it exactly
// like the production registry. For each step it decides, by TIER:
//
//   - WRITE-BEARING (a direct mutation step, or a function step whose function
//     is a mutation): the would-be write is evaluated + recorded into the
//     manifest under the run's ephemeral sandbox partition and a synthetic
//     success result is returned. The write NEVER reaches engine.Execute, so no
//     row lands in the live graph. Later steps that reference the step still see
//     a success result.
//   - WEBHOOK / external POST: the request is evaluated + recorded as a blocked
//     webhook and a synthetic success result is returned. Nothing is dispatched
//     under the isolated tier.
//   - READ / pure compute (queries, si(), similarTo, webSearch, fetchUrl, logic,
//     shape, switch, ...): DELEGATED to the real executor -> real engine.Execute
//     -> real + metered.
//
// AI/web read METERING + cost (increment 2) and full-sandbox-live webhook
// routing (increment 3) layer onto this same structure.

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/memql"
)

// sandboxStepRegistry intercepts side-effecting steps and delegates reads.
type sandboxStepRegistry struct {
	real      *Registry
	engine    *memql.MemQLEngine
	partition string
	mode      memql.DryRunMode

	mu              sync.Mutex
	mutations       []memql.RecordedMutation
	blockedWebhooks []memql.BlockedWebhook
	aiCalls         []memql.RecordedAiCall
	webCalls        []memql.RecordedWebCall
	// intercepted records, per step id, the side-effect-layer annotation so the
	// trace can mark which steps were rewritten/blocked vs ran for real.
	intercepted map[string]string
}

// newSandboxStepRegistry builds a sandbox registry wrapping the real one.
func newSandboxStepRegistry(real *Registry, engine *memql.MemQLEngine, partition string, mode memql.DryRunMode) *sandboxStepRegistry {
	return &sandboxStepRegistry{
		real:        real,
		engine:      engine,
		partition:   partition,
		mode:        mode,
		intercepted: map[string]string{},
	}
}

// Execute routes one step through the interception tiers. It satisfies
// automations.StepExecutorRegistry.
func (s *sandboxStepRegistry) Execute(ctx context.Context, step *automations.Step, stepCtx *automations.StepContext) (*automations.StepResult, error) {
	switch step.Type {
	case automations.StepTypeMutation:
		return s.interceptDirectMutation(step, stepCtx)
	case automations.StepTypeWebhook:
		return s.interceptWebhook(step, stepCtx)
	case automations.StepTypeFunction:
		switch s.functionKind(step) {
		case "mutation":
			return s.interceptMutationFunction(step, stepCtx)
		case "logic":
			// A logic function is the real authored write path: its body runs
			// mutation / webhook steps. Recurse through the SAME sandbox
			// registry (via a sandbox LogicRunner) so those nested side effects
			// are intercepted too, instead of escaping to engine.Execute.
			return s.interceptLogicFunction(ctx, step, stepCtx)
		default:
			// A query / si() / web-read builtin is a real + metered read --
			// delegate to the real executor.
			return s.real.Execute(ctx, step, stepCtx)
		}
	default:
		// Reads + pure compute (query, shape, switch, forEach, parallel, ...)
		// run for real through the production executors.
		return s.real.Execute(ctx, step, stepCtx)
	}
}

// note records a side-effect-layer annotation for a step id (for the trace).
func (s *sandboxStepRegistry) note(stepId, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.intercepted[stepId] = msg
}

// functionKind returns the lower-cased FunctionKind ("mutation" / "logic" /
// "query" / ...) of a function step's target, by consulting the engine's live
// function registry. Returns "" when the function is unknown (delegated to the
// real executor, which surfaces the error -- we must not silently swallow it).
func (s *sandboxStepRegistry) functionKind(step *automations.Step) string {
	if step.Function == nil || s.engine == nil {
		return ""
	}
	name := strings.TrimSpace(step.Function.Name)
	if name == "" {
		return ""
	}
	fn, ok := s.engine.Functions().Lookup(name)
	if !ok || fn == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(fn.FunctionKind))
}

// interceptLogicFunction runs a multi-step logic body through a sandbox
// LogicRunner that shares THIS registry, so the body's mutation / webhook steps
// are intercepted recursively. A single-statement logic (no multi-step body)
// evaluates through engine.Execute and is delegated to the real executor; its
// own mutation/webhook would route back through the engine's wired runner --
// out of scope for increment 1 (multi-step logic is the authored write path).
func (s *sandboxStepRegistry) interceptLogicFunction(ctx context.Context, step *automations.Step, stepCtx *automations.StepContext) (*automations.StepResult, error) {
	name := strings.TrimSpace(step.Function.Name)
	fn, ok := s.engine.Functions().Lookup(name)
	if !ok || fn == nil || fn.LogicSteps == nil {
		// Single-statement logic (or unknown): no multi-step body to recurse
		// into. Delegate to the real executor.
		return s.real.Execute(ctx, step, stepCtx)
	}

	started := time.Now()
	s.note(step.ID, "logic "+name+" run in sandbox (nested side effects intercepted)")
	args := s.resolveFunctionArgs(step.Function, stepCtx)
	runner := automations.NewLogicRunner(s.engine, s, nil)
	out, err := runner.RunLogic(ctx, name, fn.LogicSteps, args)
	now := time.Now()
	if err != nil {
		return &automations.StepResult{
			StepId:      step.ID,
			Status:      "failed",
			Error:       err.Error(),
			StartedAt:   started,
			CompletedAt: now,
			Duration:    now.Sub(started),
		}, err
	}
	return &automations.StepResult{
		StepId:      step.ID,
		Status:      "success",
		Result:      out,
		StartedAt:   started,
		CompletedAt: now,
		Duration:    now.Sub(started),
	}, nil
}

// interceptDirectMutation records a direct mutation step's would-be write and
// returns a synthetic success without touching the engine.
func (s *sandboxStepRegistry) interceptDirectMutation(step *automations.Step, stepCtx *automations.StepContext) (*automations.StepResult, error) {
	started := time.Now()
	rec := memql.RecordedMutation{StepId: step.ID, Partition: s.partition}
	if step.Mutation != nil {
		rec.Concept = step.Mutation.Concept
		rec.Id = step.Mutation.ID
		// Evaluate the payload through the SAME evaluator path the real
		// MutationExecutor uses, so the recorded write is faithful to what
		// would have been stored.
		if payload, err := (&MutationExecutor{}).evaluatePayload(stepCtx.Evaluator, step.Mutation.Payload); err == nil {
			rec.Payload = payload
		} else {
			rec.Payload = step.Mutation.Payload
		}
	}
	s.recordMutation(rec)
	s.note(step.ID, "mutation isolated to sandbox partition "+s.partition)
	return s.syntheticSuccess(step.ID, started, rec.Payload), nil
}

// interceptMutationFunction records a mutation-function call's would-be write
// (concept from the function's bound concept, payload from the resolved args)
// and returns a synthetic success without touching the engine.
func (s *sandboxStepRegistry) interceptMutationFunction(step *automations.Step, stepCtx *automations.StepContext) (*automations.StepResult, error) {
	started := time.Now()
	rec := memql.RecordedMutation{StepId: step.ID, Partition: s.partition}

	if fn, ok := s.engine.Functions().Lookup(strings.TrimSpace(step.Function.Name)); ok && fn != nil {
		rec.Concept = fn.BoundConcept
	}
	// Resolve the function's args against the evaluator so the recorded payload
	// reflects the values the mutation would have written. A single positional
	// object arg ({"0": {...}}) is the common authored shape; surface its fields
	// directly so the manifest reads naturally.
	resolved := s.resolveFunctionArgs(step.Function, stepCtx)
	if len(resolved) == 1 {
		if obj, ok := resolved["0"].(map[string]any); ok {
			resolved = obj
		}
	}
	rec.Payload = resolved

	s.recordMutation(rec)
	s.note(step.ID, "mutation function "+step.Function.Name+" isolated to sandbox partition "+s.partition)
	return s.syntheticSuccess(step.ID, started, rec.Payload), nil
}

// resolveFunctionArgs resolves a function step's args to concrete values via the
// MutationExecutor's value evaluator (which understands $refs, concat(), etc.).
// The raw resolved map is returned (keyed as authored, commonly {"0": {...}} for
// a single positional object arg); callers flatten the "0" object when they want
// a payload-shaped map.
func (s *sandboxStepRegistry) resolveFunctionArgs(fn *automations.FunctionStepConfig, stepCtx *automations.StepContext) map[string]any {
	if fn == nil || len(fn.Args) == 0 || stepCtx == nil || stepCtx.Evaluator == nil {
		return map[string]any{}
	}
	me := &MutationExecutor{}
	resolved := map[string]any{}
	for k, v := range fn.Args {
		val, err := me.evaluateValue(stepCtx.Evaluator, v)
		if err != nil {
			val = v
		}
		resolved[k] = val
	}
	return resolved
}

// interceptWebhook records a webhook step's request as a blocked external POST
// and returns a synthetic success. Nothing is dispatched under the isolated
// tier; full-sandbox-live routing (increment 3) re-routes to a capture sink.
func (s *sandboxStepRegistry) interceptWebhook(step *automations.Step, stepCtx *automations.StepContext) (*automations.StepResult, error) {
	started := time.Now()
	blocked := memql.BlockedWebhook{StepId: step.ID, Method: "POST"}
	if step.Webhook != nil {
		if url, err := stepCtx.Evaluator.EvaluateString(step.Webhook.URL); err == nil {
			blocked.Url = url
		} else {
			blocked.Url = step.Webhook.URL
		}
		if m := strings.ToUpper(strings.TrimSpace(step.Webhook.Method)); m != "" {
			blocked.Method = m
		}
		if step.Webhook.Body != nil {
			if body, err := stepCtx.Evaluator.EvaluateMap(step.Webhook.Body); err == nil {
				blocked.Body = body
			} else {
				blocked.Body = step.Webhook.Body
			}
		}
	}
	s.recordBlockedWebhook(blocked)
	s.note(step.ID, "webhook recorded and blocked")
	return s.syntheticSuccess(step.ID, started, map[string]any{
		"dryRun":  true,
		"blocked": true,
		"url":     blocked.Url,
		"method":  blocked.Method,
	}), nil
}

// syntheticSuccess builds a success StepResult for an intercepted step so
// downstream steps that reference it (and the executor's chain) proceed.
func (s *sandboxStepRegistry) syntheticSuccess(stepId string, started time.Time, result any) *automations.StepResult {
	now := time.Now()
	return &automations.StepResult{
		StepId:      stepId,
		Status:      "success",
		Result:      result,
		StartedAt:   started,
		CompletedAt: now,
		Duration:    now.Sub(started),
		Metadata:    map[string]any{"dryRunIntercepted": true},
	}
}

func (s *sandboxStepRegistry) recordMutation(rec memql.RecordedMutation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mutations = append(s.mutations, rec)
}

func (s *sandboxStepRegistry) recordBlockedWebhook(rec memql.BlockedWebhook) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blockedWebhooks = append(s.blockedWebhooks, rec)
}

// manifest returns the collected side-effect manifest. Slices are normalised to
// non-nil so the persisted JSON carries empty arrays rather than nulls.
func (s *sandboxStepRegistry) manifest() memql.SideEffectManifest {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := memql.SideEffectManifest{
		Mutations:       s.mutations,
		AiCalls:         s.aiCalls,
		WebCalls:        s.webCalls,
		BlockedWebhooks: s.blockedWebhooks,
	}
	if m.Mutations == nil {
		m.Mutations = []memql.RecordedMutation{}
	}
	if m.AiCalls == nil {
		m.AiCalls = []memql.RecordedAiCall{}
	}
	if m.WebCalls == nil {
		m.WebCalls = []memql.RecordedWebCall{}
	}
	if m.BlockedWebhooks == nil {
		m.BlockedWebhooks = []memql.BlockedWebhook{}
	}
	return m
}

// buildTrace assembles the behavioral trace in the automation's declared step
// order, annotating each step the side-effect layer intercepted. Driving the
// order off automation.Steps (rather than the execution's map) keeps the trace
// faithful to authoring order and carries each step's type.
func (s *sandboxStepRegistry) buildTrace(automation *automations.Automation, exec *automations.AutomationExecution) []memql.DryRunStep {
	trace := []memql.DryRunStep{}
	if automation == nil || exec == nil {
		return trace
	}
	s.mu.Lock()
	notes := make(map[string]string, len(s.intercepted))
	for k, v := range s.intercepted {
		notes[k] = v
	}
	s.mu.Unlock()

	for _, step := range automation.Steps {
		if step == nil {
			continue
		}
		ds := memql.DryRunStep{StepId: step.ID, StepType: string(step.Type)}
		if r, ok := exec.Steps[step.ID]; ok && r != nil {
			ds.Status = r.Status
			ds.DurationMs = r.Duration.Milliseconds()
			if r.Error != "" {
				ds.Note = r.Error
			}
		} else {
			// The step never produced a result (a prior step failed and stopped
			// the run). Record it as not-run so the trace shows what was reached.
			ds.Status = "notRun"
		}
		if note, ok := notes[step.ID]; ok {
			ds.Intercepted = true
			ds.Note = note
		}
		trace = append(trace, ds)
	}
	return trace
}

// compile-time check: the sandbox registry must satisfy the executor's step
// registry contract so the real Executor can drive it.
var _ automations.StepExecutorRegistry = (*sandboxStepRegistry)(nil)
