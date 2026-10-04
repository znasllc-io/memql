package pipelinesteps

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/deploycontrol"
)

// kubeReq is one request the fake API server received.
type kubeReq struct {
	Method, Path, Query, ContentType, Body string
}

func (r kubeReq) String() string {
	if r.Query != "" {
		return r.Method + " " + r.Path + "?" + r.Query
	}
	return r.Method + " " + r.Path
}

// kubeAnswer is what the fake answers: a status code and a body.
type kubeAnswer struct {
	code int
	body string
}

// kubeFake is an API server that records every request and answers it from a
// script keyed by "METHOD path" (the query is not part of the key).
type kubeFake struct {
	mu      sync.Mutex
	reqs    []kubeReq
	answers map[string]kubeAnswer
}

// newKubeFake serves answers and returns a Kube for namespace steps-ns that
// talks to it. A request with no scripted answer is a test failure, answered
// 599 so it cannot pass for success.
func newKubeFake(t *testing.T, answers map[string]kubeAnswer) (*Kube, *kubeFake) {
	t.Helper()
	f := &kubeFake{answers: answers}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		req := kubeReq{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, ContentType: r.Header.Get("Content-Type"), Body: string(body)}
		f.mu.Lock()
		f.reqs = append(f.reqs, req)
		a, ok := f.answers[r.Method+" "+r.URL.Path]
		f.mu.Unlock()
		if !ok {
			t.Errorf("unexpected request %s", req)
			a = kubeAnswer{code: 599, body: "no scripted answer"}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(a.code)
		_, _ = io.WriteString(w, a.body)
	}))
	t.Cleanup(srv.Close)
	return NewKube(deploycontrol.NewClusterAPIWith(srv.URL, "test-token", srv.Client()), "steps-ns"), f
}

func (f *kubeFake) requests() []kubeReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]kubeReq(nil), f.reqs...)
}

// wantRequests compares the requests made, by method, path and query, in
// order, and stops the test when they differ: what a test checks next is the
// body of one of them.
func (f *kubeFake) wantRequests(t *testing.T, want ...string) {
	t.Helper()
	var got []string
	for _, r := range f.requests() {
		got = append(got, r.String())
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("requests:\n  got  %q\n  want %q", got, want)
	}
}

// kubeStatus is the API server's refusal, in the shape a k3s v1.32 API server
// answers it (measured 2026-10-04).
func kubeStatus(code int, reason, message string) kubeAnswer {
	b, _ := json.Marshal(map[string]any{
		"kind": "Status", "apiVersion": "v1", "metadata": map[string]any{},
		"status": "Failure", "message": message, "reason": reason, "code": code,
	})
	return kubeAnswer{code: code, body: string(b)}
}

func kubeOK(code int, v any) kubeAnswer {
	b, _ := json.Marshal(v)
	return kubeAnswer{code: code, body: string(b)}
}

const (
	kubeJobs    = "/apis/batch/v1/namespaces/steps-ns/jobs"
	kubeSecrets = "/api/v1/namespaces/steps-ns/secrets"
	kubePods    = "/api/v1/namespaces/steps-ns/pods"
	kubeJobName = "mp-a7a72726d5075767e0b6d115"
)

// kubeJob is a step's Job as the API server returns it.
func kubeJob(uid, resourceVersion string, annots map[string]string) Job {
	return Job{
		APIVersion: "batch/v1", Kind: "Job",
		Metadata: ObjectMeta{
			Name: kubeJobName, Namespace: "steps-ns", UID: uid, ResourceVersion: resourceVersion,
			Annotations: annots,
		},
		Spec: JobSpec{BackoffLimit: ptrTo(int32(0)), ActiveDeadlineSeconds: ptrTo(int64(1200))},
	}
}

// kubeDecode reads a request body as JSON.
func kubeDecode(t *testing.T, body string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("request body is not JSON: %v: %q", err, body)
	}
	return m
}

// TestCreateJobReturnsTheExistingOneOnConflict: a Job's name is derived from
// (run, step, attempt), so AlreadyExists means this step's Job is there --
// created by an earlier attempt of this runner or by another replica. The
// runner adopts it: it gets the existing Job back, read fresh, and is told it
// did not create it.
func TestCreateJobReturnsTheExistingOneOnConflict(t *testing.T) {
	existing := kubeJob("uid-existing", "77", map[string]string{AnnotRunner: "workbench-1 2026-10-04T07:40:00Z"})
	kube, fake := newKubeFake(t, map[string]kubeAnswer{
		"POST " + kubeJobs:                    kubeStatus(409, "AlreadyExists", `jobs.batch "`+kubeJobName+`" already exists`),
		"GET " + kubeJobs + "/" + kubeJobName: kubeOK(200, existing),
	})

	got, created, err := kube.CreateJob(context.Background(), kubeJob("", "", nil))
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if created {
		t.Error("created = true for a Job that already existed")
	}
	if got.Metadata.UID != "uid-existing" || got.Metadata.ResourceVersion != "77" || got.Metadata.Annotations[AnnotRunner] != "workbench-1 2026-10-04T07:40:00Z" {
		t.Errorf("returned %+v, want the existing Job as the API server holds it", got.Metadata)
	}
	fake.wantRequests(t, "POST "+kubeJobs, "GET "+kubeJobs+"/"+kubeJobName)
	if post := fake.requests()[0]; post.ContentType != "application/json" {
		t.Errorf("create content type = %q, want application/json", post.ContentType)
	} else if name, _ := kubeDecode(t, post.Body)["metadata"].(map[string]any)["name"].(string); name != kubeJobName {
		t.Errorf("created Job named %q, want %q", name, kubeJobName)
	}
}

// TestCreateJobReturnsTheJobItCreated: the created Job comes back as the API
// server stored it, uid and resourceVersion included, which OwnSecret and the
// first conditional annotation need.
func TestCreateJobReturnsTheJobItCreated(t *testing.T) {
	kube, fake := newKubeFake(t, map[string]kubeAnswer{
		"POST " + kubeJobs: kubeOK(201, kubeJob("uid-new", "12", nil)),
	})
	got, created, err := kube.CreateJob(context.Background(), kubeJob("", "", nil))
	if err != nil || !created {
		t.Fatalf("CreateJob = created %v, err %v; want created", created, err)
	}
	if got.Metadata.UID != "uid-new" || got.Metadata.ResourceVersion != "12" {
		t.Errorf("returned uid %q rv %q, want uid-new 12", got.Metadata.UID, got.Metadata.ResourceVersion)
	}
	fake.wantRequests(t, "POST "+kubeJobs)
}

