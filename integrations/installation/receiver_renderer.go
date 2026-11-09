package installation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
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
	tlsPath, err := receiverPath("v1", "secrets", namespace, cfg.TLSSecret)
	if err != nil {
		return err
	}
	// Every other input is read before the renderer's, so a TLS Secret already
	// read here is also another input (a named repository credential, say),
	// whose content its trust anchors must not stand in for.
	_, tlsShared := r.reads[tlsPath]
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
	servingOnly, err := r.rendererInputs(ctx, namespace, podSpec, container, cfg)
	if err != nil {
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
	// Every restart, reschedule or readiness flap rewrites the EndpointSlices
	// (resourceVersion, generation, endpoint trigger time). The checks below
	// run at every call, so they are proof enough and stay out of the saved
	// digest, like the Pods and ReplicaSets they lead to (receiverObjectBinding):
	// each ready endpoint must address a Pod of the bound Deployment that runs
	// its current template and image, and there must be as many as it wants.
	r.fresh(endpointPath)
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
	// The renderer's certificate comes from the named Secret via the
	// authenticated API. The receiver trusts the Secret's issuing authority, not
	// one leaf: cert-manager reissues the leaf about every 75 days and Argo 2.13
	// serves the old one until the repo-server restarts, so pinning the leaf
	// stranded every started plan at each renewal. The trust anchors are the
	// Secret's ca.crt, which cert-manager writes, or else the top of its tls.crt
	// chain, and each must be self-signed. Each call verifies that the Secret's
	// leaf, and the certificate the repo-server presents in the TLS handshake,
	// chain to those anchors and name the Service; the anchors' names and public
	// keys are bound in place of the Secret's content when the repo-server reads
	// it only as its serving certificate, so a different CA refuses and a
	// renewal under the same one does not. A self-signed leaf is its own anchor:
	// it survives renewal only when its key is kept (cert-manager
	// privateKey.rotationPolicy: Never), and until the repo-server restarts its
	// old leaf no longer verifies, as before.
	// Regressions: TestReceiverRendererTrustAnchorSurvivesRenewalButNotANewCA and
	// TestReceiverSelfSignedRendererAnchorIsItsKey.
	pemBody, err := r.secret(ctx, namespace, receiverSecret{Name: cfg.TLSSecret, Key: "tls.crt"})
	if err != nil {
		return err
	}
	chain, err := receiverCertificates([]byte(pemBody))
	if err != nil {
		return errors.New("installation renderer TLS certificate is invalid")
	}
	anchors := chain[len(chain)-1:]
	tlsRead := r.reads[tlsPath]
	if authority := resourceText(resourceMap(tlsRead.body, "data"), "ca.crt"); authority != "" {
		decoded, err := base64.StdEncoding.Strict().DecodeString(authority)
		if err != nil {
			return errors.New("installation renderer TLS trust is invalid")
		}
		if anchors, err = receiverCertificates(decoded); err != nil {
			return errors.New("installation renderer TLS trust is invalid")
		}
	}
	// Only a self-signed certificate is a trust anchor: a CA's root, or a
	// self-signed leaf such as cert-manager's selfSigned issuer writes (with
	// itself as ca.crt). Go's verifier checks neither an anchor's signature nor
	// who issued it, so an intermediate in ca.crt, or an intermediate or issued
	// leaf at the top of a tls.crt whose root was left out, would be trusted on
	// its face, its own issuer, validity and signature unchecked.
	// Regression: TestReceiverRendererTrustAnchorMustBeSelfSigned.
	for _, anchor := range anchors {
		if !receiverSelfSigned(anchor) {
			return errors.New("installation renderer TLS trust anchor is not self-signed: the Secret's ca.crt, or the top of its tls.crt chain when it has no ca.crt, must be a root CA or a self-signed certificate")
		}
	}
	roots, intermediates := x509.NewCertPool(), x509.NewCertPool()
	for _, anchor := range anchors {
		roots.AddCert(anchor)
	}
	for _, certificate := range chain[1:] {
		intermediates.AddCert(certificate)
	}
	serverName := cfg.Service + "." + namespace + ".svc"
	if _, err := chain[0].Verify(x509.VerifyOptions{DNSName: serverName, Roots: roots, Intermediates: intermediates}); err != nil {
		return errors.New("installation renderer TLS certificate is expired or has a different identity")
	}
	// Standard verification of the served chain against only these anchors, with
	// the Service's DNS name: no system roots and no permissive Argo default.
	s.rendererTLS = &tls.Config{MinVersion: tls.VersionTLS12, ServerName: serverName, RootCAs: roots}
	// The anchors stand in for this Secret's content only while the repo-server
	// consumes it solely as its serving certificate (rendererInputs) and no other
	// input is this Secret: then a renewal under the same anchor changes nothing
	// a render depends on. Anything else (a second volume or mount, another
	// projected key, an environment reference, a repository credential kept
	// here) leaves the keyed content binding every Secret read gets
	// (receiverObjectBinding), so any changed value refuses.
	// Regression: TestReceiverRendererTLSSecretNarrowsOnlyForItsQualifiedMount.
	if servingOnly && !tlsShared {
		r.bind(tlsPath, []any{"v1", "Secret", namespace, cfg.TLSSecret, tlsRead.uid, "trust-anchors-v1", receiverTrustAnchors(anchors)})
	}
	s.rendererAddress = net.JoinHostPort(serverName, strconv.FormatInt(servicePort, 10))
	return nil
}

