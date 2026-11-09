package installation

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/integrations/argocd"
)

// These tests cover the three ways a healthy cluster stranded every started
// update before receiving-configuration-v2 (receiver_reads.go): a repo-server
// restart, a Kubernetes patch or CRD change (receiver_discovery_test.go), and
// any write to a watched ConfigMap or Secret. "Saved" below means the digests a
// plan carries from preparation into Apply, completion and rollback.

const (
	rendererDeploymentPath = "apis/apps/v1/namespaces/argocd/deployments/renderer"
	rendererSetPath        = "apis/apps/v1/namespaces/argocd/replicasets/renderer-rs"
	rendererPodPath        = "api/v1/namespaces/argocd/pods/renderer-pod"
	rendererSlicesPath     = "apis/discovery.k8s.io/v1/namespaces/argocd/endpointslices?labelSelector=kubernetes.io%2Fservice-name%3Drenderer&limit=129"
	rendererTLSPath        = "api/v1/namespaces/argocd/secrets/renderer-tls"
	receiverConfigPath     = "api/v1/namespaces/memql/configmaps/receiver"
	receiverCatalogPath    = "api/v1/namespaces/memql/secrets/catalog"
	argoConfigPath         = "api/v1/namespaces/argocd/configmaps/argocd-cm"
	argoProjectPath        = "apis/argoproj.io/v1alpha1/namespaces/argocd/appprojects/default"
)

// snapshotReceiverObjects returns a restore of the named fixture objects.
func snapshotReceiverObjects(t *testing.T, f *receiverFixture, paths ...string) func() {
	t.Helper()
	saved := map[string][]byte{}
	for _, path := range paths {
		body, err := json.Marshal(f.objects[path])
		require.NoError(t, err)
		saved[path] = body
	}
	return func() {
		for path, body := range saved {
			var object map[string]any
			require.NoError(t, json.Unmarshal(body, &object))
			f.objects[path] = object
		}
	}
}

func observeReceiver(t *testing.T, f *receiverFixture) *receiverSnapshot {
	t.Helper()
	snapshot, err := readReceiver(captureOperator(auth.RoleOwner, "operator"), f, "memql", "receiver", f.factory(t))
	require.NoError(t, err)
	return snapshot
}

// restartRenderer replays what `kubectl rollout restart` (or a node upgrade's
// eviction, with replaceSet false) does to the qualified repo-server: a new
// ReplicaSet and Pod running the same template, a rewritten EndpointSlice, and
// a Deployment whose generation, revision annotation and pod-template restart
// annotation all moved. The template's spec is the fixture's shared one.
func restartRenderer(t *testing.T, f *receiverFixture, round int, replaceSet bool) {
	t.Helper()
	deployment := f.objects[rendererDeploymentPath]
	template := resourceMap(resourceMap(deployment, "spec"), "template")
	if replaceSet {
		meta := resourceMap(deployment, "metadata")
		meta["generation"], meta["resourceVersion"] = round+1, fmt.Sprintf("rollout-%d", round)
		meta["annotations"] = map[string]any{"deployment.kubernetes.io/revision": fmt.Sprint(round + 1)}
		resourceMap(template, "metadata")["annotations"] = map[string]any{receiverRestartAnnotation: fmt.Sprintf("2026-11-09T1%d:00:00Z", round)}
		resourceMap(deployment, "status")["observedGeneration"] = round + 1
	}
	rolloutRenderer(f, round, replaceSet)
}

// rolloutRenderer replaces the renderer's Pod from the Deployment's current
// template, and with newSet first its ReplicaSet, as the Deployment controller
// does, rewriting the EndpointSlice the way the endpoint controller does.
func rolloutRenderer(f *receiverFixture, round int, newSet bool) {
	template := resourceMap(resourceMap(f.objects[rendererDeploymentPath], "spec"), "template")
	podSpec := resourceMap(template, "spec")
	set := resourceText(resourceMap(f.podOwner(), "metadata"), "name")
	if newSet {
		for path := range f.objects {
			if strings.HasPrefix(path, "apis/apps/v1/namespaces/argocd/replicasets/") {
				delete(f.objects, path)
			}
		}
		set = fmt.Sprintf("renderer-rs-%d", round)
		rs := f.put("apps/v1", "ReplicaSet", "replicasets", "argocd", set, map[string]any{"spec": map[string]any{}})
		resourceMap(rs, "metadata")["ownerReferences"] = fixtureOwner("apps/v1", "Deployment", "renderer")
	}
	for path := range f.objects {
		if strings.HasPrefix(path, "api/v1/namespaces/argocd/pods/") {
			delete(f.objects, path)
		}
	}
	name, ip := fmt.Sprintf("renderer-pod-%d", round), fmt.Sprintf("10.42.%d.9", round)
	pod := f.put("v1", "Pod", "pods", "argocd", name, map[string]any{"spec": podSpec, "status": map[string]any{"phase": "Running", "conditions": []any{map[string]any{"type": "Ready", "status": "True"}}, "podIPs": []any{map[string]any{"ip": ip}}, "containerStatuses": []any{map[string]any{"name": "repo-server", "ready": true, "imageID": "docker-pullable://" + oldImage, "state": map[string]any{"running": map[string]any{"startedAt": "2026-11-09T00:00:00Z"}}}}}})
	resourceMap(pod, "metadata")["ownerReferences"] = fixtureOwner("apps/v1", "ReplicaSet", set)
	resourceMap(pod, "metadata")["labels"] = resourceMap(resourceMap(template, "metadata"), "labels")
	useRendererTemplateHash(f, set, name)
	slices := f.objects[rendererSlicesPath]
	slice := slices["items"].([]any)[0].(map[string]any)
	sliceMeta := resourceMap(slice, "metadata")
	sliceMeta["resourceVersion"], sliceMeta["generation"] = fmt.Sprintf("slice-%d", round), round+1
	sliceMeta["annotations"] = map[string]any{"endpoints.kubernetes.io/last-change-trigger-time": fmt.Sprintf("2026-11-09T1%d:00:01Z", round)}
	slice["endpoints"] = []any{map[string]any{"addresses": []any{ip}, "conditions": map[string]any{"ready": true, "serving": true, "terminating": false}, "targetRef": map[string]any{"kind": "Pod", "namespace": "argocd", "name": name, "uid": "uid-" + name}}}
	resourceMap(slices, "metadata")["resourceVersion"] = fmt.Sprintf("list-%d", round)
}

