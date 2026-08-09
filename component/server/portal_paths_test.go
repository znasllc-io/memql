package server

import (
	"strings"
	"testing"
)

// The portal's static bundle must be PUBLIC, and it must be public as a
// PREFIX (memql#3314).
//
// Both halves are load-bearing and neither is obvious from the call site:
//
//   - Public, because the bundle contains the code that performs sign-in. A
//     bearer-gated bundle cannot be fetched by a browser that has no bearer
//     yet, so the sign-in flow could never load. What is public is bytes that
//     are identical for every visitor; the data the bundle then reads is
//     gated on the /memql/ws stream, behind the identity verifier.
//   - A prefix, because an SPA is not one URL. Its hashed assets and every
//     client-side route the fallback answers all live beneath the mount, so
//     an EXACT declaration of "/portal/" would leave /portal/assets/*.js
//     behind the verifier and the page would load blank.
//
// Asserted rather than trusted because both are silent when wrong: a missing
// entry shows up as a blank page in a browser, not as a failing build.
func TestPortalBundleIsDeclaredPublicAsAPrefix(t *testing.T) {
	t.Parallel()

	portal := PortalPaths()
	if len(portal) == 0 {
		t.Fatal("PortalPaths() is empty; this guard cannot pass vacuously")
	}

	public := PublicPaths()
	for _, p := range portal {
		if !strings.HasSuffix(p, "/") {
			t.Errorf("PortalPaths() entry %q has no trailing slash, so it declares an "+
				"EXACT path rather than a prefix -- every hashed asset beneath it would "+
				"stay behind the verifier and the portal would load blank", p)
		}
		found := false
		for _, q := range public {
			if q == p {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%q is not in PublicPaths(). The portal bundle carries the code that "+
				"performs sign-in, so a browser with no bearer must be able to fetch it "+
				"(memql#3314).", p)
		}
	}
}

// The converse: the portal prefix must not also be claimed by
// HandlerAuthorizedPaths(). That list means "not public, but authorizes
// internally"; the bundle authorizes nothing and is not trying to. Two
// contradictory declarations for one route is how a reviewer comes to believe
// a static file server is checking something.
func TestPortalIsNotAlsoDeclaredHandlerAuthorized(t *testing.T) {
	t.Parallel()

	for _, p := range PortalPaths() {
		for _, q := range HandlerAuthorizedPaths() {
			if q == p {
				t.Errorf("%q appears in BOTH PublicPaths() and HandlerAuthorizedPaths(); "+
					"the portal bundle is public and authorizes nothing", p)
			}
		}
	}
}
