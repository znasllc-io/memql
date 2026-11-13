package installation

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/integrations/argocd"
)

const cnpgBackupClusterPath = "apis/postgresql.cnpg.io/v1/namespaces/memql/clusters/workload-0"
const cnpgBackupPodsPath = "api/v1/namespaces/memql/pods?limit=256"

// This is the real declared CNPG-I layout, not a proof supplied to the checker:
// the Cluster is rendered again and admitted through the OCI/resource verifier.
func cnpgBackupWorkloadFixture(t *testing.T) *workloadFixture {
	t.Helper()
	plugins := []any{map[string]any{"name": "barman-cloud.cloudnative-pg.io", "isWALArchiver": true, "parameters": map[string]any{"barmanObjectName": "db-backup"}}}
	return cnpgBackupWorkloadWithPlugins(t, plugins)
}

func cnpgBackupWorkloadWithPlugins(t *testing.T, plugins any) *workloadFixture {
	t.Helper()
	f := newWorkloadFixture(t, "Cluster")
	withPlugin := func(before argocd.RenderInventory, revision string) argocd.RenderedRevision {
		manifests := []string{}
		for _, body := range before.Resources() {
			object := workloadObject(t, string(body))
			resourceMap(object, "spec")["plugins"] = plugins
			manifests = append(manifests, workloadJSON(t, object))
		}
		return renderInventoryFixture(t, revision, manifests)
	}
	f.artifacts.before = withPlugin(f.artifacts.before, strings.Repeat("a", 40))
	f.artifacts.after = withPlugin(f.artifacts.after, strings.Repeat("b", 40))
	cluster := workloadObject(t, f.api.response[cnpgBackupClusterPath])
	resourceMap(cluster, "spec")["plugins"] = plugins
	f.api.response[cnpgBackupClusterPath] = workloadJSON(t, cluster)
	list := workloadObject(t, f.api.response[cnpgBackupPodsPath])
	pod := list["items"].([]any)[0].(map[string]any)
	resourceMap(pod, "spec")["initContainers"] = []any{
		map[string]any{"name": "bootstrap-controller", "image": "ghcr.io/cloudnative-pg/cloudnative-pg:1.30.0", "command": []any{"/manager", "bootstrap", "/controller/manager", "--log-level=info"}},
		map[string]any{"name": "plugin-barman-cloud", "image": "ghcr.io/cloudnative-pg/plugin-barman-cloud-sidecar:v0.14.0", "args": []any{"instance"}, "restartPolicy": "Always"},
	}
	resourceMap(pod, "status")["initContainerStatuses"] = []any{
		map[string]any{"name": "bootstrap-controller", "imageID": "ghcr.io/cloudnative-pg/cloudnative-pg@sha256:a2701eb97cdd2a34b1fdb2cb51987f544b706e40bec72ae7146cd8580efefebb", "state": map[string]any{"terminated": map[string]any{"exitCode": json.Number("0")}}},
		map[string]any{"name": "plugin-barman-cloud", "imageID": "ghcr.io/cloudnative-pg/plugin-barman-cloud-sidecar@sha256:9880817c285c7afa4d195da2145064d21907405489ed6ec39abe59b1feb558a4", "ready": true, "started": true, "state": map[string]any{"running": map[string]any{}}},
	}
	f.api.response[cnpgBackupPodsPath] = workloadJSON(t, list)
	admission, err := newArtifactAdmission(context.Background(), f.api, f.artifacts.config, f.artifacts.old, f.artifacts.next, f.artifacts.before, f.artifacts.after)
	require.NoError(t, err)
	w, err := loadArtifactWorkflow(strings.Repeat("d", 40))
	require.NoError(t, err)
	f.evidence, err = w.run(captureOperator(auth.RoleOwner, "operator"), admission, "operator")
	require.NoError(t, err)
	return f
}