// podOwner returns the current renderer ReplicaSet.
func (f *receiverFixture) podOwner() map[string]any {
	for path, object := range f.objects {
		if strings.HasPrefix(path, "apis/apps/v1/namespaces/argocd/replicasets/") {
			return object
		}
	}
	return nil
}

// Fix 2: an Argo repo-server restart, eviction or readiness flap no longer
// changes what a saved plan binds; the serving Pods are checked fresh instead.
func TestReceiverRendererRestartKeepsTheSavedConfiguration(t *testing.T) {
	f := newReceiverFixture(t)
	prepared := observeReceiver(t, f)

	restartRenderer(t, f, 1, true)
	restarted := observeReceiver(t, f)
	require.Equal(t, prepared.invariantDigest, restarted.invariantDigest, "a rollout restart runs the same template")
	require.Equal(t, prepared.digest, restarted.digest)
	require.NotEqual(t, prepared.legacyInvariantDigest, restarted.legacyInvariantDigest, "v1 bound each Pod and ReplicaSet")

	restartRenderer(t, f, 2, false)
	evicted := observeReceiver(t, f)
	require.Equal(t, prepared.invariantDigest, evicted.invariantDigest, "a reschedule onto another node replaces only the Pod")

	slices := f.objects[rendererSlicesPath]
	sliceMeta := resourceMap(slices["items"].([]any)[0].(map[string]any), "metadata")
	sliceMeta["resourceVersion"], sliceMeta["generation"] = "flap", 9
	sliceMeta["annotations"] = map[string]any{"endpoints.kubernetes.io/last-change-trigger-time": "2026-11-09T18:00:00Z"}
	flapped := observeReceiver(t, f)
	require.Equal(t, prepared.invariantDigest, flapped.invariantDigest, "a readiness flap that recovered rewrote only the EndpointSlice")

	resourceMap(f.objects[rendererDeploymentPath], "spec")["replicas"] = 1
	resourceMap(f.objects[rendererDeploymentPath], "metadata")["labels"] = map[string]any{"app.kubernetes.io/part-of": "argocd"}
	relabelled := observeReceiver(t, f)
	require.Equal(t, prepared.invariantDigest, relabelled.invariantDigest)

	// What the API server and admission add to every Pod but not to its
	// template: service links, the default service account name, a node and
	// the default tolerations.
	withPodSpec(func(spec map[string]any) {
		spec["enableServiceLinks"], spec["serviceAccountName"], spec["serviceAccount"], spec["nodeName"], spec["priority"] = true, "default", "default", "aks-nodepool1-0", 0
		spec["tolerations"] = []any{map[string]any{"key": "node.kubernetes.io/not-ready", "operator": "Exists", "effect": "NoExecute", "tolerationSeconds": 300}}
	})(f)
	defaulted := observeReceiver(t, f)
	require.Equal(t, prepared.invariantDigest, defaulted.invariantDigest, "Pod-only defaults are not a different renderer")
}

// servingPod returns the renderer's current Pod, whatever restart named it.
func (f *receiverFixture) servingPod() map[string]any {
	for path, object := range f.objects {
		if strings.HasPrefix(path, "api/v1/namespaces/argocd/pods/") {
			return object
		}
	}
	return nil
}

// withPodSpec changes the serving renderer Pod's own spec only.
func withPodSpec(change func(map[string]any)) func(*receiverFixture) {
	return func(f *receiverFixture) {
		pod := f.servingPod()
		body, _ := json.Marshal(resourceMap(pod, "spec"))
		var spec map[string]any
		_ = json.Unmarshal(body, &spec)
		change(spec)
		pod["spec"] = spec
	}
}

