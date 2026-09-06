package memql

// model_journal.go -- the model-call seam (memql#4999).
//
// THIS IS THE SEAM integrations/work/fork.go names. forkRun and replayRun open
// a run whose `mode` says what its journal may serve, and that comment records
// the residual in full: "a run opened in replay mode is a row that says
// `replay` and nothing reads it". This file is what reads it.
//
// THE DECISION IS NOT MADE HERE, and that is a layering decision rather than
// a convenience. integrations/work's own header states the division: "Every
// DECISION is a pure function in component/work... This package is only
// responsible for OBEYING those decisions." An engine that called
// work.DecideServe itself would be a third party to that split, and it would
// cost the whole import graph: component/memql is imported by fifteen modules,
// every one of which would need component/work in its own go.mod to build with
// GOWORK=off. Measured, not guessed -- the module-boundaries lane named all
// fifteen.
//
// So this seam ASKS. It computes the request hash, hands it and the run to the
// journal, and obeys the verdict that comes back. work.DecideServe still has
// exactly one caller, on the other side of that interface, beside the rows it
// reads.
//
// WHAT IS COVERED, and what is not, because the difference decides whether the
// headline claim is true:
//
//	InvokeAI            covered  -- the DSL `ai(...)` form, which is how an
//	                               automation step reaches a model, and
//	                               therefore how a WORK RUN reaches one.
//	InvokeAIStructured  covered  -- structured prompts and the compile pass.
//	chat / suggest / transcription / vision / TTS / the agent tool loop
//	                    NOT covered -- none of them runs inside a work run.
//	                               A journal entry is only meaningful relative
//	                               to a run that can be replayed, and a
//	                               conversational turn has no run to replay.
//
// That is the whole reason the seam keys on the RUN CONTEXT rather than on the
// provider: a call with no run on its context is passed straight through,
// journaled nowhere and served from nowhere. Wrapping the provider registry
// instead would have decorated vision, embedding and speech too, and would
// have had to answer for each of them.

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/znasllc-io/memql/core/common"
)

// JournaledCall is one v1:work:modelCall row, in the shape this package can
// speak without importing the row's writer.
type JournaledCall struct {
	RunId         string
	StepKey       string
	RequestHash   string
	Provider      string
	Model         string
	Settings      map[string]any
	PromptRef     string
	PromptVersion string
	InputTokens   int
	OutputTokens  int
	Cost          float64
	LatencyMs     int
	// Served is one of work-spine's three: "live" (a provider answered),
	// "journal" (a recorded response was replayed), "local" (a fleet model
	// answered).
	Served string
	// Response is the recorded answer. `text` carries the string form, which
	// is what both covered seams return.
	Response map[string]any
	Error    string
}

// Answer is the recorded response as the seam returns it.
//
// TWO KEYS, because the two covered seams differ in kind: a structured prompt
// returns a string and is recorded under "text"; the DSL `ai(...)` form returns
// whatever the provider's Call produced, which may be a map or a list, and is
// recorded under "value". Reading both here keeps that difference inside this
// file rather than at every reader of a journal row.
func (c JournaledCall) Answer() any {
	if c.Response == nil {
		return nil
	}
	if v, ok := c.Response["text"]; ok {
		return v
	}
	return c.Response["value"]
}

// AnswerText is Answer for the callers that require a string.
func (c JournaledCall) AnswerText() string {
	s, _ := c.Answer().(string)
	return s
}

// responseOf records an answer under the key its kind belongs to.
func responseOf(v any) map[string]any {
	if s, ok := v.(string); ok {
		return map[string]any{"text": s}
	}
	return map[string]any{"value": v}
}

