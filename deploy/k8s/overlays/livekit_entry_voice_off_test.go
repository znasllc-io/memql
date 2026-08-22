// livekit_entry_voice_off_test.go -- the voice-off Service hold (memql#4225).
//
// WHAT IT PROTECTS. overlays/cloud-entry runs voice at replicas 0, and
// replicas 0 is not the whole of voice-off: a LoadBalancer Service with zero
// endpoints still allocates a public IP on Azure, and base declares two for
// the media plane (livekit-rtc) and the SIP plane (livekit-sip). The first
// entry install converted both to ClusterIP by hand, and the next Argo sync of
// the overlay was refused:
//
//	Service "livekit-rtc" is invalid: spec.externalTrafficPolicy: Invalid
//	value: "Local": may only be set for externally-accessible services
//
// because the overlay still wanted LoadBalancer + externalTrafficPolicy=Local,
// and a ClusterIP Service cannot carry that field (nor loadBalancerSourceRanges).
// The Application stayed OutOfSync/Failed with every pin inside it correct. So
// voice-off is ONE decision the overlay expresses for the Services too: type
// ClusterIP, and every LoadBalancer-only field removed, beside the replica-0
// patches.
//
// WHY TEXT-LEVEL AS WELL AS A RENDER. render() skips without kustomize or
// kubectl on the runner, and a guard that silently skips is what let the
// memql#4113 gap persist. This file reads the kustomization and the base
// manifests directly, so it has no such dependency;
// render_cloud_entry_test.go's TestCloudEntryLiveKitServicesAreClusterIP is
// the rendered-output assertion beside it.
package overlays

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// voiceOffLoadBalancers are the base Services the hold converts. Listed, not
// discovered: the failure worth catching is a LoadBalancer arriving in base
// and nobody extending the hold, which discovery would wave through.
var voiceOffLoadBalancers = map[string]string{
	"livekit-rtc": filepath.Join("..", "base", "livekit.yaml"),
	"livekit-sip": filepath.Join("..", "base", "livekit-sip.yaml"),
}

// kustomizationPatches is the slice of a kustomization.yaml these gates read.
type kustomizationPatches struct {
	Patches []struct {
		Target struct {
			Kind string `yaml:"kind"`
			Name string `yaml:"name"`
		} `yaml:"target"`
		Patch string `yaml:"patch"`
	} `yaml:"patches"`
}

// jsonPatchOp is one RFC 6902 operation out of an inline `patch: |` block.
type jsonPatchOp struct {
	Op    string `yaml:"op"`
	Path  string `yaml:"path"`
	Value any    `yaml:"value"`
}

// servicePatchOps returns every JSON 6902 op a kustomization aims at the named
// Service, across however many patch entries target it.
func servicePatchOps(t *testing.T, overlay, service string) []jsonPatchOp {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(overlay, "kustomization.yaml"))
	if err != nil {
		t.Fatalf("reading the %s kustomization: %v", overlay, err)
	}
	var k kustomizationPatches
	if err := yaml.Unmarshal(raw, &k); err != nil {
		t.Fatalf("parsing the %s kustomization: %v", overlay, err)
	}
	var out []jsonPatchOp
	for _, p := range k.Patches {
		if p.Target.Kind != "Service" || p.Target.Name != service {
			continue
		}
		var ops []jsonPatchOp
		if err := yaml.Unmarshal([]byte(p.Patch), &ops); err != nil {
			t.Fatalf("the %s patch targeting Service %s is not a JSON 6902 op list: %v\n%s", overlay, service, err, p.Patch)
		}
		out = append(out, ops...)
	}
	return out
}

func hasOp(ops []jsonPatchOp, op, path string) bool {
	for _, o := range ops {
		if o.Op == op && o.Path == path {
			return true
		}
	}
	return false
}

// TestBaseStillDeclaresTheLoadBalancersTheHoldConverts is the reachable
// positive. The hold uses `remove` for the LoadBalancer-only fields so that a
// base which stops declaring them fails the render loudly rather than leaving
// a silent no-op; this asserts the fields are there to be removed, so the
// gate below is about something.
func TestBaseStillDeclaresTheLoadBalancersTheHoldConverts(t *testing.T) {
	for service, file := range voiceOffLoadBalancers {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("reading %s: %v", file, err)
		}
		var found bool
		for _, r := range parse(t, string(raw)) {
			if r.Kind != "Service" || r.Metadata.Name != service {
				continue
			}
			found = true
			if r.Spec.Type != "LoadBalancer" {
				t.Errorf("base Service %s is %q, want LoadBalancer -- voice-on needs the public media IP, and the hold's patch is written against it", service, r.Spec.Type)
			}
			if r.Spec.ExternalTrafficPolicy != "Local" {
				t.Errorf("base Service %s has externalTrafficPolicy=%q, want Local", service, r.Spec.ExternalTrafficPolicy)
			}
		}
		if !found {
			t.Errorf("%s declares no Service named %s", file, service)
		}
	}
}

// TestCloudEntryHoldsTheLiveKitServicesAtClusterIP reads the hold itself.
func TestCloudEntryHoldsTheLiveKitServicesAtClusterIP(t *testing.T) {
	for service := range voiceOffLoadBalancers {
		ops := servicePatchOps(t, entryOverlay, service)
		if len(ops) == 0 {
			t.Errorf("cloud-entry carries no patch for Service %s; with voice at replicas 0 it still allocates a public IP as a LoadBalancer (memql#4225)", service)
			continue
		}
		var typed bool
		for _, o := range ops {
			if o.Op == "replace" && o.Path == "/spec/type" {
				typed = true
				if v, _ := o.Value.(string); v != "ClusterIP" {
					t.Errorf("cloud-entry sets Service %s type to %v, want ClusterIP", service, o.Value)
				}
			}
		}
		if !typed {
			t.Errorf("cloud-entry does not replace /spec/type on Service %s", service)
		}
		if !hasOp(ops, "remove", "/spec/externalTrafficPolicy") {
			t.Errorf("cloud-entry does not remove /spec/externalTrafficPolicy on Service %s; the API server refuses Local on a ClusterIP Service and Argo stays Failed", service)
		}
		if strings.Contains(readFile(t, voiceOffLoadBalancers[service]), "loadBalancerSourceRanges") &&
			!hasOp(ops, "remove", "/spec/loadBalancerSourceRanges") {
			t.Errorf("cloud-entry does not remove /spec/loadBalancerSourceRanges on Service %s; base declares it and the API server refuses it on a ClusterIP Service", service)
		}
	}
}

// TestCloudDoesNotHoldTheLiveKitServices: the hold is cloud-entry's alone.
// overlays/cloud keeps voice on, and a ClusterIP media plane there would be a
// LiveKit that advertises an address nothing can reach.
func TestCloudDoesNotHoldTheLiveKitServices(t *testing.T) {
	for service := range voiceOffLoadBalancers {
		if ops := servicePatchOps(t, cloudOverlay, service); hasOp(ops, "replace", "/spec/type") {
			t.Errorf("overlays/cloud patches /spec/type on Service %s; voice stays on there and the ClusterIP hold belongs to cloud-entry only", service)
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(raw)
}