// What still refuses: a template, image or Deployment that changed, a serving
// Pod that is not of the Deployment's current revision or runs with another
// identity, an attached debug container, and a renderer that is not completely
// ready on its bound Deployment.
func TestReceiverRendererStillRefusesAChangedOrUnreadyRenderer(t *testing.T) {
	for _, test := range []struct {
		name    string
		change  func(*receiverFixture)
		refused string // the observation itself refuses; otherwise the saved plan does
	}{
		{"changed template", func(f *receiverFixture) {
			podSpec := resourceMap(resourceMap(resourceMap(f.objects[rendererDeploymentPath], "spec"), "template"), "spec")
			podSpec["containers"].([]any)[0].(map[string]any)["env"] = []any{map[string]any{"name": "ARGOCD_EXEC_TIMEOUT", "value": "300s"}}
		}, ""},
		{"changed template annotation, rolled out", func(f *receiverFixture) {
			template := resourceMap(resourceMap(f.objects[rendererDeploymentPath], "spec"), "template")
			resourceMap(template, "metadata")["annotations"] = map[string]any{"container.apparmor.security.beta.kubernetes.io/repo-server": "unconfined"}
			rolloutRenderer(f, 1, true)
		}, ""},
		{"changed template annotation, old revision still serving", func(f *receiverFixture) {
			template := resourceMap(resourceMap(f.objects[rendererDeploymentPath], "spec"), "template")
			resourceMap(template, "metadata")["annotations"] = map[string]any{"container.apparmor.security.beta.kubernetes.io/repo-server": "unconfined"}
		}, "not of its Deployment's current revision"},
		{"Pod of an older ReplicaSet", func(f *receiverFixture) {
			older := f.put("apps/v1", "ReplicaSet", "replicasets", "argocd", "renderer-rs-older", map[string]any{"spec": map[string]any{}})
			resourceMap(older, "metadata")["ownerReferences"] = fixtureOwner("apps/v1", "Deployment", "renderer")
			podSpec := resourceMap(resourceMap(resourceMap(f.objects[rendererDeploymentPath], "spec"), "template"), "spec")
			resourceMap(older, "spec")["template"] = map[string]any{"metadata": map[string]any{"labels": map[string]any{"app": "renderer", "pod-template-hash": "7f6d5c4b3a"}, "annotations": map[string]any{receiverRestartAnnotation: "2026-11-01T00:00:00Z"}}, "spec": podSpec}
			meta := resourceMap(f.objects[rendererPodPath], "metadata")
			meta["ownerReferences"] = fixtureOwner("apps/v1", "ReplicaSet", "renderer-rs-older")
			meta["labels"] = map[string]any{"app": "renderer", "pod-template-hash": "7f6d5c4b3a"}
		}, "not of its Deployment's current revision"},
		{"Pod hash is not its ReplicaSet's", func(f *receiverFixture) {
			resourceMap(f.objects[rendererPodPath], "metadata")["labels"].(map[string]any)["pod-template-hash"] = "0000000000"
		}, "not of its ReplicaSet's pod template"},
		{"ReplicaSet without a template hash", func(f *receiverFixture) {
			delete(resourceMap(resourceMap(resourceMap(f.objects[rendererSetPath], "spec"), "template"), "metadata")["labels"].(map[string]any), "pod-template-hash")
		}, "not of its ReplicaSet's pod template"},
		{"ephemeral container attached", withPodSpec(func(spec map[string]any) {
			spec["ephemeralContainers"] = []any{map[string]any{"name": "debugger", "image": "busybox", "targetContainerName": "repo-server"}}
		}), "ephemeral container"},
		{"ephemeral container status", func(f *receiverFixture) {
			resourceMap(f.objects[rendererPodPath], "status")["ephemeralContainerStatuses"] = []any{map[string]any{"name": "debugger", "state": map[string]any{"running": map[string]any{}}}}
		}, "ephemeral container"},
		{"another service account", withPodSpec(func(spec map[string]any) {
			spec["serviceAccountName"], spec["serviceAccount"] = "argocd-application-controller", "argocd-application-controller"
		}), "identity or host context"},
		{"Git host redirected", withPodSpec(func(spec map[string]any) {
			spec["hostAliases"] = []any{map[string]any{"ip": "203.0.113.9", "hostnames": []any{"github.com"}}}
		}), "identity or host context"},
		{"resolver replaced", withPodSpec(func(spec map[string]any) {
			spec["dnsPolicy"], spec["dnsConfig"] = "None", map[string]any{"nameservers": []any{"203.0.113.53"}}
		}), "identity or host context"},
		{"host network", withPodSpec(func(spec map[string]any) { spec["hostNetwork"] = true }), "identity or host context"},
		{"root security context", withPodSpec(func(spec map[string]any) {
			spec["securityContext"] = map[string]any{"runAsUser": 0}
		}), "identity or host context"},
		{"runtime class", withPodSpec(func(spec map[string]any) { spec["runtimeClassName"] = "privileged" }), "identity or host context"},
		{"service links disabled", withPodSpec(func(spec map[string]any) { spec["enableServiceLinks"] = false }), "identity or host context"},
		{"changed image, consistently configured", func(f *receiverFixture) {
			f.config.Renderer.Image = nextImage
			f.saveConfig()
			podSpec := resourceMap(resourceMap(resourceMap(f.objects[rendererDeploymentPath], "spec"), "template"), "spec")
			podSpec["containers"].([]any)[0].(map[string]any)["image"] = nextImage
			resourceMap(f.objects[rendererPodPath], "status")["containerStatuses"].([]any)[0].(map[string]any)["imageID"] = "docker-pullable://" + nextImage
		}, ""},
		{"replaced Deployment", func(f *receiverFixture) {
			resourceMap(f.objects[rendererDeploymentPath], "metadata")["uid"] = "uid-renderer-replacement"
			resourceMap(f.objects[rendererSetPath], "metadata")["ownerReferences"].([]any)[0].(map[string]any)["uid"] = "uid-renderer-replacement"
		}, ""},
		{"template image differs from configuration", func(f *receiverFixture) {
			podSpec := resourceMap(resourceMap(resourceMap(f.objects[rendererDeploymentPath], "spec"), "template"), "spec")
			podSpec["containers"].([]any)[0].(map[string]any)["image"] = nextImage
		}, "differs from its pinned image"},
		{"Pod runs another image", func(f *receiverFixture) {
			resourceMap(f.objects[rendererPodPath], "status")["containerStatuses"].([]any)[0].(map[string]any)["imageID"] = "docker-pullable://" + nextImage
		}, "runtime image"},
		{"Pod of another Deployment", func(f *receiverFixture) {
			resourceMap(f.objects[rendererSetPath], "metadata")["ownerReferences"].([]any)[0].(map[string]any)["uid"] = "uid-other-deployment"
		}, "not owned by its Deployment"},
		{"no ready Pod", func(f *receiverFixture) {
			slice := f.objects[rendererSlicesPath]["items"].([]any)[0].(map[string]any)
			slice["endpoints"].([]any)[0].(map[string]any)["conditions"] = map[string]any{"ready": false}
		}, "not a ready named Pod"},
		{"rollout in progress", func(f *receiverFixture) {
			resourceMap(f.objects[rendererDeploymentPath], "status")["updatedReplicas"] = 0
		}, "not completely ready"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newReceiverFixture(t)
			prepared := observeReceiver(t, f)
			test.change(f)
			fresh, err := readReceiver(captureOperator(auth.RoleOwner, "operator"), f, "memql", "receiver", f.factory(t))
			if test.refused != "" {
				require.ErrorContains(t, err, test.refused)
				return
			}
			require.NoError(t, err)
			require.NotEqual(t, prepared.invariantDigest, fresh.invariantDigest, "a renderer that runs something else cannot serve a saved plan")
			require.False(t, fresh.matchesInvariant(prepared.invariantDigest))
		})
	}
}

