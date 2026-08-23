// The render gate on the engine's workload identity (memql#4336).
//
// WHY A RENDER TEST AND NOT A REVIEW. Every part of this shape fails SILENTLY
// when it is absent. A Deployment with no projected volume simply has no file
// at the path its env var names -- and until an operator federates that
// cluster, nothing reads the file, so nothing complains. The absence is
// therefore invisible for as long as it does not matter and fatal the moment
// it does: the cutover flips the ids on, the node preflights, and one node
// type that never got the volume crash-loops while every other one is fine.
// The same is true of the ServiceAccount (a pod silently runs as `default`,
// whose subject the federation rule does not match) and of the envFrom (the
// ids exist in the ConfigMap and reach nobody).
//
// So the gate asserts the whole shape on EVERY engine Deployment in BOTH
// cloud overlays, and asserts the local build leaves the ids empty -- which is
// what keeps `make up` booting on the API key with the same manifests.
package overlays

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	anthropicTokenVolume  = "anthropic-identity"
	anthropicTokenPath    = "/var/run/secrets/anthropic.com/token"
	anthropicMountPath    = "/var/run/secrets/anthropic.com"
	anthropicAudience     = "https://api.anthropic.com"
	anthropicTokenEnvName = "MEMQL_AI_ANTHROPIC_IDENTITY_TOKEN_FILE"
	federationConfigMap   = "memql-anthropic-federation"
	engineServiceAccount  = "memql-engine"

	// identityServiceAccount is the ONE documented exception, and it is not an
	// oversight. The identity node already runs as `memql-deploy`, which holds
	// the deploy console's Rollout + Application grants (memql#4257). Moving
	// identity onto memql-engine would either strip that RBAC or -- worse --
	// move it onto the account EVERY engine node runs as, handing the whole
	// mesh a privilege one node needs. So identity keeps its own account and
	// takes the rest of the shape: the volume, the mount and the env var.
	//
	// The consequence is real and belongs in the runbook rather than in a
	// comment nobody reads at 3am: identity's projected token carries the
	// subject `system:serviceaccount:memql:memql-deploy`, which the federation
	// rule's subject_prefix does NOT match. Identity does not call Anthropic
	// today. If it ever does, the rule needs a second prefix -- not a change
	// here.
	identityServiceAccount = "memql-deploy"
)

// engineDeployments is meshDeployments (render_cloud_test.go) -- the same
// closed list, for the same reason: the failure this catches is a node type
// arriving and the wiring not covering it, which a discovered list cannot see.

