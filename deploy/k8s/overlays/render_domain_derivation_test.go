// Render gates on domain derivation, for every instance overlay (memql#4281).
//
// THE FAILURE THESE CATCH, in the exact shape it shipped. A deployment states
// its domain once, as the single MEMQL_DOMAIN key of the `memql-domain`
// ConfigMap, and component/envregistry/domain.go derives six domain-shaped
// identity values from it at boot. Two things have to be true for that to
// happen, and only one of them is visible in a manifest review:
//
//   - the ConfigMap has to be MOUNTED (an envFrom append), and
//   - the base's pinned placeholders have to be DELETED, because in Kubernetes
//     an explicit `env` entry beats `envFrom` regardless of order, and because
//     the derivation is set-if-absent by design ("an explicitly configured
//     value is a statement of intent and wins").
//
// overlays/cloud-entry did the first and not the second. It rendered eleven
// workloads pinned to https://identity.example.com, so the install booted,
// formed a mesh, and rejected every token -- with the base manifest, the
// overlay and the ConfigMap all looking correct, because each of them was.
// overlays/cloud did neither and was one Application patch away from the same
// thing. overlays/local did both, and recorded why: memql#3600, the first
// install after memql#3593 produced a magic link at a domain nobody owns, so no
// owner account was ever created and the cluster could not be signed into.
//
// WHY THE GATE IS PER-VARIABLE AND NOT A GREP FOR example.com. Two of the six
// (MEMQL_IDENTITY_BOOTSTRAP_DOMAIN, MEMQL_IDENTITY_REGISTERED_CLIENTS) would
// still be wrong if an overlay pinned them to a REAL domain -- pinning defeats
// the derivation whatever the value is, which is the whole point of
// set-if-absent. And a grep would fire on every unrelated fail-closed
// placeholder an overlay carries, which is a different thing: an address that
// is not derived from MEMQL_DOMAIN is not this gate's business. Naming the six
// variables says what is actually forbidden.
package overlays

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// instanceOverlays are the overlays an operator reconciles a cluster from.
// Listed rather than discovered, for the reason the mesh list above is listed:
// the failure this catches is an overlay arriving without the wiring, and a
// discovered list would grow with the tree and assert nothing. Keep in step
// with cmd/frontdoorhosts's own instanceOverlays set (`cloud`, `cloud-entry`)
// plus `local`, which is the same shape run in k3d.
var instanceOverlays = []string{"cloud", "cloud-entry", "local"}

// derivedEnvVars are the six values component/envregistry/domain.go computes
// from MEMQL_DOMAIN. An instance overlay may not pin ANY of them: a pin is a
// statement of intent that the derivation obeys, so a pinned value is the
// derivation not running.
//
// MEMQL_MCP_PUBLIC_URL is derived too but is pinned nowhere in deploy/, so it
// has never been part of this failure. It is listed anyway: if a future base
// pins it, that pin is the same defect and should fail here rather than ship.
var derivedEnvVars = []string{
	"MEMQL_IDENTITY_BASE_URL",
	"MEMQL_IDENTITY_VERIFIER_EXPECTED_ISSUER",
	"MEMQL_IDENTITY_BOOTSTRAP_DOMAIN",
	"MEMQL_DISCOVERY_GRPC_ENDPOINT",
	"MEMQL_IDENTITY_CORS_ALLOWED_ORIGINS",
	"MEMQL_IDENTITY_REGISTERED_CLIENTS",
	"MEMQL_MCP_PUBLIC_URL",
}

// domainConfigMap is the one ConfigMap an instance overlay mounts to state its
// domain. Its single key is MEMQL_DOMAIN.
const domainConfigMap = "memql-domain"

// bootstrapSecret is the envFrom entry the append-vs-replace hazard would
// destroy. A strategic-merge patch on `envFrom` REPLACES the list (it has no
// patch merge key), which would silently take the master key, the operator key
// and the database DSN off every node it touched. Asserting it survives is how
// this gate notices somebody "simplifying" the JSON 6902 append.
const bootstrapSecret = "memql-secrets"

