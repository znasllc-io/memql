package installation

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/integrations/argocd"
)

// A read set belongs to one observation: reobserve rereads every read and
// refuses a resourceVersion that moved while the observation ran. What survives
// the observation is a digest, compared with the one saved at preparation
// before Apply, at completion and at rollback.
//
// That digest binds what each input MEANS, not its resourceVersion
// (receiving-configuration-v2, receiverBinding). Engines up to 0.31.28 bound
// resourceVersions, every renderer Pod, ReplicaSet and EndpointSlice, and the
// whole discovery surface (v1, read.legacy), so a repo-server restart, a
// Kubernetes patch, a new CRD or an annotation-only write stranded every started
// plan. Both digests are still computed: a plan saved under v1 verifies against
// v1, with v1's strictness (receiverSnapshot.matchesInvariant).
// Regressions: receiver_churn_test.go, receiver_discovery_test.go and, through
// the real journal, TestInstallationUpdateSurvivesReceiverChurnThroughCompletionAndRestore.
type receiverRead struct {
	path, uid, version string
	body               map[string]any
	// legacy is the binding v1 hashed. Never change what it holds: plans saved
	// by engines up to 0.31.28 compare against it
	// (TestReceiverLegacyDigestsReproduceTheV1Engine).
	legacy any
	// binding is what v2 hashes for this read.
	binding receiverBinding
}

// receiverBindingKind says how a read enters the v2 digest. Every saved object
// binding keeps the object's uid, so a deleted and recreated object still
// refuses; only reads proven fresh at every call are left out.
type receiverBindingKind uint8

const (
	// receiverBindLegacy binds what v1 bound. Applications keep their own
	// history-backed continuity (application_continuity.go), and a read with no
	// qualified meaning fails closed this way.
	receiverBindLegacy receiverBindingKind = iota
	// receiverBindValue binds a public value: an identity, a spec, the
	// renderer's pod template or its TLS trust anchors.
	receiverBindValue
	// receiverBindContent binds a ConfigMap's or Secret's uid and a keyed digest
	// of its content (contentDigest); labels, annotations and resourceVersion
	// stay out, so an annotation-only write cannot strand a plan.
	receiverBindContent
	// receiverBindContentList binds each listed Secret's name, uid and keyed
	// content digest: the repository inventory.
	receiverBindContentList
	// receiverBindFresh keeps the read out of every saved digest because it is
	// verified from live state at every call instead: renderer Pods, ReplicaSets
	// and EndpointSlices (observeRenderer), and raw discovery documents, which
	// the discovery contract replaces (receiverDiscoveryContract).
	receiverBindFresh
)

type receiverBinding struct {
	kind  receiverBindingKind
	value any
	// labels are metadata a content binding also keeps because a consumer acts
	// on them (bindLabel); every other label stays out.
	labels map[string]any
}

type receiverReads struct {
	api   argocd.API
	reads map[string]receiverRead
	// key keys every content digest; see useBindingKey. It never leaves memory.
	key []byte
	// contract is the Kubernetes discovery contract v2 binds in place of the
	// raw discovery documents (receiverDiscoveryContract).
	contract any
}

// receiverDigests carries one observation in both formats: current (v2) is
// what this engine saves; legacy (v1) only verifies a plan an engine up to
// 0.31.28 saved.
type receiverDigests struct{ current, legacy string }

func newReceiverReads(api argocd.API) *receiverReads {
	return &receiverReads{api: api, reads: map[string]receiverRead{}}
}
func (r *receiverReads) String() string   { return "[private installation configuration reads]" }
func (r *receiverReads) GoString() string { return r.String() }