// workload is the slice of a rendered Deployment this gate reasons about.
type workload struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Template struct {
			Spec struct {
				ServiceAccountName string `yaml:"serviceAccountName"`
				Volumes            []struct {
					Name      string `yaml:"name"`
					Projected struct {
						Sources []struct {
							ServiceAccountToken struct {
								Audience          string `yaml:"audience"`
								ExpirationSeconds int    `yaml:"expirationSeconds"`
								Path              string `yaml:"path"`
							} `yaml:"serviceAccountToken"`
						} `yaml:"sources"`
					} `yaml:"projected"`
				} `yaml:"volumes"`
				Containers []struct {
					Name    string `yaml:"name"`
					EnvFrom []struct {
						ConfigMapRef struct {
							Name     string `yaml:"name"`
							Optional *bool  `yaml:"optional"`
						} `yaml:"configMapRef"`
					} `yaml:"envFrom"`
					Env []struct {
						Name  string `yaml:"name"`
						Value string `yaml:"value"`
					} `yaml:"env"`
					VolumeMounts []struct {
						Name      string `yaml:"name"`
						MountPath string `yaml:"mountPath"`
						ReadOnly  bool   `yaml:"readOnly"`
					} `yaml:"volumeMounts"`
				} `yaml:"containers"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

// configMapDoc is the rendered federation ConfigMap.
type configMapDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Data map[string]string `yaml:"data"`
}

func parseWorkloads(t *testing.T, rendered string) map[string]workload {
	t.Helper()
	out := map[string]workload{}
	dec := yaml.NewDecoder(strings.NewReader(rendered))
	for {
		var w workload
		if err := dec.Decode(&w); err != nil {
			break
		}
		if w.Kind == "Deployment" && w.Metadata.Name != "" {
			out[w.Metadata.Name] = w
		}
	}
	if len(out) == 0 {
		t.Fatal("the rendered overlay parsed to zero Deployments")
	}
	return out
}

func federationConfig(t *testing.T, rendered string) map[string]string {
	t.Helper()
	dec := yaml.NewDecoder(strings.NewReader(rendered))
	for {
		var c configMapDoc
		if err := dec.Decode(&c); err != nil {
			break
		}
		if c.Kind == "ConfigMap" && c.Metadata.Name == federationConfigMap {
			return c.Data
		}
	}
	t.Fatalf("the rendered overlay carries no %q ConfigMap", federationConfigMap)
	return nil
}

// TestEveryEngineDeploymentCarriesTheAnthropicIdentity is the shape gate.
func TestEveryEngineDeploymentCarriesTheAnthropicIdentity(t *testing.T) {
	for _, overlay := range []string{cloudOverlay, "cloud-entry", "local"} {
		t.Run(overlay, func(t *testing.T) {
			workloads := parseWorkloads(t, render(t, overlay))
			for _, name := range meshDeployments {
				w, ok := workloads[name]
				if !ok {
					t.Errorf("%s: the overlay renders no Deployment %q", overlay, name)
					continue
				}
				assertAnthropicIdentity(t, overlay, name, w)
			}
		})
	}
}

func assertAnthropicIdentity(t *testing.T, overlay, name string, w workload) {
	t.Helper()
	spec := w.Spec.Template.Spec

	wantSA := engineServiceAccount
	if name == "identity" {
		wantSA = identityServiceAccount
	}
	if spec.ServiceAccountName != wantSA {
		t.Errorf("%s/%s: serviceAccountName is %q, want %q -- a pod running as `default` presents a subject the federation rule does not match",
			overlay, name, spec.ServiceAccountName, wantSA)
	}

	var found bool
	for _, v := range spec.Volumes {
		if v.Name != anthropicTokenVolume {
			continue
		}
		found = true
		if len(v.Projected.Sources) != 1 {
			t.Errorf("%s/%s: the %s volume has %d projected sources, want exactly 1",
				overlay, name, anthropicTokenVolume, len(v.Projected.Sources))
			break
		}
		src := v.Projected.Sources[0].ServiceAccountToken
		if src.Audience != anthropicAudience {
			t.Errorf("%s/%s: projected token audience is %q, want %q -- an audience mismatch is the most common federation denial and it is invisible until the exchange",
				overlay, name, src.Audience, anthropicAudience)
		}
		if src.ExpirationSeconds != 3600 {
			t.Errorf("%s/%s: projected token expirationSeconds is %d, want 3600 (the rule's token_lifetime_seconds)",
				overlay, name, src.ExpirationSeconds)
		}
		if src.Path != "token" {
			t.Errorf("%s/%s: projected token path is %q, want \"token\"", overlay, name, src.Path)
		}
	}
	if !found {
		t.Errorf("%s/%s: no %q projected volume -- %s would name a file that does not exist",
			overlay, name, anthropicTokenVolume, anthropicTokenEnvName)
	}

	// Exactly one container per engine Deployment today; assert on all of them
	// anyway so a sidecar arriving does not quietly skip the check.
	for _, c := range spec.Containers {
		var mounted bool
		for _, m := range c.VolumeMounts {
			if m.Name != anthropicTokenVolume {
				continue
			}
			mounted = true
			if m.MountPath != anthropicMountPath {
				t.Errorf("%s/%s/%s: the identity token mounts at %q, want %q",
					overlay, name, c.Name, m.MountPath, anthropicMountPath)
			}
			if !m.ReadOnly {
				t.Errorf("%s/%s/%s: the identity token mount is writable; it must be readOnly",
					overlay, name, c.Name)
			}
		}
		if !mounted {
			t.Errorf("%s/%s/%s: the %q volume is declared but not mounted",
				overlay, name, c.Name, anthropicTokenVolume)
		}

		var envSeen bool
		for _, e := range c.Env {
			if e.Name != anthropicTokenEnvName {
				continue
			}
			envSeen = true
			if e.Value != anthropicTokenPath {
				t.Errorf("%s/%s/%s: %s is %q, want %q",
					overlay, name, c.Name, anthropicTokenEnvName, e.Value, anthropicTokenPath)
			}
		}
		if !envSeen {
			t.Errorf("%s/%s/%s: %s is not set, so the SDK would never read the projected token",
				overlay, name, c.Name, anthropicTokenEnvName)
		}

		var envFromSeen bool
		for _, ef := range c.EnvFrom {
			if ef.ConfigMapRef.Name != federationConfigMap {
				continue
			}
			envFromSeen = true
			// optional: true is what lets a cluster that never applied the
			// ConfigMap boot at all -- without it the pod stays in
			// CreateContainerConfigError forever.
			if ef.ConfigMapRef.Optional == nil || !*ef.ConfigMapRef.Optional {
				t.Errorf("%s/%s/%s: the %s configMapRef is not optional; a cluster without that ConfigMap would fail to start containers",
					overlay, name, c.Name, federationConfigMap)
			}
		}
		if !envFromSeen {
			t.Errorf("%s/%s/%s: does not envFrom %s, so the four federation ids reach nobody",
				overlay, name, c.Name, federationConfigMap)
		}
	}
}

// TestTheCloudOverlaysCarryFederationPlaceholders holds the ids' shape in the
// overlays an operator fills in.
//
// Placeholders rather than real ids, deliberately: no file under deploy/ names
// a real rule, organization or account (the same rule the front-door
// generator's hosts follow). The runbook is where the operator learns which
// Console page produces each one.
func TestTheCloudOverlaysCarryFederationPlaceholders(t *testing.T) {
	for _, overlay := range []string{cloudOverlay, "cloud-entry"} {
		t.Run(overlay, func(t *testing.T) {
			data := federationConfig(t, render(t, overlay))
			for _, key := range []string{
				"MEMQL_AI_ANTHROPIC_FEDERATION_RULE_ID",
				"MEMQL_AI_ANTHROPIC_ORGANIZATION_ID",
				"MEMQL_AI_ANTHROPIC_SERVICE_ACCOUNT_ID",
			} {
				v, ok := data[key]
				if !ok {
					t.Errorf("%s: the federation ConfigMap has no %s", overlay, key)
					continue
				}
				if !strings.HasPrefix(v, "REPLACE-WITH-") {
					t.Errorf("%s: %s is %q -- the committed overlay must carry a placeholder, never a real id",
						overlay, key, v)
				}
			}
			// The workspace id stays empty: Anthropic needs it only when the
			// rule spans more than one workspace, and a placeholder here would
			// be a value the engine passes verbatim into the exchange.
			if v := data["MEMQL_AI_ANTHROPIC_WORKSPACE_ID"]; v != "" {
				t.Errorf("%s: MEMQL_AI_ANTHROPIC_WORKSPACE_ID is %q, want empty", overlay, v)
			}
		})
	}
}

// TestTheLocalOverlayLeavesFederationEmpty is the parity assertion: the local
// cluster runs the same manifests and keeps using the API key, because empty
// ids mean "not federating" rather than "half-configured".
func TestTheLocalOverlayLeavesFederationEmpty(t *testing.T) {
	data := federationConfig(t, render(t, "local"))
	for key, value := range data {
		if value != "" {
			t.Errorf("the local overlay sets %s=%q; local federation is not a reproducible path (k3d's OIDC issuer is not publicly reachable) and a non-empty value here would refuse boot",
				key, value)
		}
	}
	if len(data) != 4 {
		t.Errorf("the local federation ConfigMap carries %d keys, want the same 4 the cloud fills", len(data))
	}
}
