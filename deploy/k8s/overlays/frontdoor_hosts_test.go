// Package overlays -- the front-door render gates (memql#3767, memql#4224).
//
// WHY A TEST AND NOT A REVIEW. The host set is a DERIVATION -- three roles plus
// the portal plus the sites wildcard plus the apex -- written into ~440 lines
// of Ingress and Certificate by a generator. A host missing a rule is a service
// nothing can reach at the name every client was told to dial: nothing fails to
// build, nothing fails to reconcile, and the symptom arrives whenever somebody
// first dials it.
//
// The certificate half is worse, because it fails at a distance. A missing SAN
// does not stop the Ingress from existing; the request reaches TLS termination,
// the controller serves whatever default certificate it has, and the browser
// reports a name mismatch at a host nobody thinks is new. That is exactly how
// the portal failed on the first keep-it cluster (memql#4224): the Certificate
// requested `*.<domain>`, which an HTTP-01 issuer cannot issue, and the edge
// Ingress listed the wildcard under tls, so ingress-nginx served its
// self-signed default for portal.<domain>. So three things are gated here, by
// exact name: the SANs against the hosts, each Ingress's tls.hosts against its
// own rule hosts, and the union of the tls lists against the SANs.
package overlays

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/frontdoor"
	"gopkg.in/yaml.v3"
)

// committedDomain is the default cmd/frontdoorhosts writes, and the same one
// the local overlay commits. No file under deploy/ names a REAL domain
// (memql#3593) -- an install overrides these hosts through the ArgoCD
// Application's kustomize.patches, and `.localhost` is unroutable so a cluster
// reconciled before its domain is set fails visibly.
const committedDomain = "memql.localhost"

// frontDoorSecret is the one Secret every front-door Ingress terminates with
// and the Certificate creates. It is also how a front-door Ingress is told
// apart from the media plane's (deploy/k8s/base/livekit.yaml names its own).
const frontDoorSecret = "memql-front-door-tls"

// generatedOverlays are the two instance overlays cmd/frontdoorhosts writes.
// Both are gated: the keep-it / client instance (cloud-entry) is the one that
// hit memql#4224 first, and overlays/cloud carries the same generated shape.
var generatedOverlays = []string{cloudOverlay, entryOverlay}

// frontDoorIngressNames is the closed set of Ingress objects the generator
// emits. Listed so that a front-door Ingress that LOST its tls block would be
// noticed rather than silently dropping out of the certificate-union gate,
// which selects by Secret name.
var frontDoorIngressNames = []string{
	"api-front-door", "api-front-door-grpc", "identity-front-door",
	"mcp-front-door", "portal-front-door", "edge-front-door",
}

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
	for _, overlay := range generatedOverlays {
		t.Run(overlay, func(t *testing.T) {
			rendered := render(t, overlay)
			served := hostsIn(rendered)
			for _, h := range frontdoor.Hosts(committedDomain) {
				if !served[h.Name] {
					t.Errorf("the %s overlay does not serve %q (role %s); served: %v",
						overlay, h.Name, h.Role, sortedHosts(rendered))
				}
			}
		})
	}
}

// TestTheApexIsServedAndCertificated. The bare domain needs both its own
// Ingress rule and its own SAN -- it always did, since no wildcard could cover
// it, and now every exact host is in the same position. Missing either is a
// main website that answers with the controller's default certificate.
func TestTheApexIsServedAndCertificated(t *testing.T) {
	for _, overlay := range generatedOverlays {
		t.Run(overlay, func(t *testing.T) {
			rendered := render(t, overlay)
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
				t.Errorf("the apex %q is not a requested SAN (requested: %v)", committedDomain, sans)
			}
		})
	}
}

