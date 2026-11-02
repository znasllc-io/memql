package argocd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// fakeApplicationAPI is one Application behind the cluster API, applying the JSON and
// merge patches a real API server applies, so a test reads what the patch really did.
type fakeApplicationAPI struct {
	mu       sync.Mutex
	obj      map[string]any
	patches  [][]map[string]any
	merges   []map[string]any
	failNext error
}

func (f *fakeApplicationAPI) Do(_ context.Context, method, _, contentType string, body []byte) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext != nil {
		err := f.failNext
		f.failNext = nil
		return nil, err
	}
	switch method {
	case http.MethodGet:
		return json.Marshal(f.obj)
	case http.MethodPatch:
		if contentType == "application/merge-patch+json" {
			var merge map[string]any
			if err := json.Unmarshal(body, &merge); err != nil {
				return nil, err
			}
			f.merges = append(f.merges, merge)
			object(f.obj, "status")["operationState"].(map[string]any)["phase"] = "Terminating"
			return json.Marshal(f.obj)
		}
		var patch []map[string]any
		if err := json.Unmarshal(body, &patch); err != nil {
			return nil, err
		}
		f.patches = append(f.patches, patch)
		meta := object(f.obj, "metadata")
		for _, op := range patch {
			switch op["op"] {
			case "test":
				field := strings.TrimPrefix(op["path"].(string), "/metadata/")
				if meta[field] != op["value"] {
					return nil, errors.New("409 the resource changed")
				}
			case "add":
				switch op["path"] {
				case "/operation":
					f.obj["operation"] = op["value"]
				case "/status/operationState":
					delete(object(f.obj, "status"), "operationState")
				}
			}
		}
		meta["resourceVersion"] = "42"
		return json.Marshal(f.obj)
	}
	return nil, errors.New("unexpected " + method)
}

// finish is the controller finishing whatever operation is written.
func (f *fakeApplicationAPI) finish(phase, revision, reconciledAt string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	status := object(f.obj, "status")
	status["operationState"] = map[string]any{
		"phase": phase, "operation": f.obj["operation"], "finishedAt": "2026-11-02T10:00:00Z",
		"syncResult": map[string]any{"revision": revision},
	}
	status["reconciledAt"] = reconciledAt
	delete(f.obj, "operation")
}

const (
	syncRevision     = "1111111111111111111111111111111111111111"
	previousRevision = "2222222222222222222222222222222222222222"
)

func syncFixture(t *testing.T) (*fakeApplicationAPI, *Client, Target) {
	t.Helper()
	obj, err := decodeObject([]byte(`{
  "apiVersion":"argoproj.io/v1alpha1","kind":"Application",
  "metadata":{"namespace":"argocd","name":"memql","uid":"app-uid","resourceVersion":"41","generation":7},
  "spec":{"project":"memql","source":{"repoURL":"https://github.com/acme/install.git","path":"deploy","targetRevision":"main"},
    "destination":{"server":"https://kubernetes.default.svc","namespace":"memql"},
    "syncPolicy":{"syncOptions":["CreateNamespace=false","RespectIgnoreDifferences=true","ApplyOutOfSyncOnly=true"]}},
  "status":{"sync":{"status":"OutOfSync","revision":"3333333333333333333333333333333333333333"},"health":{"status":"Healthy"},
    "operationState":{"phase":"Succeeded","syncResult":{"revision":"` + previousRevision + `"},
      "operation":{"sync":{"prune":true,"resources":[{"kind":"Deployment","name":"old-only"}],"syncOptions":["Replace=true"]}}}}
}`))
	if err != nil {
		t.Fatal(err)
	}
	api := &fakeApplicationAPI{obj: obj}
	client, err := New(api)
	if err != nil {
		t.Fatal(err)
	}
	return api, client, Target{Namespace: "argocd", Name: "memql", UID: "app-uid"}
}

func TestReadApplicationNamesWhatItRunsNow(t *testing.T) {
	_, client, want := syncFixture(t)
	target, facts, err := client.ReadApplication(context.Background(), "argocd", "memql")
	if err != nil {
		t.Fatal(err)
	}
	if target != want {
		t.Errorf("target = %+v", target)
	}
	// The last successful sync, not status.sync.revision: that is what main is now.
	if facts.Revision != previousRevision || !facts.Healthy {
		t.Errorf("facts = %+v", facts)
	}
	if _, _, err := client.ReadApplication(context.Background(), "../x", "memql"); err == nil {
		t.Error("a namespace that is no name was read")
	}
}

func TestStartSyncWritesAFullSyncAtTheCommitAndLeavesTheSpecAlone(t *testing.T) {
	api, client, target := syncFixture(t)
	spec, _ := json.Marshal(object(api.obj, "spec"))
	facts, err := client.StartSync(context.Background(), SyncRequest{Target: target, RequestID: "deploy-1", Revision: syncRevision})
	if err != nil {
		t.Fatal(err)
	}
	if !facts.Mine || !facts.Pending {
		t.Fatalf("facts = %+v", facts)
	}
	after, _ := json.Marshal(object(api.obj, "spec"))
	if string(after) != string(spec) {
		t.Errorf("the sync changed the spec: %s", after)
	}
	sync := object(object(api.obj, "operation"), "sync")
	if sync["revision"] != syncRevision || sync["prune"] != false {
		t.Errorf("sync = %v: it must name the commit and write prune false", sync)
	}
	options, _ := sync["syncOptions"].([]any)
	if len(options) != 1 || options[0] != "RespectIgnoreDifferences=true" {
		t.Errorf("syncOptions = %v: only the option that keeps runtime fields out of the apply", options)
	}
	if object(object(api.obj, "status"), "operationState") != nil {
		t.Error("the earlier operation's state survived, and with it its resource selection")
	}
	tests := 0
	for _, op := range api.patches[0] {
		if op["op"] == "test" {
			tests++
		}
	}
	if tests != 2 {
		t.Errorf("the write is fenced by %d tests, want the uid and the resourceVersion", tests)
	}
}