// Fix 6: bind each ConfigMap's and Secret's content, not its resourceVersion.
func TestReceiverConfigurationBindsContentNotVersions(t *testing.T) {
	annotate := func(path string) func(*receiverFixture) {
		return func(f *receiverFixture) {
			meta := resourceMap(f.objects[path], "metadata")
			meta["resourceVersion"] = "rewritten"
			meta["annotations"] = map[string]any{"memql.io/credential-expires-at": "2027-02-07T00:00:00Z", "kubectl.kubernetes.io/last-applied-configuration": "{}"}
			meta["labels"] = map[string]any{"app.kubernetes.io/part-of": "argocd"}
		}
	}
	for _, test := range []struct {
		name     string
		change   func(*receiverFixture)
		accepted bool
	}{
		{"receiver ConfigMap annotated", annotate(receiverConfigPath), true},
		{"catalog credential annotated", annotate(receiverCatalogPath), true},
		{"Argo ConfigMap annotated", annotate(argoConfigPath), true},
		{"renderer TLS annotated", annotate(rendererTLSPath), true},
		{"Argo project status and metadata", func(f *receiverFixture) {
			project := f.objects[argoProjectPath]
			annotate(argoProjectPath)(f)
			project["status"] = map[string]any{"jwtTokensByRole": map[string]any{}}
		}, true},
		{"renderer Service metadata", annotate("api/v1/namespaces/argocd/services/renderer"), true},
		{"receiver ConfigMap data", func(f *receiverFixture) {
			resourceMap(f.objects[receiverConfigPath], "data")["notes"] = "edited by hand"
		}, false},
		{"Argo ConfigMap data", func(f *receiverFixture) {
			resourceMap(f.objects[argoConfigPath], "data")["timeout.reconciliation"] = "60s"
		}, false},
		{"Argo ConfigMap no longer read by Argo", func(f *receiverFixture) {
			delete(resourceMap(resourceMap(f.objects[argoConfigPath], "metadata"), "labels"), "app.kubernetes.io/part-of")
		}, false},
		{"Argo project spec", func(f *receiverFixture) {
			spec := resourceMap(f.objects[argoProjectPath], "spec")
			spec["destinations"] = append(spec["destinations"].([]any), map[string]any{"namespace": "other", "server": "https://kubernetes.default.svc"})
		}, false},
		{"renderer Service spec", func(f *receiverFixture) {
			resourceMap(f.objects["api/v1/namespaces/argocd/services/renderer"], "spec")["sessionAffinity"] = "ClientIP"
		}, false},
		{"Secret type", func(f *receiverFixture) { f.objects[receiverCatalogPath]["type"] = "kubernetes.io/basic-auth" }, false},
		{"ConfigMap replaced", func(f *receiverFixture) {
			resourceMap(f.objects[argoConfigPath], "metadata")["uid"] = "uid-argocd-cm-replacement"
		}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newReceiverFixture(t)
			prepared := observeReceiver(t, f)
			test.change(f)
			fresh := observeReceiver(t, f)
			if test.accepted {
				require.Equal(t, prepared.invariantDigest, fresh.invariantDigest)
				require.Equal(t, prepared.digest, fresh.digest)
				require.NotEqual(t, prepared.legacyInvariantDigest, fresh.legacyInvariantDigest, "v1 bound the resourceVersion")
				return
			}
			require.NotEqual(t, prepared.invariantDigest, fresh.invariantDigest)
			require.False(t, fresh.matchesInvariant(prepared.invariantDigest))
		})
	}
}

// A rotated credential, the catalog token included, still refuses; the
// digests are keyed, so a saved plan reveals nothing that a guess can test.
func TestReceiverContentDigestIsKeyedByTheReceiversCredential(t *testing.T) {
	factory := func(context.Context, []byte, string) (Catalog, error) { return receiverCatalogFixture{}, nil }
	ctx := captureOperator(auth.RoleOwner, "operator")
	f := newReceiverFixture(t)
	prepared, err := readReceiver(ctx, f, "memql", "receiver", factory)
	require.NoError(t, err)
	resourceMap(f.objects[receiverCatalogPath], "data")["token"] = base64.StdEncoding.EncodeToString([]byte("private-rotated-token"))
	rotated, err := readReceiver(ctx, f, "memql", "receiver", factory)
	require.NoError(t, err)
	require.NotEqual(t, prepared.invariantDigest, rotated.invariantDigest, "a rotated credential refuses")

	object := map[string]any{"type": "Opaque", "data": map[string]any{"password": base64.StdEncoding.EncodeToString([]byte("hunter2"))}}
	one, other := newReceiverReads(f), newReceiverReads(f)
	one.useBindingKey("private-catalog-token")
	other.useBindingKey("private-another-token")
	a, err := one.contentDigest(object)
	require.NoError(t, err)
	again, err := one.contentDigest(object)
	require.NoError(t, err)
	b, err := other.contentDigest(object)
	require.NoError(t, err)
	require.Equal(t, a, again)
	require.NotEqual(t, a, b, "the same content under another key is a different digest")
	_, err = newReceiverReads(f).contentDigest(object)
	require.ErrorContains(t, err, "binding key", "content is never bound unkeyed")

	r := newReceiverReads(f)
	r.useBindingKey("private-catalog-token")
	_, err = r.object(ctx, "v1", "Secret", "secrets", "memql", "catalog")
	require.NoError(t, err)
	binding, bound, err := r.currentBinding(r.reads[receiverCatalogPath])
	require.NoError(t, err)
	require.True(t, bound)
	encoded, err := json.Marshal(binding)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "private-catalog-token")
	require.NotContains(t, string(encoded), resourceText(resourceMap(f.objects[receiverCatalogPath], "data"), "token"))
}

// rendererAuthority is a CA for the qualified renderer's serving certificate,
// as cert-manager's CA issuer would hold it.
type rendererAuthority struct {
	certificate *x509.Certificate
	key         ed25519.PrivateKey
}

func newRendererAuthority(t *testing.T, name string) rendererAuthority {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, BasicConstraintsValid: true, IsCA: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, key)
	require.NoError(t, err)
	certificate, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return rendererAuthority{certificate, key}
}

func (a rendererAuthority) pem() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: a.certificate.Raw})
}

// issue returns a serving leaf for the fixture's renderer Service.
func (a rendererAuthority) issue(t *testing.T, serial int64) tls.Certificate {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "renderer"}, DNSNames: []string{"renderer.argocd.svc"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, a.certificate, pub, a.key)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// selfSigned returns a self-signed serving certificate under key, which
// cert-manager's selfSigned issuer produces (reusing the key only under
// privateKey.rotationPolicy: Never).
func selfSignedRenderer(t *testing.T, key ed25519.PrivateKey, serial int64) tls.Certificate {
	t.Helper()
	template := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "renderer"}, DNSNames: []string{"renderer.argocd.svc"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, IsCA: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// intermediate returns a CA that a issues, as a CA issuer backed by an
// intermediate would hold it.
func (a rendererAuthority) intermediate(t *testing.T, name string) rendererAuthority {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, BasicConstraintsValid: true, IsCA: true}
	der, err := x509.CreateCertificate(rand.Reader, template, a.certificate, pub, a.key)
	require.NoError(t, err)
	certificate, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return rendererAuthority{certificate, key}
}

