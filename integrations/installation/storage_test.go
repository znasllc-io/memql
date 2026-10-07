package installation

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/integrations/argocd"
)

func storageFixture(t *testing.T) (*inventoryAPI, argocd.RenderedRevision, argocd.RenderedRevision) {
	t.Helper()
	database := `{"apiVersion":"postgresql.cnpg.io/v1","kind":"Cluster","metadata":{"name":"db","namespace":"memql"},"spec":{"imageName":"` + oldImage + `","instances":1,"storage":{"size":"10Gi"},"walStorage":{"size":"2Gi"}}}`
	before := renderInventoryFixture(t, strings.Repeat("a", 40), append(resourceManifests(oldImage), database))
	after := renderInventoryFixture(t, strings.Repeat("b", 40), append(resourceManifests(nextImage), database))
	api := resourceAPIFixture()
	api.response["apis/postgresql.cnpg.io/v1"] = `{"groupVersion":"postgresql.cnpg.io/v1","resources":[{"name":"clusters","kind":"Cluster","namespaced":true}]}`
	api.response["apis/postgresql.cnpg.io/v1/namespaces/memql/clusters/db"] = strings.Replace(database, `"name":"db","namespace":"memql"`, `"name":"db","namespace":"memql","uid":"db-uid","resourceVersion":"10"`, 1)
	claims := []map[string]any{}
	for _, row := range [][3]string{{"db-1", "PG_DATA", "10Gi"}, {"db-1-wal", "PG_WAL", "2Gi"}} {
		claim := map[string]any{
			"metadata": map[string]any{"name": row[0], "namespace": "memql", "uid": row[0] + "-uid", "resourceVersion": "20", "labels": map[string]any{"cnpg.io/cluster": "db", "cnpg.io/pvcRole": row[1]},
				"ownerReferences": []any{map[string]any{"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster", "name": "db", "uid": "db-uid", "controller": true}}},
			"spec":   map[string]any{"volumeName": row[0] + "-pv", "storageClassName": "local-path", "resources": map[string]any{"requests": map[string]any{"storage": row[2]}}},
			"status": map[string]any{"phase": "Bound", "capacity": map[string]any{"storage": row[2]}},
		}
		claims = append(claims, claim)
		pv := map[string]any{
			"apiVersion": "v1", "kind": "PersistentVolume",
			"metadata": map[string]any{"name": row[0] + "-pv", "uid": row[0] + "-pv-uid", "resourceVersion": "30"},
			"spec": map[string]any{"storageClassName": "local-path", "capacity": map[string]any{"storage": row[2]}, "local": map[string]any{"path": "/private-storage/" + row[0]},
				"claimRef": map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "namespace": "memql", "name": row[0], "uid": row[0] + "-uid"}},
			"status": map[string]any{"phase": "Bound"},
		}
		body, err := json.Marshal(pv)
		require.NoError(t, err)
		api.response["api/v1/persistentvolumes/"+row[0]+"-pv"] = string(body)
	}
	body, err := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaimList", "metadata": map[string]any{"resourceVersion": "40"}, "items": claims})
	require.NoError(t, err)
	api.response[databaseClaimsPath] = string(body)
	return api, before, after
}

const databaseClaimsPath = "api/v1/namespaces/memql/persistentvolumeclaims?limit=256"

func editStorageResponse(t *testing.T, api *inventoryAPI, path string, change func(map[string]any)) {
	t.Helper()
	value, err := resourceJSON([]byte(api.response[path]))
	require.NoError(t, err)
	change(value)
	body, err := json.Marshal(value)
	require.NoError(t, err)
	api.response[path] = string(body)
}

func TestStorageEvidenceReadsOwnedLiveClaimsAndBoundVolumes(t *testing.T) {
	api, before, after := storageFixture(t)
	evidence, err := verifyStoragePreservation(context.Background(), api, before, after)
	require.NoError(t, err)
	require.Len(t, evidence.volumes, 2)
	require.NotEmpty(t, evidence.digest)
	require.Equal(t, before.Digest(), evidence.before)
	require.Equal(t, after.Digest(), evidence.after)
	require.NotContains(t, evidence.String(), "/private-storage/")
	require.NotContains(t, evidence.GoString(), "private-fixture-value")
	other, _, _ := storageFixture(t)
	again, err := verifyStoragePreservation(context.Background(), other, before, after)
	require.NoError(t, err)
	require.Equal(t, evidence, again, "independent replicas must agree without cached ownership")
	editStorageResponse(t, other, "api/v1/persistentvolumes/db-1-pv", func(pv map[string]any) {
		resourceMap(pv, "metadata")["uid"] = "replacement-pv-uid"
	})
	replaced, err := verifyStoragePreservation(context.Background(), other, before, after)
	require.NoError(t, err)
	require.NotEqual(t, evidence.digest, replaced.digest, "replacing the backing volume invalidates prior evidence")
}

