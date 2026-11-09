package installation

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
)

const discoveryArgoPath = "apis/argoproj.io/v1alpha1"

// A complete discovery surface: the receiver's own resources, an unrelated CRD
// group, and the two resource orders observed from the installed Argo CRDs.
// Discovery order varies across reads without changing any resource contract.
func discoveryFixture() *receiverFixture {
	return &receiverFixture{reads: map[string]int{}, objects: map[string]map[string]any{
		"version": {"major": "1", "minor": "33", "gitVersion": "v1.33.5", "gitCommit": "0123456789abcdef", "buildDate": "2026-10-01T00:00:00Z", "goVersion": "go1.24.6", "platform": "linux/amd64"},
		"api":     {"kind": "APIVersions", "versions": []any{"v1"}, "serverAddressByClientCIDRs": []any{map[string]any{"clientCIDR": "0.0.0.0/0", "serverAddress": "10.0.0.1:443"}}},
		"apis": {"kind": "APIGroupList", "apiVersion": "v1", "groups": []any{
			map[string]any{"name": "apps", "versions": []any{map[string]any{"groupVersion": "apps/v1", "version": "v1"}}},
			map[string]any{"name": "discovery.k8s.io", "versions": []any{map[string]any{"groupVersion": "discovery.k8s.io/v1", "version": "v1"}}},
			map[string]any{"name": "argoproj.io", "versions": []any{map[string]any{"groupVersion": "argoproj.io/v1alpha1", "version": "v1alpha1"}}},
			map[string]any{"name": "cert-manager.io", "versions": []any{map[string]any{"groupVersion": "cert-manager.io/v1", "version": "v1"}}},
		}},
		"api/v1": {"apiVersion": "v1", "kind": "APIResourceList", "groupVersion": "v1", "resources": []any{
			map[string]any{"name": "configmaps", "kind": "ConfigMap", "namespaced": true, "verbs": []any{"create", "delete", "get", "list", "patch", "update", "watch"}},
			map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []any{"get", "list", "watch"}},
			map[string]any{"name": "secrets", "kind": "Secret", "namespaced": true, "verbs": []any{"get", "list", "watch"}},
			map[string]any{"name": "services", "kind": "Service", "namespaced": true, "verbs": []any{"get", "list"}},
		}},
		"apis/apps/v1": {"apiVersion": "v1", "kind": "APIResourceList", "groupVersion": "apps/v1", "resources": []any{
			map[string]any{"name": "deployments", "kind": "Deployment", "namespaced": true, "verbs": []any{"get", "list"}},
			map[string]any{"name": "replicasets", "kind": "ReplicaSet", "namespaced": true, "verbs": []any{"get", "list"}},
		}},
		"apis/discovery.k8s.io/v1": {"apiVersion": "v1", "kind": "APIResourceList", "groupVersion": "discovery.k8s.io/v1", "resources": []any{
			map[string]any{"name": "endpointslices", "kind": "EndpointSlice", "namespaced": true, "verbs": []any{"get", "list", "watch"}},
		}},
		discoveryArgoPath: {"apiVersion": "v1", "kind": "APIResourceList", "groupVersion": "argoproj.io/v1alpha1", "resources": []any{
			map[string]any{"name": "applications", "kind": "Application", "namespaced": true, "verbs": []any{"get", "list", "patch", "watch"}},
			map[string]any{"name": "applicationsets", "kind": "ApplicationSet", "namespaced": true, "verbs": []any{"get", "list"}},
			map[string]any{"name": "applicationsets/status", "kind": "ApplicationSet", "namespaced": true, "verbs": []any{"get", "patch"}},
			map[string]any{"name": "appprojects", "kind": "AppProject", "namespaced": true, "verbs": []any{"get", "list"}},
		}},
		"apis/cert-manager.io/v1": {"apiVersion": "v1", "kind": "APIResourceList", "groupVersion": "cert-manager.io/v1", "resources": []any{
			map[string]any{"name": "certificates", "kind": "Certificate", "namespaced": true, "verbs": []any{"get", "list"}},
		}},
	}}
}

func reorderDiscovery(body map[string]any) {
	items := body["resources"].([]any)
	body["resources"] = []any{items[1], items[2], items[0], items[3]}
}

