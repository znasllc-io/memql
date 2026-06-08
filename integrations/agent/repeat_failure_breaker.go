package agent

import (
	"fmt"
	"sync"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/env"
)

// Per-turn circuit breaker on repeated IDENTICAL tool FAILURES (memql#1128).
//
// The runaway this guards against: a produceArtifact turn whose workbenchHost
// write keeps getting rejected (e.g. the acting-agent role wasn't stamped on
// ctx, so ExecuteTool denies the tool). The model gets the failure back, and
// -- having no other idea -- re-issues the SAME write next iteration. Nothing
// in the loop stopped that: the iteration cap is ~200, the all-errored-round
// guard resets the moment ANY other tool call succeeds (so one trivial success
// interleaved with the repeated failing write lets it spin to maxIterations),
// the loop-detector keys on identical calls regardless of outcome (can't tell a
// legitimate retry from a stuck failure), and the per-plan budget guards the
// planner decompose loop, not this direct production turn. ~200 LLM calls cost
// the user ~$13.
//
// This breaker is the durable, root-cause-agnostic backstop: it bounds ANY
// stuck-tool loop to a few cents. It is intentionally conservative -- it counts
// ONLY consecutive identical+failing repeats (same tool, same canonical args,
// same error class) and resets on the FIRST sign of progress (any success, any
// different call). Legitimate retries-with-corrected-args (different args) and
// eventually-succeeding calls are never killed.
const (
	// defaultMaxRepeatFailures is the consecutive-identical-failure ceiling
	// when the env override is unset. Three is enough to let the model try
	// the same thing, read the structured error, and try once more before we
	// conclude it's stuck.
	defaultMaxRepeatFailures = 3

	// envToolLoopMaxRepeatFailures overrides the ceiling. 0 disables the
	// breaker entirely (the loop falls back to its other guards + the
	// iteration cap).
	envToolLoopMaxRepeatFailures = "MEMQL_TOOL_LOOP_MAX_REPEAT_FAILURES"
)

var (
	loadMaxRepeatFailuresOnce sync.Once
	cachedMaxRepeatFailures   int
)

// maxRepeatFailures resolves the breaker ceiling from env once per process.
// Falls back to defaultMaxRepeatFailures when unset / non-numeric. A configured
// 0 (or negative) disables the breaker.
func maxRepeatFailures() int {
	loadMaxRepeatFailuresOnce.Do(func() {
		cachedMaxRepeatFailures = defaultMaxRepeatFailures
		reader := env.NewEnvReader("")
		if ptr, err := reader.OptionalInt(envToolLoopMaxRepeatFailures); err == nil && ptr != nil {
			// A configured value (including 0 to disable) wins. Negative is
			// treated as disabled.
			cachedMaxRepeatFailures = *ptr
		}
	})
	return cachedMaxRepeatFailures
}

// repeatFailureBreaker tracks consecutive identical tool failures within a
// single turn. It is NOT safe for concurrent use; both agent tool loops drive
// it from their single sequential per-iteration execution path.
type repeatFailureBreaker struct {
	// max is the consecutive-identical-failure ceiling. <= 0 disables.
	max int
	// lastKey is the (tool+args+errorClass) signature of the most recent
	// FAILING tool call, or "" when the last observed call succeeded / was a
	// different signature streak's start.
	lastKey string
	// count is how many times in a row lastKey has failed.
	count int
}

// newRepeatFailureBreaker builds a breaker with the env-resolved ceiling.
func newRepeatFailureBreaker() *repeatFailureBreaker {
	return &repeatFailureBreaker{max: maxRepeatFailures()}
}

// repeatFailureAbortError is the typed error a tool loop returns when the
// breaker trips. It names the stuck tool + the consecutive-failure count so the
// log line + the caller's failure handling carry the diagnosis.
func repeatFailureAbortError(toolName string, count int) error {
	return fmt.Errorf("tool %q failed %d times identically -- aborting turn to avoid a runaway", toolName, count)
}

// enabled reports whether the breaker is active (a positive ceiling).
func (b *repeatFailureBreaker) enabled() bool {
	return b != nil && b.max > 0
}

// observeFailure records a failing tool call and reports (trip, count). trip is
// true once the SAME (toolName, args, errorClass) has failed `max` times in a
// row. The caller passes the same execErr it fed back to the model; the error
// class keeps a tool that fails two genuinely-different ways from tripping (we
// only fuse truly identical failures).
func (b *repeatFailureBreaker) observeFailure(toolName, rawArgs string, execErr error) (bool, int) {
	if !b.enabled() {
		return false, 0
	}
	key := b.failureKey(toolName, rawArgs, execErr)
	if key == b.lastKey {
		b.count++
	} else {
		b.lastKey = key
		b.count = 1
	}
	return b.count >= b.max, b.count
}

// observeSuccess resets the streak: any successful tool call is progress, so a
// flaky failure earlier in the turn must not accumulate toward the ceiling.
func (b *repeatFailureBreaker) observeSuccess() {
	if b == nil {
		return
	}
	b.lastKey = ""
	b.count = 0
}

// failureKey builds the per-failure identity: canonical tool+args signature
// fused with the error class. We reuse memql.ToolCallSignature (key-order-
// independent JSON canonicalization) so cosmetically-different-but-equal args
// compare equal, and memql.ClassifyToolError so a validation failure and a
// permission failure on the same call are NOT fused (different root cause).
func (b *repeatFailureBreaker) failureKey(toolName, rawArgs string, execErr error) string {
	sig := memql.ToolCallSignature(toolName, rawArgs)
	class := ""
	if execErr != nil {
		if se := memql.ClassifyToolError(execErr); se != nil {
			class = string(se.Type)
		}
	}
	return sig + "\x00" + class
}
