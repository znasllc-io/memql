// Render gates on the cloud-entry overlay (memql#4203).
//
// overlays/cloud stays top + mesh 2. This overlay is the entry / client
// instance: entry CNPG, mesh 1, mcp held at replicas 0. A second
// Application next to deploy/argocd/apps/memql.yaml would be the staging
// split epic memql#3943 removed -- ZNAS Argo lives in its own cluster and
// points at this path.
package overlays

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	entryOverlay  = "cloud-entry"
	entryReplicas = 1
)

var entryMeshOn = []string{
	"identity", "bff", "agent", "planner", "workbench", "edge",
}

// entryHeldOff are the Deployments this overlay holds at replicas 0 -- a
// module that is not enabled on an entry install. Held rather than deleted, so
// enabling it is a values change on the tenant's own overlay.
var entryHeldOff = []string{"mcp"}

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

func TestCloudEntryHeldOffIsReplicasZero(t *testing.T) {
	byName := deploymentsByName(t, render(t, entryOverlay))
	for _, node := range entryHeldOff {
		r, ok := byName[node]
		if !ok {
			t.Errorf("%s does not render; a held-off module is replicas 0, not a missing Deployment", node)
			continue
		}
		if r.Spec.Replicas == nil {
			t.Errorf("%s declares no replica count", node)
			continue
		}
		if *r.Spec.Replicas != 0 {
			t.Errorf("%s has %d replicas, want 0 (held off)", node, *r.Spec.Replicas)
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

func TestCloudEntryHasNoFailOpenPins(t *testing.T) {
	rendered := render(t, entryOverlay)
	if strings.Contains(rendered, "0000000000000000000000000000000000000000000000000000000000000000") {
		t.Error("cloud-entry ships an all-zeros digest; a held-off module is replicas 0, not a fake pin")
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
	"identity", "bff", "agent", "planner",
	"workbench", "mcp", "edge",
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

// ---------------------------------------------------------------------------
// memql#4598 -- every upgrade was an outage, by construction
// ---------------------------------------------------------------------------

// entryWorkload is a Deployment view carrying the two fields #4598 is about.
// The shared `resource` struct deliberately stays narrow, so this test parses
// its own shape rather than widening one that a dozen other gates depend on.
type entryWorkload struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Replicas *int `yaml:"replicas"`
		Strategy struct {
			Type          string `yaml:"type"`
			RollingUpdate struct {
				MaxSurge       *int `yaml:"maxSurge"`
				MaxUnavailable *int `yaml:"maxUnavailable"`
			} `yaml:"rollingUpdate"`
		} `yaml:"strategy"`
		Template struct {
			Spec struct {
				Containers []struct {
					Name      string `yaml:"name"`
					Resources struct {
						Requests struct {
							CPU    string `yaml:"cpu"`
							Memory string `yaml:"memory"`
						} `yaml:"requests"`
					} `yaml:"resources"`
				} `yaml:"containers"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

func entryWorkloadsByName(t *testing.T, rendered string) map[string]entryWorkload {
	t.Helper()
	dec := yaml.NewDecoder(strings.NewReader(rendered))
	out := map[string]entryWorkload{}
	for {
		var w entryWorkload
		err := dec.Decode(&w)
		if err != nil {
			break
		}
		if w.Kind == "Deployment" && w.Metadata.Name != "" {
			out[w.Metadata.Name] = w
		}
	}
	return out
}

// AT ONE REPLICA, maxUnavailable: 1 MEANS "TAKE THE ONLY POD DOWN FIRST".
//
// base carries maxSurge 0 / maxUnavailable 1 and is right to at the 2 replicas
// it declares -- draining old before starting new keeps a roll from doubling a
// node's Postgres pools (memql#1858), and one pod is always ready. This overlay
// runs at 1, where the identical setting leaves no window with a ready pod in
// it: a v0.19.9 -> v0.20.0 bump returned 503 for about fifteen minutes on a
// live entry install (znasllc-io/memql-znas#18).
//
// Capacity-independent, so no amount of headroom substitutes for this.
func TestCloudEntryRollsWithoutAnOutage(t *testing.T) {
	byName := entryWorkloadsByName(t, render(t, entryOverlay))
	for _, node := range entryMeshOn {
		w, ok := byName[node]
		if !ok {
			t.Errorf("%s does not render", node)
			continue
		}
		ru := w.Spec.Strategy.RollingUpdate
		if ru.MaxUnavailable == nil || *ru.MaxUnavailable != 0 {
			got := "unset"
			if ru.MaxUnavailable != nil {
				got = fmt.Sprint(*ru.MaxUnavailable)
			}
			t.Errorf("%s has maxUnavailable=%s at %d replica(s). That takes the only serving pod\n"+
				"down before its replacement is ready -- every upgrade is an outage (memql#4598).",
				node, got, deref(w.Spec.Replicas))
		}
		if ru.MaxSurge == nil || *ru.MaxSurge < 1 {
			got := "unset"
			if ru.MaxSurge != nil {
				got = fmt.Sprint(*ru.MaxSurge)
			}
			t.Errorf("%s has maxSurge=%s. With maxUnavailable 0 and no surge allowed the rollout\n"+
				"cannot progress at all -- it needs somewhere to put the new pod.", node, got)
		}
	}
}

// maxUnavailable: 0 IS ONLY HONEST IF THE SURGE POD CAN BE PLACED.
//
// Reserved 200m/256Mi against 2-12m/44-56Mi measured -- 20-70x, and 72-78% of a
// two-node 2-vCPU pool reserved for 3-4% used. That is enough to make the surge
// replica unschedulable outright (FailedScheduling / Insufficient cpu), so the
// strategy above would stall a roll rather than fix it. The two are one defect.
//
// The ceiling is asserted rather than the literal value: what must hold is that
// a mesh node reserves an amount an entry pool can double, not that it reserves
// exactly 50m.
func TestCloudEntryReservesRoomForItsOwnSurgeReplica(t *testing.T) {
	const (
		maxCPUMillis = 100 // ~8x the busiest measured node (identity, 12m)
		maxMemMiB    = 192 // ~3.4x the busiest measured node (56Mi)
	)
	byName := entryWorkloadsByName(t, render(t, entryOverlay))
	totalCPU, totalMem := 0, 0
	for _, node := range entryMeshOn {
		w, ok := byName[node]
		if !ok || len(w.Spec.Template.Spec.Containers) == 0 {
			continue
		}
		req := w.Spec.Template.Spec.Containers[0].Resources.Requests
		cpu := milliCPU(t, node, req.CPU)
		mem := mebibytes(t, node, req.Memory)
		totalCPU += cpu
		totalMem += mem
		if cpu > maxCPUMillis {
			t.Errorf("%s requests %s CPU, over the %dm entry ceiling. Measured usage across the mesh\n"+
				"is 2-12m; a reservation this far above it is what leaves no room to schedule the\n"+
				"surge replica an outage-free roll needs (memql#4598).", node, req.CPU, maxCPUMillis)
		}
		if mem > maxMemMiB {
			t.Errorf("%s requests %s memory, over the %dMi entry ceiling (measured: 44-56Mi).",
				node, req.Memory, maxMemMiB)
		}
	}
	// And the aggregate has to leave a whole extra node's worth free, since
	// that is exactly what maxSurge: 1 asks the scheduler for.
	if largest := maxCPUMillis; totalCPU+largest > 1800 {
		t.Errorf("the mesh reserves %dm CPU in total; adding one surge replica (%dm) exceeds what a\n"+
			"2-vCPU entry node can allocate. Right-sizing is the half that makes maxUnavailable: 0\n"+
			"honest rather than merely stated.", totalCPU, largest)
	}
	t.Logf("entry mesh reserves %dm CPU / %dMi memory across %d nodes", totalCPU, totalMem, len(entryMeshOn))
}

func deref(p *int) int {
	if p == nil {
		return -1
	}
	return *p
}

// milliCPU parses the two Kubernetes CPU spellings ("50m", "1"). An empty
// request is 0 -- a workload that reserves nothing cannot be the thing
// crowding out a surge replica.
func milliCPU(t *testing.T, node, v string) int {
	t.Helper()
	if v == "" {
		return 0
	}
	if strings.HasSuffix(v, "m") {
		n, err := strconv.Atoi(strings.TrimSuffix(v, "m"))
		if err != nil {
			t.Fatalf("%s: unparseable cpu request %q", node, v)
		}
		return n
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		t.Fatalf("%s: unparseable cpu request %q", node, v)
	}
	return int(f * 1000)
}

func mebibytes(t *testing.T, node, v string) int {
	t.Helper()
	if v == "" {
		return 0
	}
	switch {
	case strings.HasSuffix(v, "Mi"):
		n, err := strconv.Atoi(strings.TrimSuffix(v, "Mi"))
		if err != nil {
			t.Fatalf("%s: unparseable memory request %q", node, v)
		}
		return n
	case strings.HasSuffix(v, "Gi"):
		n, err := strconv.Atoi(strings.TrimSuffix(v, "Gi"))
		if err != nil {
			t.Fatalf("%s: unparseable memory request %q", node, v)
		}
		return n * 1024
	}
	t.Fatalf("%s: unrecognised memory request %q", node, v)
	return 0
}

// overlays/cloud runs at 2 replicas, where memql#1858's drain-before-start is
// correct and there is no outage. This gate is what keeps the #4598 fix scoped
// to the overlay that has the problem instead of leaking into the one that
// does not.
func TestCloudKeepsDrainBeforeStartAtTwoReplicas(t *testing.T) {
	byName := entryWorkloadsByName(t, render(t, cloudOverlay))
	for _, node := range []string{"identity", "agent", "planner", "workbench", "edge"} {
		w, ok := byName[node]
		if !ok {
			t.Errorf("%s does not render in overlays/cloud", node)
			continue
		}
		ru := w.Spec.Strategy.RollingUpdate
		if ru.MaxSurge == nil || *ru.MaxSurge != 0 {
			t.Errorf("%s in overlays/cloud has maxSurge=%v, want 0. memql#1858 drains old before\n"+
				"starting new so a roll does not transiently double this node's Postgres pools;\n"+
				"memql#4598 is about the ONE-replica overlay and must not change this one.",
				node, ru.MaxSurge)
		}
	}
}

// ---------------------------------------------------------------------------
// memql#4634 -- every user-facing node keeps a PodDisruptionBudget
// ---------------------------------------------------------------------------

// THE GAP THIS CLOSES, AND WHY IT SURVIVED A LIVE CHECK.
//
// base ships a PDB beside the Deployment for every user-facing node. The bff
// does not live in base -- it comes from components/engine-bff, which shipped
// the Deployment alone -- so every install had its user-facing nodes protected
// and one unprotected, and the unprotected one is the node the portal and
// every SPA actually talk to.
//
// The downstream verification enumerated what it FOUND ("PDBs already exist for
// agent, edge, identity, planner, workbench") rather than what it was
// LOOKING FOR. That is a true statement and also a list with bff missing from
// it, and a check of that shape reads as complete whichever way it comes out.
//
// So this asserts the REQUIREMENT rather than the inventory: it derives the set
// from the rendered Deployments and demands a PDB for each, which means a node
// added tomorrow is covered without anybody remembering to extend a list.
func TestEveryServingNodeHasAPodDisruptionBudget(t *testing.T) {
	rendered := render(t, entryOverlay)

	protected := map[string]bool{}
	for _, r := range parse(t, rendered) {
		if r.Kind == "PodDisruptionBudget" {
			protected[r.Metadata.Name] = true
		}
	}

	// Derived from what actually renders, NOT from entryMeshOn: a list is the
	// thing that failed here. Held-off nodes are excluded because a PDB over
	// zero replicas protects nothing and blocks nothing, and so is anything
	// this overlay does not itself serve traffic from.
	byName := entryWorkloadsByName(t, rendered)
	checked := 0
	for name, w := range byName {
		if w.Spec.Replicas != nil && *w.Spec.Replicas == 0 {
			continue
		}
		checked++
		if !protected[name] {
			t.Errorf("Deployment %s serves traffic at %d replica(s) and has no PodDisruptionBudget. "+
				"A voluntary disruption -- node drain, cluster upgrade -- can then take its last "+
				"pod with nothing to stop it (memql#4634).", name, deref(w.Spec.Replicas))
		}
	}
	if checked == 0 {
		t.Fatal("no serving Deployments found; this gate would pass vacuously")
	}
	t.Logf("checked %d serving Deployment(s) for a PodDisruptionBudget", checked)
}