func TestWorkloadsObserveDeclaredCNPGBackupSidecar(t *testing.T) {
	for _, variant := range []string{"index", "arm64 manifest", "amd64 manifest", "status order", "enabled default", "qualified image pins"} {
		t.Run(variant, func(t *testing.T) {
			f := cnpgBackupWorkloadFixture(t)
			s := f.scope(t)
			list := workloadObject(t, f.api.response[cnpgBackupPodsPath])
			pod := list["items"].([]any)[0].(map[string]any)
			states := resourceMap(pod, "status")["initContainerStatuses"].([]any)
			switch variant {
			case "arm64 manifest", "amd64 manifest":
				bootstrap, sidecar := "6c7926147fd23a053dea6605d61c013d43bfd411be3532e7500ef2d2b68bb98c", "dc33620cb638a3acb73158a47b2ab41914352d688eb0494f3feaed685c945c1b"
				if variant == "amd64 manifest" {
					bootstrap, sidecar = "091d306935cfdf646debfe78010d59ebfb572150eb6eb922b0203873c0c68841", "b1f980db3e97942eafd7a063fb99cda03300e651f9fe9561ff1bcbe481c157c8"
					// Exercise the infrastructure platform fence and Node check;
					// the generic engine OCI fixture was admitted for arm64.
					s.platform = "linux/amd64"
					node := workloadObject(t, f.api.response["api/v1/nodes/worker"])
					resourceMap(resourceMap(node, "status"), "nodeInfo")["architecture"] = "amd64"
					f.api.response["api/v1/nodes/worker"] = workloadJSON(t, node)
				}
				states[0].(map[string]any)["imageID"] = "ghcr.io/cloudnative-pg/cloudnative-pg@sha256:" + bootstrap
				states[1].(map[string]any)["imageID"] = "docker-pullable://ghcr.io/cloudnative-pg/plugin-barman-cloud-sidecar@sha256:" + sidecar
			case "status order":
				states[0], states[1] = states[1], states[0]
			case "enabled default":
				cluster := workloadObject(t, f.api.response[cnpgBackupClusterPath])
				resourceMap(cluster, "spec")["plugins"].([]any)[0].(map[string]any)["enabled"] = true
				f.api.response[cnpgBackupClusterPath] = workloadJSON(t, cluster)
			case "qualified image pins":
				containers := resourceMap(pod, "spec")["initContainers"].([]any)
				containers[0].(map[string]any)["image"] = "ghcr.io/cloudnative-pg/cloudnative-pg:1.30.0@sha256:a2701eb97cdd2a34b1fdb2cb51987f544b706e40bec72ae7146cd8580efefebb"
				containers[1].(map[string]any)["image"] = "ghcr.io/cloudnative-pg/plugin-barman-cloud-sidecar@sha256:9880817c285c7afa4d195da2145064d21907405489ed6ec39abe59b1feb558a4"
			}
			f.api.response[cnpgBackupPodsPath] = workloadJSON(t, list)
			_, err := s.check(context.Background(), s.requirements[0].Key)
			require.NoError(t, err)
		})
	}
}

func TestWorkloadsRefuseUnsupportedAuthoredCNPGPluginProfiles(t *testing.T) {
	for _, fault := range []string{"absent", "malformed list", "unknown", "duplicate", "disabled", "malformed enabled", "non archiver", "no object store"} {
		t.Run(fault, func(t *testing.T) {
			plugin := map[string]any{"name": "barman-cloud.cloudnative-pg.io", "isWALArchiver": true, "parameters": map[string]any{"barmanObjectName": "db-backup"}}
			var plugins any = []any{plugin}
			switch fault {
			case "absent":
				plugins = nil
			case "malformed list":
				plugins = "invalid"
			case "unknown":
				plugin["name"] = "other.example"
			case "duplicate":
				plugins = []any{plugin, plugin}
			case "disabled":
				plugin["enabled"] = false
			case "malformed enabled":
				plugin["enabled"] = "true"
			case "non archiver":
				plugin["isWALArchiver"] = false
			case "no object store":
				delete(plugin, "parameters")
			}
			f := cnpgBackupWorkloadWithPlugins(t, plugins)
			s := f.scope(t)
			_, err := s.check(context.Background(), s.requirements[0].Key)
			require.Error(t, err, "matching authored/live declarations cannot authorize an unknown sidecar profile")
		})
	}
}

