// Render gates on the cloud-entry overlay (memql#4203).
//
// overlays/cloud stays top + mesh 2. This overlay is the entry / client
// instance: entry CNPG, mesh 1, voice-off as replicas 0. A second
// Application next to deploy/argocd/apps/memql.yaml would be the staging
// split epic memql#3943 removed -- ZNAS Argo lives in its own cluster and
// points at this path.
package overlays

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	entryOverlay  = "cloud-entry"
	entryReplicas = 1
)

var entryMeshOn = []string{
	"identity", "bff", "cognition", "agent", "planner", "workbench", "edge",
}

var entryVoiceOff = []string{
	"voice", "voice-agent", "livekit", "livekit-sip", "mcp", "livekit-redis",
}

func TestCloudEntryLandsWhollyInOneNamespace(t *testing.T) {
	var sawNamespaceObject bool
	for _, r := range parse(t, render(t, entryOverlay)) {
		if r.Kind == "Namespace" {
			sawNamespaceObject = true
			if r.Metadata.Name != cloudNamespace {
				t.Errorf("the Namespace object is named %q, want %q", r.Metadata.Name, cloudNamespace)
			}
			continue
		}
		if r.Metadata.Namespace == "" {
			continue
		}
		if r.Metadata.Namespace != cloudNamespace {
			t.Errorf("%s/%s lands in namespace %q, want %q",
				r.Kind, r.Metadata.Name, r.Metadata.Namespace, cloudNamespace)
		}
	}
	if !sawNamespaceObject {
		t.Error("no Namespace object rendered")
	}
}

func TestCloudEntryMeshReplicasAreOne(t *testing.T) {
	byName := deploymentsByName(t, render(t, entryOverlay))
	for _, node := range entryMeshOn {
		r, ok := byName[node]
		if !ok {
			t.Errorf("%s does not render", node)
			continue
		}
		if r.Spec.Replicas == nil {
			t.Errorf("%s declares no replica count", node)
			continue
		}
		if *r.Spec.Replicas != entryReplicas {
			t.Errorf("%s has %d replicas, want %d", node, *r.Spec.Replicas, entryReplicas)
		}
	}
}

func TestCloudEntryVoiceOffIsReplicasZero(t *testing.T) {
	byName := deploymentsByName(t, render(t, entryOverlay))
	for _, node := range entryVoiceOff {
		r, ok := byName[node]
		if !ok {
			t.Errorf("%s does not render; voice-off is replicas 0, not a missing Deployment", node)
			continue
		}
		if r.Spec.Replicas == nil {
			t.Errorf("%s declares no replica count", node)
			continue
		}
		if *r.Spec.Replicas != 0 {
			t.Errorf("%s has %d replicas, want 0 (voice-off)", node, *r.Spec.Replicas)
		}
	}
}

func TestCloudEntryUsesTheEntryPreset(t *testing.T) {
	rendered := render(t, entryOverlay)
	var saw bool
	for _, r := range parse(t, rendered) {
		if r.Kind != "Cluster" || r.Metadata.Name != "memql-db" {
			continue
		}
		saw = true
	}
	if !saw {
		t.Fatal("memql-db Cluster did not render")
	}
	// Sizes and instance count are the entry preset. Text-level so we do
	// not have to model the whole CNPG CRD here.
	if !strings.Contains(rendered, "instances: 1") {
		t.Error("memql-db is not 1 instance; cloud-entry must compose cnpg-db/presets/entry")
	}
	// Data and WAL are BOTH 32Gi at this tier since memql#4459, so a bare
	// substring match can no longer tell them apart, and a check that cannot
	// fail for one of the two things it names is worse than no check.
	//
	// Two things this deliberately does NOT do. It does not assert "16Gi
	// appears nowhere" -- that would fire on any unrelated 16Gi somebody adds
	// later (a memory limit, say) and report it as a WAL regression. And it
	// does not model the CNPG CRD: the authoritative per-field assertion is
	// TestPresetsMatchTheirDocumentedTiers, which decodes the Cluster. What is
	// being checked HERE is only that cloud-entry composes the entry preset.
	if !strings.Contains(rendered, "32Gi") {
		t.Error("memql-db data volume is not 32Gi")
	}
	if got := walStorageSize(rendered); got != "32Gi" {
		t.Errorf("memql-db walStorage size = %q, want 32Gi -- the entry preset must not regress to the "+
			"16Gi that filled and stopped Postgres (memql#4459)", got)
	}
	if strings.Contains(rendered, "256Gi") {
		t.Error("memql-db still carries the top preset 256Gi data volume")
	}
}

