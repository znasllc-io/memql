// internal_tls_test.go -- the memql-ca / identity-tls chain (memql#4484).
//
// WHAT IT PROTECTS. Ten workloads mount `memql-ca`; identity mounts
// `identity-tls`; every node dials https://identity:8085 and verifies against
// /etc/memql/cacerts/ca.crt. Nothing in base, components or either cloud
// overlay created either Secret -- locally they are minted with openssl, and on
// the retired cloud cluster they had been hand-seeded, which nobody noticed
// until a bring-up on a genuinely empty subscription had nothing to inherit.
//
// The failure is the reason this needs a gate rather than a note. A missing
// Secret named by a volume does not error: the pod sits in ContainerCreating
// FOREVER, with no log line, because the container never starts. Seven mesh
// Deployments hung together for seventeen minutes with nothing to read.
//
// THREE SEPARATE INVARIANTS LIVE HERE, and the last two are the ones a reader
// is most likely to undo:
//
//  1. Both cloud overlays render the chain -- now SIX objects, not four.
//  2. The anchor is not the issuer's signing Secret (memql#4599). `memql-ca`
//     is what ten pods mount and it must sit at least one tier ABOVE whatever
//     signs identity-tls. Collapsing the root and the intermediate back into
//     one Certificate is a one-line "simplification" that reads fine, renders
//     fine, deploys fine -- and reintroduces the 2026-08-25 outage the next
//     time that CA rotates, because rewriting the signer rewrites the anchor
//     under every running pod.
//  3. Every memql-ca mount projects ONLY ca.crt. cert-manager issues that
//     Secret as kubernetes.io/tls -- tls.crt, tls.key AND ca.crt -- and a
//     volume with no `items:` projects every key, which would place the
//     internal ROOT'S PRIVATE KEY in all ten pods with SSL_CERT_DIR scanning
//     it. The `items:` selector looks like noise and deleting it looks like a
//     simplification.
package overlays

