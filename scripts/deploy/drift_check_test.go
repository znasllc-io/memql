// Tests for the deployment-v2 drift detector (scripts/deploy/drift-check.sh):
// which overlay it checks, where --live looks for pods, and image-ref
// normalization.
//
// A bare run checks deploy/k8s/overlays/cloud, and --live reads pods from every
// namespace in which the rendered overlay places an image. The defaults used to
// name the retired staging overlay and a table of tier namespaces, so the bare
// commands the runbooks give failed before comparing anything. The --live cases
// run the real script against a stub kubectl that records what it was asked.
//
// Image-ref normalization (#1441). The --live mode compares the
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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
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

// driftCheckStubKubectl answers the calls drift-check.sh makes, the way a
// cluster would. It renders only a directory holding a kustomization.yaml, so a
// missing overlay fails as kubectl's own render does, and it lists pods only in
// a namespace that has a file under DRIFT_CHECK_TEST_PODS. Every call is
// appended to DRIFT_CHECK_TEST_CALLS, so a case can assert which overlay was
// rendered and which namespaces were read. Any other call fails, so a change in
// what the script asks the cluster cannot be answered by this stub.
const driftCheckStubKubectl = `#!/bin/bash
function main() {
  printf '%s\n' "$*" >> "$DRIFT_CHECK_TEST_CALLS"
  case "$1" in
    cluster-info) ;;
    kustomize) [[ -f "$2/kustomization.yaml" ]] && cat "$DRIFT_CHECK_TEST_RENDERED" ;;
    get)
      [[ "$2" == pods && "$3" == -n ]] || exit 1
      if [[ -f "$DRIFT_CHECK_TEST_PODS/$4" ]]; then cat "$DRIFT_CHECK_TEST_PODS/$4"; fi ;;
    *) exit 1 ;;
  esac
}
main "$@"
`

// driftCheckCluster is what the stub kubectl serves: one overlay render, and
// for each namespace the imageID of every running container, one per line.
type driftCheckCluster struct {
	rendered string
	pods     map[string]string
}

// runDriftCheck runs the real drift-check.sh with args against a stub kubectl
// serving cluster. It returns the script's output and every kubectl call made.
func runDriftCheck(t *testing.T, cluster driftCheckCluster, args ...string) (string, []string, error) {
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
	if err := os.Mkdir(filepath.Join(dir, "pods"), 0o755); err != nil {
		t.Fatal(err)
	}
	for ns, imageIDs := range cluster.pods {
		write(filepath.Join("pods", ns), imageIDs, 0o644)
	}
	calls := filepath.Join(dir, "calls.txt")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DRIFT_CHECK_TEST_RENDERED", write("rendered.yaml", cluster.rendered, 0o644))
	t.Setenv("DRIFT_CHECK_TEST_PODS", filepath.Join(dir, "pods"))
	t.Setenv("DRIFT_CHECK_TEST_CALLS", calls)
	out, err := runAks(t, "drift-check.sh", args...)
	// The file is absent when the script never called kubectl.
	logged, _ := os.ReadFile(calls)
	return out, strings.FieldsFunc(string(logged), func(r rune) bool { return r == '\n' }), err
}