func TestCloudEntryHasNoFailOpenVoicePins(t *testing.T) {
	rendered := render(t, entryOverlay)
	if strings.Contains(rendered, "0000000000000000000000000000000000000000000000000000000000000000") {
		t.Error("cloud-entry ships an all-zeros digest; voice-off is replicas 0, not a fake pin")
	}
	// NODE_IP=0.0.0.0 is the cloud overlay's fail-closed LiveKit advertise
	// address. bind_addresses: ["0.0.0.0"] in livekit-config is a listen
	// bind and is fine.
	for _, line := range strings.Split(rendered, "\n") {
		trim := strings.TrimSpace(line)
		if strings.Contains(trim, "name: NODE_IP") {
			t.Errorf("cloud-entry still sets NODE_IP (%s); voice-off is replicas 0", trim)
		}
	}
}

func TestCloudStaysOnTopAndTheInClusterAppIsUnchanged(t *testing.T) {
	// overlays/cloud is not this PR. The in-cluster Application must keep
	// pointing at it -- an entry install's own Argo is a different cluster.
	app := readApplication(t, cloudApp)
	if want := "deploy/k8s/overlays/" + cloudOverlay; app.Spec.Source.Path != want {
		t.Errorf("in-cluster Application path is %q, want %q -- do not retarget it at cloud-entry", app.Spec.Source.Path, want)
	}
	if _, err := os.Stat(filepath.Join("..", "..", "argocd", "apps", "memql-entry.yaml")); err == nil {
		t.Error("deploy/argocd/apps/memql-entry.yaml exists; do not add a second Application in the same cluster")
	}
}

func deploymentsByName(t *testing.T, rendered string) map[string]resource {
	t.Helper()
	byName := map[string]resource{}
	for _, r := range parse(t, rendered) {
		if r.Kind == "Deployment" {
			byName[r.Metadata.Name] = r
		}
	}
	return byName
}

// entryRematerialize are the engine Deployments whose boot sweep rematerializes
// seeds (#4222). Listed rather than discovered: the failure this catches is a
// node that rematerializes without MEMQL_DOMAIN, so the portal hostname stays
// portal.memql.localhost on an install that set a real domain.
var entryRematerialize = []string{
	"identity", "bff", "cognition", "agent", "planner",
	"workbench", "mcp", "voice", "voice-agent", "edge",
}

func TestCloudEntryRematerializingDeploymentsMountMemqlDomain(t *testing.T) {
	rendered := render(t, entryOverlay)
	docs := strings.Split(rendered, "\n---\n")

	for _, node := range entryRematerialize {
		var found bool
		for _, doc := range docs {
			if !strings.Contains(doc, "kind: Deployment") ||
				!strings.Contains(doc, "\n  name: "+node+"\n") {
				continue
			}
			found = true
			if !strings.Contains(doc, "name: memql-domain") {
				t.Errorf("%s does not mount the memql-domain ConfigMap", node)
			}
		}
		if !found {
			t.Errorf("no Deployment named %s in the rendered cloud-entry overlay", node)
		}
	}
}

func TestCloudEntryCommitsNoHostname(t *testing.T) {
	// The overlay states the relationship to MEMQL_DOMAIN, never a value.
	// A committed portal hostname would beat the materializer rewrite for
	// nothing -- the seed hostname is derived at rematerialize time.
	raw, err := os.ReadFile(filepath.Join(entryOverlay, "kustomization.yaml"))
	if err != nil {
		t.Fatalf("reading cloud-entry kustomization: %v", err)
	}
	if strings.Contains(string(raw), "portal.") && strings.Contains(string(raw), "hostname") {
		t.Error("cloud-entry kustomization commits a portal hostname")
	}
	// The envFrom append moved into ../components/domain-derive with the
	// deletes it was always meant to travel with (memql#4281) -- this overlay
	// had the append alone, which is the silent half. Assert against the
	// component: it is what this overlay now mounts.
	patch, err := os.ReadFile(filepath.Join("..", "components", "domain-derive", "patches", "envfrom.yaml"))
	if err != nil {
		t.Fatalf("reading the domain-derive envFrom patch: %v", err)
	}
	if strings.Contains(string(patch), "hostname:") {
		t.Error("the domain-derive envFrom patch commits a hostname")
	}
}

