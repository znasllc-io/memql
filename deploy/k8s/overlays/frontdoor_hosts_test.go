// Package overlays -- the front-door half of the environment gates (memql#3767).
//
// WHY A TEST AND NOT A REVIEW. The host set is a PRODUCT -- three roles plus
// the sites wildcard, times however many environments the cluster carries --
// and the failure a product has that a list does not is asymmetry. A host
// missing from ONE environment is an environment nothing can reach, while its
// manifest looks complete because the other environment's copy of the same rule
// is right there beside it in the diff. Nothing fails to build, nothing fails
// to reconcile, and the symptom arrives whenever somebody first dials that host.
//
// The certificate half is worse, because it fails at a distance. A SAN missing
// from one environment does not stop the Ingress from existing; the request
// reaches TLS termination, the controller serves whatever default certificate
// it has, and the browser reports a name mismatch at a host nobody thinks is
// new. So the SANs are checked against the hosts, by the one-label wildcard
// rule rather than by string containment.
package overlays

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/frontdoor"
)

// committedDomain is the default cmd/frontdoorhosts writes, and the same one
// the local overlay commits. No file under deploy/ names a REAL domain
// (memql#3593) -- an install overrides these hosts through the ArgoCD
// Application's kustomize.patches, and `.localhost` is unroutable so an
// environment reconciled before its domain is set fails visibly.
const committedDomain = "memql.localhost"

// hostsIn returns every Ingress rule host in a rendered stream. Parsed by line
// rather than with a decoder because the stream is many documents of many
// kinds and `host:` under spec.rules is unambiguous at the text level.
func hostsIn(rendered string) map[string]bool {
	out := map[string]bool{}
	for _, line := range strings.Split(rendered, "\n") {
		t := strings.TrimSpace(line)
		t = strings.TrimPrefix(t, "- ")
		after, ok := strings.CutPrefix(t, "host: ")
		if !ok {
			continue
		}
		if h := strings.Trim(strings.TrimSpace(after), `"'`); h != "" {
			out[h] = true
		}
	}
	return out
}

// TestEveryGeneratedHostRuleIsServedInBothOverlays is the acceptance criterion
// in test form: "the render gate asserts every expected rule is present in both
// overlays".
//
// The expectation is COMPUTED from component/frontdoor rather than listed, and
// that is the point -- a listed expectation would have to be edited by whoever
// adds an environment, which is the hand-maintenance this whole change removes.
// What stops the computation from being circular is the table in
// environments_test.go: the LABEL each environment carries is stated there
// independently, so a wrong label in an overlay's environment.yaml fails rather
// than being adopted as the expectation.
func TestEveryGeneratedHostRuleIsServedInBothOverlays(t *testing.T) {
	for _, env := range environments {
		t.Run(env.dir, func(t *testing.T) {
			served := hostsIn(render(t, env.dir))

			for _, h := range frontdoor.Hosts(env.hostLabel, committedDomain) {
				if !served[h.Name] {
					t.Errorf("the %s role's host %q is not served: this environment is unreachable at it, "+
						"and the manifest looks complete because the other environment's copy of the rule is beside it",
						h.Role, h.Name)
				}
			}
		})
	}
}

// TestNoEnvironmentServesAnotherEnvironmentsHost is the other direction, and it
// is the one that fails LOUDLY in production rather than quietly.
//
// Two Ingress rules for one hostname in two namespaces do not conflict at apply
// time. The controller resolves it by whichever it saw first, so traffic for a
// host lands in whichever environment happened to reconcile earlier -- which is
// a coin flip between staging and production, re-flipped on every controller
// restart.
func TestNoEnvironmentServesAnotherEnvironmentsHost(t *testing.T) {
	for i, env := range environments {
		other := environments[(i+1)%len(environments)]
		t.Run(env.dir, func(t *testing.T) {
			served := hostsIn(render(t, env.dir))
			mine := map[string]bool{}
			for _, h := range frontdoor.Hosts(env.hostLabel, committedDomain) {
				mine[h.Name] = true
			}
			for _, h := range frontdoor.Hosts(other.hostLabel, committedDomain) {
				if mine[h.Name] {
					continue // the product gives them no shared host; belt and braces
				}
				if served[h.Name] {
					t.Errorf("%s serves %q, which belongs to %s -- two rules for one host in two "+
						"namespaces resolve by whichever the ingress controller saw first",
						env.dir, h.Name, other.dir)
				}
			}
		})
	}
}

