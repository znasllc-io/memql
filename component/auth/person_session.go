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
// on every stream (component/grpc's LocalDevSubject, held equal by
// TestLocalDevSubjectMatchesTheInterceptor). Its access context is an
// owner with neither the Synthetic nor the Unranked flag, so the subject is the
// only thing that says nobody signed in.
const LocalDevSubject = "local-dev"

var errPersonSessionRequired = errors.New(PersonSessionRequired)

// IsPersonSessionRequired reports whether err is a RequirePersonSession refusal.
func IsPersonSessionRequired(err error) bool { return errors.Is(err, errPersonSessionRequired) }

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
// An empty class is a user's: the identity service issues a user token with no
// class claim, and the verifier reads that as ClassUser.
//
// Callers that need more (roletest re-reads the user, its rank, revocation epoch
// and expiry) ask that after this, so the shared part is decided once.
func RequirePersonSession(ctx context.Context) (string, error) {
	ac, ok := AccessFromContext(ctx)
	if !ok || ac == nil {
		return "", refusePersonSession("this call carries no signed-in person")
	}
	id := strings.TrimSpace(ac.UserId)
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
	}
	if sub, _ := claims["sub"].(string); strings.TrimSpace(sub) != id {
		return "", refusePersonSession("this call acts on somebody's behalf")
	}
	if claims["pat_id"] != nil {
		return "", refusePersonSession("a personal access token is not a session; sign in")
	}
	// A class claim that is not a string is malformed, and refused as one.
	if raw, present := claims["class"]; present {
		if class, isString := raw.(string); !isString || (class != "" && class != ForwardedClassUser) {
			return "", refusePersonSession("a machine credential is not a person")
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