// liveKitServices are the media-plane and SIP-plane Services base declares.
// `livekit` (signaling) is ClusterIP in base already; the other two are
// LoadBalancers there, which is right for overlays/cloud (voice on) and wrong
// for this overlay (memql#4225): a LoadBalancer with zero endpoints still
// allocates a public IP on Azure.
var liveKitServices = []string{"livekit", "livekit-rtc", "livekit-sip"}

// azureLoadBalancerAnnotation is the prefix of every Azure LB tuning
// annotation base carries on those Services. LoadBalancer-only, so it goes
// with the type.
const azureLoadBalancerAnnotation = "service.beta.kubernetes.io/azure-load-balancer"

func servicesByName(t *testing.T, rendered string) map[string]resource {
	t.Helper()
	byName := map[string]resource{}
	for _, r := range parse(t, rendered) {
		if r.Kind == "Service" {
			byName[r.Metadata.Name] = r
		}
	}
	return byName
}

// TestCloudEntryLiveKitServicesAreClusterIP is the rendered half of the
// voice-off Service hold (memql#4225); livekit_entry_voice_off_test.go is the
// text-level half that cannot skip.
//
// The failure this catches reconciles at first and then does not: an entry install
// converted these Services to ClusterIP by hand, and the next Argo sync of
// the overlay -- still LoadBalancer + externalTrafficPolicy=Local -- was
// refused by the API server ("may only be set for externally-accessible
// services"), leaving the Application OutOfSync/Failed while the pins inside
// it were fine.
func TestCloudEntryLiveKitServicesAreClusterIP(t *testing.T) {
	byName := servicesByName(t, render(t, entryOverlay))
	for _, name := range liveKitServices {
		r, ok := byName[name]
		if !ok {
			t.Errorf("Service %s does not render; voice-off holds the Service at ClusterIP, it does not delete it", name)
			continue
		}
		if r.Spec.Type != "ClusterIP" {
			t.Errorf("Service %s renders as type %q, want ClusterIP -- a LoadBalancer with zero endpoints still allocates a public IP", name, r.Spec.Type)
		}
		if r.Spec.ExternalTrafficPolicy != "" {
			t.Errorf("Service %s still carries externalTrafficPolicy=%q; the API server refuses that on a ClusterIP Service and Argo stays Failed", name, r.Spec.ExternalTrafficPolicy)
		}
		if len(r.Spec.LoadBalancerSourceRanges) > 0 {
			t.Errorf("Service %s still carries loadBalancerSourceRanges %v; the API server refuses that on a ClusterIP Service", name, r.Spec.LoadBalancerSourceRanges)
		}
		for k := range r.Metadata.Annotations {
			if strings.HasPrefix(k, azureLoadBalancerAnnotation) {
				t.Errorf("Service %s still carries the LoadBalancer-only annotation %s", name, k)
			}
		}
	}
}

// TestCloudKeepsLiveKitLoadBalancers is the reachable positive for the gate
// above: the same assertion, inverted, on the overlay where voice stays ON.
// If the media plane stopped being a LoadBalancer in base, the cloud-entry
// gate would pass for a reason that has nothing to do with the hold.
func TestCloudKeepsLiveKitLoadBalancers(t *testing.T) {
	byName := servicesByName(t, render(t, cloudOverlay))
	for _, name := range []string{"livekit-rtc", "livekit-sip"} {
		r, ok := byName[name]
		if !ok {
			t.Errorf("Service %s does not render in the cloud overlay", name)
			continue
		}
		if r.Spec.Type != "LoadBalancer" {
			t.Errorf("cloud overlay Service %s is %q, want LoadBalancer -- voice stays on there; the ClusterIP hold is cloud-entry's alone", name, r.Spec.Type)
		}
		if r.Spec.ExternalTrafficPolicy != "Local" {
			t.Errorf("cloud overlay Service %s has externalTrafficPolicy=%q, want Local", name, r.Spec.ExternalTrafficPolicy)
		}
	}
}