// servingSelfSigned returns the reference installation's kind of renderer
// certificate: cert-manager's selfSigned issuer for a Certificate with DNS
// names only, so no subject, not a CA, and server usages only.
func servingSelfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), DNSNames: []string{"renderer", "renderer.argocd", "renderer.argocd.svc"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, key)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// servingSignedAs returns a serving certificate under its own key whose issuer
// is the given template, signed with signer: a certificate that only looks
// self-signed (issuer named as itself, another key signed it) or only signs
// itself (its own key, another issuer's name).
func servingSignedAs(t *testing.T, issuer *x509.Certificate, signer ed25519.PrivateKey, own ed25519.PrivateKey) tls.Certificate {
	t.Helper()
	template := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "renderer"}, DNSNames: []string{"renderer.argocd.svc"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, IsCA: true}
	der, err := x509.CreateCertificate(rand.Reader, template, issuer, own.Public(), signer)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: own}
}

// useRendererCertificate writes the renderer TLS Secret as cert-manager does
// on issue and renewal: tls.crt holds leaf's whole chain, leaf first, and
// authority is its ca.crt, absent when nil.
func useRendererCertificate(f *receiverFixture, leaf tls.Certificate, authority []byte) {
	var chain []byte
	for _, der := range leaf.Certificate {
		chain = append(chain, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	data := map[string]any{"tls.crt": base64.StdEncoding.EncodeToString(chain), "tls.key": base64.StdEncoding.EncodeToString([]byte("private-key-never-output"))}
	if authority != nil {
		data["ca.crt"] = base64.StdEncoding.EncodeToString(authority)
	}
	secret := f.objects[rendererTLSPath]
	secret["data"] = data
	meta := resourceMap(secret, "metadata")
	meta["resourceVersion"] = fmt.Sprintf("%v-renewed", meta["resourceVersion"])
	f.certificate = leaf
}

// serveRenderer accepts TLS handshakes with the certificate a repo-server
// loaded at its last start.
func serveRenderer(t *testing.T, leaf tls.Certificate) string {
	t.Helper()
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{leaf}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.(*tls.Conn).Handshake()
			_ = conn.Close()
		}
	}()
	return listener.Addr().String()
}

func dialRenderer(snapshot *receiverSnapshot, address string) error {
	conn, err := (&tls.Dialer{NetDialer: &net.Dialer{Timeout: 5 * time.Second}, Config: snapshot.rendererTLS}).DialContext(context.Background(), "tcp", address)
	if err != nil {
		return err
	}
	return conn.Close()
}

// Fix 6: the renderer's TLS trust is its issuing CA. A renewed leaf under the
// same CA passes, whether or not the repo-server has restarted to serve it; a
// different CA refuses a saved plan, and the old leaf no longer verifies.
func TestReceiverRendererTrustAnchorSurvivesRenewalButNotANewCA(t *testing.T) {
	f := newReceiverFixture(t)
	authority := newRendererAuthority(t, "memql-installation-renderer")
	original := authority.issue(t, 1)
	useRendererCertificate(f, original, authority.pem())
	prepared := observeReceiver(t, f)
	servingOriginal := serveRenderer(t, original)
	require.NoError(t, dialRenderer(prepared, servingOriginal))

	renewed := authority.issue(t, 2)
	useRendererCertificate(f, renewed, authority.pem())
	afterRenewal := observeReceiver(t, f)
	require.Equal(t, prepared.invariantDigest, afterRenewal.invariantDigest, "a renewal under the same CA keeps the trust anchor")
	require.NotEqual(t, prepared.legacyInvariantDigest, afterRenewal.legacyInvariantDigest, "v1 bound the Secret's resourceVersion")
	require.NoError(t, dialRenderer(afterRenewal, servingOriginal), "Argo serves the old leaf until the repo-server restarts")
	require.NoError(t, dialRenderer(afterRenewal, serveRenderer(t, renewed)), "and the renewed one after")

	other := newRendererAuthority(t, "memql-installation-renderer")
	replaced := other.issue(t, 3)
	useRendererCertificate(f, replaced, other.pem())
	afterNewCA := observeReceiver(t, f)
	require.NotEqual(t, prepared.invariantDigest, afterNewCA.invariantDigest, "a different CA refuses a saved plan")
	require.False(t, afterNewCA.matchesInvariant(prepared.invariantDigest))
	require.Error(t, dialRenderer(afterNewCA, servingOriginal), "a leaf of the old CA is no longer trusted")
	require.NoError(t, dialRenderer(afterNewCA, serveRenderer(t, replaced)))

	// A leaf the configured trust does not issue refuses the observation.
	useRendererCertificate(f, other.issue(t, 4), authority.pem())
	_, err := readReceiver(captureOperator(auth.RoleOwner, "operator"), f, "memql", "receiver", f.factory(t))
	require.ErrorContains(t, err, "different identity")
}

// A self-signed renderer certificate (no ca.crt) is its own anchor. Reissuing
// it under the same key keeps the anchor; a new key, cert-manager's default
// rotation since v1.18, is a new anchor and refuses a plan that spans it.
func TestReceiverSelfSignedRendererAnchorIsItsKey(t *testing.T) {
	f := newReceiverFixture(t)
	_, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	original := selfSignedRenderer(t, key, 1)
	useRendererCertificate(f, original, nil)
	prepared := observeReceiver(t, f)
	servingOriginal := serveRenderer(t, original)
	require.NoError(t, dialRenderer(prepared, servingOriginal))
	reissued := selfSignedRenderer(t, key, 2)
	useRendererCertificate(f, reissued, nil)
	sameKey := observeReceiver(t, f)
	require.Equal(t, prepared.invariantDigest, sameKey.invariantDigest, "a reissue under the same key keeps the anchor")
	require.Error(t, dialRenderer(sameKey, servingOriginal), "until the repo-server restarts, its old self-signed leaf no longer verifies")
	require.NoError(t, dialRenderer(sameKey, serveRenderer(t, reissued)), "after the restart it serves the anchor itself")
	_, fresh, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	useRendererCertificate(f, selfSignedRenderer(t, fresh, 3), nil)
	newKey := observeReceiver(t, f)
	require.NotEqual(t, prepared.invariantDigest, newKey.invariantDigest, "a self-signed leaf whose key rotates is a new trust anchor and refuses")
	require.False(t, newKey.matchesInvariant(prepared.invariantDigest))
}

