// Package overlays -- the front-door render gates (memql#3767, memql#4224, memql#4347).
//
// WHY A TEST AND NOT A REVIEW. The host set is a DERIVATION -- three roles plus
// the OS shell plus the sites wildcard plus the apex -- written into ~400 lines
// of Ingress and Certificate by a generator. A host missing a rule is a service
// nothing can reach at the name every client was told to dial: nothing fails to
// build, nothing fails to reconcile, and the symptom arrives whenever somebody
// first dials it.
//
// The certificate half is worse, because it fails at a distance. A missing SAN
// does not stop the Ingress from existing; the request reaches TLS termination,
// the controller serves whatever default certificate it has, and the browser
// reports a name mismatch at a host nobody thinks is new. That is exactly how
// the platform's own site host failed on the first entry-shape cluster
// (memql#4224): the Certificate requested `*.<domain>`, which an HTTP-01 issuer
// cannot issue, and the edge Ingress listed the wildcard under tls, so
// ingress-nginx served its self-signed default for it. So three things are gated here, by
// exact name: the SANs against the hosts, each Ingress's tls.hosts against its
// own rule hosts, and the requested SANs against the rules that serve them.
//
// # TWO ISSUER REGIMES, AND THE GATES READ WHICH ONE IS IN PLAY (memql#4347)
//
// memql#4224's rule -- "no wildcard SAN, and no wildcard under tls" -- was never
// a preference. It followed from ONE fact about the issuer: letsencrypt-prod
// solves HTTP-01, ACME cannot serve an HTTP-01 challenge for a name that is not
// a host, and one wildcard dnsName fails the WHOLE order rather than the one
// name. A DNS-01 solver proves control of the ZONE by writing a TXT record,
// which is a claim over every name under it, so the same wildcard becomes
// issuable. The manifests are therefore checked against the SOLVER, not against
// a remembered conclusion:
//
//	HTTP-01 regime -- no DNS-01 issuer declared in the overlay. Every Certificate
//	names exact hosts only, no Ingress lists a wildcard under tls, and the
//	`*.<domain>` RULE is asserted to be covered by NOTHING. This is memql#4224
//	unchanged, and it is the DEFAULT: an issuer this render does not contain is
//	read as HTTP-01, so the strict regime stays in force by omission rather than
//	needing to be re-chosen.
//
//	DNS-01 regime -- the overlay declares a ClusterIssuer with a dns01 solver.
//	A Certificate issued BY THAT ISSUER may name `*.<domain>`, and the edge
//	Ingress's wildcard rule must then carry its tls entry -- a rule host with a
//	usable certificate and no tls entry gets the controller's default anyway, so
//	under this regime the omission is the bug.
//
// The two overlays are in the MIXED state today, deliberately: letsencrypt-prod
// still issues the exact-host certificate for api / identity / mcp / os and
// the apex, and letsencrypt-dns01 issues one additional certificate for the
// sites plane. Nothing here waves that through as a special case --
// TestBothOverlaysDeclareTheDNS01Issuer states it, so a regression to either
// single regime fails rather than being absorbed.
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

// frontDoorSecret is the Secret the EXACT-host certificate creates and every
// role host terminates with.
const frontDoorSecret = "memql-front-door-tls"

// wildcardSecret is the Secret the DNS-01 certificate creates (memql#4347). A
// SECOND secret rather than a replacement: the exact hosts keep terminating
// with frontDoorSecret, so a DNS-01 misconfiguration cannot reach sign-in.
const wildcardSecret = "memql-wildcard-tls"

// generatedOverlays are the two instance overlays cmd/frontdoorhosts writes.
// Both are gated: the entry / client instance (cloud-entry) is the one that
// hit memql#4224 first, and overlays/cloud carries the same generated shape.
var generatedOverlays = []string{cloudOverlay, entryOverlay}