// walStorageSize returns the `size:` under the rendered Cluster's walStorage
// block, or "" if there is none.
//
// Scanned line-wise rather than matched with one regex because YAML key order
// inside the block is not a guarantee we should depend on: a regex requiring
// `size:` to follow `walStorage:` immediately would start failing the day
// something reorders the map, and it would fail claiming the WAL volume is the
// wrong size -- which is a false report about a real risk, the worst kind.
func walStorageSize(rendered string) string {
	lines := strings.Split(rendered, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) != "walStorage:" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		for _, sub := range lines[i+1:] {
			trimmed := strings.TrimSpace(sub)
			if trimmed == "" {
				continue
			}
			// Left the block: same or shallower indentation.
			if len(sub)-len(strings.TrimLeft(sub, " ")) <= indent {
				break
			}
			if v, ok := strings.CutPrefix(trimmed, "size:"); ok {
				return strings.TrimSpace(v)
			}
		}
	}
	return ""
}

// externalSecretsByName indexes the ExternalSecrets in a rendered overlay.
func externalSecretsByName(t *testing.T, rendered string) map[string]resource {
	t.Helper()
	out := map[string]resource{}
	for _, r := range parse(t, rendered) {
		if r.Kind == "ExternalSecret" {
			out[r.Metadata.Name] = r
		}
	}
	return out
}

// TestCloudEntryRendersNoExternalSecrets is the rendered half of the memql#4487
// hold; externalsecrets_test.go carries the text-level half that cannot skip.
//
// The failure it catches is not a broken render -- it is a render that
// reconciles perfectly and leaves the Application `Degraded` forever, because
// two of its objects resolve Key Vault entries that a voice-off install
// deliberately does not have. That red was documented as expected noise, which
// is the actual defect: health that is always red carries no information, and
// an operator who learns to ignore it will ignore a real one.
func TestCloudEntryRendersNoExternalSecrets(t *testing.T) {
	for name, r := range externalSecretsByName(t, render(t, entryOverlay)) {
		t.Errorf("cloud-entry renders ExternalSecret %s; with voice off its Key Vault entries do not exist, so ESO reports SecretSyncedError and the Application is Degraded on every entry install (memql#4487). Enabling voice is the reverse, in this order: seed the entries, then drop the delete patch. (rendered as %s)",
			name, r.APIVersion)
	}
}

// TestCloudRendersTheVoiceExternalSecretsWithIgnoreExtraneous is the reachable
// positive for the gate above AND the rendered half of memql#4489.
//
// Voice is ON in overlays/cloud, so the objects belong in its render -- if base
// simply stopped shipping them, the cloud-entry gate above would pass for a
// reason that has nothing to do with the hold. And the annotation has to
// survive the render, not merely sit in base: External Secrets copies it onto
// the generated Secret, which is the only thing that stops Argo claiming a
// Secret that exists in no repository.
func TestCloudRendersTheVoiceExternalSecretsWithIgnoreExtraneous(t *testing.T) {
	byName := externalSecretsByName(t, render(t, cloudOverlay))
	for _, name := range []string{"livekit-secrets", "telephony-secrets"} {
		r, ok := byName[name]
		if !ok {
			t.Errorf("overlays/cloud does not render ExternalSecret %s; voice stays on there, and the cloud-entry hold is written against base still shipping it", name)
			continue
		}
		if got := r.Metadata.Annotations[argoCompareOptions]; !hasCompareOption(got, ignoreExtraneous) {
			t.Errorf("rendered ExternalSecret %s has %s=%q, want it to include %s -- the generated Secret inherits this annotation, and without it Argo reports that Secret OutOfSync forever (memql#4489)",
				name, argoCompareOptions, got, ignoreExtraneous)
		}
	}
}
