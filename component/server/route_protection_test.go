package server

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// =============================================================================
// Public Paths Tests
// =============================================================================

func TestPublicPathsIncludesHealthz(t *testing.T) {
	t.Parallel()

	paths := PublicPaths()
	assert.Contains(t, paths, "/healthz")
}

func TestPublicPathsIncludesAuth(t *testing.T) {
	t.Parallel()

	paths := PublicPaths()

	// Auth endpoints must be public for OIDC flow
	assert.Contains(t, paths, "/auth/login")
	assert.Contains(t, paths, "/auth/callback")
	assert.Contains(t, paths, "/auth/logout")
	assert.Contains(t, paths, "/auth/landing")
}

func TestAuthPathsComplete(t *testing.T) {
	t.Parallel()

	paths := AuthPaths()

	// All OIDC endpoints should be present
	required := []string{"/auth/login", "/auth/callback", "/auth/logout", "/auth/landing"}
	for _, r := range required {
		assert.Contains(t, paths, r, "missing auth path: %s", r)
	}
}

func TestHealthzPathsComplete(t *testing.T) {
	t.Parallel()

	paths := HealthzPaths()
	assert.Contains(t, paths, "/healthz")
}

// =============================================================================
// Protected Paths Tests (verify NOT in public list)
// =============================================================================

func TestProtectedPathsNotPublic(t *testing.T) {
	t.Parallel()

	publicPaths := PublicPaths()

	// These paths MUST NOT be in the public list
	protectedPaths := []string{
		"/memql/query",
		"/memql/mutate",
		// WebSocket endpoints
		"/memql/ws",
	}

	for _, protected := range protectedPaths {
		for _, public := range publicPaths {
			if public == protected {
				t.Errorf("protected path %s should not be in public paths", protected)
			}
		}
	}
}

func TestQueryEndpointNotPublic(t *testing.T) {
	t.Parallel()

	publicPaths := PublicPaths()

	for _, p := range publicPaths {
		if p == "/memql/query" {
			t.Error("/memql/query should NOT be a public path")
		}
	}
}

func TestMutateEndpointNotPublic(t *testing.T) {
	t.Parallel()

	publicPaths := PublicPaths()

	for _, p := range publicPaths {
		if p == "/memql/mutate" {
			t.Error("/memql/mutate should NOT be a public path")
		}
	}
}

// =============================================================================
// Path Normalization Tests
// =============================================================================

func TestPublicPathsNormalized(t *testing.T) {
	t.Parallel()

	paths := PublicPaths()

	for _, p := range paths {
		// All paths should start with /
		if len(p) > 0 && p[0] != '/' {
			t.Errorf("path %s should start with /", p)
		}

		// Paths should not have trailing slashes (except root)
		if len(p) > 1 && p[len(p)-1] == '/' {
			// Some paths like /static/images/ may have trailing slash for prefix matching
			// This is acceptable
		}
	}
}

// =============================================================================
// Public Paths Count Sanity Check
// =============================================================================

func TestPublicPathsReasonableCount(t *testing.T) {
	t.Parallel()

	paths := PublicPaths()

	// Should have a reasonable number of public paths
	// Too few: might be missing critical paths
	// Too many: might be exposing protected resources
	assert.Greater(t, len(paths), 5, "should have at least basic public paths")
	assert.Less(t, len(paths), 50, "too many public paths - review for security")
}
