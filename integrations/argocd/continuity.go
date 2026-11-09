package argocd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var ErrHistoryUnavailable = errors.New("ArgoCD Application history is incomplete; read or prepare again")

// ContinuityState is a cursor, not an approval or a live proof. Its owner must
// persist it with the original admitted intent, reject foreign scope, and make
// Failed sticky across replicas. An expired watch never becomes a fresh seed.
type ContinuityState struct {
	Intent        string `json:"intent"`
	Cursor        string `json:"cursor"`
	ObjectVersion string `json:"objectVersion"`
	Generation    int64  `json:"generation"`
	Failed        bool   `json:"failed"`
}

// ContinuityProof is constructed only after a fresh authenticated list and,
// whenever the object changed, every intervening watch event has been checked.
// It is tied to one exact object RV, which the final JSON patch also tests.
type ContinuityProof struct {
	intent, version string
	generation      int64
	observed        time.Time
}

func validResourceVersion(value string) bool {
	return value != "" && value != "0" && len(value) <= 256 &&
		strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\x00\r\n")
}

func (t Target) collectionPath() (string, error) {
	path, err := t.path()
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(path, "/"+t.Name), nil
}

// ReadPlanning uses a name-filtered list for its current global watch boundary.
// An individual object's RV can predate compaction even when read just now.
// RBAC can restrict both list and watch to this exact metadata.name selector.
func (c *Client) ReadPlanning(ctx context.Context, target Target) (Snapshot, error) {
	path, err := target.collectionPath()
	if err != nil {
		return Snapshot{}, err
	}
	if c == nil || c.api == nil {
		return Snapshot{}, errors.New("ArgoCD cluster API is unavailable")
	}
	query := url.Values{"fieldSelector": {"metadata.name=" + target.Name}, "limit": {"2"}}
	raw, err := c.api.Do(ctx, http.MethodGet, path+"?"+query.Encode(), "", nil)
	if err != nil {
		return Snapshot{}, errors.New("ArgoCD named Application list is unavailable")
	}
	list, err := decodeObject(raw)
	if err != nil || stringAt(list, "apiVersion") != "argoproj.io/v1alpha1" || stringAt(list, "kind") != "ApplicationList" {
		return Snapshot{}, errors.New("ArgoCD named Application list is malformed")
	}
	meta := object(list, "metadata")
	items, ok := list["items"].([]any)
	if !ok || len(items) != 1 || stringAt(meta, "continue") != "" || meta["remainingItemCount"] != nil || !validResourceVersion(stringAt(meta, "resourceVersion")) {
		return Snapshot{}, errors.New("ArgoCD named Application list is incomplete")
	}
	body, _ := json.Marshal(items[0])
	snapshot, err := decodeSnapshot(body, target)
	if err != nil {
		return Snapshot{}, err
	}
	snapshot.watchVersion = stringAt(meta, "resourceVersion")
	return snapshot, nil
}

// PlanObservedRevision binds the same reviewed snapshot to a current list
// boundary. A concurrent edit, including an away-and-back generation change,
// refuses this initial admission; it never silently substitutes a new baseline.
func (c *Client) PlanObservedRevision(ctx context.Context, snapshot Snapshot, request, revision string, prune bool) (Intent, error) {
	meta := object(snapshot.object, "metadata")
	target := Target{Namespace: stringAt(meta, "namespace"), Name: stringAt(meta, "name"), UID: stringAt(meta, "uid")}
	fresh, err := c.ReadPlanning(ctx, target)
	if err != nil {
		return Intent{}, err
	}
	if !equalJSON(object(fresh.object, "spec"), object(snapshot.object, "spec")) ||
		!equalJSON(object(fresh.object, "metadata")["generation"], meta["generation"]) || marker(fresh.object) != marker(snapshot.object) {
		return Intent{}, ErrChanged
	}
	return PlanRevision(fresh, request, revision, prune)
}

func NewContinuityState(intent Intent) (ContinuityState, error) {
	_, _, digest, err := intent.validate()
	if err != nil || intent.FormatVersion != 2 {
		return ContinuityState{}, errors.New("Application history needs an admitted watch boundary")
	}
	return ContinuityState{Intent: digest, Cursor: intent.WatchResourceVersion, ObjectVersion: intent.BeforeResourceVersion, Generation: intent.BeforeGeneration}, nil
}

func (s ContinuityState) Validate(intent Intent) error {
	_, _, digest, err := intent.validate()
	if err != nil || intent.FormatVersion != 2 || s.Intent != digest || !validResourceVersion(s.Cursor) || !validResourceVersion(s.ObjectVersion) || s.Generation < intent.BeforeGeneration {
		return errors.New("Application history cursor belongs to another intent or is malformed")
	}
	return nil
}

