package auth

import "strings"

// NamesNoPerson reports whether an actor id names nobody a person could be:
// blank, or an actor the engine SYNTHESIZED -- a system actor (which covers an
// automation's `system:automation:<name>` and the maintenance principal), a
// connector, or the anonymous actor.
//
// For the places that hold only the STRING -- a router request's UserId, a
// ledger row -- and must decide something only a person can be the subject
// of. An app session is the case that needs it: the session runs under a
// credential whose subject is a person, on that person's own machine, so a
// call acting for nobody has no machine to open one on (memql app door,
// component/memql/app_provider.go).
//
// Every synthesized id carries its prefix precisely so a stored value says
// what minted it; ids the identity service mints carry none of them.
func NamesNoPerson(id string) bool {
	id = strings.TrimSpace(id)
	return id == "" ||
		IsSystemActorId(id) ||
		strings.HasPrefix(id, connectorUserIdPrefix) ||
		id == AnonymousUserId
}
