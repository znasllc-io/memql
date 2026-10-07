package installation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/argocd"
)

// storageEvidence records current identities and capacity, never permission to
// restart a database. Installation preparation must combine it with the other
// native proofs and any required maintenance approval, then reobserve before
// an effect. It contains no credentials or resource bodies.
type storageEvidence struct {
	digest, before, after string
	volumes               []storageObservation
	observed              time.Time
}

type storageObservation struct {
	Resource                    resourceIdentity
	UID, OwnerUID               string
	Requested, Capacity         string
	Desired                     string
	Volume, Class               string
	VolumeUID, VolumeSpecDigest string
}

func (e storageEvidence) String() string {
	return fmt.Sprintf("installation storage volumes=%d digest=%s", len(e.volumes), e.digest)
}

func (e storageEvidence) GoString() string { return e.String() }

// storageBytes accepts the positive integer SI/binary forms used by our
// storage manifests and Kubernetes capacity reports. Other Quantity forms
// refuse until qualified; never approximate capacity with floating point.
var storageQuantity = regexp.MustCompile(`^([1-9][0-9]{0,18})(Ki|Mi|Gi|Ti|Pi|Ei|k|M|G|T|P|E)?$`)

func storageBytes(raw string) (*big.Int, error) {
	m := storageQuantity.FindStringSubmatch(raw)
	if m == nil {
		return nil, errors.New("storage quantity requires a supported positive integer unit")
	}
	value, ok := new(big.Int).SetString(m[1], 10)
	if !ok {
		return nil, errors.New("storage quantity is invalid")
	}
	base, power := int64(1000), int64(0)
	if len(m[2]) == 2 {
		base = 1024
		power = int64(strings.Index("KMGTPE", m[2][:1]) + 1)
	} else if m[2] != "" {
		power = int64(strings.Index("kMGTPE", m[2]) + 1)
	}
	value.Mul(value, new(big.Int).Exp(big.NewInt(base), big.NewInt(power), nil))
	if !value.IsInt64() {
		return nil, errors.New("storage quantity exceeds the supported capacity bound")
	}
	return value, nil
}