// TestCreateJobPassesEveryOtherRefusalThrough: a refusal that is not
// AlreadyExists -- the ceiling's quota first among them, which the runner
// waits out -- reaches the caller typed, and nothing is read back.
func TestCreateJobPassesEveryOtherRefusalThrough(t *testing.T) {
	kube, fake := newKubeFake(t, map[string]kubeAnswer{
		"POST " + kubeJobs: kubeStatus(403, "Forbidden", `jobs.batch "`+kubeJobName+`" is forbidden: exceeded quota: memql-pipelines-ceiling, requested: count/jobs.batch=1, used: count/jobs.batch=4, limited: count/jobs.batch=4`),
	})
	_, created, err := kube.CreateJob(context.Background(), kubeJob("", "", nil))
	if !deploycontrol.IsForbiddenQuota(err) {
		t.Fatalf("err = %v, want the quota refusal, typed", err)
	}
	if created {
		t.Error("created = true on a refusal")
	}
	fake.wantRequests(t, "POST "+kubeJobs)
}

// TestGetJobReadsTheJobAndReportsAnAbsentOneAsNotFound.
func TestGetJobReadsTheJobAndReportsAnAbsentOneAsNotFound(t *testing.T) {
	kube, _ := newKubeFake(t, map[string]kubeAnswer{
		"GET " + kubeJobs + "/" + kubeJobName: kubeOK(200, kubeJob("uid-1", "5", map[string]string{AnnotOutcome: `{"status":"succeeded"}`})),
		"GET " + kubeJobs + "/mp-gone":        kubeStatus(404, "NotFound", `jobs.batch "mp-gone" not found`),
	})
	got, err := kube.GetJob(context.Background(), kubeJobName)
	if err != nil || got.Metadata.UID != "uid-1" || got.Metadata.Annotations[AnnotOutcome] != `{"status":"succeeded"}` {
		t.Errorf("GetJob = %+v, %v; want the Job with its outcome annotation", got.Metadata, err)
	}
	if _, err := kube.GetJob(context.Background(), "mp-gone"); !deploycontrol.IsNotFound(err) {
		t.Errorf("GetJob of an absent Job: err = %v, want NotFound", err)
	}
}

// TestAnnotateJobSendsMergePatchWithResourceVersion: taking a Job over is a
// compare-and-swap. The merge patch carries the resourceVersion the runner
// read, so a replica that lost the race gets a 409 instead of overwriting the
// winner's claim; an empty one patches unconditionally (the outcome, the
// heartbeat).
func TestAnnotateJobSendsMergePatchWithResourceVersion(t *testing.T) {
	patched := kubeJob("uid-1", "42", map[string]string{AnnotRunner: "workbench-0 2026-10-04T07:41:00Z"})
	kube, fake := newKubeFake(t, map[string]kubeAnswer{
		"PATCH " + kubeJobs + "/" + kubeJobName: kubeOK(200, patched),
	})
	annots := map[string]string{AnnotRunner: "workbench-0 2026-10-04T07:41:00Z"}

	got, err := kube.AnnotateJob(context.Background(), kubeJobName, annots, "41")
	if err != nil {
		t.Fatalf("AnnotateJob: %v", err)
	}
	if got.Metadata.ResourceVersion != "42" || got.Metadata.Annotations[AnnotRunner] != "workbench-0 2026-10-04T07:41:00Z" {
		t.Errorf("returned %+v, want the patched Job", got.Metadata)
	}

	if _, err := kube.AnnotateJob(context.Background(), kubeJobName, annots, ""); err != nil {
		t.Fatalf("unconditional AnnotateJob: %v", err)
	}

	reqs := fake.requests()
	if len(reqs) != 2 {
		t.Fatalf("made %d requests, want 2", len(reqs))
	}
	for i, wantBody := range []map[string]any{
		{"metadata": map[string]any{"annotations": map[string]any{AnnotRunner: "workbench-0 2026-10-04T07:41:00Z"}, "resourceVersion": "41"}},
		{"metadata": map[string]any{"annotations": map[string]any{AnnotRunner: "workbench-0 2026-10-04T07:41:00Z"}}},
	} {
		r := reqs[i]
		if r.Method != "PATCH" || r.Path != kubeJobs+"/"+kubeJobName {
			t.Errorf("request %d = %s, want PATCH %s/%s", i, r, kubeJobs, kubeJobName)
		}
		if r.ContentType != "application/merge-patch+json" {
			t.Errorf("request %d content type = %q, want application/merge-patch+json", i, r.ContentType)
		}
		if got := kubeDecode(t, r.Body); !reflect.DeepEqual(got, wantBody) {
			t.Errorf("request %d body = %v, want %v", i, got, wantBody)
		}
	}
}

// TestAnnotateJobNeverClearsTheAnnotations: in a JSON merge patch a null
// deletes, so "annotations": null would wipe every annotation on the Job --
// the persisted outcome and the runner's claim with them. No annotations to
// set is an empty object.
func TestAnnotateJobNeverClearsTheAnnotations(t *testing.T) {
	kube, fake := newKubeFake(t, map[string]kubeAnswer{
		"PATCH " + kubeJobs + "/" + kubeJobName: kubeOK(200, kubeJob("uid-1", "43", nil)),
	})
	if _, err := kube.AnnotateJob(context.Background(), kubeJobName, nil, "42"); err != nil {
		t.Fatalf("AnnotateJob: %v", err)
	}
	fake.wantRequests(t, "PATCH "+kubeJobs+"/"+kubeJobName)
	want := map[string]any{"metadata": map[string]any{"annotations": map[string]any{}, "resourceVersion": "42"}}
	if got := kubeDecode(t, fake.requests()[0].Body); !reflect.DeepEqual(got, want) {
		t.Errorf("patch = %v, want %v", got, want)
	}
}

// TestAnnotateJobReportsALostRaceAsAConflict.
func TestAnnotateJobReportsALostRaceAsAConflict(t *testing.T) {
	kube, _ := newKubeFake(t, map[string]kubeAnswer{
		"PATCH " + kubeJobs + "/" + kubeJobName: kubeStatus(409, "Conflict", `Operation cannot be fulfilled on jobs.batch "`+kubeJobName+`": the object has been modified; please apply your changes to the latest version and try again`),
	})
	_, err := kube.AnnotateJob(context.Background(), kubeJobName, map[string]string{AnnotRunner: "workbench-0 x"}, "41")
	if !deploycontrol.IsConflict(err) {
		t.Errorf("err = %v, want a Conflict", err)
	}
}

// TestCreateSecretTreatsAnExistingSecretAsReattached: the Secret's name is
// derived from the Job's, so AlreadyExists is this step's Secret, left by an
// earlier Run of the same step -- and the caller is told, since the clone
// token in it is that Run's.
func TestCreateSecretTreatsAnExistingSecretAsReattached(t *testing.T) {
	secret := Secret{APIVersion: "v1", Kind: "Secret", Metadata: ObjectMeta{Name: kubeJobName + "-env", Namespace: "steps-ns"}, Type: "Opaque",
		Data: map[string][]byte{"GIT_TOKEN": []byte("clone-" + strings.Repeat("x", 12))}}

	for _, c := range []struct {
		name        string
		answer      kubeAnswer
		wantExisted bool
		wantErr     bool
	}{
		{"created", kubeOK(201, secret), false, false},
		{"already there", kubeStatus(409, "AlreadyExists", `secrets "`+kubeJobName+`-env" already exists`), true, false},
		{"refused", kubeStatus(403, "Forbidden", `secrets is forbidden: User "system:serviceaccount:memql:memql-engine" cannot create resource "secrets"`), false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			kube, fake := newKubeFake(t, map[string]kubeAnswer{"POST " + kubeSecrets: c.answer})
			existed, err := kube.CreateSecret(context.Background(), secret)
			if (err != nil) != c.wantErr || existed != c.wantExisted {
				t.Fatalf("CreateSecret = existed %v, err %v; want existed %v, error %v", existed, err, c.wantExisted, c.wantErr)
			}
			fake.wantRequests(t, "POST "+kubeSecrets)
			r := fake.requests()[0]
			if r.ContentType != "application/json" {
				t.Errorf("content type = %q, want application/json", r.ContentType)
			}
			if name, _ := kubeDecode(t, r.Body)["metadata"].(map[string]any)["name"].(string); name != kubeJobName+"-env" {
				t.Errorf("created Secret named %q", name)
			}
		})
	}
}

