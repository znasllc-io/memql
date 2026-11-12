package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/core/common"
)

// PersonSessionRequired is the code RequirePersonSession's every refusal begins
// with, followed by a colon and a sentence saying who may do this.
const PersonSessionRequired = "person_session_required"

// LocalDevSubject is the subject the no-auth troubleshooting interceptor stamps
// on every stream; component/grpc's LocalDevSubject is defined as this value.
// Its access context is an owner with neither the Synthetic nor the Unranked
// flag, so the subject is the only thing that says nobody signed in.
const LocalDevSubject = "local-dev"

var errPersonSessionRequired = errors.New(PersonSessionRequired)

// IsPersonSessionRequired reports whether err is a RequirePersonSession refusal.
func IsPersonSessionRequired(err error) bool { return errors.Is(err, errPersonSessionRequired) }

// The reasons more than one path gives. A badge is a person signed in under a
// role ceiling someone granted them, not a machine, and is told so.
const (
	badgeRefusal   = "a badge session acts under a ceiling granted to it, so sign in as yourself"
	patRefusal     = "a personal access token is not a session; sign in"
	machineRefusal = "a machine credential is not a person"
)

func refusePersonSession(why string) error {
	return fmt.Errorf("%w: Only a person signed in as themselves, in their own session, may do this; %s.", errPersonSessionRequired, why)
}

// RequirePersonSession answers the user id of a person's own signed-in session
// and refuses everything else.
//
// WHY A SEPARATE GATE, BELOW THE CAPABILITY. A part (`execute app:<id>/<part>`)
// says which rank may act; it does not say who is at the keyboard. The acts this
// gates -- role testing, exporting an account, moving rows between accounts --
// are a person's decision, and every other credential that can hold an owner's
// parts is refused here: the operator credential and the local-dev shim (an owner
// nobody signed in as), borrowed authority and every synthetic or unranked actor
// (the cluster acting), a connector, a machine credential of any class (service
// account, app session, worker token, node, a role-test persona's own token), a
// personal access token, delegated access, and any call made inside a run, whose
// journal and model loop are not a person deciding.
//
// THE CLAIMS MUST NAME THE ACTOR. Borrowed authority and the system helpers each
// stamp claims of their own, so a context whose claims' subject differs from its
// access context's user was rebuilt on somebody's behalf, not opened by them.
//
// The ids are compared exactly. A subject or user id with stray whitespace is
// not trimmed into a match: the identity service mints neither with any, so a
// padded one was written by something other than a sign-in.
//
// An empty class is a user's: the identity service issues a user token with no
// class claim, and the verifier reads that as ClassUser.
//
// A FORWARDED HOP IS READ OFF ITS ASSERTION. A mesh receiver rebuilds the
// claims from the forwarded authority (BindForwardedContext), and those carry no
// class, no pat_id and no delegation; the credential class travels on the
// assertion alone. So when an assertion is on the context it must be a user
// principal of class `user`, and a present but zero assertion -- what a refused
// binding leaves behind so an older one cannot be used -- is refused. The same
// person's own direct stream carries a `user` assertion too, so this refuses
// nothing a sign-in opened.
//
// RESTORED WORK IS REFUSED BY ITS MARK. ContextWithPersistedOwner rebuilds a
// context for work carried on later on a person's behalf, and its assertion of a
// recorded `user` grant is indistinguishable from the person's own forwarded
// session, so that function marks what it returns (restoredWork) and a marked
// context is refused whatever its grant.
//
// WHAT THE MARK DOES NOT COVER. It is local to the node that restored the work
// and is never sent, so a restored context forwarded to another node arrives as
// a plain `user` assertion, which this gate cannot tell from a person's own
// forwarded session. The mesh receivers (the AI and worker forwards, the run
// relay) do not bind the run either, so the run check does not stand in on the
// receiving node. It stands in on the restoring node: a goal's run restores its
// owner and then binds the run (app/work_execution_context.go), so a step that
// reached one of these acts there is refused by the run check as well as by the
// mark. The acts this gates are answered on the node a person's stream reached,
// which is what keeps a forwarded restoration from reaching them.
//
// INTERNAL ORIGIN IS NOT CONSULTED. Server code stamps it beneath a person's own
// request to open one row-tier write; the person is still the one asking, so the
// stamp does not change the answer. A context that is internal origin and nothing
// else carries no person's access context and is refused by the first check.
//
// Callers that need more (roletest re-reads the user, its rank, revocation epoch
// and expiry) ask that after this, so the shared part is decided once.
func RequirePersonSession(ctx context.Context) (string, error) {
	ac, ok := AccessFromContext(ctx)
	if !ok || ac == nil {
		return "", refusePersonSession("this call carries no signed-in person")
	}
	id := ac.UserId
	claims, hasClaims := ClaimsFromContext(ctx)
	switch {
	case ac.IsAnonymousActor():
		return "", refusePersonSession("nobody is signed in")
	case ac.IsConnector():
		return "", refusePersonSession("a connector is not a person")
	case ac.Synthetic || ac.Unranked || ActsForNoPerson(ctx, id):
		return "", refusePersonSession("this call is the cluster acting, not a person")
	case strings.EqualFold(id, LocalDevSubject) || claims[forwardedClaimLocalDev] != nil:
		return "", refusePersonSession("sign-in is turned off on this cluster")
	case !hasClaims:
		return "", refusePersonSession("this call carries no sign-in")
	case ac.RoleStandIn || restoredWork(ctx):
		return "", refusePersonSession("work continued on a person's behalf is not that person's session")
	}
	if sub, _ := claims["sub"].(string); sub != id || id != strings.TrimSpace(id) {
		return "", refusePersonSession("this call acts on somebody's behalf")
	}
	if a, forwarded := ForwardedAuthorityFromContext(ctx); forwarded {
		switch {
		case a.Version == "" || a.Kind != ForwardedPrincipalUser || a.Subject != id:
			return "", refusePersonSession("this call carries no person's assertion")
		case a.CredentialClass == ForwardedClassBadge:
			return "", refusePersonSession(badgeRefusal)
		case a.CredentialClass == ForwardedClassPat:
			return "", refusePersonSession(patRefusal)
		case a.CredentialClass != ForwardedClassUser:
			return "", refusePersonSession(machineRefusal)
		}
	}
	if claims["pat_id"] != nil {
		return "", refusePersonSession(patRefusal)
	}
	// A class claim that is not a string is malformed, and refused as one.
	if raw, present := claims["class"]; present {
		class, isString := raw.(string)
		switch {
		case isString && class == ForwardedClassBadge:
			return "", refusePersonSession(badgeRefusal)
		case !isString || (class != "" && class != ForwardedClassUser):
			return "", refusePersonSession(machineRefusal)
		}
	}
	if _, delegated := DelegationFromContext(ctx); delegated {
		return "", refusePersonSession("delegated access is not the person themselves")
	}
	if _, inRun := common.RunFromContext(ctx); inRun {
		return "", refusePersonSession("a call made inside a run is not a person deciding, so do it from your own session")
	}
	return id, nil
}
