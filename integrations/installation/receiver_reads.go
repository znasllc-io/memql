package installation

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/integrations/argocd"
)

// A read set belongs to one observation. Kubernetes versions fence replacement
// and change-away-and-back, including Secret rotation without persisting secret
// values or a dictionary-testable hash of those values.
type receiverRead struct {
	path, uid, version string
	body               map[string]any
	stable             any
}
type receiverReads struct {
	api   argocd.API
	reads map[string]receiverRead
}

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
	body, err := r.get(ctx, path)
	if err != nil {
		return nil, err
	}
	if resourceText(body, "apiVersion") != version || resourceText(body, "kind") != kind || validLiveStorage(body, resourceIdentity{Kind: kind, Namespace: namespace, Name: name}) != nil {
		return nil, errors.New("installation configuration identity is absent, changed or deleting")
	}
	read := r.reads[path]
	// Configuration data and Secret data are bound by their authenticated
	// versions. Never hash their bodies: ConfigMaps may also contain credentials.
	read.stable = []string{version, kind, namespace, name, read.uid, read.version}
	if kind == "Application" || kind == "Deployment" || kind == "ReplicaSet" || kind == "Pod" {
		meta := resourceMap(body, "metadata")
		// Status writes do not change configuration. Recheck exact RV during this
		// observation, but do not invalidate recovery for a status-only heartbeat.
		read.stable = []any{version, kind, namespace, name, read.uid, meta["generation"], meta["labels"], meta["annotations"], meta["ownerReferences"], body["spec"]}
		if kind == "Application" {
			// An Argo CRD without a /status subresource increments generation on
			// status writes. Native preparation and writes instead require retained
			// complete Application history. Keep spec/identity/metadata here and
			// the exact RV fence within this individual authenticated observation.
			read.stable = []any{version, kind, namespace, name, read.uid, "application-history-v1", meta["labels"], meta["annotations"], meta["ownerReferences"], body["spec"]}
		}
	}
	r.reads[path] = read
	return body, nil
}
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
func (r *receiverReads) finish(ctx context.Context) (string, error) {
	paths := make([]string, 0, len(r.reads))
	for path := range r.reads {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	bindings := make([]any, 0, len(paths))
	for _, path := range paths {
		read := r.reads[path]
		raw, err := r.api.Do(ctx, http.MethodGet, path, "", nil)
		if err != nil {
			return "", errors.New("installation configuration could not be reobserved")
		}
		var current map[string]any
		if receiverJSON(raw, &current) != nil {
			return "", errors.New("installation configuration reobservation is malformed")
		}
		if _, collection := read.body["items"]; collection {
			if !sameJSON(receiverCollectionIdentity(read.body), receiverCollectionIdentity(current)) {
				return "", errors.New("installation discovery changed during observation")
			}
		} else if read.uid != "" || read.version != "" {
			meta := resourceMap(current, "metadata")
			if resourceText(meta, "uid") != read.uid || resourceText(meta, "resourceVersion") != read.version || meta["deletionTimestamp"] != nil {
				return "", errors.New("installation configuration changed during observation")
			}
			// A collection's RV is not a content identity: the API server may return
			// another subset under that version. Also compare its full contents.
			if read.uid == "" && !sameJSON(read.body, current) {
				return "", errors.New("installation discovery changed during observation")
			}
		} else if !sameJSON(receiverDiscoveryIdentity(read.body), receiverDiscoveryIdentity(current)) {
			return "", errors.New("installation discovery changed during observation")
		}
		stable := read.stable
		if stable == nil {
			stable = receiverDiscoveryIdentity(read.body)
			if _, collection := read.body["items"]; collection {
				stable = receiverCollectionIdentity(read.body)
			}
		}
		bindings = append(bindings, []any{path, stable})
	}
	return artifactHash("receiving-configuration-v1", bindings), nil
}

// Called only after finish has reobserved every original resource. Application
// spec/generation/operation changes belong to the separately verified native
// intent. Its actual identity remains bound here; no synthetic baseline object
// is substituted into either read or verification.
func (r *receiverReads) configurationInvariant(applications ...receiverNamed) (string, error) {
	appBindings := map[string][]string{}
	for _, application := range applications {
		path, err := receiverPath("argoproj.io/v1alpha1", "applications", application.Namespace, application.Name)
		if err != nil {
			return "", err
		}
		app, ok := r.reads[path]
		if !ok || app.uid == "" || app.version == "" || app.stable == nil || appBindings[path] != nil {
			return "", errors.New("installation invariant has no unique authenticated Application")
		}
		appBindings[path] = []string{"argoproj.io/v1alpha1", "Application", application.Namespace, application.Name, app.uid}
	}

	paths := make([]string, 0, len(r.reads))
	for key := range r.reads {
		paths = append(paths, key)
	}
	sort.Strings(paths)
	bindings := make([]any, 0, len(paths))
	for _, key := range paths {
		read := r.reads[key]
		stable := read.stable
		if binding, ok := appBindings[key]; ok {
			stable = binding
		} else if stable == nil {
			stable = receiverDiscoveryIdentity(read.body)
			if _, collection := read.body["items"]; collection {
				stable = receiverCollectionIdentity(read.body)
			}
		}
		bindings = append(bindings, []any{key, stable})
	}
	return artifactHash("receiving-configuration-invariant-v1", bindings), nil
}

// CRD discovery can return the same APIResourceList in a different order on
// each authenticated read. Bind every complete entry, including duplicates and
// unknown fields, in canonical order for both reobservation and recovery. Only
// this inventory order is immaterial: resource contracts, other arrays, object
// versions and Secret rotation remain exact. Never mutate the captured body.
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