// TestSetCloneTokenPatchesOnlyTheTokenKey (fix round 1, minor 7): the token
// is written with a merge patch naming the clone's key alone, so the step's
// secrets beside it stay as they are.
func TestSetCloneTokenPatchesOnlyTheTokenKey(t *testing.T) {
	const token = "ghs_fresh-token-0042"
	path := kubeSecrets + "/" + kubeJobName + "-env"
	kube, fake := newKubeFake(t, map[string]kubeAnswer{"PATCH " + path: kubeOK(200, map[string]any{"kind": "Secret"})})
	if err := kube.SetCloneToken(context.Background(), kubeJobName+"-env", token); err != nil {
		t.Fatalf("SetCloneToken: %v", err)
	}
	fake.wantRequests(t, "PATCH "+path)
	r := fake.requests()[0]
	if r.ContentType != "application/merge-patch+json" {
		t.Errorf("content type = %q, want application/merge-patch+json", r.ContentType)
	}
	want := map[string]any{"data": map[string]any{"GIT_TOKEN": base64.StdEncoding.EncodeToString([]byte(token))}}
	if got := kubeDecode(t, r.Body); !reflect.DeepEqual(got, want) {
		t.Errorf("patch = %v\nwant    %v (the token, base64, under GIT_TOKEN alone)", got, want)
	}
}

// TestSecretCloneTokenReadsTheTokenAlone (fix round 1, minor 8): the one value
// of a Secret that leaves Kube; a Secret without one answers "", and an absent
// Secret the API server's 404.
func TestSecretCloneTokenReadsTheTokenAlone(t *testing.T) {
	path := kubeSecrets + "/" + kubeJobName + "-env"
	withToken := Secret{APIVersion: "v1", Kind: "Secret", Metadata: ObjectMeta{Name: kubeJobName + "-env"},
		Data: map[string][]byte{"GIT_TOKEN": []byte("ghs_held-token-0007"), "NPM_TOKEN": []byte("npm_planted")}}
	without := withToken
	without.Data = map[string][]byte{"NPM_TOKEN": []byte("npm_planted")}
	for _, c := range []struct {
		name     string
		answer   kubeAnswer
		want     string
		notFound bool
	}{
		{"a Secret holding a token", kubeOK(200, withToken), "ghs_held-token-0007", false},
		{"a Secret of an anonymous clone", kubeOK(200, without), "", false},
		{"no Secret", kubeStatus(404, "NotFound", `secrets "`+kubeJobName+`-env" not found`), "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			kube, fake := newKubeFake(t, map[string]kubeAnswer{"GET " + path: c.answer})
			got, err := kube.SecretCloneToken(context.Background(), kubeJobName+"-env")
			if got != c.want || deploycontrol.IsNotFound(err) != c.notFound || (err != nil && !c.notFound) {
				t.Errorf("SecretCloneToken = %q, %v; want %q, not found %v", got, err, c.want, c.notFound)
			}
			fake.wantRequests(t, "GET "+path)
		})
	}
}

// TestOwnSecretMakesTheJobItsOwner: once the Job exists its Secret is owned by
// it, so collecting the Job (the ack, the TTL, a cancel) collects the clone
// token with it. Not as its controller, and without blocking the Job's
// deletion on it.
func TestOwnSecretMakesTheJobItsOwner(t *testing.T) {
	kube, fake := newKubeFake(t, map[string]kubeAnswer{
		"PATCH " + kubeSecrets + "/" + kubeJobName + "-env": kubeOK(200, map[string]any{"kind": "Secret"}),
	})
	if err := kube.OwnSecret(context.Background(), kubeJobName+"-env", kubeJob("uid-9", "3", nil)); err != nil {
		t.Fatalf("OwnSecret: %v", err)
	}
	fake.wantRequests(t, "PATCH "+kubeSecrets+"/"+kubeJobName+"-env")
	r := fake.requests()[0]
	if r.ContentType != "application/merge-patch+json" {
		t.Errorf("content type = %q, want application/merge-patch+json", r.ContentType)
	}
	want := map[string]any{"metadata": map[string]any{"ownerReferences": []any{map[string]any{
		"apiVersion": "batch/v1", "kind": "Job", "name": kubeJobName, "uid": "uid-9",
		"controller": false, "blockOwnerDeletion": false,
	}}}}
	if got := kubeDecode(t, r.Body); !reflect.DeepEqual(got, want) {
		t.Errorf("patch = %v\nwant    %v", got, want)
	}
}

// TestKubeRefusesAnEmptyName (fix round 1): an empty name makes the path of
// the whole collection -- DeleteJob("") would delete every Job in the
// namespace, GetJob("") read a list as one Job -- so every method that acts on
// one object refuses it and sends nothing. The fake answers each collection
// the way the API server would, so a request made would be a success.
func TestKubeRefusesAnEmptyName(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name string
		call func(k *Kube) error
	}{
		{"GetJob", func(k *Kube) error { _, err := k.GetJob(ctx, ""); return err }},
		{"AnnotateJob", func(k *Kube) error {
			_, err := k.AnnotateJob(ctx, "", map[string]string{AnnotRunner: "workbench-0 x"}, "")
			return err
		}},
		{"DeleteJob", func(k *Kube) error { return k.DeleteJob(ctx, "") }},
		{"DeleteSecret", func(k *Kube) error { return k.DeleteSecret(ctx, "") }},
		{"OwnSecret", func(k *Kube) error { return k.OwnSecret(ctx, "", kubeJob("uid-9", "3", nil)) }},
		{"SetCloneToken", func(k *Kube) error { return k.SetCloneToken(ctx, "", "ghs_fresh-token-0042") }},
		{"SecretCloneToken", func(k *Kube) error { _, err := k.SecretCloneToken(ctx, ""); return err }},
	} {
		t.Run(c.name, func(t *testing.T) {
			kube, fake := newKubeFake(t, map[string]kubeAnswer{
				"GET " + kubeJobs:       kubeOK(200, map[string]any{"kind": "JobList", "apiVersion": "batch/v1", "items": []Job{kubeJob("uid-1", "8", nil)}}),
				"PATCH " + kubeJobs:     kubeOK(200, map[string]any{"kind": "Job"}),
				"DELETE " + kubeJobs:    kubeOK(200, map[string]any{"kind": "JobList", "apiVersion": "batch/v1", "items": []Job{}}),
				"GET " + kubeSecrets:    kubeOK(200, map[string]any{"kind": "SecretList", "apiVersion": "v1", "items": []Secret{}}),
				"PATCH " + kubeSecrets:  kubeOK(200, map[string]any{"kind": "Secret"}),
				"DELETE " + kubeSecrets: kubeOK(200, map[string]any{"kind": "SecretList", "apiVersion": "v1", "items": []Secret{}}),
			})
			if err := c.call(kube); err == nil || !strings.Contains(err.Error(), "empty name") {
				t.Errorf("err = %v, want the empty name refused", err)
			}
			fake.wantRequests(t)
		})
	}
}