// Only a self-signed certificate is a trust anchor. The anchors come from the
// TLS Secret's ca.crt or, without one, the top of its tls.crt chain, so an
// intermediate or an issued leaf there would otherwise become a root of trust
// in its own right. The reference installation's self-signed leaf (with
// itself as ca.crt, as cert-manager's selfSigned issuer writes it) and a
// self-signed CA in ca.crt pass, and the chain each serves verifies.
func TestReceiverRendererTrustAnchorMustBeSelfSigned(t *testing.T) {
	root := newRendererAuthority(t, "memql-installation-root")
	intermediate := root.intermediate(t, "memql-installation-intermediate")
	chain := func(leaf tls.Certificate, issuers ...rendererAuthority) tls.Certificate {
		out := tls.Certificate{Certificate: append([][]byte(nil), leaf.Certificate...), PrivateKey: leaf.PrivateKey}
		for _, issuer := range issuers {
			out.Certificate = append(out.Certificate, issuer.certificate.Raw)
		}
		return out
	}
	viaIntermediate := chain(intermediate.issue(t, 5), intermediate)
	reference := servingSelfSigned(t)
	_, own, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	namedLikeItself := newRendererAuthority(t, "renderer")
	otherIssuer := &x509.Certificate{Subject: pkix.Name{CommonName: "another-issuer"}, PublicKey: own.Public()}
	cases := []struct {
		name      string
		serving   tls.Certificate
		authority []byte
		refuse    bool
	}{
		{"self-signed leaf with itself as ca.crt", reference, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: reference.Certificate[0]}), false},
		{"self-signed leaf without ca.crt", reference, nil, false},
		{"self-signed CA in ca.crt", root.issue(t, 6), root.pem(), false},
		{"self-signed root at the top of tls.crt", chain(viaIntermediate, root), nil, false},
		{"intermediate at the top of tls.crt", viaIntermediate, nil, true},
		{"intermediate in ca.crt", viaIntermediate, intermediate.pem(), true},
		{"issued leaf with no ca.crt", root.issue(t, 7), nil, true},
		{"names itself as issuer, another key signed it", servingSignedAs(t, namedLikeItself.certificate, namedLikeItself.key, own), nil, true},
		{"signed by its own key under another issuer", servingSignedAs(t, otherIssuer, own, own), nil, true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			f := newReceiverFixture(t)
			useRendererCertificate(f, test.serving, test.authority)
			snapshot, err := readReceiver(captureOperator(auth.RoleOwner, "operator"), f, "memql", "receiver", f.factory(t))
			if test.refuse {
				require.ErrorContains(t, err, "trust anchor is not self-signed")
				return
			}
			require.NoError(t, err)
			require.NoError(t, dialRenderer(snapshot, serveRenderer(t, test.serving)), "the chain the repo-server serves verifies against the anchors")
		})
	}
}

// The trust anchors stand in for the TLS Secret's content only while the
// repo-server consumes that Secret exactly as the qualified profile does: one
// volume, mounted once, at /app/config/reposerver/tls, projecting no key
// beyond tls.crt, tls.key and ca.crt, and no other reader of the Secret.
// Anything else keeps the keyed content binding every Secret gets, so a
// changed value of any key refuses. The probe is a changed tls.key, which the
// anchors do not cover.
func TestReceiverRendererTLSSecretNarrowsOnlyForItsQualifiedMount(t *testing.T) {
	podSpec := func(f *receiverFixture) map[string]any {
		return resourceMap(resourceMap(resourceMap(f.objects[rendererDeploymentPath], "spec"), "template"), "spec")
	}
	container := func(f *receiverFixture) map[string]any { return podSpec(f)["containers"].([]any)[0].(map[string]any) }
	mount := func(f *receiverFixture, volume, path string) {
		c := container(f)
		c["volumeMounts"] = append(c["volumeMounts"].([]any), map[string]any{"name": volume, "mountPath": path})
	}
	project := func(f *receiverFixture, keys ...string) {
		items := []any{}
		for _, key := range keys {
			items = append(items, map[string]any{"key": key, "path": key})
		}
		volume := podSpec(f)["volumes"].([]any)[0].(map[string]any)
		resourceMap(volume, "secret")["items"] = items
	}
	readThroughEnvironment := func(key string) func(*receiverFixture) {
		return func(f *receiverFixture) {
			container(f)["env"] = []any{map[string]any{"name": "REDIS_PASSWORD", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "renderer-tls", "key": key}}}}
		}
	}
	cases := []struct {
		name   string
		change func(*receiverFixture)
		narrow bool
	}{
		{"whole Secret holding only serving keys", func(*receiverFixture) {}, true},
		{"upstream projection", func(f *receiverFixture) { project(f, "tls.crt", "tls.key", "ca.crt") }, true},
		{"mounted a second time", func(f *receiverFixture) {
			spec := podSpec(f)
			spec["volumes"] = append(spec["volumes"].([]any), map[string]any{"name": "tls-copy", "secret": map[string]any{"secretName": "renderer-tls"}})
			mount(f, "tls-copy", "/app/config/tls")
		}, false},
		{"declared as a second volume", func(f *receiverFixture) {
			spec := podSpec(f)
			spec["volumes"] = append(spec["volumes"].([]any), map[string]any{"name": "tls-spare", "secret": map[string]any{"secretName": "renderer-tls"}})
		}, false},
		{"also mounted elsewhere", func(f *receiverFixture) { mount(f, "tls", "/app/config/tls") }, false},
		{"mounted by the init container", func(f *receiverFixture) {
			podSpec(f)["initContainers"] = []any{map[string]any{"name": "copyutil", "image": oldImage, "command": []any{"/bin/cp", "-n", "/usr/local/bin/argocd", "/var/run/argocd/argocd-cmp-server"}, "volumeMounts": []any{map[string]any{"name": "tls", "mountPath": "/var/run/argocd"}}}}
		}, false},
		{"projects another key", func(f *receiverFixture) { project(f, "tls.crt", "tls.key", "keystore.p12") }, false},
		{"whole Secret holding another key", func(f *receiverFixture) {
			resourceMap(f.objects[rendererTLSPath], "data")["keystore.p12"] = base64.StdEncoding.EncodeToString([]byte("keystore"))
		}, false},
		{"another key read through the environment", readThroughEnvironment("tls.key"), false},
		{"its certificate read through the environment", readThroughEnvironment("tls.crt"), false},
		{"also another input's Secret", func(f *receiverFixture) {
			// The projection is the upstream one, so only the other reader decides.
			project(f, "tls.crt", "tls.key", "ca.crt")
			f.config.RepositoryScope = &receiverRepositoryScope{Profile: namedProjectRepositoryProfile, Bindings: []receiverRepositoryBinding{{Application: receiverNamed{"argocd", "memql"}, Secret: receiverNamed{"argocd", "renderer-tls"}}}}
			secret := f.objects[rendererTLSPath]
			data := resourceMap(secret, "data")
			data["url"] = base64.StdEncoding.EncodeToString([]byte("https://github.com/acme/platform.git"))
			data["project"] = base64.StdEncoding.EncodeToString([]byte("default"))
			resourceMap(secret, "metadata")["labels"] = map[string]any{"argocd.argoproj.io/secret-type": "repository"}
			delete(f.objects, repositoryInventoryFixturePath)
			f.saveConfig()
		}, false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			f := newReceiverFixture(t)
			test.change(f)
			prepared := observeReceiver(t, f)
			secret := f.objects[rendererTLSPath]
			resourceMap(secret, "data")["tls.key"] = base64.StdEncoding.EncodeToString([]byte("private-key-rotated"))
			meta := resourceMap(secret, "metadata")
			meta["resourceVersion"] = fmt.Sprintf("%v-rotated", meta["resourceVersion"])
			changed := observeReceiver(t, f)
			if test.narrow {
				require.Equal(t, prepared.invariantDigest, changed.invariantDigest, "the trust anchors stand in for the Secret")
				return
			}
			require.NotEqual(t, prepared.invariantDigest, changed.invariantDigest, "any changed value of the Secret refuses")
			require.False(t, changed.matchesInvariant(prepared.invariantDigest))
		})
	}
}