// verifyStoragePreservation is read-only and deliberately refuses resizing:
// applying the original rollback revision after an expansion would ask for a
// shrink. That needs a separately qualified maintenance/rollback contract.
func verifyStoragePreservation(ctx context.Context, api argocd.API, before, after argocd.RenderedRevision) (storageEvidence, error) {
	observed := time.Now().UTC()
	if api == nil || before.Digest() == "" || after.Digest() == "" {
		return storageEvidence{}, errors.New("storage verification requires native renders and cluster reads")
	}
	if err := sameRenderScope(before.Spec(), after.Spec()); err != nil {
		return storageEvidence{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	schemas := map[string]map[string]resourceType{}
	old, err := resourceInventory(ctx, api, before, schemas)
	if err != nil {
		return storageEvidence{}, err
	}
	next, err := resourceInventory(ctx, api, after, schemas)
	if err != nil {
		return storageEvidence{}, err
	}
	observations := []storageObservation{}
	for key, current := range next {
		switch key.Group + "/" + key.Kind {
		case "/PersistentVolume":
			return storageEvidence{}, errors.New("persistent volume management needs a qualified maintenance contract")
		case "apps/StatefulSet":
			if claims, present := resourceMap(current.value, "spec")["volumeClaimTemplates"]; present && claims != nil {
				return storageEvidence{}, errors.New("StatefulSet claim templates need a qualified preservation codec")
			}
		case "/PersistentVolumeClaim", "postgresql.cnpg.io/Cluster":
			if _, present := old[key]; !present {
				return storageEvidence{}, errors.New("new persistent storage needs a qualified maintenance contract")
			}
		}
	}
	for key, previous := range old {
		kind := key.Group + "/" + key.Kind
		if kind != "/PersistentVolumeClaim" && kind != "postgresql.cnpg.io/Cluster" {
			if kind == "/PersistentVolume" || (kind == "apps/StatefulSet" && resourceMap(previous.value, "spec")["volumeClaimTemplates"] != nil) {
				return storageEvidence{}, errors.New("persistent workload needs a qualified preservation codec")
			}
			continue
		}
		candidate, present := next[key]
		if !present {
			return storageEvidence{}, fmt.Errorf("protected storage %s is removed", key)
		}
		live, err := readStorageObject(ctx, api, previous)
		if err != nil {
			return storageEvidence{}, err
		}
		if kind == "/PersistentVolumeClaim" {
			if !sameJSON(resourceMap(previous.value, "spec"), resourceMap(candidate.value, "spec")) {
				return storageEvidence{}, errors.New("persistent claim changes need a qualified maintenance contract")
			}
			size := claimRequest(candidate.value)
			if err := unchangedClaimIdentity(candidate.value, live); err != nil {
				return storageEvidence{}, err
			}
			observation, err := observeClaim(ctx, api, live, key, "", size)
			if err != nil {
				return storageEvidence{}, err
			}
			observations = append(observations, observation)
			continue
		}
		found, err := observeDatabaseClaims(ctx, api, previous, candidate, live)
		if err != nil {
			return storageEvidence{}, err
		}
		observations = append(observations, found...)
	}
	sort.Slice(observations, func(i, j int) bool { return observations[i].Resource.String() < observations[j].Resource.String() })
	for i := 1; i < len(observations); i++ {
		if observations[i-1].Resource == observations[i].Resource {
			return storageEvidence{}, errors.New("persistent claim has overlapping installation ownership")
		}
	}
	body, _ := json.Marshal(struct {
		Before, After                   string
		BeforeResources, AfterResources []resourceAddress
		Volumes                         []storageObservation
	}{before.Digest(), after.Digest(), inventoryAddresses(old), inventoryAddresses(next), observations})
	return storageEvidence{digest: "memql-id:" + string(id.NewUntracked().FromString("installation-storage-evidence-v1:"+string(body))), before: before.Digest(), after: after.Digest(), volumes: observations, observed: observed}, nil
}

func sameJSON(a, b any) bool {
	x, ex := json.Marshal(a)
	y, ey := json.Marshal(b)
	return ex == nil && ey == nil && bytes.Equal(x, y)
}

func readStorageObject(ctx context.Context, api argocd.API, object resourceObject) (map[string]any, error) {
	body, err := api.Do(ctx, http.MethodGet, object.apiPath, "", nil)
	if err != nil {
		return nil, errors.New("protected storage could not be read")
	}
	live, err := resourceJSON(body)
	if err != nil || resourceText(live, "apiVersion") != object.version || resourceText(live, "kind") != object.identity.Kind || validLiveStorage(live, object.identity) != nil {
		return nil, errors.New("protected storage identity is absent, changed or deleting")
	}
	return live, nil
}

func validLiveStorage(live map[string]any, key resourceIdentity) error {
	meta := resourceMap(live, "metadata")
	if resourceText(meta, "name") != key.Name || resourceText(meta, "namespace") != key.Namespace ||
		!identifier.MatchString(resourceText(meta, "uid")) || resourceText(meta, "resourceVersion") == "" || meta["deletionTimestamp"] != nil {
		return errors.New("protected storage identity is absent, changed or deleting")
	}
	return nil
}

func claimRequest(claim map[string]any) string {
	return resourceText(resourceMap(resourceMap(resourceMap(claim, "spec"), "resources"), "requests"), "storage")
}

func unchangedClaimIdentity(candidate, live map[string]any) error {
	wanted, current := resourceMap(candidate, "spec"), resourceMap(live, "spec")
	for _, field := range []string{"accessModes", "storageClassName", "volumeName", "volumeMode", "selector", "dataSource", "dataSourceRef"} {
		if value, present := wanted[field]; present && !sameJSON(value, current[field]) {
			return errors.New("persistent claim identity differs from live storage")
		}
	}
	return nil
}

func observeClaim(ctx context.Context, api argocd.API, live map[string]any, key resourceIdentity, ownerUID, desired string) (storageObservation, error) {
	if err := validLiveStorage(live, key); err != nil {
		return storageObservation{}, err
	}
	status := resourceMap(live, "status")
	if resourceText(live, "apiVersion") != "v1" || resourceText(live, "kind") != "PersistentVolumeClaim" || resourceText(status, "phase") != "Bound" {
		return storageObservation{}, errors.New("protected claim is not a bound volume")
	}
	requested, capacity := claimRequest(live), resourceText(resourceMap(status, "capacity"), "storage")
	want, err := storageBytes(desired)
	if err != nil {
		return storageObservation{}, err
	}
	for _, raw := range []string{requested, capacity} {
		current, err := storageBytes(raw)
		if err != nil || want.Cmp(current) != 0 {
			return storageObservation{}, fmt.Errorf("desired storage for %s differs from live request or capacity, or cannot be verified", key)
		}
	}
	meta, spec := resourceMap(live, "metadata"), resourceMap(live, "spec")
	volume, class := resourceText(spec, "volumeName"), resourceText(spec, "storageClassName")
	if !kubernetesSegment.MatchString(volume) {
		return storageObservation{}, errors.New("protected claim has no bound volume identity")
	}
	pv, err := readStorageObject(ctx, api, resourceObject{identity: resourceIdentity{"", "PersistentVolume", "", volume}, version: "v1", apiPath: "api/v1/persistentvolumes/" + volume})
	if err != nil {
		return storageObservation{}, err
	}
	pvSpec := resourceMap(pv, "spec")
	volumeCapacity, err := storageBytes(resourceText(resourceMap(pvSpec, "capacity"), "storage"))
	if err != nil || volumeCapacity.Cmp(want) != 0 {
		return storageObservation{}, errors.New("persistent volume capacity differs from the protected claim")
	}
	claim := resourceMap(pvSpec, "claimRef")
	if resourceText(resourceMap(pv, "status"), "phase") != "Bound" || resourceText(claim, "kind") != "PersistentVolumeClaim" ||
		resourceText(claim, "namespace") != key.Namespace || resourceText(claim, "name") != key.Name || resourceText(claim, "uid") != resourceText(meta, "uid") || resourceText(pvSpec, "storageClassName") != class {
		return storageObservation{}, errors.New("persistent volume binding differs from the protected claim")
	}
	pvBody, _ := json.Marshal(pvSpec)
	return storageObservation{Resource: key, UID: resourceText(meta, "uid"), OwnerUID: ownerUID, Requested: requested, Capacity: capacity, Desired: desired,
		Volume: volume, Class: class, VolumeUID: resourceText(resourceMap(pv, "metadata"), "uid"), VolumeSpecDigest: "memql-id:" + string(id.NewUntracked().FromString("installation-volume-spec-v1:"+string(pvBody)))}, nil
}

func observeDatabaseClaims(ctx context.Context, api argocd.API, before, after resourceObject, live map[string]any) ([]storageObservation, error) {
	previous, candidate, current := resourceMap(before.value, "spec"), resourceMap(after.value, "spec"), resourceMap(live, "spec")
	count, err := databaseInstances(current)
	if err != nil {
		return nil, err
	}
	for _, spec := range []map[string]any{previous, candidate} {
		declared, err := databaseInstances(spec)
		if err != nil || declared != count {
			return nil, errors.New("database replica changes need a qualified maintenance contract")
		}
	}
	for _, field := range []string{"storage", "walStorage", "tablespaces", "bootstrap", "externalClusters"} {
		if !sameJSON(previous[field], candidate[field]) {
			return nil, errors.New("database storage or bootstrap changes need a qualified maintenance contract")
		}
	}
	if candidate["tablespaces"] != nil || current["tablespaces"] != nil {
		return nil, errors.New("database tablespaces need a qualified preservation codec")
	}
	sizes := map[string]string{}
	classes := map[string]string{}
	for _, pair := range [][2]string{{"storage", "PG_DATA"}, {"walStorage", "PG_WAL"}} {
		configured, actual := resourceMap(candidate, pair[0]), resourceMap(current, pair[0])
		wanted, err := storageBytes(resourceText(configured, "size"))
		if err != nil {
			return nil, err
		}
		actualSize, err := storageBytes(resourceText(actual, "size"))
		if err != nil || wanted.Cmp(actualSize) != 0 || resourceText(configured, "storageClass") != resourceText(actual, "storageClass") {
			return nil, errors.New("database storage differs from live size or storage class")
		}
		sizes[pair[1]] = resourceText(configured, "size")
		classes[pair[1]] = resourceText(configured, "storageClass")
	}
	uid := resourceText(resourceMap(live, "metadata"), "uid")
	// Inspect the whole bounded namespace inventory: a removed label must not
	// conceal a claim still owned by this database.
	path := "api/v1/namespaces/" + after.identity.Namespace + "/persistentvolumeclaims?limit=256"
	body, err := api.Do(ctx, http.MethodGet, path, "", nil)
	if err != nil {
		return nil, errors.New("database volume inventory could not be read")
	}
	list, err := resourceJSON(body)
	if err != nil || resourceText(list, "kind") != "PersistentVolumeClaimList" || resourceText(list, "apiVersion") != "v1" || resourceText(resourceMap(list, "metadata"), "continue") != "" {
		return nil, errors.New("database volume inventory is incomplete")
	}
	items, ok := list["items"].([]any)
	if !ok || len(items) == 0 || len(items) > 256 {
		return nil, errors.New("database volume inventory exceeds its bound or is empty")
	}
	seen := map[string]bool{}
	roles := map[string]int{}
	out := []storageObservation{}
	for _, raw := range items {
		claim, ok := raw.(map[string]any)
		meta := resourceMap(claim, "metadata")
		labels := resourceMap(meta, "labels")
		role, name := resourceText(labels, "cnpg.io/pvcRole"), resourceText(meta, "name")
		if !ok || !kubernetesSegment.MatchString(name) || seen[name] {
			return nil, errors.New("database volume inventory is malformed")
		}
		// Kubernetes typed list items omit TypeMeta on the wire. The verified
		// v1 PVC collection establishes it; contradictory explicit fields do
		// not get normalized into valid claims.
		for field, expected := range map[string]string{"apiVersion": "v1", "kind": "PersistentVolumeClaim"} {
			if actual, present := claim[field]; present && actual != expected {
				return nil, errors.New("database volume inventory has inconsistent resource types")
			}
			claim[field] = expected
		}
		seen[name] = true
		owned := databaseOwnsClaim(meta, after.identity.Name, uid)
		if !owned && resourceText(labels, "cnpg.io/cluster") != after.identity.Name && !mentionsDatabaseOwner(meta, after.identity.Name, uid) {
			continue
		}
		if sizes[role] == "" || resourceText(labels, "cnpg.io/cluster") != after.identity.Name || !owned {
			return nil, errors.New("database volume ownership is missing or ambiguous")
		}
		if class := classes[role]; class != "" && class != resourceText(resourceMap(claim, "spec"), "storageClassName") {
			return nil, errors.New("database volume storage class differs from the configured class")
		}
		roles[role]++
		observation, err := observeClaim(ctx, api, claim, resourceIdentity{"", "PersistentVolumeClaim", after.identity.Namespace, name}, uid, sizes[role])
		if err != nil {
			return nil, err
		}
		out = append(out, observation)
	}
	if int64(roles["PG_DATA"]) < count || int64(roles["PG_WAL"]) < count {
		return nil, errors.New("database data and WAL volumes must both be observed")
	}
	return out, nil
}

func databaseInstances(spec map[string]any) (int64, error) {
	instances, ok := spec["instances"].(json.Number)
	count, err := instances.Int64()
	if !ok || err != nil || count < 1 || count > 256 {
		return 0, errors.New("database instance count is absent or invalid")
	}
	return count, nil
}

func mentionsDatabaseOwner(meta map[string]any, name, uid string) bool {
	owners, _ := meta["ownerReferences"].([]any)
	for _, raw := range owners {
		owner, _ := raw.(map[string]any)
		if resourceText(owner, "uid") == uid || (resourceText(owner, "kind") == "Cluster" && resourceText(owner, "name") == name) {
			return true
		}
	}
	return false
}

func databaseOwnsClaim(meta map[string]any, name, uid string) bool {
	owners, ok := meta["ownerReferences"].([]any)
	if !ok || len(owners) != 1 {
		return false
	}
	owner, ok := owners[0].(map[string]any)
	return ok && resourceText(owner, "apiVersion") == "postgresql.cnpg.io/v1" && resourceText(owner, "kind") == "Cluster" &&
		resourceText(owner, "name") == name && resourceText(owner, "uid") == uid && owner["controller"] == true
}
