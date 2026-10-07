// Package argocd supplies bounded Application protocol effects. It does not
// select releases, authorize installation requests, or decide rollout policy.
// The caller must persist its verified, authorized intent before Apply.
package argocd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/core/id"
)

const (
	maxApplicationBytes = 4 << 20
	intentAnnotation    = "memql.io/update-intent"
	operationInfoName   = "memql.io/update-intent"
)

var (
	dnsLabel  = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$`)
	commitSHA = regexp.MustCompile(`^[a-f0-9]{40}$`)
	requestID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,127}$`)
	// ErrChanged requires a new observation/review, never an unconditional retry.
	ErrChanged = errors.New("ArgoCD Application no longer matches the installation intent")
	ErrBusy    = errors.New("ArgoCD Application has another operation in progress")
)

// API is implemented by deploycontrol.ClusterAPI, sharing projected token
// rotation, cluster CA and bounded HTTP with the existing cluster operations.
type API interface {
	Do(context.Context, string, string, string, []byte) ([]byte, error)
}

type Target struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
}

// Intent is a persistable protocol request. It is not an approval token. A
// digest of this entire value belongs in the installation's native authority
// journal together with the candidate, rendered diff and rollback evidence.
// All fields are revalidated after recovery; an editable DSL object cannot
// grant permission to call this native adapter.
type Intent struct {
	FormatVersion    int             `json:"formatVersion"`
	RequestID        string          `json:"requestId"`
	Target           Target          `json:"target"`
	BeforeGeneration int64           `json:"beforeGeneration"`
	BeforeMarker     string          `json:"beforeMarker"`
	BeforeSpec       json.RawMessage `json:"beforeSpec"`
	Revision         string          `json:"revision"`
	Prune            bool            `json:"prune"`
}

// Snapshot owns a decoded response; callers cannot mutate it after review.
type Snapshot struct{ object map[string]any }

// Facts report an observation, never a completed installation verdict. Even an
// Argo sync that succeeded still needs workload, traffic and cleanup evidence.
type Facts struct {
	IntentObserved     bool
	OperationPending   bool
	OperationPhase     string
	OperationSucceeded bool
	RevisionObserved   bool
	Healthy            bool
	Synced             bool
}

type Client struct{ api API }

func New(api API) (*Client, error) {
	if api == nil {
		return nil, errors.New("ArgoCD requires the cluster API")
	}
	return &Client{api: api}, nil
}

func (t Target) path() (string, error) {
	if len(t.Namespace) > 63 || !dnsLabel.MatchString(t.Namespace) || len(t.Name) > 63 || !dnsLabel.MatchString(t.Name) ||
		len(t.UID) == 0 || len(t.UID) > 128 || strings.TrimSpace(t.UID) != t.UID {
		return "", errors.New("ArgoCD requires an exact named Application and UID")
	}
	return "apis/argoproj.io/v1alpha1/namespaces/" + t.Namespace + "/applications/" + t.Name, nil
}