// ModelCallVerdict is what the journal answers about one request.
//
// Exactly one of the three states holds: serve the Call, diverge with the
// Reason, or neither -- call the provider. Diverged is checked FIRST by the
// seam, because the underlying decision reports a strict miss as "would have
// gone live, and that is a divergence": reading the serve flag first is how a
// strict replay quietly becomes a live call.
type ModelCallVerdict struct {
	// Call is the journaled answer to serve, or nil.
	Call *JournaledCall
	// Diverged marks a strict replay that could not be served.
	Diverged bool
	// Reason is why, in the journal's own words. Required when Diverged.
	Reason string
	// Err is a failure to ASK -- no journal wired for a run that needs one,
	// or a read that could not be made. Distinct from a miss, because a miss
	// is an ordinary answer and this is not.
	Err error
}

// ModelCallJournal is the seam, implemented in integrations/work.
//
// AN INTERFACE RATHER THAN A DIRECT CALL for two reasons. The row is written
// by an @serverOnly mutation whose stamping discipline lives in
// integrations/work -- one allowlisted package, one stamp site, asserted by
// its own precondition test -- and the engine must not grow a second one nor
// import upward. And the DECISION belongs beside the rows it is made from; see
// the header.
type ModelCallJournal interface {
	// Decide answers what to do with this request for this run: serve a
	// recorded answer, diverge, or neither. A run in live mode reads nothing
	// and answers "call the provider" without touching the database.
	Decide(ctx context.Context, run common.RunContext, requestHash string) ModelCallVerdict
	// Record writes one row. A failure is returned, never swallowed -- see
	// record for what the caller does with it and why.
	Record(ctx context.Context, ownerUserId string, call JournaledCall) error
}

// modelSeam is the shared journal seam.
//
// A VALUE THE ENGINE AND THE ai() RUNTIME BOTH HOLD, rather than a method on
// one of them, because the two covered call sites live on different types --
// InvokeAIStructured on the engine, Invoke on aiRuntime -- and a back-pointer
// from the runtime to the engine would make the seam reachable from everything
// the runtime is reachable from. One value, one pointer, both sides.
type modelSeam struct {
	journal ModelCallJournal
	logger  *slog.Logger
}

// SetModelCallJournal installs the journal. Wired from app/ once the work
// integration exists; nil until then, and nil is a working configuration --
// every call simply runs live and records nothing.
func (e *MemQLEngine) SetModelCallJournal(j ModelCallJournal) {
	if e == nil || e.modelSeam == nil {
		return
	}
	e.modelSeam.journal = j
}

// DivergenceError is a strict replay that could not be served.
//
// IT IS AN ERROR, not a fresh call, and that is the whole of replayPolicy. A
// strict replay exists to prove a recorded run reproduces; silently calling a
// provider when the journal misses would answer "yes it reproduced" having
// reproduced nothing. `permissive` is the documented way to ask for the other
// behaviour.
type DivergenceError struct {
	RunId       string
	StepKey     string
	RequestHash string
	Reason      string
}

// ErrorCodeReplayDiverged is the catalogued code a diverged run records in
// v1:work:run.errorCode.
//
// A CODE RATHER THAN A MESSAGE MATCH, because the surface that reads it is a
// browser: the Nexus run page shows a diverged replay as a divergence and
// everything else as a failure, and keying that on a substring of an English
// sentence would break the day the sentence is reworded. Without it a
// divergence lands as `compile_failed`, which is a plausible label for a run
// whose compile did not fail at all.
const ErrorCodeReplayDiverged = "replay_diverged"

func (e *DivergenceError) Error() string {
	step := e.StepKey
	if step == "" {
		step = "<unnamed step>"
	}
	return fmt.Sprintf("replay diverged at step %s of run %s: %s (requestHash %s)",
		step, e.RunId, e.Reason, e.RequestHash)
}

// modelCallOutcome is what a live call produced.
type modelCallOutcome struct {
	Value any
	Usage common.ChatUsage
	Cost  float64
	// Local marks an answer from a fleet (user-owned) model, which is
	// `served: "local"` rather than `served: "live"` -- MemQL was not billed
	// for it and the scorecard counts it separately.
	Local bool
}

