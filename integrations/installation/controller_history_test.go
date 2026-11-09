package installation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/integrations/argocd"
)

// historyController adds what Kubernetes serves for one Application beside the
// object itself to the JSON Patch controller double: the name-filtered list
// whose resourceVersion is a watch boundary, and the watch of every change
// since a boundary. Reversals are planned and proved through those reads
// (argocd.ReadPlanning, argocd.BoundaryProof). Every write the double accepts
// and every change a test makes through it is one MODIFIED event.
type historyController struct {
	*revisionControllerFixture
	// events, watchError and beforeWrite are guarded by the embedded double's
	// mutex.
	events     []map[string]any
	watchError bool
	changes    int
	// beforeWrite, when set, runs once just before the next PATCH reaches the
	// Application, as another writer's change lands between a request's last
	// read and its patch.
	beforeWrite func()
}

func newHistoryController(t *testing.T, db *sql.DB, r revisionRecord) *historyController {
	t.Helper()
	return &historyController{revisionControllerFixture: newRevisionControllerFixture(t, db, r)}
}

func (h *historyController) collection() (string, string) {
	i := strings.LastIndex(h.path, "/")
	return h.path[:i], h.path[i+1:]
}

func (h *historyController) Do(ctx context.Context, method, path, contentType string, body []byte) ([]byte, error) {
	collection, name := h.collection()
	if u, err := url.Parse(path); err == nil && u.Path == collection {
		q := u.Query()
		if method != http.MethodGet || q.Get("fieldSelector") != "metadata.name="+name {
			return nil, errors.New("unexpected Application collection request")
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		if q.Get("watch") == "true" {
			if h.watchError {
				return nil, errors.New("history compacted")
			}
			from, err := strconv.Atoi(strings.TrimPrefix(q.Get("resourceVersion"), "events-"))
			if err != nil || from < 0 || from > len(h.events) {
				return nil, errors.New("unknown watch boundary")
			}
			var out []byte
			for _, event := range h.events[from:] {
				raw, _ := json.Marshal(event)
				out = append(out, raw...)
				out = append(out, '\n')
			}
			return out, nil
		}
		return json.Marshal(map[string]any{"apiVersion": "argoproj.io/v1alpha1", "kind": "ApplicationList", "metadata": map[string]any{"resourceVersion": fmt.Sprintf("events-%d", len(h.events))}, "items": []any{h.object}})
	}
	h.mu.Lock()
	before := h.writes
	hook := h.beforeWrite
	if method == http.MethodPatch {
		h.beforeWrite = nil
	} else {
		hook = nil
	}
	h.mu.Unlock()
	if hook != nil {
		hook()
	}
	out, err := h.revisionControllerFixture.Do(ctx, method, path, contentType, body)
	h.mu.Lock()
	if h.writes != before {
		h.record()
	}
	h.mu.Unlock()
	return out, err
}

// record appends the current object as a MODIFIED event; the caller holds mu.
func (h *historyController) record() {
	raw, _ := json.Marshal(h.object)
	var snapshot map[string]any
	_ = json.Unmarshal(raw, &snapshot)
	h.events = append(h.events, map[string]any{"type": "MODIFIED", "object": snapshot})
}

// change applies one write by someone other than the plan: a new
// resourceVersion and, when bump is set, a new generation, as Argo's own status
// writes advance it on an Application CRD without a status subresource.
func (h *historyController) change(bump bool, edit func(map[string]any)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.changes++
	meta := resourceMap(h.object, "metadata")
	if bump {
		generation, _ := strconv.ParseInt(fmt.Sprint(meta["generation"]), 10, 64)
		meta["generation"] = generation + 1
	}
	meta["resourceVersion"] = fmt.Sprintf("external-%d", h.changes)
	if edit != nil {
		edit(h.object)
	}
	h.record()
}

// statusWrite is Argo refreshing the Application: only status changes.
func (h *historyController) statusWrite() {
	h.change(true, func(o map[string]any) {
		status, _ := o["status"].(map[string]any)
		if status == nil {
			status = map[string]any{}
			o["status"] = status
		}
		status["reconciledAt"] = fmt.Sprintf("2026-11-09T12:00:%02dZ", h.changes)
	})
}

// specEdit is another writer changing the Application's source.
func (h *historyController) specEdit() {
	h.change(true, func(o map[string]any) {
		resourceMap(resourceMap(o, "spec"), "source")["path"] = "deploy/edited-by-hand"
	})
}

// laterSync is an operator syncing the Application by hand once more: Argo
// replaces the plan's own operation in operationState with this one.
func (h *historyController) laterSync(revision string) {
	h.change(false, func(o map[string]any) {
		status := resourceMap(o, "status")
		source := resourceMap(resourceMap(o, "spec"), "source")
		status["operationState"] = map[string]any{"phase": "Succeeded",
			"operation":  map[string]any{"initiatedBy": map[string]any{"username": "admin"}, "sync": map[string]any{"revision": revision}},
			"syncResult": map[string]any{"revision": revision, "source": source}}
	})
}

// deployHistory is Argo appending one completed sync to status.history, as it
// does for every sync it finishes: the newest entry has the highest id. Its
// deployedAt is ago before the database clock, the clock a reverse start is
// stamped by, so a test never depends on the host and the database agreeing.
func (h *historyController) deployHistory(revision string, ago time.Duration) {
	var now time.Time
	if err := h.db.QueryRow(`SELECT clock_timestamp()`).Scan(&now); err != nil {
		panic(err)
	}
	h.change(false, func(o map[string]any) {
		status := resourceMap(o, "status")
		raw, _ := json.Marshal(resourceMap(resourceMap(o, "spec"), "source"))
		var source map[string]any
		_ = json.Unmarshal(raw, &source)
		history, _ := status["history"].([]any)
		status["history"] = append(history, map[string]any{"id": len(history) + 1, "revision": revision, "source": source, "deployedAt": now.Add(-ago).UTC().Format(time.RFC3339)})
	})
}

// applyReversalIntent sends a recorded reversal the way the rollback workflow
// does: an applied one is only observed again, and a history-backed one is
// patched with a proof from its own planning boundary.
func applyReversalIntent(ctx context.Context, client *argocd.Client, intent argocd.Intent) (argocd.Facts, error) {
	if facts, err := client.Observe(ctx, intent); err == nil {
		return facts, nil
	} else if !errors.Is(err, argocd.ErrChanged) {
		return argocd.Facts{}, err
	}
	var proof *argocd.ContinuityProof
	if intent.FormatVersion == 2 {
		var err error
		if proof, err = client.BoundaryProof(ctx, intent); err != nil {
			return argocd.Facts{}, err
		}
	}
	return client.ApplyWithContinuity(ctx, intent, proof)
}
