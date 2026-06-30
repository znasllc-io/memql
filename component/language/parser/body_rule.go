package parser

import (
	"fmt"
	"regexp"
)

// body_rule.go enforces Decision 5 of the construct-invocation ADR
// (docs/internal/design/construct-invocation-syntax-adr.md): `body { }` is
// the procedural marker. It is MANDATORY on `logic` (always, even one-liners)
// and FORBIDDEN on every other construct -- its presence therefore *means*
// "procedural code here." A `logic` without `body { }` and any non-`logic`
// construct *with* `body { }` are both rejected here in the parser (mirrored
// by the authoring-sandbox cross-reference pass + the whole-tree CI gate via
// component/memql/callgraph).
//
// The logic-mandatory-body half lives in the struct-form rewriter (emitLogic
// in rewriter.go), which is where the `body { }` wrapper is recognised and
// inlined. The forbidden-elsewhere half is enforced per non-logic construct:
//   - query / mutation -> rejectNonLogicBodyBlock (called from emitQuery /
//     emitMutation on the construct's source text);
//   - spec / trait     -> a token guard in parseSpecDecl;
//   - automation       -> a token guard in parseGoStyleAutomationBody;
//   - action           -> parseActionDecl's closed key set already rejects it;
//   - capability       -> parseCapabilityDecl already rejects it explicitly.

// bodyRuleBlockHeader matches a `body { ... }` block opener at statement
// position (start of source or after a newline, optional leading whitespace).
// It deliberately requires the `{` so a declarative field merely *named* body
// (e.g. a concept field `body string`) is never mistaken for the procedural
// marker.
var bodyRuleBlockHeader = regexp.MustCompile(`(^|[\n\r])[ \t]*body[ \t]*\{`)

// bodyRuleHint returns the construct's correct authoring form, pointing an
// author who wrongly added a `body { }` block at what they should write
// instead (ADR Decision 5 reference table).
func bodyRuleHint(kind string) string {
	switch kind {
	case "spec", "trait":
		return "a spec/trait body is a bare `return <expr>`"
	case "query", "mutation":
		return "a query/mutation body is declarative clauses (filter / shape / sort / insert / update)"
	case "action":
		return "an action body is the single `capability <name>(...)` call"
	case "automation":
		return "an automation body is `step ...` blocks"
	default:
		return "remove the `body { }` block (it is reserved for `logic`)"
	}
}

// bodyRuleForbiddenMessage is the canonical "non-logic construct carries a
// body { }" rejection text, shared by every enforcement site so the message
// is identical regardless of which parser path catches the violation.
func bodyRuleForbiddenMessage(kind, name string) string {
	return fmt.Sprintf("%s %q must not declare a `body { }` block -- `body { }` is the procedural marker reserved for `logic` (mandatory on logic, forbidden on every other construct; ADR Decision 5); %s", kind, name, bodyRuleHint(kind))
}

// rejectNonLogicBodyBlock returns the body-rule violation error when a
// non-logic construct's source text contains a `body { }` block, or nil when
// none is present. Used by the query / mutation struct-form rewriters, which
// operate on the construct's raw inner text.
func rejectNonLogicBodyBlock(kind, name, source string) error {
	if bodyRuleBlockHeader.MatchString(source) {
		return fmt.Errorf("%s", bodyRuleForbiddenMessage(kind, name))
	}
	return nil
}
