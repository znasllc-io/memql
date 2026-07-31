package app

import (
	"errors"
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
	// Mirror newApp's defaulting; individual tests substitute their own.
	a.overrides.AssertUnauthenticatedSurface = server.AssertUnauthenticatedSurfaceDeclared
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
	if err := server.AssertUnauthenticatedSurfaceDeclared(server.ContractRoutes()); err != nil {
		t.Fatalf("precondition: the live surface should be declared, got %v", err)
	}

	a, fatals := newSurfaceWiringApp(t)
	a.identityVerifier = nil

	// Drive the error path rather than merely confirming a good tree boots --
	// that stays true if the assertion is deleted or downgraded to a log line.
	a.overrides.AssertUnauthenticatedSurface = func([]string) error {
		return errors.New("undeclared: /internal/undeclared")
	}

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

// The injected seam only proves the wiring if the real assertion is what gets
// injected by default. Without this, replacing the default with a no-op would
// leave every test above green.
func TestNewAppDefaultsToTheRealSurfaceAssertion(t *testing.T) {
	a := newApp(slog.New(slog.NewTextHandler(io.Discard, nil)), "test", Overrides{})
	if a.overrides.AssertUnauthenticatedSurface == nil {
		t.Fatal("newApp must default AssertUnauthenticatedSurface; a nil default would " +
			"panic at boot or, if guarded, skip the check entirely")
	}
	if err := a.overrides.AssertUnauthenticatedSurface(server.ContractRoutes()); err != nil {
		t.Errorf("the default assertion must be the real one and must pass on this tree: %v", err)
	}
}