// TestOwnSecretRefusesAJobWithoutAUID: an owner reference names its owner by
// uid; without one there is nothing to own the Secret, and nothing is sent.
func TestOwnSecretRefusesAJobWithoutAUID(t *testing.T) {
	kube, fake := newKubeFake(t, map[string]kubeAnswer{})
	if err := kube.OwnSecret(context.Background(), kubeJobName+"-env", kubeJob("", "", nil)); err == nil {
		t.Error("OwnSecret accepted a Job with no uid")
	}
	fake.wantRequests(t)
}

// TestDeleteJobIsBackgroundAndAbsentIsSuccess: the API's default for a Job is
// to ORPHAN its pods, which would leave the step running after its Job is
// gone, so every delete asks for background propagation. Deleting what is
// already gone is the outcome wanted.
func TestDeleteJobIsBackgroundAndAbsentIsSuccess(t *testing.T) {
	for _, c := range []struct {
		name    string
		answer  kubeAnswer
		wantErr bool
	}{
		{"deleted", kubeOK(200, map[string]any{"kind": "Job"}), false},
		{"already gone", kubeStatus(404, "NotFound", `jobs.batch "`+kubeJobName+`" not found`), false},
		{"refused", kubeStatus(500, "InternalError", "etcdserver: request timed out"), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			kube, fake := newKubeFake(t, map[string]kubeAnswer{"DELETE " + kubeJobs + "/" + kubeJobName: c.answer})
			if err := kube.DeleteJob(context.Background(), kubeJobName); (err != nil) != c.wantErr {
				t.Errorf("DeleteJob err = %v, want error %v", err, c.wantErr)
			}
			fake.wantRequests(t, "DELETE "+kubeJobs+"/"+kubeJobName+"?propagationPolicy=Background")
		})
	}
}

// TestDeleteSecretIsBackgroundAndAbsentIsSuccess.
func TestDeleteSecretIsBackgroundAndAbsentIsSuccess(t *testing.T) {
	for _, c := range []struct {
		name    string
		answer  kubeAnswer
		wantErr bool
	}{
		{"deleted", kubeOK(200, map[string]any{"kind": "Secret"}), false},
		{"already gone", kubeStatus(404, "NotFound", `secrets "`+kubeJobName+`-env" not found`), false},
		{"refused", kubeStatus(500, "InternalError", "etcdserver: request timed out"), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			kube, fake := newKubeFake(t, map[string]kubeAnswer{"DELETE " + kubeSecrets + "/" + kubeJobName + "-env": c.answer})
			if err := kube.DeleteSecret(context.Background(), kubeJobName+"-env"); (err != nil) != c.wantErr {
				t.Errorf("DeleteSecret err = %v, want error %v", err, c.wantErr)
			}
			fake.wantRequests(t, "DELETE "+kubeSecrets+"/"+kubeJobName+"-env?propagationPolicy=Background")
		})
	}
}

// TestDeleteRunSelectsByRunLabel: a cancel deletes every Job and every Secret
// of the run, on whichever replica holds them, by the run label alone, and
// answers how many Jobs it matched. The API server answers a deletecollection
// with the list of what it deleted (measured).
func TestDeleteRunSelectsByRunLabel(t *testing.T) {
	kube, fake := newKubeFake(t, map[string]kubeAnswer{
		"DELETE " + kubeJobs: kubeOK(200, map[string]any{"kind": "JobList", "apiVersion": "batch/v1",
			"items": []Job{kubeJob("uid-1", "8", nil), kubeJob("uid-2", "9", nil)}}),
		"DELETE " + kubeSecrets: kubeOK(200, map[string]any{"kind": "SecretList", "apiVersion": "v1", "items": []Secret{}}),
	})
	n, err := kube.DeleteRun(context.Background(), "run-7f3a")
	if err != nil {
		t.Fatalf("DeleteRun: %v", err)
	}
	if n != 2 {
		t.Errorf("DeleteRun matched %d Jobs, want 2", n)
	}
	fake.wantRequests(t,
		"DELETE "+kubeJobs+"?labelSelector=memql.io%2Fpipelines-run%3Drun-7f3a&propagationPolicy=Background",
		"DELETE "+kubeSecrets+"?labelSelector=memql.io%2Fpipelines-run%3Drun-7f3a&propagationPolicy=Background",
	)
}

// TestDeleteRunSelectsACanonicalRunByItsLabelValue: a run id that is not a
// legal label value was labelled with its hash (RunLabelValue), so the cancel
// selects by the same hash -- pinned in names_test.go.
func TestDeleteRunSelectsACanonicalRunByItsLabelValue(t *testing.T) {
	kube, fake := newKubeFake(t, map[string]kubeAnswer{
		"DELETE " + kubeJobs:    kubeOK(200, map[string]any{"kind": "JobList", "items": []Job{}}),
		"DELETE " + kubeSecrets: kubeOK(200, map[string]any{"kind": "SecretList", "items": []Secret{}}),
	})
	n, err := kube.DeleteRun(context.Background(), "v1:pipelines:run:7f3a")
	if err != nil || n != 0 {
		t.Fatalf("DeleteRun = %d, %v; want 0, nil", n, err)
	}
	fake.wantRequests(t,
		"DELETE "+kubeJobs+"?labelSelector=memql.io%2Fpipelines-run%3Dr-a52dd336cd341cefd8e6ab57&propagationPolicy=Background",
		"DELETE "+kubeSecrets+"?labelSelector=memql.io%2Fpipelines-run%3Dr-a52dd336cd341cefd8e6ab57&propagationPolicy=Background",
	)
}

// TestDeleteRunDeletesTheSecretsEvenWhenTheJobsFail: the Secrets hold the
// clone token and the step's secrets; a cancel removes them whatever happened
// to the Jobs, and reports the failure.
func TestDeleteRunDeletesTheSecretsEvenWhenTheJobsFail(t *testing.T) {
	kube, fake := newKubeFake(t, map[string]kubeAnswer{
		"DELETE " + kubeJobs:    kubeStatus(500, "InternalError", "etcdserver: request timed out"),
		"DELETE " + kubeSecrets: kubeOK(200, map[string]any{"kind": "SecretList", "items": []Secret{}}),
	})
	if _, err := kube.DeleteRun(context.Background(), "run-7f3a"); err == nil {
		t.Error("DeleteRun hid the Jobs' failure")
	}
	if got := len(fake.requests()); got != 2 {
		t.Errorf("made %d requests, want both collections asked", got)
	}
}