// TestTheCertificateNamesExactHostsOnly is the memql#4224 decision, gated.
//
// The issuer is HTTP-01. ACME cannot serve an HTTP-01 challenge for
// `*.<domain>`, and ONE wildcard dnsName fails the WHOLE order -- the
// Certificate sits Pending and every host serves the controller's default. So
// the requested SANs are exactly component/frontdoor.CertificateSANs: every
// exact host, in rule order, and no wildcard.
func TestTheCertificateNamesExactHostsOnly(t *testing.T) {
	for _, overlay := range generatedOverlays {
		t.Run(overlay, func(t *testing.T) {
			sans := certificateSANs(t, render(t, overlay))
			if len(sans) == 0 {
				t.Fatalf("the %s overlay requests no front-door certificate at all, so every host in it "+
					"terminates TLS with whatever default the controller has", overlay)
			}
			for _, s := range sans {
				if strings.HasPrefix(s, "*") {
					t.Errorf("the %s overlay requests the wildcard SAN %q; an HTTP-01 issuer fails the whole "+
						"order on it, and the cluster ends up with no certificate (memql#4224)", overlay, s)
				}
			}
			want := frontdoor.CertificateSANs(committedDomain)
			if strings.Join(sans, ",") != strings.Join(want, ",") {
				t.Errorf("the %s overlay requests SANs %v, want %v", overlay, sans, want)
			}
		})
	}
}

// TestEveryExactHostIsCoveredByARequestedSANAndTheWildcardIsNot is what makes
// the gates above mean something together: it is possible to serve every
// expected host and request every expected SAN and still have one host that no
// SAN covers, if the wildcard rule is misread. The rule is ONE label, and it is
// applied here rather than assumed -- and with no wildcard SAN requested, the
// only way a host is covered is by exact name.
//
// The wildcard RULE is asserted NOT covered. That is the honest state of the
// front door under HTTP-01 and the gate says so, rather than letting a future
// wildcard SAN slip back in as "coverage".
func TestEveryExactHostIsCoveredByARequestedSANAndTheWildcardIsNot(t *testing.T) {
	for _, overlay := range generatedOverlays {
		t.Run(overlay, func(t *testing.T) {
			sans := certificateSANs(t, render(t, overlay))
			for _, h := range frontdoor.Hosts(committedDomain) {
				if h.Wildcard {
					if wildcardCovers(h.Name, sans) {
						t.Errorf("the wildcard rule %q is covered by the requested SANs %v; the cloud issuer "+
							"cannot issue that, so this certificate will never become Ready", h.Name, sans)
					}
					continue
				}
				if !wildcardCovers(h.Name, sans) {
					t.Errorf("%q is served but covered by none of the requested SANs %v -- TLS "+
						"termination succeeds with the controller's default certificate, so this "+
						"presents as a browser name mismatch and names nothing", h.Name, sans)
				}
			}
		})
	}
}

// ingressDoc is the slice of a rendered Ingress these gates reason about.
type ingressDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		TLS []struct {
			Hosts      []string `yaml:"hosts"`
			SecretName string   `yaml:"secretName"`
		} `yaml:"tls"`
		Rules []struct {
			Host string `yaml:"host"`
			HTTP struct {
				Paths []struct {
					Backend struct {
						Service struct {
							Name string `yaml:"name"`
						} `yaml:"service"`
					} `yaml:"backend"`
				} `yaml:"paths"`
			} `yaml:"http"`
		} `yaml:"rules"`
	} `yaml:"spec"`
}

// frontDoorIngresses decodes every Ingress in a rendered stream that
// terminates with the front-door Secret, and fails unless that is exactly the
// generator's closed set -- so an Ingress that lost its tls block cannot drop
// out of the union gate unnoticed.
func frontDoorIngresses(t *testing.T, rendered string) []ingressDoc {
	t.Helper()

	dec := yaml.NewDecoder(strings.NewReader(rendered))
	var out []ingressDoc
	for {
		var d ingressDoc
		err := dec.Decode(&d)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decoding document %d of the rendered overlay: %v", len(out)+1, err)
		}
		if d.Kind != "Ingress" {
			continue
		}
		var frontDoor bool
		for _, tls := range d.Spec.TLS {
			if tls.SecretName == frontDoorSecret {
				frontDoor = true
			}
		}
		if frontDoor {
			out = append(out, d)
		}
	}

	var names []string
	for _, d := range out {
		names = append(names, d.Metadata.Name)
	}
	sort.Strings(names)
	want := append([]string(nil), frontDoorIngressNames...)
	sort.Strings(want)
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("the Ingresses terminating with %s are %v, want exactly %v -- an Ingress missing "+
			"here either lost its tls block (and serves the controller's default certificate) or "+
			"is a front-door rule the generator does not know about", frontDoorSecret, names, want)
	}
	return out
}

