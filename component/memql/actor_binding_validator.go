package memql

import (
	"fmt"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// validateActorBinding enforces the #2621 rule: reading the auth
// envelope is a declared capability. A query / mutation / logic /
// automation whose body references `actor.*` must carry the `@actor`
// annotation in its preamble; declared-but-unused is fine (the exact
// shape of the logic event-binding rule, memql#1706). Spec and trait
// bodies keep their INVERSE rule (direct actor reads are load-rejected
// there, epic #2281) and never reach this validator; the seed-file
// `@actor("system")` is a different construct on a different keyword.
//
// The detector lives in the parser package
// (languageParser.ActorRefInSource / ActorDeclaredInSource) so the
// memqlmigrate codemod and this validator share one definition of
// "reads the actor".
func validateActorBinding(rawSource string, funcDef *languageParser.FunctionDef) error {
	if funcDef == nil {
		return nil
	}
	switch funcDef.Type {
	case languageParser.FunctionTypeQuery, languageParser.FunctionTypeMutation,
		languageParser.FunctionTypeLogic, languageParser.FunctionTypeAutomation:
	default:
		return nil
	}
	body := extractFunctionBody(rawSource)
	if body == "" {
		return nil
	}
	if !languageParser.ActorRefInSource(stripAttrLines(body)) {
		return nil
	}
	if languageParser.ActorDeclaredInSource(rawSource) {
		return nil
	}
	return fmt.Errorf(
		"%s %q reads the auth envelope (actor.*) but does not declare `@actor` in its preamble; "+
			"reading the actor is a declared capability (#2621) -- add a bare `@actor` annotation line above the declaration",
		funcDef.Type, funcDef.Name,
	)
}