import (
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// internalTLSOverlays are the overlays that compose components/internal-tls.
// `local` is deliberately absent: k3d gets the openssl chain from
// `make secrets`, and two mechanisms owning one Secret is a provenance
// question nobody can answer later.
var internalTLSOverlays = []string{"cloud", "cloud-entry"}

func TestCloudOverlaysRenderTheInternalTLSChain(t *testing.T) {
	// The four objects, and what each one is for. Listed rather than counted,
	// so a chain that renders three of four fails HERE rather than as pods in
	// ContainerCreating.
	want := map[string]string{
		"Issuer/memql-internal-selfsigned":  "the self-signed issuer the ROOT is minted from",
		"Certificate/memql-internal-ca":     "the root; its secretName IS the memql-ca anchor contract",
		"Issuer/memql-internal-ca":          "the issuer that signs the intermediate from the root Secret",
		"Certificate/memql-internal-issuer": "the intermediate; the tier allowed to rotate",
		"Issuer/memql-internal-issuer":      "the issuer that signs leaves from the INTERMEDIATE Secret",
		"Certificate/identity-tls":          "identity's serving certificate",
	}

	for _, overlay := range internalTLSOverlays {
		t.Run(overlay, func(t *testing.T) {
			rendered := render(t, overlay)

			saw := map[string]bool{}
			for _, r := range parse(t, rendered) {
				if r.Kind == "Issuer" || r.Kind == "Certificate" {
					saw[r.Kind+"/"+r.Metadata.Name] = true
				}
			}
			for key, why := range want {
				if !saw[key] {
					t.Errorf("%s does not render %s (%s). Without the full chain the Secrets are "+
						"never created, and a Deployment mounting an absent Secret stays in "+
						"ContainerCreating forever with no log line", overlay, key, why)
				}
			}

			// The two Secret names are the contract the Deployments name.
			for _, c := range certificatesIn(t, rendered) {
				switch c.Name {
				case "memql-internal-ca":
					// THE ROOT, AND IT MUST KEEP THE SECRET IT ALREADY OWNS.
					// Moving this Certificate onto a different Secret is what
					// deadlocks adoption: cert-manager stamps
					// cert-manager.io/certificate-name on the Secret, so a
					// second Certificate cannot claim memql-ca until this one
					// releases it (memql#4599, measured 2026-08-26).
					if c.SecretName != "memql-ca" {
						t.Errorf("%s: the ROOT Certificate writes %q, but ten Deployments mount "+
							"\"memql-ca\" -- and moving this Certificate off that Secret turns an "+
							"additive adoption into a trust-anchor replacement", overlay, c.SecretName)
					}
				case "memql-internal-issuer":
					if c.SecretName == "memql-ca" {
						t.Errorf("%s: the INTERMEDIATE writes \"memql-ca\", which is the anchor ten "+
							"Deployments mount. That is exactly the coupling memql#4599 removed: "+
							"this tier is meant to rotate, and rotating it would rewrite the trust "+
							"bundle under every running pod. It must write its own Secret", overlay)
					}
					if c.SecretName != "memql-ca-issuer" {
						t.Errorf("%s: the intermediate writes %q; Issuer/memql-internal-issuer reads "+
							"\"memql-ca-issuer\"", overlay, c.SecretName)
					}
					if c.IssuerName != "memql-internal-ca" {
						t.Errorf("%s: the intermediate is issued by %q rather than the root, so it "+
							"is not chained to the anchor the mesh trusts", overlay, c.IssuerName)
					}
				case "identity-tls":
					if c.SecretName != "identity-tls" {
						t.Errorf("%s: identity's Certificate writes %q, but identity mounts "+
							"\"identity-tls\"", overlay, c.SecretName)
					}
					if c.IssuerName != "memql-internal-issuer" {
						t.Errorf("%s: identity-tls is issued by %q rather than the INTERMEDIATE. "+
							"Issuing it from the root puts the anchor and the signer back on one "+
							"Secret, which is the memql#4599 coupling", overlay, c.IssuerName)
					}
				}
			}
		})
	}
}

// TestTheLocalOverlayDoesNotComposeTheInternalChain is the control. Without it
// the test above would pass just as happily if the component had been added to
// `base`, which would put cert-manager and `make secrets` in a race for one
// Secret on every k3d cluster.
func TestTheLocalOverlayDoesNotComposeTheInternalChain(t *testing.T) {
	rendered := render(t, "local")
	for _, r := range parse(t, rendered) {
		if (r.Kind == "Issuer" || r.Kind == "Certificate") &&
			strings.HasPrefix(r.Metadata.Name, "memql-internal") {
			t.Errorf("the local overlay renders %s/%s. Locally memql-ca and identity-tls are "+
				"minted by deploy/k8s/base/tls/gen-internal-ca.sh via `make secrets`, which SKIPS "+
				"when they already exist -- so composing the cert-manager chain here makes two "+
				"mechanisms race for one Secret", r.Kind, r.Metadata.Name)
		}
	}
}

// TestEveryMemqlCAMountProjectsOnlyTheCertificate is invariant 2, and it is
// checked against the RENDERED overlay rather than the base files so that a
// component or overlay patch reintroducing a bare mount is caught too.
func TestEveryMemqlCAMountProjectsOnlyTheCertificate(t *testing.T) {
	for _, overlay := range append([]string{"local"}, internalTLSOverlays...) {
		t.Run(overlay, func(t *testing.T) {
			rendered := render(t, overlay)

			checked := 0
			for _, doc := range strings.Split(rendered, "\n---\n") {
				if !strings.Contains(doc, "kind: Deployment") {
					continue
				}
				var d struct {
					Metadata struct {
						Name string `yaml:"name"`
					} `yaml:"metadata"`
					Spec struct {
						Template struct {
							Spec struct {
								Volumes []struct {
									Name   string `yaml:"name"`
									Secret struct {
										SecretName string `yaml:"secretName"`
										Items      []struct {
											Key  string `yaml:"key"`
											Path string `yaml:"path"`
										} `yaml:"items"`
									} `yaml:"secret"`
								} `yaml:"volumes"`
							} `yaml:"spec"`
						} `yaml:"template"`
					} `yaml:"spec"`
				}
				if err := yaml.Unmarshal([]byte(doc), &d); err != nil {
					continue
				}
				for _, v := range d.Spec.Template.Spec.Volumes {
					if v.Secret.SecretName != "memql-ca" {
						continue
					}
					checked++
					if len(v.Secret.Items) != 1 || v.Secret.Items[0].Key != "ca.crt" {
						t.Errorf("%s: Deployment %s mounts memql-ca without selecting ca.crt "+
							"(items=%v). cert-manager issues memql-ca as a kubernetes.io/tls "+
							"Secret carrying tls.crt, tls.key AND ca.crt, and a volume with no "+
							"items projects EVERY key -- so this places the internal CA's PRIVATE "+
							"KEY in this pod, where SSL_CERT_DIR also scans it. The selector is "+
							"not noise; restore it rather than simplifying it away",
							overlay, d.Metadata.Name, v.Secret.Items)
					}
					if v.Secret.Items[0].Path != "ca.crt" {
						t.Errorf("%s: Deployment %s projects ca.crt to path %q, but every node "+
							"reads MEMQL_HTTP_TLS_CA_FILE=/etc/memql/cacerts/ca.crt",
							overlay, d.Metadata.Name, v.Secret.Items[0].Path)
					}
				}
			}
			// Not a silent pass: a rename that made every mount invisible to
			// this walk would otherwise read as a clean bill of health.
			if checked == 0 {
				t.Fatalf("%s: found no Deployment mounting memql-ca. Either the mount was renamed "+
					"or this walk stopped matching -- and either way this gate is watching nothing",
					overlay)
			}
			t.Logf("%s: checked %d memql-ca mount(s)", overlay, checked)
		})
	}
}

// TestTheAnchorIsNotTheSigningSecret is invariant 2, and it is the gate that
// exists because of memql#4599 -- the 2026-08-25 outage in one assertion.
//
// THE COUPLING IT REFUSES. Until v0.20.4 one Secret was both the trust anchor
// every pod mounts AND the material Issuer/memql-internal-ca signs with. That
// means "rotate the signing CA" and "rewrite what every running pod trusts"
// were THE SAME OPERATION, and cert-manager offers no ordering between the two
// -- so adopting the component on a cluster with a hand-seeded CA minted a leaf
// from the outgoing CA one second before the anchor became the incoming one.
// Every node then rejected identity with `remote error: tls: bad certificate`,
// while staying Running, Ready and Healthy.
//
// WHY IT IS PHRASED AS A WALK RATHER THAN A NAME CHECK. Asserting
// `memql-internal-ca.secretName == "memql-ca-issuer"` only catches the rename.
// What actually matters is the SHAPE: whatever issues identity's leaf must not
// sign with the Secret the Deployments mount, however the objects are named. A
// future reader collapsing the tiers back together will pick new names, and
// this still fails.
func TestTheAnchorIsNotTheSigningSecret(t *testing.T) {
	// The anchor, read off the Deployments rather than hardcoded -- if the
	// mount ever moves, this gate moves with it instead of watching a Secret
	// nothing consumes.
	const anchor = "memql-ca"

	for _, overlay := range internalTLSOverlays {
		t.Run(overlay, func(t *testing.T) {
			rendered := render(t, overlay)

			// Which Secret does each namespaced CA Issuer sign with?
			signsWith := map[string]string{}
			eachDocument(t, rendered, func(kind string, doc *yaml.Node) {
				if kind != "Issuer" && kind != "ClusterIssuer" {
					return
				}
				var i struct {
					Metadata struct {
						Name string `yaml:"name"`
					} `yaml:"metadata"`
					Spec struct {
						CA struct {
							SecretName string `yaml:"secretName"`
						} `yaml:"ca"`
					} `yaml:"spec"`
				}
				if err := doc.Decode(&i); err != nil {
					t.Fatalf("decoding an Issuer: %v", err)
				}
				if i.Spec.CA.SecretName != "" {
					signsWith[i.Metadata.Name] = i.Spec.CA.SecretName
				}
			})

			leaf := certificateFor(t, rendered, "identity-tls")
			signer := signsWith[leaf.IssuerName]
			if signer == "" {
				t.Fatalf("%s: identity-tls names issuer %q, which is not a CA Issuer in this "+
					"render -- so this gate cannot see what signs the mesh's serving certificate",
					overlay, leaf.IssuerName)
			}
			if signer == anchor {
				t.Errorf("%s: identity-tls is signed by Issuer %q, which signs with %q -- the SAME "+
					"Secret the mesh mounts as its trust anchor.\n\n"+
					"That is the memql#4599 coupling: rotating this CA rewrites the bundle under "+
					"every running pod, they can no longer verify the leaf identity presents, and "+
					"the mesh returns 502 on sign-in while every pod stays Running and Ready and "+
					"ArgoCD reports Healthy.\n\n"+
					"The anchor must sit at least one tier above the signer: a long-lived root "+
					"writes %q and never rotates in normal operation, an intermediate under it "+
					"writes its own Secret and is the tier allowed to move.",
					overlay, leaf.IssuerName, signer, anchor)
			}
		})
	}
}

// TestTheChainLandsBeforeTheWorkloadsThatMountIt gates the ordering half of
// memql#4599, and the shape of that gate is itself a finding.
//
// THE WAVES EXIST FOR ONE REASON: the whole chain must land before the
// default-wave workloads that mount these Secrets. Everything else in the
// Application is wave 0, including the ten Deployments; if any of these were
// wave 0 too, a Deployment could be created alongside a Secret that does not
// exist yet and sit in ContainerCreating forever. Hence negative.
//
// THEY ARE NOT A DEPENDENCY ORDERING BETWEEN THE OBJECTS HERE, and the first
// attempt at this change assumed they were. Grading them -3 / -2 / -1 renders
// correctly, passes every other gate, and DEADLOCKS on adoption -- measured on
// a live cluster, 2026-08-26:
//
//	Certificate/memql-internal-root  False
//	  "Secret was issued for "memql-internal-ca". If this message is not
//	   transient, you might have two conflicting Certificates pointing to the
//	   same secret."
//
// ArgoCD will not start a wave until the previous one is Healthy, and the
// object that RELEASES the contested Secret was in the later wave. Circular,
// and invisible to every render-time check.
//
// So the invariant is EQUALITY, not order: cert-manager retries until the chain
// converges, and any grading here is a claim that ArgoCD should sequence what
// cert-manager already reconciles.
func TestTheChainLandsBeforeTheWorkloadsThatMountIt(t *testing.T) {
	for _, overlay := range internalTLSOverlays {
		t.Run(overlay, func(t *testing.T) {
			rendered := render(t, overlay)

			waves := map[string]int{}
			for _, r := range parse(t, rendered) {
				if r.Kind != "Issuer" && r.Kind != "Certificate" {
					continue
				}
				if !strings.HasPrefix(r.Metadata.Name, "memql-internal") && r.Metadata.Name != "identity-tls" {
					continue
				}
				key := r.Kind + "/" + r.Metadata.Name
				raw, ok := r.Metadata.Annotations["argocd.argoproj.io/sync-wave"]
				if !ok {
					t.Errorf("%s: %s carries no argocd.argoproj.io/sync-wave. Without one it lands "+
						"in wave 0 alongside the Deployments that mount it, and a Deployment can "+
						"then be created before the Secret it mounts exists", overlay, key)
					continue
				}
				n, err := strconv.Atoi(raw)
				if err != nil {
					t.Errorf("%s: %s has sync-wave %q, which is not an integer", overlay, key, raw)
					continue
				}
				if n >= 0 {
					t.Errorf("%s: %s is in wave %d. The chain must be NEGATIVE so it lands before "+
						"the default-wave workloads that mount these Secrets", overlay, key, n)
				}
				waves[key] = n
			}

			if len(waves) == 0 {
				t.Fatalf("%s: found no chain object carrying a sync-wave -- this gate is watching "+
					"nothing", overlay)
			}

			// EQUAL, not merely ordered. See the comment above: grading these
			// is what produced the adoption deadlock.
			var first string
			for k := range waves {
				if first == "" || k < first {
					first = k
				}
			}
			want := waves[first]
			for key, got := range waves {
				if got != want {
					t.Errorf("%s: %s is in wave %d but %s is in wave %d. The internal-tls chain must "+
						"be in ONE wave.\n\n"+
						"Grading these looks like a dependency ordering and is not one: cert-manager "+
						"retries until the chain converges, whereas ArgoCD refuses to start a wave "+
						"until the previous is Healthy. On adoption that is a DEADLOCK -- the "+
						"Certificate that releases the contested Secret sits in a later wave than "+
						"the one trying to claim it, and cert-manager reports \"Secret was issued "+
						"for ...\" forever (memql#4599).",
						overlay, key, got, first, want)
				}
			}
			t.Logf("%s: %d chain object(s), all in wave %d", overlay, len(waves), want)
		})
	}
}
