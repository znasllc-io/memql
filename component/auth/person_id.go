package auth

import (
	"context"
	"strings"
)

// clusterPrincipalPrefix starts the ids of the cluster's own named principals:
// the proving suite (component/proving), the work journal
// (component/automations journal.go) and the operator credential's stream
// subject (component/grpc OperatorSubject). None is a person, and no id the
// identity service mints starts with it -- a canonical person id starts
// `v1:identity:user:`, and a canonical row id of the cluster namespace starts
// `v1:cluster:`.
const clusterPrincipalPrefix = "cluster:"

// NamesNoPerson reports whether an actor id names nobody a person could be:
// blank, or an actor the engine SYNTHESIZED -- a system actor (which covers an
// automation's `system:automation:<name>` and the maintenance principal), one
// of the cluster's own named principals, a connector, or the anonymous actor.
//
// For the places that hold only the STRING -- a router request's UserId, a
// ledger row -- and must decide something only a person can be the subject
// of. An app session is the case that needs it: the session runs under a
// credential whose subject is a person, on that person's own machine, so a
// call acting for nobody has no machine to open one on (memql app door,
// component/memql/app_provider.go). Where the context is at hand,
// ActsForNoPerson asks the actor's own flag too.
//
// Every synthesized id carries its prefix precisely so a stored value says
// what minted it; ids the identity service mints carry none of them.
func NamesNoPerson(id string) bool {
	id = strings.TrimSpace(id)
	return id == "" ||
		IsSystemActorId(id) ||
		strings.HasPrefix(id, clusterPrincipalPrefix) ||
		strings.HasPrefix(id, connectorUserIdPrefix) ||
		id == AnonymousUserId
}

// ActsForNoPerson is NamesNoPerson with the context's actor asked as well:
// the id names nobody, OR it is the id of the context's own actor and that
// actor is Synthetic -- the cluster acting, which can never be a row's owner
// and so can never be the person an app session is opened for. The flag is
// set only by the synthetic constructors, so a principal minted under a
// prefix NamesNoPerson does not know is still nobody here.
//
// THE IDS MUST MATCH. A call that names a person explicitly while running
// under the cluster's own context acts for that person; and borrowed authority
// (ContextWithUserActor) is not Synthetic at all -- its UserId is a real
// person's.
func ActsForNoPerson(ctx context.Context, id string) bool {
	if NamesNoPerson(id) {
		return true
	}
	ac, ok := AccessFromContext(ctx)
	return ok && ac != nil && ac.Synthetic && strings.TrimSpace(ac.UserId) == strings.TrimSpace(id)
}
