package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/znasllc-io/memql/component/language/parser"
)

// TestResolveToken pins the #2179 token-resolution precedence: --jwt flag >
// --jwt-file / $MEMQL_SVC_JWT_FILE (only if the file exists AND is non-empty) >
// $MEMQL_SVC_JWT. A missing or empty file is NOT an error -- it falls through to
// the next source, so a best-effort self-mint init container that fails to write
// the file leaves the gate on the static-secret fallback (strictly safer than
// today). All sources are trimmed (the mint writes a trailing newline).
func TestResolveToken(t *testing.T) {
	dir := t.TempDir()

	write := func(name, contents string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(contents), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return p
	}

	fileWithToken := write("svc-jwt", "eyJfile\n")  // trailing newline trimmed
	emptyFile := write("empty", "   \n")            // whitespace-only -> falls through
	missing := filepath.Join(dir, "does-not-exist") // missing -> falls through

	cases := []struct {
		name                                  string
		flagJWT, flagJWTFile, envFile, envJWT string
		want                                  string
	}{
		{"flag-wins-over-everything", "eyJflag", fileWithToken, fileWithToken, "eyJenv", "eyJflag"},
		{"flag-is-trimmed", "  eyJflag  ", "", "", "eyJenv", "eyJflag"},
		{"file-flag-over-env", "", fileWithToken, "", "eyJenv", "eyJfile"},
		{"file-env-over-secret-env", "", "", fileWithToken, "eyJenv", "eyJfile"},
		{"flag-jwt-file-beats-env-jwt-file", "", fileWithToken, missing, "eyJenv", "eyJfile"},
		{"missing-file-falls-through-to-env", "", missing, "", "eyJenv", "eyJenv"},
		{"empty-file-falls-through-to-env", "", emptyFile, "", "eyJenv", "eyJenv"},
		{"no-file-configured-uses-env", "", "", "", "eyJenv", "eyJenv"},
		{"env-jwt-is-trimmed", "", "", "", "  eyJenv\n", "eyJenv"},
		{"nothing-resolves-empty", "", missing, "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveToken(tc.flagJWT, tc.flagJWTFile, tc.envFile, tc.envJWT); got != tc.want {
				t.Fatalf("resolveToken(%q,%q,%q,%q) = %q, want %q",
					tc.flagJWT, tc.flagJWTFile, tc.envFile, tc.envJWT, got, tc.want)
			}
		})
	}
}

// TestDefaultGateQueryParses is the regression guard for znasllc-io/memql#1130:
// the deploy gate's default query is sent to the engine on every gate run, so it
// MUST be a well-formed MemQL query. The prior default ("count v1:cognition:space")
// failed the parser with `unexpected token after expression: "v1:cognition:space"`,
// which the engine logged as a `memql query execution failed` ERROR on the node on
// every gate run (component=memQL, bff node) -- the staging log noise the issue
// flagged. This test fails (with that exact error) on the old default and passes on
// the well-formed queryActivePartitionIds replacement.
func TestDefaultGateQueryParses(t *testing.T) {
	if _, err := parser.ParseExpression(defaultGateQuery); err != nil {
		t.Fatalf("default gate query %q must parse cleanly (znasllc-io/memql#1130); got: %v", defaultGateQuery, err)
	}

	// Pin the exact pre-fix failure shape so this regression stays meaningful:
	// the retired `count <bareConcept>` form is what produced the staging error.
	const oldDefault = "count v1:cognition:space"
	_, err := parser.ParseExpression(oldDefault)
	if err == nil {
		t.Fatalf("expected the retired default %q to fail parsing (it is the #1130 repro); it parsed cleanly", oldDefault)
	}
	if !strings.Contains(err.Error(), `unexpected token after expression: "v1:cognition:space"`) {
		t.Fatalf("retired default %q failed with an unexpected error; want the #1130 shape, got: %v", oldDefault, err)
	}
}

// TestReadyzURLFor pins how the readiness URL is derived from the gRPC addr (or
// an explicit override): http /readyz on :8085 of the addr's host.
func TestReadyzURLFor(t *testing.T) {
	cases := []struct {
		addr, override, want string
	}{
		{"bff:50051", "", "http://bff:8085/readyz"},
		{"cognition-canary:50051", "", "http://cognition-canary:8085/readyz"},
		{"10.0.0.5:50051", "", "http://10.0.0.5:8085/readyz"},
		{"bff:50051", "http://custom/readyz", "http://custom/readyz"}, // override wins
		{"bff", "", "http://bff:8085/readyz"},                         // no port -> host == addr
	}
	for _, c := range cases {
		if got := readyzURLFor(c.addr, c.override); got != c.want {
			t.Errorf("readyzURLFor(%q,%q) = %q, want %q", c.addr, c.override, got, c.want)
		}
	}
}

// TestClassifyGRPCTransientMarking pins the #1782 retry contract: a JWKS/startup
// warm-up race (Unauthenticated) and an unready backend (Unavailable) are marked
// errTransientGate so checkAuthQueryWithRetry retries them, while a real
// authorization deny (PermissionDenied) is NOT retryable and fails the gate now.
func TestClassifyGRPCTransientMarking(t *testing.T) {
	cases := []struct {
		name      string
		code      codes.Code
		transient bool
	}{
		{"unauthenticated-warmup", codes.Unauthenticated, true},
		{"unavailable-unready", codes.Unavailable, true},
		{"permission-denied-real", codes.PermissionDenied, false},
		{"internal-other", codes.Internal, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyGRPC("recv", status.Error(tc.code, "boom"))
			if got := errors.Is(err, errTransientGate); got != tc.transient {
				t.Fatalf("classifyGRPC(%s): transient=%v, want %v (err=%v)", tc.code, got, tc.transient, err)
			}
		})
	}
}
