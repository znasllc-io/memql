package memql

import (
	"fmt"

	"github.com/znasllc-io/memql/component/auth"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// validateActorMemberPaths rejects an unknown actor-envelope member at
// LOAD time (#2625). Before this gate, `actor.displayName` was
// memqllint-green and failed only when the construct ran -- and on the
// logic/spec surfaces it did not even fail: the unknown key resolved to
// silent nil, a wrong answer with no error.
//
// The accepted set is the canonical envelope (auth.ActorEnvelopeFields,
// #2623), which is UNIFORM across all four runtime surfaces -- that
// unification is what collapses the story's per-surface column into one
// table, so a member valid at load is valid wherever the construct
// runs. The runtime errors stay as the backstop.
func validateActorMemberPaths(rawSource string, funcDef *languageParser.FunctionDef) error {
	if funcDef == nil {
		return nil
	}
	switch funcDef.Type {
	case languageParser.FunctionTypeQuery, languageParser.FunctionTypeMutation,
		languageParser.FunctionTypeLogic, languageParser.FunctionTypeAutomation:
	default:
		return nil
	}
	for _, ref := range languageParser.ActorMemberRefs(rawSource) {
		if _, ok := auth.ActorEnvelopeCanonicalName(ref.Name); !ok {
			return fmt.Errorf(
				"%s %q reads unknown actor member `actor.%s`; the auth envelope is a closed set (#2623) -- valid: %s",
				funcDef.Type, funcDef.Name, ref.Name, auth.ActorEnvelopeValidNames(),
			)
		}
	}
	return nil
}
