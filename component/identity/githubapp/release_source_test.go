package githubapp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

type releaseSourceFixture struct {
	client                         *Client
	hub                            *fakeHub
	repository, commit, branch, pr map[string]any
	rules                          []any
	checks                         map[string][]map[string]any
	beforeSnapshot                 func(int)
	snapshots                      int
}

func newReleaseSourceFixture(t *testing.T) *releaseSourceFixture {
	t.Helper()
	f := &releaseSourceFixture{hub: newHub(), rules: queueRules("ci-required", 42), checks: map[string][]map[string]any{}}
	f.branch = map[string]any{"name": "main", "target": map[string]string{"oid": headSHA}, "branchProtectionRule": nil}
	f.commit = map[string]any{"oid": headSHA, "tree": map[string]string{"oid": strings.Repeat("b", 40)}, "parents": map[string]any{"nodes": []any{map[string]string{"oid": strings.Repeat("c", 40)}, map[string]string{"oid": strings.Repeat("d", 40)}}, "pageInfo": map[string]bool{"hasNextPage": false}}, "associatedPullRequests": map[string]any{"nodes": []any{}, "pageInfo": map[string]bool{"hasNextPage": false}}}
	f.repository = map[string]any{"id": "R_widget", "databaseId": 123, "nameWithOwner": "acme/widget", "defaultBranchRef": f.branch, "object": f.commit}
	f.checks[headSHA] = []map[string]any{queueRun(17, "ci-required", 42, "completed", "success")}
	f.hub.on("/graphql", func(r *http.Request) (int, string) {
		if r.Method != http.MethodPost {
			t.Fatal("GraphQL query must use POST")
		}
		bodies := f.hub.sentBodies()
		var body struct {
			Query     string
			Variables map[string]any
		}
		if err := json.Unmarshal([]byte(bodies[len(bodies)-1]), &body); err != nil {
			t.Fatal(err)
		}
		if body.Query != releaseSourceQuery || body.Variables["owner"] != "acme" || body.Variables["repo"] != "widget" || body.Variables["selected"] != headSHA && body.Variables["selected"] != "refs/heads/main" {
			t.Fatal("unexpected provider operation or unbound selection")
		}
		f.snapshots++
		if f.beforeSnapshot != nil {
			f.beforeSnapshot(f.snapshots)
		}
		return 200, queueJSON(map[string]any{"data": map[string]any{"repository": f.repository}})
	})
	f.hub.on("/repos/acme/widget/rules/branches/main", func(r *http.Request) (int, string) {
		if r.Method != http.MethodGet {
			t.Fatal("provider rules mutated")
		}
		return 200, queueJSON(f.rules)
	})
	for _, sha := range []string{headSHA, strings.Repeat("d", 40)} {
		f.hub.on("/repos/acme/widget/commits/"+sha+"/check-runs", func(r *http.Request) (int, string) {
			if r.Method != http.MethodGet || r.URL.Query().Get("filter") != "latest" {
				t.Fatal("checks must be a latest-only read")
			}
			checks := f.checks[sha]
			if checks == nil {
				checks = []map[string]any{}
			}
			return 200, queueJSON(map[string]any{"total_count": len(checks), "check_runs": checks})
		})
	}
	now := time.Now()
	f.client = testClient(t, f.hub, &now)
	return f
}

func (f *releaseSourceFixture) merged() {
	identity := map[string]any{"id": "R_widget", "databaseId": 123, "nameWithOwner": "acme/widget"}
	f.pr = map[string]any{"number": 7, "state": "MERGED", "merged": true, "headRefOid": strings.Repeat("d", 40), "baseRefName": "main", "mergeCommit": map[string]string{"oid": headSHA}, "repository": identity, "headRepository": identity, "baseRepository": identity}
	f.commit["associatedPullRequests"] = map[string]any{"nodes": []any{f.pr}, "pageInfo": map[string]bool{"hasNextPage": false}}
	f.checks[headSHA] = nil
	run := queueRun(18, "ci-required", 42, "completed", "success")
	run["head_sha"] = strings.Repeat("d", 40)
	f.checks[strings.Repeat("d", 40)] = []map[string]any{run}
	f.hub.json("/repos/acme/widget/git/commits/"+strings.Repeat("d", 40), 200, queueJSON(map[string]any{"sha": strings.Repeat("d", 40), "tree": map[string]string{"sha": strings.Repeat("b", 40)}}))
}

