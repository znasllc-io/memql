// The render gate on the engine's SECOND vendor door (epic memql#5088).
//
// WHY A SECOND FILE AND NOT A PARAMETER. The Anthropic gate one file over
// (render_anthropic_federation_test.go) says why a render test exists at all:
// every part of this shape fails SILENTLY when it is absent, because until an
// operator federates the cluster nothing reads the projected token and nothing
// complains. That argument is unchanged here. What is different is the thing
// being asserted -- a DIFFERENT audience, a DIFFERENT mount, a DIFFERENT
// ConfigMap -- and folding the two into one table-driven gate would make the
// failure message name a row rather than a vendor.
//
// The one thing the two gates deliberately do NOT duplicate is the
// ServiceAccount. There is one workload identity, `memql-engine`, and the
// Anthropic gate already asserts it on every engine Deployment; asserting it
// again here would spend a second failure on the same fact and imply, wrongly,
// that a second account exists.
//
// A projected service-account token is minted for ONE audience, which is why
// there are two volumes rather than one shared file: OpenAI's audience is
// `https://api.openai.com/v1` and it refuses a token minted for anybody else.
package overlays

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	openaiTokenVolume  = "openai-identity"
	openaiTokenPath    = "/var/run/secrets/openai.com/token"
	openaiMountPath    = "/var/run/secrets/openai.com"
	openaiAudience     = "https://api.openai.com/v1"
	openaiTokenEnvName = "MEMQL_AI_OPENAI_IDENTITY_TOKEN_FILE"
	openaiConfigMap    = "memql-openai-federation"
)

// openaiFederationConfig pulls the rendered OpenAI federation ConfigMap out of
// an overlay. It is a sibling of federationConfig rather than a generalisation
// of it: the failure it reports names the vendor whose door is missing, which a
// shared helper taking a name would have to be asked for.
func openaiFederationConfig(t *testing.T, rendered string) map[string]string {
	t.Helper()
	dec := yaml.NewDecoder(strings.NewReader(rendered))
	for {
		var c configMapDoc
		if err := dec.Decode(&c); err != nil {
			break
		}
		if c.Kind == "ConfigMap" && c.Metadata.Name == openaiConfigMap {
			return c.Data
		}
	}
	t.Fatalf("the rendered overlay carries no %q ConfigMap", openaiConfigMap)
	return nil
}

// TestEveryEngineDeploymentCarriesTheOpenAIIdentity is the shape gate.
func TestEveryEngineDeploymentCarriesTheOpenAIIdentity(t *testing.T) {
	for _, overlay := range []string{cloudOverlay, "cloud-entry", "local"} {
		t.Run(overlay, func(t *testing.T) {
			workloads := parseWorkloads(t, render(t, overlay))
			for _, name := range meshDeployments {
				w, ok := workloads[name]
				if !ok {
					t.Errorf("%s: the overlay renders no Deployment %q", overlay, name)
					continue
				}
				assertOpenAIIdentity(t, overlay, name, w)
			}
		})
	}
}

func assertOpenAIIdentity(t *testing.T, overlay, name string, w workload) {
	t.Helper()
	spec := w.Spec.Template.Spec

	var found bool
	for _, v := range spec.Volumes {
		if v.Name != openaiTokenVolume {
			continue
		}
		found = true
		if len(v.Projected.Sources) != 1 {
			t.Errorf("%s/%s: the %s volume has %d projected sources, want exactly 1",
				overlay, name, openaiTokenVolume, len(v.Projected.Sources))
			break
		}
		src := v.Projected.Sources[0].ServiceAccountToken
		if src.Audience != openaiAudience {
			t.Errorf("%s/%s: projected token audience is %q, want %q -- an audience mismatch is the most common federation denial and it is invisible until the exchange",
				overlay, name, src.Audience, openaiAudience)
		}
		// 3600, AND DELIBERATELY NOT THE 600 ANTHROPIC'S TOKEN NOW HAS (design
		// record 2026-11-21-vendor-accounts-and-api-keys, A3). An OpenAI bearer
		// never outlives the identity token it was minted from, so a 600 second
		// token would force a sign-in every 8 minutes with about 90 seconds
		// between kubelet's rotation and the bearer's expiry. OpenAI documents
		// no rule against presenting a token twice, so the restart case that
		// motivates 600 for Anthropic does not arise here.
		if src.ExpirationSeconds != 3600 {
			t.Errorf("%s/%s: projected token expirationSeconds is %d, want 3600 (the ceiling OpenAI's exchange honours)",
				overlay, name, src.ExpirationSeconds)
		}
		if src.Path != "token" {
			t.Errorf("%s/%s: projected token path is %q, want \"token\"", overlay, name, src.Path)
		}
	}
	if !found {
		t.Errorf("%s/%s: no %q projected volume -- %s would name a file that does not exist",
			overlay, name, openaiTokenVolume, openaiTokenEnvName)
	}

	// Exactly one container per engine Deployment today; assert on all of them
	// anyway so a sidecar arriving does not quietly skip the check.
	for _, c := range spec.Containers {
		var mounted bool
		for _, m := range c.VolumeMounts {
			if m.Name != openaiTokenVolume {
				continue
			}
			mounted = true
			if m.MountPath != openaiMountPath {
				t.Errorf("%s/%s/%s: the identity token mounts at %q, want %q",
					overlay, name, c.Name, m.MountPath, openaiMountPath)
			}
			if !m.ReadOnly {
				t.Errorf("%s/%s/%s: the identity token mount is writable; it must be readOnly",
					overlay, name, c.Name)
			}
		}
		if !mounted {
			t.Errorf("%s/%s/%s: the %q volume is declared but not mounted",
				overlay, name, c.Name, openaiTokenVolume)
		}

		var envSeen bool
		for _, e := range c.Env {
			if e.Name != openaiTokenEnvName {
				continue
			}
			envSeen = true
			if e.Value != openaiTokenPath {
				t.Errorf("%s/%s/%s: %s is %q, want %q",
					overlay, name, c.Name, openaiTokenEnvName, e.Value, openaiTokenPath)
			}
		}
		if !envSeen {
			t.Errorf("%s/%s/%s: %s is not set, so the exchanger would never read the projected token",
				overlay, name, c.Name, openaiTokenEnvName)
		}

		var envFromSeen bool
		for _, ef := range c.EnvFrom {
			if ef.ConfigMapRef.Name != openaiConfigMap {
				continue
			}
			envFromSeen = true
			// optional: true is what lets a cluster that never applied the
			// ConfigMap boot at all -- without it the pod stays in
			// CreateContainerConfigError forever.
			if ef.ConfigMapRef.Optional == nil || !*ef.ConfigMapRef.Optional {
				t.Errorf("%s/%s/%s: the %s configMapRef is not optional; a cluster without that ConfigMap would fail to start containers",
					overlay, name, c.Name, openaiConfigMap)
			}
		}
		if !envFromSeen {
			t.Errorf("%s/%s/%s: does not envFrom %s, so the two federation ids reach nobody",
				overlay, name, c.Name, openaiConfigMap)
		}
	}
}