func TestWorkloadsRefuseUnqualifiedCNPGBackupSidecars(t *testing.T) {
	for _, fault := range []string{"undeclared", "disabled", "unknown plugin", "duplicate plugin", "not WAL archiver", "missing object store", "missing sidecar", "extra init", "extra normal container", "ephemeral container", "duplicate status", "missing status", "reordered init", "foreign name", "foreign image", "unqualified version", "unverified runtime", "wrong platform", "operator image", "operator runtime", "bootstrap failure", "restart policy", "not ready", "not started", "terminated", "command override", "custom args", "owner", "postgres runtime"} {
		t.Run(fault, func(t *testing.T) {
			f := cnpgBackupWorkloadFixture(t)
			s := f.scope(t)
			cluster := workloadObject(t, f.api.response[cnpgBackupClusterPath])
			list := workloadObject(t, f.api.response[cnpgBackupPodsPath])
			pod := list["items"].([]any)[0].(map[string]any)
			spec, status := resourceMap(pod, "spec"), resourceMap(pod, "status")
			inits, states := spec["initContainers"].([]any), status["initContainerStatuses"].([]any)
			sidecar, state := inits[1].(map[string]any), states[1].(map[string]any)
			plugin := resourceMap(cluster, "spec")["plugins"].([]any)[0].(map[string]any)
			switch fault {
			case "undeclared":
				for key, object := range s.objects {
					object.value = workloadObject(t, workloadJSON(t, object.value))
					delete(resourceMap(object.value, "spec"), "plugins")
					s.objects[key] = object
				}
			case "disabled":
				plugin["enabled"] = false
			case "unknown plugin":
				plugin["name"] = "unknown.example"
			case "duplicate plugin":
				resourceMap(cluster, "spec")["plugins"] = []any{plugin, plugin}
			case "not WAL archiver":
				plugin["isWALArchiver"] = false
			case "missing object store":
				delete(plugin, "parameters")
			case "missing sidecar":
				spec["initContainers"], status["initContainerStatuses"] = inits[:1], states[:1]
			case "extra init":
				spec["initContainers"] = append(inits, map[string]any{"name": "other"})
			case "extra normal container":
				spec["containers"] = append(spec["containers"].([]any), sidecar)
			case "ephemeral container":
				spec["ephemeralContainers"] = []any{sidecar}
			case "duplicate status":
				status["initContainerStatuses"] = []any{states[0], states[0]}
			case "missing status":
				status["initContainerStatuses"] = states[:1]
			case "reordered init":
				inits[0], inits[1] = inits[1], inits[0]
			case "foreign name":
				sidecar["name"] = "other"
			case "foreign image":
				sidecar["image"] = "example.com/plugin:v0.14.0"
			case "unqualified version":
				sidecar["image"] = "ghcr.io/cloudnative-pg/plugin-barman-cloud-sidecar:latest"
			case "unverified runtime":
				state["imageID"] = "ghcr.io/cloudnative-pg/plugin-barman-cloud-sidecar@sha256:" + strings.Repeat("f", 64)
			case "wrong platform":
				state["imageID"] = "ghcr.io/cloudnative-pg/plugin-barman-cloud-sidecar@sha256:b1f980db3e97942eafd7a063fb99cda03300e651f9fe9561ff1bcbe481c157c8"
			case "operator image":
				inits[0].(map[string]any)["image"] = "example.com/operator:1.30.0"
			case "operator runtime":
				states[0].(map[string]any)["imageID"] = "ghcr.io/cloudnative-pg/cloudnative-pg@sha256:" + strings.Repeat("f", 64)
			case "bootstrap failure":
				resourceMap(resourceMap(states[0].(map[string]any), "state"), "terminated")["exitCode"] = json.Number("1")
			case "restart policy":
				delete(sidecar, "restartPolicy")
			case "not ready":
				state["ready"] = false
			case "not started":
				state["started"] = false
			case "terminated":
				state["state"] = map[string]any{"terminated": map[string]any{"exitCode": json.Number("0")}}
			case "command override":
				sidecar["command"] = []any{"/bin/sh"}
			case "custom args":
				sidecar["args"] = []any{"instance", "--unknown"}
			case "owner":
				resourceMap(pod, "metadata")["ownerReferences"].([]any)[0].(map[string]any)["uid"] = "foreign"
			case "postgres runtime":
				status["containerStatuses"].([]any)[0].(map[string]any)["imageID"] = oldImage
			}
			f.api.response[cnpgBackupClusterPath], f.api.response[cnpgBackupPodsPath] = workloadJSON(t, cluster), workloadJSON(t, list)
			_, err := s.check(context.Background(), s.requirements[0].Key)
			require.Error(t, err)
		})
	}
}
