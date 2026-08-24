package auth

import (
	"context"
	"strings"
)

// The CONNECTOR ACTOR (epic memql#4378, D4). A connector -- the code
// that fills a mirror from its origin, or pushes a MemQL-origin change
// out to an external mirror -- is an internal WRITER, and it needs a
// name.
//
// This is the characterised internal actor memql#4366 asks for, FOR
// THIS CLASS OF WRITER ONLY. The planner's system actor is a separate
// decision and is not this: that one acts on a user's behalf and has a
// user to name, while a connector acts on nobody's behalf and is
// admitted by what a concept DECLARES about it.
//
// # Why a named actor rather than an internal-origin stamp
//
// The tree already has a way for trusted server-side Go to write past a
// row-authz tier: stamp auth.ContextWithInternalOrigin for one write
// (rowauthz_write_guard.go's first escape). That mechanism says "this
// caller is the engine", which is true of a connector and useless for
// deciding anything -- it would admit the Shopify connector to campaign
// rows, to identity rows, and to every mirror belonging to some other
// origin.
//
// A connector actor instead says WHICH connector is writing, and row
// admission answers with the concept's own declaration: admitted to the
// concepts whose @origin or @mirroredTo names it, and to nothing else
// (component/memql/rowauthz_connector.go). That is a targeted rule, not
// a blanket bypass, and it is the property that makes "a mirror is
// read-only by construction" a statement a reader can trust rather than
// a convention.
//
// # It is never minted from a request
//
// There is no header, claim, token class or role value that produces
// one of these. The only constructor is ConnectorActor, called by the
// runtime immediately before it invokes a connector's contract method.
// RoleConnector deliberately sits outside ValidRoles() for that reason:
// it is not a role a user row may carry, so no identity can be issued
// with it and no delegation can be capped to it.

// RoleConnector is the role a connector actor carries.
//
// It is NOT in ValidRoles(): a connector is not a kind of user, and a
// user must never be assignable to it. It is also deliberately outside
// the rank model, so it resolves as the LEAST privileged role anywhere
// that ranks roles -- a connector's power comes entirely from the
// targeted row-admission rule keyed on its name, never from the role
// itself. If this value ever leaked into a normal request context, it
// would grant less than a reader, not more.
const RoleConnector Role = "connector"

// connectorUserIdPrefix is the prefix of the synthetic UserId a
// connector actor carries.
//
// The actor needs a non-empty UserId because several engine surfaces
// treat a blank one as "no identity" and refuse -- and a refusal is the
// wrong answer for a writer that is authorized by a different rule. The
// prefix makes the value self-describing in a `createdBy` stamp and in
// an audit line: a row a connector wrote says so, by name, forever.
//
// It cannot collide with a real user id: ids are minted by the identity
// service and none of them carry this prefix.
const connectorUserIdPrefix = "connector:"

// ConnectorActor builds the AccessContext a connector runs under.
//
// Returns nil for a blank name rather than an actor with no name, so a
// caller that cannot say which connector it is gets no actor at all --
// and is then refused by every tier -- instead of an unnamed writer
// that row admission would have to guess about.
func ConnectorActor(name string) *AccessContext {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	return &AccessContext{
		UserId:        connectorUserIdPrefix + name,
		Role:          RoleConnector,
		ConnectorName: name,
	}
}

// ContextWithConnectorActor stamps a connector actor onto ctx.
//
// A blank name returns ctx UNCHANGED, matching ContextWithUserActor's
// contract: a caller that cannot resolve the connector must refuse
// before calling rather than rely on this, or the work runs as whoever
// the inbound caller happened to be.
func ContextWithConnectorActor(ctx context.Context, name string) context.Context {
	ac := ConnectorActor(name)
	if ac == nil {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return ContextWithAccess(ctx, ac)
}

// ConnectorFromContext returns the connector name the caller is acting
// as, and whether it is acting as one at all.
//
// It reads ConnectorName rather than parsing the UserId prefix. The
// prefix exists so a stored `createdBy` is legible; it is not a
// protocol, and inferring identity from a string shape is how a value
// a caller can influence becomes an authorization decision.
func ConnectorFromContext(ctx context.Context) (string, bool) {
	ac, ok := AccessFromContext(ctx)
	if !ok {
		return "", false
	}
	return ac.ConnectorNameValue()
}

// ConnectorNameValue returns the connector this actor is, if it is one.
//
// Both halves must agree: a context carrying a connector name but some
// other role, or RoleConnector with no name, is a malformed actor and
// answers "not a connector" -- which denies rather than admits, since
// the connector rule is the only thing that would have admitted it.
func (ac *AccessContext) ConnectorNameValue() (string, bool) {
	if ac == nil {
		return "", false
	}
	name := strings.TrimSpace(ac.ConnectorName)
	if name == "" || ac.Role != RoleConnector {
		return "", false
	}
	return name, true
}

// IsConnector reports whether this actor is a connector.
func (ac *AccessContext) IsConnector() bool {
	_, ok := ac.ConnectorNameValue()
	return ok
}
