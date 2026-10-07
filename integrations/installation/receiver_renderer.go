package installation

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/znasllc-io/memql/integrations/argocd"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

func receiverInteger(value any) int64 {
	n, ok := value.(json.Number)
	if !ok {
		return -1
	}
	v, err := n.Int64()
	if err != nil {
		return -1
	}
	return v
}
func receiverController(object map[string]any, kind, name, uid string) bool {
	refs, ok := resourceMap(object, "metadata")["ownerReferences"].([]any)
	if !ok || len(refs) != 1 {
		return false
	}
	ref, ok := refs[0].(map[string]any)
	if !ok {
		return false
	}
	version := "apps/v1"
	if kind == "Service" {
		version = "v1"
	}
	return resourceText(ref, "apiVersion") == version && resourceText(ref, "kind") == kind && resourceText(ref, "name") == name && resourceText(ref, "uid") == uid && ref["controller"] == true
}
func receiverReady(pod map[string]any) bool {
	status := resourceMap(pod, "status")
	conditions, _ := status["conditions"].([]any)
	if resourceText(status, "phase") != "Running" {
		return false
	}
	for _, raw := range conditions {
		c, _ := raw.(map[string]any)
		if resourceText(c, "type") == "Ready" && resourceText(c, "status") == "True" {
			return true
		}
	}
	return false
}
func receiverContainers(spec map[string]any, key string) ([]map[string]any, error) {
	raw, ok := spec[key].([]any)
	if !ok || len(raw) > 8 {
		return nil, errors.New("renderer container inventory is invalid")
	}
	out := make([]map[string]any, len(raw))
	for i, item := range raw {
		v, ok := item.(map[string]any)
		if !ok {
			return nil, errors.New("renderer container is malformed")
		}
		out[i] = v
	}
	return out, nil
}