// TestDeleteRunRefusesABlankRun: no run id selects no run, and a cancel that
// matched anything with it would be a cancel of somebody else's run.
func TestDeleteRunRefusesABlankRun(t *testing.T) {
	kube, fake := newKubeFake(t, map[string]kubeAnswer{})
	for _, id := range []string{"", "  "} {
		if _, err := kube.DeleteRun(context.Background(), id); err == nil {
			t.Errorf("DeleteRun(%q) accepted a blank run", id)
		}
	}
	fake.wantRequests(t)
}

func kubePod(name string, created time.Time) Pod {
	return Pod{Metadata: ObjectMeta{Name: name, CreationTimestamp: created}, Status: PodStatus{Phase: "Running"}}
}

// TestJobPodIsTheNewestPodOfTheJob: the pod is found by the job-name label the
// Job controller puts on it; when more than one answers (a leftover of a
// deleted Job of the same name), the newest is the Job's.
func TestJobPodIsTheNewestPodOfTheJob(t *testing.T) {
	older := time.Date(2026, 10, 4, 7, 23, 12, 0, time.UTC)
	kube, fake := newKubeFake(t, map[string]kubeAnswer{
		"GET " + kubePods: kubeOK(200, map[string]any{"kind": "PodList", "apiVersion": "v1", "items": []Pod{
			kubePod(kubeJobName+"-old11", older),
			kubePod(kubeJobName+"-new22", older.Add(time.Minute)),
			kubePod(kubeJobName+"-mid33", older.Add(time.Second)),
		}}),
	})
	got, err := kube.JobPod(context.Background(), kubeJobName)
	if err != nil {
		t.Fatalf("JobPod: %v", err)
	}
	if got == nil || got.Metadata.Name != kubeJobName+"-new22" {
		t.Errorf("JobPod = %+v, want the newest pod", got)
	}
	fake.wantRequests(t, "GET "+kubePods+"?labelSelector=job-name%3D"+kubeJobName)
}

// TestJobPodIsNilBeforeThePodExists: the Job controller creates the pod a
// moment after the Job; until then the answer is no pod, not an error.
func TestJobPodIsNilBeforeThePodExists(t *testing.T) {
	kube, fake := newKubeFake(t, map[string]kubeAnswer{
		"GET " + kubePods: kubeOK(200, map[string]any{"kind": "PodList", "apiVersion": "v1", "items": []Pod{}}),
	})
	got, err := kube.JobPod(context.Background(), kubeJobName)
	if err != nil || got != nil {
		t.Errorf("JobPod = %+v, %v; want nil, nil", got, err)
	}
	fake.wantRequests(t, "GET "+kubePods+"?labelSelector=job-name%3D"+kubeJobName)
}

