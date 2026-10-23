package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

type queueFixture struct {
	client            *Client
	hub               *fakeHub
	pr, repository    map[string]any
	rules             []any
	checks            []map[string]any
	statuses          []map[string]any
	snapshots, grants int
	mutations         []string
	inputs            []map[string]any
	mutationTokens    []string
	beforeSnapshot    func(int)
	mutate            func(string) (int, string)
	authorize         func(context.Context) (string, error)
}

func queueJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
func queueRun(id int64, name string, app int64, status, conclusion string) map[string]any {
	return map[string]any{"id": id, "name": name, "app": map[string]any{"id": app}, "head_sha": headSHA, "status": status, "conclusion": conclusion}
}
func queueRules(context string, app int64) []any {
	return []any{map[string]any{"type": "required_status_checks", "parameters": map[string]any{"required_status_checks": []any{map[string]any{"context": context, "integration_id": app}}}}}
}
func newQueueFixture(t *testing.T) *queueFixture {
	t.Helper()
	r := map[string]any{"id": "R_widget", "nameWithOwner": "acme/widget"}
	f := &queueFixture{
		hub: newHub(), rules: queueRules("ci-required", 42),
		checks:   []map[string]any{queueRun(17, "ci-required", 42, "completed", "success")},
		statuses: []map[string]any{},
		pr:       map[string]any{"id": "PR_7", "number": 7, "state": "OPEN", "headRefOid": headSHA, "baseRefName": "main", "isDraft": true, "merged": false, "mergeCommit": nil, "mergeable": "MERGEABLE", "mergeStateStatus": "CLEAN", "reviewDecision": "APPROVED", "isInMergeQueue": false, "isMergeQueueEnabled": true, "repository": r, "headRepository": r, "baseRepository": r, "baseRef": map[string]any{"name": "main", "branchProtectionRule": nil}, "mergeQueueEntry": nil},
	}
	f.repository = map[string]any{"id": "R_widget", "nameWithOwner": "acme/widget", "defaultBranchRef": map[string]any{"name": "main"}, "pullRequest": f.pr}
	now := time.Now()
	f.client = testClient(t, f.hub, &now)
	f.authorize = func(context.Context) (string, error) {
		f.grants++
		return fmt.Sprintf("fresh-queue-token-%d", f.grants), nil
	}
	f.hub.on("/graphql", func(req *http.Request) (int, string) {
		if req.Method != http.MethodPost {
			t.Fatal("GraphQL is not POST")
		}
		bodies := f.hub.sentBodies()
		var body struct {
			Query     string
			Variables map[string]any
		}
		if err := json.Unmarshal([]byte(bodies[len(bodies)-1]), &body); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(body.Query, "query MemQLQueueSnapshot") {
			f.snapshots++
			if f.beforeSnapshot != nil {
				f.beforeSnapshot(f.snapshots)
			}
			if body.Variables["owner"] != "acme" || body.Variables["repo"] != "widget" || body.Variables["number"] != float64(7) {
				t.Fatal("unbound PR lookup", body.Variables)
			}
			return 200, queueJSON(map[string]any{"data": map[string]any{"repository": f.repository}})
		}
		op := ""
		if strings.Contains(body.Query, "mutation MemQLQueueReady") {
			op = "ready"
		}
		if strings.Contains(body.Query, "mutation MemQLQueueEnqueue") {
			op = "enqueue"
		}
		if op == "" {
			t.Fatal("unexpected GraphQL write", body.Query)
		}
		input, ok := body.Variables["input"].(map[string]any)
		if !ok || input["pullRequestId"] != "PR_7" {
			t.Fatal("write not bound to observed PR", input)
		}
		if op == "enqueue" {
			if len(input) != 3 || input["expectedHeadOid"] != headSHA || input["jump"] != false {
				t.Fatal("queue write weakened exact-head or ordinary-order guard", input)
			}
		} else if len(input) != 1 {
			t.Fatal("ready mutation uses unsupported or unexpected inputs", input)
		}
		f.mutations = append(f.mutations, op)
		f.inputs = append(f.inputs, input)
		f.mutationTokens = append(f.mutationTokens, req.Header.Get("Authorization"))
		if f.mutate != nil {
			return f.mutate(op)
		}
		f.apply(op)
		return 200, `{"data":{"operation":{"id":"observed-after-write"}}}`
	})
	f.hub.on("/repos/acme/widget/rules/branches/main", func(r *http.Request) (int, string) {
		if r.Method != http.MethodGet {
			t.Fatal("attempt to modify protections")
		}
		return 200, queueJSON(f.rules)
	})
	f.hub.on("/repos/acme/widget/commits/"+headSHA+"/check-runs", func(r *http.Request) (int, string) {
		if r.URL.Query().Get("filter") != "latest" {
			t.Fatal("historical checks could substitute for current checks")
		}
		return 200, queueJSON(map[string]any{"total_count": len(f.checks), "check_runs": f.checks})
	})
	f.hub.on("/repos/acme/widget/commits/"+headSHA+"/statuses", func(*http.Request) (int, string) { return 200, queueJSON(f.statuses) })
	return f
}
func (f *queueFixture) apply(op string) {
	if op == "ready" {
		f.pr["isDraft"] = false
		return
	}
	f.pr["isInMergeQueue"] = true
	f.pr["mergeQueueEntry"] = map[string]any{"id": "MQ_7", "pullRequest": map[string]any{"id": "PR_7", "headRefOid": headSHA}}
}
func (f *queueFixture) run() (QueueAdmission, error) {
	return f.client.QueueReviewedChange(context.Background(), installToken, "acme", "widget", 7, headSHA, "main", f.authorize)
}
func (f *queueFixture) noWrites(t *testing.T) {
	t.Helper()
	if len(f.mutations) != 0 {
		t.Fatal("a refused change was mutated", f.mutations)
	}
}