func (c *Client) Read(ctx context.Context, target Target) (Snapshot, error) {
	path, err := target.path()
	if err != nil {
		return Snapshot{}, err
	}
	if c == nil || c.api == nil {
		return Snapshot{}, errors.New("ArgoCD cluster API is unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	body, err := c.api.Do(ctx, http.MethodGet, path, "", nil)
	if err != nil {
		return Snapshot{}, fmt.Errorf("read ArgoCD Application: %w", err)
	}
	return decodeSnapshot(body, target)
}

// PlanRevision changes only targetRevision in the existing single-source Git
// Application. Image digests must already be committed in the reviewed overlay;
// this adapter cannot add image overrides, change repositories, or target a
// different namespace/project. BeforeGeneration fences changes away and back.
func PlanRevision(snapshot Snapshot, id, revision string, prune bool) (Intent, error) {
	obj := snapshot.object
	meta := object(obj, "metadata")
	if err := idle(obj); err != nil {
		return Intent{}, err
	}
	generation, err := integer(meta["generation"])
	if err != nil || generation < 1 {
		return Intent{}, errors.New("ArgoCD Application generation is missing")
	}
	spec, err := canonical(object(obj, "spec"))
	if err != nil {
		return Intent{}, err
	}
	intent := Intent{
		FormatVersion: 1, RequestID: id,
		Target:           Target{Namespace: stringAt(meta, "namespace"), Name: stringAt(meta, "name"), UID: stringAt(meta, "uid")},
		BeforeGeneration: generation, BeforeMarker: marker(obj), BeforeSpec: spec, Revision: revision, Prune: prune,
	}
	_, _, _, err = intent.validate()
	return intent, err
}

func (i Intent) Digest() (string, error) {
	_, _, digest, err := i.validate()
	return digest, err
}

// DecodeIntent restores one journal value without dropping unknown fields or
// duplicate keys. The journal must separately verify its stored intent digest.
func DecodeIntent(body []byte) (Intent, error) {
	obj, err := decodeObject(body)
	if err != nil {
		return Intent{}, err
	}
	allowed := map[string]bool{"formatVersion": true, "requestId": true, "target": true, "beforeGeneration": true, "beforeMarker": true, "beforeSpec": true, "revision": true, "prune": true}
	for key := range obj {
		if !allowed[key] {
			return Intent{}, errors.New("ArgoCD intent contains a noncanonical field")
		}
	}
	for key := range object(obj, "target") {
		if key != "namespace" && key != "name" && key != "uid" {
			return Intent{}, errors.New("ArgoCD intent target contains a noncanonical field")
		}
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	var intent Intent
	if err := d.Decode(&intent); err != nil {
		return Intent{}, errors.New("ArgoCD intent has invalid or unknown fields")
	}
	_, _, _, err = intent.validate()
	return intent, err
}

func (i Intent) validate() (before, after map[string]any, digest string, err error) {
	if _, err = i.Target.path(); err != nil {
		return
	}
	if i.FormatVersion != 1 || !requestID.MatchString(i.RequestID) || !commitSHA.MatchString(i.Revision) ||
		i.BeforeGeneration < 1 || len(i.BeforeMarker) > 128 {
		err = errors.New("ArgoCD intent requires a request identity, generation and immutable Git commit")
		return
	}
	before, err = decodeObject(i.BeforeSpec)
	if err != nil {
		return
	}
	source := object(before, "source")
	if len(source) == 0 || before["sources"] != nil || stringAt(source, "repoURL") == "" || stringAt(source, "path") == "" ||
		stringAt(source, "targetRevision") == "" || source["chart"] != nil || source["helm"] != nil || source["plugin"] != nil ||
		stringAt(before, "project") == "" || len(object(before, "destination")) == 0 {
		err = errors.New("ArgoCD revision effect requires a single Git source, project and destination")
		return
	}
	if images := object(source, "kustomize")["images"]; images != nil {
		values, ok := images.([]any)
		if !ok || len(values) != 0 {
			err = errors.New("ArgoCD image overrides must be moved into the reviewed Git overlay")
			return
		}
	}
	// Normalize JSON so whitespace or map order cannot change operation identity.
	i.BeforeSpec, err = canonical(before)
	if err != nil {
		return
	}
	raw, err := json.Marshal(i)
	if err != nil {
		return nil, nil, "", err
	}
	// This is an internal content identity, not an OCI/SHA wire digest. Avoid
	// retaining the intermediate per-byte ID history of a potentially large spec.
	digest = "memql-id:" + string(id.NewUntracked().FromBytes(raw))
	after, err = decodeObject(i.BeforeSpec)
	if err == nil {
		object(after, "source")["targetRevision"] = i.Revision
	}
	return
}

// Apply first reconciles the exact intent against fresh state. A lost reply
// returns an error; callers persist uncertainty and call Observe or Apply with
// the SAME intent. A matching marker never causes another sync, even if that
// sync failed. There is no detached watcher, blind patch retry, or polling loop.
func (c *Client) Apply(ctx context.Context, intent Intent) (Facts, error) {
	before, after, digest, err := intent.validate()
	if err != nil {
		return Facts{}, err
	}
	snapshot, err := c.Read(ctx, intent.Target)
	if err != nil {
		return Facts{}, err
	}
	obj := snapshot.object
	if marker(obj) == digest {
		return observe(snapshot, intent, after, digest)
	}
	meta := object(obj, "metadata")
	generation, err := integer(meta["generation"])
	if err != nil || generation != intent.BeforeGeneration || marker(obj) != intent.BeforeMarker || !equalJSON(object(obj, "spec"), before) {
		return Facts{}, ErrChanged
	}
	if err := idle(obj); err != nil {
		return Facts{}, err
	}
	annotations := object(meta, "annotations")
	if annotations == nil {
		annotations = map[string]any{}
	}
	annotations[intentAnnotation] = digest
	operation := map[string]any{
		"initiatedBy": map[string]any{"username": "memql-cluster-update", "automated": false},
		"info":        []any{map[string]any{"name": operationInfoName, "value": digest}},
		"sync":        syncRequest(intent, after),
	}
	patch := []map[string]any{
		{"op": "test", "path": "/metadata/uid", "value": intent.Target.UID},
		{"op": "test", "path": "/metadata/resourceVersion", "value": stringAt(meta, "resourceVersion")},
		{"op": "test", "path": "/metadata/generation", "value": intent.BeforeGeneration},
		{"op": "test", "path": "/spec", "value": before},
		{"op": "add", "path": "/metadata/annotations", "value": annotations},
		{"op": "replace", "path": "/spec", "value": after},
		{"op": "add", "path": "/operation", "value": operation},
	}
	// Clear old operationState atomically. Argo's merge-patched omitempty fields
	// must not inherit a previous selective resource list, sync options or prune.
	if status := object(obj, "status"); status != nil {
		patch = append(patch, map[string]any{"op": "add", "path": "/status/operationState", "value": nil})
	}
	body, err := json.Marshal(patch)
	if err != nil {
		return Facts{}, err
	}
	path, _ := intent.Target.path()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	body, err = c.api.Do(ctx, http.MethodPatch, path, "application/json-patch+json", body)
	if err != nil {
		// The request may have committed before any response or transport failure.
		return Facts{}, fmt.Errorf("ArgoCD revision effect is unconfirmed; reconcile the same intent: %w", err)
	}
	applied, err := decodeSnapshot(body, intent.Target)
	if err != nil {
		return Facts{}, fmt.Errorf("ArgoCD revision reply is unconfirmed; reconcile the same intent: %w", err)
	}
	return observe(applied, intent, after, digest)
}

func (c *Client) Observe(ctx context.Context, intent Intent) (Facts, error) {
	_, after, digest, err := intent.validate()
	if err != nil {
		return Facts{}, err
	}
	snapshot, err := c.Read(ctx, intent.Target)
	if err != nil {
		return Facts{}, err
	}
	return observe(snapshot, intent, after, digest)
}

func observe(snapshot Snapshot, intent Intent, after map[string]any, digest string) (Facts, error) {
	obj := snapshot.object
	if err := unmanaged(obj); err != nil {
		return Facts{}, err
	}
	if marker(obj) != digest || !equalJSON(object(obj, "spec"), after) {
		return Facts{}, ErrChanged
	}
	status := object(obj, "status")
	op := object(obj, "operation")
	state := object(status, "operationState")
	if op != nil && !ownsOperation(op, digest, intent, after) {
		return Facts{}, ErrBusy
	}
	ownedState := ownsOperation(object(state, "operation"), digest, intent, after)
	sync := object(status, "sync")
	revisionObserved := stringAt(sync, "revision") == intent.Revision && equalJSON(object(object(sync, "comparedTo"), "source"), object(after, "source"))
	facts := Facts{
		IntentObserved: true, OperationPending: op != nil,
		RevisionObserved: revisionObserved, Healthy: stringAt(object(status, "health"), "status") == "Healthy",
		Synced: revisionObserved && stringAt(sync, "status") == "Synced",
	}
	if ownedState {
		facts.OperationPhase = stringAt(state, "phase")
		result := object(state, "syncResult")
		facts.OperationSucceeded = op == nil && facts.OperationPhase == "Succeeded" && stringAt(result, "revision") == intent.Revision && equalJSON(object(result, "source"), object(after, "source"))
	}
	return facts, nil
}

func syncRequest(intent Intent, spec map[string]any) map[string]any {
	sync := map[string]any{"revision": intent.Revision, "prune": intent.Prune}
	if options, ok := object(spec, "syncPolicy")["syncOptions"].([]any); ok && len(options) != 0 {
		sync["syncOptions"] = options
	}
	return sync
}

func ownsOperation(op map[string]any, digest string, intent Intent, spec map[string]any) bool {
	sync := object(op, "sync")
	for key := range sync {
		if key != "revision" && key != "prune" && key != "syncOptions" && key != "resources" {
			// Inline manifests or an operation-specific source can bypass the
			// reviewed Git tree even while retaining our revision and marker.
			return false
		}
	}
	if stringAt(sync, "revision") != intent.Revision {
		return false
	}
	prune, _ := sync["prune"].(bool)
	if value := sync["prune"]; value != nil {
		if _, ok := value.(bool); !ok {
			return false
		}
	}
	if prune != intent.Prune || !emptyArray(sync["resources"]) {
		return false
	}
	actualOptions := sync["syncOptions"]
	wantOptions := syncRequest(intent, spec)["syncOptions"]
	if !equalJSON(actualOptions, wantOptions) && !(emptyArray(actualOptions) && emptyArray(wantOptions)) {
		return false
	}
	info, _ := op["info"].([]any)
	matches := 0
	for _, raw := range info {
		entry, _ := raw.(map[string]any)
		if stringAt(entry, "name") == operationInfoName {
			if stringAt(entry, "value") != digest {
				return false
			}
			matches++
		}
	}
	return matches == 1
}

func idle(obj map[string]any) error {
	if err := unmanaged(obj); err != nil {
		return err
	}
	phase := stringAt(object(object(obj, "status"), "operationState"), "phase")
	if obj["operation"] != nil || phase == "Running" || phase == "Terminating" {
		return ErrBusy
	}
	return nil
}

func unmanaged(obj map[string]any) error {
	meta := object(obj, "metadata")
	if meta["deletionTimestamp"] != nil || !emptyArray(meta["ownerReferences"]) {
		// ApplicationSet/controller-owned Applications need their owning source
		// changed; patching this child would just be reverted by that controller.
		return errors.New("ArgoCD Application is deleting or controlled by another resource")
	}
	return nil
}

func emptyArray(value any) bool {
	if value == nil {
		return true
	}
	items, ok := value.([]any)
	return ok && len(items) == 0
}

func decodeSnapshot(body []byte, target Target) (Snapshot, error) {
	obj, err := decodeObject(body)
	if err != nil {
		return Snapshot{}, err
	}
	meta := object(obj, "metadata")
	if stringAt(obj, "apiVersion") != "argoproj.io/v1alpha1" || stringAt(obj, "kind") != "Application" ||
		stringAt(meta, "namespace") != target.Namespace || stringAt(meta, "name") != target.Name || stringAt(meta, "uid") != target.UID ||
		stringAt(meta, "resourceVersion") == "" || len(object(obj, "spec")) == 0 {
		return Snapshot{}, ErrChanged
	}
	return Snapshot{object: obj}, nil
}

// Reject ambiguous JSON before decoding maps. Preserve numbers exactly; a
// generation is never narrowed through float64, including after SQL recovery.
func decodeObject(body []byte) (map[string]any, error) {
	if len(body) == 0 || len(body) > maxApplicationBytes {
		return nil, errors.New("ArgoCD object is empty or exceeds four MiB")
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	if err := uniqueJSON(d, 0); err != nil {
		return nil, errors.New("ArgoCD object contains ambiguous or invalid JSON")
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("ArgoCD object contains trailing JSON")
	}
	d = json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	var obj map[string]any
	if err := d.Decode(&obj); err != nil || obj == nil {
		return nil, errors.New("ArgoCD requires a JSON object")
	}
	return obj, nil
}

func uniqueJSON(d *json.Decoder, depth int) error {
	if depth > 64 {
		return errors.New("excessive JSON depth")
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return errors.New("duplicate JSON field")
			}
			seen[name] = true
			if err := uniqueJSON(d, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := uniqueJSON(d, depth+1); err != nil {
				return err
			}
		}
	default:
		return errors.New("invalid JSON delimiter")
	}
	_, err = d.Token()
	return err
}

func canonical(value any) ([]byte, error) { return json.Marshal(value) }
func equalJSON(a, b any) bool {
	left, err := canonical(a)
	if err != nil {
		return false
	}
	right, err := canonical(b)
	return err == nil && bytes.Equal(left, right)
}
func object(m map[string]any, key string) map[string]any {
	value, _ := m[key].(map[string]any)
	return value
}
func stringAt(m map[string]any, key string) string {
	value, _ := m[key].(string)
	return value
}
func integer(value any) (int64, error) {
	n, ok := value.(json.Number)
	if !ok {
		return 0, errors.New("not an integer")
	}
	return strconv.ParseInt(string(n), 10, 64)
}
func marker(obj map[string]any) string {
	return stringAt(object(object(obj, "metadata"), "annotations"), intentAnnotation)
}