// A plan an engine up to 0.31.28 saved holds the v1 digests. These golden
// values were computed by that engine itself: an unmodified export of
// 4d4464a6c running HEAD's fixture with exactly the two additions this
// fixture makes (argocd-cm labels and the renderer pod-template-hash,
// useRendererTemplateHash), three identical runs. The receiver must keep
// reproducing them so the update that installs this engine, prepared by the
// previous one, can still complete or be restored; matching them also proves
// this engine makes no Kubernetes read that engine does not. v1 keeps its
// strictness: a restarted renderer refuses such a plan as it did, and its
// operator closes it (CloseSelected), while a plan saved in v2 still verifies.
func TestReceiverLegacyDigestsReproduceTheV1Engine(t *testing.T) {
	const singleDigest = "memql-id:657f14d72f1e037ba0aa2373e7d4c8896ce5dc68ae2373b05595bad6a60d4c9a"
	const singleInvariant = "memql-id:310b16e5b0366c5d0baf88844cb02d30ca2962c89ecfcd356b24b15711e09dd4"
	const groupDigest = "memql-id:06000290947aa5bcf679a9a8323ef38791eac2f3027bc04a019ba62082dfe70f"
	const groupInvariant = "memql-id:1883a7eee8698111b60dd97fa18dca1551f39f32c7c7174dd484cd469b770984"

	f := newReceiverFixture(t)
	single := observeReceiver(t, f)
	require.Equal(t, singleDigest, single.legacyDigest)
	require.Equal(t, singleInvariant, single.legacyInvariantDigest)
	require.NotEqual(t, singleInvariant, single.invariantDigest, "this engine saves v2")

	legacyPlan := preparedPlan{FormatVersion: 1, Preparation: &preparationBinding{ConfigurationDigest: singleDigest, ConfigurationInvariantDigest: singleInvariant}}
	currentPlan := preparedPlan{FormatVersion: 1, Preparation: &preparationBinding{ConfigurationDigest: single.digest, ConfigurationInvariantDigest: single.invariantDigest}}
	mixedPlan := preparedPlan{FormatVersion: 1, Preparation: &preparationBinding{ConfigurationDigest: single.digest, ConfigurationInvariantDigest: singleInvariant}}
	require.True(t, single.matchesInvariant(singleInvariant))
	require.True(t, single.matchesPreparation(legacyPlan))
	require.True(t, single.matchesPreparation(currentPlan))
	require.False(t, single.matchesPreparation(mixedPlan), "one plan's two digests come from one format")
	require.True(t, configurationMatchesPlan(single, legacyPlan))

	restartRenderer(t, f, 1, true)
	restarted := observeReceiver(t, f)
	require.False(t, restarted.matchesInvariant(singleInvariant), "a v1 plan keeps v1's strictness")
	require.False(t, restarted.matchesPreparation(legacyPlan))
	require.True(t, restarted.matchesInvariant(single.invariantDigest), "a v2 plan survives the restart")
	require.True(t, restarted.matchesPreparation(currentPlan))

	g := namedRepositoryFixture(t)
	g.config.FormatVersion = 2
	g.config.Application = receiverNamed{}
	g.config.Applications = []receiverApplication{{Key: "engine", Namespace: "argocd", Name: "memql"}, {Key: "product", Namespace: "argocd", Name: "product"}}
	g.config.ImageBindings[0].Application = "engine"
	g.config.RepositoryScope.Bindings = append(g.config.RepositoryScope.Bindings, receiverRepositoryBinding{Application: receiverNamed{"argocd", "product"}, Secret: receiverNamed{"argocd", "product.repository"}})
	clone := func(value map[string]any) map[string]any {
		body, err := json.Marshal(value)
		require.NoError(t, err)
		var out map[string]any
		require.NoError(t, json.Unmarshal(body, &out))
		return out
	}
	app := clone(g.objects["apis/argoproj.io/v1alpha1/namespaces/argocd/applications/memql"])
	resourceMap(app, "spec")["project"] = "product"
	g.put("argoproj.io/v1alpha1", "Application", "applications", "argocd", "product", app)
	g.put("argoproj.io/v1alpha1", "AppProject", "appprojects", "argocd", "product", clone(g.objects[argoProjectPath]))
	secret := g.put("v1", "Secret", "secrets", "argocd", "product.repository", clone(g.objects[namedRepositoryFixturePath]))
	resourceMap(secret, "metadata")["uid"] = "uid-product-repository"
	resourceMap(secret, "metadata")["labels"] = map[string]any{"argocd.argoproj.io/secret-type": "repository"}
	resourceMap(secret, "data")["project"] = base64.StdEncoding.EncodeToString([]byte("product"))
	g.saveConfig()
	group := observeReceiver(t, g)
	require.Equal(t, groupDigest, group.legacyDigest)
	require.Equal(t, groupInvariant, group.legacyInvariantDigest)
	require.True(t, group.matchesPreparation(preparedPlan{FormatVersion: 2, Preparation: &preparationBinding{ConfigurationInvariantDigest: groupInvariant}}))
}

