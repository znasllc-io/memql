package auth

import "context"

// ChoiceIsThePersons reports whether a choice made on this call -- a route
// picked, a source pinned, a step re-run on a named model -- can be counted as
// the acting person's OWN choice.
//
// WHY THE SUBJECT IS NOT ENOUGH (vendor accounts design, K8 line 4 and owner
// decision O4). A route a person picks is them saying their own API key may
// pay; work nobody chose spends a key only when its owner switched "Let Auto
// use this key" on. Every stamp of "who chose" in the tree takes the call's
// acting user, and several credentials act as a person without being them:
//
//   - an APP SESSION's subject is the person who owns the machine. The coding
//     agent holding it can name a vendor source on an agent turn, or on a goal
//     it opens, and that is the agent choosing;
//   - DELEGATED ACCESS and a BADGE act as somebody under a grant;
//   - server code BORROWS a person's authority to work on their behalf.
//
// None of those is the person deciding, so a choice made through one is
// counted like no choice at all: the Auto rule, where a key pays only if its
// owner allowed that. The narrowing direction: this can only stop a key being
// spent.
//
// A PERSONAL ACCESS TOKEN COUNTS. It is the person's own credential, made by
// them and revocable by them, and a script they run with it is them acting.
// RequirePersonSession refuses one for a stricter reason that does not apply
// here: its acts create or change credentials, and this one only says how a
// person's own call is served.
//
// It answers for the moment a choice is MADE. A run executes later on a
// restored context that says nothing about the credential the choice came
// through, so whoever stamps a choice onto a row records this answer beside it
// (common.RouteChoice.ByMachine, common.StepOverride.ByMachine).
//
// Regression tests: choice_test.go.
func ChoiceIsThePersons(ctx context.Context) bool {
	ac, ok := AccessFromContext(ctx)
	if !ok || ac == nil {
		return false
	}
	id := ac.UserId
	claims, hasClaims := ClaimsFromContext(ctx)
	if !hasClaims || ac.IsAnonymousActor() || ac.IsConnector() || ac.Synthetic || ac.Unranked || ActsForNoPerson(ctx, id) {
		return false
	}
	// Claims that name somebody else were rebuilt on a person's behalf.
	if sub, _ := claims["sub"].(string); sub != id {
		return false
	}
	// On a forwarded hop the assertion is the only thing that says which
	// credential the call came from.
	if a, forwarded := ForwardedAuthorityFromContext(ctx); forwarded {
		if a.Version == "" || a.Kind != ForwardedPrincipalUser || a.Subject != id {
			return false
		}
		if a.CredentialClass != ForwardedClassUser && a.CredentialClass != ForwardedClassPat {
			return false
		}
	}
	if raw, present := claims["class"]; present {
		class, isString := raw.(string)
		if !isString || (class != "" && class != ForwardedClassUser && class != ForwardedClassPat) {
			return false
		}
	}
	if _, delegated := DelegationFromContext(ctx); delegated {
		return false
	}
	return true
}
