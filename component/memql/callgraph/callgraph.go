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
	// pure helpers (coalesce/concat/if) and method calls (.first()) are
	// never imported, so they fall out.
	callRE = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)\s*\(`)
	// A graph write: an `insert {` or `update {` block opener at statement
	// position.
	writeBlockRE = regexp.MustCompile(`(?m)^\s*(insert|update)\s*\{`)
	// A trigger annotation (the reactive surface).
	triggerRE = regexp.MustCompile(`@trigger\b`)
	// A `body { }` block opener at statement position -- the procedural
	// marker (construct-invocation ADR Decision 5). Requires the `{` so a
	// declarative field merely *named* body is never matched.
	bodyBlockRE = regexp.MustCompile(`(?m)^[ \t]*body[ \t]*\{`)
	// A capability declaration inside an action body:
	// `capability "<namespace>.<verb>"` at statement position.
	capabilityRE = regexp.MustCompile(`(?m)^[ \t]*capability[ \t]+"([^"]*)"`)
	// File-top use import: `use a.b.c.{ x, y }`. The brace body may span
	// lines; `[^}]*` (with default dot) crosses newlines.
	useRE = regexp.MustCompile(`(?m)^[ \t]*use[ \t]+([A-Za-z_][A-Za-z0-9_.]*)\.\{([^}]*)\}`)
)

// bodyRuleProceduralKinds is the set of non-logic constructs whose top-level
// `body {` opener is unambiguously the procedural marker (and thus a body-rule
// violation, ADR Decision 5) rather than a declarative object field. Declarative
// constructs (concept/shape/prompt/provider/tool/builtin/seed/policy) are
// intentionally absent: they may carry a nested object field named `body`.
var bodyRuleProceduralKinds = map[string]bool{
	"query":      true,
	"mutation":   true,
	"action":     true,
	"automation": true,
	"spec":       true,
	"trait":      true,
}

// validCapabilityNamespaces is the closed action-capability vocabulary
// (ADR §2.3). The authoritative per-verb sideEffectClass registry lives in
// component/actions/capability; the structural call-graph rules only need the
// namespace set. Kept in sync by component/actions/capability.Namespaces.
var validCapabilityNamespaces = map[string]bool{
	"fs":          true,
	"shell":       true,
	"http":        true,
	"integration": true,
	"mcp":         true,
}

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

	// Rule: body { } (construct-invocation ADR Decision 5). The procedural
	// `body { }` marker is MANDATORY on logic and FORBIDDEN on every other
	// construct. Mirrors the parser's enforcement so the whole-tree CI gate
	// + the authoring-sandbox cross-reference pass flag the same violation.
	// The forbidden arm is scoped to the procedural/behavioral kinds whose
	// `body {` opener is unambiguously the marker; a *declarative* construct
	// (concept/shape/...) may legitimately carry a nested object field named
	// `body`, so those are not flagged here.
	hasBody := bodyBlockRE.MatchString(text)
	if kind == "logic" {
		if !hasBody {
			add("body-rule", "must wrap its procedural code in a `body { }` block (mandatory on logic; ADR Decision 5)")
		}
	} else if bodyRuleProceduralKinds[kind] && hasBody {
		add("body-rule", fmt.Sprintf("declares a `body { }` block -- forbidden on %s; `body { }` is the procedural marker reserved for logic (ADR Decision 5)", kind))
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
	case "action":
		// An action performs EXACTLY ONE external capability call on a surface
		// and NEVER touches the MemQL graph or any other construct (ADR §2.3).
		caps := capabilityRE.FindAllStringSubmatch(text, -1)
		switch len(caps) {
		case 0:
			add("action-one-capability", "declares no external capability -- an action performs exactly one (ADR §2.3)")
		case 1:
			// The single capability's namespace must be in the vocabulary.
			capName := caps[0][1]
			ns := capName
			if i := strings.IndexByte(capName, '.'); i >= 0 {
				ns = capName[:i]
			}
			if !validCapabilityNamespaces[ns] {
				add("action-capability-namespace", fmt.Sprintf("capability %q is not in the namespace vocabulary -- one of fs.* / shell.* / http.* / integration.* / mcp.* (ADR §2.3)", capName))
			}
		default:
			add("action-one-capability", fmt.Sprintf("declares %d capabilities -- an action performs exactly one (ADR §2.3); compose multiples in an automation", len(caps)))
		}
		// An action may not invoke any other DSL construct.
		for cn, ck := range calls {
			switch ck {
			case "logic", "query", "mutation", "automation", "action":
				add("action-no-calls", fmt.Sprintf("calls %s %q -- an action is a single external capability and may not invoke other constructs (ADR §2.3)", ck, cn))
			}
		}
		// An action never reaches the graph: graph writes are mutations.
		if writeBlockRE.MatchString(text) {
			add("action-no-graph", "contains a graph write block -- an action never touches the MemQL graph (ADR §2.3); persist via a following mutation step")
		}
	}
	return out
}
