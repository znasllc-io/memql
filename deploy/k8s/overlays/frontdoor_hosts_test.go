// Package overlays -- the front-door render gates (memql#3767).
//
// WHY A TEST AND NOT A REVIEW. The host set is a DERIVATION -- three roles plus
// the sites wildcard plus the apex -- written into ~390 lines of Ingress and
// Certificate by a generator. A host missing a rule is a service nothing can
// reach at the name every client was told to dial: nothing fails to build,
// nothing fails to reconcile, and the symptom arrives whenever somebody first
// dials it.
//
// The certificate half is worse, because it fails at a distance. A missing SAN
// does not stop the Ingress from existing; the request reaches TLS termination,
// the controller serves whatever default certificate it has, and the browser
// reports a name mismatch at a host nobody thinks is new. So the SANs are
// checked against the hosts, by the one-label wildcard rule rather than by
// string containment.
package overlays

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/frontdoor"
)

// committedDomain is the default cmd/frontdoorhosts writes, and the same one
// the local overlay commits. No file under deploy/ names a REAL domain
// (memql#3593) -- an install overrides these hosts through the ArgoCD
// Application's kustomize.patches, and `.localhost` is unroutable so a cluster
// reconciled before its domain is set fails visibly.
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

// TestEveryGeneratedHostRuleIsServed is the acceptance criterion in test form.
//
// The expectation is COMPUTED from component/frontdoor rather than listed, so
// the generator and the gate cannot disagree about what the host set is -- and
// a listed expectation would have to be edited by whoever changes the role set,
// which is the hand-maintenance this whole arrangement removes.
func TestEveryGeneratedHostRuleIsServed(t *testing.T) {
	served := hostsIn(render(t, cloudOverlay))
	for _, h := range frontdoor.Hosts(committedDomain) {
		if !served[h.Name] {
			t.Errorf("the cloud overlay does not serve %q (role %s); served: %v",
				h.Name, h.Role, sortedHosts(render(t, cloudOverlay)))
		}
	}
}

// TestTheApexIsServedAndCertificated. The bare domain is the one host no
// wildcard covers -- `*.<domain>` matches exactly one label and the apex has
// none -- so it needs both its own Ingress rule and its own SAN. Missing either
// is a main website that answers with the controller's default certificate.
func TestTheApexIsServedAndCertificated(t *testing.T) {
	rendered := render(t, cloudOverlay)
	if !hostsIn(rendered)[committedDomain] {
		t.Errorf("the apex %q has no Ingress rule; for a customer cluster the bare domain IS their main website", committedDomain)
	}
	sans := certificateSANs(t, rendered)
	var found bool
	for _, s := range sans {
		if s == committedDomain {
			found = true
		}
	}
	if !found {
		t.Errorf("the apex %q is not a requested SAN (requested: %v); no wildcard covers it", committedDomain, sans)
	}
}

// TestTheWildcardCertificateIsRequested is the second half, and the reason
// every front-door host is a single label: `*.<domain>` covers all of them with
// one order and one renewal, however many sites the cluster serves.
func TestTheWildcardCertificateIsRequested(t *testing.T) {
	sans := certificateSANs(t, render(t, cloudOverlay))
	if len(sans) == 0 {
		t.Fatal("the cloud overlay requests no front-door certificate at all, so every host in it " +
			"terminates TLS with whatever default the controller has")
	}
	want := frontdoor.CertificateSANs(committedDomain)
	if strings.Join(sans, ",") != strings.Join(want, ",") {
		t.Errorf("requests SANs %v, want %v", sans, want)
	}
}

// TestEveryServedHostIsCoveredByARequestedSAN is what makes the two gates above
// mean something together: it is possible to serve every expected host and
// request every expected SAN and still have one host that no SAN covers, if the
// wildcard rule is misread. The rule is ONE label, and it is applied here rather
// than assumed.
func TestEveryServedHostIsCoveredByARequestedSAN(t *testing.T) {
	sans := certificateSANs(t, render(t, cloudOverlay))
	for _, h := range frontdoor.Hosts(committedDomain) {
		if !wildcardCovers(h.Name, sans) {
			t.Errorf("%q is served but covered by none of the requested SANs %v -- TLS "+
				"termination succeeds with the controller's default certificate, so this "+
				"presents as a browser name mismatch and names nothing", h.Name, sans)
		}
	}
}

// certificateSANs returns the dnsNames of every Certificate in a rendered
// stream.
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
// up, and the cluster is simply not reachable from outside.
func TestTheGeneratedFrontDoorIsReconciled(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(cloudOverlay, "kustomization.yaml"))
	if err != nil {
		t.Fatalf("reading the cloud kustomization: %v", err)
	}
	if !strings.Contains(string(raw), "front-door.generated.yaml") {
		t.Error("the cloud kustomization does not list front-door.generated.yaml, so the generated " +
			"front door is a file rather than a reconciled resource and the cluster is " +
			"unreachable from outside")
	}
}

// TestGeneratedFrontDoorIsNotHandEdited. The generated file carries the marker
// every other generated artifact in this tree carries, so a reader who opens it
// is told before they read it. TestFrontDoorHostsAreNotStale is what enforces
// it; this is what makes the enforcement discoverable from the file.
func TestGeneratedFrontDoorIsNotHandEdited(t *testing.T) {
	path := filepath.Join(cloudOverlay, "front-door.generated.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if !strings.HasPrefix(string(raw), "# GENERATED by cmd/frontdoorhosts -- DO NOT EDIT.") {
		t.Errorf("%s does not open with the generated marker", path)
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
	rendered := render(t, cloudOverlay)

	want := map[string]bool{}
	for _, h := range frontdoor.Hosts(committedDomain) {
		want[h.Name] = true
	}
	for _, got := range sortedHosts(rendered) {
		if want[got] || strings.HasPrefix(got, "livekit.") {
			continue
		}
		t.Errorf("the cloud overlay serves %q, which is not one of the five derived hosts and is "+
			"not the media plane -- a sixth host rule is a design change, not a configuration "+
			"change (docs/public/operate/front-door.md)", got)
	}
}
