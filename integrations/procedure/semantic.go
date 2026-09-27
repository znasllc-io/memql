package procedure

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/znasllc-io/memql/core/num"
)

// semantic.go -- the arguments an action's EXECUTION reads, and only those.
//
// An app records the whole call it made, and some of what it records is for
// the model or for a person rather than for the executor: Claude Code's Bash
// carries a `description` the model wrote for that one call and a `timeout`,
// and WebFetch carries the `prompt` the model asked about the page. None of
// them changes what the call does, and all of them vary from one recording to
// the next -- so generalizing them makes a FREE parameter no goal input can
// ever supply. A procedure with one such hole passes shadow (the app's own
// call binds it) and then refuses every canary start as unbound, so the ladder
// promotes it, demotes it and proposes it again, forever.
//
// ONE OF THEM IS STILL NEEDED, beside the template rather than in it: the
// timeout (review finding I2). Dropped entirely, every replayed command ran
// under the executor's default -- sixty seconds on the workbench -- however
// long the recordings let it run. recordedTimeoutMs reads it off the raw call
// before this projection drops it, and the lift keeps the longest per step as
// a hint outside the procedure's hash (payload.go).
//
// A DENYLIST, deliberately, rather than a list of the keys each dispatcher
// reads. An unknown key left in place is a hole the ladder can see and refuse;
// an unknown key dropped by an allowlist is a call that replays differently
// from the one recorded with nothing anywhere saying so.
var nonSemanticArgs = map[string]map[string]bool{
	"exec":  {"description": true, "timeout": true},
	"fetch": {"prompt": true},
}

// semanticArgs returns args without the keys the step type's execution never
// reads. The input map is not modified.
func semanticArgs(stepType string, args map[string]any) map[string]any {
	drop := nonSemanticArgs[stepType]
	if len(drop) == 0 || len(args) == 0 {
		return args
	}
	out := make(map[string]any, len(args))
	for k, v := range args {
		if !drop[k] {
			out[k] = v
		}
	}
	return out
}

// timeoutArgs names, per step type, the argument that carries the call's own
// time limit, in MILLISECONDS (Claude Code's Bash `timeout`, which the app's
// dispatchers also read in milliseconds).
var timeoutArgs = map[string]string{"exec": "timeout"}

// recordedTimeoutMs is the time limit one recorded call asked for, in
// milliseconds; 0 when it asked for none or the value is not a positive
// number. Narrowing: saturate -- a limit is compared with others for the
// longest, and a value past the platform int is still the longest.
func recordedTimeoutMs(stepType string, args map[string]any) int {
	key := timeoutArgs[stepType]
	if key == "" {
		return 0
	}
	var ms int
	switch v := args[key].(type) {
	case float64:
		ms = num.ClampFloat64(v)
	case int:
		ms = v
	case int64:
		ms = num.ClampInt64(v)
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return 0
		}
		ms = num.ClampFloat64(f)
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return 0
		}
		ms = num.ClampFloat64(f)
	}
	if ms < 0 {
		return 0
	}
	return ms
}
