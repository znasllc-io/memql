package procedure

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
