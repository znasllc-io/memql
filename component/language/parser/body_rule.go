package parser

import (
	"fmt"
	"regexp"
	"strings"
)

// body_rule.go enforces the body rule: no MemQL construct has a `body { }`
// block. Decision 5 of the construct-invocation ADR
// (docs/internal/design/construct-invocation-syntax-adr.md) made `body { }`
// the procedural marker, mandatory on `logic`; epic memql#5370 retired it
// there too, so a logic's and an automation's statements follow their args
// block directly. The rule is enforced per construct (mirrored by the
// authoring-sandbox cross-reference pass via component/memql/callgraph):
//   - logic / automation -> the statement parser refuses the wrapper by name
//     (body_block_retired, v1_body.go);
//   - query / mutation   -> rejectNonLogicBodyBlock (called from emitQuery /
//     emitMutation on the construct's source text);
//   - spec / trait       -> a token guard in parseSpecDecl;
//   - action             -> parseActionDecl's closed key set already rejects it;
//   - capability         -> parseCapabilityDecl already rejects it explicitly.

// bodyRuleBlockHeader matches a `body { ... }` block opener at statement
// position (start of source or after a newline, optional leading whitespace).
// It deliberately requires the `{` so a declarative field merely *named* body
// (e.g. a concept field `body string`) is never mistaken for the block.
var bodyRuleBlockHeader = regexp.MustCompile(`(^|[\n\r])[ \t]*body[ \t]*\{`)

// bodyRuleHint returns the construct's correct authoring form, pointing an
// author who wrongly added a `body { }` block at what they should write
// instead.
func bodyRuleHint(kind string) string {
	switch kind {
	case "spec", "trait":
		return "a spec or trait is one lambda, `= row => <predicate>`"
	case "query", "mutation":
		return "a query/mutation body is declarative clauses (filter / shape / sort / insert / update)"
	case "action":
		return "an action body is the single `capability <name>(...)` call"
	default:
		return "a logic's and an automation's statements follow their args block directly"
	}
}

// bodyRuleForbiddenMessage is the canonical "construct carries a body { }"
// rejection text, shared by every enforcement site so the message is
// identical regardless of which parser path catches the violation.
func bodyRuleForbiddenMessage(kind, name string) string {
	return fmt.Sprintf("%s %q must not declare a `body { }` block -- no MemQL construct has one; %s", kind, name, bodyRuleHint(kind))
}

// rejectNonLogicBodyBlock returns the body-rule violation error when a
// non-logic construct's source text contains a `body { }` block, or nil when
// none is present. Used by the query / mutation struct-form rewriters, which
// operate on the construct's raw inner text.
//
// The refusal names the `body` keyword (rewrite_errors.go): source is the
// construct's body, which is what the offset indexes.
func rejectNonLogicBodyBlock(kind, name, source string) error {
	if loc := bodyRuleBlockHeader.FindStringIndex(source); loc != nil {
		at := loc[0] + strings.Index(source[loc[0]:loc[1]], "body")
		return refuseAtBody(at, len("body"), fmt.Errorf("%s", bodyRuleForbiddenMessage(kind, name)))
	}
	return nil
}