func (s *receiverSnapshot) observeRenderer(ctx context.Context, r *receiverReads) error {
	cfg := s.configuration.Renderer
	namespace := s.configuration.Application.Namespace
	if cfg.Profile != "argocd-2.13.3-kustomize-5.4.3" {
		return errors.New("installation renderer profile is unqualified")
	}
	if _, err := immutableImage(cfg.Image); err != nil {
		return errors.New("installation renderer image must be pinned by digest")
	}
	deployment, err := r.object(ctx, "apps/v1", "Deployment", "deployments", namespace, cfg.Deployment)
	if err != nil {
		return err
	}
	spec, status := resourceMap(deployment, "spec"), resourceMap(deployment, "status")
	desired := receiverInteger(spec["replicas"])
	generation := receiverInteger(resourceMap(deployment, "metadata")["generation"])
	if desired < 1 || desired > 32 || generation < 1 || receiverInteger(status["observedGeneration"]) != generation {
		return errors.New("installation renderer Deployment is not observed")
	}
	for _, field := range []string{"replicas", "updatedReplicas", "readyReplicas", "availableReplicas"} {
		if receiverInteger(status[field]) != desired {
			return errors.New("installation renderer Deployment is not completely ready")
		}
	}
	selector := resourceMap(resourceMap(spec, "selector"), "matchLabels")
	if len(selector) == 0 || len(selector) > 16 || resourceMap(spec, "selector")["matchExpressions"] != nil {
		return errors.New("installation renderer selector is unsupported")
	}
	template := resourceMap(spec, "template")
	podSpec := resourceMap(template, "spec")
	containers, err := receiverContainers(podSpec, "containers")
	if err != nil || len(containers) != 1 {
		return errors.New("installation renderer sidecars require a qualified profile")
	}
	container := containers[0]
	if resourceText(container, "name") != cfg.Container || resourceText(container, "image") != cfg.Image {
		return errors.New("installation renderer differs from its pinned image")
	}
	if err := r.rendererInputs(ctx, namespace, podSpec, container, cfg); err != nil {
		return err
	}
	service, err := r.object(ctx, "v1", "Service", "services", namespace, cfg.Service)
	if err != nil {
		return err
	}
	serviceSpec := resourceMap(service, "spec")
	if resourceText(serviceSpec, "type") != "ClusterIP" || net.ParseIP(resourceText(serviceSpec, "clusterIP")) == nil || !sameJSON(serviceSpec["selector"], selector) || serviceSpec["externalName"] != nil || serviceSpec["publishNotReadyAddresses"] == true {
		return errors.New("installation renderer requires a selected internal service")
	}
	ports, ok := serviceSpec["ports"].([]any)
	if !ok || len(ports) > 8 {
		return errors.New("installation renderer service ports are invalid")
	}
	servicePort := int64(0)
	portName := ""
	for _, raw := range ports {
		p, ok := raw.(map[string]any)
		if !ok {
			return errors.New("installation renderer service port is invalid")
		}
		if receiverInteger(p["targetPort"]) == 8081 && resourceText(p, "protocol") == "TCP" {
			if servicePort != 0 {
				return errors.New("installation renderer service has ambiguous ports")
			}
			servicePort = receiverInteger(p["port"])
			portName = resourceText(p, "name")
		}
	}
	if servicePort < 1 || servicePort > 65535 || portName == "" {
		return errors.New("installation renderer service has no qualified gRPC port")
	}
	endpointPath := "apis/discovery.k8s.io/v1/namespaces/" + namespace + "/endpointslices?labelSelector=" + url.QueryEscape("kubernetes.io/service-name="+cfg.Service) + "&limit=129"
	collection, err := r.get(ctx, endpointPath)
	if err != nil {
		return err
	}
	items, ok := collection["items"].([]any)
	if !ok || len(items) == 0 || len(items) > 128 || resourceText(resourceMap(collection, "metadata"), "continue") != "" || resourceMap(collection, "metadata")["remainingItemCount"] != nil {
		return errors.New("installation renderer endpoints are missing or truncated")
	}
	seen := map[string]bool{}
	for _, raw := range items {
		slice, ok := raw.(map[string]any)
		if !ok {
			return errors.New("installation renderer endpoint slice is malformed")
		}
		sliceMeta := resourceMap(slice, "metadata")
		if validLiveStorage(slice, resourceIdentity{Kind: "EndpointSlice", Namespace: namespace, Name: resourceText(sliceMeta, "name")}) != nil ||
			(resourceText(slice, "kind") != "" && resourceText(slice, "kind") != "EndpointSlice") ||
			(resourceText(slice, "apiVersion") != "" && resourceText(slice, "apiVersion") != "discovery.k8s.io/v1") {
			return errors.New("installation renderer endpoint slice identity is invalid")
		}
		if resourceText(sliceMeta, "namespace") != namespace || resourceText(resourceMap(sliceMeta, "labels"), "kubernetes.io/service-name") != cfg.Service || !receiverController(slice, "Service", cfg.Service, resourceText(resourceMap(service, "metadata"), "uid")) || sliceMeta["deletionTimestamp"] != nil {
			return errors.New("installation renderer endpoint slice belongs to another service")
		}
		endpointPorts, ok := slice["ports"].([]any)
		if !ok {
			return errors.New("installation renderer endpoint ports are invalid")
		}
		hasPort := false
		for _, raw := range endpointPorts {
			p, _ := raw.(map[string]any)
			if resourceText(p, "name") == portName && resourceText(p, "protocol") == "TCP" && receiverInteger(p["port"]) == 8081 {
				hasPort = true
			}
		}
		if !hasPort {
			return errors.New("installation renderer endpoint port differs")
		}
		endpoints, ok := slice["endpoints"].([]any)
		if !ok || len(endpoints) > 128 {
			return errors.New("installation renderer endpoint inventory is invalid")
		}
		for _, raw := range endpoints {
			endpoint, ok := raw.(map[string]any)
			if !ok {
				return errors.New("installation renderer endpoint is malformed")
			}
			condition := resourceMap(endpoint, "conditions")
			ref := resourceMap(endpoint, "targetRef")
			if condition["ready"] != true || condition["terminating"] == true || resourceText(ref, "kind") != "Pod" || resourceText(ref, "namespace") != namespace {
				return errors.New("installation renderer endpoint is not a ready named Pod")
			}
			pod, err := r.object(ctx, "v1", "Pod", "pods", namespace, resourceText(ref, "name"))
			if err != nil {
				return err
			}
			uid := resourceText(resourceMap(pod, "metadata"), "uid")
			if uid != resourceText(ref, "uid") || !receiverReady(pod) {
				return errors.New("installation renderer Pod identity or readiness changed")
			}
			if err := s.rendererPod(ctx, r, pod, deployment, podSpec, selector); err != nil {
				return err
			}
			addresses, err := receiverStringList(endpoint["addresses"])
			if err != nil || len(addresses) == 0 {
				return errors.New("installation renderer endpoint addresses are invalid")
			}
			podIPs, _ := resourceMap(pod, "status")["podIPs"].([]any)
			for _, address := range addresses {
				found := false
				for _, raw := range podIPs {
					v, _ := raw.(map[string]any)
					if address == resourceText(v, "ip") && net.ParseIP(address) != nil {
						found = true
					}
				}
				if !found {
					return errors.New("installation renderer endpoint does not address its authenticated Pod")
				}
			}
			seen[uid] = true
		}
	}
	if int64(len(seen)) != desired {
		return errors.New("installation renderer endpoint inventory differs from its replicas")
	}
	// The exact certificate comes from the named Secret via the authenticated API.
	// Pin the leaf as well as validating its chain/SAN; rotation to another leaf
	// under the same CA cannot silently reuse the previous configuration scope.
	pemBody, err := r.secret(ctx, namespace, receiverSecret{Name: cfg.TLSSecret, Key: "tls.crt"})
	if err != nil {
		return err
	}
	block, _ := pem.Decode([]byte(pemBody))
	if block == nil || block.Type != "CERTIFICATE" {
		return errors.New("installation renderer TLS certificate is invalid")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return errors.New("installation renderer TLS certificate is invalid")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(pemBody)) {
		return errors.New("installation renderer TLS trust is invalid")
	}
	serverName := cfg.Service + "." + namespace + ".svc"
	if _, err := cert.Verify(x509.VerifyOptions{DNSName: serverName, Roots: roots}); err != nil {
		return errors.New("installation renderer TLS certificate is expired or has a different identity")
	}
	pinned := append([]byte(nil), cert.Raw...)
	s.rendererTLS = &tls.Config{MinVersion: tls.VersionTLS12, ServerName: serverName, RootCAs: roots, VerifyConnection: func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) == 0 || !bytes.Equal(state.PeerCertificates[0].Raw, pinned) {
			return errors.New("installation renderer certificate changed")
		}
		return nil
	}}
	s.rendererAddress = net.JoinHostPort(serverName, strconv.FormatInt(servicePort, 10))
	return nil
}

