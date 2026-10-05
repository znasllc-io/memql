// substrate_overlay_coupling_test.go -- the storage-class/zonality coupling
// (memql#4497).
//
// WHAT IT PROTECTS. An instance overlay pins the database's storage class;
// scripts/deploy/azure-provision.sh creates the node pools that storage has to
// attach to. Those two facts live in different files, in different languages,
// maintained for different reasons -- and Premium SSD v2 makes them ONE
// decision: a PremiumV2_LRS disk attaches only to a VM in an availability
// zone, so pinning managed-csi-premium-v2 is a REQUIREMENT ON THE SUBSTRATE,
// not a preference about disks.
//
// Before this gate they were two independent defaults, and they disagreed: the
// script created a non-zonal cluster (no --zones) while the overlay it exists
// to serve demanded zonal storage. The failure surfaced three layers away at
// the CNPG initdb pod, naming the disk type and nothing about node pools, and
// a PVC-bind probe cleared it wrongly -- the PV provisions fine and only the
// attach fails.
//
// HOW TO REPAIR A FAILURE HERE -- there are two directions and they are not
// interchangeable:
//
//   - The overlay still wants Premium SSD v2 (the normal case): restore a
//     non-empty default for `zones` in azure-provision.sh. Do NOT relax this
//     gate; an empty default reintroduces exactly the substrate that cannot
//     run the overlay.
//   - The overlay deliberately moved OFF a zone-requiring class (e.g. to
//     managed-csi-premium, which is zone-agnostic): add that class to
//     zoneAgnosticStorageClasses below. The coupling genuinely does not apply
//     to it, and saying so here is the way to record that.
//
// A SECOND COUPLING OF THE SAME SHAPE (memql#5492). The pipelines step cache
// is ReadWriteMany on the cloud overlays, which on AKS is the Blob CSI driver's
// NFS class (azureblob-nfs-premium) -- a class that exists only on a cluster
// running the driver, which the same script has to turn on. Without it the
// claim sits Pending naming a class that does not exist, and every step that
// declares a cache waits out its scheduling timeout. Repair it the same two
// ways: keep the script's `--enable-blob-driver` (create AND the existing-
// cluster update), or move the overlays off the azureblob- classes.
//
// TEXT-LEVEL ON PURPOSE. render() skips without kustomize on the runner, and a
// guard that silently skips is not a guard. This reads both files directly and
// has no external dependency. (The RENDERED cache class is pinned separately,
// per overlay, by TestPipelinesCacheClassAndAccessModeAreTheOverlaysValues.)
package overlays

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// zoneRequiringStorageClasses are the Azure storage classes whose disks can
// only attach to a VM in an availability zone. Listed rather than pattern-
// matched: "v2" is not a general rule, it is a property of these specific
// classes, and a future class that shares the constraint should have to be
// named here by someone who checked.
var zoneRequiringStorageClasses = []string{
	"managed-csi-premium-v2",
}

// zoneAgnosticStorageClasses are classes explicitly known NOT to carry the
// constraint. Kept so that moving an overlay off Premium SSD v2 is a recorded
// decision rather than a silently-passing one.
var zoneAgnosticStorageClasses = []string{
	"managed-csi",
	"managed-csi-premium",
	"azurefile-csi",
}

// instanceOverlayKustomizations are the overlays that pin a database storage
// class for a cloud substrate. The local overlay is absent deliberately: k3d
// has no Azure disks and no zones.
var instanceOverlayKustomizations = []string{
	filepath.Join("cloud", "kustomization.yaml"),
	filepath.Join("cloud-entry", "kustomization.yaml"),
}

const provisionScriptRel = "../../../scripts/deploy/azure-provision.sh"

var storageClassPin = regexp.MustCompile(`/spec/(?:wal)?[Ss]torage/storageClass, value: ([A-Za-z0-9._-]+)`)

