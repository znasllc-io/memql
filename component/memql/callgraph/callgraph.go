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
	// The single capability call inside a simplified action body (ADR
	// Decision 3): `capability <verb>(...)` at statement position. The verb
	// is a bare identifier resolved to a full dotted capability via the file's
	// `use capabilities.*` imports.
	capabilityCallRE = regexp.MustCompile(`(?m)^[ \t]*capability[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\(`)
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
		// `use capabilities.<path>.{ verb }` imports a capability verb (ADR
		// Decision 4): tag it kind "capability" so a capability call inside an
		// action body is not mistaken for a construct call.
		kind := singular(parts[len(parts)-1])
		if parts[0] == "capabilities" {
			kind = "capability"
		}
		for _, raw := range strings.Split(m[2], ",") {
			name := strings.TrimSpace(raw)
			if name != "" {
				out[name] = kind
			}
		}
	}
	return out
}

// capabilityNamespaces builds a verb -> namespace map from a source's
// `use capabilities.<namespace>.<...>.{ verb }` imports. The namespace is the
// first path segment after "capabilities" (e.g. "shell" for
// `capabilities.shell`, "integration" for `capabilities.integration.github`).
// Used to validate the namespace of an action's single capability call against
// the closed vocabulary. A verb whose import is not in `text` is absent (the
// loader enforces resolution loudly; the structural pass stays best-effort).
func capabilityNamespaces(source string) map[string]string {
	out := map[string]string{}
	for _, m := range useRE.FindAllStringSubmatch(source, -1) {
		parts := strings.Split(m[1], ".")
		if len(parts) < 2 || parts[0] != "capabilities" {
			continue
		}
		ns := parts[1]
		for _, raw := range strings.Split(m[2], ",") {
			verb := strings.TrimSpace(raw)
			if verb != "" {
				out[verb] = ns
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

// kindPrefixedInvocationKinds are the construct kinds that MUST carry their
// kind keyword at the call site (construct-invocation ADR Decision 1 / #2335):
// `<kind> name(args)`. spec / trait are predicate references (a bare name with
// no parens in a filter; exempt), shape / concept are signature-bound (never
// called), and a capability call is `capability <verb>(...)` validated by the
// action case -- so none of those are listed here.
var kindPrefixedInvocationKinds = map[string]bool{
	"query":      true,
	"mutation":   true,
	"logic":      true,
	"action":     true,
	"builtin":    true,
	"automation": true,
}

func isCallgraphIdentByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// missingKindPrefixCalls returns the imported construct names invoked as a bare
// `name(` -- WITHOUT the kind keyword that the kind-prefixed invocation form
// requires -- keyed to their kind. A call written `<kind> name(` is correctly
// prefixed and never reported; a dotted/method call `x.name(` is not a construct
// invocation and is skipped. Only names whose useKinds kind is in
// kindPrefixedInvocationKinds are checked, so ambient language builtins
// (coalesce / concat / where / ...), bare predicate refs in a filter, and
// capability calls never produce a finding.
// stripLiteralsAndComments blanks the CONTENTS of double-quoted string literals
// and `//` line comments (replacing each interior byte with a space, preserving
// length + offsets) so a call-shaped token mentioned in prose -- an
// `@description("... staleClusterNodes({...}) ...")` string or a `//` comment --
// is never mistaken for a real call site.
func stripLiteralsAndComments(text string) string {
	b := []byte(text)
	inStr, inComment := false, false
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch {
		case inComment:
			if c == '\n' {
				inComment = false
			} else {
				b[i] = ' '
			}
		case inStr:
			if c == '\\' && i+1 < len(b) {
				b[i] = ' '
				b[i+1] = ' '
				i++
			} else if c == '"' {
				inStr = false
			} else {
				b[i] = ' '
			}
		case c == '"':
			inStr = true
		case c == '/' && i+1 < len(b) && b[i+1] == '/':
			inComment = true
			b[i] = ' '
		}
	}
	return string(b)
}

func missingKindPrefixCalls(text string, useKinds map[string]string) map[string]string {
	text = stripLiteralsAndComments(text)
	out := map[string]string{}
	for _, idx := range callRE.FindAllStringSubmatchIndex(text, -1) {
		nameStart, nameEnd := idx[2], idx[3]
		name := text[nameStart:nameEnd]
		kind, ok := useKinds[name]
		if !ok || !kindPrefixedInvocationKinds[kind] {
			continue
		}
		// `x.name(` is a method/dotted access, not a construct call.
		if nameStart > 0 && text[nameStart-1] == '.' {
			continue
		}
		// Walk back over whitespace to the preceding word.
		j := nameStart
		for j > 0 {
			c := text[j-1]
			if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
				j--
				continue
			}
			break
		}
		k := j
		for k > 0 && isCallgraphIdentByte(text[k-1]) {
			k--
		}
		prevWord := text[k:j]
		// Correctly kind-prefixed `<kind> name(` -- the preceding word IS the
		// construct's kind keyword (and there was real whitespace between them).
		if j < nameStart && prevWord == kind {
			continue
		}
		out[name] = kind
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

	// Rule: kind-prefixed invocation (construct-invocation ADR Decision 1 /
	// #2335). A call to an imported construct of an invocation kind
	// (query / mutation / logic / action / builtin / automation) MUST carry its
	// kind keyword at the call site -- `<kind> name(...)`. A bare `name(...)` is
	// flagged. Bare predicate refs in a filter (spec / trait, no parens) and
	// ambient language builtins are never reported (they are not invocation
	// kinds / not in useKinds).
	for cn, ck := range missingKindPrefixCalls(text, useKinds) {
		add("missing-kind-prefix", fmt.Sprintf("calls %s %q without its kind prefix -- write `%s %s(...)` (construct-invocation ADR Decision 1)", ck, cn, ck, cn))
	}

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
		// and NEVER touches the MemQL graph or any other construct (ADR
		// Decision 3): `capability <verb>(...)`, the verb resolved to a full
		// dotted capability via the file's `use capabilities.*` imports.
		caps := capabilityCallRE.FindAllStringSubmatch(text, -1)
		switch len(caps) {
		case 0:
			add("action-one-capability", "declares no external capability call -- an action body is exactly one `capability <verb>(...)` call (ADR Decision 3)")
		case 1:
			// The capability verb must resolve (via the file's capability
			// imports) to a namespace in the closed vocabulary. When the import
			// is not visible in this construct's text the structural pass skips
			// the check -- the loader enforces resolution loudly.
			verb := caps[0][1]
			if ns, known := capabilityNamespaces(text)[verb]; known && !validCapabilityNamespaces[ns] {
				add("action-capability-namespace", fmt.Sprintf("capability %q resolves to namespace %q, not in the vocabulary -- one of fs.* / shell.* / http.* / integration.* / mcp.* (ADR Decision 4)", verb, ns))
			}
		default:
			add("action-one-capability", fmt.Sprintf("declares %d capability calls -- an action performs exactly one (ADR Decision 3); compose multiples in an automation", len(caps)))
		}
		// An action may not invoke any other DSL construct (the capability verb
		// itself is tagged kind "capability", so it is permitted here).
		for cn, ck := range calls {
			switch ck {
			case "logic", "query", "mutation", "automation", "action":
				add("action-no-calls", fmt.Sprintf("calls %s %q -- an action is a single external capability and may not invoke other constructs (ADR Decision 3)", ck, cn))
			}
		}
		// An action never reaches the graph: graph writes are mutations.
		if writeBlockRE.MatchString(text) {
			add("action-no-graph", "contains a graph write block -- an action never touches the MemQL graph (ADR §2.3); persist via a following mutation step")
		}
	case "automation":
		// Automations orchestrate; they do not DECIDE (P4, memql#2371).
		// Conditions (if gates, forEach where clauses, @filter) may gate on
		// step results, presence, and single-value fan-out equality -- but
		// POLICY in a condition is a finding: a same-field string-literal
		// ||-vocabulary (role/status sets), date-math or default-injecting
		// builtins (addDuration / coalesce / concat), each of which belongs
		// in a pure decide logic (or a query pushdown / @filter relevance
		// check). This is the rule whose ABSENCE let 13 policy sites
		// accumulate invisibly after the #2235 burn-down.
		for _, cond := range automationConditions(text) {
			if m := conditionBuiltinRE.FindStringSubmatch(cond); m != nil {
				add("automation-condition-builtin", fmt.Sprintf("condition %q calls %s() -- date math / defaults are POLICY; compute the decision in a pure logic (or push a cutoff into the query) and gate on steps.<decide>.result (P4, #2371)", snippet(cond), m[1]))
			}
			if field, ok := literalVocabularyField(cond); ok {
				add("automation-condition-vocabulary", fmt.Sprintf("condition %q compares %q against multiple string literals -- a value VOCABULARY is policy; own it in one pure decide logic and switch on its result (P4, #2371)", snippet(cond), field))
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Automation condition vocabulary (P4, memql#2371)
// ---------------------------------------------------------------------------

var (
	// if-step gates: `if <cond> {` at a step position (never matches the
	// construct headers -- automations have no top-level if).
	automationIfRE = regexp.MustCompile(`(?m)^\s*if\s+(.+?)\s*\{\s*$`)
	// forEach where clauses: `forEach x in <src> where <cond> {`.
	automationWhereRE = regexp.MustCompile(`(?m)forEach\s+\w+\s+in\s+.+?\s+where\s+(.+?)\s*\{\s*$`)
	// trigger relevance filters: `@filter(<cond>)`.
	automationFilterRE = regexp.MustCompile(`@filter\(([^)]*)\)`)

	// Policy smells inside a condition. exists() is the sanctioned presence
	// guard and is deliberately NOT in this list.
	conditionBuiltinRE = regexp.MustCompile(`\b(addDuration|coalesce|concat)\s*\(`)
	// Every `<ident> == "<literal>"` atom in a condition; two on the SAME
	// identifier joined by || form a vocabulary (checked in Go -- RE2 has no
	// backreferences).
	conditionEqualityAtomRE = regexp.MustCompile(`([A-Za-z_][\w.]*)\s*==\s*"[^"]*"`)
)

// literalVocabularyField reports the field name when cond compares the SAME
// identifier against 2+ string literals under || -- the role/status
// vocabulary shape (`x == "a" || x == "b"`). &&-joined equalities on one
// field are contradictory, not a vocabulary, so || presence is required.
func literalVocabularyField(cond string) (string, bool) {
	if !strings.Contains(cond, "||") {
		return "", false
	}
	seen := map[string]int{}
	for _, m := range conditionEqualityAtomRE.FindAllStringSubmatch(cond, -1) {
		seen[m[1]]++
		if seen[m[1]] >= 2 {
			return m[1], true
		}
	}
	return "", false
}

// automationConditions extracts every condition surface from an automation's
// source: if-step gates, forEach where clauses, and the @filter annotation.
// Switch SUBJECTS are exempt -- switching on a decided value is the sanctioned
// fan-out shape.
func automationConditions(text string) []string {
	var out []string
	for _, m := range automationIfRE.FindAllStringSubmatch(text, -1) {
		out = append(out, m[1])
	}
	for _, m := range automationWhereRE.FindAllStringSubmatch(text, -1) {
		out = append(out, m[1])
	}
	for _, m := range automationFilterRE.FindAllStringSubmatch(text, -1) {
		out = append(out, m[1])
	}
	return out
}

// snippet truncates a condition for the finding message.
func snippet(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 80 {
		return s[:77] + "..."
	}
	return s
}