func continuityObject(snapshot Snapshot, intent Intent, minimumGeneration int64) error {
	before, _, _, err := intent.validate()
	if err != nil {
		return err
	}
	gen, err := integer(object(snapshot.object, "metadata")["generation"])
	if err != nil || gen < minimumGeneration || marker(snapshot.object) != intent.BeforeMarker || !equalJSON(object(snapshot.object, "spec"), before) {
		return ErrChanged
	}
	return idle(snapshot.object)
}

// CheckContinuity permits status-driven generation increments only with
// uninterrupted history. Any spec/intent/operation transition is terminal,
// including a change away and back in the same response. Missing history or a
// transport failure returns no proof and never advances the persisted cursor.
func (c *Client) CheckContinuity(ctx context.Context, intent Intent, prior ContinuityState) (ContinuityState, *ContinuityProof, error) {
	if err := prior.Validate(intent); err != nil {
		return prior, nil, err
	}
	if prior.Failed {
		return prior, nil, ErrChanged
	}
	fail := func() (ContinuityState, *ContinuityProof, error) { prior.Failed = true; return prior, nil, ErrChanged }
	fresh, err := c.ReadPlanning(ctx, intent.Target)
	if err != nil {
		return prior, nil, err
	}
	if err = continuityObject(fresh, intent, prior.Generation); err != nil {
		return fail()
	}
	meta := object(fresh.object, "metadata")
	targetVersion := stringAt(meta, "resourceVersion")
	targetGeneration, _ := integer(meta["generation"])
	if targetVersion == prior.ObjectVersion {
		if targetGeneration != prior.Generation {
			return fail()
		}
		// Exact object RV equality proves no transition since the durable cursor.
		// Refresh the global boundary, even for an object unchanged for hours.
		prior.Cursor = fresh.watchVersion
		return prior, &ContinuityProof{prior.Intent, targetVersion, targetGeneration, time.Now()}, nil
	}
	path, _ := intent.Target.collectionPath()
	query := url.Values{"fieldSelector": {"metadata.name=" + intent.Target.Name}, "watch": {"true"}, "resourceVersion": {prior.Cursor}, "allowWatchBookmarks": {"true"}, "timeoutSeconds": {"1"}}
	raw, err := c.api.Do(ctx, http.MethodGet, path+"?"+query.Encode(), "", nil)
	if err != nil || len(raw) == 0 || len(raw) >= maxApplicationBytes {
		return prior, nil, ErrHistoryUnavailable
	}
	next := prior
	decoder := json.NewDecoder(bytes.NewReader(raw))
	seenTarget := false
	for n := 0; ; n++ {
		var eventRaw json.RawMessage
		err = decoder.Decode(&eventRaw)
		if err == io.EOF {
			break
		}
		if err != nil || n >= 256 {
			return prior, nil, ErrHistoryUnavailable
		}
		event, e := decodeObject(eventRaw)
		if e != nil || len(event) != 2 {
			return prior, nil, ErrHistoryUnavailable
		}
		body := object(event, "object")
		version := stringAt(object(body, "metadata"), "resourceVersion")
		switch stringAt(event, "type") {
		case "BOOKMARK":
			if !validResourceVersion(version) || stringAt(body, "apiVersion") != "argoproj.io/v1alpha1" || stringAt(body, "kind") != "Application" {
				return prior, nil, ErrHistoryUnavailable
			}
			next.Cursor = version
		case "MODIFIED":
			encoded, _ := json.Marshal(body)
			observed, e := decodeSnapshot(encoded, intent.Target)
			if e != nil || !validResourceVersion(version) || continuityObject(observed, intent, next.Generation) != nil {
				return fail()
			}
			next.Generation, _ = integer(object(observed.object, "metadata")["generation"])
			next.Cursor, next.ObjectVersion = version, version
			seenTarget = seenTarget || version == targetVersion
		case "ERROR":
			return prior, nil, ErrHistoryUnavailable
		case "ADDED", "DELETED":
			return fail()
		default:
			return prior, nil, ErrHistoryUnavailable
		}
	}
	if !seenTarget {
		return prior, nil, ErrHistoryUnavailable
	}
	if next.ObjectVersion == targetVersion {
		next.Cursor = fresh.watchVersion
	}
	return next, &ContinuityProof{next.Intent, next.ObjectVersion, next.Generation, time.Now()}, nil
}