// Pods exactly as a k3s v1.32.13 API server returned them (2026-10-04), with
// only node-local ids and addresses removed. A misspelt JSON tag on a status
// field is not an error anywhere -- the decoder leaves the field zero and the
// classifier reads "never stopped" -- so the classifier's inputs are decoded
// from the API's own words here, not built as Go values.
const (
	// A service whose startup probe failed: killed (137), restarted, running
	// again with only its last state saying so.
	kubePodServiceRestarted = `{"metadata": {"name": "r-svcprobe2-vkvrt", "creationTimestamp": "2026-10-04T08:03:12Z", "labels": {"batch.kubernetes.io/controller-uid": "658d66cd-6f62-479b-bcb7-927837aa9104", "batch.kubernetes.io/job-name": "r-svcprobe2", "controller-uid": "658d66cd-6f62-479b-bcb7-927837aa9104", "job-name": "r-svcprobe2", "memql.io/pipelines-run": "probe-run-2"}}, "status": {"conditions": [{"lastProbeTime": null, "lastTransitionTime": "2026-10-04T08:03:12Z", "status": "True", "type": "PodReadyToStartContainers"}, {"lastProbeTime": null, "lastTransitionTime": "2026-10-04T08:03:12Z", "message": "containers with incomplete status: [svc-db]", "reason": "ContainersNotInitialized", "status": "False", "type": "Initialized"}, {"lastProbeTime": null, "lastTransitionTime": "2026-10-04T08:03:12Z", "message": "containers with unready status: [svc-db step]", "reason": "ContainersNotReady", "status": "False", "type": "Ready"}, {"lastProbeTime": null, "lastTransitionTime": "2026-10-04T08:03:12Z", "message": "containers with unready status: [svc-db step]", "reason": "ContainersNotReady", "status": "False", "type": "ContainersReady"}, {"lastProbeTime": null, "lastTransitionTime": "2026-10-04T08:03:12Z", "status": "True", "type": "PodScheduled"}], "containerStatuses": [{"image": "alpine:3.20", "lastState": {}, "name": "step", "ready": false, "restartCount": 0, "started": false, "state": {"waiting": {"reason": "PodInitializing"}}}], "initContainerStatuses": [{"image": "docker.io/library/alpine:3.20", "lastState": {}, "name": "clone", "ready": true, "restartCount": 0, "started": false, "state": {"terminated": {"exitCode": 0, "finishedAt": "2026-10-04T08:03:12Z", "reason": "Completed", "startedAt": "2026-10-04T08:03:12Z"}}}, {"image": "docker.io/library/alpine:3.20", "lastState": {"terminated": {"exitCode": 137, "finishedAt": "2026-10-04T08:03:21Z", "reason": "Error", "startedAt": "2026-10-04T08:03:12Z"}}, "name": "svc-db", "ready": false, "restartCount": 1, "started": false, "state": {"running": {"startedAt": "2026-10-04T08:03:21Z"}}}], "phase": "Pending", "startTime": "2026-10-04T08:03:12Z"}}`
	// The same service eleven restarts later, inside its back-off: terminated,
	// with an empty last state and only the restart count.
	kubePodServiceBackOff = `{"metadata": {"name": "r-svcprobe-tktrq", "creationTimestamp": "2026-10-04T07:38:08Z", "labels": {"batch.kubernetes.io/controller-uid": "cef244d1-ddaa-4af2-a17c-f4706263349a", "batch.kubernetes.io/job-name": "r-svcprobe", "controller-uid": "cef244d1-ddaa-4af2-a17c-f4706263349a", "job-name": "r-svcprobe", "memql.io/pipelines-run": "probe-run-2"}}, "status": {"conditions": [{"lastProbeTime": null, "lastTransitionTime": "2026-10-04T07:38:10Z", "status": "True", "type": "PodReadyToStartContainers"}, {"lastProbeTime": null, "lastTransitionTime": "2026-10-04T07:38:09Z", "message": "containers with incomplete status: [svc-db]", "reason": "ContainersNotInitialized", "status": "False", "type": "Initialized"}, {"lastProbeTime": null, "lastTransitionTime": "2026-10-04T07:38:09Z", "message": "containers with unready status: [svc-db step]", "reason": "ContainersNotReady", "status": "False", "type": "Ready"}, {"lastProbeTime": null, "lastTransitionTime": "2026-10-04T07:38:09Z", "message": "containers with unready status: [svc-db step]", "reason": "ContainersNotReady", "status": "False", "type": "ContainersReady"}, {"lastProbeTime": null, "lastTransitionTime": "2026-10-04T07:38:08Z", "status": "True", "type": "PodScheduled"}], "containerStatuses": [{"image": "alpine:3.20", "lastState": {}, "name": "step", "ready": false, "restartCount": 0, "started": false, "state": {"waiting": {"reason": "PodInitializing"}}}], "initContainerStatuses": [{"image": "docker.io/library/alpine:3.20", "lastState": {}, "name": "clone", "ready": true, "restartCount": 0, "started": false, "state": {"terminated": {"exitCode": 0, "finishedAt": "2026-10-04T07:38:09Z", "reason": "Completed", "startedAt": "2026-10-04T07:38:09Z"}}}, {"image": "docker.io/library/alpine:3.20", "lastState": {}, "name": "svc-db", "ready": false, "restartCount": 11, "started": false, "state": {"terminated": {"exitCode": 137, "finishedAt": "2026-10-04T07:55:16Z", "reason": "Error", "startedAt": "2026-10-04T07:55:07Z"}}}], "phase": "Pending", "startTime": "2026-10-04T07:38:09Z"}}`
	// A pod the kubelet refused at admission: failed, no container statuses,
	// no conditions, the reason on the pod alone.
	kubePodRejected = `{"metadata": {"name": "r-rejected-cznfj"}, "status": {"message": "Pod was rejected: Node didn't have enough resource: cpu, requested: 500000, used: 100, capacity: 24000", "phase": "Failed", "qosClass": "Burstable", "reason": "OutOfcpu", "startTime": "2026-10-04T07:59:31Z"}}`
	// A running step evicted (the API a node drain uses) at 08:31:58 and
	// killed by the kubelet when its grace ran out, 08:32:08.
	kubePodDrainedAfterKill = `{"metadata": {"name": "mp-a7a72726d5075767e0b6d115-8s7jz", "creationTimestamp": "2026-10-04T08:31:40Z", "deletionTimestamp": "2026-10-04T08:32:08Z", "labels": {"job-name": "mp-a7a72726d5075767e0b6d115"}}, "status": {"conditions": [{"lastProbeTime": null, "lastTransitionTime": "2026-10-04T08:31:58Z", "message": "Eviction API: evicting", "reason": "EvictionByEvictionAPI", "status": "True", "type": "DisruptionTarget"}, {"lastProbeTime": null, "lastTransitionTime": "2026-10-04T08:32:10Z", "status": "False", "type": "PodReadyToStartContainers"}, {"lastProbeTime": null, "lastTransitionTime": "2026-10-04T08:31:41Z", "status": "True", "type": "Initialized"}, {"lastProbeTime": null, "lastTransitionTime": "2026-10-04T08:32:10Z", "reason": "PodFailed", "status": "False", "type": "Ready"}, {"lastProbeTime": null, "lastTransitionTime": "2026-10-04T08:32:10Z", "reason": "PodFailed", "status": "False", "type": "ContainersReady"}, {"lastProbeTime": null, "lastTransitionTime": "2026-10-04T08:31:40Z", "status": "True", "type": "PodScheduled"}], "containerStatuses": [{"image": "docker.io/library/alpine:3.20", "lastState": {}, "name": "step", "ready": false, "restartCount": 0, "started": false, "state": {"terminated": {"exitCode": 137, "finishedAt": "2026-10-04T08:32:08Z", "reason": "Error", "startedAt": "2026-10-04T08:31:41Z"}}}], "initContainerStatuses": [{"image": "docker.io/library/alpine:3.20", "lastState": {}, "name": "clone", "ready": true, "restartCount": 0, "started": false, "state": {"terminated": {"exitCode": 0, "finishedAt": "2026-10-04T08:31:40Z", "reason": "Completed", "startedAt": "2026-10-04T08:31:40Z"}}}, {"image": "docker.io/library/alpine:3.20", "lastState": {}, "name": "svc-db", "ready": false, "restartCount": 0, "started": false, "state": {"terminated": {"exitCode": 137, "finishedAt": "2026-10-04T08:32:10Z", "reason": "Error", "startedAt": "2026-10-04T08:31:40Z"}}}], "phase": "Failed", "startTime": "2026-10-04T08:31:40Z"}}`
	// A step that exited 0 at 08:31:41, its pod evicted at 08:31:54 while
	// its service still ran.
	kubePodDisruptedAfterExit = `{"metadata": {"name": "mp-a7a72726d5075767e0b6d115-njpwn", "creationTimestamp": "2026-10-04T08:31:40Z", "deletionTimestamp": "2026-10-04T08:32:24Z", "labels": {"job-name": "mp-a7a72726d5075767e0b6d115"}}, "status": {"conditions": [{"lastProbeTime": null, "lastTransitionTime": "2026-10-04T08:31:40Z", "status": "True", "type": "PodReadyToStartContainers"}, {"lastProbeTime": null, "lastTransitionTime": "2026-10-04T08:31:41Z", "status": "True", "type": "Initialized"}, {"lastProbeTime": null, "lastTransitionTime": "2026-10-04T08:31:40Z", "message": "containers with unready status: [step]", "reason": "ContainersNotReady", "status": "False", "type": "Ready"}, {"lastProbeTime": null, "lastTransitionTime": "2026-10-04T08:31:40Z", "message": "containers with unready status: [step]", "reason": "ContainersNotReady", "status": "False", "type": "ContainersReady"}, {"lastProbeTime": null, "lastTransitionTime": "2026-10-04T08:31:40Z", "status": "True", "type": "PodScheduled"}, {"lastProbeTime": null, "lastTransitionTime": "2026-10-04T08:31:54Z", "message": "Eviction API: evicting", "reason": "EvictionByEvictionAPI", "status": "True", "type": "DisruptionTarget"}], "containerStatuses": [{"image": "docker.io/library/alpine:3.20", "lastState": {}, "name": "step", "ready": false, "restartCount": 0, "started": false, "state": {"terminated": {"exitCode": 0, "finishedAt": "2026-10-04T08:31:41Z", "reason": "Completed", "startedAt": "2026-10-04T08:31:41Z"}}}], "initContainerStatuses": [{"image": "docker.io/library/alpine:3.20", "lastState": {}, "name": "clone", "ready": true, "restartCount": 0, "started": false, "state": {"terminated": {"exitCode": 0, "finishedAt": "2026-10-04T08:31:40Z", "reason": "Completed", "startedAt": "2026-10-04T08:31:40Z"}}}, {"image": "docker.io/library/alpine:3.20", "lastState": {}, "name": "svc-db", "ready": true, "restartCount": 0, "started": true, "state": {"running": {"startedAt": "2026-10-04T08:31:40Z"}}}], "phase": "Running", "startTime": "2026-10-04T08:31:40Z"}}`
)