func discoveryDigests(t *testing.T, f *receiverFixture) (receiverDigests, receiverDigests) {
	t.Helper()
	r := newReceiverReads(f)
	before, err := json.Marshal(f.objects[discoveryArgoPath])
	require.NoError(t, err)
	_, _, err = r.discovery(context.Background())
	require.NoError(t, err)
	configuration, err := r.configuration(context.Background())
	require.NoError(t, err)
	invariant, err := r.configurationInvariant()
	require.NoError(t, err)
	after, err := json.Marshal(r.reads[discoveryArgoPath].body)
	require.NoError(t, err)
	require.JSONEq(t, string(before), string(after), "normalization must not mutate authenticated reads")
	return configuration, invariant
}

// Order normalization holds for both saved formats: v1's full inventory and
// v2's discovery contract, within one observation and on recovery.
func TestReceiverDiscoveryResourceOrderIsStableAcrossReadsAndRecovery(t *testing.T) {
	var digest, invariant receiverDigests
	for _, phase := range []string{"unchanged", "during observation", "on recovery"} {
		t.Run(phase, func(t *testing.T) {
			f := discoveryFixture()
			if phase == "on recovery" {
				reorderDiscovery(f.objects[discoveryArgoPath])
			}
			if phase == "during observation" {
				f.before = func(p string, count int) {
					if p == discoveryArgoPath && count == 2 {
						reorderDiscovery(f.objects[p])
					}
				}
			}
			got, stable := discoveryDigests(t, f)
			if phase == "unchanged" {
				digest, invariant = got, stable
				return
			}
			require.Equal(t, digest, got)
			require.Equal(t, invariant, stable)
		})
	}
}

// Within one observation every discovery document stays exact: a read set is
// one snapshot, so any change between its reads refuses that observation.
func TestReceiverDiscoveryStillRefusesResourceContractChanges(t *testing.T) {
	for _, change := range []string{"removed", "added", "duplicate", "kind", "scope", "verbs", "group version", "extra field", "patch release"} {
		t.Run(change, func(t *testing.T) {
			f := discoveryFixture()
			f.before = func(p string, count int) {
				if count != 2 {
					return
				}
				if change == "patch release" && p == "version" {
					f.objects[p]["gitVersion"] = "v1.33.6"
				}
				if p != discoveryArgoPath {
					return
				}
				body := f.objects[p]
				items := body["resources"].([]any)
				first := items[0].(map[string]any)
				switch change {
				case "removed":
					body["resources"] = items[1:]
				case "added":
					body["resources"] = append(items, map[string]any{"name": "new", "kind": "New"})
				case "duplicate":
					body["resources"] = append(items, items[0])
				case "kind":
					first["kind"] = "Other"
				case "scope":
					first["namespaced"] = false
				case "verbs":
					first["verbs"] = []any{"get"}
				case "group version":
					body["groupVersion"] = "argoproj.io/v2"
				case "extra field":
					body["newConfiguration"] = true
				}
			}
			r := newReceiverReads(f)
			_, _, err := r.discovery(context.Background())
			require.NoError(t, err)
			_, err = r.configuration(context.Background())
			require.ErrorContains(t, err, "discovery changed")
		})
	}
}

