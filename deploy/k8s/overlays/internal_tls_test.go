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
// TWO SEPARATE INVARIANTS LIVE HERE, and the second is the one a reader is
// most likely to undo:
//
//  1. Both cloud overlays render the chain.
//  2. Every memql-ca mount projects ONLY ca.crt. cert-manager issues that
//     Secret as kubernetes.io/tls -- tls.crt, tls.key AND ca.crt -- and a
//     volume with no `items:` projects every key, which would place the
//     internal CA's PRIVATE KEY in all ten pods with SSL_CERT_DIR scanning it.
//     The `items:` selector looks like noise and deleting it looks like a
//     simplification.
package overlays

import (
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
		"Issuer/memql-internal-selfsigned": "the self-signed root the CA certificate is issued from",
		"Certificate/memql-internal-ca":    "the CA itself; its secretName IS the memql-ca contract",
		"Issuer/memql-internal-ca":         "the issuer that signs leaves from the CA Secret",
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
				case "memql-internal-ca":
					if c.SecretName != "memql-ca" {
						t.Errorf("%s: the CA Certificate writes %q, but ten Deployments mount "+
							"\"memql-ca\"", overlay, c.SecretName)
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