func (s *receiverSnapshot) rendererPod(ctx context.Context, r *receiverReads, pod, deployment, podSpec, selector map[string]any) error {
	cfg := s.configuration.Renderer
	namespace := s.configuration.Application.Namespace
	meta := resourceMap(pod, "metadata")
	for key, value := range selector {
		if resourceMap(meta, "labels")[key] != value {
			return errors.New("installation renderer Pod selector differs")
		}
	}
	refs, _ := meta["ownerReferences"].([]any)
	if len(refs) != 1 {
		return errors.New("installation renderer Pod owner is ambiguous")
	}
	owner, _ := refs[0].(map[string]any)
	rs, err := r.object(ctx, "apps/v1", "ReplicaSet", "replicasets", namespace, resourceText(owner, "name"))
	if err != nil {
		return err
	}
	if !receiverController(pod, "ReplicaSet", resourceText(owner, "name"), resourceText(resourceMap(rs, "metadata"), "uid")) || !receiverController(rs, "Deployment", cfg.Deployment, resourceText(resourceMap(deployment, "metadata"), "uid")) {
		return errors.New("installation renderer Pod is not owned by its Deployment")
	}
	// Admission-injected defaults are expected, but executable inputs must equal
	// the observed deployment template. No new command, env or mount is ignored.
	actual := resourceMap(pod, "spec")
	for _, field := range []string{"containers", "initContainers", "volumes"} {
		if !sameJSON(actual[field], podSpec[field]) {
			return errors.New("installation renderer Pod executable configuration differs from its Deployment")
		}
	}
	states, _ := resourceMap(pod, "status")["containerStatuses"].([]any)
	if len(states) != 1 {
		return errors.New("installation renderer container status is incomplete")
	}
	state, _ := states[0].(map[string]any)
	imageID := strings.TrimPrefix(resourceText(state, "imageID"), "docker-pullable://")
	if resourceText(state, "name") != cfg.Container || state["ready"] != true || imageID != cfg.Image || resourceMap(resourceMap(state, "state"), "running") == nil {
		return errors.New("installation renderer runtime image is not the configured immutable image")
	}
	return nil
}

