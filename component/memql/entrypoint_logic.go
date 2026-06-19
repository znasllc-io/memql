package memql

// entrypoint_logic.go -- the entry-point-logic registration invariant
// (memql#1707). A logic construct marked `@entrypoint` is meant to be invoked
// directly via run_automation (no triggering event), but the engine's
// top-level dispatch only runs AUTOMATIONS -- a bare logic construct is not a
// runnable entry point on its own. Before this, such a logic was uninvokable
// unless an author happened to hand-write a wrapping automation, with nothing
// enforcing it (logicEnsureDailySpaceForCaller was the orphan that surfaced the
// gap).
//
// This file makes invocability a registration INVARIANT by AUTO-GENERATING a
// wrapping automation for every `@entrypoint` logic that no authored automation
// already wraps. The generated wrapper is deterministic source text, compiled
// through the exact same pipeline as authored automations, so the logic becomes
// reachable by name (full `logicXxx` or bare form) on BOTH the live and dry-run
// run_automation paths. The automations Loader consumes EntryPointLogicWrappers
// to append the synthetic wrappers at load time; DSLConstructSource returns the
// same generated source so the dry-run sandbox can re-parse it.
//
// The companion audit (ValidateEntryPointLogics, in component/automations)
// fails the build if any @entrypoint logic still has no invocation path.

import (
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/baseloader"
)

// entryPointAnnotationRE matches the `@entrypoint` annotation on its own line
// (the canonical placement in a construct's @-attribute preamble). Anchored to
// line start so it never misfires on the substring inside a description string.
var entryPointAnnotationRE = regexp.MustCompile(`(?m)^[ \t]*@entrypoint\b`)

// logicArgsBlockRE detects an `args { ... }` block inside a logic body, so the
// generated wrapper knows whether to forward the run_automation input (bound as
// `event`) into the logic call.
var logicArgsBlockRE = regexp.MustCompile(`(?m)^[ \t]*args[ \t]*\{`)

// EntryPointLogicWrapper describes an `@entrypoint` logic construct and the
// synthetic wrapping automation that makes it invokable via run_automation.
type EntryPointLogicWrapper struct {
	// LogicName is the full `logicXxx` construct name.
	LogicName string
	// BareName is the bare invocation form (`logic` prefix stripped, first
	// letter lower-cased) -- the name an author naturally reaches for.
	BareName string
	// WrapperName is the deterministic synthetic automation name.
	WrapperName string
	// Source is the generated automation `.memql` source for the wrapper.
	Source string
}

// EntryPointLogicWrappers scans the embedded/override DSL tree for logic
// constructs carrying `@entrypoint` and returns one wrapper descriptor per
// logic, sorted by logic name for deterministic ordering. The wrapper Source is
// a complete `automation NAME { step run { logic <bare> {...} } }` declaration
// ready to compile through the automation loader's normal pipeline.
//
// This is the single generator both the automations Loader (live + LoadAll
// enumeration) and DSLConstructSource (dry-run source fetch) derive from, so
// the two surfaces cannot diverge.
func EntryPointLogicWrappers(logger *slog.Logger) []EntryPointLogicWrapper {
	seen := make(map[string]bool)
	var out []EntryPointLogicWrapper
	for _, raw := range baseloader.ReadAll(logger) {
		for _, slice := range ExtractFunctionSlices(raw.Content) {
			if slice.Kind != languageParser.FunctionTypeLogic {
				continue
			}
			if !entryPointAnnotationRE.MatchString(slice.Source) {
				continue
			}
			full := slice.Name
			if seen[full] {
				continue
			}
			seen[full] = true
			out = append(out, EntryPointLogicWrapper{
				LogicName:   full,
				BareName:    bareEntryPointName(full),
				WrapperName: entryPointWrapperName(full),
				Source:      generateEntryPointWrapperSource(full, logicSliceHasArgs(slice.Source)),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LogicName < out[j].LogicName })
	return out
}

// EntryPointWrapperSource returns the generated wrapper source for the synthetic
// automation named wrapperName, or false if wrapperName is not an entry-point
// wrapper. Used by DSLConstructSource so the dry-run run_automation path can
// re-parse the same source the live path executes.
func EntryPointWrapperSource(logger *slog.Logger, wrapperName string) (string, bool) {
	for _, w := range EntryPointLogicWrappers(logger) {
		if w.WrapperName == wrapperName {
			return w.Source, true
		}
	}
	return "", false
}

// logicSliceHasArgs reports whether a logic slice declares an `args { ... }`
// block. An entry-point logic with args receives the run_automation input
// (bound as `event`); one with no args is called with an empty object.
func logicSliceHasArgs(source string) bool {
	return logicArgsBlockRE.MatchString(source)
}

// bareEntryPointName strips the `logic` prefix and lower-cases the first letter
// (the bare invocation form). A name not in `logicXxx` form is returned
// unchanged.
func bareEntryPointName(full string) string {
	const prefix = "logic"
	if !strings.HasPrefix(full, prefix) || len(full) <= len(prefix) {
		return full
	}
	rest := full[len(prefix):]
	return strings.ToLower(rest[:1]) + rest[1:]
}

// entryPointWrapperName derives the deterministic synthetic automation name for
// an entry-point logic: the bare invocation form suffixed with `Entrypoint`
// (e.g. logicEnsureDailySpaceForCaller -> ensureDailySpaceForCallerEntrypoint).
// The suffix keeps it from colliding with any authored automation name; name
// resolution itself works through the wrapped-logic path, not this name.
func entryPointWrapperName(full string) string {
	return bareEntryPointName(full) + "Entrypoint"
}

// generateEntryPointWrapperSource builds the wrapper automation source for an
// entry-point logic. When the logic declares args it forwards the trigger input
// (`event: event`); otherwise it calls the logic with an empty object.
func generateEntryPointWrapperSource(full string, hasArgs bool) string {
	bare := bareEntryPointName(full)
	wrapper := entryPointWrapperName(full)
	stepCall := "logic " + bare + " {}"
	if hasArgs {
		stepCall = "logic " + bare + " { event: event }"
	}
	return fmt.Sprintf(`// Auto-generated wrapper for the @entrypoint logic %q (memql#1707).
// Makes the logic invokable by name via run_automation; do not author by hand.
@enabled
@description("Auto-generated run_automation entry point wrapping the %s logic.")
automation %s {
  step run {
    %s
  }
}
`, full, full, wrapper, stepCall)
}
