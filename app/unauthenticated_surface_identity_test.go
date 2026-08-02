//go:build identity

package app

import (
	"net/http"
	"strings"
	"testing"
)

// The scope argument at the call site had no test.
//
// createHTTPServer passes a.unauthenticatedSurfaceRoutes(!verifierRequired).
// TestUnauthenticatedSurfaceRoutesIncludeMuxRoutesForIdentity exercises that
// helper directly with both values, so it proves the ASSEMBLY is right and
// says nothing about which value the caller actually passes. Reverting the
// argument to a literal false -- exactly the review-round-2 defect, dropping
// the identity binary from its 7 paths back to the 5 contract routes -- left
// `go test ./app/` AND `go test -tags identity ./app/...` both green.
//
// This file is identity-tagged because verifierRequired is a build-tag const:
// only in this binary is `!verifierRequired` true, so only here can the call
// site's choice be observed. It is the branch that runs in production.
func TestIdentityBinaryAssertsTheRoutesItRegisters(t *testing.T) {
	a, fatals := newSurfaceWiringApp(t)
	a.identityVerifier = nil
	a.mux = http.NewServeMux()

	// A route app code mounts, declared nowhere. On the identity binary the
	// assertion must see it; under the contract-only scope it is invisible.
	a.handleRoute("POST /internal/dump-secrets", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	a.createHTTPServer()

	if len(*fatals) == 0 {
		t.Fatal("the identity binary must assert the routes app code registers, not just the " +
			"five contract routes -- an undeclared a.handleRoute route was accepted, so the " +
			"scope argument at the createHTTPServer call site has been narrowed (#2939)")
	}
	if !strings.Contains((*fatals)[0], "/internal/dump-secrets") {
		t.Errorf("the fatal must name the undeclared route, got: %v", *fatals)
	}
}
