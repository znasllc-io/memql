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
// The same holds for a tag beside the digest. A CloudNativePG Cluster pins
// memql-db:<tag>@sha256:... because CNPG reads the PostgreSQL major from the
// tag, while the kubelet's imageID for that image is memql-db@sha256:... with
// no tag. TestDriftCheckLiveComparesTaggedPinsByDigest runs --live end to end
// against a stub kubectl to pin it.
//
// The aksScript/runAks helpers live in helpers_test.go; names here are
// normalize- or driftCheck-prefixed to avoid collisions in this package. Cases
// shell out via the hidden --normalize mode, or run --live against the stub
// kubectl, and skip when bash is unavailable.
package deploy

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// normalizeImageRef runs the hidden --normalize mode of drift-check.sh and
// returns the canonical ref. Reuses runAks from helpers_test.go.
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
		{
			name: "tag beside the digest is dropped (the CNPG memql-db pin)",
			in:   "acrmemql.azurecr.io/memql-db:16.15-timescaledb-2.29.1@sha256:e2629270",
			want: "acrmemql.azurecr.io/memql-db@sha256:e2629270",
		},
		{
			name: "tag beside the digest is dropped with the Docker Hub defaults",
			in:   "docker.io/library/redis:7.2@sha256:abc123",
			want: "redis@sha256:abc123",
		},
		{
			name: "registry port is kept",
			in:   "localhost:5000/x@sha256:abc123",
			want: "localhost:5000/x@sha256:abc123",
		},
		{
			name: "registry port is kept while the tag is dropped",
			in:   "localhost:5000/x:dev@sha256:abc123",
			want: "localhost:5000/x@sha256:abc123",
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
		// The overlay's tag-and-digest database pin and the kubelet's imageID.
		{"acrmemql.azurecr.io/memql-db:16.15-timescaledb-2.29.1@sha256:e2629270", "acrmemql.azurecr.io/memql-db@sha256:e2629270"},
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
		// Dropping the tag never drops the digest.
		{"acrmemql.azurecr.io/memql-db:16.15-timescaledb-2.29.1@sha256:aaaa", "acrmemql.azurecr.io/memql-db@sha256:bbbb"},
		// Two registries on one host, told apart only by the port.
		{"localhost:5000/x@sha256:abc", "localhost:5001/x@sha256:abc"},
	}
	for _, p := range distinct {
		a, b := normalizeImageRef(t, p[0]), normalizeImageRef(t, p[1])
		if a == b {
			t.Errorf("expected %q and %q to stay distinct, both normalized to %q", p[0], p[1], a)
		}
	}
}

// driftCheckStubKubectl answers the three calls check_live makes: the
// cluster-info probe, the overlay render and the pod imageID listing. Any
// other call fails, so a change in what --live asks the cluster cannot be
// answered by this stub.
const driftCheckStubKubectl = `#!/bin/bash
function main() {
  case "$1" in
    cluster-info) ;;
    kustomize) cat "$DRIFT_CHECK_TEST_RENDERED" ;;
    get) [[ "$2" == pods ]] && cat "$DRIFT_CHECK_TEST_IMAGE_IDS" ;;
    *) exit 1 ;;
  esac
}
main "$@"
`

// runDriftCheckLive runs the real drift-check.sh --live, with a stub kubectl
// serving the given render and one running imageID per line.
func runDriftCheckLive(t *testing.T, rendered, imageIDs string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	write := func(name, content string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("kubectl", driftCheckStubKubectl, 0o755)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DRIFT_CHECK_TEST_RENDERED", write("rendered.yaml", rendered, 0o644))
	t.Setenv("DRIFT_CHECK_TEST_IMAGE_IDS", write("image-ids.txt", imageIDs, 0o644))
	return runAks(t, "drift-check.sh", "--live", "--env=cloud", "--namespace=memql")
}

// TestDriftCheckLiveComparesTaggedPinsByDigest is the regression test for a
// database pinned the way CNPG requires: tag AND digest. The rendered
// imageName keeps the tag; the kubelet's imageID for the same image does not.
// Before the fix every --live run reported that pin as drift while its exact
// digest was running.
func TestDriftCheckLiveComparesTaggedPinsByDigest(t *testing.T) {
	// Full-length digests, because the script extracts them with its own grep.
	pinned := "sha256:" + strings.Repeat("d", 64)
	other := "sha256:" + strings.Repeat("e", 64)
	rendered := `apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: memql-db
spec:
  instances: 1
  imageName: acrmemql.azurecr.io/memql-db:16.15-timescaledb-2.29.1@` + pinned + `
  imagePullPolicy: IfNotPresent
`
	// A report line names the pinned image and digest. Whether it prints the
	// tag is presentation, so the pattern accepts the line with or without it.
	reportLine := func(before, after string) *regexp.Regexp {
		return regexp.MustCompile(regexp.QuoteMeta(before+"acrmemql.azurecr.io/memql-db") +
			`(:[^@\s]+)?` + regexp.QuoteMeta("@"+pinned+after))
	}

	t.Run("the pinned digest running reports converged", func(t *testing.T) {
		out, err := runDriftCheckLive(t, rendered, "acrmemql.azurecr.io/memql-db@"+pinned+"\n")
		if err != nil {
			t.Fatalf("--live exited non-zero with the pinned digest running: %v\n%s", err, out)
		}
		if want := reportLine("converged: ", ""); !want.MatchString(out) {
			t.Errorf("--live output has no line matching %q:\n%s", want, out)
		}
	})

	t.Run("a different digest running reports drift", func(t *testing.T) {
		out, err := runDriftCheckLive(t, rendered, "acrmemql.azurecr.io/memql-db@"+other+"\n")
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("--live did not exit non-zero with a different digest running (err %v):\n%s", err, out)
		}
		if want := reportLine("DRIFT: overlay pins ", " but no live pod runs it"); !want.MatchString(out) {
			t.Errorf("--live output has no line matching %q:\n%s", want, out)
		}
	})
}