// TestOnlyOneEnvironmentClaimsTheApex. The bare domain has no label in front of
// it, so there is nothing for a second environment to distinguish itself with.
// frontdoor.Apex returns "" for a labelled environment; this asserts the render
// agrees.
func TestOnlyOneEnvironmentClaimsTheApex(t *testing.T) {
	var claimants []string
	for _, env := range environments {
		if hostsIn(render(t, env.dir))[committedDomain] {
			claimants = append(claimants, env.dir)
		}
	}
	if len(claimants) != 1 {
		t.Errorf("the apex %q is claimed by %v, want exactly one environment", committedDomain, claimants)
	}
}

// TestBothWildcardCertificatesAreRequested is the second half of the acceptance
// criterion, and the reason the role hosts hyphenate.
//
// `*.<domain>` covers every environment's api / identity / mcp host because
// they are single-label by construction. A labelled environment's SITES cannot
// be single-label -- the site's own name occupies that label -- so it needs one
// more wildcard, and exactly one: `*.<label>.<domain>`, for all of its sites.
func TestBothWildcardCertificatesAreRequested(t *testing.T) {
	requested := map[string][]string{} // SAN -> environments requesting it

	for _, env := range environments {
		sans := certificateSANs(t, render(t, env.dir))
		if len(sans) == 0 {
			t.Errorf("%s requests no front-door certificate at all, so every host in it terminates "+
				"TLS with whatever default the controller has", env.dir)
			continue
		}
		want := frontdoor.CertificateSANs(env.hostLabel, committedDomain)
		if strings.Join(sans, ",") != strings.Join(want, ",") {
			t.Errorf("%s requests SANs %v, want %v", env.dir, sans, want)
		}
		for _, s := range sans {
			requested[s] = append(requested[s], env.dir)
		}
	}

	for _, wildcard := range []string{"*." + committedDomain, "*.staging." + committedDomain} {
		if len(requested[wildcard]) == 0 {
			t.Errorf("no environment requests the %q certificate", wildcard)
		}
	}
}

// TestEveryServedHostIsCoveredByARequestedSAN is what makes the two gates above
// mean something together: it is possible to serve every expected host and
// request every expected SAN and still have one host that no SAN covers, if the
// wildcard rule is misread. The rule is ONE label, and it is applied here rather
// than assumed.
func TestEveryServedHostIsCoveredByARequestedSAN(t *testing.T) {
	for _, env := range environments {
		t.Run(env.dir, func(t *testing.T) {
			rendered := render(t, env.dir)
			sans := certificateSANs(t, rendered)

			for _, h := range frontdoor.Hosts(env.hostLabel, committedDomain) {
				if !wildcardCovers(h.Name, sans) {
					t.Errorf("%q is served but covered by none of the requested SANs %v -- TLS "+
						"termination succeeds with the controller's default certificate, so this "+
						"presents as a browser name mismatch and names nothing", h.Name, sans)
				}
			}
		})
	}
}