// The selected reader, which a group's Apply, every completion, Restore and its
// progress check use, accepts a saved plan in the format of the engine that
// saved it: a v1 plan with v1's strictness, a v2 plan with v2's.
func TestReceiverSelectedReaderVerifiesEachSavedFormat(t *testing.T) {
	ctx := captureOperator(auth.RoleOwner, "operator")
	f := newReceiverFixture(t)
	prepared := observeReceiver(t, f)
	intent, err := argocd.PlanRevision(prepared.application, "saved-format", strings.Repeat("b", 40), false)
	require.NoError(t, err)
	host := &preparationHost{api: f, catalog: f.factory(t), namespace: "memql", configurationName: "receiver"}
	saved := func(digest, invariant string) revisionRecord {
		return revisionRecord{State: "prepared", Plan: preparedPlan{FormatVersion: 1, InstallationID: f.config.InstallationID, Intent: intent,
			Preparation: &preparationBinding{ConfigurationDigest: digest, ConfigurationInvariantDigest: invariant}}}
	}
	legacy, current := saved(prepared.legacyDigest, prepared.legacyInvariantDigest), saved(prepared.digest, prepared.invariantDigest)
	for _, record := range []revisionRecord{legacy, current} {
		_, err := readSelectedReceiver(ctx, host, record)
		require.NoError(t, err)
	}
	restartRenderer(t, f, 1, true)
	_, err = readSelectedReceiver(ctx, host, legacy)
	require.ErrorContains(t, err, "configuration or membership changed", "a v1 plan keeps v1's strictness")
	_, err = readSelectedReceiver(ctx, host, current)
	require.NoError(t, err, "a v2 plan survives the restart")
	_, err = readSelectedReceiver(ctx, host, saved(prepared.digest, prepared.digest))
	require.ErrorContains(t, err, "configuration or membership changed", "no other saved digest stands in for the invariant")
}

func receiverConfigManifest(data, namespace string, metadata map[string]any) string {
	meta := map[string]any{"name": "receiver"}
	if namespace != "" {
		meta["namespace"] = namespace
	}
	for key, value := range metadata {
		meta[key] = value
	}
	body, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": meta, "data": map[string]any{"installation.json": data}})
	return string(body)
}

// Fix 6: a candidate that would rewrite the receiver's own configuration
// (memql-installation, rendered by the memql Application) can never complete,
// so preparation refuses it and says how to deploy it instead.
func TestPreparationRefusesACandidateThatChangesTheReceiverConfiguration(t *testing.T) {
	f := newReceiverFixture(t)
	snapshot := observeReceiver(t, f)
	live := resourceText(resourceMap(f.objects[receiverConfigPath], "data"), "installation.json")
	edited := strings.Replace(live, `"platform":"linux/arm64"`, `"platform":"linux/amd64"`, 1)
	require.NotEqual(t, live, edited)
	render := func(revision string, extra ...string) argocd.RenderedRevision {
		return renderInventoryFixture(t, strings.Repeat(revision, 40), append(resourceManifests(oldImage), extra...))
	}
	for _, test := range []struct {
		name          string
		before, after []string
		refused, todo string
	}{
		{"unchanged declaration", []string{receiverConfigManifest(live, "memql", nil)}, []string{receiverConfigManifest(live, "memql", nil)}, "", ""},
		{"metadata only", []string{receiverConfigManifest(live, "memql", nil)}, []string{receiverConfigManifest(live, "memql", map[string]any{"labels": map[string]any{"release": "next"}, "annotations": map[string]any{"argocd.argoproj.io/sync-options": "Prune=false"}})}, "", ""},
		{"destination namespace", []string{receiverConfigManifest(live, "", nil)}, []string{receiverConfigManifest(live, "", nil)}, "", ""},
		{"never declared", nil, nil, "", ""},
		{"another namespace", []string{receiverConfigManifest(live, "other", nil)}, []string{receiverConfigManifest(edited, "other", nil)}, "", ""},
		{"candidate edits installation.json", []string{receiverConfigManifest(live, "memql", nil)}, []string{receiverConfigManifest(edited, "memql", nil)}, "changes the installation's own update configuration", "syncing the Application that renders the ConfigMap to a revision that declares it"},
		{"candidate edits it without a namespace", []string{receiverConfigManifest(live, "", nil)}, []string{receiverConfigManifest(edited, "", nil)}, "changes the installation's own update configuration", "syncing the Application that renders the ConfigMap to a revision that declares it"},
		{"candidate starts declaring other content", nil, []string{receiverConfigManifest(edited, "memql", nil)}, "changes the installation's own update configuration", "syncing the Application that renders the ConfigMap to a revision that declares it"},
		{"candidate stops declaring it", []string{receiverConfigManifest(live, "memql", nil)}, nil, "stops declaring", "pruned or out of sync"},
		{"current release declares other content", []string{receiverConfigManifest(edited, "memql", nil)}, []string{receiverConfigManifest(live, "memql", nil)}, "restoring that release would change it", "declares the live content or restore the declared content"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := snapshot.ownConfigurationUnchanged(render("a", test.before...), render("b", test.after...))
			if test.refused == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, test.refused)
			require.ErrorContains(t, err, "ConfigMap memql/receiver")
			require.ErrorContains(t, err, test.todo)
			// The preparation's reservation holds the installation until it is
			// cancelled, so every reason starts there.
			require.ErrorContains(t, err, "cancel this preparation")
			require.ErrorContains(t, err, "by hand")
		})
	}

	// Through the preparation recipe's native port: refused before any image,
	// registry or cluster read, with no resource evidence left to promote.
	scope := &preparationWorkflowScope{host: &preparationHost{api: refusingAPI{t}}, operator: "operator", configuration: snapshot, record: preparationRecord{Scope: preparationScope{FormatVersion: 1}},
		renders: map[string]argocd.RenderedRevision{"rollback": render("a", receiverConfigManifest(live, "memql", nil)), "candidate": render("b", receiverConfigManifest(edited, "memql", nil))}}
	_, err := scope.verifyResources(context.Background(), nil)
	require.ErrorContains(t, err, "cancel this preparation, deploy that change by hand")
	require.Empty(t, scope.resources.digest)
	require.Nil(t, scope.artifacts)
}

type refusingAPI struct{ t *testing.T }

func (a refusingAPI) Do(context.Context, string, string, string, []byte) ([]byte, error) {
	a.t.Error("a refused candidate must not reach the cluster")
	return nil, errors.New("unexpected cluster read")
}