func TestObserveReviewedChangeTracksQueueWithoutAnyAdmissionEffects(t *testing.T) {
	for _, tc := range []struct {
		name, state, reason string
		alter               func(*queueFixture)
	}{
		{"still queued", "queued", "verifying", func(*queueFixture) {}},
		{"merged", "merged", "merged", func(f *queueFixture) {
			f.pr["state"], f.pr["merged"] = "MERGED", true
			f.pr["mergeCommit"] = map[string]string{"oid": strings.Repeat("b", 40)}
			f.pr["isInMergeQueue"], f.pr["mergeQueueEntry"] = false, nil
		}},
		{"removed", "attention", "no longer in the merge queue", func(f *queueFixture) {
			f.pr["isInMergeQueue"], f.pr["mergeQueueEntry"] = false, nil
		}},
		{"closed", "attention", "closed", func(f *queueFixture) {
			f.pr["state"] = "CLOSED"
			f.pr["isInMergeQueue"], f.pr["mergeQueueEntry"] = false, nil
		}},
		{"new head", "attention", "new commits", func(f *queueFixture) { f.pr["headRefOid"] = strings.Repeat("b", 40) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newQueueFixture(t)
			f.apply("enqueue")
			observe := func() (QueueAdmission, error) {
				return f.client.ObserveReviewedChange(context.Background(), installToken, "acme", "widget", 7, headSHA, "main")
			}
			before, err := observe()
			if err != nil || before.State != "queued" || before.QueueEntryID != "MQ_7" {
				t.Fatalf("initial observation %+v %v", before, err)
			}
			tc.alter(f)
			got, err := observe()
			if err != nil || got.State != tc.state || !strings.Contains(got.Reason, tc.reason) {
				t.Fatalf("updated observation %+v %v", got, err)
			}
			f.noWrites(t)
			if f.grants != 0 || f.snapshots != 2 || len(f.hub.seen()) != 2 {
				t.Fatal("observation attempted authorization or more than exact readback", f.grants, f.snapshots, len(f.hub.seen()))
			}
		})
	}
}

func TestObserveReviewedChangeProviderOutageAndInvalidScopeDoNotWrite(t *testing.T) {
	f := newQueueFixture(t)
	f.hub.json("/graphql", http.StatusServiceUnavailable, `{"message":"Unavailable"}`)
	got, err := f.client.ObserveReviewedChange(context.Background(), installToken, "acme", "widget", 7, headSHA, "main")
	if err == nil || got.State != "" {
		t.Fatalf("outage reported as a change outcome: %+v %v", got, err)
	}
	reads := len(f.hub.seen())
	_, err = f.client.ObserveReviewedChange(context.Background(), installToken, "acme", "widget", 7, "unreviewed", "main")
	if err == nil || len(f.hub.seen()) != reads {
		t.Fatal("invalid reviewed scope reached provider", err)
	}
	f.noWrites(t)
	if f.grants != 0 {
		t.Fatal("observation requested write authorization")
	}
}

