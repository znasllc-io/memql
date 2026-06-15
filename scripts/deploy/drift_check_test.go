// Tests for the deployment-v2 drift detector's image-ref normalization
// (scripts/deploy/drift-check.sh, #1441). The --live mode compares the
// digests the overlay pins against the digests a cluster is actually
// running (pod imageID). Docker reports an imageID with the registry fully
// qualified (docker.io/livekit/livekit-server@sha256:...) while the overlay
// typically pins the bare form (livekit/livekit-server@sha256:...) -- the
// SAME image, different surface text -- which used to spuriously turn the
// green gate red. drift-check.sh now canonicalizes both sides via
// normalize_imageref before set comparison; these tests pin that contract.
//
// Same package as aks_deploy_test.go (which already owns the script-syntax
// + rendered-gate cases and the aksScript/runAks helpers); names here are
// normalize-prefixed to avoid collisions. Cases shell out via the hidden
// --normalize mode and skip when bash is unavailable.
package deploy

import (
	"strings"
	"testing"
)

// normalizeImageRef runs the hidden --normalize mode of drift-check.sh and
// returns the canonical ref. Reuses runAks from aks_deploy_test.go.
func normalizeImageRef(t *testing.T, ref string) string {
	t.Helper()
	out, err := runAks(t, "drift-check.sh", "--normalize="+ref)
	if err != nil {
		t.Fatalf("--normalize=%q exited non-zero: %v\n%s", ref, err, out)
	}
	return strings.TrimRight(out, "\n")
}

// TestDriftCheckNormalizeImageRef is the regression test for #1441: a
// docker.io/-prefixed imageID (as Docker reports a running pod's image) and
// the bare overlay pin of the SAME image must normalize to one canonical
// form so the --live drift check does not false-positive. It also covers the
// index.docker.io / registry-1.docker.io hosts and the implicit library/
// official-image namespace, and asserts that a non-Docker-Hub registry (ACR,
// ghcr) keeps its host (different registries are legitimately different images).
func TestDriftCheckNormalizeImageRef(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "docker.io-prefixed livekit (the #1441 repro)",
			in:   "docker.io/livekit/livekit-server@sha256:2c6869d",
			want: "livekit/livekit-server@sha256:2c6869d",
		},
		{
			name: "bare livekit overlay pin",
			in:   "livekit/livekit-server@sha256:2c6869d",
			want: "livekit/livekit-server@sha256:2c6869d",
		},
		{
			name: "index.docker.io host with library namespace",
			in:   "index.docker.io/library/redis@sha256:abc123",
			want: "redis@sha256:abc123",
		},
		{
			name: "registry-1.docker.io host",
			in:   "registry-1.docker.io/livekit/livekit-server@sha256:2c6869d",
			want: "livekit/livekit-server@sha256:2c6869d",
		},
		{
			name: "bare library namespace",
			in:   "library/redis@sha256:abc123",
			want: "redis@sha256:abc123",
		},
		{
			name: "already-bare official image",
			in:   "redis@sha256:abc123",
			want: "redis@sha256:abc123",
		},
		{
			name: "ACR host is preserved (not Docker Hub)",
			in:   "acrmemql.azurecr.io/memql-bff@sha256:deadbeef",
			want: "acrmemql.azurecr.io/memql-bff@sha256:deadbeef",
		},
		{
			name: "ghcr.io host is preserved",
			in:   "ghcr.io/owner/img@sha256:cafe",
			want: "ghcr.io/owner/img@sha256:cafe",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeImageRef(t, tc.in)
			if got != tc.want {
				t.Errorf("normalize(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestDriftCheckNormalizeEquivalence is the property at the heart of #1441:
// the registry-qualified form Docker reports and the bare form the overlay
// pins must normalize to the SAME value (so set membership compares equal),
// while genuinely different images must NOT collide.
func TestDriftCheckNormalizeEquivalence(t *testing.T) {
	equal := [][2]string{
		{"docker.io/livekit/livekit-server@sha256:2c6869d", "livekit/livekit-server@sha256:2c6869d"},
		{"index.docker.io/library/redis@sha256:abc", "redis@sha256:abc"},
		{"registry-1.docker.io/livekit/livekit-server@sha256:2c6869d", "docker.io/livekit/livekit-server@sha256:2c6869d"},
	}
	for _, p := range equal {
		a, b := normalizeImageRef(t, p[0]), normalizeImageRef(t, p[1])
		if a != b {
			t.Errorf("expected %q and %q to normalize equal, got %q vs %q", p[0], p[1], a, b)
		}
	}

	distinct := [][2]string{
		// Different digests: never equal.
		{"docker.io/livekit/livekit-server@sha256:aaaa", "livekit/livekit-server@sha256:bbbb"},
		// Different registries holding the same path: legitimately different.
		{"acrmemql.azurecr.io/memql-bff@sha256:abc", "ghcr.io/memql-bff@sha256:abc"},
	}
	for _, p := range distinct {
		a, b := normalizeImageRef(t, p[0]), normalizeImageRef(t, p[1])
		if a == b {
			t.Errorf("expected %q and %q to stay distinct, both normalized to %q", p[0], p[1], a)
		}
	}
}
