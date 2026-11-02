package argocd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// sync.go -- a full sync of an Application at one exact commit, leaving the
// Application tracking what it tracked. Apply (revision.go) moves targetRevision and
// is how a selected installation update lands; this names the revision on the
// operation alone, so a deploy can be tried from a commit that is not on main yet, and
// undone by syncing the commit before it with nothing to merge first (design record
// docs/superpowers/specs/2026-11-02-nightly-release-from-main-design.md, section 7).
//
// It writes what scripts/dev/argocd-sync.sh writes, for the reason that script exists:
// the controller persists a new operation into status.operationState with a merge
// patch marshaled with omitempty, so prune: false, an empty syncOptions and an empty
// resources list are omitted and the PREVIOUS operation's values survive. A plain sync
// after a selective one runs partial and reports Succeeded. So the patch clears the
// persisted state in the same write that starts the operation, prune is written false,
// and a sync that ran with a resource selection reads as not full (memql#4871).

const syncRequestInfo = "memql.io/deploy-request"

// SyncRequest names one sync. RequestID is what makes it safe to ask twice: the caller
// records it before asking, and a second ask finds it on the Application.
type SyncRequest struct {
	Target    Target
	RequestID string
	Revision  string
}

// SyncFacts report an Application and the sync named by the request that read them,
// never a verdict on an installation.
type SyncFacts struct {
	// Revision is the commit the Application's last successful sync ran at.
	Revision string
	// Mine says the operation below is the one this request started.
	Mine bool
	// Pending says an operation is written and not finished, whoever's it is.
	Pending   bool
	Phase     string
	Message   string
	Succeeded bool
	// Partial says it ran with a resource selection, so it is no deploy.
	Partial bool
	Healthy bool
	Health  string
	// Settled says the controller compared the Application after the operation ended,
	// so Healthy is about what it applied. Health read the moment an operation ends is
	// still the earlier rollout's.
	Settled bool
}