// receiverCertificates parses every certificate of a PEM bundle, in order. Any
// other block refuses, as the previous single-certificate parser did.
func receiverCertificates(body []byte) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	for len(body) > 0 {
		block, rest := pem.Decode(body)
		if block == nil {
			break
		}
		body = rest
		if block.Type != "CERTIFICATE" {
			return nil, errors.New("installation renderer TLS bundle holds something other than certificates")
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		out = append(out, certificate)
		if len(out) > 16 {
			return nil, errors.New("installation renderer TLS bundle exceeds its bound")
		}
	}
	if len(out) == 0 {
		return nil, errors.New("installation renderer TLS bundle has no certificate")
	}
	return out, nil
}

// receiverSelfSigned reports whether a certificate can be a trust anchor: it
// names itself as its issuer and its own key verifies its signature. The
// signature is checked directly, not with CheckSignatureFrom, which also
// requires a CA certificate: a self-signed serving leaf is not one.
func receiverSelfSigned(certificate *x509.Certificate) bool {
	return bytes.Equal(certificate.RawSubject, certificate.RawIssuer) &&
		certificate.CheckSignature(certificate.SignatureAlgorithm, certificate.RawTBSCertificate, certificate.Signature) == nil
}

// A trust anchor is a name and a public key (RFC 5280, section 6.1.1): a CA
// certificate reissued under the same name and key is the same anchor.
func receiverTrustAnchors(anchors []*x509.Certificate) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, anchor := range anchors {
		name, key := sha256.Sum256(anchor.RawSubject), sha256.Sum256(anchor.RawSubjectPublicKeyInfo)
		value := hex.EncodeToString(name[:]) + ":" + hex.EncodeToString(key[:])
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
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
	// The Pods are verified fresh at every call instead of being saved
	// (receiverObjectBinding), so this check is what proves a serving Pod runs
	// the bound Deployment's current template; Deployment status counters alone
	// can lag a controller sync or be written by anyone with deployments/status.
	if err := receiverCurrentRevision(pod, rs, deployment); err != nil {
		return err
	}
	// Admission-injected defaults are expected, but executable inputs must equal
	// the observed deployment template. No new command, env or mount is ignored.
	actual := resourceMap(pod, "spec")
	for _, field := range []string{"containers", "initContainers", "volumes"} {
		if !sameJSON(actual[field], podSpec[field]) {
			return errors.New("installation renderer Pod executable configuration differs from its Deployment")
		}
	}
	// A debug container attached with `kubectl debug` shares the renderer's
	// network and process namespaces and can read its serving key, yet appears
	// in no template. v1 refused it by binding each Pod's whole spec; v2 saves no
	// Pod, so the live Pod is refused here, attached before or after preparation.
	if receiverListed(actual["ephemeralContainers"]) || receiverListed(resourceMap(pod, "status")["ephemeralContainerStatuses"]) {
		return errors.New("installation renderer Pod has an attached ephemeral container")
	}
	// What the Pod runs as and how it reaches its sources, beyond its containers
	// and volumes: a mutating webhook could change these on the next restart
	// (hostAliases or dnsConfig redirecting the Git host, another service
	// account), and every restart is now accepted by design.
	if !sameJSON(receiverPodContext(actual), receiverPodContext(podSpec)) {
		return errors.New("installation renderer Pod runs with another identity or host context than its Deployment template")
	}
	states, _ := resourceMap(pod, "status")["containerStatuses"].([]any)
	if len(states) != 1 {
		return errors.New("installation renderer container status is incomplete")
	}
	state, _ := states[0].(map[string]any)
	imageID := strings.TrimPrefix(resourceText(state, "imageID"), "docker-pullable://")
	// The Deployment carries the configured text verbatim, but the kubelet
	// reports repository@digest. Normalize a repo:tag@digest pin, which is how
	// kustomize renders newTag plus digest, before comparing the runtime.
	pinned, err := immutableImage(cfg.Image)
	if err != nil || resourceText(state, "name") != cfg.Container || state["ready"] != true || imageID != pinned || resourceMap(resourceMap(state, "state"), "running") == nil {
		return errors.New("installation renderer runtime image is not the configured immutable image")
	}
	return nil
}

