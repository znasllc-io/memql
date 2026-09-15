package steps

// sandbox_registry.go -- the side-effect interception layer for the Gate-2
// behavioral dry-run sandbox (issue #958).
//
// sandboxStepRegistry wraps the real step Registry and satisfies
// automations.StepExecutorRegistry, so the automation Executor drives it exactly
// like the production registry. For each step it decides, by TIER:
//
//   - WRITE-BEARING (a `mutation` call): the would-be write is evaluated +
//     recorded into the manifest under the run's ephemeral sandbox partition
//     and a synthetic success result is returned. The write NEVER reaches
//     engine.Execute, so no row lands in the live graph. Later statements that
//     read its name still see a success result.
//   - SIDE EFFECTS (a publish, an action, a sub-automation): recorded, and a
//     synthetic success returned.
//   - A LOGIC call runs its statements through this same registry, so every
//     write in it is intercepted too.
//   - READ / pure compute: queries and explicitly classified builtins are
//     metered and delegated to the real executor. Unclassified builtins,
//     including integration web reads, are refused before dispatch.

import (
	"context"
	"encoding/json"
	"fmt"
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

	mu        sync.Mutex
	mutations []memql.RecordedMutation
	aiCalls   []memql.RecordedAiCall
	webCalls  []memql.RecordedWebCall
	// intercepted records, per step id, the side-effect-layer annotation so the
	// trace can mark which steps were rewritten/blocked vs ran for real.
	intercepted map[string]string
}

// newSandboxStepRegistry builds a sandbox registry wrapping the real one.
func newSandboxStepRegistry(real *Registry, engine *memql.MemQLEngine, partition string) *sandboxStepRegistry {
	return &sandboxStepRegistry{
		real:        real,
		engine:      engine,
		partition:   partition,
		intercepted: map[string]string{},
	}
}

// Execute routes one step through the interception tiers. It satisfies
// automations.StepExecutorRegistry.
func (s *sandboxStepRegistry) Execute(ctx context.Context, step *automations.Step, stepCtx *automations.StepContext) (*automations.StepResult, error) {
	ctx = memql.WithBuiltinPreview(ctx)
	switch step.Type {
	case automations.StepTypeFunction:
		switch s.functionKind(step) {
		case "mutation":
			return s.interceptMutationFunction(step, stepCtx)
		case "logic":
			// A logic's statements may write. Run them through the SAME
			// sandbox registry (via a sandbox LogicRunner) so those nested side
			// effects are intercepted too, instead of escaping to
			// engine.Execute.
			return s.interceptLogicFunction(ctx, step, stepCtx)
		case "builtin":
			executor := ""
			if s.engine != nil && step.Function != nil {
				if fn, ok := s.engine.Functions().Lookup(step.Function.Name); ok && fn != nil {
					executor = fn.Executor
				}
			}
			if err := memql.CheckBuiltinPreview(executor); err != nil {
				s.note(step.ID, err.Error())
				now := time.Now()
				return &automations.StepResult{
					StepId: step.ID, Status: "failed", Error: err.Error(),
					StartedAt: now, CompletedAt: now,
				}, err
			}
			s.meterRead(step, stepCtx)
			return s.real.Execute(ctx, step, stepCtx)
		default:
			// A read (ai() / similarTo / webSearch / fetchUrl) or a plain
			// query: meter the read into the manifest (real + metered), then
			// delegate to the real executor so it runs for real.
			s.meterRead(step, stepCtx)
			return s.real.Execute(ctx, step, stepCtx)
		}
	// Write-bearing step types that used to fall through to the production
	// executors (memql#2943). Each reaches a real side effect:
	//   event      -> stepCtx.EventBus.Publish, on the LIVE bus
	//   action     -> engine.ExecuteToolByName, a real capability call
	//   automation -> triggers another automation, unbounded
	case automations.StepTypeEvent,
		automations.StepTypeAction,
		automations.StepTypeAutomation:
		return s.interceptSideEffect(step, stepCtx)

	// Containers. Delegating these to the real registry is what let a nested
	// write escape (memql#2943): a nested step must re-enter this switch. A
	// parallel resolves its branches with Dispatch pointed back at Execute
	// (child_dispatch.go); a `for` and a branch run their lists through the
	// sequence runner, whose registry is this one.
	case automations.StepTypeForEach:
		// A `for` runs its list through the executor's sequence runner, whose
		// registry is this one, so every statement in it -- to any depth --
		// re-enters this switch.
		return (&ForEachExecutor{}).Execute(ctx, step, stepCtx)
	case automations.StepTypeParallel:
		return (&ParallelExecutor{Registry: s.real, Dispatch: s.Execute}).Execute(ctx, step, stepCtx)
	case automations.StepTypeBlock:
		// A parallel's branch. Its list runs through the executor's sequence
		// runner, whose registry is this one, so every statement in it -- to
		// any depth -- re-enters this switch. (A `for` does the same through
		// the ForEachExecutor above.)
		return (&BlockExecutor{}).Execute(ctx, step, stepCtx)

	default:
		// FAIL CLOSED. This arm used to forward every unclassified step to the
		// production executors, which is how event and action steps reached
		// the live graph while dryrun.go promised "zero rows land in the live
		// graph".
		//
		// Refusing is the conservative answer for a preview whose OUTPUT is an
		// approval artifact: an operator reading the manifest has to be able to
		// treat it as complete. A step type nobody has classified might write,
		// and a manifest that silently omits a write is worse than a preview
		// that declines to run one. A newly added step type therefore fails
		// loudly here until it is classified above.
		return s.refuseUnclassified(step)
	}
}