// podResource is the slice of a rendered Deployment these gates reason about.
// The shared `resource` type in render_cloud_test.go stops at spec.replicas.
type podResource struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Template struct {
			Spec struct {
				Containers []struct {
					Name string `yaml:"name"`
					Env  []struct {
						Name  string `yaml:"name"`
						Value string `yaml:"value"`
					} `yaml:"env"`
					EnvFrom []struct {
						ConfigMapRef struct {
							Name string `yaml:"name"`
						} `yaml:"configMapRef"`
						SecretRef struct {
							Name string `yaml:"name"`
						} `yaml:"secretRef"`
					} `yaml:"envFrom"`
				} `yaml:"containers"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

// parseDeployments decodes every Deployment out of a rendered overlay.
//
// A mid-stream decode error is FATAL rather than a break, for the reason
// parse() gives: stopping quietly at the first bad document leaves every
// assertion below reasoning about a truncated prefix, and they would all pass
// having checked a fraction of the overlay.
func parseDeployments(t *testing.T, rendered string) map[string]podResource {
	t.Helper()
	dec := yaml.NewDecoder(strings.NewReader(rendered))
	out := map[string]podResource{}
	for i := 0; ; i++ {
		var r podResource
		err := dec.Decode(&r)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decoding document %d of the rendered overlay: %v", i+1, err)
		}
		if r.Kind != "Deployment" {
			continue
		}
		out[r.Metadata.Name] = r
	}
	return out
}

// TestInstanceOverlaysPinNoDerivedDomainValue is the gate on the half that was
// missing: the deletes.
func TestInstanceOverlaysPinNoDerivedDomainValue(t *testing.T) {
	for _, overlay := range instanceOverlays {
		t.Run(overlay, func(t *testing.T) {
			deployments := parseDeployments(t, render(t, overlay))

			// COVERAGE FLOOR. A walker that examined nothing must not be able
			// to report a pass: without this, deleting every Deployment from
			// the base would turn this gate green.
			if len(deployments) < len(meshDeployments) {
				t.Fatalf("rendered only %d Deployments, want at least %d (%v) -- "+
					"this gate cannot pass on an overlay it did not read",
					len(deployments), len(meshDeployments), meshDeployments)
			}

			forbidden := map[string]bool{}
			for _, name := range derivedEnvVars {
				forbidden[name] = true
			}
			for _, node := range meshDeployments {
				d, ok := deployments[node]
				if !ok {
					t.Errorf("%s does not render", node)
					continue
				}
				for _, c := range d.Spec.Template.Spec.Containers {
					for _, e := range c.Env {
						if forbidden[e.Name] {
							t.Errorf("%s/%s pins %s=%q. It is derived from MEMQL_DOMAIN "+
								"set-if-absent, so a pin -- placeholder or real -- means the "+
								"derivation never runs and this node serves whatever is written "+
								"here. Take ../../components/domain-derive instead of patching "+
								"it by hand (memql#4281).",
								node, c.Name, e.Name, e.Value)
						}
					}
				}
			}
		})
	}
}

// TestInstanceOverlaysMountTheDomainConfigMap is the gate on the other half.
//
// Both halves are asserted separately and deliberately: having only the deletes
// is a node with no domain at all, which fails loudly at boot, while having only
// the mount is the silent case that shipped. Neither gate alone would have
// caught cloud-entry.
func TestInstanceOverlaysMountTheDomainConfigMap(t *testing.T) {
	for _, overlay := range instanceOverlays {
		t.Run(overlay, func(t *testing.T) {
			deployments := parseDeployments(t, render(t, overlay))
			if len(deployments) < len(meshDeployments) {
				t.Fatalf("rendered only %d Deployments, want at least %d -- coverage floor",
					len(deployments), len(meshDeployments))
			}

			var mounted int
			for _, node := range meshDeployments {
				d, ok := deployments[node]
				if !ok {
					t.Errorf("%s does not render", node)
					continue
				}
				var sawDomain, sawSecrets bool
				for _, c := range d.Spec.Template.Spec.Containers {
					for _, ef := range c.EnvFrom {
						if ef.ConfigMapRef.Name == domainConfigMap {
							sawDomain = true
						}
						if ef.SecretRef.Name == bootstrapSecret {
							sawSecrets = true
						}
					}
				}
				if !sawDomain {
					t.Errorf("%s does not mount the %q ConfigMap, so MEMQL_DOMAIN never "+
						"reaches it and every value derived from it is empty (memql#4281)",
						node, domainConfigMap)
					continue
				}
				mounted++
				if !sawSecrets {
					t.Errorf("%s mounts %q but no longer mounts the %q Secret. envFrom has no "+
						"patch merge key, so a strategic merge REPLACES the list -- this is what "+
						"that looks like, and it takes the master key, the operator key and the "+
						"database DSN with it. The append must stay JSON 6902.",
						node, domainConfigMap, bootstrapSecret)
				}
			}
			if mounted != len(meshDeployments) {
				t.Errorf("%d of %d mesh nodes mount %q", mounted, len(meshDeployments), domainConfigMap)
			}
		})
	}
}

// TestDomainDeriveComponentNamesNoDomain holds the rule the whole mechanism
// exists to keep: no file under deploy/ names a domain (memql#3593). The
// component is the one place that could plausibly acquire one -- it is where
// the domain-shaped values are discussed -- so it is the one place worth
// gating directly.
func TestDomainDeriveComponentNamesNoDomain(t *testing.T) {
	const dir = "../components/domain-derive"
	files := []string{dir + "/kustomization.yaml", dir + "/patches/envfrom.yaml"}
	var scanned int
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("reading %s: %v -- the component is the fix for memql#4281 and "+
				"this gate cannot pass without reading it", f, err)
		}
		scanned++
		// Read whole-file rather than line-by-line: bufio.Scanner refuses this
		// repository (component/architecture/embedded/topology.model.json is
		// 40 MB on one line), and a helper that works everywhere is worth more
		// than one that happens to work here.
		for _, ln := range strings.Split(string(b), "\n") {
			code := ln
			if i := strings.Index(ln, "#"); i >= 0 {
				code = ln[:i] // a comment may DISCUSS example.com; a value may not be one
			}
			if strings.Contains(code, "example.com") || strings.Contains(code, ".localhost") {
				t.Errorf("%s names a domain outside a comment: %q. The component states the "+
					"RELATIONSHIP (MEMQL_DOMAIN in, six values out) and never the value.", f, strings.TrimSpace(ln))
			}
		}
	}
	if scanned != len(files) {
		t.Fatalf("scanned %d files, want %d -- coverage floor", scanned, len(files))
	}
}