func TestQueueReviewedChangeExactReadyAndAdmissionThenRecovery(t *testing.T) {
	f := newQueueFixture(t)
	// Unrelated failure does not silently become a new required check.
	f.checks = append(f.checks, queueRun(18, "optional-experiment", 90, "completed", "failure"))
	for range 2 {
		got, err := f.run()
		if err != nil || got.State != "queued" || got.QueueEntryID != "MQ_7" || got.HeadSHA != headSHA {
			t.Fatalf("%+v %v", got, err)
		}
	}
	if strings.Join(f.mutations, ",") != "ready,enqueue" || f.grants != 4 {
		t.Fatal("effects repeated or not freshly authorized", f.mutations, f.grants)
	}
	if strings.Join(f.mutationTokens, ",") != "Bearer fresh-queue-token-2,Bearer fresh-queue-token-4" {
		t.Fatal("stale bearer used", f.mutationTokens)
	}
}

func TestQueueReviewedChangeReadyStartsRequiredReviewButCannotApproveIt(t *testing.T) {
	f := newQueueFixture(t)
	f.pr["reviewDecision"] = "REVIEW_REQUIRED"
	f.pr["mergeStateStatus"] = "BLOCKED"
	got, err := f.run()
	if err != nil || got.State != "attention" || !strings.Contains(got.Reason, "CODEOWNER") || strings.Join(f.mutations, ",") != "ready" {
		t.Fatalf("%+v %v writes=%v", got, err, f.mutations)
	}
	// The same exact PR is now externally approved; no second ready mutation.
	f.pr["reviewDecision"], f.pr["mergeStateStatus"] = "APPROVED", "CLEAN"
	got, err = f.run()
	if err != nil || got.State != "queued" || strings.Join(f.mutations, ",") != "ready,enqueue" {
		t.Fatalf("%+v %v writes=%v", got, err, f.mutations)
	}
}