// Fix 3. Across observations (recovery: Apply, completion and rollback compare
// with preparation), a saved plan binds the Kubernetes minor version and the
// contract of the resources the receiver reads: an AKS patch, a new or upgraded
// CRD and the API server's address no longer strand it. v1, which engines up
// to 0.31.28 saved, binds all of it; a plan saved in v1 keeps that strictness.
func TestReceiverDiscoveryBindsOnlyTheContractTheReceiverUses(t *testing.T) {
	baseConfiguration, baseInvariant := discoveryDigests(t, discoveryFixture())
	argo := func(f *receiverFixture) map[string]any {
		return f.objects[discoveryArgoPath]["resources"].([]any)[0].(map[string]any)
	}
	for _, test := range []struct {
		name     string
		change   func(*receiverFixture)
		accepted bool
	}{
		{"Kubernetes patch release", func(f *receiverFixture) {
			v := f.objects["version"]
			v["gitVersion"], v["gitCommit"], v["buildDate"], v["goVersion"] = "v1.33.6", "fedcba9876543210", "2026-11-08T00:00:00Z", "go1.24.9"
		}, true},
		{"API server address", func(f *receiverFixture) {
			f.objects["api"]["serverAddressByClientCIDRs"] = []any{map[string]any{"clientCIDR": "0.0.0.0/0", "serverAddress": "memql-dns-1a2b3c.hcp.eastus.azmk8s.io:443"}}
		}, true},
		{"unrelated CRD group added", func(f *receiverFixture) {
			apis := f.objects["apis"]
			apis["groups"] = append(apis["groups"].([]any), map[string]any{"name": "kueue.x-k8s.io", "versions": []any{map[string]any{"groupVersion": "kueue.x-k8s.io/v1beta1", "version": "v1beta1"}}})
			f.objects["apis/kueue.x-k8s.io/v1beta1"] = map[string]any{"apiVersion": "v1", "kind": "APIResourceList", "groupVersion": "kueue.x-k8s.io/v1beta1", "resources": []any{map[string]any{"name": "workloads", "kind": "Workload", "namespaced": true, "verbs": []any{"get"}}}}
		}, true},
		{"unrelated CRD upgraded", func(f *receiverFixture) {
			body := f.objects["apis/cert-manager.io/v1"]
			body["resources"] = append(body["resources"].([]any), map[string]any{"name": "certificaterequests", "kind": "CertificateRequest", "namespaced": true, "verbs": []any{"get"}})
		}, true},
		{"unrelated resource in a receiver group", func(f *receiverFixture) {
			body := f.objects[discoveryArgoPath]
			body["resources"].([]any)[1].(map[string]any)["verbs"] = []any{"get", "list", "watch", "patch"}
		}, true},
		{"verbs the receiver does not use", func(f *receiverFixture) {
			argo(f)["verbs"] = []any{"create", "delete", "deletecollection", "get", "list", "patch", "update", "watch"}
		}, true},
		{"storage version and short names", func(f *receiverFixture) {
			argo(f)["storageVersionHash"], argo(f)["shortNames"] = "fresh", []any{"app", "apps"}
		}, true},
		{"Kubernetes minor upgrade", func(f *receiverFixture) { f.objects["version"]["gitVersion"] = "v1.34.1" }, false},
		{"receiver resource rescoped", func(f *receiverFixture) { argo(f)["namespaced"] = false }, false},
		{"receiver resource kind", func(f *receiverFixture) { argo(f)["kind"] = "ApplicationV2" }, false},
		{"receiver verb removed", func(f *receiverFixture) { argo(f)["verbs"] = []any{"get", "list", "watch"} }, false},
		{"receiver resource removed", func(f *receiverFixture) {
			body := f.objects["apis/discovery.k8s.io/v1"]
			body["resources"] = []any{}
		}, false},
		{"receiver group removed", func(f *receiverFixture) {
			apis := f.objects["apis"]
			apis["groups"] = apis["groups"].([]any)[:2]
			delete(f.objects, discoveryArgoPath)
		}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := discoveryFixture()
			test.change(f)
			configuration, invariant := discoveryDigests(t, f)
			require.NotEqual(t, baseInvariant.legacy, invariant.legacy, "v1 binds the whole discovery surface")
			if test.accepted {
				require.Equal(t, baseInvariant.current, invariant.current)
				require.Equal(t, baseConfiguration.current, configuration.current)
				return
			}
			require.NotEqual(t, baseInvariant.current, invariant.current)
			require.NotEqual(t, baseConfiguration.current, configuration.current)
		})
	}
}