// receiverTemplateHash is the label the Deployment controller adds to each
// revision's ReplicaSet, its pod template and its Pods.
const receiverTemplateHash = "pod-template-hash"

// receiverCurrentRevision proves a renderer Pod belongs to its Deployment's
// current revision, from the ReplicaSet already read by name: the ReplicaSet's
// pod template equals the Deployment's ignoring the hash label (the
// controller's own EqualIgnoreHash test for the current revision), and the Pod
// carries that ReplicaSet's hash. A Pod of an older ReplicaSet of the same
// Deployment refuses, however its status counters read.
func receiverCurrentRevision(pod, rs, deployment map[string]any) error {
	template := resourceMap(resourceMap(rs, "spec"), "template")
	hash := resourceText(resourceMap(resourceMap(template, "metadata"), "labels"), receiverTemplateHash)
	if hash == "" || resourceText(resourceMap(resourceMap(pod, "metadata"), "labels"), receiverTemplateHash) != hash {
		return errors.New("installation renderer Pod is not of its ReplicaSet's pod template")
	}
	if !sameJSON(receiverWithoutTemplateHash(template), receiverWithoutTemplateHash(resourceMap(resourceMap(deployment, "spec"), "template"))) {
		return errors.New("installation renderer Pod is not of its Deployment's current revision")
	}
	return nil
}

// receiverWithoutTemplateHash copies a pod template without the hash label;
// the read bodies are reobserved and must never be mutated.
func receiverWithoutTemplateHash(template map[string]any) map[string]any {
	meta := resourceMap(template, "metadata")
	labels := resourceMap(meta, "labels")
	if _, present := labels[receiverTemplateHash]; !present {
		return template
	}
	out, outMeta, outLabels := map[string]any{}, map[string]any{}, map[string]any{}
	for key, value := range template {
		out[key] = value
	}
	for key, value := range meta {
		outMeta[key] = value
	}
	for key, value := range labels {
		if key != receiverTemplateHash {
			outLabels[key] = value
		}
	}
	outMeta["labels"] = outLabels
	out["metadata"] = outMeta
	return out
}

func receiverListed(value any) bool {
	list, ok := value.([]any)
	return value != nil && (!ok || len(list) > 0)
}