// TestZoneRequiringStorageNeedsAZonalDefaultSubstrate is the coupling itself:
// if any instance overlay pins a zone-requiring storage class, the provisioning
// script's `zones` parameter must default to something non-empty.
func TestZoneRequiringStorageNeedsAZonalDefaultSubstrate(t *testing.T) {
	pinning := map[string][]string{}
	for _, rel := range instanceOverlayKustomizations {
		body, err := os.ReadFile(rel)
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		for _, m := range storageClassPin.FindAllStringSubmatch(string(body), -1) {
			class := m[1]
			if !contains(zoneRequiringStorageClasses, class) {
				if !contains(zoneAgnosticStorageClasses, class) {
					t.Errorf("%s pins storage class %q, which is classified by neither "+
						"zoneRequiringStorageClasses nor zoneAgnosticStorageClasses in this file. "+
						"Decide which it is and add it: whether its disks can attach to a non-zonal "+
						"VM is exactly the question this gate exists to keep answered.", rel, class)
				}
				continue
			}
			pinning[class] = appendUnique(pinning[class], rel)
		}
	}

	if len(pinning) == 0 {
		// Not a silent pass: say what was examined, so a future reader can tell
		// "the coupling is satisfied" from "nothing pins such a class any more".
		t.Logf("no instance overlay pins a zone-requiring storage class; the coupling is inert. "+
			"Examined: %v", instanceOverlayKustomizations)
		return
	}

	script, err := os.ReadFile(provisionScriptRel)
	if err != nil {
		t.Fatalf("read %s: %v", provisionScriptRel, err)
	}
	def, ok := capParamDefault(string(script), "zones")
	if !ok {
		t.Fatalf("scripts/deploy/azure-provision.sh no longer declares a `zones` capability "+
			"parameter, but %v still pin zone-requiring storage (%v). Premium SSD v2 attaches "+
			"only to a VM in an availability zone, so removing --zones makes the script's own "+
			"substrate unable to run the overlay it exists to serve.",
			flatten(pinning), keys(pinning))
	}
	if strings.TrimSpace(def) == "" {
		t.Errorf("scripts/deploy/azure-provision.sh defaults `zones` to the empty string, which "+
			"creates NON-ZONAL node pools, while %v pin %v -- storage that can only attach to a "+
			"VM in an availability zone. Restore a non-empty default (it was \"1\"), or move the "+
			"overlays off the class and record that in zoneAgnosticStorageClasses.",
			flatten(pinning), keys(pinning))
	}
}

// TestTheZonalityCouplingIsStatedWhereTheStorageClassLives keeps the reason
// beside the decision. The gate above catches the drift; a reader editing the
// kustomization needs to know the constraint exists BEFORE they change the
// value, and a test failure they have not triggered yet cannot tell them.
func TestTheZonalityCouplingIsStatedWhereTheStorageClassLives(t *testing.T) {
	for _, rel := range instanceOverlayKustomizations {
		body, err := os.ReadFile(rel)
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		text := string(body)
		pinsZoneRequiring := false
		for _, m := range storageClassPin.FindAllStringSubmatch(text, -1) {
			if contains(zoneRequiringStorageClasses, m[1]) {
				pinsZoneRequiring = true
			}
		}
		if !pinsZoneRequiring {
			continue
		}
		if !strings.Contains(text, "availability zone") {
			t.Errorf("%s pins a zone-requiring storage class but says nothing about availability "+
				"zones. State the coupling next to the pin: the value is a requirement on the "+
				"substrate underneath, and the failure it causes (a CNPG initdb pod naming "+
				"PremiumV2_LRS) points nowhere near this line.", rel)
		}
		if !strings.Contains(text, "azure-provision.sh") {
			t.Errorf("%s pins a zone-requiring storage class without naming the other half of "+
				"the coupling. Point at scripts/deploy/azure-provision.sh --zones, so a reader "+
				"changing this value knows which file has to agree with it.", rel)
		}
	}
}

// blobDriverClassPrefix names the storage classes the Azure Blob CSI driver
// provides. AKS creates them (azureblob-nfs-premium, azureblob-fuse-premium)
// only on a cluster running the driver.
const blobDriverClassPrefix = "azureblob-"

var storageClassNameValue = regexp.MustCompile(`^\s*storageClassName:\s*["']?([A-Za-z0-9._-]+)`)