// TestEachIngressTLSHostsAreItsOwnExactRuleHosts is the half of memql#4224
// that the Certificate alone cannot fix.
//
// ingress-nginx creates a server block per RULE host and verifies the
// certificate against each host listed under tls; a tls host the certificate
// does not name -- the wildcard -- gets the controller's self-signed default.
// So every Ingress lists under tls exactly the exact hosts its rules carry:
// no wildcard, nothing missing, nothing extra.
func TestEachIngressTLSHostsAreItsOwnExactRuleHosts(t *testing.T) {
	for _, overlay := range generatedOverlays {
		t.Run(overlay, func(t *testing.T) {
			for _, ing := range frontDoorIngresses(t, render(t, overlay)) {
				var tlsHosts, ruleHosts []string
				for _, tls := range ing.Spec.TLS {
					if tls.SecretName != frontDoorSecret {
						t.Errorf("%s: a tls entry names Secret %q, want %s", ing.Metadata.Name, tls.SecretName, frontDoorSecret)
					}
					tlsHosts = append(tlsHosts, tls.Hosts...)
				}
				for _, r := range ing.Spec.Rules {
					if r.Host == "" {
						t.Errorf("%s: a rule has no host; every front-door rule is host-routed", ing.Metadata.Name)
						continue
					}
					if strings.HasPrefix(r.Host, "*") {
						continue // the wildcard rule has no certificate behind it
					}
					ruleHosts = append(ruleHosts, r.Host)
				}
				sort.Strings(tlsHosts)
				sort.Strings(ruleHosts)
				if len(tlsHosts) == 0 {
					t.Errorf("%s lists no tls hosts", ing.Metadata.Name)
				}
				for _, h := range tlsHosts {
					if strings.HasPrefix(h, "*") {
						t.Errorf("%s lists the wildcard %q under tls; ingress-nginx cannot verify the "+
							"exact-name certificate for it and serves its self-signed default instead "+
							"(memql#4224)", ing.Metadata.Name, h)
					}
				}
				if strings.Join(tlsHosts, ",") != strings.Join(ruleHosts, ",") {
					t.Errorf("%s: tls.hosts %v, but its exact rule hosts are %v -- the two must match, "+
						"a rule host missing from tls terminates with the controller's default and a "+
						"tls host with no rule has no server block to be verified for",
						ing.Metadata.Name, tlsHosts, ruleHosts)
				}
			}
		})
	}
}

// TestTheUnionOfTLSHostsIsTheCertificate closes the loop: the set of hosts the
// Ingresses terminate TLS for is the set of hosts the Certificate is issued
// for. One name on either side only is a host that presents the controller's
// default (tls without SAN) or a SAN the order pays for and nothing serves.
func TestTheUnionOfTLSHostsIsTheCertificate(t *testing.T) {
	for _, overlay := range generatedOverlays {
		t.Run(overlay, func(t *testing.T) {
			rendered := render(t, overlay)

			union := map[string]bool{}
			for _, ing := range frontDoorIngresses(t, rendered) {
				for _, tls := range ing.Spec.TLS {
					for _, h := range tls.Hosts {
						union[h] = true
					}
				}
			}
			var tlsHosts []string
			for h := range union {
				tlsHosts = append(tlsHosts, h)
			}
			sort.Strings(tlsHosts)

			sans := certificateSANs(t, rendered)
			sort.Strings(sans)

			if strings.Join(tlsHosts, ",") != strings.Join(sans, ",") {
				t.Errorf("the %s overlay terminates TLS for %v but the Certificate names %v", overlay, tlsHosts, sans)
			}
		})
	}
}