// TestAThirdEnvironmentIsAValueChange is the third acceptance criterion, and it
// is proved by rendering one rather than by asserting about the two that exist.
//
// The whole test runs against a throwaway overlay OUTSIDE this repository. If
// adding an environment needed a code change, this could not pass without one --
// so a green run is the claim: a new environment is a directory with a label in
// it, and its entire front door follows.
func TestAThirdEnvironmentIsAValueChange(t *testing.T) {
	const label = "e2"

	root := t.TempDir()
	dir := filepath.Join(root, "hypothetical")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	// The litmus, captured BEFORE the generator runs so it measures this test's
	// effect rather than whatever the working tree happened to look like.
	before := worktreeState(t)

	// The ONLY input: the same three values the two committed overlays carry.
	write(t, filepath.Join(dir, "environment.yaml"), `apiVersion: v1
kind: ConfigMap
metadata:
  name: memql-environment
  namespace: memql
data:
  MEMQL_ENVIRONMENT: development
  MEMQL_DB_SEARCH_PATH: "memql_e2, public"
  MEMQL_HOST_LABEL: "`+label+`"
`)

	// Deliberately self-contained -- it does not reference ../../base. What is
	// under test is that the generator produces this environment's whole front
	// door from its label, not that a temp directory can reach the base.
	write(t, filepath.Join(dir, "kustomization.yaml"), `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: memql-hypothetical
resources:
  - environment.yaml
  - front-door.generated.yaml
`)

	out, err := exec.Command("go", "run", "../../../cmd/frontdoorhosts", "--overlays="+root).CombinedOutput()
	if err != nil {
		t.Fatalf("the generator refused a new environment, so adding one is NOT a value change: %v\n%s", err, out)
	}

	rendered := render(t, dir)
	served := hostsIn(rendered)

	for _, h := range frontdoor.Hosts(label, committedDomain) {
		if !served[h.Name] {
			t.Errorf("a third environment does not serve %q (%s)", h.Name, h.Role)
		}
	}
	// The properties the design is emphatic about, restated for a label nobody
	// wrote code for: roles stay single-label, sites nest, no apex.
	for _, want := range []string{
		"api-e2." + committedDomain,
		"identity-e2." + committedDomain,
		"mcp-e2." + committedDomain,
		"*.e2." + committedDomain,
	} {
		if !served[want] {
			t.Errorf("a third environment does not serve %q", want)
		}
	}
	if served[committedDomain] {
		t.Error("a third environment claims the apex, which belongs to the unprefixed environment alone")
	}

	sans := certificateSANs(t, rendered)
	if want := []string{"*." + committedDomain, "*.e2." + committedDomain}; strings.Join(sans, ",") != strings.Join(want, ",") {
		t.Errorf("a third environment requests SANs %v, want %v -- one additional wildcard, for its sites", sans, want)
	}

	// And the litmus: nothing in the repository moved to make that work. The
	// whole environment was generated and rendered from three lines of YAML in
	// a throwaway directory -- no manifest edited, no generator taught a new
	// name, no test list extended.
	if after := worktreeState(t); after != before {
		t.Errorf("generating a third environment changed the working tree, so it is not a value change.\nbefore:\n%s\nafter:\n%s",
			before, after)
	}
}

// worktreeState is `git status --porcelain` over the whole repository. Compared
// before and against after rather than asserted to be empty: this suite runs on
// a branch mid-change, and an empty-tree assertion would fail for reasons that
// have nothing to do with what it is measuring.
func worktreeState(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "status", "--porcelain").Output()
	if err != nil {
		t.Skipf("git is unavailable, so the no-code-change litmus cannot run: %v", err)
	}
	return string(out)
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// certificateSANs reads the front-door Certificate's dnsNames out of a rendered
// stream, in order.
//
// Text-scanned for the same reason hostsIn is: the stream carries many kinds,
// and here the block is unambiguous -- `dnsNames:` followed by a list. A decoder
// would need a cert-manager schema this package has no reason to depend on.
func certificateSANs(t *testing.T, rendered string) []string {
	t.Helper()

	var out []string
	lines := strings.Split(rendered, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) != "dnsNames:" {
			continue
		}
		for _, next := range lines[i+1:] {
			trimmed := strings.TrimSpace(next)
			after, ok := strings.CutPrefix(trimmed, "- ")
			if !ok {
				break
			}
			out = append(out, strings.Trim(strings.TrimSpace(after), `"'`))
		}
	}
	return out
}

// wildcardCovers applies the ONE-LABEL rule that the whole hyphenation choice
// rests on. A checker that accepted `*.example.test` for
// `api.staging.example.test` would pass the exact configuration the design
// refuses, so it is spelled out rather than approximated with strings.HasSuffix.
func wildcardCovers(host string, sans []string) bool {
	for _, san := range sans {
		if san == host {
			return true
		}
		suffix, ok := strings.CutPrefix(san, "*")
		if !ok || !strings.HasSuffix(host, suffix) {
			continue
		}
		if head := strings.TrimSuffix(host, suffix); head != "" && !strings.Contains(head, ".") {
			return true
		}
	}
	return false
}

func TestWildcardCoversRefusesAMultiLabelMatch(t *testing.T) {
	// Self-test: without this, every SAN assertion above could be vacuous.
	if wildcardCovers("api.staging.example.test", []string{"*.example.test"}) {
		t.Error("wildcardCovers accepted a two-label host under a one-label wildcard")
	}
	if !wildcardCovers("api-staging.example.test", []string{"*.example.test"}) {
		t.Error("wildcardCovers rejected a single-label host under its wildcard")
	}
}