// serveModelCall is the seam.
//
// It decides -- via work.DecideServe and nothing else -- whether this request
// is answered from the journal or by calling through, then journals the result.
// `live` is the call itself, deferred so that a journal hit never makes it.
//
// A call with no run on its context runs live and is not journaled. That is by
// far the common case and it costs one map lookup.
func (s *modelSeam) serve(
	ctx context.Context,
	req common.ModelRequest,
	promptRef string,
	live func(context.Context) (modelCallOutcome, error),
) (any, error) {
	rc, inRun := common.RunFromContext(ctx)
	if s == nil || !inRun {
		out, err := live(ctx)
		return out.Value, err
	}

	hash := req.Hash()
	verdict := s.decide(ctx, rc, hash)

	// FAILING TO ASK IS NOT A MISS. With no journal wired, every lookup
	// misses, and a strict replay reported the miss's reason -- "the prompt,
	// the model or the settings changed since the recorded run". Confident,
	// checkable, and wrong: nothing changed, the node has no journal. The
	// decision cannot tell those apart, because both reach it as a miss, so
	// they are told apart before it.
	if verdict.Err != nil {
		return nil, verdict.Err
	}

	// DIVERGENCE IS CHECKED BEFORE THE ANSWER. A strict miss is reported as
	// "would have gone live, and that is a divergence" -- reading the served
	// call first and the flag second is how a strict replay quietly becomes a
	// live call.
	if verdict.Diverged {
		return nil, &DivergenceError{
			RunId:       rc.RunId,
			StepKey:     rc.StepKey,
			RequestHash: hash,
			Reason:      verdict.Reason,
		}
	}

	if journaled := verdict.Call; journaled != nil {
		// The replay run gets a row of its OWN, marked `journal`. Without it
		// a replayed run's journal is empty and the two runs cannot be
		// compared -- which is most of what a replay is for.
		s.record(ctx, rc, JournaledCall{
			RunId:         rc.RunId,
			StepKey:       rc.StepKey,
			RequestHash:   hash,
			Provider:      journaled.Provider,
			Model:         journaled.Model,
			Settings:      req.Settings,
			PromptRef:     promptRef,
			PromptVersion: journaled.PromptVersion,
			InputTokens:   journaled.InputTokens,
			OutputTokens:  journaled.OutputTokens,
			// Cost is ZERO, not the recorded cost. Nothing was billed for
			// this call, and copying the original's cost would make a replay
			// look exactly as expensive as the run it is proving was free.
			Cost:     0,
			Served:   "journal",
			Response: journaled.Response,
		})
		return journaled.Answer(), nil
	}

	started := time.Now()
	out, err := live(ctx)
	served := "live"
	if out.Local {
		served = "local"
	}
	call := JournaledCall{
		RunId:        rc.RunId,
		StepKey:      rc.StepKey,
		RequestHash:  hash,
		Provider:     req.Provider,
		Model:        req.Model,
		Settings:     req.Settings,
		PromptRef:    promptRef,
		InputTokens:  int(out.Usage.InputTokens),
		OutputTokens: int(out.Usage.OutputTokens),
		Cost:         out.Cost,
		LatencyMs:    int(time.Since(started).Milliseconds()),
		Served:       served,
	}
	if err != nil {
		// A FAILED CALL IS JOURNALED TOO. A replay whose journal skips the
		// failures reproduces a run that never happened, and "the third
		// attempt is where it broke" is exactly the question a journal is
		// read to answer.
		call.Error = err.Error()
		s.record(ctx, rc, call)
		return nil, err
	}
	call.Response = responseOf(out.Value)
	s.record(ctx, rc, call)
	return out.Value, nil
}

// decide asks the journal, and answers for it when there is none.
//
// A run in LIVE mode is not asked at all: it reads no journal by definition,
// and asking would cost a database round trip per model call for an answer
// that is fixed. A run that DOES need one and has no journal wired is an
// error rather than a miss -- see serve.
func (s *modelSeam) decide(ctx context.Context, rc common.RunContext, hash string) ModelCallVerdict {
	if strings.TrimSpace(rc.SourceRunId) == "" {
		return ModelCallVerdict{}
	}
	if s.journal == nil {
		return ModelCallVerdict{Err: fmt.Errorf(
			"run %s is in %s mode against run %s, but this node has no model-call journal wired, so nothing can be served from it",
			rc.RunId, rc.Mode, rc.SourceRunId)}
	}
	return s.journal.Decide(ctx, rc, hash)
}