func TestReleaseSourceRequiresAppBoundSuccessfulChecks(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alter func(*releaseSourceFixture)
	}{
		{"pending", func(f *releaseSourceFixture) { f.checks[headSHA][0]["status"] = "in_progress" }},
		{"failed", func(f *releaseSourceFixture) { f.checks[headSHA][0]["conclusion"] = "failure" }},
		{"neutral", func(f *releaseSourceFixture) { f.checks[headSHA][0]["conclusion"] = "neutral" }},
		{"wrong App", func(f *releaseSourceFixture) { f.checks[headSHA][0]["app"] = map[string]int{"id": 99} }},
		{"wrong SHA", func(f *releaseSourceFixture) { f.checks[headSHA][0]["head_sha"] = strings.Repeat("f", 40) }},
		{"missing", func(f *releaseSourceFixture) { f.checks[headSHA] = nil }},
		{"partial", func(f *releaseSourceFixture) { f.rules = append(f.rules, queueRules("security", 43)...) }},
		{"unbound policy", func(f *releaseSourceFixture) { f.rules = queueRules("ci-required", 0) }},
		{"no required checks", func(f *releaseSourceFixture) { f.rules = []any{} }},
		{"duplicate latest", func(f *releaseSourceFixture) {
			f.checks[headSHA] = append(f.checks[headSHA], queueRun(19, "ci-required", 42, "completed", "success"))
		}},
		{"classic missing App", func(f *releaseSourceFixture) {
			f.branch["branchProtectionRule"] = map[string]any{"requiresStatusChecks": true, "requiredStatusChecks": []any{map[string]any{"context": "classic", "app": nil}}}
		}},
		{"changed source", func(f *releaseSourceFixture) {
			f.beforeSnapshot = func(n int) {
				if n == 2 {
					f.repository["databaseId"] = 999
				}
			}
		}},
		{"changed rules", func(f *releaseSourceFixture) {
			f.beforeSnapshot = func(n int) {
				if n == 2 {
					f.rules = queueRules("changed", 42)
				}
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReleaseSourceFixture(t)
			tc.alter(f)
			if _, err := f.client.ObserveReleaseSource(context.Background(), installToken, "acme", "widget", "main", ""); err == nil {
				t.Fatal("unsafe release evidence accepted")
			}
		})
	}
	f := newReleaseSourceFixture(t)
	got, err := f.client.ObserveReleaseSource(context.Background(), installToken, "acme", "widget", "main", "")
	if err != nil || got.SHA != headSHA || got.ProofSHA != headSHA || got.ProofPullRequest != 0 || len(got.Checks) != 1 || got.Checks[0].AppID != 42 {
		t.Fatalf("direct main proof: %+v %v", got, err)
	}
}

