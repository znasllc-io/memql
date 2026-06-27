package memql

// dead_logic_lint.go -- the dead-logic lint that replaced the @entrypoint
// registration audit (memql#2216 / ADR §2.4). With @entrypoint retired, a logic
// is no longer a standalone entry point; it must be reached through some other
// construct. The invariant flips from "every @entrypoint logic has a wrapping
// automation" to the more general "every logic is referenced by some
// logic / query / tool / automation". Orphans are reported as a WARNING during
// the migration window; I5 promotes the check to a hard gate.

import (
	"log/slog"
	"regexp"
	"sort"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/baseloader"
)

var (
	// A call site: identifier immediately followed by `(`. Never matches a
	// `logic NAME {` definition header (no parenthesis there), so it is safe
	// to scan over raw file content.
	deadLogicCallRE = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)\s*\(`)
	// A longhand automation step reference: an INDENTED `logic NAME` (inside a
	// `step ... { ... }` block). The leading whitespace distinguishes it from a
	// top-of-line `logic NAME {` construct DEFINITION, which must not count as a
	// self-reference.
	deadLogicStepRE = regexp.MustCompile(`(?m)^[ \t]+logic[ \t]+([A-Za-z_][A-Za-z0-9_]*)`)
	// A terse single-step automation reference: `=> logic NAME` (memql#2215).
	deadLogicTerseRE = regexp.MustCompile(`=>[ \t]*logic[ \t]+([A-Za-z_][A-Za-z0-9_]*)`)
	// A tool function handler: `@handler(type="function", name="NAME")`.
	deadLogicHandlerRE = regexp.MustCompile(`name="([A-Za-z_][A-Za-z0-9_]*)"`)
)

// DeadLogicNames returns the names of logic constructs that no other construct
// references -- the dead-logic lint. A logic is "referenced" when its name
// appears as a call site in any body, as a `logic <name>` automation step
// (longhand or terse `=> logic <name>`), or as a tool `@handler(name="<name>")`.
// The result is sorted for deterministic reporting.
func DeadLogicNames(logger *slog.Logger) []string {
	logicNames := map[string]bool{}
	referenced := map[string]bool{}

	for _, raw := range baseloader.ReadAll(logger) {
		content := raw.Content
		for _, m := range deadLogicCallRE.FindAllStringSubmatch(content, -1) {
			referenced[m[1]] = true
		}
		for _, m := range deadLogicStepRE.FindAllStringSubmatch(content, -1) {
			referenced[m[1]] = true
		}
		for _, m := range deadLogicTerseRE.FindAllStringSubmatch(content, -1) {
			referenced[m[1]] = true
		}
		for _, m := range deadLogicHandlerRE.FindAllStringSubmatch(content, -1) {
			referenced[m[1]] = true
		}
		for _, slice := range ExtractFunctionSlices(content) {
			if slice.Kind == languageParser.FunctionTypeLogic {
				logicNames[slice.Name] = true
			}
		}
	}

	var dead []string
	for name := range logicNames {
		if !referenced[name] {
			dead = append(dead, name)
		}
	}
	sort.Strings(dead)
	return dead
}