func receiverJSON(body []byte, value any) error {
	if len(body) == 0 || len(body) > 4<<20 {
		return errors.New("installation configuration exceeds its bound")
	}
	if err := receiverUniqueJSON(json.NewDecoder(bytes.NewReader(body)), 0); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	d.DisallowUnknownFields()
	if d.Decode(value) != nil || d.Decode(new(any)) != io.EOF {
		return errors.New("installation configuration has invalid or unsupported fields")
	}
	return nil
}
func receiverUniqueJSON(d *json.Decoder, depth int) error {
	if depth > 64 {
		return errors.New("installation configuration nesting exceeds its bound")
	}
	token, err := d.Token()
	if err != nil {
		return errors.New("installation configuration is malformed")
	}
	switch token {
	case json.Delim('{'):
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return errors.New("installation configuration is malformed")
			}
			name, ok := key.(string)
			if !ok || seen[strings.ToLower(name)] {
				return errors.New("installation configuration contains ambiguous fields")
			}
			seen[strings.ToLower(name)] = true
			if err := receiverUniqueJSON(d, depth+1); err != nil {
				return err
			}
		}
		_, err = d.Token()
	case json.Delim('['):
		for d.More() {
			if err := receiverUniqueJSON(d, depth+1); err != nil {
				return err
			}
		}
		_, err = d.Token()
	}
	return err
}
func (r *receiverReads) get(ctx context.Context, path string) (map[string]any, error) {
	if read, ok := r.reads[path]; ok {
		return read.body, nil
	}
	if r.api == nil || len(r.reads) >= 256 || ctx.Err() != nil {
		return nil, errors.New("installation configuration read scope is unavailable")
	}
	raw, err := r.api.Do(ctx, http.MethodGet, path, "", nil)
	if err != nil {
		return nil, errors.New("installation configuration resource could not be read")
	}
	var body map[string]any
	if receiverJSON(raw, &body) != nil || body == nil {
		return nil, errors.New("installation configuration resource is malformed")
	}
	meta := resourceMap(body, "metadata")
	uid, version := resourceText(meta, "uid"), resourceText(meta, "resourceVersion")
	r.reads[path] = receiverRead{path: path, uid: uid, version: version, body: body}
	return body, nil
}
func receiverPath(version, plural, namespace, name string) (string, error) {
	if !receiverDNS.MatchString(namespace) || len(namespace) > 63 || !kubernetesSegment.MatchString(name) {
		return "", errors.New("installation resource name is invalid")
	}
	prefix := "apis/" + version
	if version == "v1" {
		prefix = "api/v1"
	}
	return prefix + "/namespaces/" + namespace + "/" + plural + "/" + name, nil
}
func (r *receiverReads) object(ctx context.Context, version, kind, plural, namespace, name string) (map[string]any, error) {
	path, err := receiverPath(version, plural, namespace, name)
	if err != nil {
		return nil, err
	}
	previous, seen := r.reads[path]
	body, err := r.get(ctx, path)
	if err != nil {
		return nil, err
	}
	if resourceText(body, "apiVersion") != version || resourceText(body, "kind") != kind || validLiveStorage(body, resourceIdentity{Kind: kind, Namespace: namespace, Name: name}) != nil {
		return nil, errors.New("installation configuration identity is absent, changed or deleting")
	}
	read := r.reads[path]
	// v1: configuration data and Secret data are bound by their authenticated
	// versions, renderer objects and Applications by identity, metadata and spec.
	read.legacy = []string{version, kind, namespace, name, read.uid, read.version}
	if kind == "Application" || kind == "Deployment" || kind == "ReplicaSet" || kind == "Pod" {
		meta := resourceMap(body, "metadata")
		// Status writes do not change configuration. Recheck exact RV during this
		// observation, but do not invalidate recovery for a status-only heartbeat.
		read.legacy = []any{version, kind, namespace, name, read.uid, meta["generation"], meta["labels"], meta["annotations"], meta["ownerReferences"], body["spec"]}
		if kind == "Application" {
			// An Argo CRD without a /status subresource increments generation on
			// status writes. Native preparation and writes instead require retained
			// complete Application history. Keep spec/identity/metadata here and
			// the exact RV fence within this individual authenticated observation.
			read.legacy = []any{version, kind, namespace, name, read.uid, "application-history-v1", meta["labels"], meta["annotations"], meta["ownerReferences"], body["spec"]}
		}
	}
	// A second read of the same object (the renderer's TLS Secret is both a
	// volume and the trust source) keeps a binding its reader already chose.
	if !seen || previous.binding.kind == receiverBindLegacy {
		read.binding = receiverObjectBinding(version, kind, namespace, name, read.uid, body)
	}
	r.reads[path] = read
	return body, nil
}

