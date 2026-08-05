package server

import (
	"strings"
	"testing"
)

// self_authenticated_declared_test.go -- memql#3062, the tier's own boot gate.
//
// The third tier (SelfAuthenticatedPaths) is the only declaration that makes
// the verifier middleware STEP ASIDE on a node that installs it. That is a
// strictly stronger power than the other two lists have:
//
//   - PublicPaths() also bypasses the middleware, but it says the route is
//     genuinely public everywhere, which is a claim a reviewer reads as such.
//   - HandlerAuthorizedPaths() is consulted ONLY where no verifier runs, so it
//     cannot open a hole on a verifier-consuming node at all.
//
// So an entry added to SelfAuthenticatedPaths() removes bearer verification on
// every verifier-consuming node -- the bff included -- and nothing at boot
// checked that such a route has any authentication of its own.
//
// It holds today only by construction: both SelfAuthenticatedPaths() and the
// tail of HandlerAuthorizedPaths() return InboundWebhookPaths(). Two lists that
// agree by construction are exactly the pair that drifts, and the drift is
// silent and one-directional -- toward an unauthenticated route.
//
// The invariant asserted here is the one the tier's own contract states: a
// route qualifies for the exemption ONLY if it fails closed with no
// credentials, which is precisely what HandlerAuthorizedPaths() membership
// certifies. So every self-authenticated route must also be declared there.

// The live lists must satisfy the invariant.
func TestSelfAuthenticatedRoutesAreAlsoHandlerAuthorized(t *testing.T) {
	if err := AssertSelfAuthenticatedRoutesFailClosed(); err != nil {
		t.Fatalf("the live declarations violate the third tier's own contract: %v", err)
	}
}

// The rule must actually catch a violation -- tested against fixtures, because
// asserting only the live lists proves the tree is clean today without proving
// the check would notice a route that is not.
func TestSelfAuthenticatedRuleCatchesAnUndeclaredExemption(t *testing.T) {
	for _, tc := range []struct {
		name              string
		selfAuth, handler []string
		wantErr           bool
	}{
		{
			name:     "self-authenticated route with no fail-closed declaration",
			selfAuth: []string{"/inbound/"},
			handler:  []string{"/automations/"},
			wantErr:  true,
		},
		{
			name:     "declared in both -- the live shape",
			selfAuth: []string{"/inbound/"},
			handler:  []string{"/automations/", "/inbound/"},
			wantErr:  false,
		},
		{
			name:     "empty self-auth set is trivially fine",
			selfAuth: nil,
			handler:  []string{"/automations/"},
			wantErr:  false,
		},
		{
			// A prefix declaration on the handler side does NOT cover a
			// different exact route: being allowed to skip the bearer on
			// /inbound/ says nothing about /webhooks/.
			name:     "a different prefix does not certify it",
			selfAuth: []string{"/webhooks/"},
			handler:  []string{"/inbound/"},
			wantErr:  true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := assertSelfAuthenticatedFailClosed(tc.selfAuth, tc.handler)
			if tc.wantErr && err == nil {
				t.Fatalf("expected a violation for selfAuth=%v handler=%v, got none", tc.selfAuth, tc.handler)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no violation for selfAuth=%v handler=%v, got: %v", tc.selfAuth, tc.handler, err)
			}
		})
	}
}

// The error has to say what to do about it. A boot fatal on an auth surface is
// read by someone who did not write the code that tripped it.
func TestSelfAuthenticatedViolationNamesTheRouteAndTheRemedy(t *testing.T) {
	err := assertSelfAuthenticatedFailClosed([]string{"/webhooks/"}, []string{"/inbound/"})
	if err == nil {
		t.Fatal("expected a violation")
	}
	msg := err.Error()
	for _, want := range []string{"/webhooks/", "SelfAuthenticatedPaths", "HandlerAuthorizedPaths", "fails closed"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the violation message must mention %q; got:\n  %s", want, msg)
		}
	}
}