func TestQueueReviewedChangeRefusesMissingEvidenceAndProtectionBlockers(t *testing.T) {
	for _, tc := range []struct {
		name, state string
		alter       func(*queueFixture)
	}{
		{"missing required check despite green rollup", "waiting", func(f *queueFixture) {
			f.checks = []map[string]any{queueRun(19, "other", 42, "completed", "success")}
			f.pr["statusCheckRollup"] = map[string]string{"state": "SUCCESS"}
		}},
		{"wrong required app", "waiting", func(f *queueFixture) { f.checks[0]["app"] = map[string]int{"id": 43} }},
		{"check pending", "waiting", func(f *queueFixture) { f.checks[0]["status"] = "in_progress" }},
		{"check failed", "attention", func(f *queueFixture) { f.checks[0]["conclusion"] = "failure" }},
		{"check skipped", "attention", func(f *queueFixture) { f.checks[0]["conclusion"] = "skipped" }},
		{"check wrong head", "", func(f *queueFixture) { f.checks[0]["head_sha"] = strings.Repeat("b", 40) }},
		{"no queue", "attention", func(f *queueFixture) { f.pr["isMergeQueueEnabled"] = false }},
		{"head changed", "attention", func(f *queueFixture) { f.pr["headRefOid"] = strings.Repeat("b", 40) }},
		{"foreign head repository", "attention", func(f *queueFixture) {
			f.pr["headRepository"] = map[string]string{"id": "R_other", "nameWithOwner": "acme/widget"}
		}},
		{"renamed base", "attention", func(f *queueFixture) { f.pr["baseRefName"] = "release" }},
		{"default branch changed", "attention", func(f *queueFixture) { f.repository["defaultBranchRef"] = map[string]string{"name": "trunk"} }},
		{"closed", "attention", func(f *queueFixture) { f.pr["state"] = "CLOSED" }},
		{"changes requested", "attention", func(f *queueFixture) { f.pr["reviewDecision"] = "CHANGES_REQUESTED" }},
		{"review required after ready", "attention", func(f *queueFixture) { f.pr["isDraft"] = false; f.pr["reviewDecision"] = "REVIEW_REQUIRED" }},
		{"unknown mergeability", "waiting", func(f *queueFixture) { f.pr["mergeable"] = "UNKNOWN" }},
		{"conflicts", "attention", func(f *queueFixture) { f.pr["mergeable"] = "CONFLICTING" }},
		{"other protections blocked", "attention", func(f *queueFixture) { f.pr["isDraft"] = false; f.pr["mergeStateStatus"] = "BLOCKED" }},
		{"missing queued boolean", "", func(f *queueFixture) { delete(f.pr, "isInMergeQueue") }},
		{"invalid queue entry", "", func(f *queueFixture) {
			f.apply("enqueue")
			f.pr["mergeQueueEntry"].(map[string]any)["pullRequest"] = map[string]string{"id": "PR_other", "headRefOid": headSHA}
		}},
		{"missing rules", "", func(f *queueFixture) { f.rules = nil }},
		{"missing required contexts", "", func(f *queueFixture) {
			f.rules = []any{map[string]any{"type": "required_status_checks", "parameters": map[string]any{}}}
		}},
		{"missing approval despite codeowner policy", "attention", func(f *queueFixture) {
			f.pr["isDraft"] = false
			f.pr["reviewDecision"] = nil
			f.rules = append(f.rules, map[string]any{"type": "pull_request", "parameters": map[string]any{"require_code_owner_review": true}})
		}},
		{"missing classic required check", "waiting", func(f *queueFixture) {
			f.pr["baseRef"].(map[string]any)["branchProtectionRule"] = map[string]any{"requiresStatusChecks": true, "requiredStatusChecks": []any{map[string]any{"context": "classic-test", "app": map[string]int{"databaseId": 42}}}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newQueueFixture(t)
			tc.alter(f)
			got, err := f.run()
			if tc.state == "" {
				if err == nil {
					t.Fatalf("missing evidence accepted: %+v", got)
				}
			} else if err != nil || got.State != tc.state {
				t.Fatalf("%+v %v", got, err)
			}
			f.noWrites(t)
		})
	}
}

func TestQueueReviewedChangeCommitStatusesAreCurrentAndCannotSpoofAnApp(t *testing.T) {
	for _, tc := range []struct {
		name, state string
		app         int64
		statuses    []map[string]any
	}{
		{"latest failure beats old success", "attention", 0, []map[string]any{{"context": "ci-required", "state": "failure"}, {"context": "ci-required", "state": "success"}}},
		{"latest pending beats old success", "waiting", 0, []map[string]any{{"context": "ci-required", "state": "pending"}, {"context": "ci-required", "state": "success"}}},
		{"unbound status alone can satisfy requirement", "queued", 0, []map[string]any{{"context": "ci-required", "state": "success"}}},
		{"status cannot prove integration binding", "attention", 42, []map[string]any{{"context": "ci-required", "state": "success", "creator": map[string]any{"id": 42, "type": "Bot", "login": "trusted-app[bot]"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newQueueFixture(t)
			f.rules = queueRules("ci-required", tc.app)
			f.checks = []map[string]any{}
			f.statuses = tc.statuses
			got, err := f.run()
			if err != nil || got.State != tc.state {
				t.Fatalf("%+v %v", got, err)
			}
			if tc.state != "queued" {
				f.noWrites(t)
			}
		})
	}
	t.Run("check and status with same name must both pass", func(t *testing.T) {
		f := newQueueFixture(t)
		f.rules = queueRules("ci-required", 0)
		f.statuses = []map[string]any{{"context": "ci-required", "state": "failure"}}
		got, err := f.run()
		if err != nil || got.State != "attention" {
			t.Fatalf("%+v %v", got, err)
		}
		f.noWrites(t)
	})
}

func TestQueueReviewedChangeRechecksAfterGrantRefreshAndBeforeEachWrite(t *testing.T) {
	for _, change := range []string{"head", "checks", "rules", "review", "revoked"} {
		t.Run(change, func(t *testing.T) {
			f := newQueueFixture(t)
			f.pr["isDraft"] = false
			f.authorize = func(context.Context) (string, error) {
				switch change {
				case "head":
					f.pr["headRefOid"] = strings.Repeat("b", 40)
				case "checks":
					f.checks[0]["status"] = "in_progress"
				case "rules":
					f.rules = queueRules("newly-required", 42)
				case "review":
					f.pr["reviewDecision"] = "REVIEW_REQUIRED"
				case "revoked":
					return "", errors.New("grant revoked")
				}
				return "fresh", nil
			}
			got, err := f.run()
			if err == nil && got.State != "waiting" && got.State != "attention" {
				t.Fatalf("%+v %v", got, err)
			}
			f.noWrites(t)
		})
	}
	t.Run("new head after ready cannot enqueue", func(t *testing.T) {
		f := newQueueFixture(t)
		f.mutate = func(op string) (int, string) {
			f.apply(op)
			f.pr["headRefOid"] = strings.Repeat("b", 40)
			return 200, `{"data":{"ready":{}}}`
		}
		got, err := f.run()
		if err != nil || got.State != "attention" || strings.Join(f.mutations, ",") != "ready" {
			t.Fatalf("%+v %v %v", got, err, f.mutations)
		}
	})
	t.Run("fresh review revoked after check read", func(t *testing.T) {
		f := newQueueFixture(t)
		f.pr["isDraft"] = false
		f.beforeSnapshot = func(n int) {
			if n == 4 {
				f.pr["reviewDecision"] = "REVIEW_REQUIRED"
			}
		}
		got, err := f.run()
		if err != nil || got.State != "attention" {
			t.Fatalf("%+v %v", got, err)
		}
		f.noWrites(t)
	})
	t.Run("grant revoked between ready and enqueue", func(t *testing.T) {
		f := newQueueFixture(t)
		f.authorize = func(context.Context) (string, error) {
			f.grants++
			if f.grants == 3 {
				return "", errors.New("revoked")
			}
			return "fresh", nil
		}
		_, err := f.run()
		if err == nil || strings.Join(f.mutations, ",") != "ready" {
			t.Fatal(err, f.mutations)
		}
	})
}

func TestQueueReviewedChangeLostResponsesUseExactReadback(t *testing.T) {
	for _, op := range []string{"ready", "enqueue"} {
		for _, landed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/landed=%v", op, landed), func(t *testing.T) {
				f := newQueueFixture(t)
				if op == "enqueue" {
					f.pr["isDraft"] = false
				}
				f.mutate = func(current string) (int, string) {
					if current == op {
						if landed {
							f.apply(current)
						}
						return 502, `{}`
					}
					f.apply(current)
					return 200, `{"data":{"ok":{}}}`
				}
				got, err := f.run()
				if landed {
					if err != nil || got.State != "queued" {
						t.Fatalf("%+v %v", got, err)
					}
					again, e := f.run()
					if e != nil || again.State != "queued" {
						t.Fatal(again, e)
					}
				} else if StatusOf(err) != 502 {
					t.Fatalf("lost reply hidden: %+v %v", got, err)
				}
				count := 0
				for _, written := range f.mutations {
					if written == op {
						count++
					}
				}
				if count != 1 {
					t.Fatal("uncertain mutation repeated", f.mutations)
				}
			})
		}
	}
	t.Run("lost enqueue followed by changed head is never success", func(t *testing.T) {
		f := newQueueFixture(t)
		f.pr["isDraft"] = false
		f.mutate = func(op string) (int, string) {
			f.apply(op)
			f.pr["headRefOid"] = strings.Repeat("b", 40)
			return 502, `{}`
		}
		got, err := f.run()
		if err != nil || got.State != "attention" {
			t.Fatalf("%+v %v", got, err)
		}
	})
}