// What the plan's own render applies is checked from live discovery at every
// call, by resource evidence rather than the configuration invariant: a CRD
// the render uses that is removed or rescoped refuses Apply, completion and
// rollback through ResourceDiffDigest, while the receiver's configuration,
// which never reads that CRD, is unchanged.
func TestReceiverDiscoveryLeavesTheRenderedAPIsToResourceEvidence(t *testing.T) {
	publication := publishedFixture(t)
	certificate := `{"apiVersion":"cert-manager.io/v1","kind":"Certificate","metadata":{"name":"identity","namespace":"memql"},"spec":{"secretName":"identity-tls"}}`
	before := renderInventoryFixture(t, strings.Repeat("a", 40), append(resourceManifests(oldImage), certificate))
	after := renderInventoryFixture(t, strings.Repeat("b", 40), append(resourceManifests(nextImage), certificate))
	evidenceAPI := func(certificates string) *inventoryAPI {
		api := resourceAPIFixture()
		api.response["apis/cert-manager.io/v1"] = `{"groupVersion":"cert-manager.io/v1","resources":[` + certificates + `]}`
		return api
	}
	served := `{"name":"certificates","kind":"Certificate","namespaced":true}`
	prepared, err := verifyImagesAndDiff(context.Background(), evidenceAPI(served), publication, before, after, "linux/arm64", resourceBindings())
	require.NoError(t, err)
	_, preparedInvariant := discoveryDigests(t, discoveryFixture())
	for _, test := range []struct{ name, certificates string }{
		{"removed", ``},
		{"rescoped", `{"name":"certificates","kind":"Certificate","namespaced":false}`},
		{"renamed", `{"name":"certs","kind":"Certificate","namespaced":true}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := discoveryFixture()
			list := f.objects["apis/cert-manager.io/v1"]
			list["resources"] = []any{}
			if test.certificates != "" {
				var entry map[string]any
				require.NoError(t, json.Unmarshal([]byte(test.certificates), &entry))
				list["resources"] = []any{entry}
			}
			_, invariant := discoveryDigests(t, f)
			require.Equal(t, preparedInvariant.current, invariant.current, "the receiver never reads Certificates")
			fresh, err := verifyImagesAndDiff(context.Background(), evidenceAPI(test.certificates), publication, before, after, "linux/arm64", resourceBindings())
			require.False(t, err == nil && fresh.digest == prepared.digest, "a rendered API that changed cannot reproduce the plan's resource evidence")
		})
	}
}

// The discovery contract leaves the full API list out of the saved digest,
// which is sound only while the qualified render profile cannot consume it:
// no Helm, so neither KubeVersion nor APIVersions can change what renders.
// Enabling Helm through Argo's build options or an Application source is
// refused before anything renders.
func TestReceiverRenderProfileCannotConsumeDiscovery(t *testing.T) {
	ctx := captureOperator(auth.RoleOwner, "operator")
	for _, options := range []string{"--enable-helm", "--load-restrictor LoadRestrictionsNone --enable-helm", "--load-restrictor LoadRestrictionsNone --helm-kube-version 1.35.0"} {
		f := newReceiverFixture(t)
		resourceMap(f.objects[argoConfigPath], "data")["kustomize.buildOptions"] = options
		_, err := readReceiver(ctx, f, "memql", "receiver", f.factory(t))
		require.ErrorContains(t, err, "renderer configuration is unsupported", options)
	}
	f := newReceiverFixture(t)
	source := resourceMap(resourceMap(f.objects["apis/argoproj.io/v1alpha1/namespaces/argocd/applications/memql"], "spec"), "source")
	source["helm"] = map[string]any{"releaseName": "memql"}
	_, err := readReceiver(ctx, f, "memql", "receiver", f.factory(t))
	require.Error(t, err, "a Helm source is outside the qualified profile")
	f = newReceiverFixture(t)
	resourceMap(f.objects[argoConfigPath], "data")["kustomize.buildOptions"] = "--load-restrictor LoadRestrictionsNone"
	_, err = readReceiver(ctx, f, "memql", "receiver", f.factory(t))
	require.NoError(t, err, "the one qualified build option still renders")
}

// The whole receiver, through readReceiver: discovery churn leaves the saved
// invariant and configuration digest unchanged.
func TestReceiverDiscoveryChurnKeepsTheSavedConfiguration(t *testing.T) {
	ctx := captureOperator(auth.RoleOwner, "operator")
	f := newReceiverFixture(t)
	before, err := readReceiver(ctx, f, "memql", "receiver", f.factory(t))
	require.NoError(t, err)
	f.objects["version"]["gitVersion"] = "v1.35.4"
	f.objects["api"]["serverAddressByClientCIDRs"] = []any{map[string]any{"clientCIDR": "0.0.0.0/0", "serverAddress": "10.0.0.9:443"}}
	f.objects["apis"]["groups"] = append(f.objects["apis"]["groups"].([]any), map[string]any{"name": "external-secrets.io", "versions": []any{map[string]any{"groupVersion": "external-secrets.io/v1", "version": "v1"}}})
	f.objects["apis/external-secrets.io/v1"] = map[string]any{"groupVersion": "external-secrets.io/v1", "resources": []any{map[string]any{"kind": "ExternalSecret", "name": "externalsecrets", "namespaced": true}}}
	after, err := readReceiver(ctx, f, "memql", "receiver", f.factory(t))
	require.NoError(t, err)
	require.Equal(t, before.invariantDigest, after.invariantDigest)
	require.Equal(t, before.digest, after.digest)
	require.NotEqual(t, before.legacyInvariantDigest, after.legacyInvariantDigest)
	require.NotEqual(t, before.render.APIVersions, after.render.APIVersions, "the renderer still receives the complete current surface")
	f.objects["version"]["gitVersion"] = "v1.36.0"
	upgraded, err := readReceiver(ctx, f, "memql", "receiver", f.factory(t))
	require.NoError(t, err)
	require.NotEqual(t, before.invariantDigest, upgraded.invariantDigest, "a minor upgrade still refuses a saved plan")
}
