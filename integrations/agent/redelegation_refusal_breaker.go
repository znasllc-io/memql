package agent

import (
	"fmt"
	"sync"

	"github.com/znasllc-io/memql/core/env"
)

// Per-turn breaker on repeated produceArtifact RE-DELEGATION REFUSALS (memql#1138).
//
// #1134 added guardProduceArtifactRedelegation: when a produceArtifact EXECUTOR
// turn calls the produceArtifact tool again, the guard refuses BEFORE dispatch
// (no new plan is minted). But the refusal path in both agent tool loops only
// appended the refusal tool-result and `continue`d -- it never fed the #1128
// repeat-failure breaker (that breaker only sees exec ERRORS, and it keys on
// args, which the model can vary). So the model could re-call produceArtifact,
// get refused (uncounted), loop -- straight to the ~200-iteration cap, ~200 LLM
// calls. The guard fired but didn't stop the runaway.
//
// This breaker closes that gap: a turn-local, ARGS-INDEPENDENT counter keyed on
// tool name. After N refusals (default 2) the turn ABORTS with a clear error so
// the plan fails cleanly. The model should never call produceArtifact on an
// executor turn; two refusals is enough to be sure it's stuck, not exploring.
const (
	// defaultMaxRedelegationRefusals is the per-turn refusal ceiling when the
	// env override is unset. 2 lets the model see one refusal + correct before
	// we conclude it's looping.
	defaultMaxRedelegationRefusals = 2

	// envToolLoopMaxRedelegationRefusals overrides the ceiling. 0 (or negative)
	// disables the breaker (the loop falls back to its other guards + the
	// iteration cap).
	envToolLoopMaxRedelegationRefusals = "MEMQL_TOOL_LOOP_MAX_REDELEGATION_REFUSALS"
)

var (
	loadMaxRedelegationRefusalsOnce sync.Once
	cachedMaxRedelegationRefusals   int
)

// maxRedelegationRefusals resolves the ceiling from env once per process.
func maxRedelegationRefusals() int {
	loadMaxRedelegationRefusalsOnce.Do(func() {
		cachedMaxRedelegationRefusals = defaultMaxRedelegationRefusals
		reader := env.NewEnvReader("")
		if ptr, err := reader.OptionalInt(envToolLoopMaxRedelegationRefusals); err == nil && ptr != nil {
			cachedMaxRedelegationRefusals = *ptr
		}
	})
	return cachedMaxRedelegationRefusals
}

// redelegationRefusalBreaker counts produceArtifact re-delegation refusals
// within a single turn, keyed by tool name (args-independent, since the model
// may vary the args between refused calls). NOT safe for concurrent use; both
// agent tool loops drive it from their single sequential execution path.
type redelegationRefusalBreaker struct {
	max    int
	counts map[string]int
}

// newRedelegationRefusalBreaker builds a breaker with the env-resolved ceiling.
func newRedelegationRefusalBreaker() *redelegationRefusalBreaker {
	return &redelegationRefusalBreaker{max: maxRedelegationRefusals(), counts: map[string]int{}}
}

// redelegationRefusalAbortError is the typed error a tool loop returns when the
// refusal breaker trips.
func redelegationRefusalAbortError(toolName string, count int) error {
	return fmt.Errorf("tool %q re-delegation refused %d times -- aborting turn to avoid a runaway (execute the deliverable directly; do not re-call %s)", toolName, count, toolName)
}

// enabled reports whether the breaker is active (a positive ceiling).
func (b *redelegationRefusalBreaker) enabled() bool {
	return b != nil && b.max > 0
}

// observeRefusal records a refused re-delegation of toolName and reports
// (trip, count). trip is true once toolName has been refused `max` times this
// turn. When disabled, never trips (count is still returned for logging).
func (b *redelegationRefusalBreaker) observeRefusal(toolName string) (bool, int) {
	if b == nil {
		return false, 0
	}
	b.counts[toolName]++
	count := b.counts[toolName]
	if !b.enabled() {
		return false, count
	}
	return count >= b.max, count
}