func TestQueueReviewedChangeMergedObservationNeverWrites(t *testing.T) {
	f := newQueueFixture(t)
	f.pr["state"], f.pr["merged"] = "MERGED", true
	f.pr["mergeCommit"] = map[string]string{"oid": strings.Repeat("c", 40)}
	got, err := f.run()
	if err != nil || got.State != "merged" {
		t.Fatalf("%+v %v", got, err)
	}
	f.noWrites(t)
	if f.grants != 0 {
		t.Fatal("observation renewed a mutation grant")
	}
	f.pr["mergeCommit"] = nil
	if _, err = f.run(); err == nil {
		t.Fatal("merge without receipt accepted")
	}
}

func TestQueueReviewedChangeProviderOutageNeverMutatesAndErrorsSurvive(t *testing.T) {
	for _, endpoint := range []string{"/graphql", "/repos/acme/widget/rules/branches/main", "/repos/acme/widget/commits/" + headSHA + "/check-runs", "/repos/acme/widget/commits/" + headSHA + "/statuses"} {
		t.Run(endpoint, func(t *testing.T) {
			f := newQueueFixture(t)
			f.hub.json(endpoint, 503, `{}`)
			_, err := f.run()
			if StatusOf(err) != 503 {
				t.Fatal("provider error lost", err)
			}
			f.noWrites(t)
		})
	}
	t.Run("graphql 200 errors are not success", func(t *testing.T) {
		f := newQueueFixture(t)
		f.hub.json("/graphql", 200, `{"errors":[{"message":"provider echoed secret"}],"data":null}`)
		_, err := f.run()
		var gql *QueueGraphQLError
		if !errors.As(err, &gql) || strings.Contains(err.Error(), "secret") {
			t.Fatal(err)
		}
		f.noWrites(t)
	})
}

