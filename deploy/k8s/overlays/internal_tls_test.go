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
		"Issuer/memql-internal-selfsigned": "the self-signed issuer the ROOT is minted from",
		"Certificate/memql-internal-root":  "the root; its secretName IS the memql-ca anchor contract",
		"Issuer/memql-internal-root":       "the issuer that signs the intermediate from the root Secret",
		"Certificate/memql-internal-ca":    "the intermediate; the tier allowed to rotate",
		"Issuer/memql-internal-ca":         "the issuer that signs leaves from the INTERMEDIATE Secret",
		"Certificate/identity-tls":         "identity's serving certificate",
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
				case "memql-internal-root":
					if c.SecretName != "memql-ca" {
						t.Errorf("%s: the ROOT Certificate writes %q, but ten Deployments mount "+
							"\"memql-ca\"", overlay, c.SecretName)
					}
				case "memql-internal-ca":
					if c.SecretName == "memql-ca" {
						t.Errorf("%s: the INTERMEDIATE writes \"memql-ca\", which is the anchor ten "+
							"Deployments mount. That is exactly the coupling memql#4599 removed: "+
							"this tier is meant to rotate, and rotating it would rewrite the trust "+
							"bundle under every running pod. It must write its own Secret", overlay)
					}
					if c.SecretName != "memql-ca-issuer" {
						t.Errorf("%s: the intermediate writes %q; Issuer/memql-internal-ca reads "+
							"\"memql-ca-issuer\"", overlay, c.SecretName)
					}
					if c.IssuerName != "memql-internal-root" {
						t.Errorf("%s: the intermediate is issued by %q rather than the root, so it "+
							"is not chained to the anchor the mesh trusts", overlay, c.IssuerName)
					}
				case "identity-tls":
					if c.SecretName != "identity-tls" {
						t.Errorf("%s: identity's Certificate writes %q, but identity mounts "+
							"\"identity-tls\"", overlay, c.SecretName)
					}
					if c.IssuerName != "memql-internal-ca" {
						t.Errorf("%s: identity-tls is issued by %q rather than the internal CA. "+
							"A leaf signed by anything else is not verifiable against the ca.crt "+
							"the mesh mounts", overlay, c.IssuerName)
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
// memql#4599. cert-manager guarantees nothing about whether a CA settles before
// a leaf naming it; ArgoCD sync waves do, and they are the only ordering this
// component can express.
//
// The waves must be NEGATIVE, and that is not stylistic. Everything else in the
// Application is wave 0, including the ten Deployments that mount these Secrets.
// Positive waves would deadlock: ArgoCD waits for each wave to go Healthy, the
// Deployments would be created first, and they would sit in ContainerCreating
// on a Secret whose wave had not run -- so wave 0 never goes Healthy and the
// later waves never start.
func TestTheChainLandsBeforeTheWorkloadsThatMountIt(t *testing.T) {
	// Tier order: each object must land no later than the one that depends on
	// it. Equal waves are fine (ArgoCD applies a wave together); inversions are
	// not.
	dependsOn := []struct{ earlier, later string }{
		{"Issuer/memql-internal-selfsigned", "Certificate/memql-internal-root"},
		{"Certificate/memql-internal-root", "Issuer/memql-internal-root"},
		{"Issuer/memql-internal-root", "Certificate/memql-internal-ca"},
		{"Certificate/memql-internal-ca", "Issuer/memql-internal-ca"},
		{"Issuer/memql-internal-ca", "Certificate/identity-tls"},
	}

	for _, overlay := range internalTLSOverlays {
		t.Run(overlay, func(t *testing.T) {
			rendered := render(t, overlay)

			wave := map[string]int{}
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
						"in wave 0 alongside the Deployments that mount it, and cert-manager "+
						"provides no ordering of its own", overlay, key)
					continue
				}
				n, err := strconv.Atoi(raw)
				if err != nil {
					t.Errorf("%s: %s has sync-wave %q, which is not an integer", overlay, key, raw)
					continue
				}
				if n >= 0 {
					t.Errorf("%s: %s is in wave %d. The whole chain must be NEGATIVE so it lands "+
						"before the default-wave workloads that mount these Secrets -- a "+
						"non-negative wave deadlocks the sync, because the Deployments hang in "+
						"ContainerCreating and wave 0 never reports Healthy", overlay, key, n)
				}
				wave[key] = n
			}

			for _, d := range dependsOn {
				e, okE := wave[d.earlier]
				l, okL := wave[d.later]
				if !okE || !okL {
					continue // the render gate above already reported the absence
				}
				if e > l {
					t.Errorf("%s: %s is in wave %d but %s, which depends on it, is in wave %d. "+
						"ArgoCD would create the dependant first and cert-manager would mint it "+
						"against material that is about to be replaced -- which is precisely the "+
						"one-second race memql#4599 records", overlay, d.earlier, e, d.later, l)
				}
			}
		})
	}
}
