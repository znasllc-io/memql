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
		} else if !sameJSON(read.body, current) {
			return "", errors.New("installation discovery changed during observation")
		}
		stable := read.stable
		if stable == nil {
			stable = read.body
			if _, collection := read.body["items"]; collection {
				stable = receiverCollectionIdentity(read.body)
			}
		}
		bindings = append(bindings, []any{path, stable})
	}
	return artifactHash("receiving-configuration-v1", bindings), nil
}
