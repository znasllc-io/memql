package memql

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// inner_loop.go holds the hardening primitives for the bounded single-turn
// tool loop (si_tool_loop.go) and the streaming replier
// (integrations/agent/streaming.go). Everything here is deliberately pure
// and table-testable -- no DB, no live LLM, no engine receiver -- so the
// loop's stopping logic, budget accounting, error classification, retry
// policy, and idempotency book-keeping can be unit-tested in isolation.
//
// Part of issue #584 (inner-loop hardening). The harness types from #582
// (observations) are consumed behind the ToolLoopObservationSink interface
// so this code builds standalone while #582 is in flight.

// ---------------------------------------------------------------------------
// Structured tool errors
// ---------------------------------------------------------------------------

// ToolErrorType classifies a tool failure so the model -- and the loop --
// can reason about it rather than parsing a raw string. The split that
// matters most is user-fixable vs. system: a user-fixable error (bad
// argument, validation failure) is worth feeding back so the model can
// retry with corrected args; a system error (timeout, provider down) is
// retryable mechanically but not by changing the call shape.
type ToolErrorType string

const (
	// ToolErrorValidation -- the call shape was wrong (bad/missing args,
	// schema mismatch). User/model-fixable: retry with corrected args.
	ToolErrorValidation ToolErrorType = "validation"
	// ToolErrorNotFound -- the named tool (or a resource it needs) does
	// not exist. Model-fixable only by choosing a different tool.
	ToolErrorNotFound ToolErrorType = "not_found"
	// ToolErrorPermission -- the caller is not allowed to run this tool
	// (role gate, scope gate). Not retryable without an out-of-band grant.
	ToolErrorPermission ToolErrorType = "permission"
	// ToolErrorTimeout -- the tool exceeded its per-call deadline. Retryable.
	ToolErrorTimeout ToolErrorType = "timeout"
	// ToolErrorUnavailable -- a downstream dependency was transiently
	// unavailable (provider overloaded, rate limited, 5xx). Retryable.
	ToolErrorUnavailable ToolErrorType = "unavailable"
	// ToolErrorInternal -- an unexpected system error. Retryable once or
	// twice, but not user-fixable.
	ToolErrorInternal ToolErrorType = "internal"
)

// userFixableTypes are the error classes the MODEL can recover from by
// changing what it sends -- as opposed to system classes that only a
// mechanical retry (or operator intervention) can clear.
var userFixableTypes = map[ToolErrorType]bool{
	ToolErrorValidation: true,
	ToolErrorNotFound:   true,
}

// StructuredToolError is the typed, machine-actionable error surface fed
// back into the model when a tool fails. It serialises to a compact JSON
// object `{type, message, retryable, userFixable, attempts?, details?}`
// that the model can branch on. This replaces the prior raw-string
// `{"error": "..."}` feedback in the tool loop.
type StructuredToolError struct {
	// Type is the high-level classification (see ToolErrorType).
	Type ToolErrorType `json:"type"`
	// Message is the human/model-readable description.
	Message string `json:"message"`
	// Retryable signals whether re-dispatching the SAME call could
	// plausibly succeed (transient/system errors) -- distinct from
	// UserFixable, which says the model should change the call.
	Retryable bool `json:"retryable"`
	// UserFixable signals the model can recover by correcting its args
	// or choosing a different tool within the same loop.
	UserFixable bool `json:"userFixable"`
	// Attempts is the number of execution attempts made before this error
	// was surfaced (1 = no retry happened). Surfaced as state so the model
	// knows we already tried.
	Attempts int `json:"attempts,omitempty"`
	// Details carries optional structured context (e.g. allowed enum
	// values for a validation error).
	Details map[string]any `json:"details,omitempty"`
}

// Error implements the error interface so a StructuredToolError can flow
// through normal error plumbing when needed.
func (e *StructuredToolError) Error() string {
	if e == nil {
		return ""
	}
	return string(e.Type) + ": " + e.Message
}

// JSON renders the structured error as the compact object the model sees
// in the tool_result message. Never returns an empty string -- a marshal
// failure falls back to a minimal hand-built object.
func (e *StructuredToolError) JSON() string {
	if e == nil {
		return `{"type":"internal","message":"nil error","retryable":false,"userFixable":false}`
	}
	b, err := json.Marshal(e)
	if err != nil {
		return fmt.Sprintf(`{"type":%q,"message":%q,"retryable":%t,"userFixable":%t}`,
			string(e.Type), e.Message, e.Retryable, e.UserFixable)
	}
	return string(b)
}

