package k3d

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func upDomainScript(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("up.sh")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// The param exists and is documented, which is what the install graph and the
// runbook both read.
func TestUpDeclaresDomainParam(t *testing.T) {
	out, err := exec.Command("bash", upDomainScript(t), "--print-spec").CombinedOutput()
	if err != nil {
		t.Fatalf("--print-spec failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "domain") {
		t.Errorf("--print-spec does not declare --domain\n%s", out)
	}
}

// upEmit sources up.sh far enough to call one of its emitters without running
// main. The script guards nothing, so it is read and truncated at `function
// main`, which is the only shape that lets these pure functions be tested at
// all without a cluster.
func upEmit(t *testing.T, domain, registry, tag, call string) string {
	t.Helper()
	body, err := os.ReadFile(upDomainScript(t))
	if err != nil {
		t.Fatal(err)
	}
	src := string(body)
	cut := strings.Index(src, "\nfunction main()")
	if cut < 0 {
		t.Fatal("up.sh has no `function main()` -- this harness cannot truncate it")
	}
	// REPO_ROOT and OVERLAY_PATH point at the REAL overlay: the image emitter
	// reads the node image names out of its kustomization.yaml rather than
	// carrying a list of its own, so a harness that pointed elsewhere would be
	// testing nothing.
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	harness := src[:cut] +
		"\nDOMAIN='" + domain + "'" +
		"\nIMAGE_REGISTRY='" + registry + "'" +
		"\nIMAGE_TAG='" + tag + "'" +
		"\nREPO_ROOT='" + repoRoot + "'" +
		"\nOVERLAY_PATH='deploy/k8s/overlays/local'\n" + call + "\n"

	// up.sh derives SCRIPT_DIR from its own location and sources ../lib from
	// there, so the harness needs the same directory SHAPE: a sibling of a
	// `lib` directory. Symlinked rather than copied so the harness always runs
	// against the real capability runtime.
	root := t.TempDir()
	dir := filepath.Join(root, "k3d")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	realLib, err := filepath.Abs(filepath.Join("..", "lib"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realLib, filepath.Join(root, "lib")); err != nil {
		t.Fatal(err)
	}

	script := filepath.Join(dir, "harness.sh")
	if err := os.WriteFile(script, []byte(harness), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("bash", script).CombinedOutput()
	if err != nil {
		t.Fatalf("harness failed: %v\n%s", err, out)
	}
	return string(out)
}

// The overlay commits its own Ingress hostname, so an install that accepts the
// default needs no patch at all. Emitting one always would mean every install
// carries generated YAML restating what the manifests already say.
func TestKustomizeHostOverridesSilentOnTheDefault(t *testing.T) {
	got := upEmit(t, "memql.localhost", "", "", "kustomize_source_block")
	if strings.TrimSpace(got) != "" {
		t.Errorf("default domain emitted a kustomize block:\n%s", got)
	}
}

// A custom domain repoints both Ingresses -- rule host AND tls host, because a
// certificate that does not cover the name is the same TLS failure by another
// route.
func TestKustomizeHostOverridesRepointBothIngresses(t *testing.T) {
	got := upEmit(t, "lab.example.com", "", "", "kustomize_source_block")

	for _, want := range []string{
		"kustomize:",
		"patches:",
		"name: api-front-door",
		"name: identity-front-door",
		"value: api.lab.example.com",
		"value: identity.lab.example.com",
		"/spec/rules/0/host",
		"/spec/tls/0/hosts/0",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("emitted block missing %q:\n%s", want, got)
		}
	}
	if n := strings.Count(got, "value: api.lab.example.com"); n != 2 {
		t.Errorf("api host replaced %d times, want 2 (rule + tls)", n)
	}
}

// ONE `kustomize:` key. Both emitters produce sub-blocks of the same mapping,
// and a second `kustomize:` line would be a duplicate mapping key -- ArgoCD
// would silently keep one of them, dropping either the image overrides or the
// hostnames with no error anywhere.
func TestKustomizeSourceBlockHasASingleKustomizeKey(t *testing.T) {
	got := upEmit(t, "lab.example.com", "ghcr.io/example", "1.2.3", "kustomize_source_block")

	if n := strings.Count(got, "kustomize:"); n != 1 {
		t.Errorf("kustomize: appears %d times, want exactly 1:\n%s", n, got)
	}
	if !strings.Contains(got, "images:") {
		t.Errorf("image overrides missing when both are requested:\n%s", got)
	}
	if !strings.Contains(got, "patches:") {
		t.Errorf("host patches missing when both are requested:\n%s", got)
	}

	// `patches:` must start its own line. It did not: command substitution
	// strips trailing newlines, so the last image override and the patches key
	// ran together on one line and the Application's YAML lost every host
	// patch -- valid YAML, silently missing half of what was asked for.
	for _, line := range strings.Split(got, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasSuffix(trimmed, "patches:") && trimmed != "patches:" {
			t.Errorf("`patches:` is not on its own line: %q", line)
		}
	}
}

// Images alone still work -- this is the memql#3572 path, unchanged.
func TestKustomizeSourceBlockImagesOnly(t *testing.T) {
	got := upEmit(t, "memql.localhost", "ghcr.io/example", "1.2.3", "kustomize_source_block")

	if !strings.Contains(got, "images:") {
		t.Errorf("image overrides missing:\n%s", got)
	}
	if strings.Contains(got, "patches:") {
		t.Errorf("host patches emitted for the default domain:\n%s", got)
	}
}