// runDriftCheckLive runs --live against the cloud overlay's render and the pods
// of namespace memql, which run imageIDs (one per line).
func runDriftCheckLive(t *testing.T, rendered, imageIDs string) (string, error) {
	t.Helper()
	out, _, err := runDriftCheck(t, driftCheckCluster{rendered: rendered, pods: map[string]string{"memql": imageIDs}},
		"--live", "--env=cloud", "--namespace=memql")
	return out, err
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

// driftCheckDeployment renders, in kustomize's output layout, a Deployment
// named name in namespace ns with one container running image.
func driftCheckDeployment(ns, name, image string) string {
	return fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %[2]s
  namespace: %[1]s
spec:
  template:
    spec:
      containers:
      - name: %[2]s
        image: %[3]s
`, ns, name, image)
}

// driftCheckDatabase renders a CloudNativePG Cluster in namespace ns. It names
// its image under imageName rather than image.
func driftCheckDatabase(ns, image string) string {
	return fmt.Sprintf(`apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: memql-db
  namespace: %s
spec:
  imageName: %s
  instances: 1
`, ns, image)
}

// driftCheckStepAccount names no image, in the namespace every overlay renders
// for pipeline steps.
const driftCheckStepAccount = `apiVersion: v1
kind: ServiceAccount
metadata:
  name: memql-pipelines-step
  namespace: memql-pipelines
`

// driftCheckRenderedOverlays returns the directory of every overlay render in
// calls.
func driftCheckRenderedOverlays(calls []string) []string {
	var dirs []string
	for _, c := range calls {
		if dir, ok := strings.CutPrefix(c, "kustomize "); ok {
			dirs = append(dirs, dir)
		}
	}
	return dirs
}

// driftCheckPodNamespaces returns, sorted, the namespace of every pod listing
// in calls.
func driftCheckPodNamespaces(calls []string) []string {
	var namespaces []string
	for _, c := range calls {
		if rest, ok := strings.CutPrefix(c, "get pods -n "); ok {
			namespaces = append(namespaces, strings.Fields(rest)[0])
		}
	}
	slices.Sort(namespaces)
	return namespaces
}

// TestDriftCheckDefaultsToTheCloudOverlay pins the overlay a bare run checks:
// deploy/k8s/overlays/cloud, the one deploy/argocd/apps/memql.yaml reconciles.
// The default was staging, an overlay retired with the staging/production split
// (epic memql#3943), so the bare commands deploy/argocd/README.md and
// docs/internal/ops/dr-runbook.md give failed before reading any overlay.
func TestDriftCheckDefaultsToTheCloudOverlay(t *testing.T) {
	image := "acrmemql.azurecr.io/memql-bff@sha256:" + strings.Repeat("a", 64)
	cluster := driftCheckCluster{
		rendered: driftCheckDeployment("memql", "bff", image),
		pods:     map[string]string{"memql": image + "\n"},
	}
	for _, mode := range []string{"--rendered", "--live"} {
		t.Run(mode, func(t *testing.T) {
			out, calls, err := runDriftCheck(t, cluster, mode)
			if err != nil {
				t.Fatalf("a bare %s exited non-zero: %v\n%s", mode, err, out)
			}
			overlays := driftCheckRenderedOverlays(calls)
			if len(overlays) == 0 {
				t.Fatalf("a bare %s rendered no overlay; kubectl calls: %q", mode, calls)
			}
			for _, dir := range overlays {
				if !strings.HasSuffix(dir, "/deploy/k8s/overlays/cloud") {
					t.Errorf("a bare %s rendered %s; want deploy/k8s/overlays/cloud", mode, dir)
				}
			}
		})
	}
}

// TestDriftCheckLiveReadsTheOverlaysNamespaces pins where --live looks for
// pods. Without --namespace, it reads every namespace in which the rendered
// overlay places a resource that names an image, and no other. The namespace
// used to come from a table keyed by --env, whose fallback sent --env=cloud to
// memql-cloud, a namespace nothing deploys to.
func TestDriftCheckLiveReadsTheOverlaysNamespaces(t *testing.T) {
	bff := "acrmemql.azurecr.io/memql-bff@sha256:" + strings.Repeat("b", 64)
	db := "acrmemql.azurecr.io/memql-db@sha256:" + strings.Repeat("c", 64)
	cases := []struct {
		name     string
		rendered string
		pods     map[string]string
		args     []string
		want     []string
	}{
		{
			name:     "the cloud overlay's namespace, not memql-cloud",
			rendered: driftCheckDeployment("memql", "bff", bff),
			pods:     map[string]string{"memql": bff + "\n"},
			args:     []string{"--live", "--env=cloud"},
			want:     []string{"memql"},
		},
		{
			name:     "whatever namespace the overlay names",
			rendered: driftCheckDeployment("elsewhere", "bff", bff),
			pods:     map[string]string{"elsewhere": bff + "\n"},
			args:     []string{"--live", "--env=cloud"},
			want:     []string{"elsewhere"},
		},
		{
			name: "every namespace holding an image, and none holding none",
			rendered: driftCheckDeployment("memql", "bff", bff) + "---\n" +
				driftCheckDatabase("memql-data", db) + "---\n" + driftCheckStepAccount,
			pods: map[string]string{"memql": bff + "\n", "memql-data": db + "\n"},
			args: []string{"--live", "--env=cloud"},
			want: []string{"memql", "memql-data"},
		},
		{
			name:     "--namespace overrides the overlay",
			rendered: driftCheckDeployment("memql", "bff", bff),
			pods:     map[string]string{"other": bff + "\n"},
			args:     []string{"--live", "--env=cloud", "--namespace=other"},
			want:     []string{"other"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, calls, err := runDriftCheck(t, driftCheckCluster{rendered: tc.rendered, pods: tc.pods}, tc.args...)
			if err != nil {
				t.Fatalf("%q exited non-zero: %v\n%s", tc.args, err, out)
			}
			if got := driftCheckPodNamespaces(calls); !slices.Equal(got, tc.want) {
				t.Errorf("%q read pods from %q; want %q", tc.args, got, tc.want)
			}
		})
	}
}

// TestDriftCheckNamesAMissingOverlay pins the failure for an --env with no
// overlay, such as the retired staging and prod. Both modes name the missing
// directory and list the overlays that exist. --live used to report it as
// "overlay rendered no digests", which named the symptom rather than the cause.
func TestDriftCheckNamesAMissingOverlay(t *testing.T) {
	want := regexp.MustCompile(`no overlay at .*/deploy/k8s/overlays/staging \(overlays: [^)]*\bcloud\b[^)]*\)`)
	for _, mode := range []string{"--rendered", "--live"} {
		t.Run(mode, func(t *testing.T) {
			out, _, err := runDriftCheck(t, driftCheckCluster{}, mode, "--env=staging")
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("%s --env=staging did not exit non-zero (err %v):\n%s", mode, err, out)
			}
			if !want.MatchString(out) {
				t.Errorf("%s --env=staging output has no line matching %q:\n%s", mode, want, out)
			}
		})
	}
}