// ClassifyToolError maps a raw error from tool dispatch into a
// StructuredToolError. The classification is substring-based against the
// error text because the dispatch path wraps a heterogeneous set of
// underlying errors (engine StructuredError, fmt.Errorf strings, provider
// errors) and we don't want to plumb a typed surface through every tool
// handler. A nil error returns nil.
func ClassifyToolError(err error) *StructuredToolError {
	if err == nil {
		return nil
	}
	// If the engine already produced a StructuredError, map its code
	// directly -- it's the most authoritative signal we have.
	if se, ok := asStructuredError(err); ok {
		t := toolErrorTypeForCode(se.Code)
		return newStructuredToolError(t, se.Message, detailsFromStructured(se))
	}
	msg := err.Error()
	t := classifyByText(msg)
	return newStructuredToolError(t, msg, nil)
}

// asStructuredError unwraps an engine *StructuredError if present.
func asStructuredError(err error) (*StructuredError, bool) {
	for e := err; e != nil; {
		if se, ok := e.(*StructuredError); ok {
			return se, true
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			break
		}
		e = u.Unwrap()
	}
	return nil, false
}

func detailsFromStructured(se *StructuredError) map[string]any {
	if se == nil || len(se.Details) == 0 {
		return nil
	}
	return se.Details
}

// toolErrorTypeForCode maps an engine ErrorCode to a ToolErrorType.
func toolErrorTypeForCode(code ErrorCode) ToolErrorType {
	switch code {
	case ErrorCodeValidationFailed, ErrorCodeMissingRequiredFields,
		ErrorCodeInvalidFieldType, ErrorCodeInvalidArgument,
		ErrorCodeInvalidOperator, ErrorCodeSyntaxError:
		return ToolErrorValidation
	case ErrorCodeUnknownConcept, ErrorCodeUnknownFunction,
		ErrorCodeNotFound, ErrorCodeRelationshipNotFound:
		return ToolErrorNotFound
	default:
		return ToolErrorInternal
	}
}

// classifyByText buckets a raw error message into a ToolErrorType using
// substring markers. Order matters: more specific markers win.
func classifyByText(msg string) ToolErrorType {
	m := strings.ToLower(msg)
	switch {
	// Validation first: schema messages like "additional property X not
	// allowed" contain "not allowed" but are user-fixable arg errors, not
	// permission errors -- classify them before the permission bucket.
	case containsAny(m, "additional property", "invalid argument", "validation",
		"required field", "missing required", "must be one of", "invalid field",
		"not allowed:"):
		return ToolErrorValidation
	case containsAny(m, "permission denied", "not authorized", "is not allowed for",
		"forbidden", "agent-only", "scope_elevation", "out of scope"):
		return ToolErrorPermission
	case containsAny(m, "not found", "unknown tool", "no handler",
		"does not exist", "unknown function", "unknown concept"):
		return ToolErrorNotFound
	case containsAny(m, "timeout", "deadline exceeded", "context deadline",
		"timed out"):
		return ToolErrorTimeout
	case containsAny(m, "overloaded", "rate limit", "rate_limit",
		"service unavailable", "503", "529", "connection reset",
		"temporarily unavailable"):
		return ToolErrorUnavailable
	default:
		return ToolErrorInternal
	}
}

func containsAny(haystack string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}

// newStructuredToolError builds a StructuredToolError with Retryable /
// UserFixable derived from the type so callers never set them
// inconsistently.
func newStructuredToolError(t ToolErrorType, msg string, details map[string]any) *StructuredToolError {
	return &StructuredToolError{
		Type:        t,
		Message:     msg,
		Retryable:   isRetryableToolErrorType(t),
		UserFixable: userFixableTypes[t],
		Attempts:    1,
		Details:     details,
	}
}

