// The render gate on the key that unseals a person's API key (design record
// docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md,
// mesh rule M2).
//
// A person's API key is sealed under MEMQL_MASTER_KEY on the node that received
// it, and unsealed on the node that makes the call it pays for. Those are
// rarely the same node: the key is added through the bff a browser is
// connected to, and the call is made by an agent, a planner, a workbench or
// the MCP node. So EVERY engine node needs the master key, and one that lacks
// it fails in a way nothing reports until somebody's call lands there: the
// account is passed over as "could not be unsealed on this node" and the call
// moves to another account or refuses, on that replica only.
//
// The key reaches a pod as a key of the `memql-secrets` Secret, which every
// engine Deployment takes with envFrom. The Secret is created out of band and
// never rendered, so what a render can assert is the half that drifts: that
// each Deployment still takes the Secret.
package overlays

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const engineSecretName = "memql-secrets"

type secretWorkload struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Template struct {
			Spec struct {
				Containers []struct {
					Name    string `yaml:"name"`
					EnvFrom []struct {
						SecretRef struct {
							Name     string `yaml:"name"`
							Optional *bool  `yaml:"optional"`
						} `yaml:"secretRef"`
					} `yaml:"envFrom"`
				} `yaml:"containers"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

func TestEveryNodeThatCallsAVendorHoldsTheMasterKey(t *testing.T) {
	for _, overlay := range []string{cloudOverlay, "cloud-entry", "local"} {
		t.Run(overlay, func(t *testing.T) {
			workloads := map[string]secretWorkload{}
			dec := yaml.NewDecoder(strings.NewReader(render(t, overlay)))
			for {
				var w secretWorkload
				if err := dec.Decode(&w); err != nil {
					break
				}
				if w.Kind == "Deployment" && w.Metadata.Name != "" {
					workloads[w.Metadata.Name] = w
				}
			}
			// meshDeployments is the closed list of engine node types
			// (render_cloud_test.go). The failure this catches is a node type
			// arriving, or one being edited, without the Secret.
			for _, name := range meshDeployments {
				w, ok := workloads[name]
				if !ok {
					t.Errorf("%s: the overlay renders no Deployment %q", overlay, name)
					continue
				}
				if len(w.Spec.Template.Spec.Containers) == 0 {
					t.Errorf("%s/%s: no containers", overlay, name)
					continue
				}
				found := false
				for _, source := range w.Spec.Template.Spec.Containers[0].EnvFrom {
					if source.SecretRef.Name != engineSecretName {
						continue
					}
					found = true
					if source.SecretRef.Optional != nil && *source.SecretRef.Optional {
						t.Errorf("%s/%s: %s is optional. A node that starts without it holds no master key, "+
							"and a person's API key cannot be unsealed for a call that lands there.", overlay, name, engineSecretName)
					}
				}
				if !found {
					t.Errorf("%s/%s: the engine container does not take %s with envFrom, so it has no MEMQL_MASTER_KEY. "+
						"A vendor call paid by a person's API key is made on whichever node runs it, and this one could not unseal the key.",
						overlay, name, engineSecretName)
				}
			}
		})
	}
}