// kubeDisruptedJob is the evicted pods' Job, as it read (measured): started
// 08:31:40 with a 600 s deadline, and marked failing the moment its pod was
// evicted.
func kubeDisruptedJob() Job {
	return Job{
		Metadata: ObjectMeta{Name: kubeJobName},
		Spec:     JobSpec{ActiveDeadlineSeconds: ptrTo(int64(600))},
		Status: JobStatus{
			StartTime:  time.Date(2026, 10, 4, 8, 31, 40, 0, time.UTC),
			Conditions: []JobCondition{clsCond("FailureTarget", "BackoffLimitExceeded", "Job has reached the specified backoff limit")},
		},
	}
}

// kubeDisruption is the pod's DisruptionTarget condition, or nil.
func kubeDisruption(p *Pod) *PodCondition {
	for i, c := range p.Status.Conditions {
		if c.Type == "DisruptionTarget" {
			return &p.Status.Conditions[i]
		}
	}
	return nil
}

// TestJobPodDecodesTheStatusTheClassifierReads: each pod above, read through
// JobPod as the runner reads it, decodes to the fields the classifier needs
// and classifies as what it is.
func TestJobPodDecodesTheStatusTheClassifierReads(t *testing.T) {
	failedJob := clsJob(1200,
		clsCond("FailureTarget", "BackoffLimitExceeded", "Job has reached the specified backoff limit"),
		clsCond("Failed", "BackoffLimitExceeded", "Job has reached the specified backoff limit"))
	for _, c := range []struct {
		name     string
		pod      string
		job      Job
		check    func(t *testing.T, p *Pod)
		phase    Phase
		exitCode int
		code     string   // "" means no Failure
		mentions []string // in the Failure's message, or in Detail when there is none
	}{
		{
			name: "a service restarted after its startup probe failed",
			pod:  kubePodServiceRestarted,
			job:  clsJob(1200),
			check: func(t *testing.T, p *Pod) {
				svc := podContainer(p, true, "svc-db")
				if svc == nil || svc.Image != "docker.io/library/alpine:3.20" || svc.RestartCount != 1 || svc.State.Running == nil ||
					svc.LastState.Terminated == nil || svc.LastState.Terminated.ExitCode != 137 || svc.LastState.Terminated.Reason != "Error" {
					t.Errorf("svc-db decoded as %+v, want image, restartCount 1, running, last state terminated 137", svc)
				}
			},
			phase: PhaseFailed, exitCode: -1, code: "pipeline_service_failed",
			mentions: []string{"exited 137", "restarted once"},
		},
		{
			name: "a running step a drain evicted, after its kill",
			pod:  kubePodDrainedAfterKill,
			job:  kubeDisruptedJob(),
			check: func(t *testing.T, p *Pod) {
				d := kubeDisruption(p)
				if d == nil || d.Status != "True" || d.Reason != "EvictionByEvictionAPI" || !d.LastTransitionTime.Equal(time.Date(2026, 10, 4, 8, 31, 58, 0, time.UTC)) {
					t.Errorf("DisruptionTarget decoded as %+v, want True, EvictionByEvictionAPI, at 08:31:58", d)
				}
			},
			phase: PhaseFailed, exitCode: -1, code: "pipeline_node_lost",
			mentions: []string{"EvictionByEvictionAPI", "Eviction API: evicting"},
		},
		{
			name: "a step that exited 0 before its pod was evicted",
			pod:  kubePodDisruptedAfterExit,
			job:  kubeDisruptedJob(),
			check: func(t *testing.T, p *Pod) {
				d := kubeDisruption(p)
				if d == nil || !d.LastTransitionTime.Equal(time.Date(2026, 10, 4, 8, 31, 54, 0, time.UTC)) {
					t.Errorf("DisruptionTarget decoded as %+v, want it at 08:31:54", d)
				}
			},
			phase: PhaseSucceeded, exitCode: 0,
		},
		{
			name: "a service in its restart back-off",
			pod:  kubePodServiceBackOff,
			job:  clsJob(1200),
			check: func(t *testing.T, p *Pod) {
				svc := podContainer(p, true, "svc-db")
				if svc == nil || svc.RestartCount != 11 || svc.State.Terminated == nil || svc.State.Terminated.ExitCode != 137 || svc.LastState.Terminated != nil {
					t.Errorf("svc-db decoded as %+v, want restartCount 11, terminated 137, an empty last state", svc)
				}
				step := podContainer(p, false, "step")
				if step == nil || step.State.Waiting == nil || step.State.Waiting.Reason != "PodInitializing" {
					t.Errorf("step decoded as %+v, want waiting PodInitializing", step)
				}
			},
			phase: PhaseFailed, exitCode: -1, code: "pipeline_service_failed",
			mentions: []string{"exited 137", "restarted 11 times"},
		},
		{
			name: "a pod the kubelet rejected",
			pod:  kubePodRejected,
			job:  failedJob,
			check: func(t *testing.T, p *Pod) {
				if p.Status.Phase != "Failed" || p.Status.Reason != "OutOfcpu" || !strings.HasPrefix(p.Status.Message, "Pod was rejected:") {
					t.Errorf("pod status decoded as %+v, want Failed, OutOfcpu and the kubelet's message", p.Status)
				}
			},
			phase: PhaseFailed, exitCode: -1, code: "pipeline_node_lost",
			mentions: []string{"OutOfcpu", "Node didn't have enough resource: cpu"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			kube, _ := newKubeFake(t, map[string]kubeAnswer{
				"GET " + kubePods: {code: 200, body: `{"kind": "PodList", "apiVersion": "v1", "metadata": {"resourceVersion": "1600"}, "items": [` + c.pod + `]}`},
			})
			pod, err := kube.JobPod(context.Background(), kubeJobName)
			if err != nil || pod == nil {
				t.Fatalf("JobPod = %v, %v", pod, err)
			}
			c.check(t, pod)

			got := Classify(c.job, pod, clsCreated, clsNow, clsConfig(), "pipeline_step_timeout")
			gotCode, text := "", got.Detail
			if got.Failure != nil {
				gotCode, text = got.Failure.Code, got.Failure.Message
			}
			if got.Phase != c.phase || got.ExitCode != c.exitCode || gotCode != c.code {
				t.Fatalf("classified %s exit %d failure %+v, want %s exit %d code %q", clsPhaseNames[got.Phase], got.ExitCode, got.Failure, clsPhaseNames[c.phase], c.exitCode, c.code)
			}
			for _, m := range c.mentions {
				if !strings.Contains(text, m) {
					t.Errorf("%q does not mention %q", text, m)
				}
			}
		})
	}
}

