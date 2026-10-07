package argocd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/deploycontrol"
)

var testTarget = Target{Namespace: "argocd", Name: "installation", UID: "original-application-uid"}

const testRevision = "0123456789012345678901234567890123456789"

func applicationFixture(t *testing.T) map[string]any {
	t.Helper()
	obj, err := decodeObject([]byte(`{
  "apiVersion":"argoproj.io/v1alpha1","kind":"Application",
  "metadata":{"namespace":"argocd","name":"installation","uid":"original-application-uid",
    "resourceVersion":"41","generation":7,"annotations":{"other.example/key":"preserve"}},
  "spec":{"project":"installation","source":{"repoURL":"https://github.com/example/installation.git",
    "path":"deploy/instance","targetRevision":"main","kustomize":{"namespace":"memql"}},
    "destination":{"server":"https://kubernetes.default.svc","namespace":"memql"},
    "syncPolicy":{"automated":{"prune":true,"selfHeal":true},"syncOptions":["RespectIgnoreDifferences=true"]},
    "ignoreDifferences":[{"group":"apps","kind":"Deployment","jsonPointers":["/spec/replicas"]}]},
  "status":{"sync":{"status":"Synced","revision":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
    "health":{"status":"Healthy"},"operationState":{"phase":"Succeeded",
      "operation":{"sync":{"prune":true,"resources":[{"kind":"Deployment","name":"old-only"}],"syncOptions":["Force=true"]}}}}}
`))
	require.NoError(t, err)
	return obj
}

// apiFixture interprets JSON Patch test/add/replace atomically, including a
// simulated concurrent writer between GET and PATCH. It is intentionally a
// protocol double; installed Argo reconciliation has separate qualification.
type apiFixture struct {
	mu            sync.Mutex
	object        map[string]any
	patchAttempts int
	writes        int
	loseReply     bool
	concurrent    bool
	server        *httptest.Server
}

func newFixture(t *testing.T) *apiFixture {
	t.Helper()
	f := &apiFixture{object: applicationFixture(t)}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *apiFixture) client(t *testing.T) *Client {
	t.Helper()
	c, err := New(deploycontrol.NewClusterAPIWith(f.server.URL, "fixture-token", f.server.Client()))
	require.NoError(t, err)
	return c
}

func (f *apiFixture) change(t *testing.T, fn func(map[string]any)) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f.object)
}

func (f *apiFixture) configure(loseReply, concurrent bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loseReply, f.concurrent = loseReply, concurrent
}

func (f *apiFixture) writeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writes
}

func (f *apiFixture) patchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.patchAttempts
}

func (f *apiFixture) snapshot() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := canonical(f.object)
	obj, _ := decodeObject(body)
	return obj
}

func (f *apiFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path, _ := testTarget.path()
	if r.URL.Path != "/"+path || r.Header.Get("Authorization") != "Bearer fixture-token" {
		http.Error(w, "wrong target or credential", http.StatusForbidden)
		return
	}
	switch r.Method {
	case http.MethodGet:
		_ = json.NewEncoder(w).Encode(f.object)
	case http.MethodPatch:
		f.patchAttempts++
		if r.Header.Get("Content-Type") != "application/json-patch+json" {
			http.Error(w, "wrong patch format", http.StatusBadRequest)
			return
		}
		var ops []map[string]any
		d := json.NewDecoder(r.Body)
		d.UseNumber()
		if err := d.Decode(&ops); err != nil {
			http.Error(w, "bad patch", http.StatusBadRequest)
			return
		}
		if f.concurrent {
			object(f.object, "metadata")["resourceVersion"] = "concurrent-writer"
		}
		raw, _ := canonical(f.object)
		next, _ := decodeObject(raw)
		for _, op := range ops {
			parts := strings.Split(strings.TrimPrefix(stringAt(op, "path"), "/"), "/")
			parent := next
			for _, key := range parts[:len(parts)-1] {
				parent = object(parent, key)
			}
			if parent == nil {
				http.Error(w, "absent parent", http.StatusUnprocessableEntity)
				return
			}
			key := parts[len(parts)-1]
			switch stringAt(op, "op") {
			case "test":
				if !equalJSON(parent[key], op["value"]) {
					http.Error(w, "test failed", http.StatusUnprocessableEntity)
					return
				}
			case "add", "replace":
				parent[key] = op["value"]
			default:
				http.Error(w, "unsupported patch", http.StatusBadRequest)
				return
			}
		}
		f.writes++
		object(next, "metadata")["generation"] = json.Number("8")
		object(next, "metadata")["resourceVersion"] = fmt.Sprint(41 + f.writes)
		f.object = next
		if f.loseReply {
			f.loseReply = false
			connection, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = connection.Close()
			}
			return
		}
		_ = json.NewEncoder(w).Encode(next)
	default:
		http.Error(w, "unsupported verb", http.StatusMethodNotAllowed)
	}
}