// Names are the executable inputs in the qualified upstream deployment. New
// knobs require an explicit profile review rather than inheriting a prefix.
var receiverRendererEnvironment = map[string]bool{
	"ARGOCD_EXEC_TIMEOUT": true, "REDIS_PASSWORD": true,
	"ARGOCD_RECONCILIATION_TIMEOUT": true, "ARGOCD_REPO_SERVER_LOGFORMAT": true,
	"ARGOCD_REPO_SERVER_LOGLEVEL": true, "ARGOCD_REPO_SERVER_PARALLELISM_LIMIT": true,
	"ARGOCD_REPO_SERVER_LISTEN_ADDRESS": true, "ARGOCD_REPO_SERVER_LISTEN_METRICS_ADDRESS": true,
	"ARGOCD_REPO_SERVER_DISABLE_TLS": true, "ARGOCD_TLS_MIN_VERSION": true,
	"ARGOCD_TLS_MAX_VERSION": true, "ARGOCD_TLS_CIPHERS": true,
	"ARGOCD_REPO_CACHE_EXPIRATION": true, "REDIS_SERVER": true,
	"REDIS_COMPRESSION": true, "REDISDB": true, "ARGOCD_DEFAULT_CACHE_EXPIRATION": true,
	"ARGOCD_REPO_SERVER_OTLP_ADDRESS": true, "ARGOCD_REPO_SERVER_OTLP_INSECURE": true,
	"ARGOCD_REPO_SERVER_OTLP_HEADERS": true, "ARGOCD_REPO_SERVER_MAX_COMBINED_DIRECTORY_MANIFESTS_SIZE": true,
	"ARGOCD_REPO_SERVER_PLUGIN_TAR_EXCLUSIONS": true, "ARGOCD_REPO_SERVER_ALLOW_OUT_OF_BOUNDS_SYMLINKS": true,
	"ARGOCD_REPO_SERVER_STREAMED_MANIFEST_MAX_TAR_SIZE": true, "ARGOCD_REPO_SERVER_STREAMED_MANIFEST_MAX_EXTRACTED_SIZE": true,
	"ARGOCD_REPO_SERVER_HELM_MANIFEST_MAX_EXTRACTED_SIZE": true, "ARGOCD_REPO_SERVER_DISABLE_HELM_MANIFEST_MAX_EXTRACTED_SIZE": true,
	"ARGOCD_REVISION_CACHE_LOCK_TIMEOUT": true, "ARGOCD_GIT_MODULES_ENABLED": true,
	"ARGOCD_GIT_LS_REMOTE_PARALLELISM_LIMIT": true, "ARGOCD_GIT_REQUEST_TIMEOUT": true,
	"ARGOCD_GRPC_MAX_SIZE_MB": true, "ARGOCD_REPO_SERVER_INCLUDE_HIDDEN_DIRECTORIES": true,
	"HELM_CACHE_HOME": true, "HELM_CONFIG_HOME": true, "HELM_DATA_HOME": true,
}