// interceptSideEffect records a write-bearing step into the manifest and
// returns a synthetic success without letting it reach the real executor, so
// later steps that reference this one still see a result.
func (s *sandboxStepRegistry) interceptSideEffect(step *automations.Step, stepCtx *automations.StepContext) (*automations.StepResult, error) {
	started := time.Now()
	s.recordMutation(memql.RecordedMutation{
		StepId:    step.ID,
		Concept:   sandboxConceptForStep(step),
		Partition: s.partition,
	})
	s.note(step.ID, string(step.Type)+" step intercepted: side effect recorded, not performed")
	return s.syntheticSuccess(step.ID, started, map[string]any{
		"dryRun":      true,
		"intercepted": true,
		"stepType":    string(step.Type),
	}), nil
}

// refuseUnclassified fails a step whose type has no sandbox disposition.
func (s *sandboxStepRegistry) refuseUnclassified(step *automations.Step) (*automations.StepResult, error) {
	s.note(step.ID, "step type "+string(step.Type)+" has no sandbox classification; refused")
	return nil, fmt.Errorf(
		"dry-run refused step %q: step type %q has no sandbox classification, so it "+
			"cannot be shown to be side-effect free. Classify it in "+
			"sandboxStepRegistry.Execute (component/automations/steps/sandbox_registry.go) "+
			"before authoring it into a previewable automation (memql#2943)",
		step.ID, step.Type)
}

// sandboxConceptForStep is a best-effort label for the manifest entry.
func sandboxConceptForStep(step *automations.Step) string {
	return string(step.Type)
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
//
// A statement names its callee's kind itself (`mutation advance(...)`, epic
// memql#5370), so a statement that says mutation is intercepted as one even
// where the registry cannot answer; otherwise the registry decides, and the
// statement's word stands in for an unknown name.
func (s *sandboxStepRegistry) functionKind(step *automations.Step) string {
	if step.Function == nil {
		return ""
	}
	said := strings.ToLower(strings.TrimSpace(step.Function.Kind))
	if said == "mutation" {
		return said
	}
	name := strings.TrimSpace(step.Function.Name)
	if s.engine != nil && name != "" {
		if fn, ok := s.engine.Functions().Lookup(name); ok && fn != nil {
			return strings.ToLower(strings.TrimSpace(fn.FunctionKind))
		}
	}
	return said
}

// interceptLogicFunction runs a logic's statements through a sandbox
// LogicRunner that shares THIS registry, so every write in them is intercepted
// recursively. The sandbox's runner journals nothing -- a preview leaves no
// run, whatever the logic does.
func (s *sandboxStepRegistry) interceptLogicFunction(ctx context.Context, step *automations.Step, stepCtx *automations.StepContext) (*automations.StepResult, error) {
	name := strings.TrimSpace(step.Function.Name)
	fn, ok := s.engine.Functions().Lookup(name)
	if !ok || fn == nil || fn.LogicBody == nil {
		// An unknown name: the real executor surfaces the error.
		return s.real.Execute(ctx, step, stepCtx)
	}

	started := time.Now()
	s.note(step.ID, "logic "+name+" run in sandbox (nested side effects intercepted)")
	args := s.stepCallArgs(step, stepCtx)
	runner := automations.NewLogicRunner(s.engine, s, nil).WithoutJournal()
	out, err := runner.RunLogicBody(ctx, name, fn.LogicBody, args)
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
	resolved := s.stepCallArgs(step, stepCtx)
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

// stepCallArgs resolves a function step's arguments for the sandbox through
// the same evaluation the real FunctionExecutor runs (memql#5367) -- values,
// never reference text -- so a logic dispatched in the dry-run sandbox
// receives the args it would on the live path, the triggering event's
// envelope under its declared `event` input included (memql#1727). An
// argument that fails to evaluate leaves the map unresolved rather than
// half-resolved.
func (s *sandboxStepRegistry) stepCallArgs(step *automations.Step, stepCtx *automations.StepContext) map[string]any {
	fn := step.Function
	if fn == nil || len(fn.Args) == 0 || stepCtx == nil || stepCtx.Evaluator == nil {
		return map[string]any{}
	}
	resolved, err := stepCtx.Evaluator.ResolveV1Map(context.Background(), fn.Args)
	if err != nil {
		return fn.Args
	}
	return resolved
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
	resolved := s.stepCallArgs(step, stepCtx)
	promptTokens := estimateTokens(resolved)
	outputTokens := defaultAiOutputTokens
	cost := estimateAiCostUsd(promptTokens, outputTokens)
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
	resolved := s.stepCallArgs(step, stepCtx)
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
	// defaultAiOutputTokens is the assumed completion size for one ai() call
	// when actual usage is unknown.
	defaultAiOutputTokens = 512
	// aiInputUsdPerMillion / aiOutputUsdPerMillion are conservative default
	// rates (a mid-tier chat model) for the estimate. They are intentionally a
	// fixed heuristic, not a per-provider lookup -- the dry-run cannot know
	// which provider a given ai() template will resolve to at run time.
	aiInputUsdPerMillion  = 0.50
	aiOutputUsdPerMillion = 1.50
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

// estimateAiCostUsd applies the default per-million rates to an estimated
// prompt/output token split.
func estimateAiCostUsd(promptTokens, outputTokens int) float64 {
	return aiInputUsdPerMillion*float64(promptTokens)/1_000_000 +
		aiOutputUsdPerMillion*float64(outputTokens)/1_000_000
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
		Mutations: s.mutations,
		AiCalls:   s.aiCalls,
		WebCalls:  s.webCalls,
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
