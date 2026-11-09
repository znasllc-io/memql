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

// BoundaryProof checks an intent's complete history from the boundary it was
// planned at (NewContinuityState) and returns the proof its patch needs. It is
// for a reversal (PlanRollback, or PlanUnappliedRollback over a ReadPlanning
// snapshot), whose journal keeps no cursor. Every check rereads the whole
// window from the planning boundary, so an edit seen once is seen by every
// later check while Kubernetes retains the history. Status-only generation
// advances pass, as CheckContinuity allows; a spec, marker, operation or
// identity change refuses.
//
// A reversal resumed after Kubernetes compacted that history (minutes on a
// busy cluster) is proved without it only while the Application's generation
// has not moved since planning (unchangedSincePlanning); otherwise it gets
// ErrHistoryUnavailable, never a proof rebased onto a fresh baseline, and its
// caller may plan the reversal again only when its journal proves nothing of
// it can still reach the cluster.
func (c *Client) BoundaryProof(ctx context.Context, intent Intent) (*ContinuityProof, error) {
	state, err := NewContinuityState(intent)
	if err != nil {
		return nil, err
	}
	_, proof, err := c.CheckContinuity(ctx, intent, state)
	if errors.Is(err, ErrHistoryUnavailable) {
		return c.unchangedSincePlanning(ctx, intent)
	}
	if err != nil {
		return nil, err
	}
	return proof, nil
}

// unchangedSincePlanning proves, with no history, that only metadata changed
// since intent was planned: the same UID, the exact generation, marker and
// complete spec it was planned against, and an idle Application. Every spec
// change advances the generation, which never decreases, so an edit made and
// undone since planning cannot hide here. It is the strict fence a format 1
// reversal's patch tested, so a history-backed reversal resumed after its
// history is gone accepts at least what that fence accepted, such as a label
// or annotation write by another controller. A moved generation keeps
// ErrHistoryUnavailable: without the history, Argo's status writes and a real
// edit undone since are indistinguishable.
func (c *Client) unchangedSincePlanning(ctx context.Context, intent Intent) (*ContinuityProof, error) {
	before, _, digest, err := intent.validate()
	if err != nil {
		return nil, err
	}
	snapshot, err := c.Read(ctx, intent.Target)
	if err != nil {
		return nil, err
	}
	meta := object(snapshot.object, "metadata")
	generation, err := integer(meta["generation"])
	if err != nil || marker(snapshot.object) != intent.BeforeMarker || !equalJSON(object(snapshot.object, "spec"), before) {
		return nil, ErrChanged
	}
	if generation != intent.BeforeGeneration {
		return nil, ErrHistoryUnavailable
	}
	if err := idle(snapshot.object); err != nil {
		return nil, err
	}
	return &ContinuityProof{digest, stringAt(meta, "resourceVersion"), generation, time.Now()}, nil
}

// Admit checks, without writing, that Apply could accept intent now, and
// returns the proof a history-backed intent's patch needs. A history-backed
// intent is proved from its planning boundary (BoundaryProof). A format 1
// intent must pass the same strict fence its patch tests: the exact UID,
// generation, marker and spec it was planned against, and an idle
// Application; any generation change, status-only or not, is ErrChanged. A
// caller runs it before recording that it is about to write, so that a refusal
// leaves nothing to account for.
func (c *Client) Admit(ctx context.Context, intent Intent) (*ContinuityProof, error) {
	if intent.FormatVersion == 2 {
		return c.BoundaryProof(ctx, intent)
	}
	before, _, digest, err := intent.validate()
	if err != nil {
		return nil, err
	}
	snapshot, err := c.Read(ctx, intent.Target)
	if err != nil {
		return nil, err
	}
	obj := snapshot.object
	if marker(obj) == digest {
		return nil, nil // Already applied; Apply reconciles it without writing.
	}
	generation, err := integer(object(obj, "metadata")["generation"])
	if err != nil || generation != intent.BeforeGeneration || marker(obj) != intent.BeforeMarker || !equalJSON(object(obj, "spec"), before) {
		return nil, ErrChanged
	}
	return nil, idle(obj)
}

// Superseded reports whether a format 1 intent can never be applied and never
// was: the Application still holds the intent's exact baseline (same UID,
// marker and complete spec) while its generation has moved past the one the
// intent's patch tests. Every request ever formed for the intent tested that
// generation and the resourceVersion of its moment, so none can land now or
// later; and none landed, or the Application would carry the intent's own
// marker. A history-backed intent's patch tests the version its proof named,
// which nothing retains, so this never reports one superseded.
func (c *Client) Superseded(ctx context.Context, intent Intent) (bool, error) {
	if intent.FormatVersion == 2 {
		return false, nil
	}
	before, _, digest, err := intent.validate()
	if err != nil {
		return false, err
	}
	snapshot, err := c.Read(ctx, intent.Target)
	if err != nil {
		return false, err
	}
	obj := snapshot.object
	if err := unmanaged(obj); err != nil {
		return false, err
	}
	generation, err := integer(object(obj, "metadata")["generation"])
	return err == nil && generation > intent.BeforeGeneration && marker(obj) != digest &&
		marker(obj) == intent.BeforeMarker && equalJSON(object(obj, "spec"), before), nil
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