// A Deployment-wide timeout override also reaches copyutil. It does not
// configure /bin/cp; recognize only the same literal as the main container,
// rather than allowing executable-affecting environment or indirect sources.
func receiverCopyInitEnvironment(init, container map[string]any) bool {
	if init["env"] == nil {
		return true
	}
	values, ok := init["env"].([]any)
	if !ok || len(values) != 1 {
		return false
	}
	item, ok := values[0].(map[string]any)
	if !ok || len(item) != 2 || resourceText(item, "name") != "ARGOCD_EXEC_TIMEOUT" || resourceText(item, "value") == "" {
		return false
	}
	main, _ := container["env"].([]any)
	for _, value := range main {
		if sameJSON(value, item) {
			return true
		}
	}
	return false
}

func (r *receiverReads) rendererInputs(ctx context.Context, namespace string, pod, container map[string]any, cfg receiverRenderer) error {
	args, _ := receiverStringList(container["args"])
	command, _ := receiverStringList(container["command"])
	wanted := []string{"/usr/local/bin/argocd-repo-server"}
	if !(len(command) == 0 && sameJSON(args, wanted)) && !(len(args) == 0 && sameJSON(command, wanted)) {
		return errors.New("installation renderer command requires a qualified profile")
	}
	if container["envFrom"] != nil || pod["ephemeralContainers"] != nil {
		return errors.New("installation renderer has unqualified executable inputs")
	}
	if raw, found := pod["initContainers"]; found {
		values, ok := raw.([]any)
		if !ok || len(values) > 1 {
			return errors.New("installation renderer init containers require qualification")
		}
		for _, raw := range values {
			init, _ := raw.(map[string]any)
			command, _ := receiverStringList(init["command"])
			if resourceText(init, "image") != cfg.Image || !sameJSON(command, []string{"/bin/cp", "-n", "/usr/local/bin/argocd", "/var/run/argocd/argocd-cmp-server"}) || init["args"] != nil || !receiverCopyInitEnvironment(init, container) || init["envFrom"] != nil {
				return errors.New("installation renderer init container requires qualification")
			}
		}
	}
	env, _ := container["env"].([]any)
	if len(env) > 128 {
		return errors.New("installation renderer environment exceeds its bound")
	}
	names := map[string]bool{}
	for _, raw := range env {
		item, ok := raw.(map[string]any)
		if !ok {
			return errors.New("installation renderer environment is invalid")
		}
		name := resourceText(item, "name")
		if names[name] || !receiverRendererEnvironment[name] {
			return errors.New("installation renderer environment requires a qualified profile")
		}
		names[name] = true
		if (name == "REDIS_PASSWORD" || name == "ARGOCD_REPO_SERVER_OTLP_HEADERS") && item["value"] != nil {
			return errors.New("installation renderer credentials require a versioned reference")
		}
		value := resourceText(item, "value")
		if from := resourceMap(item, "valueFrom"); from != nil {
			if len(from) != 1 || item["value"] != nil {
				return errors.New("installation renderer environment source is ambiguous")
			}
			var obj map[string]any
			var err error
			if ref := resourceMap(from, "configMapKeyRef"); ref != nil {
				obj, err = r.object(ctx, "v1", "ConfigMap", "configmaps", namespace, resourceText(ref, "name"))
				if err != nil {
					return err
				}
				value = resourceText(resourceMap(obj, "data"), resourceText(ref, "key"))
			} else if ref := resourceMap(from, "secretKeyRef"); ref != nil {
				value, err = r.secret(ctx, namespace, receiverSecret{Name: resourceText(ref, "name"), Key: resourceText(ref, "key")})
				if err != nil {
					return err
				}
			} else {
				return errors.New("installation renderer environment source is unsupported")
			}
		}
		if name == "ARGOCD_REPO_SERVER_DISABLE_TLS" && value != "" && value != "false" {
			return errors.New("installation renderer TLS is disabled")
		}
	}
	volumes, _ := pod["volumes"].([]any)
	if len(volumes) > 32 {
		return errors.New("installation renderer volumes exceed their bound")
	}
	tlsVolume := ""
	for _, raw := range volumes {
		volume, ok := raw.(map[string]any)
		if !ok {
			return errors.New("installation renderer volume is invalid")
		}
		switch {
		case volume["configMap"] != nil:
			ref := resourceMap(volume, "configMap")
			if _, err := r.object(ctx, "v1", "ConfigMap", "configmaps", namespace, resourceText(ref, "name")); err != nil {
				return err
			}
		case volume["secret"] != nil:
			ref := resourceMap(volume, "secret")
			name := resourceText(ref, "secretName")
			if _, err := r.object(ctx, "v1", "Secret", "secrets", namespace, name); err != nil {
				return err
			}
			if name == cfg.TLSSecret {
				tlsVolume = resourceText(volume, "name")
			}
		case volume["emptyDir"] != nil:
		default:
			return errors.New("installation renderer volume type requires qualification")
		}
	}
	mounts, _ := container["volumeMounts"].([]any)
	tlsMounted := false
	for _, raw := range mounts {
		mount, _ := raw.(map[string]any)
		path := resourceText(mount, "mountPath")
		// The qualified image supplies all executables; shared volumes cannot
		// replace them. Existing config, scratch and plugin socket paths are scoped.
		allowed := map[string]bool{"/app/config/ssh": true, "/app/config/tls": true, "/app/config/gpg/source": true, "/app/config/gpg/keys": true, "/app/config/reposerver/tls": true, "/tmp": true, "/helm-working-dir": true, "/home/argocd/cmp-server/plugins": true}
		if !allowed[path] || mount["subPath"] != nil || mount["subPathExpr"] != nil {
			return errors.New("installation renderer mount requires qualification")
		}
		if path == "/app/config/reposerver/tls" && resourceText(mount, "name") == tlsVolume && tlsVolume != "" {
			tlsMounted = true
		}
	}
	if !tlsMounted {
		return errors.New("installation renderer does not mount its configured TLS Secret")
	}
	return nil
}