// serveText is serve for the callers whose answer is always a string.
func (s *modelSeam) serveText(
	ctx context.Context,
	req common.ModelRequest,
	promptRef string,
	live func(context.Context) (modelCallOutcome, error),
) (string, error) {
	v, err := s.serve(ctx, req, promptRef, live)
	if err != nil {
		return "", err
	}
	text, _ := v.(string)
	return text, nil
}

// recordCall writes one row, and LOGS a failure rather than returning it.
//
// The journal is a record of work, not a precondition for it. A run whose
// journal write fails has still done the work and still has an answer for its
// caller; failing the step would turn an observability outage into a product
// outage. It is logged at WARN with the run and step, which is what makes a
// silently empty journal findable -- the failure mode this project has been
// bitten by twice.
func (s *modelSeam) record(ctx context.Context, rc common.RunContext, call JournaledCall) {
	if s == nil || s.journal == nil {
		return
	}
	if err := s.journal.Record(ctx, rc.OwnerUserId, call); err != nil && s.logger != nil {
		s.logger.Warn("work: journaling a model call failed; the run continues and its journal is incomplete",
			"runId", rc.RunId, "stepKey", rc.StepKey, "served", call.Served, "error", err)
	}
}

// callStructuredWithUsage calls a structured provider, preferring the variant
// that reports token usage.
//
// ChatStructuredUsageProvider is the ONLY interface in the provider set that
// carries real, provider-reported usage; everything else leaves the journal's
// token columns at zero. ChatUsage.Reported exists to say "nobody measured
// this", and a zero with Reported=false is a different answer from a measured
// zero -- which is why the fallback does not invent an estimate here.
func callStructuredWithUsage(
	ctx context.Context,
	p common.ChatStructuredProvider,
	messages []common.ChatMessage,
	spec common.StructuredSchema,
) (string, common.ChatUsage, error) {
	if withUsage, ok := p.(common.ChatStructuredUsageProvider); ok {
		return withUsage.CallChatStructuredWithUsage(ctx, messages, spec)
	}
	text, err := p.CallChatStructured(ctx, messages, spec)
	return text, common.ChatUsage{}, err
}

// providerModel is the model id a provider name resolves to, or "" when the
// name resolves to nothing.
func (e *MemQLEngine) providerModel(name string) string {
	if e == nil {
		return ""
	}
	entry, ok := e.ProviderEntry(name)
	if !ok || entry == nil {
		return ""
	}
	return entry.Config.Model
}

// costOnlyParams are the provider params that do NOT affect the answer.
//
// A DENYLIST RATHER THAN AN ALLOWLIST, and the direction is the decision. An
// allowlist that misses a new answer-affecting parameter lets a replay serve
// the wrong answer, silently. A denylist that misses a new cost field
// invalidates journal entries and turns replays into misses -- visible, and
// recoverable by re-running. Between a silent wrong answer and a loud miss,
// this fails toward the miss.
var costOnlyParams = map[string]bool{
	"inputCostPerMillion":  true,
	"outputCostPerMillion": true,
	"contextWindow":        true,
}

// answerAffectingSettings is a provider's params with the ones that only
// describe billing and capacity removed.
func (e *MemQLEngine) answerAffectingSettings(name string) map[string]any {
	if e == nil {
		return nil
	}
	entry, ok := e.ProviderEntry(name)
	if !ok || entry == nil {
		return nil
	}
	return answerAffectingParams(entry.Config.Params)
}

// answerAffectingParams is answerAffectingSettings for a params map already in
// hand, so a caller that resolved the provider entry does not resolve it twice.
func answerAffectingParams(params map[string]any) map[string]any {
	if len(params) == 0 {
		return nil
	}
	out := make(map[string]any, len(params))
	for k, v := range params {
		if costOnlyParams[k] {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
