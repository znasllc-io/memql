package app

import (
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/identity/verifier"
	"github.com/znasllc-io/memql/component/server"
)

// self_authenticated_wiring_test.go -- memql#3062.
//
// The third-tier check has to run on EVERY binary, which is what distinguishes
// it from the surface assertion beside it. That one runs only when
// `a.identityVerifier == nil`; the hole this one guards opens on the binaries
// where a verifier IS installed, because SelfAuthenticatedPaths() is precisely
// the declaration that makes the verifier middleware step aside there.
//
// So a version of this check that inherited the `identityVerifier == nil`
// branch would be inert exactly where it matters, and every test would pass.

// The error path must be reached on a binary WITH a verifier -- the case the
// neighbouring assertion skips, and the one this check exists for.
func TestCreateHTTPServerRejectsUncertifiedSelfAuthRouteWithVerifier(t *testing.T) {
	a, fatals := newSurfaceWiringApp(t)
	a.identityVerifier = &verifier.Verifier{}

	a.overrides.AssertSelfAuthenticatedSurface = func() error {
		return errors.New("not certified: /webhooks/")
	}

	a.createHTTPServer()

	if len(*fatals) == 0 {
		t.Fatal("a self-authenticated route with no fail-closed certification must fail the " +
			"boot. This check runs where the verifier IS installed -- if it inherited the " +
			"`identityVerifier == nil` branch beside it, it would be inert on exactly the " +
			"binaries whose middleware it tells to step aside (memql#3062)")
	}
	if joined := strings.Join(*fatals, " | "); !strings.Contains(joined, "/webhooks/") {
		t.Errorf("the fatal must name the offending route; got: %s", joined)
	}
}

// And on a binary WITHOUT one, so the check is not accidentally scoped to
// either branch.
func TestCreateHTTPServerRejectsUncertifiedSelfAuthRouteWithoutVerifier(t *testing.T) {
	a, fatals := newSurfaceWiringApp(t)
	a.identityVerifier = nil

	a.overrides.AssertSelfAuthenticatedSurface = func() error {
		return errors.New("not certified: /webhooks/")
	}

	a.createHTTPServer()

	if len(*fatals) == 0 {
		t.Fatal("the third-tier check must run on a no-verifier binary too")
	}
}

// The live tree must boot: the real declarations satisfy the invariant.
func TestCreateHTTPServerAcceptsTheLiveSelfAuthDeclarations(t *testing.T) {
	a, fatals := newSurfaceWiringApp(t)
	a.identityVerifier = &verifier.Verifier{}

	a.createHTTPServer()

	for _, f := range *fatals {
		if strings.Contains(f, "self-authenticated") {
			t.Fatalf("the live declarations must boot; got: %s", f)
		}
	}
}

// The injected seam only proves the wiring if the REAL assertion is what gets
// injected by default -- otherwise replacing the default with a no-op leaves
// every test above green. Mirrors TestNewAppDefaultsToTheRealSurfaceAssertion.
func TestNewAppDefaultsToTheRealSelfAuthAssertion(t *testing.T) {
	a := newApp(slog.New(slog.NewTextHandler(io.Discard, nil)), "test", Overrides{})
	if a.overrides.AssertSelfAuthenticatedSurface == nil {
		t.Fatal("newApp must default AssertSelfAuthenticatedSurface; a nil default would " +
			"panic at boot or, if guarded, skip the check entirely")
	}
	if err := a.overrides.AssertSelfAuthenticatedSurface(); err != nil {
		t.Errorf("the default assertion must be the real one and must pass on this tree: %v", err)
	}
	// And it must actually BE the real one, not a stub that always passes.
	if err := server.AssertSelfAuthenticatedRoutesFailClosed(); err != nil {
		t.Errorf("precondition: the live declarations must satisfy the invariant: %v", err)
	}
}
