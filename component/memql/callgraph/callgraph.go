// Package callgraph enforces the behavioral DSL call-graph contract from the
// ADR (docs/internal/design/dsl-behavioral-constructs-adr.md, §2):
//
//	logic decides, mutations persist, actions touch the world,
//	queries read, automations orchestrate and react.
//
//	| Construct  | May call                                   | May NOT             |
//	|------------|--------------------------------------------|---------------------|
//	| query      | queries, read-only builtins                | any write           |
//	| mutation   | builtins; RMW on its own aggregate         | >1 aggregate write  |
//	| logic      | queries, other logic, read-only builtins   | mutations, actions, |
//	|            |                                            | triggers            |
//	| action     | one external capability (I6/I7)            | graph / other calls |
//	| automation | logic/query/mutation/action/sub-automation | --                  |
//
// It is the single source of truth for the rules, wired into BOTH the
// authoring sandbox cross-reference pass (reject at define->promote) and a
// whole-tree CI conformance check (warning during the I2 migration window;
// hard gate from I5).
//
// The analysis is source-level and leans on two invariants of the flattened
// DSL layout: a construct's kind is given by its file name
// (dsl/<ns>/<kind>s.memql), and every cross-file construct it calls is named
// by a file-top `use <ns>.<kind>s.{ name, ... }` import -- so the kind of every
// callable name is known without resolving the whole tree.
package callgraph

import (
	"fmt"
	"regexp"
	"strings"
)

// Finding is one violation of the §2 call-graph contract.
type Finding struct {
	Construct string // construct name
	Kind      string // logic | mutation | query | automation | ...
	Rule      string // short rule id (stable; used in tests + messages)
	Message   string // human-readable, prefixed with the construct
}

// SideEffectClassifier reports whether a builtin (by name) is side-effecting
// (its capability sideEffectClass is write/exec rather than read). Until I7
// declares sideEffectClass on capabilities, the whole-tree pass uses a
// conservative classifier that returns false for everything (no findings);
// unit tests inject one. A nil classifier is treated as "nothing is
// side-effecting."
type SideEffectClassifier func(builtinName string) bool

var (
	// A call site: an identifier immediately followed by `(`. Intersected
	// with the file's use-map so only cross-file construct calls count --
	// pure helpers (coalesce/concat/if) and method calls (.First()) are
	// never imported, so they fall out.
	callRE = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)\s*\(`)
	// A graph write: an `insert {` or `update {` block opener at statement
	// position.
	writeBlockRE = regexp.MustCompile(`(?m)^\s*(insert|update)\s*\{`)
	// A trigger annotation (the reactive surface).
	triggerRE = regexp.MustCompile(`@trigger\b`)
	// File-top use import: `use a.b.c.{ x, y }`. The brace body may span
	// lines; `[^}]*` (with default dot) crosses newlines.
	useRE = regexp.MustCompile(`(?m)^[ \t]*use[ \t]+([A-Za-z_][A-Za-z0-9_.]*)\.\{([^}]*)\}`)
)

// singular maps a construct-file base name (the `<kind>s` segment) to the
// singular construct kind.
func singular(kind string) string {
	switch kind {
	case "logic":
		return "logic"
	case "queries":
		return "query"
	case "mutations":
		return "mutation"
	case "automations":
		return "automation"
	case "builtins":
		return "builtin"
	case "actions":
		return "action"
	case "concepts":
		return "concept"
	case "shapes":
		return "shape"
	case "specs":
		return "spec"
	case "traits":
		return "trait"
	default:
		return strings.TrimSuffix(kind, "s")
	}
}

// UseKinds builds a name->kind map from a source's file-top `use` imports.
// e.g. `use cluster.mutations.{ createNode }` yields {"createNode": "mutation"}.
func UseKinds(source string) map[string]string {
	out := map[string]string{}
	for _, m := range useRE.FindAllStringSubmatch(source, -1) {
		path := m[1]
		parts := strings.Split(path, ".")
		if len(parts) < 2 {
			continue
		}
		kind := singular(parts[len(parts)-1])
		for _, raw := range strings.Split(m[2], ",") {
			name := strings.TrimSpace(raw)
			if name != "" {
				out[name] = kind
			}
		}
	}
	return out
}

// callNames returns the distinct cross-file construct names called in text
// (call sites whose identifier appears in useKinds), each tagged with its kind.
func callNames(text string, useKinds map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range callRE.FindAllStringSubmatch(text, -1) {
		name := m[1]
		if kind, ok := useKinds[name]; ok {
			out[name] = kind
		}
	}
	return out
}

// ConstructFindings analyses a single authored construct against the §2
// contract. text is the construct's full authored text (its annotations + body
// is sufficient; a leading `use` block is harmless). useKinds maps every
// imported name to its kind. sideEffecting classifies builtins (nil => none).
func ConstructFindings(kind, name, text string, useKinds map[string]string, sideEffecting SideEffectClassifier) []Finding {
	if sideEffecting == nil {
		sideEffecting = func(string) bool { return false }
	}
	var out []Finding
	add := func(rule, msg string) {
		out = append(out, Finding{Construct: name, Kind: kind, Rule: rule, Message: fmt.Sprintf("%s %q: %s", kind, name, msg)})
	}

	// Rule: trigger monopoly. Only automations may carry @trigger.
	if kind != "automation" && triggerRE.MatchString(text) {
		add("trigger-monopoly", "carries @trigger -- only automations are reactive (ADR §2.4); express reactivity as a triggered automation")
	}

	calls := callNames(text, useKinds)

	switch kind {
	case "logic":
		// Logic is pure: no mutations, no actions, no side-effecting builtins.
		for cn, ck := range calls {
			switch ck {
			case "mutation":
				add("logic-purity", fmt.Sprintf("calls mutation %q -- logic is pure (ADR §2.1); move the write to an automation step", cn))
			case "action":
				add("logic-purity", fmt.Sprintf("calls action %q -- logic may not touch the world (ADR §2.1); call the action from an automation step", cn))
			case "builtin":
				if sideEffecting(cn) {
					add("read-only-builtin", fmt.Sprintf("calls side-effecting builtin %q in read context -- reclassify it as an action (ADR §3)", cn))
				}
			}
		}
	case "query":
		// Query is a pure read.
		if writeBlockRE.MatchString(text) {
			add("query-no-write", "contains a graph write block -- queries only read (ADR §2)")
		}
		for cn, ck := range calls {
			switch ck {
			case "mutation":
				add("query-no-write", fmt.Sprintf("calls mutation %q -- queries only read (ADR §2)", cn))
			case "builtin":
				if sideEffecting(cn) {
					add("read-only-builtin", fmt.Sprintf("calls side-effecting builtin %q in read context -- reclassify it as an action (ADR §3)", cn))
				}
			}
		}
	case "mutation":
		// Exactly one graph write per body (one aggregate boundary). A RMW on
		// the same aggregate is a single insert/update block, so >1 block is a
		// second write; calling another mutation is a second aggregate write.
		if n := len(writeBlockRE.FindAllString(text, -1)); n > 1 {
			add("mutation-single-write", fmt.Sprintf("has %d graph-write blocks -- a mutation writes exactly one aggregate (ADR §2.3)", n))
		}
		for cn, ck := range calls {
			if ck == "mutation" {
				add("mutation-single-write", fmt.Sprintf("calls mutation %q -- a second aggregate write is rejected (ADR §2.3); sequence writes as separate automation steps", cn))
			}
		}
	}
	return out
}