func fixtureIntent(t *testing.T, c *Client) Intent {
	t.Helper()
	snapshot, err := c.Read(context.Background(), testTarget)
	require.NoError(t, err)
	intent, err := PlanRevision(snapshot, "installation-request-1", testRevision, false)
	require.NoError(t, err)
	return intent
}

func finishOperation(obj map[string]any, phase string) {
	status := object(obj, "status")
	op := object(obj, "operation")
	status["operationState"] = map[string]any{
		"phase": phase, "operation": op,
		"syncResult": map[string]any{"revision": testRevision, "source": object(object(obj, "spec"), "source")},
	}
	delete(obj, "operation")
	status["sync"] = map[string]any{
		"status": "Synced", "revision": testRevision,
		"comparedTo": map[string]any{"source": object(object(obj, "spec"), "source")},
	}
}

func TestRevisionSurvivesLostReplyAndSecondClient(t *testing.T) {
	f := newFixture(t)
	c := f.client(t)
	intent := fixtureIntent(t, c)
	digest, err := intent.Digest()
	require.NoError(t, err)
	raw, err := json.Marshal(intent)
	require.NoError(t, err)
	// This round trip models the native installation journal. The second host
	// has none of the first client's local state.
	recovered, err := DecodeIntent(raw)
	require.NoError(t, err)
	recoveredDigest, err := recovered.Digest()
	require.NoError(t, err)
	require.Equal(t, digest, recoveredDigest)
	f.configure(true, false)
	_, err = c.Apply(context.Background(), intent)
	require.ErrorContains(t, err, "unconfirmed")
	require.Equal(t, 1, f.writeCount())
	second := f.client(t)
	facts, err := second.Apply(context.Background(), recovered)
	require.NoError(t, err)
	require.True(t, facts.IntentObserved)
	require.True(t, facts.OperationPending)
	require.False(t, facts.OperationSucceeded)
	require.False(t, facts.RevisionObserved)
	require.Equal(t, 1, f.patchCount())
	require.Nil(t, object(f.snapshot(), "status")["operationState"], "previous selective state must be cleared")
	require.Equal(t, "preserve", stringAt(object(object(f.snapshot(), "metadata"), "annotations"), "other.example/key"))
	require.Equal(t, []any{"RespectIgnoreDifferences=true"}, object(object(f.snapshot(), "operation"), "sync")["syncOptions"])
	before, after, _, err := intent.validate()
	require.NoError(t, err)
	require.True(t, equalJSON(object(f.snapshot(), "spec"), after))
	object(after, "source")["targetRevision"] = stringAt(object(before, "source"), "targetRevision")
	require.True(t, equalJSON(before, after), "repository, destination and all existing policy must stay unchanged")
	f.change(t, func(obj map[string]any) { finishOperation(obj, "Succeeded") })
	facts, err = second.Observe(context.Background(), recovered)
	require.NoError(t, err)
	require.True(t, facts.OperationSucceeded)
	require.True(t, facts.RevisionObserved && facts.Synced && facts.Healthy)
	require.False(t, facts.OperationPending)
	_, err = second.Apply(context.Background(), recovered)
	require.NoError(t, err)
	require.Equal(t, 1, f.patchCount())
}

