package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// checks_test.go -- the check run a pipeline writes back to GitHub (epic
// memql#5477, D4): created queued, moved to in_progress, completed with the
// stage table as its summary.

const installToken = "ghs_PIPELINETOKEN_0042"

// sentJSON decodes the body the fake received for the n-th request.
func sentJSON(t *testing.T, hub *fakeHub, n int) map[string]any {
	t.Helper()
	bodies := hub.sentBodies()
	if n >= len(bodies) {
		t.Fatalf("want request %d, the fake saw %d", n, len(bodies))
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(bodies[n]), &out); err != nil {
		t.Fatalf("request %d did not carry a JSON body: %v (%q)", n, err, bodies[n])
	}
	return out
}

func TestCreateCheckRunSendsTheRunAndAnswersItsId(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	hub := newHub().json("/repos/acme/widget/check-runs", http.StatusCreated, `{"id":4242,"status":"queued"}`)
	c := testClient(t, hub, &now)

	id, err := c.CreateCheckRun(context.Background(), installToken, "acme", "widget", CheckRun{
		Name:       "MemQL / memql",
		HeadSHA:    "0123456789abcdef0123456789abcdef01234567",
		Status:     "queued",
		DetailsURL: "https://os.example.com/?pipelineRun=abc",
		ExternalID: "v1:pipelines:run:abc",
		StartedAt:  now,
		Output:     &CheckRunOutput{Title: "Queued", Summary: "Waiting for an agent."},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if id != 4242 {
		t.Fatalf("want the id GitHub answered, got %d", id)
	}

	req := hub.seen()[0]
	if req.Method != http.MethodPost {
		t.Fatalf("create is a POST, got %s", req.Method)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer "+installToken {
		t.Fatalf("the call must carry the bearer it was handed, got %q", got)
	}
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("a JSON body must say so, got Content-Type %q", got)
	}
	body := sentJSON(t, hub, 0)
	for key, want := range map[string]string{
		"name":        "MemQL / memql",
		"head_sha":    "0123456789abcdef0123456789abcdef01234567",
		"status":      "queued",
		"details_url": "https://os.example.com/?pipelineRun=abc",
		"external_id": "v1:pipelines:run:abc",
		"started_at":  "2026-10-03T12:00:00Z",
	} {
		if body[key] != want {
			t.Errorf("%s = %v, want %q", key, body[key], want)
		}
	}
	output, _ := body["output"].(map[string]any)
	if output["title"] != "Queued" || output["summary"] != "Waiting for an agent." {
		t.Errorf("output = %v", body["output"])
	}
	// ZERO MEANS OMITTED: a queued run has no conclusion and no completion
	// time, and sending an empty one is a 422 at GitHub.
	for _, absent := range []string{"conclusion", "completed_at"} {
		if _, ok := body[absent]; ok {
			t.Errorf("%s was sent for a run that has none: %v", absent, body[absent])
		}
	}
	if _, ok := output["text"]; ok {
		t.Errorf("an empty text was sent: %v", output)
	}
}

// A check run with no name or no head SHA is refused before any request:
// GitHub would answer 422, and the run's own row would carry a status nobody
// can act on.
func TestCreateCheckRunRefusesAnIncompleteRunBeforeAnyRequest(t *testing.T) {
	now := time.Now().UTC()
	hub := newHub()
	c := testClient(t, hub, &now)
	for _, run := range []CheckRun{{HeadSHA: "abc"}, {Name: "MemQL / memql"}} {
		if _, err := c.CreateCheckRun(context.Background(), installToken, "acme", "widget", run); err == nil {
			t.Fatalf("an incomplete run must be refused: %+v", run)
		}
	}
	if n := len(hub.seen()); n != 0 {
		t.Fatalf("%d request(s) left for runs that could not be created", n)
	}
}

func TestUpdateCheckRunPatchesTheRunAndCompletesIt(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 30, 0, 0, time.UTC)
	hub := newHub().json("/repos/acme/widget/check-runs/4242", http.StatusOK, `{"id":4242}`)
	c := testClient(t, hub, &now)

	err := c.UpdateCheckRun(context.Background(), installToken, "acme", "widget", 4242, CheckRun{
		Status:      "completed",
		Conclusion:  "failure",
		CompletedAt: now,
		Output:      &CheckRunOutput{Title: "1 of 3 steps failed", Summary: "| stage | step |", Text: "log tail"},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	req := hub.seen()[0]
	if req.Method != http.MethodPatch {
		t.Fatalf("update is a PATCH, got %s", req.Method)
	}
	body := sentJSON(t, hub, 0)
	if body["status"] != "completed" || body["conclusion"] != "failure" || body["completed_at"] != "2026-10-03T12:30:00Z" {
		t.Fatalf("body = %v", body)
	}
	output, _ := body["output"].(map[string]any)
	if output["text"] != "log tail" {
		t.Fatalf("output = %v", body["output"])
	}
	// The update endpoint takes no head_sha: a run's commit is fixed when it
	// is created.
	if _, ok := body["head_sha"]; ok {
		t.Fatalf("an update must not send head_sha: %v", body)
	}

	// A zero id names no run, and nothing is sent.
	if err := c.UpdateCheckRun(context.Background(), installToken, "acme", "widget", 0, CheckRun{Status: "in_progress"}); err == nil {
		t.Fatal("an update with no run id must be refused")
	}
	if n := len(hub.seen()); n != 1 {
		t.Fatalf("want only the first request on the wire, got %d", n)
	}
}

// TestACheckRunRefusedForPermissionIsAStatusError403: an app installed before
// pipelines has no checks: write, and every check-run write answers 403. The
// caller keys on exactly that -- errors.As to a StatusError with Status 403 --
// to record checkRunState "refused" and carry on, so the 403 must arrive as
// one and must NOT read as rate limiting.
func TestACheckRunRefusedForPermissionIsAStatusError403(t *testing.T) {
	now := time.Now().UTC()
	hub := newHub().
		json("/repos/acme/widget/check-runs", http.StatusForbidden, `{"message":"Resource not accessible by integration"}`).
		json("/repos/acme/widget/check-runs/7", http.StatusForbidden, `{"message":"Resource not accessible by integration"}`)
	c := testClient(t, hub, &now)

	_, err := c.CreateCheckRun(context.Background(), installToken, "acme", "widget", CheckRun{Name: "MemQL / memql", HeadSHA: "abc", Status: "queued"})
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusForbidden || se.RateLimited {
		t.Fatalf("want a 403 StatusError that is not rate limiting, got %v", err)
	}
	if se.Endpoint != "/repos/acme/widget/check-runs" {
		t.Fatalf("the error names the endpoint, got %q", se.Endpoint)
	}
	if strings.Contains(err.Error(), installToken) {
		t.Fatalf("the bearer reached the error: %v", err)
	}

	uerr := c.UpdateCheckRun(context.Background(), installToken, "acme", "widget", 7, CheckRun{Status: "in_progress"})
	if StatusOf(uerr) != http.StatusForbidden {
		t.Fatalf("an update refused for permission must answer 403 too, got %v", uerr)
	}
}
