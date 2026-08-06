package app

import (
	"errors"
	"io"
	"log/slog"
	"reflect"
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
// every test above green.
//
// An earlier version of this test said exactly that and then did not check it.
// It called the default and the real function in turn and asserted each returned
// nil -- two independent nil checks on a tree that is already clean, which an
// always-nil stub satisfies identically. Measured: swapping newApp's default for
// `func() error { return nil }` left the whole app suite green, and so did
// rewriting AssertSelfAuthenticatedRoutesFailClosed itself to return nil.
//
// So the identity is compared directly. That is the only assertion here that a
// stub cannot pass.
func TestNewAppDefaultsToTheRealSelfAuthAssertion(t *testing.T) {
	a := newApp(slog.New(slog.NewTextHandler(io.Discard, nil)), "test", Overrides{})
	if a.overrides.AssertSelfAuthenticatedSurface == nil {
		t.Fatal("newApp must default AssertSelfAuthenticatedSurface; a nil default would " +
			"panic at boot or, if guarded, skip the check entirely")
	}

	got := reflect.ValueOf(a.overrides.AssertSelfAuthenticatedSurface).Pointer()
	want := reflect.ValueOf(server.AssertSelfAuthenticatedRoutesFailClosed).Pointer()
	if got != want {
		t.Fatal("newApp defaulted AssertSelfAuthenticatedSurface to something OTHER than " +
			"server.AssertSelfAuthenticatedRoutesFailClosed. The boot check is then whatever " +
			"that something does -- including nothing -- and every other test here would still " +
			"pass, because they only assert the tree is currently clean (memql#3062).")
	}

	if err := a.overrides.AssertSelfAuthenticatedSurface(); err != nil {
		t.Errorf("the default assertion must pass on this tree: %v", err)
	}
}

// The exported entry point must be able to FAIL, not merely return nil on a
// clean tree.
//
// Rewriting AssertSelfAuthenticatedRoutesFailClosed to `return nil` was caught
// by nothing: the fixture tests exercise the unexported inner rule, and every
// test of the exported one asserted only that it returns nil today. That leaves
// the one function actually wired at boot deletable without a failure.
//
// This drives the exported rule with a deliberately uncertified entry appended,
// so it must report.
//
// BE PRECISE ABOUT WHAT IS AND IS NOT PINNED, because overclaiming here is the
// defect this test exists to correct. Three things can go wrong and only two are
// now caught:
//
//   - the boot path stops calling this function      -> caught, by the identity
//     comparison above;
//   - the RULE stops being able to report            -> caught, here;
//   - the one-line wrapper body is rewritten to
//     `return nil`                                    -> NOT caught.
//
// The last one has no test-reachable seam: the wrapper takes no arguments and
// reads the live declarations, which are clean, so nothing can drive it to a
// failing state from outside. It is a single delegating line and any rewrite of
// it is visible in a diff -- that is the whole of the remaining defence, and it
// is worth writing down rather than leaving a reader to assume otherwise.
func TestExportedSelfAuthAssertionCanActuallyFail(t *testing.T) {
	if err := server.AssertSelfAuthenticatedRoutesFailClosed(); err != nil {
		t.Fatalf("precondition: the live declarations must satisfy the invariant: %v", err)
	}
	if err := server.AssertSelfAuthenticatedFailClosedFor(
		append(server.SelfAuthenticatedPaths(), "/zz-uncertified/"),
		server.HandlerAuthorizedPaths(),
	); err == nil {
		t.Error("the exported rule accepted a self-authenticated route that is NOT certified as " +
			"failing closed. A rule that cannot fail is not a rule -- and this is the function " +
			"the boot check calls (memql#3062).")
	}
}