// isRetryableToolErrorType reports whether a mechanical retry of the same
// call could plausibly succeed.
func isRetryableToolErrorType(t ToolErrorType) bool {
	switch t {
	case ToolErrorTimeout, ToolErrorUnavailable, ToolErrorInternal:
		return true
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// Deterministic stopping: loop detection + terminal reasons
// ---------------------------------------------------------------------------

// StopReason names why the inner loop terminated. Surfaced in logs (and,
// when the observation sink is wired, as a note observation) so a stalled
// turn is never a silent mystery.
type StopReason string

const (
	// StopRespondToUser -- the model emitted the terminal envelope. Normal
	// completion for the chat path.
	StopRespondToUser StopReason = "respondToUser"
	// StopTextOnly -- the model returned text with no tool calls. Normal
	// completion for non-envelope paths.
	StopTextOnly StopReason = "text_only"
	// StopBudgetExhausted -- the context/iteration budget ran out.
	StopBudgetExhausted StopReason = "budget_exhausted"
	// StopLoopDetected -- the model repeated an identical tool call beyond
	// the threshold (loop detection tripped).
	StopLoopDetected StopReason = "loop_detected"
	// StopHardError -- a non-retryable system error ended the turn.
	StopHardError StopReason = "hard_error"
	// StopMaxIterations -- the iteration cap was hit.
	StopMaxIterations StopReason = "max_iterations"
)

// LoopDetector tracks repeated identical tool calls so the loop can stop
// deterministically instead of spinning forever on the same failing call.
// A "call signature" is toolName + canonicalized-args. When the same
// signature recurs threshold times in a row, the loop trips.
//
// Zero value is NOT ready -- use NewLoopDetector so the threshold is sane.
type LoopDetector struct {
	threshold int
	last      string
	repeats   int
	// seen counts total occurrences per signature across the whole turn,
	// used to detect non-consecutive thrashing (A,B,A,B,A,B...).
	seen           map[string]int
	totalThreshold int
}

// defaultLoopRepeatThreshold is how many CONSECUTIVE identical calls trip
// the detector. 3 gives the model two chances to self-correct (e.g. a
// validation error whose message names the fix) before we call it a loop.
const defaultLoopRepeatThreshold = 3

// defaultLoopTotalThreshold is how many times the SAME signature may
// appear across the whole turn (non-consecutively) before we trip. Catches
// A/B thrashing that the consecutive counter misses.
const defaultLoopTotalThreshold = 5

// NewLoopDetector returns a detector with the given consecutive-repeat
// threshold. Non-positive threshold falls back to the default.
func NewLoopDetector(threshold int) *LoopDetector {
	if threshold <= 0 {
		threshold = defaultLoopRepeatThreshold
	}
	return &LoopDetector{
		threshold:      threshold,
		seen:           make(map[string]int),
		totalThreshold: defaultLoopTotalThreshold,
	}
}

// ToolCallSignature canonicalizes a tool call into a stable string so two
// calls with semantically identical args (key order aside) compare equal.
// rawArgs is parsed as JSON when possible and re-marshalled with sorted
// keys; on parse failure the trimmed raw string is used.
func ToolCallSignature(toolName, rawArgs string) string {
	name := strings.TrimSpace(toolName)
	canon := canonicalizeArgs(rawArgs)
	return name + "\x00" + canon
}

// canonicalizeArgs returns a key-order-independent encoding of a JSON
// object string. Falls back to the trimmed input on parse failure.
func canonicalizeArgs(rawArgs string) string {
	trimmed := strings.TrimSpace(rawArgs)
	if trimmed == "" {
		return ""
	}
	var v any
	if err := json.Unmarshal([]byte(trimmed), &v); err != nil {
		return trimmed
	}
	// json.Marshal sorts map keys, giving a stable encoding.
	b, err := json.Marshal(v)
	if err != nil {
		return trimmed
	}
	return string(b)
}

// Observe records a tool call signature and reports whether the loop
// should stop. It trips when either the same signature recurs threshold
// times consecutively, or appears totalThreshold times across the turn.
func (d *LoopDetector) Observe(signature string) (trip bool) {
	if d == nil {
		return false
	}
	if d.seen == nil {
		d.seen = make(map[string]int)
	}
	d.seen[signature]++
	if d.seen[signature] >= d.totalThreshold {
		return true
	}
	if signature == d.last {
		d.repeats++
	} else {
		d.last = signature
		d.repeats = 1
	}
	return d.repeats >= d.threshold
}

// ---------------------------------------------------------------------------
// Context-budget manager
// ---------------------------------------------------------------------------

// ContextBudget tracks an approximate token budget for the conversation
// window. The loop calls Exceeded after appending each turn; when the
// estimated size of retrieved-context + history crosses MaxTokens, the
// caller summarizes/trims the oldest turns and records that as an
// observation of kind note (when the sink is wired).
//
// Token counting is approximate -- charsPerToken chars ~= 1 token -- which
// is good enough for a trim trigger and avoids a tokenizer dependency.
type ContextBudget struct {
	// MaxTokens is the ceiling. Non-positive disables the budget (Exceeded
	// always returns false).
	MaxTokens int
	// CharsPerToken is the divisor for the char->token estimate.
	CharsPerToken int
}

// defaultCharsPerToken is a conservative average across English prose +
// JSON tool payloads (~4 chars/token for prose, denser for JSON).
const defaultCharsPerToken = 4

// NewContextBudget returns a budget with a sane CharsPerToken default.
func NewContextBudget(maxTokens int) ContextBudget {
	return ContextBudget{MaxTokens: maxTokens, CharsPerToken: defaultCharsPerToken}
}

// EstimateTokens returns an approximate token count for a single string.
func (b ContextBudget) EstimateTokens(s string) int {
	cpt := b.CharsPerToken
	if cpt <= 0 {
		cpt = defaultCharsPerToken
	}
	if len(s) == 0 {
		return 0
	}
	return (len(s) + cpt - 1) / cpt
}

// Exceeded reports whether the combined estimated token count of the
// supplied message contents exceeds MaxTokens. A non-positive MaxTokens
// disables the check.
func (b ContextBudget) Exceeded(contents ...string) bool {
	if b.MaxTokens <= 0 {
		return false
	}
	total := 0
	for _, c := range contents {
		total += b.EstimateTokens(c)
	}
	return total > b.MaxTokens
}

// TrimPlan describes how many leading (oldest) message slots to drop and a
// human-readable summary line to insert in their place. Pure data so the
// loop can apply it however its message representation requires.
type TrimPlan struct {
	// DropCount is the number of oldest non-pinned messages to remove.
	DropCount int
	// Summary is the note text recorded in place of the dropped turns.
	Summary string
}

// PlanContextTrim decides how to trim a conversation window that exceeds
// budget. It always preserves the first `pinnedHead` messages (the system
// prompt and any pinned context) and the last `keepTail` messages (the
// recent turns that carry the live thread). Everything between is a
// candidate for trimming; the plan drops as many of the oldest candidates
// as needed to bring the estimate back under budget.
//
// sizes is the per-message estimated token count, index-aligned with the
// caller's message slice. Returns a zero-DropCount plan when nothing can
// or needs to be trimmed.
func PlanContextTrim(sizes []int, maxTokens, pinnedHead, keepTail int) TrimPlan {
	if maxTokens <= 0 || len(sizes) == 0 {
		return TrimPlan{}
	}
	if pinnedHead < 0 {
		pinnedHead = 0
	}
	if keepTail < 0 {
		keepTail = 0
	}
	total := 0
	for _, s := range sizes {
		total += s
	}
	if total <= maxTokens {
		return TrimPlan{}
	}
	// Candidate window is [pinnedHead, len-keepTail).
	candStart := pinnedHead
	candEnd := len(sizes) - keepTail
	if candEnd <= candStart {
		return TrimPlan{} // nothing trimmable without touching pinned/tail
	}
	drop := 0
	freed := 0
	for i := candStart; i < candEnd && total-freed > maxTokens; i++ {
		freed += sizes[i]
		drop++
	}
	if drop == 0 {
		return TrimPlan{}
	}
	return TrimPlan{
		DropCount: drop,
		Summary: fmt.Sprintf("[context trimmed: %d earlier turn(s) summarized to stay within the %d-token window]",
			drop, maxTokens),
	}
}

// ---------------------------------------------------------------------------
// Per-tool timeout + retry policy
// ---------------------------------------------------------------------------

// ToolRetryPolicy governs per-tool timeout + retry-with-backoff. Surfacing
// the attempt count back as state (on StructuredToolError.Attempts) lets
// the model see we already tried.
type ToolRetryPolicy struct {
	// PerCallTimeout bounds a single tool execution attempt. Non-positive
	// disables the per-call deadline.
	PerCallTimeout time.Duration
	// MaxAttempts is the total number of attempts (1 = no retry).
	MaxAttempts int
	// BaseBackoff is the first retry's delay; subsequent retries grow
	// exponentially up to MaxBackoff.
	BaseBackoff time.Duration
	// MaxBackoff caps the exponential growth.
	MaxBackoff time.Duration
}

// DefaultToolRetryPolicy returns the loop's default per-tool policy:
// a 30s per-call deadline, up to 3 attempts, 250ms base backoff capped at 4s.
func DefaultToolRetryPolicy() ToolRetryPolicy {
	return ToolRetryPolicy{
		PerCallTimeout: 30 * time.Second,
		MaxAttempts:    3,
		BaseBackoff:    250 * time.Millisecond,
		MaxBackoff:     4 * time.Second,
	}
}

// Backoff returns the delay before attempt N (1-indexed; attempt 1 has no
// preceding delay so returns 0). Exponential, capped at MaxBackoff.
func (p ToolRetryPolicy) Backoff(attempt int) time.Duration {
	if attempt <= 1 {
		return 0
	}
	if p.BaseBackoff <= 0 {
		return 0
	}
	d := p.BaseBackoff << (attempt - 2)
	if p.MaxBackoff > 0 && d > p.MaxBackoff {
		return p.MaxBackoff
	}
	return d
}

// toolExecFn is the unit of work the retry loop drives. It returns the
// result string and an error; the error is classified to decide whether a
// retry is warranted.
type toolExecFn func(ctx context.Context) (string, error)

// ExecuteWithRetry runs fn under the policy: each attempt is wrapped in a
// per-call timeout context, transient/retryable failures back off and
// retry, and the final structured error (if any) carries the attempt
// count. On success it returns (result, nil, attempts). On terminal
// failure it returns ("", structuredErr, attempts).
func (p ToolRetryPolicy) ExecuteWithRetry(ctx context.Context, fn toolExecFn) (string, *StructuredToolError, int) {
	attempts := 0
	maxAttempts := p.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	var lastErr *StructuredToolError
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		attempts = attempt
		if attempt > 1 {
			if backoff := p.Backoff(attempt); backoff > 0 {
				select {
				case <-ctx.Done():
					se := newStructuredToolError(ToolErrorTimeout, ctx.Err().Error(), nil)
					se.Attempts = attempts
					return "", se, attempts
				case <-time.After(backoff):
				}
			}
		}

		callCtx := ctx
		var cancel context.CancelFunc
		if p.PerCallTimeout > 0 {
			callCtx, cancel = context.WithTimeout(ctx, p.PerCallTimeout)
		}
		result, err := fn(callCtx)
		if cancel != nil {
			cancel()
		}
		if err == nil {
			return result, nil, attempts
		}
		lastErr = ClassifyToolError(err)
		lastErr.Attempts = attempts
		// Only retry mechanically-retryable system errors. A user-fixable
		// error (validation/not-found) is returned immediately so the
		// model can correct -- retrying the identical call is pointless.
		if !lastErr.Retryable || attempt == maxAttempts {
			return "", lastErr, attempts
		}
	}
	return "", lastErr, attempts
}

// ---------------------------------------------------------------------------
// Tool-set scoping
// ---------------------------------------------------------------------------

// ScopeToolNames filters a tool-name list down to an allow-set. Out-of-scope
// names are silently dropped. A nil/empty scope returns the input unchanged
// (no scoping requested) -- this is the hook #588 will drive with
// role-derived tool subsets. Order of `names` is preserved; duplicates in
// the input are preserved.
func ScopeToolNames(names []string, scope []string) []string {
	if len(scope) == 0 {
		return names
	}
	allow := make(map[string]bool, len(scope))
	for _, n := range scope {
		n = strings.TrimSpace(n)
		if n != "" {
			allow[n] = true
		}
	}
	if len(allow) == 0 {
		return names
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		if allow[strings.TrimSpace(n)] {
			out = append(out, n)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Idempotency
// ---------------------------------------------------------------------------

// IdempotencyKeyFor derives a stable key for a tool call so a re-dispatched
// step does not repeat an external side effect. When the caller supplies an
// explicit step key (threaded from #582/#583's step.idempotencyKey), it is
// combined with the call signature; otherwise the signature alone is used.
// The result is a short hex digest suitable as a map key.
func IdempotencyKeyFor(stepKey, toolName, rawArgs string) string {
	sig := ToolCallSignature(toolName, rawArgs)
	seed := strings.TrimSpace(stepKey) + "\x00" + sig
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:16])
}

// IdempotencyStore is the minimal contract the loop needs to honor an
// idempotency key. The in-loop default is loopIdempotencyStore (a plain
// map); the full cross-turn implementation lands with #582/#583 behind the
// same interface so this hook does not hard-depend on that work.
type IdempotencyStore interface {
	// Get returns a previously-recorded result for key and whether it
	// existed.
	Get(key string) (string, bool)
	// Put records a result for key.
	Put(key, result string)
}

// loopIdempotencyStore is the loop-local default IdempotencyStore: a map
// scoped to a single turn so re-dispatch of an identical call within the
// turn reuses the first result. NOT safe for concurrent use; the loop
// guards access. Cross-turn persistence is #582/#583's job.
type loopIdempotencyStore struct {
	m map[string]string
}

// newLoopIdempotencyStore returns an empty loop-local store.
func newLoopIdempotencyStore() *loopIdempotencyStore {
	return &loopIdempotencyStore{m: make(map[string]string)}
}

func (s *loopIdempotencyStore) Get(key string) (string, bool) {
	if s == nil || s.m == nil {
		return "", false
	}
	v, ok := s.m[key]
	return v, ok
}

func (s *loopIdempotencyStore) Put(key, result string) {
	if s == nil {
		return
	}
	if s.m == nil {
		s.m = make(map[string]string)
	}
	s.m[key] = result
}

// ---------------------------------------------------------------------------
// Observation sink (harness #582 consumed behind an interface)
// ---------------------------------------------------------------------------

// ToolLoopObservationSink is the optional hook the loop uses to record
// observations (context-trim notes, stop reasons, structured errors) into
// the harness graph. It is consumed behind this interface so this PR builds
// standalone while #582 (observation persistence) is built in parallel:
// when no sink is wired, observations stay loop-local (logs only).
//
// TODO(#582): provide a concrete implementation that writes
// v1:harness:observation rows of the given kind, and wire it through
// ToolLoopOptions.Observations from the replier / planner dispatch.
type ToolLoopObservationSink interface {
	// RecordObservation persists a single observation. kind is the
	// observation kind ("note", "error", "stop", ...); text is the body;
	// meta carries optional structured fields. Implementations must be
	// safe to call from the loop goroutine and must not block on the LLM.
	RecordObservation(ctx context.Context, kind, text string, meta map[string]any)
}

// ToolLoopOptions bundles the per-call hardening knobs the loop consults.
// All fields are optional; the zero value yields the loop's built-in
// defaults so existing callers are unaffected.
type ToolLoopOptions struct {
	// ScopedTools, when non-empty, restricts the tool set for this call to
	// the named tools (the #588 hook). Out-of-scope tools are ignored.
	ScopedTools []string
	// IdempotencyKey is the step-level key threaded from #582/#583 so a
	// re-dispatched step does not repeat external side effects. Empty
	// leaves idempotency keyed on the call signature alone.
	IdempotencyKey string
	// RetryPolicy overrides the default per-tool timeout/retry policy.
	// Zero value uses DefaultToolRetryPolicy.
	RetryPolicy *ToolRetryPolicy
	// ContextBudget, when MaxTokens>0, enables context-budget trimming.
	ContextBudget *ContextBudget
	// Observations, when set, receives note/stop/error observations.
	Observations ToolLoopObservationSink
}

// resolvedRetryPolicy returns the effective retry policy for these options.
func (o *ToolLoopOptions) resolvedRetryPolicy() ToolRetryPolicy {
	if o != nil && o.RetryPolicy != nil {
		return *o.RetryPolicy
	}
	return DefaultToolRetryPolicy()
}

// recordObservation is a nil-safe helper: records to the sink when wired,
// otherwise no-ops (the caller logs separately).
func (o *ToolLoopOptions) recordObservation(ctx context.Context, kind, text string, meta map[string]any) {
	if o == nil || o.Observations == nil {
		return
	}
	o.Observations.RecordObservation(ctx, kind, text, meta)
}