// ReadApplication reads an Application by name, for the identity a sync is fenced on
// and the commit it runs now.
func (c *Client) ReadApplication(ctx context.Context, namespace, name string) (Target, SyncFacts, error) {
	if c == nil || c.api == nil {
		return Target{}, SyncFacts{}, errors.New("ArgoCD cluster API is unavailable")
	}
	if len(namespace) > 63 || !dnsLabel.MatchString(namespace) || len(name) > 63 || !dnsLabel.MatchString(name) {
		return Target{}, SyncFacts{}, errors.New("ArgoCD requires an exact named Application")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	body, err := c.api.Do(ctx, http.MethodGet, "apis/argoproj.io/v1alpha1/namespaces/"+namespace+"/applications/"+name, "", nil)
	if err != nil {
		return Target{}, SyncFacts{}, fmt.Errorf("read ArgoCD Application: %w", err)
	}
	obj, err := decodeObject(body)
	if err != nil {
		return Target{}, SyncFacts{}, err
	}
	meta := object(obj, "metadata")
	target := Target{Namespace: stringAt(meta, "namespace"), Name: stringAt(meta, "name"), UID: stringAt(meta, "uid")}
	if target.Namespace != namespace || target.Name != name {
		return Target{}, SyncFacts{}, ErrChanged
	}
	snapshot, err := decodeSnapshot(body, target)
	if err != nil {
		return Target{}, SyncFacts{}, err
	}
	return target, syncFacts(snapshot.object, ""), nil
}

// StartSync starts the named sync, or reports it when it has already started. It never
// replaces another operation: ErrBusy tells the caller to wait or to stop that one.
func (c *Client) StartSync(ctx context.Context, r SyncRequest) (SyncFacts, error) {
	if !requestID.MatchString(r.RequestID) || !commitSHA.MatchString(r.Revision) {
		return SyncFacts{}, errors.New("an ArgoCD sync requires a request identity and an immutable Git commit")
	}
	path, err := r.Target.path()
	if err != nil {
		return SyncFacts{}, err
	}
	snapshot, err := c.Read(ctx, r.Target)
	if err != nil {
		return SyncFacts{}, err
	}
	obj := snapshot.object
	if err := unmanaged(obj); err != nil {
		return SyncFacts{}, err
	}
	if facts := syncFacts(obj, r.RequestID); facts.Mine {
		return facts, nil
	}
	if err := idle(obj); err != nil {
		return SyncFacts{}, err
	}
	meta := object(obj, "metadata")
	operation := map[string]any{
		"initiatedBy": map[string]any{"username": "memql-deploy", "automated": false},
		"info":        []any{map[string]any{"name": syncRequestInfo, "value": r.RequestID}},
		"sync": map[string]any{
			"revision":     r.Revision,
			"prune":        false,
			"syncOptions":  declaredSyncOptions(obj),
			"syncStrategy": map[string]any{"hook": map[string]any{}},
		},
	}
	patch := []map[string]any{
		{"op": "test", "path": "/metadata/uid", "value": r.Target.UID},
		{"op": "test", "path": "/metadata/resourceVersion", "value": stringAt(meta, "resourceVersion")},
		{"op": "add", "path": "/operation", "value": operation},
	}
	if status := object(obj, "status"); status != nil {
		patch = append(patch, map[string]any{"op": "add", "path": "/status/operationState", "value": nil})
	}
	body, err := json.Marshal(patch)
	if err != nil {
		return SyncFacts{}, err
	}
	writing, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	body, err = c.api.Do(writing, http.MethodPatch, path, "application/json-patch+json", body)
	if err != nil {
		// It may have committed before any reply. The next ask finds it by its request.
		return SyncFacts{}, fmt.Errorf("the ArgoCD sync is unconfirmed; ask again with the same request: %w", err)
	}
	applied, err := decodeSnapshot(body, r.Target)
	if err != nil {
		return SyncFacts{}, fmt.Errorf("the ArgoCD sync reply is unconfirmed; ask again with the same request: %w", err)
	}
	return syncFacts(applied.object, r.RequestID), nil
}

// ObserveSync reads the Application as the named sync sees it. It writes nothing.
func (c *Client) ObserveSync(ctx context.Context, r SyncRequest) (SyncFacts, error) {
	snapshot, err := c.Read(ctx, r.Target)
	if err != nil {
		return SyncFacts{}, err
	}
	return syncFacts(snapshot.object, r.RequestID), nil
}

// TerminateSync asks the controller to stop the running operation, as `argocd app
// terminate-op` does: a sync waiting on a Deployment that never becomes healthy runs
// forever otherwise. With nothing running it does nothing.
func (c *Client) TerminateSync(ctx context.Context, target Target) error {
	path, err := target.path()
	if err != nil {
		return err
	}
	snapshot, err := c.Read(ctx, target)
	if err != nil {
		return err
	}
	if stringAt(object(object(snapshot.object, "status"), "operationState"), "phase") != "Running" {
		return nil
	}
	body, err := json.Marshal(map[string]any{"status": map[string]any{"operationState": map[string]any{"phase": "Terminating"}}})
	if err != nil {
		return err
	}
	writing, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := c.api.Do(writing, http.MethodPatch, path, "application/merge-patch+json", body); err != nil {
		return fmt.Errorf("terminate the ArgoCD operation: %w", err)
	}
	return nil
}

// declaredSyncOptions copies the one option that keeps runtime-owned fields (replicas,
// capacity) out of the apply. Nothing else: an option that changes what a full sync is
// has no place in a deploy.
func declaredSyncOptions(obj map[string]any) []any {
	out := []any{}
	options, _ := object(object(obj, "spec"), "syncPolicy")["syncOptions"].([]any)
	for _, option := range options {
		if option == "RespectIgnoreDifferences=true" {
			out = append(out, option)
		}
	}
	return out
}

func syncFacts(obj map[string]any, request string) SyncFacts {
	status := object(obj, "status")
	state := object(status, "operationState")
	pending := object(obj, "operation")
	facts := SyncFacts{
		Pending: pending != nil,
		Phase:   stringAt(state, "phase"),
		Message: stringAt(state, "message"),
		Health:  stringAt(object(status, "health"), "status"),
	}
	facts.Healthy = facts.Health == "Healthy"
	result := object(state, "syncResult")
	if facts.Phase == "Succeeded" {
		facts.Revision = stringAt(result, "revision")
	} else {
		// An operation that failed or still runs says nothing of what ran before it.
		// History holds each full sync that succeeded, oldest first.
		history, _ := status["history"].([]any)
		if len(history) > 0 {
			last, _ := history[len(history)-1].(map[string]any)
			facts.Revision = stringAt(last, "revision")
		}
	}
	if request == "" {
		return facts
	}
	switch {
	case carriesRequest(pending, request):
		facts.Mine = true
		// Written and not yet picked up: whatever operationState says is older.
		facts.Phase, facts.Message = "", ""
	case carriesRequest(object(state, "operation"), request):
		facts.Mine = true
		sync := object(object(state, "operation"), "sync")
		facts.Partial = !emptyArray(sync["resources"])
		facts.Succeeded = !facts.Pending && facts.Phase == "Succeeded" && !facts.Partial
		finished, errFinished := time.Parse(time.RFC3339, stringAt(state, "finishedAt"))
		reconciled, errReconciled := time.Parse(time.RFC3339, stringAt(status, "reconciledAt"))
		facts.Settled = errFinished == nil && errReconciled == nil && reconciled.After(finished)
	}
	return facts
}

func carriesRequest(operation map[string]any, request string) bool {
	info, _ := operation["info"].([]any)
	for _, raw := range info {
		entry, _ := raw.(map[string]any)
		if stringAt(entry, "name") == syncRequestInfo && stringAt(entry, "value") == request {
			return true
		}
	}
	return false
}