// receiverObjectBinding is what each object the receiver reads means.
func receiverObjectBinding(version, kind, namespace, name, uid string, body map[string]any) receiverBinding {
	identity := []any{version, kind, namespace, name, uid}
	spec := resourceMap(body, "spec")
	switch kind {
	case "ConfigMap", "Secret":
		// Content, never resourceVersion: cert-manager, External Secrets, Argo's
		// own apply and an operator's expiry annotation all rewrite metadata
		// without changing what the object means. A changed value still refuses.
		return receiverBinding{kind: receiverBindContent, value: identity}
	case "Pod", "ReplicaSet":
		// A repo-server restart, eviction or node upgrade replaces these. Each
		// call instead proves its serving Pods belong to the bound Deployment,
		// run its template and are ready (observeRenderer, rendererPod).
		return receiverBinding{kind: receiverBindFresh}
	case "Deployment":
		// The renderer Deployment's identity, selector and pod template: its
		// images, arguments, environment and volumes. Replicas, status,
		// generation and the deployment.kubernetes.io/revision annotation move on
		// a scale or a rollout restart that changes nothing it runs.
		return receiverBinding{kind: receiverBindValue, value: append(identity, spec["selector"], receiverPodTemplate(spec["template"]))}
	case "Service", "AppProject":
		// What routes to the renderer, and what the Argo project permits.
		// Status and metadata are controller and apply bookkeeping.
		return receiverBinding{kind: receiverBindValue, value: append(identity, body["spec"])}
	}
	return receiverBinding{kind: receiverBindLegacy}
}

// receiverRestartAnnotation is the pod-template annotation kubectl rollout
// restart and Argo CD's restart action write to roll every Pod while running
// the same thing. It is the only template field left out of the binding: other
// restart markers, such as Stakater Reloader's annotation or environment
// strategies, change the bound template and still refuse a started plan.
const receiverRestartAnnotation = "kubectl.kubernetes.io/restartedAt"

func receiverPodTemplate(value any) any {
	template, ok := value.(map[string]any)
	if !ok {
		return value
	}
	meta := resourceMap(template, "metadata")
	annotations := resourceMap(meta, "annotations")
	if _, present := annotations[receiverRestartAnnotation]; !present {
		return template
	}
	// Copy: the captured body is reobserved and must never be mutated.
	out, outMeta, outAnnotations := map[string]any{}, map[string]any{}, map[string]any{}
	for key, value := range template {
		out[key] = value
	}
	for key, value := range meta {
		outMeta[key] = value
	}
	for key, value := range annotations {
		if key != receiverRestartAnnotation {
			outAnnotations[key] = value
		}
	}
	if len(outAnnotations) == 0 {
		delete(outMeta, "annotations")
	} else {
		outMeta["annotations"] = outAnnotations
	}
	out["metadata"] = outMeta
	return out
}

// bind and fresh record a reader's own choice for a read it made.
func (r *receiverReads) bind(path string, value any) {
	read := r.reads[path]
	read.binding = receiverBinding{kind: receiverBindValue, value: value}
	r.reads[path] = read
}
func (r *receiverReads) fresh(path string) {
	read := r.reads[path]
	read.binding = receiverBinding{kind: receiverBindFresh}
	r.reads[path] = read
}
func (r *receiverReads) contentList(path string) {
	read := r.reads[path]
	read.binding = receiverBinding{kind: receiverBindContentList}
	r.reads[path] = read
}

// bindLabel keeps one label of a content-bound object in its binding. Setting
// it again is idempotent: a group reads one object once per member.
func (r *receiverReads) bindLabel(path, key string) {
	read, ok := r.reads[path]
	if !ok || read.binding.kind != receiverBindContent {
		return
	}
	labels := map[string]any{}
	for name, value := range read.binding.labels {
		labels[name] = value
	}
	labels[key] = resourceMap(resourceMap(read.body, "metadata"), "labels")[key]
	read.binding.labels = labels
	r.reads[path] = read
}

// useBindingKey keys every content digest with the receiver's own catalog
// credential. The digests are saved with the plan, and a ConfigMap or Secret
// may hold low-entropy credential material (a hand-set registry password), so
// an unkeyed digest could be reversed by guessing. The package's existing
// Secret fingerprint, sensitiveDigest (sensitive.go, and b74ef4f3a's owner
// fingerprints), is an unkeyed SHA-256 chain, so it is not reused here. The
// catalog credential is the installation's minted bearer token: required, read
// at every call, high-entropy and never persisted. Rotating it re-keys every
// digest, and refuses exactly as a rotation always has. The Secret it is read
// from is bound like any other, under the derived key.
func (r *receiverReads) useBindingKey(credential string) {
	mac := hmac.New(sha256.New, []byte(credential))
	mac.Write([]byte("memql-installation-receiver-binding-key-v1"))
	r.key = mac.Sum(nil)
}

// receiverContent is what a ConfigMap or Secret means to its reader: type,
// immutability and data, exactly as the API server encodes them. Nothing is
// decoded, so no credential is ever materialized for hashing.
func receiverContent(object map[string]any) map[string]any {
	out := map[string]any{}
	for _, field := range []string{"type", "immutable", "data", "binaryData"} {
		if value, present := object[field]; present {
			out[field] = value
		}
	}
	return out
}