// TestThePortalHasItsOwnExactRuleToTheEdge pins the mechanism of the fix. A
// tls entry for portal.<domain> on the wildcard Ingress would NOT have been
// enough -- ingress-nginx has no server block to attach it to -- so the portal
// carries an exact rule of its own, pointing at the same edge Service the
// wildcard does. The wildcard rule stays: it is how every other site reaches
// the edge.
func TestThePortalHasItsOwnExactRuleToTheEdge(t *testing.T) {
	portal := frontdoor.PortalHost(committedDomain)
	wildcard := frontdoor.SitesWildcard(committedDomain)

	for _, overlay := range generatedOverlays {
		t.Run(overlay, func(t *testing.T) {
			var sawPortal, sawWildcard bool
			for _, ing := range frontDoorIngresses(t, render(t, overlay)) {
				for _, r := range ing.Spec.Rules {
					switch r.Host {
					case wildcard:
						sawWildcard = true
					case portal:
						sawPortal = true
						for _, p := range r.HTTP.Paths {
							if p.Backend.Service.Name != "edge" {
								t.Errorf("the portal rule on %s points at Service %q, want edge -- the portal is a site "+
									"and takes the site path; the exact rule exists for TLS only",
									ing.Metadata.Name, p.Backend.Service.Name)
							}
						}
					}
				}
			}
			if !sawPortal {
				t.Errorf("the %s overlay has no exact Ingress rule for %q; ingress-nginx then answers it from the "+
					"wildcard's server block with its self-signed default certificate (memql#4224)", overlay, portal)
			}
			if !sawWildcard {
				t.Errorf("the %s overlay has no %q rule; every other site reaches the edge through it", overlay, wildcard)
			}
		})
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

// wildcardCovers applies the ONE-LABEL rule that TLS applies, so that a
// wildcard SAN -- should a DNS-01 issuer ever bring one back -- is read the
// way a browser reads it. A checker that accepted `*.example.test` for
// `api.staging.example.test` would pass the exact configuration the design
// refuses, so it is spelled out rather than approximated with
// strings.HasSuffix.
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
	if wildcardCovers("shop.example.test", []string{"portal.example.test", "example.test"}) {
		t.Error("wildcardCovers accepted a host that no exact SAN names")
	}
}

// TestTheGeneratedFrontDoorIsReconciled closes the silent-failure hole one
// level up from the manifests: a generated file no kustomization lists is a
// file, not a front door. Nothing fails -- the overlay renders, the mesh comes
// up, and the cluster is simply not reachable from outside.
func TestTheGeneratedFrontDoorIsReconciled(t *testing.T) {
	for _, overlay := range generatedOverlays {
		t.Run(overlay, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(overlay, "kustomization.yaml"))
			if err != nil {
				t.Fatalf("reading the %s kustomization: %v", overlay, err)
			}
			if !strings.Contains(string(raw), "front-door.generated.yaml") {
				t.Errorf("the %s kustomization does not list front-door.generated.yaml, so the generated "+
					"front door is a file rather than a reconciled resource and the cluster is "+
					"unreachable from outside", overlay)
			}
		})
	}
}

// TestGeneratedFrontDoorIsNotHandEdited. The generated file carries the marker
// every other generated artifact in this tree carries, so a reader who opens it
// is told before they read it. TestFrontDoorHostsAreNotStale is what enforces
// it; this is what makes the enforcement discoverable from the file.
func TestGeneratedFrontDoorIsNotHandEdited(t *testing.T) {
	for _, overlay := range generatedOverlays {
		t.Run(overlay, func(t *testing.T) {
			path := filepath.Join(overlay, "front-door.generated.yaml")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading %s: %v", path, err)
			}
			if !strings.HasPrefix(string(raw), "# GENERATED by cmd/frontdoorhosts -- DO NOT EDIT.") {
				t.Errorf("%s does not open with the generated marker", path)
			}
		})
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
	for _, overlay := range generatedOverlays {
		t.Run(overlay, func(t *testing.T) {
			rendered := render(t, overlay)

			want := map[string]bool{}
			for _, h := range frontdoor.Hosts(committedDomain) {
				want[h.Name] = true
			}
			for _, got := range sortedHosts(rendered) {
				if want[got] || strings.HasPrefix(got, "livekit.") {
					continue
				}
				t.Errorf("the %s overlay serves %q, which is not one of the six derived hosts and is "+
					"not the media plane -- a seventh host rule is a design change, not a configuration "+
					"change (docs/public/operate/front-door.md)", overlay, got)
			}
		})
	}
}