// TestFollowLogAsksForFollowAndTimestamps: the runner follows the step's
// output as it is written, every line stamped (the capture's cursor), resuming
// an adopted step from the cursor. sinceTime is whole seconds on the wire: the
// API server forwards it to the kubelet as RFC3339 (measured), so the cursor's
// fraction is dropped, never rounded up past a line.
func TestFollowLogAsksForFollowAndTimestamps(t *testing.T) {
	const logText = "2026-10-04T07:39:07.465426787Z line 55\n2026-10-04T07:39:08.466996039Z line 56\n"
	kube, fake := newKubeFake(t, map[string]kubeAnswer{
		"GET " + kubePods + "/" + kubeJobName + "-x7kk6/log": {code: 200, body: logText},
	})
	pod := kubeJobName + "-x7kk6"
	cursor := time.Date(2026, 10, 4, 9, 39, 7, 465426787, time.FixedZone("CEST", 2*60*60))

	for _, since := range []time.Time{cursor, {}} {
		rc, err := kube.FollowLog(context.Background(), pod, "step", since)
		if err != nil {
			t.Fatalf("FollowLog(since %v): %v", since, err)
		}
		got, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil || string(got) != logText {
			t.Errorf("read %q, %v; want the log as served", got, err)
		}
	}
	fake.wantRequests(t,
		"GET "+kubePods+"/"+pod+"/log?container=step&follow=true&sinceTime=2026-10-04T07%3A39%3A07Z&timestamps=true",
		"GET "+kubePods+"/"+pod+"/log?container=step&follow=true&timestamps=true",
	)
}

// TestFollowLogIsBoundedByTheCallersContext: a followed log runs as long as
// the step does, so the caller's context is what ends it.
func TestFollowLogIsBoundedByTheCallersContext(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "2026-10-04T07:39:07Z first\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)
	kube := NewKube(deploycontrol.NewClusterAPIWith(srv.URL, "test-token", srv.Client()), "steps-ns")

	ctx, cancel := context.WithCancel(context.Background())
	rc, err := kube.FollowLog(ctx, "p", "step", time.Time{})
	if err != nil {
		t.Fatalf("FollowLog: %v", err)
	}
	defer rc.Close()
	first := make([]byte, len("2026-10-04T07:39:07Z first\n"))
	if _, err := io.ReadFull(rc, first); err != nil {
		t.Fatalf("first line: %v", err)
	}
	cancel()
	done := make(chan error, 1)
	go func() { _, err := io.ReadAll(rc); done <- err }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling the context did not end the followed log")
	}
}

// TestTailLogAsksForTheLastLines: after a step ends, the runner keeps the last
// lines of the clone and the services; not followed, not stamped.
func TestTailLogAsksForTheLastLines(t *testing.T) {
	const tail = "memql: checked out 4c1f0a7e\n"
	kube, fake := newKubeFake(t, map[string]kubeAnswer{
		"GET " + kubePods + "/p-1/log": {code: 200, body: tail},
	})
	got, err := kube.TailLog(context.Background(), "p-1", "clone", 200)
	if err != nil || got != tail {
		t.Errorf("TailLog = %q, %v; want %q", got, err, tail)
	}
	fake.wantRequests(t, "GET "+kubePods+"/p-1/log?container=clone&tailLines=200")
}

// TestTailLogRefusesNoLines: tailLines 0 asks for nothing and a negative count
// is an API error; neither is a request worth sending.
func TestTailLogRefusesNoLines(t *testing.T) {
	kube, fake := newKubeFake(t, map[string]kubeAnswer{})
	for _, n := range []int{0, -1} {
		if _, err := kube.TailLog(context.Background(), "p-1", "clone", n); err == nil {
			t.Errorf("TailLog(%d lines) accepted", n)
		}
	}
	fake.wantRequests(t)
}

// TestLogOfAContainerThatHasNotStarted: before a container runs, the API
// server answers a log request 400 with the kubelet's sentence (measured: the
// image cannot be pulled, PodInitializing, CreateContainerConfigError, ...).
// The runner meets that answer before every step container starts and for a
// service the step never reached, so it arrives as ErrContainerNotStarted --
// still carrying the API server's error -- and every other refusal arrives as
// it was.
func TestLogOfAContainerThatHasNotStarted(t *testing.T) {
	notStarted := []string{
		`container "step" in pod "mp-x-79dqj" is waiting to start: image can't be pulled`,
		`container "step" in pod "mp-x-79dqj" is waiting to start: trying and failing to pull image`,
		`container "svc-db" in pod "mp-x-rrqrn" is waiting to start: PodInitializing`,
		`container "step" in pod "mp-x-ssrlz" is waiting to start: CreateContainerConfigError`,
		`container "step" in pod "mp-x-ssrlz" is waiting to start - no logs yet`,
		`container "step" in pod "mp-x-ssrlz" is not available`,
		`container "svc-db" in pod "mp-x-rrqrn" is terminated`,
	}
	for _, message := range notStarted {
		answer := kubeStatus(400, "BadRequest", message)
		for _, call := range []string{"FollowLog", "TailLog"} {
			t.Run(call+": "+message, func(t *testing.T) {
				kube, _ := newKubeFake(t, map[string]kubeAnswer{"GET " + kubePods + "/p-1/log": answer})
				err := kubeCallLog(kube, call)
				if !errors.Is(err, ErrContainerNotStarted) {
					t.Errorf("err = %v, want ErrContainerNotStarted", err)
				}
				var se *deploycontrol.StatusError
				if !errors.As(err, &se) || se.Code != 400 {
					t.Errorf("err = %v, want it to still carry the API server's 400", err)
				}
			})
		}
	}

	for name, answer := range map[string]kubeAnswer{
		"a container the pod does not have": kubeStatus(400, "BadRequest", "container nope is not valid for pod mp-x-6crbg"),
		"a malformed parameter":             kubeStatus(400, "BadRequest", `parsing time "x" as "2006-01-02T15:04:05Z07:00": cannot parse "x" as "2006"`),
		"a pod that is gone":                kubeStatus(404, "NotFound", `pods "mp-x-6crbg" not found`),
		"a 400 that is not a Status":        {code: 400, body: "<html>bad request</html>"},
		"a refusal of the log itself":       kubeStatus(403, "Forbidden", `pods "mp-x" is forbidden: User "system:serviceaccount:memql:memql-engine" cannot get resource "pods/log"`),
	} {
		for _, call := range []string{"FollowLog", "TailLog"} {
			t.Run(call+": "+name, func(t *testing.T) {
				kube, _ := newKubeFake(t, map[string]kubeAnswer{"GET " + kubePods + "/p-1/log": answer})
				err := kubeCallLog(kube, call)
				if err == nil || errors.Is(err, ErrContainerNotStarted) {
					t.Errorf("err = %v, want an error that is not ErrContainerNotStarted", err)
				}
			})
		}
	}
}

func kubeCallLog(kube *Kube, call string) error {
	switch call {
	case "FollowLog":
		rc, err := kube.FollowLog(context.Background(), "p-1", "step", time.Time{})
		if rc != nil {
			_ = rc.Close()
		}
		return err
	case "TailLog":
		_, err := kube.TailLog(context.Background(), "p-1", "step", 50)
		return err
	}
	panic(fmt.Sprintf("unknown log call %q", call))
}
