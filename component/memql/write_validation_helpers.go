package memql

import (
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
)

// Helpers shared by the server-side write validators (agent kind, agent role
// slug uniqueness, agent role predefined-lock, account self-archive).
//
// They used to live in the cognition participant/utterance validators, which
// were the first writers to need them; those went with the cognition concepts
// (epic memql#4988) and these came here rather than being duplicated into each
// remaining caller.

// stringFromAny renders a decoded payload value as a string. A nil reads as
// empty; a non-string is formatted rather than dropped, so a validator
// comparing against a literal sees what was actually written.
func stringFromAny(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

// isSystemActor reports whether a write is the engine acting on its own
// behalf -- the seed materializer, an automation's system actor, a
// maintenance sweep -- rather than a person. Either the resolved role is
// "system", or the actor string carries the "system:" prefix the internal
// call sites stamp.
func isSystemActor(identity auth.UserIdentity, actor string) bool {
	if strings.EqualFold(strings.TrimSpace(identity.Role), "system") {
		return true
	}
	return strings.HasPrefix(strings.TrimSpace(actor), "system:")
}