// receiverPodContext is what decides a Pod's identity and host context beyond
// its containers and volumes. The API server defaults a Pod, and not its
// template, with enableServiceLinks true, and the ServiceAccount admission
// plugin names "default" when the template names no service account, so those
// two compare normalized; the qualified profile admits nothing that sets the
// rest. Scheduling fields admission does set on Pods (tolerations, priority,
// overhead, nodeName) are left out: they decide where a Pod runs, not what.
func receiverPodContext(spec map[string]any) map[string]any {
	out := map[string]any{}
	for _, field := range []string{"automountServiceAccountToken", "hostUsers", "runtimeClassName", "hostAliases", "dnsConfig", "os"} {
		out[field] = spec[field]
	}
	for _, field := range []string{"hostNetwork", "hostPID", "hostIPC", "shareProcessNamespace"} {
		out[field] = spec[field] == true
	}
	security := resourceMap(spec, "securityContext")
	if security == nil {
		security = map[string]any{}
	}
	out["securityContext"] = security
	account := resourceText(spec, "serviceAccountName")
	if account == "" {
		account = resourceText(spec, "serviceAccount")
	}
	if account == "" {
		account = "default"
	}
	out["serviceAccountName"] = account
	dns := resourceText(spec, "dnsPolicy")
	if dns == "" {
		dns = "ClusterFirst"
	}
	out["dnsPolicy"] = dns
	out["enableServiceLinks"] = spec["enableServiceLinks"] != false
	return out
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

// receiverRendererTLSMount is where the qualified repo-server reads its
// serving certificate and key.
const receiverRendererTLSMount = "/app/config/reposerver/tls"

// receiverServingFiles are the keys the qualified repo-server volume projects
// into receiverRendererTLSMount: its serving certificate, key and CA bundle.
var receiverServingFiles = map[string]bool{"tls.crt": true, "tls.key": true, "ca.crt": true}

// receiverServingProjection reports whether a TLS Secret volume projects only
// the serving files: its items name no other key or, with no items, the
// Secret holds no other key, since every key then becomes a file.
func receiverServingProjection(source, secret map[string]any) bool {
	items, ok := source["items"].([]any)
	if source["items"] != nil && !ok {
		return false
	}
	if len(items) == 0 {
		for key := range resourceMap(secret, "data") {
			if !receiverServingFiles[key] {
				return false
			}
		}
		return true
	}
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if !receiverServingFiles[resourceText(item, "key")] {
			return false
		}
	}
	return true
}