func TestReleaseSourceMergedTreeProofNeverMasksExistingMainFailure(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alter func(*releaseSourceFixture)
	}{
		{"pending main", func(f *releaseSourceFixture) {
			f.checks[headSHA] = []map[string]any{queueRun(30, "ci-required", 42, "in_progress", "")}
		}},
		{"failed main", func(f *releaseSourceFixture) {
			f.checks[headSHA] = []map[string]any{queueRun(30, "ci-required", 42, "completed", "failure")}
		}},
		{"different tree", func(f *releaseSourceFixture) { f.commit["tree"] = map[string]string{"oid": strings.Repeat("e", 40)} }},
		{"head not parent", func(f *releaseSourceFixture) { f.pr["headRefOid"] = strings.Repeat("e", 40) }},
		{"not merged", func(f *releaseSourceFixture) { f.pr["merged"] = false }},
		{"wrong merge", func(f *releaseSourceFixture) { f.pr["mergeCommit"] = map[string]string{"oid": strings.Repeat("e", 40)} }},
		{"foreign PR", func(f *releaseSourceFixture) {
			f.pr["headRepository"] = map[string]any{"id": "R_fork", "databaseId": 999, "nameWithOwner": "fork/widget"}
		}},
		{"ambiguous PR", func(f *releaseSourceFixture) {
			f.commit["associatedPullRequests"].(map[string]any)["nodes"] = []any{f.pr, f.pr}
		}},
		{"truncated PR", func(f *releaseSourceFixture) {
			f.commit["associatedPullRequests"].(map[string]any)["pageInfo"] = map[string]bool{"hasNextPage": true}
		}},
		{"missing completeness", func(f *releaseSourceFixture) { delete(f.commit["associatedPullRequests"].(map[string]any), "pageInfo") }},
		{"failed PR check", func(f *releaseSourceFixture) { f.checks[strings.Repeat("d", 40)][0]["conclusion"] = "failure" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReleaseSourceFixture(t)
			f.merged()
			tc.alter(f)
			if _, err := f.client.ObserveReleaseSource(context.Background(), installToken, "acme", "widget", "main", headSHA); err == nil {
				t.Fatal("invalid merged-tree evidence accepted")
			}
		})
	}
	f := newReleaseSourceFixture(t)
	f.merged()
	got, err := f.client.ObserveReleaseSource(context.Background(), installToken, "acme", "widget", "main", headSHA)
	if err != nil || got.ProofSHA != strings.Repeat("d", 40) || got.ProofPullRequest != 7 || got.SHA != headSHA || got.Checks[0].SHA != got.ProofSHA {
		t.Fatalf("merged proof: %+v %v", got, err)
	}
}

func TestReleaseSourceSelectionRemainsIncludedWithoutRetargeting(t *testing.T) {
	for _, status := range []string{"ahead", "diverged", "behind"} {
		t.Run(status, func(t *testing.T) {
			f := newReleaseSourceFixture(t)
			newHead := strings.Repeat("e", 40)
			f.branch["target"] = map[string]string{"oid": newHead}
			f.hub.json("/repos/acme/widget/compare/"+headSHA+"..."+newHead, 200, queueJSON(map[string]any{"status": status, "behind_by": 0, "base_commit": map[string]string{"sha": headSHA}, "merge_base_commit": map[string]string{"sha": headSHA}}))
			got, err := f.client.ObserveReleaseSource(context.Background(), installToken, "acme", "widget", "main", headSHA)
			if status == "ahead" {
				if err != nil || got.SHA != headSHA || got.HeadSHA != newHead {
					t.Fatalf("selection retargeted: %+v %v", got, err)
				}
			} else if err == nil {
				t.Fatal("non-ancestor accepted")
			}
		})
	}
}

func TestReleaseSourceRefusesTruncatedCheckEvidence(t *testing.T) {
	f := newReleaseSourceFixture(t)
	f.hub.json("/repos/acme/widget/commits/"+headSHA+"/check-runs", 200, queueJSON(map[string]any{"total_count": 2, "check_runs": f.checks[headSHA]}))
	if _, err := f.client.ObserveReleaseSource(context.Background(), installToken, "acme", "widget", "main", headSHA); err == nil {
		t.Fatal("truncated required check list accepted")
	}
}

func TestReleaseSourceRequiresEveryAppWhenOneContextHasMultipleBindings(t *testing.T) {
	f := newReleaseSourceFixture(t)
	f.rules = append(f.rules, queueRules("ci-required", 43)...)
	if _, err := f.client.ObserveReleaseSource(context.Background(), installToken, "acme", "widget", "main", headSHA); err == nil {
		t.Fatal("one App substituted for a second required App")
	}
	f.checks[headSHA] = append(f.checks[headSHA], queueRun(18, "ci-required", 43, "completed", "success"))
	proof, err := f.client.ObserveReleaseSource(context.Background(), installToken, "acme", "widget", "main", headSHA)
	if err != nil || len(proof.Checks) != 2 || proof.Checks[0].AppID != 42 || proof.Checks[1].AppID != 43 {
		t.Fatalf("complete distinct App bindings refused: %+v %v", proof, err)
	}
}