// TestTheGeneratedFrontDoorIsReconciled closes the silent-failure hole one
// level up from the manifests: a generated file no kustomization lists is a
// file, not a front door. Nothing fails -- the overlay renders, the mesh comes
// up, and the environment is simply not reachable from outside.
func TestTheGeneratedFrontDoorIsReconciled(t *testing.T) {
	for _, env := range environments {
		raw, err := os.ReadFile(filepath.Join(env.dir, "kustomization.yaml"))
		if err != nil {
			t.Fatalf("reading %s's kustomization: %v", env.dir, err)
		}
		if !strings.Contains(string(raw), "front-door.generated.yaml") {
			t.Errorf("%s's kustomization does not list front-door.generated.yaml, so the generated "+
				"front door is a file rather than a reconciled resource and the environment is "+
				"unreachable from outside", env.dir)
		}
	}
}

// TestEveryEnvironmentDeclaresItsHostLabel checks the input the generator reads
// against the table this package states independently.
//
// Without it the host expectations above would be circular -- computed from the
// same value the manifests were generated from, so a wrong label would be
// adopted as the expectation and every gate would pass.
func TestEveryEnvironmentDeclaresItsHostLabel(t *testing.T) {
	for _, env := range environments {
		t.Run(env.dir, func(t *testing.T) {
			var found bool
			for _, r := range parse(t, render(t, env.dir)) {
				if r.Kind != "ConfigMap" || r.Metadata.Name != "memql-environment" {
					continue
				}
				found = true
				got, declared := r.Data["MEMQL_HOST_LABEL"]
				if !declared {
					t.Fatal("no MEMQL_HOST_LABEL: an environment with a namespace and a schema but no " +
						"host label has data isolation and no front door, which is an environment " +
						"nothing can reach")
				}
				if got != env.hostLabel {
					t.Errorf("MEMQL_HOST_LABEL is %q, want %q", got, env.hostLabel)
				}
			}
			if !found {
				t.Error("no memql-environment ConfigMap rendered")
			}
		})
	}
}

// TestHostLabelsAreDistinct. Two environments on one label generate identical
// hostnames in two namespaces; see TestNoEnvironmentServesAnotherEnvironmentsHost
// for why that resolves by coin flip rather than by error.
func TestHostLabelsAreDistinct(t *testing.T) {
	seen := map[string]string{}
	for _, env := range environments {
		if prior, dup := seen[env.hostLabel]; dup {
			t.Errorf("%s and %s both use host label %q", prior, env.dir, env.hostLabel)
		}
		seen[env.hostLabel] = env.dir
	}
}

// TestGeneratedFrontDoorsAreNotHandEdited. The generated files carry the marker
// every other generated artifact in this tree carries, so a reader who opens one
// is told before they read it. TestFrontDoorHostsAreNotStale is what enforces it;
// this is what makes the enforcement discoverable from the file.
func TestGeneratedFrontDoorsAreNotHandEdited(t *testing.T) {
	for _, env := range environments {
		path := filepath.Join(env.dir, "front-door.generated.yaml")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		if !strings.HasPrefix(string(raw), "# GENERATED by cmd/frontdoorhosts -- DO NOT EDIT.") {
			t.Errorf("%s does not open with the generated marker", path)
		}
	}
}

// sortedHosts is a diagnostic helper for failures above -- kept so a failing
// run prints what the render actually served rather than only what it missed.
func sortedHosts(rendered string) []string {
	var out []string
	for h := range hostsIn(rendered) {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// TestRenderedHostsAreExactlyTheProduct is the closing statement: not merely
// that every expected host is present, but that NO OTHER front-door host is.
//
// The media plane is the deliberate exception and it is subtracted by name.
// Voice is not a front-door host and never will be -- WebRTC media is UDP and
// cannot traverse an HTTP front door -- so deploy/k8s/base/livekit.yaml's
// signaling Ingress is a separate plane, documented as such in
// docs/public/operate/front-door.md.
func TestRenderedHostsAreExactlyTheProduct(t *testing.T) {
	for _, env := range environments {
		t.Run(env.dir, func(t *testing.T) {
			rendered := render(t, env.dir)

			want := map[string]bool{}
			for _, h := range frontdoor.Hosts(env.hostLabel, committedDomain) {
				want[h.Name] = true
			}
			for _, got := range sortedHosts(rendered) {
				if want[got] || strings.HasPrefix(got, "livekit.") {
					continue
				}
				t.Errorf("%s serves %q, which is not in the role x environment product and is not "+
					"the media plane -- a sixth host rule is a design change, not a configuration "+
					"change (docs/public/operate/front-door.md)", env.dir, got)
			}
		})
	}
}