// Render opens only the snapshot's verified TLS endpoint. Upstream errors never
// disclose credentials; the caller reobserves configuration before promotion.
func (s *receiverSnapshot) renderRevision(ctx context.Context, spec argocd.RenderSpec, credential argocd.RepositoryCredentials) (argocd.RenderedRevision, error) {
	if s == nil || s.rendererTLS == nil || s.digest == "" {
		return argocd.RenderedRevision{}, errors.New("installation renderer has no authenticated configuration")
	}
	conn, err := grpc.NewClient(s.rendererAddress, grpc.WithTransportCredentials(credentials.NewTLS(s.rendererTLS.Clone())))
	if err != nil {
		return argocd.RenderedRevision{}, errors.New("installation renderer connection is unavailable")
	}
	defer conn.Close()
	return argocd.RenderRevision(ctx, conn, spec, credential)
}

// Collection resourceVersion advances for unrelated writes. Bind the ordered
// item identities/versions instead, while still detecting additions/removals.
func receiverCollectionIdentity(body map[string]any) any {
	items, _ := body["items"].([]any)
	encoded := make([]string, 0, len(items))
	for _, item := range items {
		raw, _ := json.Marshal(item)
		encoded = append(encoded, string(raw))
	}
	sort.Strings(encoded)
	return []any{body["apiVersion"], body["kind"], encoded, resourceMap(body, "metadata")["continue"], resourceMap(body, "metadata")["remainingItemCount"]}
}