func TestQueueReviewedChangePagesEveryRequiredRuleAndCheck(t *testing.T) {
	f := newQueueFixture(t)
	f.hub.on("/repos/acme/widget/rules/branches/main", func(r *http.Request) (int, string) {
		if r.URL.Query().Get("page") == "1" {
			rules := make([]any, 100)
			for i := range rules {
				rules[i] = map[string]string{"type": "deletion"}
			}
			return 200, queueJSON(rules)
		}
		return 200, queueJSON(queueRules("ci-required", 42))
	})
	f.hub.on("/repos/acme/widget/commits/"+headSHA+"/check-runs", func(r *http.Request) (int, string) {
		runs := []map[string]any{queueRun(101, "ci-required", 42, "completed", "success")}
		if r.URL.Query().Get("page") == "1" {
			runs = make([]map[string]any, 100)
			for i := range runs {
				runs[i] = queueRun(int64(i+1), fmt.Sprint("optional-", i), 42, "completed", "failure")
			}
		}
		return 200, queueJSON(map[string]any{"total_count": 101, "check_runs": runs})
	})
	got, err := f.run()
	if err != nil || got.State != "queued" {
		t.Fatalf("%+v %v", got, err)
	}
	// Missing pages must fail closed, even when the first page was green.
	f = newQueueFixture(t)
	f.hub.json("/repos/acme/widget/commits/"+headSHA+"/check-runs", 200, queueJSON(map[string]any{"total_count": 2, "check_runs": f.checks}))
	if _, err = f.run(); err == nil {
		t.Fatal("truncated list accepted")
	}
	f.noWrites(t)
}

// The installation can read protection rules the ordinary developer cannot.
// No mutation may borrow that installation authority: the authorizer's user
// token is the only bearer that can mark-ready or enqueue.
func TestQueueReviewedChangeSeparatesPolicyReaderFromDeveloperWriter(t *testing.T) {
	f := newQueueFixture(t)
	original := f.hub.handlers["/graphql"]
	f.hub.on("/graphql", func(r *http.Request) (int, string) {
		bodies := f.hub.sentBodies()
		query := bodies[len(bodies)-1]
		isRead := strings.Contains(query, "query MemQLQueueSnapshot")
		isInstallation := r.Header.Get("Authorization") == "Bearer "+installToken
		if isRead && !isInstallation {
			return 403, `{}` // User has write, not repository-administration access.
		}
		if !isRead && isInstallation {
			t.Fatal("installation observation authority reached a mutable request")
		}
		return original(r)
	})
	for _, endpoint := range []string{"/repos/acme/widget/rules/branches/main", "/repos/acme/widget/commits/" + headSHA + "/check-runs", "/repos/acme/widget/commits/" + headSHA + "/statuses"} {
		handler := f.hub.handlers[endpoint]
		f.hub.on(endpoint, func(r *http.Request) (int, string) {
			if r.Header.Get("Authorization") != "Bearer "+installToken {
				return 403, `{}`
			}
			return handler(r)
		})
	}
	got, err := f.run()
	if err != nil || got.State != "queued" || strings.Join(f.mutationTokens, ",") != "Bearer fresh-queue-token-2,Bearer fresh-queue-token-4" {
		t.Fatalf("ordinary developer could not queue with separate policy read authority: %+v %v %v", got, err, f.mutationTokens)
	}
}

func TestQueueReviewedChangeGrantRevokedDuringPreflightCannotMutate(t *testing.T) {
	f := newQueueFixture(t)
	f.authorize = func(context.Context) (string, error) {
		f.grants++
		if f.grants > 1 {
			return "", errors.New("review lease revoked during provider observation")
		}
		return "fresh-user", nil
	}
	_, err := f.run()
	if err == nil || !strings.Contains(err.Error(), "review lease revoked") {
		t.Fatal("preflight extended the native grant lifetime", err)
	}
	f.noWrites(t)
}
