package app

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/server"
)

// The declaration machinery in component/server is tested there. What this
// covers is the wiring: that createHTTPServer actually consults it, and only
// where it should.
//
// Without this, deleting the call is invisible -- the component/server tests
// still pass, because they exercise the function directly. The check would
// become dead code and the identity binary would go back to inheriting its
// unauthenticated surface by omission (memql#2939).

func newSurfaceWiringApp(t *testing.T) (*App, *[]string) {
	t.Helper()
	fatals := []string{}
	a := &App{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	a.overrides.FatalWithLogger = func(_ *slog.Logger, msg string, args ...any) {
		parts := []string{msg}
		for _, v := range args {
			if s, ok := v.(string); ok {
				parts = append(parts, s)
			} else if err, ok := v.(error); ok {
				parts = append(parts, err.Error())
			}
		}
		fatals = append(fatals, strings.Join(parts, " "))
	}
	// createHTTPServer builds a server after the assertion; a nil-returning
	// override keeps the test on the assertion rather than the transport.
	a.overrides.NewHTTPServer = func(...server.ServerArg) (*server.Server, error) {
		return nil, nil
	}
	return a, &fatals
}

// The live tree is declared, so a nil verifier must NOT fatal. If this fails,
// the identity binary cannot boot.
func TestCreateHTTPServerAcceptsDeclaredSurface(t *testing.T) {
	a, fatals := newSurfaceWiringApp(t)
	a.identityVerifier = nil

	a.createHTTPServer()

	if len(*fatals) != 0 {
		t.Fatalf("a declared surface must boot; got fatal(s): %v", *fatals)
	}
}

// The assertion must be reached on the no-middleware path. Pointing it at an
// undeclared route set proves createHTTPServer consults the declarations rather
// than merely containing a call that never fires.
func TestCreateHTTPServerRejectsUndeclaredSurface(t *testing.T) {
	if err := server.AssertUnauthenticatedSurfaceDeclared(); err != nil {
		t.Fatalf("precondition: the live surface should be declared, got %v", err)
	}

	a, fatals := newSurfaceWiringApp(t)
	a.identityVerifier = nil

	// Substitute a route set that is not fully declared, so this exercises the
	// error path rather than merely confirming a good tree boots -- which stays
	// true if the assertion is deleted or downgraded to a log line.
	restore := server.SetContractRoutesForTest([]string{"/healthz", "/internal/undeclared"})
	defer restore()

	a.createHTTPServer()

	if len(*fatals) == 0 {
		t.Fatal("an undeclared route must fail the boot -- a warning is exactly the " +
			"signal that went unread when the automations routes were added (#2937)")
	}
	joined := strings.Join(*fatals, " | ")
	if !strings.Contains(joined, "/internal/undeclared") {
		t.Errorf("the fatal must name the offending route so the fix is obvious; got: %s", joined)
	}
}