func (r *receiverReads) contentDigest(object map[string]any) (string, error) {
	if len(r.key) != sha256.Size {
		return "", errors.New("installation configuration content has no binding key")
	}
	body, err := json.Marshal(receiverContent(object))
	if err != nil {
		return "", errors.New("installation configuration content is malformed")
	}
	mac := hmac.New(sha256.New, r.key)
	mac.Write([]byte("memql-installation-receiver-content-v1\n"))
	mac.Write(body)
	return "hmac-sha256:" + hex.EncodeToString(mac.Sum(nil)), nil
}

// legacyBinding is v1's binding for a read, byte for byte.
func (read receiverRead) legacyBinding() any {
	if read.legacy != nil {
		return read.legacy
	}
	if _, collection := read.body["items"]; collection {
		return receiverCollectionIdentity(read.body)
	}
	return receiverDiscoveryIdentity(read.body)
}

// currentBinding is v2's binding for a read; bound is false when the read is
// verified fresh at every call instead.
func (r *receiverReads) currentBinding(read receiverRead) (value any, bound bool, err error) {
	switch read.binding.kind {
	case receiverBindFresh:
		return nil, false, nil
	case receiverBindValue:
		return read.binding.value, true, nil
	case receiverBindContent:
		identity, _ := read.binding.value.([]any)
		digest, err := r.contentDigest(read.body)
		if err != nil {
			return nil, false, err
		}
		value := append(append([]any(nil), identity...), digest)
		if len(read.binding.labels) != 0 {
			value = append(value, read.binding.labels)
		}
		return value, true, nil
	case receiverBindContentList:
		items, _ := read.body["items"].([]any)
		out := make([][]string, 0, len(items))
		for _, raw := range items {
			item, _ := raw.(map[string]any)
			meta := resourceMap(item, "metadata")
			digest, err := r.contentDigest(item)
			if err != nil {
				return nil, false, err
			}
			out = append(out, []string{resourceText(meta, "name"), resourceText(meta, "uid"), digest})
		}
		sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
		return []any{"content-list", out}, true, nil
	}
	return read.legacyBinding(), true, nil
}

// The contract stands in the place of a path that no Kubernetes read can have.
const receiverDiscoveryBinding = "#discovery-contract"

func (r *receiverReads) secret(ctx context.Context, namespace string, ref receiverSecret) (string, error) {
	if !receiverKey.MatchString(ref.Key) {
		return "", errors.New("installation Secret key is invalid")
	}
	body, err := r.object(ctx, "v1", "Secret", "secrets", namespace, ref.Name)
	if err != nil {
		return "", err
	}
	value, err := base64.StdEncoding.Strict().DecodeString(resourceText(resourceMap(body, "data"), ref.Key))
	if err != nil || len(value) == 0 || len(value) > 64<<10 {
		return "", errors.New("installation credential or trust material is unavailable")
	}
	return string(value), nil
}

// reobserve rereads every read and refuses one that changed while this
// observation ran, however it is bound: staying exact within one observation is
// what makes the read set one snapshot.
func (r *receiverReads) reobserve(ctx context.Context) error {
	for _, path := range r.paths() {
		read := r.reads[path]
		raw, err := r.api.Do(ctx, http.MethodGet, path, "", nil)
		if err != nil {
			return errors.New("installation configuration could not be reobserved")
		}
		var current map[string]any
		if receiverJSON(raw, &current) != nil {
			return errors.New("installation configuration reobservation is malformed")
		}
		if _, collection := read.body["items"]; collection {
			if !sameJSON(receiverCollectionIdentity(read.body), receiverCollectionIdentity(current)) {
				return errors.New("installation discovery changed during observation")
			}
		} else if read.uid != "" || read.version != "" {
			meta := resourceMap(current, "metadata")
			if resourceText(meta, "uid") != read.uid || resourceText(meta, "resourceVersion") != read.version || meta["deletionTimestamp"] != nil {
				return errors.New("installation configuration changed during observation")
			}
			// A collection's RV is not a content identity: the API server may return
			// another subset under that version. Also compare its full contents.
			if read.uid == "" && !sameJSON(read.body, current) {
				return errors.New("installation discovery changed during observation")
			}
		} else if !sameJSON(receiverDiscoveryIdentity(read.body), receiverDiscoveryIdentity(current)) {
			return errors.New("installation discovery changed during observation")
		}
	}
	return nil
}