// TestPipelinesCacheClassNeedsTheBlobDriver is the second coupling: if any
// instance overlay names an azureblob- storage class, the provisioning script
// must turn the Blob CSI driver on -- when it CREATES a cluster, and on a
// cluster that already exists, without a prompt.
//
// Every YAML file of each instance overlay is read, not just
// pipelines-values.yaml, so moving the pin to another file of the overlay
// cannot walk it out of the gate. Comment lines are skipped: a class named in
// prose is not a class anything provisions.
func TestPipelinesCacheClassNeedsTheBlobDriver(t *testing.T) {
	pinning := map[string][]string{}
	var examined []string
	for _, kustomization := range instanceOverlayKustomizations {
		dir := filepath.Dir(kustomization)
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
				continue
			}
			rel := filepath.Join(dir, e.Name())
			examined = append(examined, rel)
			body, err := os.ReadFile(rel)
			if err != nil {
				t.Fatalf("read %s: %v", rel, err)
			}
			for _, line := range strings.Split(string(body), "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "#") {
					continue
				}
				if m := storageClassNameValue.FindStringSubmatch(line); m != nil && strings.HasPrefix(m[1], blobDriverClassPrefix) {
					pinning[m[1]] = appendUnique(pinning[m[1]], rel)
				}
			}
		}
	}
	if len(pinning) == 0 {
		t.Logf("no instance overlay names an %s* storage class; the blob-driver coupling is inert. Examined: %v",
			blobDriverClassPrefix, examined)
		return
	}

	script, err := os.ReadFile(provisionScriptRel)
	if err != nil {
		t.Fatalf("read %s: %v", provisionScriptRel, err)
	}
	var onCreate, onUpdate, updatePrompts bool
	inEnsureCluster := false
	for _, line := range strings.Split(string(script), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "function ") {
			inEnsureCluster = strings.HasPrefix(trimmed, "function ensure_cluster()")
		}
		if strings.HasPrefix(trimmed, "#") || !strings.Contains(trimmed, "--enable-blob-driver") {
			continue
		}
		switch {
		case strings.Contains(trimmed, "az aks update"):
			onUpdate = true
			// az asks "Please make sure there is no open-source Blob CSI
			// driver installed before enabling" on an UPDATE (never on a
			// create) unless told --yes, and a capability script never
			// prompts: with stdin closed the call fails instead.
			updatePrompts = !strings.Contains(trimmed, "--yes")
		case inEnsureCluster:
			onCreate = true
		}
	}
	if !onCreate {
		t.Errorf("%v name %v, but scripts/deploy/azure-provision.sh's ensure_cluster creates the AKS cluster "+
			"without --enable-blob-driver. The class exists only on a cluster running the Blob CSI driver, so "+
			"the pipelines cache claim would sit Pending naming a class that does not exist.",
			flatten(pinning), keys(pinning))
	}
	if !onUpdate {
		t.Errorf("%v name %v, but scripts/deploy/azure-provision.sh has no `az aks update ... --enable-blob-driver` "+
			"for a cluster that already exists. A re-run against a cluster created before the driver was "+
			"required would report converged while the cache class is still missing.",
			flatten(pinning), keys(pinning))
	}
	if updatePrompts {
		t.Error("scripts/deploy/azure-provision.sh enables the Blob CSI driver with `az aks update` but without " +
			"--yes; az prompts on that update, and a capability script must never prompt (with stdin closed " +
			"the call fails instead)")
	}

	// The reason beside the decision, as for the zonality coupling.
	for _, rel := range flatten(pinning) {
		body, err := os.ReadFile(rel)
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if !strings.Contains(string(body), "azure-provision.sh") || !strings.Contains(string(body), "enable-blob-driver") {
			t.Errorf("%s names an %s* class without naming the other half of the coupling. Point at "+
				"scripts/deploy/azure-provision.sh --enable-blob-driver, so a reader changing the class knows "+
				"which file has to agree with it.", rel, blobDriverClassPrefix)
		}
	}
}

// capParamDefault reads the default a capability script gives one parameter,
// from the `NAME="$(cap_param <param> "<default>")"` assignment form every
// script in scripts/ uses. Returns ok=false when the parameter is not declared
// at all, which is a different failure from declaring it with an empty default.
func capParamDefault(script, param string) (string, bool) {
	re := regexp.MustCompile(`cap_param\s+` + regexp.QuoteMeta(param) + `\s+"([^"]*)"`)
	m := re.FindStringSubmatch(script)
	if m == nil {
		return "", false
	}
	return m[1], true
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func appendUnique(dst []string, s string) []string {
	if contains(dst, s) {
		return dst
	}
	return append(dst, s)
}

func keys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func flatten(m map[string][]string) []string {
	var out []string
	for _, files := range m {
		for _, f := range files {
			out = appendUnique(out, f)
		}
	}
	return out
}