// rendererInputs reads and qualifies everything the renderer template
// consumes. It reports whether the repo-server consumes its TLS Secret solely
// as its serving certificate: one volume, mounted once, by the repo-server
// container at receiverRendererTLSMount, projecting only the serving files,
// and read through no environment variable. Only then may observeRenderer
// bind that Secret by its trust anchors instead of its content.
func (r *receiverReads) rendererInputs(ctx context.Context, namespace string, pod, container map[string]any, cfg receiverRenderer) (bool, error) {
	args, _ := receiverStringList(container["args"])
	command, _ := receiverStringList(container["command"])
	wanted := []string{"/usr/local/bin/argocd-repo-server"}
	if !(len(command) == 0 && sameJSON(args, wanted)) && !(len(args) == 0 && sameJSON(command, wanted)) {
		return false, errors.New("installation renderer command requires a qualified profile")
	}
	if container["envFrom"] != nil || pod["ephemeralContainers"] != nil {
		return false, errors.New("installation renderer has unqualified executable inputs")
	}
	servingOnly := true
	inits, _ := pod["initContainers"].([]any)
	if raw, found := pod["initContainers"]; found {
		values, ok := raw.([]any)
		if !ok || len(values) > 1 {
			return false, errors.New("installation renderer init containers require qualification")
		}
		for _, raw := range values {
			init, _ := raw.(map[string]any)
			command, _ := receiverStringList(init["command"])
			if resourceText(init, "image") != cfg.Image || !sameJSON(command, []string{"/bin/cp", "-n", "/usr/local/bin/argocd", "/var/run/argocd/argocd-cmp-server"}) || init["args"] != nil || !receiverCopyInitEnvironment(init, container) || init["envFrom"] != nil {
				return false, errors.New("installation renderer init container requires qualification")
			}
		}
	}
	env, _ := container["env"].([]any)
	if len(env) > 128 {
		return false, errors.New("installation renderer environment exceeds its bound")
	}
	names := map[string]bool{}
	for _, raw := range env {
		item, ok := raw.(map[string]any)
		if !ok {
			return false, errors.New("installation renderer environment is invalid")
		}
		name := resourceText(item, "name")
		if names[name] || !receiverRendererEnvironment[name] {
			return false, errors.New("installation renderer environment requires a qualified profile")
		}
		names[name] = true
		if (name == "REDIS_PASSWORD" || name == "ARGOCD_REPO_SERVER_OTLP_HEADERS") && item["value"] != nil {
			return false, errors.New("installation renderer credentials require a versioned reference")
		}
		value := resourceText(item, "value")
		if from := resourceMap(item, "valueFrom"); from != nil {
			if len(from) != 1 || item["value"] != nil {
				return false, errors.New("installation renderer environment source is ambiguous")
			}
			var obj map[string]any
			var err error
			if ref := resourceMap(from, "configMapKeyRef"); ref != nil {
				obj, err = r.object(ctx, "v1", "ConfigMap", "configmaps", namespace, resourceText(ref, "name"))
				if err != nil {
					return false, err
				}
				value = resourceText(resourceMap(obj, "data"), resourceText(ref, "key"))
			} else if ref := resourceMap(from, "secretKeyRef"); ref != nil {
				// Any key of the TLS Secret in the environment, tls.crt included,
				// is configuration its trust anchors do not cover.
				if resourceText(ref, "name") == cfg.TLSSecret {
					servingOnly = false
				}
				value, err = r.secret(ctx, namespace, receiverSecret{Name: resourceText(ref, "name"), Key: resourceText(ref, "key")})
				if err != nil {
					return false, err
				}
			} else {
				return false, errors.New("installation renderer environment source is unsupported")
			}
		}
		if name == "ARGOCD_REPO_SERVER_DISABLE_TLS" && value != "" && value != "false" {
			return false, errors.New("installation renderer TLS is disabled")
		}
	}
	volumes, _ := pod["volumes"].([]any)
	if len(volumes) > 32 {
		return false, errors.New("installation renderer volumes exceed their bound")
	}
	tlsVolumes := map[string]bool{}
	for _, raw := range volumes {
		volume, ok := raw.(map[string]any)
		if !ok {
			return false, errors.New("installation renderer volume is invalid")
		}
		switch {
		case volume["configMap"] != nil:
			ref := resourceMap(volume, "configMap")
			if _, err := r.object(ctx, "v1", "ConfigMap", "configmaps", namespace, resourceText(ref, "name")); err != nil {
				return false, err
			}
		case volume["secret"] != nil:
			ref := resourceMap(volume, "secret")
			name := resourceText(ref, "secretName")
			secret, err := r.object(ctx, "v1", "Secret", "secrets", namespace, name)
			if err != nil {
				return false, err
			}
			if name == cfg.TLSSecret {
				tlsVolumes[resourceText(volume, "name")] = true
				servingOnly = servingOnly && receiverServingProjection(ref, secret)
			}
		case volume["emptyDir"] != nil:
		default:
			return false, errors.New("installation renderer volume type requires qualification")
		}
	}
	if len(tlsVolumes) != 1 {
		servingOnly = false
	}
	mounts, _ := container["volumeMounts"].([]any)
	tlsMounted := false
	for _, raw := range mounts {
		mount, _ := raw.(map[string]any)
		path := resourceText(mount, "mountPath")
		// The qualified image supplies all executables; shared volumes cannot
		// replace them. Existing config, scratch and plugin socket paths are scoped.
		allowed := map[string]bool{"/app/config/ssh": true, "/app/config/tls": true, "/app/config/gpg/source": true, "/app/config/gpg/keys": true, receiverRendererTLSMount: true, "/tmp": true, "/helm-working-dir": true, "/home/argocd/cmp-server/plugins": true}
		if !allowed[path] || mount["subPath"] != nil || mount["subPathExpr"] != nil {
			return false, errors.New("installation renderer mount requires qualification")
		}
		if tlsVolumes[resourceText(mount, "name")] {
			if path == receiverRendererTLSMount {
				tlsMounted = true
			} else {
				// /app/config/tls, for one, is where Argo reads the CAs it trusts
				// for Git hosts.
				servingOnly = false
			}
		}
	}
	// The copy helper's mounts are not otherwise scoped: any mount of the TLS
	// Secret there is another consumer.
	for _, raw := range inits {
		init, _ := raw.(map[string]any)
		initMounts, _ := init["volumeMounts"].([]any)
		for _, raw := range initMounts {
			mount, _ := raw.(map[string]any)
			if tlsVolumes[resourceText(mount, "name")] {
				servingOnly = false
			}
		}
	}
	if !tlsMounted {
		return false, errors.New("installation renderer does not mount its configured TLS Secret")
	}
	return servingOnly, nil
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