// TestTheProjectedIdentitiesDoNotShareAnAudience is the assertion that the two
// vendor doors are genuinely two.
//
// It exists because the cheapest wrong implementation of this epic is to mount
// the Anthropic token at a second path and call it OpenAI's. That renders,
// deploys, passes every shape check taken one vendor at a time, and fails only
// at the first exchange -- an hour into a cutover, with an error naming an
// audience nobody set deliberately.
func TestTheProjectedIdentitiesDoNotShareAnAudience(t *testing.T) {
	if anthropicAudience == openaiAudience {
		t.Fatalf("the two vendor audiences are the same string (%q); one projected token cannot serve both", openaiAudience)
	}
	for _, overlay := range []string{cloudOverlay, "cloud-entry", "local"} {
		t.Run(overlay, func(t *testing.T) {
			workloads := parseWorkloads(t, render(t, overlay))
			for _, name := range meshDeployments {
				w, ok := workloads[name]
				if !ok {
					continue
				}
				seen := map[string]string{}
				for _, v := range w.Spec.Template.Spec.Volumes {
					if len(v.Projected.Sources) != 1 {
						continue
					}
					aud := v.Projected.Sources[0].ServiceAccountToken.Audience
					if aud == "" {
						continue
					}
					if other, dup := seen[aud]; dup {
						t.Errorf("%s/%s: volumes %q and %q both project the audience %q -- two vendor doors need two audiences",
							overlay, name, other, v.Name, aud)
					}
					seen[aud] = v.Name
				}
				if seen[anthropicAudience] == "" || seen[openaiAudience] == "" {
					t.Errorf("%s/%s: projects audiences %v, want both %q and %q",
						overlay, name, seen, anthropicAudience, openaiAudience)
				}
			}
		})
	}
}

// TestTheCloudOverlaysCarryOpenAIFederationPlaceholders holds the ids' shape in
// the overlays an operator fills in.
//
// Placeholders rather than real ids, deliberately: no file under deploy/ names
// a real identity provider or service account. The runbook is where the
// operator learns which Platform console page produces each one.
func TestTheCloudOverlaysCarryOpenAIFederationPlaceholders(t *testing.T) {
	for _, overlay := range []string{cloudOverlay, "cloud-entry"} {
		t.Run(overlay, func(t *testing.T) {
			data := openaiFederationConfig(t, render(t, overlay))
			for _, key := range []string{
				"MEMQL_AI_OPENAI_IDENTITY_PROVIDER_ID",
				"MEMQL_AI_OPENAI_SERVICE_ACCOUNT_ID",
			} {
				v, ok := data[key]
				if !ok {
					t.Errorf("%s: the OpenAI federation ConfigMap has no %s", overlay, key)
					continue
				}
				if !strings.HasPrefix(v, "REPLACE-WITH-") {
					t.Errorf("%s: %s is %q -- the committed overlay must carry a placeholder, never a real id",
						overlay, key, v)
				}
			}
		})
	}
}

// TestTheLocalOverlayLeavesOpenAIFederationEmpty is the parity assertion, and
// for OpenAI it is also a statement of what a local cluster gives up.
//
// k3d's OIDC issuer is not publicly reachable, so neither vendor can federate
// with it and there is no key arm left to fall back to. Empty ids mean "not
// federating", which is the normal state of a local cluster; a non-empty value
// here would be a half-configuration and would refuse boot.
func TestTheLocalOverlayLeavesOpenAIFederationEmpty(t *testing.T) {
	data := openaiFederationConfig(t, render(t, "local"))
	for key, value := range data {
		if value != "" {
			t.Errorf("the local overlay sets %s=%q; local federation is not a reproducible path (k3d's OIDC issuer is not publicly reachable) and a non-empty value here would refuse boot",
				key, value)
		}
	}
	if len(data) != 2 {
		t.Errorf("the local OpenAI federation ConfigMap carries %d keys, want the same 2 the cloud fills", len(data))
	}
}
