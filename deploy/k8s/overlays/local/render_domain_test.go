// Package local holds tests that render this overlay and assert about the
// result.
//
// WHY A TEST AND NOT A REVIEW. The overlay delivers the domain through a
// name-regex patch target covering nine node Deployments (memql#3593). A regex
// that silently stops matching one of them is invisible: the cluster boots, the
// mesh forms, and that node rejects every token with "invalid issuer" while its
// manifest looks correct. Rendering and asserting is the difference between
// believing the regex covers a tenth node type and knowing it.
package local

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The ten node Deployments the local mesh runs. Listed rather than discovered,
// because the failure this exists to catch is precisely a node type arriving
// and the patch not covering it -- discovery would grow the list and the check
// with it, and assert nothing.
var nodes = []string{
	"identity", "bff", "cognition", "agent", "planner",
	"workbench", "mcp", "voice", "voice-agent", "edge",
}

// render builds this overlay with whichever renderer the machine has.
func render(t *testing.T) string {
	t.Helper()
	for _, cmd := range [][]string{
		{"kustomize", "build", "."},
		{"kubectl", "kustomize", "."},
	} {
		if _, err := exec.LookPath(cmd[0]); err != nil {
			continue
		}
		out, err := exec.Command(cmd[0], cmd[1:]...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s failed: %v\n%s", strings.Join(cmd, " "), err, out)
		}
		return string(out)
	}
	t.Skip("neither kustomize nor kubectl is installed; cannot render the overlay")
	return ""
}

// After memql#3593 the overlay states the RELATIONSHIP to the domain and never
// the value -- except the two Ingress hosts, which are Kubernetes API objects
// and carry the committed default.
func TestRenderedOverlayCarriesNoVendorDomain(t *testing.T) {
	rendered := render(t)

	if strings.Contains(rendered, "znas.io") {
		t.Error("the rendered overlay still names znas.io")
	}
	if strings.Contains(rendered, "local-znas-tls") {
		t.Error("the rendered overlay still names the old TLS secret")
	}
}

// Every node reads the ConfigMap, and no node overrides it with a pinned env
// entry. An explicit env entry beats envFrom in Kubernetes regardless of order,
// so a single survivor means that node verifies against the base manifests'
// staging placeholder issuer and rejects every mesh token.
func TestEveryNodeReadsTheDomainConfigMap(t *testing.T) {
	rendered := render(t)
	docs := strings.Split(rendered, "\n---\n")

	for _, node := range nodes {
		var found bool
		for _, doc := range docs {
			if !strings.Contains(doc, "kind: Deployment") ||
				!strings.Contains(doc, "\n  name: "+node+"\n") {
				continue
			}
			found = true
			if !strings.Contains(doc, "name: memql-domain") {
				t.Errorf("%s does not mount the memql-domain ConfigMap", node)
			}
			if strings.Contains(doc, "MEMQL_IDENTITY_VERIFIER_EXPECTED_ISSUER") {
				t.Errorf("%s still pins an expected issuer, which beats envFrom", node)
			}
		}
		if !found {
			t.Errorf("no Deployment named %s in the rendered overlay", node)
		}
	}
}

// No node may carry a PLACEHOLDER domain (memql#3600).
//
// This is the check that was missing, and its absence shipped a broken install.
// The earlier version asserted only that `MEMQL_IDENTITY_VERIFIER_EXPECTED_ISSUER`
// was gone and that no `znas.io` survived -- and the base manifests set FIVE more
// domain-bearing values on identity, all to the placeholder domain, which is
// neither of those things.
//
// An explicit env entry beats envFrom, so each survivor silently defeated the
// derivation for its own variable. Identity ran with
// MEMQL_IDENTITY_BASE_URL=https://identity.staging.example.com (the placeholder
// as it was spelled then), the installer
// recovered a magic link at that host, nobody could click it, no owner account
// was created, and the cluster could not be signed into at all -- while every
// install step reported success.
//
// The assertion is therefore about the RENDERED VALUES, not about a list of
// variable names: a name-keyed check would have to be extended by whoever adds
// the seventh, which is exactly the discipline that failed here.
//
// placeholderDomain must stay in step with what deploy/k8s/base actually sets.
// It was `staging.example.com` until epic memql#3943 dropped the environment
// label from every example host; a check still looking for the old spelling
// would find nothing and pass having verified nothing, which is this gate's own
// failure mode. TestThePlaceholderDomainIsStillInTheBase below is what stops
// that -- it fails if the base stops using this string at all.
// placeholderDomain is the domain deploy/k8s/base sets its example hosts to.
const placeholderDomain = "example.com"

// TestThePlaceholderDomainIsStillInTheBase keeps the gate above honest.
//
// That gate asserts an ABSENCE, so it passes trivially the moment the string it
// searches for stops being the one the base uses -- which is precisely what
// happened when epic memql#3943 renamed `staging.example.com` to `example.com`.
// This asserts the string is still there to be found.
func TestThePlaceholderDomainIsStillInTheBase(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "base", "identity.yaml"))
	if err != nil {
		t.Fatalf("reading base/identity.yaml: %v", err)
	}
	if !strings.Contains(string(raw), placeholderDomain) {
		t.Fatalf("deploy/k8s/base/identity.yaml no longer contains %q, so "+
			"TestNoNodeCarriesAPlaceholderDomain searches for a string nothing "+
			"emits and passes having checked nothing", placeholderDomain)
	}
}

func TestNoNodeCarriesAPlaceholderDomain(t *testing.T) {
	rendered := render(t)

	for i, line := range strings.Split(rendered, "\n") {
		if !strings.Contains(line, placeholderDomain) {
			continue
		}
		t.Errorf("line %d carries a placeholder domain, which beats the derivation "+
			"for that variable and makes the node serve a host nothing resolves:\n  %s",
			i+1, strings.TrimSpace(line))
	}
}

// The committed default is what an un-overridden install serves, and it is the
// value scripts/k3d/up.sh compares against to decide whether to emit any
// Ingress patches at all.
func TestFrontDoorHostsUseTheCommittedDefault(t *testing.T) {
	rendered := render(t)

	for _, host := range []string{"api.memql.localhost", "identity.memql.localhost"} {
		if !strings.Contains(rendered, "host: "+host) {
			t.Errorf("the rendered overlay does not serve %s", host)
		}
	}
}