// frontDoorIngressNames is the closed set of Ingress objects the generator
// emits. Listed so that a front-door Ingress that LOST its tls block would be
// noticed rather than silently dropping out of the certificate gates, which
// select by Secret name.
var frontDoorIngressNames = []string{
	"api-front-door", "api-front-door-grpc", "identity-front-door",
	"mcp-front-door", "os-front-door", "edge-front-door",
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
// it, and a DNS-01 wildcard does not change that: `*.<domain>` matches exactly
// one label and the apex has none. Missing either is a main website that
// answers with the controller's default certificate.
func TestTheApexIsServedAndCertificated(t *testing.T) {
	for _, overlay := range generatedOverlays {
		t.Run(overlay, func(t *testing.T) {
			rendered := render(t, overlay)
			if !hostsIn(rendered)[committedDomain] {
				t.Errorf("the apex %q has no Ingress rule; for a customer cluster the bare domain IS their main website", committedDomain)
			}
			sans := requestedSANs(t, rendered)
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

// TestTheFrontDoorCertificateNamesExactHostsOnly is the memql#4224 decision,
// gated -- now scoped to the certificate it was always about.
//
// memql-front-door-tls is issued by letsencrypt-prod, which solves HTTP-01.
// ACME cannot serve an HTTP-01 challenge for `*.<domain>`, and ONE wildcard
// dnsName fails the WHOLE order -- the Certificate sits Pending and every host
// under it serves the controller's default. So its requested SANs are exactly
// component/frontdoor.CertificateSANs: every exact host, in rule order, and no
// wildcard.
//
// THE WILDCARD CERTIFICATE DOES NOT RELAX THIS. memql#4347 adds a second
// Certificate under a DNS-01 issuer; it does not move a single name off this
// one. If the two are ever merged, this gate is what has to be re-argued
// deliberately rather than discovered to have stopped applying.
func TestTheFrontDoorCertificateNamesExactHostsOnly(t *testing.T) {
	for _, overlay := range generatedOverlays {
		t.Run(overlay, func(t *testing.T) {
			cert := certificateFor(t, render(t, overlay), frontDoorSecret)
			for _, s := range cert.DNSNames {
				if strings.HasPrefix(s, "*") {
					t.Errorf("the %s overlay requests the wildcard SAN %q on %s; its issuer %s solves "+
						"HTTP-01, which fails the whole order on a wildcard, and the cluster ends up "+
						"with no certificate for any exact host (memql#4224)",
						overlay, s, frontDoorSecret, cert.IssuerName)
				}
			}
			want := frontdoor.CertificateSANs(committedDomain)
			if strings.Join(cert.DNSNames, ",") != strings.Join(want, ",") {
				t.Errorf("the %s overlay requests SANs %v on %s, want %v",
					overlay, cert.DNSNames, frontDoorSecret, want)
			}
		})
	}
}

// TestAWildcardSANIsRequestedOnlyFromADNS01Issuer is the memql#4224 rule made
// CONDITIONAL rather than repealed (memql#4347).
//
// The rule is about the solver, not about the name of the issuer and not about
// which certificate it is: any Certificate in the overlay may name `*.<domain>`
// if and only if its issuerRef names an issuer DECLARED IN THIS OVERLAY whose
// ACME solver list contains a dns01 solver.
//
// "Declared in this overlay" is doing real work. letsencrypt-prod is created
// out of band on the cluster, so this render cannot see what it solves -- and
// an issuer the gate cannot inspect is read as HTTP-01, which keeps the strict
// regime in force by default. The alternative (assume DNS-01 unless proven
// otherwise) would silently permit exactly the Pending-forever Certificate that
// memql#4224 was filed for.
func TestAWildcardSANIsRequestedOnlyFromADNS01Issuer(t *testing.T) {
	for _, overlay := range generatedOverlays {
		t.Run(overlay, func(t *testing.T) {
			rendered := render(t, overlay)
			dns01 := dns01Issuers(t, rendered)
			certs := certificatesIn(t, rendered)
			if len(certs) == 0 {
				t.Fatalf("the %s overlay requests no certificate at all, so every host in it "+
					"terminates TLS with whatever default the controller has", overlay)
			}
			for _, c := range certs {
				for _, s := range c.DNSNames {
					if !strings.HasPrefix(s, "*") {
						continue
					}
					if !dns01[c.IssuerName] {
						t.Errorf("the %s overlay requests the wildcard SAN %q on Certificate %s, whose "+
							"issuer %q is not a DNS-01 issuer declared in this overlay. ACME cannot "+
							"serve an HTTP-01 challenge for a wildcard and one wildcard dnsName fails "+
							"the whole order, so this Certificate would sit Pending forever "+
							"(memql#4224). Declared DNS-01 issuers here: %v",
							overlay, s, c.Name, c.IssuerName, sortedKeys(dns01))
					}
				}
			}
		})
	}
}

// TestBothOverlaysDeclareTheDNS01Issuer states the MIXED state explicitly, so
// that it is a decision rather than something the gates happen to tolerate.
//
// memql#4347 asked for the mixed regime to be accepted deliberately: HTTP-01
// keeps the role hosts while DNS-01 takes the sites plane. Two failures this
// catches, in opposite directions -- the DNS-01 issuer or its certificate
// quietly disappearing (the wildcard rule silently back to the controller's
// default, which is the memql#4224 symptom at a host nobody re-tests), and the
// conditional above going vacuous because there is no longer a DNS-01 issuer
// anywhere for it to be conditional ON.
func TestBothOverlaysDeclareTheDNS01Issuer(t *testing.T) {
	for _, overlay := range generatedOverlays {
		t.Run(overlay, func(t *testing.T) {
			rendered := render(t, overlay)

			if dns01 := dns01Issuers(t, rendered); len(dns01) == 0 {
				t.Fatalf("the %s overlay declares no DNS-01 issuer, so the wildcard rule has no "+
					"certificate behind it and every hosted site terminates TLS with ingress-nginx's "+
					"self-signed default (memql#4347)", overlay)
			}

			cert := certificateFor(t, rendered, wildcardSecret)
			want := []string{frontdoor.SitesWildcard(committedDomain), frontdoor.Apex(committedDomain)}
			if strings.Join(cert.DNSNames, ",") != strings.Join(want, ",") {
				t.Errorf("the %s overlay's %s names %v, want %v -- the apex is on it as well as the "+
					"wildcard because `*.<domain>` matches exactly one label and the apex has none, "+
					"so a sites-plane certificate without it could not serve the main website",
					overlay, wildcardSecret, cert.DNSNames, want)
			}
			// The exact-host certificate must still come from a DIFFERENT
			// issuer. Collapsing the two is a design change -- one order for
			// everything, HTTP-01 retired -- and has to be argued, not arrived
			// at, because it puts sign-in behind the DNS-01 solver too.
			exact := certificateFor(t, rendered, frontDoorSecret)
			if exact.IssuerName == cert.IssuerName {
				t.Errorf("the %s overlay issues both %s and %s from %q; the staged reversal keeps the "+
					"exact hosts on HTTP-01 precisely so a DNS-01 misconfiguration cannot reach sign-in",
					overlay, frontDoorSecret, wildcardSecret, cert.IssuerName)
			}
		})
	}
}

// TestEveryHostIsCoveredExactlyWhenItsRegimeAllows is what makes the gates
// above mean something together: it is possible to serve every expected host
// and request every expected SAN and still have one host that no SAN covers, if
// the wildcard rule is misread. The rule is ONE label, and it is applied here
// rather than assumed.
//
// BOTH REGIMES, STATED (memql#4347). Every EXACT host must be covered by a
// requested SAN under either issuer -- that half never changes. The wildcard
// rule is covered if and only if the overlay declares a DNS-01 issuer:
//
//   - HTTP-01 only: asserted NOT covered. That is the honest state of the front
//     door under that issuer, and saying so is what stopped a wildcard SAN
//     slipping back in as "coverage" (memql#4224).
//   - DNS-01 declared: asserted covered. An issuer that is declared and then not
//     used for the one name it exists to issue is the whole feature missing,
//     with every manifest looking present.
func TestEveryHostIsCoveredExactlyWhenItsRegimeAllows(t *testing.T) {
	for _, overlay := range generatedOverlays {
		t.Run(overlay, func(t *testing.T) {
			rendered := render(t, overlay)
			sans := requestedSANs(t, rendered)
			wildcardIssuable := len(dns01Issuers(t, rendered)) > 0

			for _, h := range frontdoor.Hosts(committedDomain) {
				covered := wildcardCovers(h.Name, sans)
				if h.Wildcard {
					switch {
					case wildcardIssuable && !covered:
						t.Errorf("the %s overlay declares a DNS-01 issuer but no requested SAN covers the "+
							"wildcard rule %q (requested: %v) -- every hosted site still terminates with "+
							"the controller's default certificate (memql#4347)", overlay, h.Name, sans)
					case !wildcardIssuable && covered:
						t.Errorf("the wildcard rule %q is covered by the requested SANs %v, but this overlay "+
							"declares no DNS-01 issuer; an HTTP-01 order fails whole on that name, so this "+
							"certificate will never become Ready (memql#4224)", h.Name, sans)
					}
					continue
				}
				if !covered {
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

// frontDoorIngresses decodes every Ingress in a rendered stream that terminates
// with a Secret one of THIS OVERLAY'S Certificates creates, and fails unless
// that is exactly the generator's closed set -- so an Ingress that lost its tls
// block cannot drop out of the coverage gates unnoticed.
//
// Selecting by "a Secret a declared Certificate writes" rather than by the one
// hard-coded name is what lets the edge Ingress carry two certificates without
// the selector having to learn each new secret by hand. An Ingress that asks
// cert-manager's ingress-shim for a certificate through an annotation is
// excluded by that same rule rather than by a special case: no Certificate
// object for it exists in the render.
func frontDoorIngresses(t *testing.T, rendered string) []ingressDoc {
	t.Helper()

	issued := issuedSecrets(t, rendered)

	dec := yaml.NewDecoder(strings.NewReader(rendered))
	var out []ingressDoc
	for i := 0; ; i++ {
		var d ingressDoc
		err := dec.Decode(&d)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decoding document %d of the rendered overlay: %v", i+1, err)
		}
		if d.Kind != "Ingress" {
			continue
		}
		var frontDoor bool
		for _, tls := range d.Spec.TLS {
			if issued[tls.SecretName] {
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
		t.Fatalf("the Ingresses terminating with a certificate this overlay declares are %v, want "+
			"exactly %v -- an Ingress missing here either lost its tls block (and serves the "+
			"controller's default certificate) or is a front-door rule the generator does not "+
			"know about", names, want)
	}
	return out
}

// TestEachIngressTLSHostsAreItsCertifiableRuleHosts is the half of memql#4224
// that the Certificate alone cannot fix, restated for two certificates
// (memql#4347).
//
// ingress-nginx creates a server block per RULE host and verifies the
// certificate against each host listed under tls; a tls host the named Secret's
// certificate does not cover gets the controller's self-signed default. So two
// equalities per Ingress:
//
//   - every host under a tls entry is covered by the Certificate that writes
//     THAT entry's Secret. Not "some certificate in the overlay" -- nginx serves
//     the Secret the entry names, so a host covered only by the other one still
//     gets the default.
//   - the set of tls hosts equals the set of rule hosts something in this
//     overlay can certify. That is the both-regimes form of "no wildcard under
//     tls": under HTTP-01 nothing can certify `*.<domain>`, so it must not be
//     listed (memql#4224); with the DNS-01 certificate it can be, so it must be
//     -- an uncertified rule host and an unlisted certifiable one are the same
//     browser warning.
func TestEachIngressTLSHostsAreItsCertifiableRuleHosts(t *testing.T) {
	for _, overlay := range generatedOverlays {
		t.Run(overlay, func(t *testing.T) {
			rendered := render(t, overlay)

			bySecret := map[string]certificateDoc{}
			for _, c := range certificatesIn(t, rendered) {
				bySecret[c.SecretName] = c
			}

			for _, ing := range frontDoorIngresses(t, rendered) {
				var tlsHosts, certifiableRuleHosts []string
				for _, tls := range ing.Spec.TLS {
					cert, ok := bySecret[tls.SecretName]
					if !ok {
						t.Errorf("%s terminates with Secret %q, which no Certificate in this overlay "+
							"writes -- the Secret is never created and every host in the entry serves "+
							"the controller's default", ing.Metadata.Name, tls.SecretName)
						continue
					}
					for _, h := range tls.Hosts {
						if !wildcardCovers(h, cert.DNSNames) {
							t.Errorf("%s lists %q under tls against Secret %s, whose Certificate names %v. "+
								"ingress-nginx verifies the certificate it is pointed at and falls back to "+
								"its self-signed default for a host that certificate does not cover "+
								"(memql#4224)", ing.Metadata.Name, h, tls.SecretName, cert.DNSNames)
						}
					}
					tlsHosts = append(tlsHosts, tls.Hosts...)
				}
				for _, r := range ing.Spec.Rules {
					if r.Host == "" {
						t.Errorf("%s: a rule has no host; every front-door rule is host-routed", ing.Metadata.Name)
						continue
					}
					if !anyCertificateCovers(r.Host, bySecret) {
						continue // nothing here can issue for it; the regime gates own that case
					}
					certifiableRuleHosts = append(certifiableRuleHosts, r.Host)
				}
				sort.Strings(tlsHosts)
				sort.Strings(certifiableRuleHosts)
				if len(tlsHosts) == 0 {
					t.Errorf("%s lists no tls hosts", ing.Metadata.Name)
				}
				if strings.Join(tlsHosts, ",") != strings.Join(certifiableRuleHosts, ",") {
					t.Errorf("%s: tls.hosts %v, but the rule hosts this overlay can certify are %v -- the "+
						"two must match. A rule host missing from tls terminates with the controller's "+
						"default, and a tls host with no rule has no server block to be verified for",
						ing.Metadata.Name, tlsHosts, certifiableRuleHosts)
				}
			}
		})
	}
}

// TestEveryRequestedSANIsAHostTheFrontDoorServes closes the loop from the other
// side: a SAN is a name the ACME order pays for and every renewal keeps paying
// for, so each one must be a host some front-door rule actually answers.
//
// This used to be stated as strict set equality between the union of tls hosts
// and the certificate's dnsNames, which was the right shape while there was one
// certificate. With two it would forbid the apex appearing on both -- which is
// deliberate (memql#4347: `*.<domain>` cannot cover the apex, so a sites-plane
// certificate without it is a trap for whoever later retires HTTP-01). What
// survives is the invariant that was doing the work: no SAN nothing serves,
// and -- asserted next door, per Secret -- no tls host without a SAN.
//
// IT APPLIES TO ACME CERTIFICATES ONLY (memql#4484). The reason above is the
// scope: an ORDER is what a useless SAN costs, and only an ACME issuer places
// orders. An internally-issued certificate -- the memql-ca / identity-tls chain
// off a selfSigned Issuer -- costs nothing, is never served by the front door,
// and carries in-cluster Service names by design. Holding it to this rule would
// be asking an internal certificate to justify itself against a public one.
//
// WHICH ISSUERS ARE ACME IS DERIVED, not listed: the rendered Issuer and
// ClusterIssuer objects are read and the ones carrying `spec.acme` are the ACME
// ones. So a NEW internal certificate needs no edit here, and a new ACME
// certificate is covered the moment it renders -- which is the direction the
// mistakes go in.
func TestEveryRequestedSANIsAHostTheFrontDoorServes(t *testing.T) {
	for _, overlay := range generatedOverlays {
		t.Run(overlay, func(t *testing.T) {
			rendered := render(t, overlay)

			ruleHosts := map[string]bool{}
			for _, ing := range frontDoorIngresses(t, rendered) {
				for _, r := range ing.Spec.Rules {
					ruleHosts[r.Host] = true
				}
			}
			if len(ruleHosts) == 0 {
				t.Fatal("no front-door rule hosts rendered; this gate cannot pass on an overlay it did not read")
			}

			acme := acmeIssuersIn(t, rendered)
			if len(acme) == 0 {
				t.Fatal("no ACME issuer rendered; this gate cannot pass on an overlay whose " +
					"certificates it classified as all-internal")
			}
			for _, c := range certificatesIn(t, rendered) {
				if !acme[c.IssuerName] {
					continue // internal chain: no order, no renewal, no front door
				}
				for _, s := range c.DNSNames {
					if !ruleHosts[s] {
						t.Errorf("the %s overlay's Certificate %s requests %q, which no front-door rule "+
							"serves -- the order and every renewal pay for a name nothing answers at. "+
							"Rules: %v", overlay, c.Name, s, sortedKeys(ruleHosts))
					}
				}
			}
		})
	}
}

// TestTheOsHasItsOwnExactRuleToTheEdge pins the mechanism of the memql#4224
// fix, which the OS shell now carries alone (memql#4705; the portal held this
// slot and had a twin of this test until epic memql#4984).
//
// A tls entry for os.<domain> on the wildcard Ingress would NOT be enough --
// ingress-nginx has no server block to attach it to -- so the OS carries an
// exact rule of its own, pointing at the same edge Service the wildcard does.
// The wildcard rule stays: it is how every other site reaches the edge.
//
// The DNS-01 wildcard certificate does not retire this rule. It removes the
// CERTIFICATE reason the OS needs one, not the server-block reason: nginx
// still builds a certificate-bearing server block per rule host, and the OS's
// exact rule is what outranks the wildcard for that name.
func TestTheOsHasItsOwnExactRuleToTheEdge(t *testing.T) {
	osHost := frontdoor.OsHost(committedDomain)
	wildcard := frontdoor.SitesWildcard(committedDomain)

	for _, overlay := range generatedOverlays {
		t.Run(overlay, func(t *testing.T) {
			var sawOs, sawWildcard bool
			for _, ing := range frontDoorIngresses(t, render(t, overlay)) {
				for _, r := range ing.Spec.Rules {
					switch r.Host {
					case wildcard:
						sawWildcard = true
					case osHost:
						sawOs = true
						for _, p := range r.HTTP.Paths {
							if p.Backend.Service.Name != "edge" {
								t.Errorf("the OS rule on %s points at Service %q, want edge -- the OS is a site "+
									"and takes the site path; the exact rule exists for TLS only",
									ing.Metadata.Name, p.Backend.Service.Name)
							}
						}
					}
				}
			}
			if !sawOs {
				t.Errorf("the %s overlay has no exact Ingress rule for %q; ingress-nginx then answers it from the "+
					"wildcard's server block with its self-signed default certificate (memql#4224 / #4705)", overlay, osHost)
			}
			if !sawWildcard {
				t.Errorf("the %s overlay has no %q rule; every other site reaches the edge through it", overlay, wildcard)
			}
		})
	}
}

// certificateDoc is the slice of a rendered cert-manager Certificate these
// gates reason about.
type certificateDoc struct {
	Name       string
	SecretName string
	IssuerName string
	IssuerKind string
	DNSNames   []string
}

// eachDocument hands every document of a rendered stream to fn along with its
// kind, decoding lazily so that a document of an unrelated kind can never fail
// a typed decode meant for another one.
//
// A mid-stream decode error is FATAL rather than a break, for the reason
// parse() gives in render_cloud_test.go: stopping quietly at the first bad
// document leaves every assertion reasoning about a truncated prefix of the
// overlay, and they would all pass having checked a fraction of it.
func eachDocument(t *testing.T, rendered string, fn func(kind string, doc *yaml.Node)) {
	t.Helper()

	dec := yaml.NewDecoder(strings.NewReader(rendered))
	for i := 0; ; i++ {
		var node yaml.Node
		err := dec.Decode(&node)
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			t.Fatalf("decoding document %d of the rendered overlay: %v", i+1, err)
		}
		var probe struct {
			Kind string `yaml:"kind"`
		}
		if err := node.Decode(&probe); err != nil || probe.Kind == "" {
			continue
		}
		fn(probe.Kind, &node)
	}
}

// certificatesIn decodes every cert-manager Certificate in a rendered stream.
func certificatesIn(t *testing.T, rendered string) []certificateDoc {
	t.Helper()

	var out []certificateDoc
	eachDocument(t, rendered, func(kind string, doc *yaml.Node) {
		if kind != "Certificate" {
			return
		}
		var c struct {
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
			Spec struct {
				SecretName string   `yaml:"secretName"`
				DNSNames   []string `yaml:"dnsNames"`
				IssuerRef  struct {
					Name string `yaml:"name"`
					Kind string `yaml:"kind"`
				} `yaml:"issuerRef"`
			} `yaml:"spec"`
		}
		if err := doc.Decode(&c); err != nil {
			t.Fatalf("decoding a Certificate: %v", err)
		}
		out = append(out, certificateDoc{
			Name:       c.Metadata.Name,
			SecretName: c.Spec.SecretName,
			IssuerName: c.Spec.IssuerRef.Name,
			IssuerKind: c.Spec.IssuerRef.Kind,
			DNSNames:   c.Spec.DNSNames,
		})
	})
	return out
}

// certificateFor returns the Certificate that writes a named Secret, and fails
// unless there is exactly one. None is a Secret that never exists and hosts
// that terminate with the controller's default; two is a race whose winner is
// whichever reconciled last.
func certificateFor(t *testing.T, rendered, secret string) certificateDoc {
	t.Helper()

	var found []certificateDoc
	for _, c := range certificatesIn(t, rendered) {
		if c.SecretName == secret {
			found = append(found, c)
		}
	}
	switch len(found) {
	case 1:
		return found[0]
	case 0:
		t.Fatalf("no Certificate writes Secret %q; every host that terminates with it serves whatever "+
			"default the controller has", secret)
	default:
		t.Fatalf("%d Certificates write Secret %q; they overwrite each other and which one wins is "+
			"whichever reconciled last", len(found), secret)
	}
	return certificateDoc{}
}

// issuedSecrets is the set of Secret names the overlay's Certificates create.
func issuedSecrets(t *testing.T, rendered string) map[string]bool {
	t.Helper()

	out := map[string]bool{}
	for _, c := range certificatesIn(t, rendered) {
		out[c.SecretName] = true
	}
	return out
}

// requestedSANs is every dnsName the overlay's Certificates request, across all
// of them, de-duplicated -- the apex is deliberately on two (memql#4347).
func requestedSANs(t *testing.T, rendered string) []string {
	t.Helper()

	seen := map[string]bool{}
	var out []string
	for _, c := range certificatesIn(t, rendered) {
		for _, s := range c.DNSNames {
			if seen[s] {
				continue
			}
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// dns01Issuers is the set of Issuer / ClusterIssuer names DECLARED IN THIS
// OVERLAY whose ACME solver list contains a dns01 solver.
//
// Read from the SOLVER, never from the name: `letsencrypt-dns01` is a label an
// operator chose and could put on an HTTP-01 issuer, while the property that
// makes a wildcard issuable is which challenge the ACME server is asked to
// verify. An issuer this render does not contain is absent from this set, which
// is what keeps the memql#4224 regime in force for letsencrypt-prod -- created
// out of band, so nothing here can inspect it.
func dns01Issuers(t *testing.T, rendered string) map[string]bool {
	t.Helper()

	out := map[string]bool{}
	eachDocument(t, rendered, func(kind string, doc *yaml.Node) {
		if kind != "ClusterIssuer" && kind != "Issuer" {
			return
		}
		var iss struct {
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
			Spec struct {
				ACME struct {
					Solvers []struct {
						DNS01  map[string]any `yaml:"dns01"`
						HTTP01 map[string]any `yaml:"http01"`
					} `yaml:"solvers"`
				} `yaml:"acme"`
			} `yaml:"spec"`
		}
		if err := doc.Decode(&iss); err != nil {
			t.Fatalf("decoding a %s: %v", kind, err)
		}
		for _, s := range iss.Spec.ACME.Solvers {
			if len(s.DNS01) > 0 {
				out[iss.Metadata.Name] = true
			}
		}
	})
	return out
}

// anyCertificateCovers reports whether some Certificate in the overlay names a
// SAN covering a host, applying the one-label wildcard rule.
func anyCertificateCovers(host string, bySecret map[string]certificateDoc) bool {
	for _, c := range bySecret {
		if wildcardCovers(host, c.DNSNames) {
			return true
		}
	}
	return false
}

// sortedKeys is a diagnostic helper: a failure message that names what the gate
// DID find is the whole of what the person who tripped it learns.
func sortedKeys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// wildcardCovers applies the ONE-LABEL rule that TLS applies, so that a
// wildcard SAN is read the way a browser reads it. A checker that accepted
// `*.example.test` for `api.staging.example.test` would pass a configuration
// no browser accepts, so it is spelled out rather than approximated with
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
	if wildcardCovers("shop.example.test", []string{"os.example.test", "example.test"}) {
		t.Error("wildcardCovers accepted a host that no exact SAN names")
	}
	// The apex is NOT covered by its own wildcard -- which is why the DNS-01
	// certificate names both (memql#4347). If this ever passed, that second
	// dnsName would read as redundant to the next person who tidied it.
	if wildcardCovers("example.test", []string{"*.example.test"}) {
		t.Error("wildcardCovers accepted the apex under its own wildcard")
	}
	// And the wildcard RULE host is covered only by an identical SAN, which is
	// the comparison the regime gates make.
	if wildcardCovers("*.example.test", []string{"os.example.test", "example.test"}) {
		t.Error("wildcardCovers accepted the wildcard rule under exact SANs")
	}
	if !wildcardCovers("*.example.test", []string{"*.example.test"}) {
		t.Error("wildcardCovers rejected the wildcard rule under its own SAN")
	}
}

// TestTheIssuerRegimeIsReadFromTheSolverNotTheName is the self-test on the
// conditional the whole of memql#4347 turns on.
//
// Without it, dns01Issuers could return an empty map for every input -- every
// wildcard assertion would then read as "HTTP-01 regime" and the gates would
// pass while asserting the opposite of the truth. So both branches are shown
// reachable on a fixture, and neither is decided by the issuer's name.
func TestTheIssuerRegimeIsReadFromTheSolverNotTheName(t *testing.T) {
	const fixture = `
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: named-dns01-but-solves-http01
spec:
  acme:
    solvers:
      - http01:
          ingress:
            ingressClassName: nginx
---
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: named-prod-but-solves-dns01
spec:
  acme:
    solvers:
      - dns01:
          azureDNS:
            hostedZoneName: example.test
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: an-unrelated-document
data:
  acme: "not an issuer"
`
	got := dns01Issuers(t, fixture)
	if got["named-dns01-but-solves-http01"] {
		t.Error("an HTTP-01 issuer was read as DNS-01 because of its name; the solver is the property that matters")
	}
	if !got["named-prod-but-solves-dns01"] {
		t.Error("a DNS-01 issuer was not detected; every regime assertion in this file would then read as HTTP-01")
	}
	if len(got) != 1 {
		t.Errorf("dns01Issuers returned %v, want exactly the one dns01-solving issuer", sortedKeys(got))
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
//
// It is also why the memql#4347 reversal stays OUT of the generated file: the
// wildcard tls entry is a kustomize patch and the DNS-01 objects are their own
// resource, because a hand edit here is reverted by the next `make frontdoor`
// and a reviewer would have approved a change that then vanishes.
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
// There is no exception. Every host the render serves comes from the role set
// (docs/public/operate/front-door.md).
func TestRenderedHostsAreExactlyTheProduct(t *testing.T) {
	for _, overlay := range generatedOverlays {
		t.Run(overlay, func(t *testing.T) {
			rendered := render(t, overlay)

			want := map[string]bool{}
			for _, h := range frontdoor.Hosts(committedDomain) {
				want[h.Name] = true
			}
			for _, got := range sortedHosts(rendered) {
				if want[got] {
					continue
				}
				t.Errorf("the %s overlay serves %q, which is not one of the derived hosts -- an extra "+
					"host rule is a design change, not a configuration "+
					"change (docs/public/operate/front-door.md)", overlay, got)
			}
		})
	}
}

// acmeIssuersIn returns the names of every rendered Issuer / ClusterIssuer that
// places ACME orders, by reading `spec.acme` rather than by matching a name.
//
// Derived on purpose. A name list ("letsencrypt-*") would silently stop
// covering an ACME issuer somebody named differently -- and this gate's whole
// job is to catch a SAN that costs money, so failing OPEN on an unrecognised
// name is the one direction it must not fail in.
func acmeIssuersIn(t *testing.T, rendered string) map[string]bool {
	t.Helper()

	out := map[string]bool{}
	eachDocument(t, rendered, func(kind string, doc *yaml.Node) {
		if kind != "Issuer" && kind != "ClusterIssuer" {
			return
		}
		var i struct {
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
			Spec struct {
				ACME map[string]any `yaml:"acme"`
			} `yaml:"spec"`
		}
		if err := doc.Decode(&i); err != nil {
			t.Fatalf("decoding an %s: %v", kind, err)
		}
		if len(i.Spec.ACME) > 0 {
			out[i.Metadata.Name] = true
		}
	})
	return out
}