func TestRevisionRefusesChangedBaselineBeforeWriting(t *testing.T) {
	cases := map[string]func(map[string]any){
		"different application": func(obj map[string]any) { object(obj, "metadata")["uid"] = "replacement-uid" },
		"spec edit":             func(obj map[string]any) { object(obj, "spec")["project"] = "another" },
		"change away and back":  func(obj map[string]any) { object(obj, "metadata")["generation"] = json.Number("9") },
		"successor intent": func(obj map[string]any) {
			object(object(obj, "metadata"), "annotations")[intentAnnotation] = "successor-intent"
		},
		"operation queued": func(obj map[string]any) { obj["operation"] = map[string]any{"sync": map[string]any{}} },
		"operation running": func(obj map[string]any) {
			object(object(obj, "status"), "operationState")["phase"] = "Running"
		},
		"deleting": func(obj map[string]any) { object(obj, "metadata")["deletionTimestamp"] = "2026-10-07T00:00:00Z" },
		"ApplicationSet": func(obj map[string]any) {
			object(obj, "metadata")["ownerReferences"] = []any{map[string]any{"kind": "ApplicationSet"}}
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			c := f.client(t)
			intent := fixtureIntent(t, c)
			f.change(t, change)
			_, err := c.Apply(context.Background(), intent)
			require.Error(t, err)
			require.Zero(t, f.patchCount())
		})
	}
}

func TestRevisionCASDoesNotRetryConcurrentWrite(t *testing.T) {
	f := newFixture(t)
	c := f.client(t)
	intent := fixtureIntent(t, c)
	f.configure(false, true)
	_, err := c.Apply(context.Background(), intent)
	require.ErrorContains(t, err, "unconfirmed")
	require.Equal(t, 1, f.patchCount())
	require.Zero(t, f.writeCount())
	require.Equal(t, "main", stringAt(object(object(f.snapshot(), "spec"), "source"), "targetRevision"))
}

func TestRevisionObservationsCannotBorrowAnotherSync(t *testing.T) {
	cases := map[string]func(map[string]any){
		"prior revision": func(obj map[string]any) { object(object(obj, "status"), "sync")["revision"] = "old" },
		"prior source": func(obj map[string]any) {
			object(object(object(obj, "status"), "sync"), "comparedTo")["source"] = map[string]any{"targetRevision": "main"}
		},
		"foreign marker": func(obj map[string]any) {
			object(object(object(obj, "status"), "operationState"), "operation")["info"] = []any{map[string]any{"name": operationInfoName, "value": "another"}}
		},
		"selective sync": func(obj map[string]any) {
			object(object(object(object(obj, "status"), "operationState"), "operation"), "sync")["resources"] = []any{map[string]any{"kind": "Deployment"}}
		},
		"inline manifests": func(obj map[string]any) {
			object(object(object(object(obj, "status"), "operationState"), "operation"), "sync")["manifests"] = []any{"unreviewed manifest"}
		},
		"operation source": func(obj map[string]any) {
			object(object(object(object(obj, "status"), "operationState"), "operation"), "sync")["source"] = map[string]any{"repoURL": "elsewhere"}
		},
		"different prune": func(obj map[string]any) {
			object(object(object(object(obj, "status"), "operationState"), "operation"), "sync")["prune"] = true
		},
		"wrong result": func(obj map[string]any) {
			object(object(object(obj, "status"), "operationState"), "syncResult")["revision"] = "old"
		},
		"wrong result source": func(obj map[string]any) {
			object(object(object(obj, "status"), "operationState"), "syncResult")["source"] = map[string]any{}
		},
		"failed sync": func(obj map[string]any) { object(object(obj, "status"), "operationState")["phase"] = "Failed" },
		"new spec":    func(obj map[string]any) { object(object(obj, "spec"), "source")["targetRevision"] = "newer" },
		"deleted":     func(obj map[string]any) { object(obj, "metadata")["deletionTimestamp"] = "now" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			c := f.client(t)
			intent := fixtureIntent(t, c)
			_, err := c.Apply(context.Background(), intent)
			require.NoError(t, err)
			f.change(t, func(obj map[string]any) { finishOperation(obj, "Succeeded"); change(obj) })
			facts, err := c.Observe(context.Background(), intent)
			if err == nil {
				require.False(t, facts.OperationSucceeded && facts.RevisionObserved && facts.Synced)
			}
			// Retrying an observed failed/foreign operation never starts a sync.
			_, _ = c.Apply(context.Background(), intent)
			require.Equal(t, 1, f.patchCount())
		})
	}
}