func TestStorageRejectsLargerLiveVolumesAndIncompleteOwnership(t *testing.T) {
	for _, fault := range []string{"larger request", "larger capacity", "pending expansion", "smaller live request", "wrong owner", "removed label", "wrong role", "missing WAL", "truncated list", "duplicate claim", "unbound claim", "deleting claim", "wrong namespace", "wrong kind", "wrong API version", "wrong volume binding", "missing backing volume", "larger live database", "missing replica volumes"} {
		t.Run(fault, func(t *testing.T) {
			api, before, after := storageFixture(t)
			switch fault {
			case "wrong volume binding":
				editStorageResponse(t, api, "api/v1/persistentvolumes/db-1-pv", func(pv map[string]any) { resourceMap(resourceMap(pv, "spec"), "claimRef")["uid"] = "another-claim-uid" })
			case "missing backing volume":
				delete(api.response, "api/v1/persistentvolumes/db-1-pv")
			case "larger live database", "missing replica volumes":
				editStorageResponse(t, api, "apis/postgresql.cnpg.io/v1/namespaces/memql/clusters/db", func(db map[string]any) {
					if fault == "larger live database" {
						resourceMap(resourceMap(db, "spec"), "storage")["size"] = "20Gi"
					} else {
						resourceMap(db, "spec")["instances"] = 2
					}
				})
			default:
				editStorageResponse(t, api, databaseClaimsPath, func(list map[string]any) {
					items := list["items"].([]any)
					claim := items[0].(map[string]any)
					meta := resourceMap(claim, "metadata")
					switch fault {
					case "larger request":
						resourceMap(resourceMap(resourceMap(claim, "spec"), "resources"), "requests")["storage"] = "20Gi"
					case "larger capacity":
						resourceMap(resourceMap(claim, "status"), "capacity")["storage"] = "20Gi"
					case "pending expansion":
						resourceMap(resourceMap(claim, "status"), "capacity")["storage"] = "8Gi"
					case "smaller live request":
						resourceMap(resourceMap(resourceMap(claim, "spec"), "resources"), "requests")["storage"] = "8Gi"
					case "wrong owner":
						meta["ownerReferences"].([]any)[0].(map[string]any)["uid"] = "replacement-db-uid"
					case "removed label":
						delete(resourceMap(meta, "labels"), "cnpg.io/cluster")
					case "wrong role":
						resourceMap(meta, "labels")["cnpg.io/pvcRole"] = "PG_UNKNOWN"
					case "missing WAL":
						list["items"] = items[:1]
					case "truncated list":
						resourceMap(list, "metadata")["continue"] = "another-page"
					case "duplicate claim":
						list["items"] = append(items, claim)
					case "unbound claim":
						resourceMap(claim, "status")["phase"] = "Pending"
					case "deleting claim":
						meta["deletionTimestamp"] = "2026-10-07T09:00:00Z"
					case "wrong namespace":
						meta["namespace"] = "another"
					case "wrong kind":
						claim["kind"] = "Secret"
					case "wrong API version":
						claim["apiVersion"] = "other/v1"
					}
				})
			}
			evidence, err := verifyStoragePreservation(context.Background(), api, before, after)
			require.Error(t, err)
			require.Empty(t, evidence.digest)
		})
	}
}

func TestStorageRefusesRenderedRemovalResizeAndUnsupportedPersistentControllers(t *testing.T) {
	for _, fault := range []string{"remove database", "resize data", "remove WAL", "change bootstrap", "new claim", "StatefulSet claims", "persistent volume"} {
		t.Run(fault, func(t *testing.T) {
			api, before, after := storageFixture(t)
			manifests := []string{}
			for _, body := range after.Resources() {
				value, err := resourceJSON(body)
				require.NoError(t, err)
				if resourceText(value, "kind") == "Cluster" {
					spec := resourceMap(value, "spec")
					switch fault {
					case "remove database":
						continue
					case "resize data":
						resourceMap(spec, "storage")["size"] = "20Gi"
					case "remove WAL":
						delete(spec, "walStorage")
					case "change bootstrap":
						spec["bootstrap"] = map[string]any{"recovery": map[string]any{"source": "another"}}
					}
				}
				encoded, err := json.Marshal(value)
				require.NoError(t, err)
				manifests = append(manifests, string(encoded))
			}
			switch fault {
			case "new claim":
				manifests = append(manifests, `{"apiVersion":"v1","kind":"PersistentVolumeClaim","metadata":{"name":"new-claim"},"spec":{"resources":{"requests":{"storage":"10Gi"}}}}`)
				api.response["api/v1"] = strings.Replace(api.response["api/v1"], `"resources":[`, `"resources":[{"name":"persistentvolumeclaims","kind":"PersistentVolumeClaim","namespaced":true},`, 1)
			case "StatefulSet claims":
				manifests = append(manifests, `{"apiVersion":"apps/v1","kind":"StatefulSet","metadata":{"name":"new-stateful"},"spec":{"volumeClaimTemplates":[]}}`)
				api.response["apis/apps/v1"] = strings.Replace(api.response["apis/apps/v1"], `"resources":[`, `"resources":[{"name":"statefulsets","kind":"StatefulSet","namespaced":true},`, 1)
			case "persistent volume":
				manifests = append(manifests, `{"apiVersion":"v1","kind":"PersistentVolume","metadata":{"name":"new-volume"}}`)
				api.response["api/v1"] = strings.Replace(api.response["api/v1"], `"resources":[`, `"resources":[{"name":"persistentvolumes","kind":"PersistentVolume","namespaced":false},`, 1)
			}
			after = renderInventoryFixture(t, strings.Repeat("b", 40), manifests)
			evidence, err := verifyStoragePreservation(context.Background(), api, before, after)
			require.Error(t, err)
			require.Empty(t, evidence.digest)
		})
	}
}