func TestStartSyncAskedTwiceStartsOnce(t *testing.T) {
	api, client, target := syncFixture(t)
	r := SyncRequest{Target: target, RequestID: "deploy-1", Revision: syncRevision}
	if _, err := client.StartSync(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if _, err := client.StartSync(context.Background(), r); err != nil {
		t.Fatalf("the second ask: %v", err)
	}
	api.finish("Succeeded", syncRevision, "2026-11-02T10:00:30Z")
	if _, err := client.StartSync(context.Background(), r); err != nil {
		t.Fatalf("the ask after it finished: %v", err)
	}
	if len(api.patches) != 1 {
		t.Errorf("%d writes, want one", len(api.patches))
	}
}

func TestStartSyncAfterALostReplyFindsItsOwnSync(t *testing.T) {
	api, client, target := syncFixture(t)
	r := SyncRequest{Target: target, RequestID: "deploy-1", Revision: syncRevision}
	if _, err := client.StartSync(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	api.failNext = errors.New("connection reset")
	if _, err := client.ObserveSync(context.Background(), r); err == nil {
		t.Fatal("the read did not fail as arranged")
	}
	facts, err := client.StartSync(context.Background(), r)
	if err != nil || !facts.Mine {
		t.Fatalf("after a lost reply: %+v, %v", facts, err)
	}
	if len(api.patches) != 1 {
		t.Errorf("%d writes, want one", len(api.patches))
	}
}

func TestStartSyncWillNotReplaceAnotherOperation(t *testing.T) {
	api, client, target := syncFixture(t)
	api.obj["operation"] = map[string]any{"sync": map[string]any{"revision": "someone-elses"}}
	_, err := client.StartSync(context.Background(), SyncRequest{Target: target, RequestID: "deploy-1", Revision: syncRevision})
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want busy", err)
	}
	if len(api.patches) != 0 {
		t.Error("it wrote over another operation")
	}
}

func TestStartSyncRefusesAMovingRevision(t *testing.T) {
	_, client, target := syncFixture(t)
	if _, err := client.StartSync(context.Background(), SyncRequest{Target: target, RequestID: "deploy-1", Revision: "main"}); err == nil {
		t.Fatal("a branch name was accepted")
	}
}

func TestSyncFactsOfTheFinishedOperation(t *testing.T) {
	for _, tc := range []struct {
		name, phase, reconciledAt  string
		wantSucceeded, wantSettled bool
	}{
		{"it passed and the controller has looked since", "Succeeded", "2026-11-02T10:00:30Z", true, true},
		{"it passed and health is still the earlier rollout's", "Succeeded", "2026-11-02T09:59:00Z", true, false},
		{"it failed", "Failed", "2026-11-02T10:00:30Z", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api, client, target := syncFixture(t)
			r := SyncRequest{Target: target, RequestID: "deploy-1", Revision: syncRevision}
			if _, err := client.StartSync(context.Background(), r); err != nil {
				t.Fatal(err)
			}
			api.finish(tc.phase, syncRevision, tc.reconciledAt)
			facts, err := client.ObserveSync(context.Background(), r)
			if err != nil {
				t.Fatal(err)
			}
			if facts.Succeeded != tc.wantSucceeded || facts.Settled != tc.wantSettled || !facts.Mine {
				t.Errorf("facts = %+v", facts)
			}
		})
	}
}

// A selective sync reports Succeeded. That is the failure this file exists for.
func TestASelectiveSyncIsNotADeploy(t *testing.T) {
	api, client, target := syncFixture(t)
	r := SyncRequest{Target: target, RequestID: "deploy-1", Revision: syncRevision}
	if _, err := client.StartSync(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	object(object(api.obj, "operation"), "sync")["resources"] = []any{map[string]any{"kind": "Deployment", "name": "one"}}
	api.finish("Succeeded", syncRevision, "2026-11-02T10:00:30Z")
	facts, err := client.ObserveSync(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if facts.Succeeded || !facts.Partial {
		t.Errorf("facts = %+v", facts)
	}
}

func TestTerminateSyncStopsOnlyWhatRuns(t *testing.T) {
	api, client, target := syncFixture(t)
	if err := client.TerminateSync(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	if len(api.merges) != 0 {
		t.Fatal("it asked to stop an operation that had finished")
	}
	object(object(api.obj, "status"), "operationState")["phase"] = "Running"
	if err := client.TerminateSync(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	if len(api.merges) != 1 || stringAt(object(object(api.obj, "status"), "operationState"), "phase") != "Terminating" {
		t.Errorf("merges = %v", api.merges)
	}
}

// A failed sync says nothing of what ran before it, and that is what a deploy goes back to.
func TestReadApplicationFallsBackOnHistoryAfterAFailedSync(t *testing.T) {
	api, client, _ := syncFixture(t)
	status := object(api.obj, "status")
	object(status, "operationState")["phase"] = "Failed"
	status["history"] = []any{
		map[string]any{"revision": "4444444444444444444444444444444444444444"},
		map[string]any{"revision": previousRevision},
	}
	_, facts, err := client.ReadApplication(context.Background(), "argocd", "memql")
	if err != nil {
		t.Fatal(err)
	}
	if facts.Revision != previousRevision {
		t.Errorf("revision = %q", facts.Revision)
	}
	delete(status, "history")
	if _, facts, err = client.ReadApplication(context.Background(), "argocd", "memql"); err != nil || facts.Revision != "" {
		t.Errorf("with no history: %q, %v", facts.Revision, err)
	}
}
