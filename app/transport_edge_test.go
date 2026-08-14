//go:build edge

package app

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/server"
)

// The edge node runs with no identity verifier at all (verifierRequired=false,
// auth_mode_edge.go, memql#3710) because it is not an auth boundary -- so its
// one registered route, the site-serving mount at "/"
// (transport_edge.go's mountEdgeEndpoints), has to survive the same
// unauthenticated-surface boot check every other undeclared route fails.
//
// This does NOT go through createHTTPServer(): the wholeMux branch it would
// take is chosen by `!verifierRequired`, a build-tag const, so calling
// createHTTPServer() directly would only exercise the branch belonging to
// WHATEVER tag the test binary happens to be compiled with -- exactly the
// gap TestUnauthenticatedSurfaceRoutesIncludeMuxRoutesForIdentity (the
// generic, untagged test file) already works around for the identity
// binary's own mux routes. Assembling the whole-mux route list directly, as
// that test does, makes this deterministic.
//
// MUST live under //go:build edge, and not in the untagged
// unauthenticated_surface_wiring_test.go where an earlier version of this
// test lived: component/server.EdgePaths()' contribution to PublicPaths()
// is itself build-tag-scoped to the edge binary (fix round 2 -- PublicPaths()
// also feeds verifier.shouldBypassAuth on every OTHER verifier-consuming
// node, whose exact-match branch has no "/" guard), so "the edge's root
// mount is declared public" is only a TRUE statement when compiled as part
// of the edge binary. Asserting it from an untagged file broke
// `go test -tags identity ./app/` the moment EdgePaths() stopped being
// unconditional -- caught by running that exact command, not by inspection.
//
// Exercises the REAL, live PublicPaths() (via
// server.AssertUnauthenticatedSurfaceDeclared, not a stand-in) -- if
// component/server's EdgePaths() declaration is ever removed or its
// build-tag scoping broken, this is what catches it.
func TestEdgeRootMountSurvivesTheUnauthenticatedSurfaceAssertion(t *testing.T) {
	// The exact pattern mountEdgeEndpoints registers: no method verb, because
	// the /_memql/* reverse proxy (Task 7, #3712) needs every HTTP method
	// routed through this same mount, not just GET.
	a := &App{registeredRoutes: []string{"/"}}

	whole := a.unauthenticatedSurfaceRoutes(true)
	if err := server.AssertUnauthenticatedSurfaceDeclared(whole); err != nil {
		t.Fatalf("the edge's root mount must be declared public (component/server.EdgePaths) -- %v", err)
	}
}

// A malformed MEMQL_EDGE_SITE_CACHE_TTL_SECONDS must not be silently
// swallowed: it degrades to the default (a boot-time a.fatal would be
// disproportionate for a cache knob), but the operator who set a bad value
// gets a warning naming the var, or this is exactly the "debugged for an
// hour" class of misconfiguration.
func TestEdgeSiteCacheTTLFromEnvWarnsOnMalformedValue(t *testing.T) {
	t.Setenv("MEMQL_EDGE_SITE_CACHE_TTL_SECONDS", "not-a-number")
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	got := edgeSiteCacheTTLFromEnv(logger)

	if got != defaultSiteCacheTTL {
		t.Errorf("got %v, want the default %v", got, defaultSiteCacheTTL)
	}
	out := buf.String()
	if !strings.Contains(out, "MEMQL_EDGE_SITE_CACHE_TTL_SECONDS") {
		t.Errorf("warning should name the env var so an operator can find it: %q", out)
	}
}

// Same shape for a present-but-non-positive value ("0" parses fine as an
// int but is not a usable TTL) -- a distinct failure mode from a parse
// error, and both must warn.
func TestEdgeSiteCacheTTLFromEnvWarnsOnNonPositiveValue(t *testing.T) {
	t.Setenv("MEMQL_EDGE_SITE_CACHE_TTL_SECONDS", "0")
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	got := edgeSiteCacheTTLFromEnv(logger)

	if got != defaultSiteCacheTTL {
		t.Errorf("got %v, want the default %v", got, defaultSiteCacheTTL)
	}
	if !strings.Contains(buf.String(), "MEMQL_EDGE_SITE_CACHE_TTL_SECONDS") {
		t.Errorf("warning should name the env var so an operator can find it: %q", buf.String())
	}
}

// A valid value must be used quietly -- no warning for the ordinary case.
func TestEdgeSiteCacheTTLFromEnvUsesAValidValueQuietly(t *testing.T) {
	t.Setenv("MEMQL_EDGE_SITE_CACHE_TTL_SECONDS", "45")
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	got := edgeSiteCacheTTLFromEnv(logger)

	if got != 45*time.Second {
		t.Errorf("got %v, want 45s", got)
	}
	if buf.Len() != 0 {
		t.Errorf("a valid value must not warn: %q", buf.String())
	}
}

// Unset must fall back to the default quietly -- the ordinary "nothing
// configured" case, not a misconfiguration.
func TestEdgeSiteCacheTTLFromEnvDefaultsQuietlyWhenUnset(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	got := edgeSiteCacheTTLFromEnv(logger)

	if got != defaultSiteCacheTTL {
		t.Errorf("got %v, want the default %v", got, defaultSiteCacheTTL)
	}
	if buf.Len() != 0 {
		t.Errorf("an unset var must not warn: %q", buf.String())
	}
}