func (r *receiverReads) paths() []string {
	paths := make([]string, 0, len(r.reads))
	for path := range r.reads {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

// finish reobserves and returns the v1 digest of every read. The workload and
// status readers (workloads.go, selection.go) use it unchanged: their digest
// binds every object they observed, Pods included, within one call, and they
// hold no binding key.
func (r *receiverReads) finish(ctx context.Context) (string, error) {
	if err := r.reobserve(ctx); err != nil {
		return "", err
	}
	return r.legacyDigest("receiving-configuration-v1", nil), nil
}

// configuration reobserves the receiver's configuration reads and returns
// their digest in both formats.
func (r *receiverReads) configuration(ctx context.Context) (receiverDigests, error) {
	if err := r.reobserve(ctx); err != nil {
		return receiverDigests{}, err
	}
	current, err := r.currentDigest("receiving-configuration-v2", nil)
	if err != nil {
		return receiverDigests{}, err
	}
	return receiverDigests{current: current, legacy: r.legacyDigest("receiving-configuration-v1", nil)}, nil
}

// legacyDigest is v1's hash of every read, byte for byte; replaced substitutes
// a binding at the given paths.
func (r *receiverReads) legacyDigest(domain string, replaced map[string][]string) string {
	paths := r.paths()
	bindings := make([]any, 0, len(paths))
	for _, path := range paths {
		if binding, ok := replaced[path]; ok {
			bindings = append(bindings, []any{path, binding})
			continue
		}
		bindings = append(bindings, []any{path, r.reads[path].legacyBinding()})
	}
	return artifactHash(domain, bindings)
}

// currentDigest is v2's hash: every read's current binding, without the reads
// verified fresh at each call, and the discovery contract last.
func (r *receiverReads) currentDigest(domain string, replaced map[string][]string) (string, error) {
	paths := r.paths()
	bindings := make([]any, 0, len(paths)+1)
	for _, path := range paths {
		if binding, ok := replaced[path]; ok {
			bindings = append(bindings, []any{path, binding})
			continue
		}
		value, bound, err := r.currentBinding(r.reads[path])
		if err != nil {
			return "", err
		}
		if bound {
			bindings = append(bindings, []any{path, value})
		}
	}
	if r.contract != nil {
		bindings = append(bindings, []any{receiverDiscoveryBinding, r.contract})
	}
	return artifactHash(domain, bindings), nil
}

// Called only after configuration has reobserved every original resource.
// Application spec/generation/operation changes belong to the separately
// verified native intent. Its actual identity remains bound here; no synthetic
// baseline object is substituted into either read or verification.
func (r *receiverReads) configurationInvariant(applications ...receiverNamed) (receiverDigests, error) {
	appBindings := map[string][]string{}
	for _, application := range applications {
		path, err := receiverPath("argoproj.io/v1alpha1", "applications", application.Namespace, application.Name)
		if err != nil {
			return receiverDigests{}, err
		}
		app, ok := r.reads[path]
		if !ok || app.uid == "" || app.version == "" || app.legacy == nil || appBindings[path] != nil {
			return receiverDigests{}, errors.New("installation invariant has no unique authenticated Application")
		}
		appBindings[path] = []string{"argoproj.io/v1alpha1", "Application", application.Namespace, application.Name, app.uid}
	}

	current, err := r.currentDigest("receiving-configuration-invariant-v2", appBindings)
	if err != nil {
		return receiverDigests{}, err
	}
	return receiverDigests{current: current, legacy: r.legacyDigest("receiving-configuration-invariant-v1", appBindings)}, nil
}

// CRD discovery can return the same APIResourceList in a different order on
// each authenticated read. Compare every complete entry, including duplicates
// and unknown fields, in canonical order: within one observation (reobserve), and
// in v1's saved binding. Only this inventory order is immaterial there. v2 saves
// the receiver's discovery contract instead (receiverDiscoveryContract), whose
// entries are ordered the same way. Never mutate the captured body.
func receiverDiscoveryIdentity(body map[string]any) any {
	items, ok := body["resources"].([]any)
	if resourceText(body, "kind") != "APIResourceList" || !ok {
		return body
	}
	entries := make([]string, 0, len(items))
	for _, item := range items {
		raw, err := json.Marshal(item)
		if err != nil {
			return body
		}
		entries = append(entries, string(raw))
	}
	sort.Strings(entries)
	identity := make(map[string]any, len(body))
	for key, value := range body {
		identity[key] = value
	}
	identity["resources"] = entries
	return identity
}