func TestRevisionPlanRequiresImmutableSingleSource(t *testing.T) {
	for name, edit := range map[string]func(*Intent){
		"mutable tag":      func(i *Intent) { i.Revision = "v1.0.0" },
		"missing identity": func(i *Intent) { i.RequestID = "" },
		"path traversal":   func(i *Intent) { i.Target.Name = "../other" },
		"no UID":           func(i *Intent) { i.Target.UID = "" },
		"no generation":    func(i *Intent) { i.BeforeGeneration = 0 },
		"duplicate JSON": func(i *Intent) {
			i.BeforeSpec = json.RawMessage(`{"source":{},"source":{}}`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			c := f.client(t)
			intent := fixtureIntent(t, c)
			edit(&intent)
			_, err := c.Apply(context.Background(), intent)
			require.Error(t, err)
			require.Zero(t, f.patchCount())
		})
	}
	for name, edit := range map[string]func(map[string]any){
		"multiple sources": func(spec map[string]any) { spec["sources"] = []any{spec["source"]} },
		"chart":            func(spec map[string]any) { object(spec, "source")["chart"] = "chart" },
		"image override": func(spec map[string]any) {
			object(object(spec, "source"), "kustomize")["images"] = []any{"example=other:latest"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.change(t, func(obj map[string]any) { edit(object(obj, "spec")) })
			c := f.client(t)
			snapshot, err := c.Read(context.Background(), testTarget)
			require.NoError(t, err)
			_, err = PlanRevision(snapshot, "request", testRevision, false)
			require.Error(t, err)
		})
	}
}

func TestRevisionJournalAndResponseDecoding(t *testing.T) {
	f := newFixture(t)
	intent := fixtureIntent(t, f.client(t))
	raw, err := json.Marshal(intent)
	require.NoError(t, err)
	for _, body := range [][]byte{
		append(append([]byte{}, raw...), []byte(` {}`)...),
		[]byte(strings.Replace(string(raw), `"formatVersion":1`, `"formatVersion":1,"formatVersion":1`, 1)),
		[]byte(strings.Replace(string(raw), `"formatVersion":1`, `"formatVersion":1,"approved":true`, 1)),
		[]byte(strings.Replace(string(raw), `"formatVersion":1`, `"formatVersion":1,"FormatVersion":2`, 1)),
		[]byte(`null`),
	} {
		_, err := DecodeIntent(body)
		require.Error(t, err)
	}
	for _, body := range []string{`{"a":1,"a":2}`, `{} {}`, strings.Repeat("[", 66) + strings.Repeat("]", 66), strings.Repeat(" ", maxApplicationBytes+1)} {
		_, err := decodeObject([]byte(body))
		require.Error(t, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = f.client(t).Apply(ctx, intent)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, f.writeCount())
	require.False(t, errors.Is(err, ErrChanged))
}
