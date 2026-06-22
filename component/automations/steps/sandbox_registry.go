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
//   - READ / pure compute (queries, ai(), similarTo, webSearch, fetchUrl, logic,
//     shape, switch, ...): the read is METERED into the manifest (an ai() call
//     -> aiCalls + a cost estimate; a similarTo / webSearch / fetchUrl call ->
//     webCalls) and then DELEGATED to the real executor -> real engine.Execute
//     -> real read.
//
// Full-sandbox-live webhook routing (increment 3) layers onto this structure.

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/memql"
)

// sandboxStepRegistry intercepts side-effecting steps and delegates reads.
type sandboxStepRegistry struct {
	real        *Registry
	engine      *memql.MemQLEngine
	partition   string
	mode        memql.DryRunMode
	captureSink string

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
// captureSink is the full-sandbox-live webhook capture sink URL (ignored under
// the isolated tier).
func newSandboxStepRegistry(real *Registry, engine *memql.MemQLEngine, partition string, mode memql.DryRunMode, captureSink string) *sandboxStepRegistry {
	return &sandboxStepRegistry{
		real:        real,
		engine:      engine,
		partition:   partition,
		mode:        mode,
		captureSink: captureSink,
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
			// A read (ai() / similarTo / webSearch / fetchUrl) or a plain
			// query: meter the read into the manifest (real + metered), then
			// delegate to the real executor so it runs for real.
			s.meterRead(step, stepCtx)
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
	args := s.resolveLogicCallArgs(step.Function, stepCtx)
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

// resolveLogicCallArgs resolves a forwarded LOGIC call's args through the SAME
// resolver the live FunctionExecutor uses (resolveArgsRefs), so a logic
// dispatched in the dry-run sandbox receives byte-for-byte the args it would on
// the live path -- and, critically, the triggering-event envelope under its
// declared `event` input.
//
// Why this differs from resolveFunctionArgs (memql#1727): an authored wrapper
// step forwards the event as the BARE runtime-reference token `event`
// (`logic autoJoinSI { event: event }`, the #418-pinned spelling). The live
// FunctionExecutor's resolveArgsRefs special-cases that token (isRuntimeReference
// recognises a bare `event`) and resolves it to the seeded event envelope
// object. The mutation-style evaluateValue resolveFunctionArgs uses does NOT --
// it treats bare `event` as a literal string, so RunLogic received
// args["event"] = "event" and every nested `args.event.payload.X` navigated into
// a string and resolved to null. That made the first nested step feeding such a
// reference to a @required argument fail with "required argument <x> is missing"
// for all five event-trigger logics on the dry-run / run_automation path, even
// though the live path (fixed in #1706) bound the event correctly. Converging
// the logic-forwarding arg resolution onto the live resolver makes the two
// surfaces bind the event identically and cannot drift.
func (s *sandboxStepRegistry) resolveLogicCallArgs(fn *automations.FunctionStepConfig, stepCtx *automations.StepContext) map[string]any {
	if fn == nil || len(fn.Args) == 0 || stepCtx == nil || stepCtx.Evaluator == nil {
		return map[string]any{}
	}
	resolved, err := resolveArgsRefs(fn.Args, stepCtx.Evaluator)
	if err != nil {
		resolved = fn.Args
	}
	// A logic call's named-arg block compiles to a SINGLE positional object arg
	// ({"0": {event: ...}}). The live FunctionExecutor flattens that {"0": obj}
	// shape (renderFunctionArgs) so the engine binds the inner object as the
	// logic's named args; the LogicRunner likewise expects the flat
	// {event: ...} map (newEvaluatorForLogic reads args["event"]). Unwrap the
	// positional object here so the dry-run hands RunLogic the SAME flat shape --
	// without this the event lands at args["0"]["event"], args["event"] is nil,
	// and the event binding is lost (memql#1727).
	if len(resolved) == 1 {
		if obj, ok := resolved["0"].(map[string]any); ok {
			return obj
		}
	}
	return resolved
}

// interceptWebhook captures a webhook step's request and returns a synthetic
// success. Under the isolated tier it is recorded-and-blocked (nothing
// dispatched). Under the full-sandbox-live tier it is re-routed to the capture
// sink (Sink recorded) -- the automation's REAL webhook target is never POSTed
// to under either tier.
func (s *sandboxStepRegistry) interceptWebhook(step *automations.Step, stepCtx *automations.StepContext) (*automations.StepResult, error) {
	started := time.Now()
	captured := memql.BlockedWebhook{StepId: step.ID, Method: "POST"}
	if step.Webhook != nil {
		if url, err := stepCtx.Evaluator.EvaluateString(step.Webhook.URL); err == nil {
			captured.Url = url
		} else {
			captured.Url = step.Webhook.URL
		}
		if m := strings.ToUpper(strings.TrimSpace(step.Webhook.Method)); m != "" {
			captured.Method = m
		}
		if step.Webhook.Body != nil {
			if body, err := stepCtx.Evaluator.EvaluateMap(step.Webhook.Body); err == nil {
				captured.Body = body
			} else {
				captured.Body = step.Webhook.Body
			}
		}
	}

	live := s.mode == memql.DryRunModeFullSandboxLive
	noteMsg := "webhook recorded and blocked"
	if live {
		captured.Sink = s.captureSink
		if captured.Sink == "" {
			captured.Sink = "default-capture-sink"
		}
		noteMsg = "webhook re-routed to capture sink " + captured.Sink + " (real target not called)"
	}

	s.recordBlockedWebhook(captured)
	s.note(step.ID, noteMsg)
	return s.syntheticSuccess(step.ID, started, map[string]any{
		"dryRun":  true,
		"blocked": !live,
		"sink":    captured.Sink,
		"url":     captured.Url,
		"method":  captured.Method,
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

// webReadFunctions is the set of read-builtin names that touch external surfaces
// (the web reads from the issue's tiered list). A function step targeting one of
// these is metered into webCalls.
var webReadFunctions = map[string]bool{
	"similarTo": true,
	"webSearch": true,
	"fetchUrl":  true,
}

// meterRead records a read step into the manifest before it is delegated to the
// real executor. An ai() call (the AI read) lands in aiCalls with a heuristic
// token + cost estimate; a similarTo / webSearch / fetchUrl call lands in
// webCalls with the resolved target. Plain query reads carry no external cost
// and are not metered. Recording happens up-front so the manifest reflects the
// read intent even if the real delegate later fails (e.g. no provider wired).
func (s *sandboxStepRegistry) meterRead(step *automations.Step, stepCtx *automations.StepContext) {
	if step.Function == nil {
		return
	}
	name := strings.TrimSpace(step.Function.Name)
	switch {
	case name == "ai":
		s.meterAiCall(step, stepCtx)
	case webReadFunctions[name]:
		s.meterWebCall(step, name, stepCtx)
	}
}

// meterAiCall records an ai() read with a heuristic prompt-token estimate (from
// the resolved-arg size) and the corresponding USD cost. Exact provider token
// usage is not surfaced through the step boundary, so the estimate is a
// documented heuristic the approver reads as an upper-bound order of magnitude,
// not a billed figure.
func (s *sandboxStepRegistry) meterAiCall(step *automations.Step, stepCtx *automations.StepContext) {
	resolved := s.resolveFunctionArgs(step.Function, stepCtx)
	promptTokens := estimateTokens(resolved)
	outputTokens := defaultSiOutputTokens
	cost := estimateSiCostUsd(promptTokens, outputTokens)
	s.mu.Lock()
	s.aiCalls = append(s.aiCalls, memql.RecordedAiCall{
		StepId:        step.ID,
		Function:      "ai",
		PromptTokens:  promptTokens,
		OutputTokens:  outputTokens,
		EstimatedCost: cost,
	})
	s.mu.Unlock()
	s.note(step.ID, "ai() metered (real read)")
}

// meterWebCall records a web read (similarTo / webSearch / fetchUrl) with the
// resolved target where one is discernible from the args (a url / query field).
func (s *sandboxStepRegistry) meterWebCall(step *automations.Step, name string, stepCtx *automations.StepContext) {
	resolved := s.resolveFunctionArgs(step.Function, stepCtx)
	target := webCallTarget(resolved)
	s.mu.Lock()
	s.webCalls = append(s.webCalls, memql.RecordedWebCall{
		StepId:   step.ID,
		Function: name,
		Target:   target,
	})
	s.mu.Unlock()
	s.note(step.ID, name+"() metered (real read)")
}

// webCallTarget pulls a human-readable target (url / query / chunk text) out of
// a web-read call's resolved args. Returns "" when no obvious target field is
// present.
func webCallTarget(resolved map[string]any) string {
	// A single positional object arg ({"0": {...}}) is the common shape.
	obj := resolved
	if len(resolved) == 1 {
		if inner, ok := resolved["0"].(map[string]any); ok {
			obj = inner
		}
	}
	for _, key := range []string{"url", "query", "text", "chunkId"} {
		if v, ok := obj[key]; ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return s
			}
		}
	}
	return ""
}

// Cost-estimate heuristics. Exact provider token usage is not surfaced through
// the step-execution boundary, so the dry-run reports an estimate the approver
// reads as an order-of-magnitude upper bound, not a billed figure.
const (
	// charsPerToken is the rough chars-per-token ratio used to estimate prompt
	// size from the resolved-arg JSON length (~4 chars/token for English).
	charsPerToken = 4
	// defaultSiOutputTokens is the assumed completion size for one ai() call
	// when actual usage is unknown.
	defaultSiOutputTokens = 512
	// siInputUsdPerMillion / siOutputUsdPerMillion are conservative default
	// rates (a mid-tier chat model) for the estimate. They are intentionally a
	// fixed heuristic, not a per-provider lookup -- the dry-run cannot know
	// which provider a given ai() template will resolve to at run time.
	siInputUsdPerMillion  = 0.50
	siOutputUsdPerMillion = 1.50
)

// estimateTokens estimates the prompt-token count for a resolved arg map from
// its JSON-serialized length. Returns a small floor so an empty-arg ai() call
// still reports a non-zero estimate.
func estimateTokens(resolved map[string]any) int {
	if len(resolved) == 0 {
		return 1
	}
	b, err := json.Marshal(resolved)
	if err != nil {
		return 1
	}
	n := len(b) / charsPerToken
	if n < 1 {
		n = 1
	}
	return n
}

// estimateSiCostUsd applies the default per-million rates to an estimated
// prompt/output token split.
func estimateSiCostUsd(promptTokens, outputTokens int) float64 {
	return siInputUsdPerMillion*float64(promptTokens)/1_000_000 +
		siOutputUsdPerMillion*float64(outputTokens)/1_000_000
}

// costEstimate aggregates the metered AI calls into the report's cost estimate
// (total tokens + USD). Web reads carry no token cost.
func (s *sandboxStepRegistry) costEstimate() memql.CostEstimate {
	s.mu.Lock()
	defer s.mu.Unlock()
	est := memql.CostEstimate{}
	for _, c := range s.aiCalls {
		est.Tokens += c.PromptTokens + c.OutputTokens
		est.Usd += c.EstimatedCost
	}
	return est
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