func TestStorageQuantityComparisonIsExactAndBounded(t *testing.T) {
	for input, expected := range map[string]string{"10Gi": "10737418240", "10485760Ki": "10737418240", "100G": "100000000000", "9223372036854775807": "9223372036854775807", "8Pi": "9007199254740992"} {
		value, err := storageBytes(input)
		require.NoError(t, err)
		require.Equal(t, expected, value.String())
	}
	for _, input := range []string{"", "0", "-1Gi", "1.5Gi", "100m", "1e9", "8Ei", "10Gi ", "01Gi", "9" + strings.Repeat("0", 1000)} {
		_, err := storageBytes(input)
		require.Error(t, err, input)
	}
}

func TestDatabaseClaimsMustUseTheConfiguredStorageClass(t *testing.T) {
	api, before, after := storageFixture(t)
	withClass := func(rendered argocd.RenderedRevision, revision string) argocd.RenderedRevision {
		manifests := []string{}
		for _, body := range rendered.Resources() {
			value, err := resourceJSON(body)
			require.NoError(t, err)
			if resourceText(value, "kind") == "Cluster" {
				resourceMap(resourceMap(value, "spec"), "storage")["storageClass"] = "expected-class"
			}
			encoded, err := json.Marshal(value)
			require.NoError(t, err)
			manifests = append(manifests, string(encoded))
		}
		return renderInventoryFixture(t, revision, manifests)
	}
	before, after = withClass(before, strings.Repeat("a", 40)), withClass(after, strings.Repeat("b", 40))
	editStorageResponse(t, api, "apis/postgresql.cnpg.io/v1/namespaces/memql/clusters/db", func(db map[string]any) {
		resourceMap(resourceMap(db, "spec"), "storage")["storageClass"] = "expected-class"
	})
	evidence, err := verifyStoragePreservation(context.Background(), api, before, after)
	require.ErrorContains(t, err, "volume storage class differs")
	require.Empty(t, evidence.digest)
}

func TestStorageRequiresMatchingReplicaCountsAndBackingCapacity(t *testing.T) {
	for _, where := range []string{"candidate", "rollback"} {
		for _, count := range []any{json.Number("0"), json.Number("2"), json.Number("1.5"), "1", nil} {
			t.Run(where+"/"+fmt.Sprint(count), func(t *testing.T) {
				api, before, after := storageFixture(t)
				original, revision := after, strings.Repeat("b", 40)
				if where == "rollback" {
					original, revision = before, strings.Repeat("a", 40)
				}
				manifests := []string{}
				for _, body := range original.Resources() {
					value, err := resourceJSON(body)
					require.NoError(t, err)
					if resourceText(value, "kind") == "Cluster" {
						resourceMap(value, "spec")["instances"] = count
					}
					encoded, err := json.Marshal(value)
					require.NoError(t, err)
					manifests = append(manifests, string(encoded))
				}
				changed := renderInventoryFixture(t, revision, manifests)
				if where == "rollback" {
					before = changed
				} else {
					after = changed
				}
				evidence, err := verifyStoragePreservation(context.Background(), api, before, after)
				require.Error(t, err)
				require.Empty(t, evidence.digest)
			})
		}
	}
	for _, capacity := range []string{"1Gi", "20Gi", "", "invalid"} {
		t.Run("backing capacity/"+capacity, func(t *testing.T) {
			api, before, after := storageFixture(t)
			editStorageResponse(t, api, "api/v1/persistentvolumes/db-1-pv", func(pv map[string]any) {
				resourceMap(resourceMap(pv, "spec"), "capacity")["storage"] = capacity
			})
			evidence, err := verifyStoragePreservation(context.Background(), api, before, after)
			require.ErrorContains(t, err, "volume capacity differs")
			require.Empty(t, evidence.digest)
		})
	}
}
