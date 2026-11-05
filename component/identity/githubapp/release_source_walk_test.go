package githubapp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

// walkSHA is the n-th commit of a walk fixture: forty hex digits, distinct
// for every n.
func walkSHA(n int) string { return fmt.Sprintf("%040x", n) }

// walkFixture is a default branch whose history and checks a test arranges.
// order is the history as GitHub returns it (git log order from the head);
// parents keeps each commit's parents in git's order, first parent first.
type walkFixture struct {
	t       *testing.T
	client  *Client
	hub     *fakeHub
	head    string
	rules   []any
	order   []string
	parents map[string][]string
	checks  map[string][]map[string]any
	// page caps how many commits one history read answers with, so a test can
	// make the first-parent chain leave a read without a hundred commits.
	page int
	// listed records every check listing as "sha check_name", in order.
	listed []string
	// anchors records where each history read started.
	anchors []string
	// history lets a test damage one history answer before it is sent.
	history func(nodes []any) []any
}

func newWalkFixture(t *testing.T) *walkFixture {
	t.Helper()
	f := &walkFixture{t: t, hub: newHub(), rules: queueRules("ci-required", 42), parents: map[string][]string{}, checks: map[string][]map[string]any{}}
	f.hub.on("/graphql", f.graphql)
	f.hub.on("/repos/acme/widget/rules/branches/main", func(r *http.Request) (int, string) {
		if r.Method != http.MethodGet {
			t.Fatal("provider rules mutated")
		}
		return 200, queueJSON(f.rules)
	})
	now := time.Now()
	f.client = testClient(t, f.hub, &now)
	return f
}

// linear arranges n commits, walkSHA(1) the head, each the only parent of the
// one before it, walkSHA(n) the root.
func (f *walkFixture) linear(n int) {
	for i := 1; i <= n; i++ {
		var parents []string
		if i < n {
			parents = []string{walkSHA(i + 1)}
		}
		f.commit(walkSHA(i), parents...)
	}
}

// commit appends sha to the history order with its parents, first parent
// first.
func (f *walkFixture) commit(sha string, parents ...string) {
	if f.head == "" {
		f.head = sha
	}
	f.order = append(f.order, sha)
	f.parents[sha] = parents
	f.hub.on("/repos/acme/widget/commits/"+sha+"/check-runs", func(r *http.Request) (int, string) {
		if r.Method != http.MethodGet || r.URL.Query().Get("filter") != "latest" {
			f.t.Fatal("checks must be a latest-only read")
		}
		name := r.URL.Query().Get("check_name")
		f.listed = append(f.listed, sha+" "+name)
		runs := []map[string]any{}
		for _, run := range f.checks[sha] {
			if name == "" || run["name"] == name {
				runs = append(runs, run)
			}
		}
		return 200, queueJSON(map[string]any{"total_count": len(runs), "check_runs": runs})
	})
}

// run adds a latest check run on sha.
func (f *walkFixture) run(sha string, id int64, name string, app int64, status, conclusion string) map[string]any {
	run := queueRun(id, name, app, status, conclusion)
	run["head_sha"] = sha
	f.checks[sha] = append(f.checks[sha], run)
	return run
}

func (f *walkFixture) graphql(r *http.Request) (int, string) {
	if r.Method != http.MethodPost {
		f.t.Fatal("GraphQL query must use POST")
	}
	bodies := f.hub.sentBodies()
	var body struct {
		Query     string
		Variables map[string]any
	}
	if err := json.Unmarshal([]byte(bodies[len(bodies)-1]), &body); err != nil {
		f.t.Fatal(err)
	}
	if body.Variables["owner"] != "acme" || body.Variables["repo"] != "widget" {
		f.t.Fatal("unbound repository")
	}
	switch body.Query {
	case releaseSourceQuery:
		if body.Variables["selected"] != "refs/heads/main" {
			f.t.Fatalf("the walk must read the default branch's head, not %v", body.Variables["selected"])
		}
		head := map[string]any{"oid": f.head, "tree": map[string]string{"oid": strings.Repeat("b", 40)}, "parents": map[string]any{"nodes": []any{}, "pageInfo": map[string]bool{"hasNextPage": false}}, "associatedPullRequests": map[string]any{"nodes": []any{}, "pageInfo": map[string]bool{"hasNextPage": false}}}
		branch := map[string]any{"name": "main", "target": map[string]string{"oid": f.head}, "branchProtectionRule": nil}
		return 200, queueJSON(map[string]any{"data": map[string]any{"repository": map[string]any{"id": "R_widget", "databaseId": 123, "nameWithOwner": "acme/widget", "defaultBranchRef": branch, "object": head}}})
	case releaseSourceHistoryQuery:
		anchor, _ := body.Variables["head"].(string)
		if window, _ := body.Variables["window"].(float64); int(window) != releaseSourceHistoryWindow {
			f.t.Fatalf("history window %v", body.Variables["window"])
		}
		f.anchors = append(f.anchors, anchor)
		start := -1
		for i, sha := range f.order {
			if sha == anchor {
				start = i
			}
		}
		if start < 0 {
			return 200, queueJSON(map[string]any{"data": map[string]any{"repository": map[string]any{"object": nil}}})
		}
		end := len(f.order)
		if f.page > 0 && start+f.page < end {
			end = start + f.page
		}
		nodes := []any{}
		for _, sha := range f.order[start:end] {
			parents := []any{}
			for i, parent := range f.parents[sha] {
				if i < 2 {
					parents = append(parents, map[string]string{"oid": parent})
				}
			}
			nodes = append(nodes, map[string]any{"oid": sha, "parents": map[string]any{"totalCount": len(f.parents[sha]), "nodes": parents}})
		}
		if f.history != nil {
			nodes = f.history(nodes)
		}
		return 200, queueJSON(map[string]any{"data": map[string]any{"repository": map[string]any{"object": map[string]any{"oid": anchor, "history": map[string]any{"nodes": nodes}}}}})
	}
	f.t.Fatalf("unexpected provider operation %q", body.Query)
	return 0, ""
}

func (f *walkFixture) walk(lookBack int) (ReleaseSourceWalk, error) {
	return f.client.WalkReleaseSource(context.Background(), installToken, "acme", "widget", "main", lookBack)
}

// A merge's second parent and everything only it reaches were never main's
// head: the walk follows first parents from the head, so a green commit on a
// merged branch is never chosen and its checks are never read.
func TestReleaseSourceWalkFollowsFirstParentsFromTheHead(t *testing.T) {
	f := newWalkFixture(t)
	merge, branchTip, previous, root := walkSHA(1), walkSHA(2), walkSHA(3), walkSHA(4)
	f.commit(merge, previous, branchTip)
	f.commit(branchTip, previous)
	f.commit(previous, root)
	f.commit(root)
	f.run(merge, 1, "ci-required", 42, "in_progress", "")
	f.run(branchTip, 2, "ci-required", 42, "completed", "success")
	f.run(previous, 3, "ci-required", 42, "completed", "success")
	got, err := f.walk(20)
	if err != nil {
		t.Fatal(err)
	}
	want := ReleaseSourceWalk{RepositoryID: 123, Repository: "acme/widget", Branch: "main", HeadSHA: merge, LookBack: 20, Pick: previous,
		Newer: []ReleaseSourceSkip{{SHA: merge, State: ReleaseSourcePending, Check: "ci-required"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("walk:\n got %+v\nwant %+v", got, want)
	}
	if !reflect.DeepEqual(f.listed, []string{merge + " ci-required", previous + " ci-required"}) {
		t.Fatalf("the walk read checks off the first-parent chain: %v", f.listed)
	}
	if !reflect.DeepEqual(f.anchors, []string{merge}) {
		t.Fatalf("history reads: %v", f.anchors)
	}
}

func TestReleaseSourceWalkPicksAGreenHeadWithNothingSkipped(t *testing.T) {
	f := newWalkFixture(t)
	f.linear(3)
	f.run(walkSHA(1), 1, "ci-required", 42, "completed", "success")
	got, err := f.walk(20)
	if err != nil || got.Pick != walkSHA(1) || got.HeadSHA != walkSHA(1) || len(got.Newer) != 0 || len(f.listed) != 1 {
		t.Fatalf("green head: %+v %v (listed %v)", got, err, f.listed)
	}
}

// Every state a commit can be skipped in, and what the record keeps of it.
func TestReleaseSourceWalkSkipsEveryUntestedState(t *testing.T) {
	f := newWalkFixture(t)
	f.linear(7)
	f.run(walkSHA(1), 1, "ci-required", 42, "queued", "")
	f.run(walkSHA(2), 2, "ci-required", 42, "completed", "failure")
	f.run(walkSHA(3), 3, "ci-required", 42, "completed", "cancelled")
	// walkSHA(4) has no run: a commit inside a multi-commit push.
	f.run(walkSHA(5), 5, "ci-required", 42, "completed", "neutral")
	f.run(walkSHA(6), 6, "ci-required", 42, "completed", "success")
	f.run(walkSHA(7), 7, "ci-required", 42, "completed", "success")
	got, err := f.walk(20)
	if err != nil {
		t.Fatal(err)
	}
	want := []ReleaseSourceSkip{
		{SHA: walkSHA(1), State: ReleaseSourcePending, Check: "ci-required"},
		{SHA: walkSHA(2), State: ReleaseSourceFailed, Check: "ci-required", Conclusion: "failure"},
		{SHA: walkSHA(3), State: ReleaseSourceCancelled, Check: "ci-required", Conclusion: "cancelled"},
		{SHA: walkSHA(4), State: ReleaseSourceMissing, Check: "ci-required"},
		{SHA: walkSHA(5), State: ReleaseSourceFailed, Check: "ci-required", Conclusion: "neutral"},
	}
	if got.Pick != walkSHA(6) || !reflect.DeepEqual(got.Newer, want) {
		t.Fatalf("walk picked %s after\n%+v\nwant %s after\n%+v", got.Pick, got.Newer, walkSHA(6), want)
	}
	if len(f.listed) != 6 {
		t.Fatalf("the walk read past its pick: %v", f.listed)
	}
}

// The bound counts commits examined, the pick included, and the walk reads no
// check beyond it.
func TestReleaseSourceWalkStopsAtItsBound(t *testing.T) {
	f := newWalkFixture(t)
	f.linear(25)
	f.run(walkSHA(21), 21, "ci-required", 42, "completed", "success")
	got, err := f.walk(20)
	if err != nil || got.Pick != "" || len(got.Newer) != 20 || got.Newer[0].SHA != walkSHA(1) || got.Newer[19].SHA != walkSHA(20) || len(f.listed) != 20 {
		t.Fatalf("bounded walk: pick %q, %d skipped, %d listed, %v", got.Pick, len(got.Newer), len(f.listed), err)
	}
	for _, skip := range got.Newer {
		if skip.State != ReleaseSourceMissing {
			t.Fatalf("untested commit recorded as %+v", skip)
		}
	}
	f = newWalkFixture(t)
	f.linear(25)
	f.run(walkSHA(20), 20, "ci-required", 42, "completed", "success")
	if got, err = f.walk(20); err != nil || got.Pick != walkSHA(20) || len(got.Newer) != 19 {
		t.Fatalf("the last commit inside the bound: pick %q, %d skipped, %v", got.Pick, len(got.Newer), err)
	}
}

// A root commit ends the walk: there is nothing older to look at.
func TestReleaseSourceWalkEndsAtTheRoot(t *testing.T) {
	f := newWalkFixture(t)
	f.linear(3)
	got, err := f.walk(20)
	if err != nil || got.Pick != "" || len(got.Newer) != 3 || len(f.listed) != 3 {
		t.Fatalf("root: %+v %v", got, err)
	}
}

// A merge commit nobody tested can still be proven through its merged pull
// request, which only the proof can judge, so the walk offers it rather than
// skipping it. An octopus merge cannot be, and is skipped.
func TestReleaseSourceWalkLeavesAnUntestedMergeToTheProof(t *testing.T) {
	f := newWalkFixture(t)
	merge, previous, branchTip := walkSHA(1), walkSHA(2), walkSHA(3)
	f.commit(merge, previous, branchTip)
	f.commit(previous)
	f.commit(branchTip)
	f.run(previous, 2, "ci-required", 42, "completed", "success")
	if got, err := f.walk(20); err != nil || got.Pick != merge || len(got.Newer) != 0 {
		t.Fatalf("untested merge: %+v %v", got, err)
	}

	f = newWalkFixture(t)
	octopus, third := walkSHA(1), walkSHA(4)
	f.commit(octopus, previous, branchTip, third)
	f.commit(previous)
	f.commit(branchTip)
	f.commit(third)
	f.run(previous, 2, "ci-required", 42, "completed", "success")
	got, err := f.walk(20)
	if err != nil || got.Pick != previous || len(got.Newer) != 1 || got.Newer[0].State != ReleaseSourceMissing {
		t.Fatalf("untested octopus merge: %+v %v", got, err)
	}
}

// With two required checks the record names the one that decided, and a
// commit is green only when both are.
func TestReleaseSourceWalkNamesTheCheckThatDecided(t *testing.T) {
	f := newWalkFixture(t)
	f.rules = append(f.rules, queueRules("security", 43)...)
	f.linear(4)
	f.run(walkSHA(1), 1, "ci-required", 42, "completed", "success")
	f.run(walkSHA(2), 2, "ci-required", 42, "completed", "success")
	f.run(walkSHA(2), 3, "security", 43, "in_progress", "")
	f.run(walkSHA(3), 4, "ci-required", 42, "in_progress", "")
	f.run(walkSHA(3), 5, "security", 43, "completed", "failure")
	f.run(walkSHA(4), 6, "ci-required", 42, "completed", "success")
	f.run(walkSHA(4), 7, "security", 43, "completed", "success")
	got, err := f.walk(20)
	if err != nil {
		t.Fatal(err)
	}
	want := []ReleaseSourceSkip{
		{SHA: walkSHA(1), State: ReleaseSourceMissing, Check: "security"},
		{SHA: walkSHA(2), State: ReleaseSourcePending, Check: "security"},
		{SHA: walkSHA(3), State: ReleaseSourceFailed, Check: "security", Conclusion: "failure"},
	}
	if got.Pick != walkSHA(4) || !reflect.DeepEqual(got.Newer, want) {
		t.Fatalf("two checks: pick %s after %+v", got.Pick, got.Newer)
	}
	if f.listed[0] != walkSHA(1)+" ci-required" || f.listed[1] != walkSHA(1)+" security" {
		t.Fatalf("each required check is listed by its own name: %v", f.listed)
	}
}

// A merge of a long branch puts the previous head of main further down the
// history than one read reaches. The walk reads again from the first parent
// it is missing instead of ending there.
func TestReleaseSourceWalkReadsHistoryAgainWhenTheChainLeavesIt(t *testing.T) {
	f := newWalkFixture(t)
	f.page = 2
	f.linear(6)
	f.run(walkSHA(5), 5, "ci-required", 42, "completed", "success")
	got, err := f.walk(20)
	if err != nil || got.Pick != walkSHA(5) || len(got.Newer) != 4 {
		t.Fatalf("paged history: %+v %v", got, err)
	}
	if !reflect.DeepEqual(f.anchors, []string{walkSHA(1), walkSHA(3), walkSHA(5)}) {
		t.Fatalf("history reads: %v", f.anchors)
	}
}

// Evidence the walk cannot read is never skipped: a commit it cannot classify
// may be the one that passed, so the walk stops rather than reaching past it
// to an older green commit.
func TestReleaseSourceWalkAbortsOnEvidenceItCannotTrust(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alter func(*walkFixture)
	}{
		{"required name reported by a different App", func(f *walkFixture) { f.run(walkSHA(1), 1, "ci-required", 99, "completed", "failure") }},
		{"unbound check", func(f *walkFixture) {
			f.run(walkSHA(1), 1, "ci-required", 42, "completed", "failure")["head_sha"] = walkSHA(9)
		}},
		{"check without an App", func(f *walkFixture) { f.run(walkSHA(1), 1, "ci-required", 42, "completed", "failure")["app"] = nil }},
		{"ambiguous latest run", func(f *walkFixture) {
			f.run(walkSHA(1), 1, "ci-required", 42, "completed", "failure")
			f.run(walkSHA(1), 2, "ci-required", 42, "completed", "success")
		}},
		{"another check under the requested name", func(f *walkFixture) {
			f.hub.json("/repos/acme/widget/commits/"+walkSHA(1)+"/check-runs", 200, queueJSON(map[string]any{"total_count": 1, "check_runs": []any{queueRun(1, "lint", 42, "completed", "failure")}}))
		}},
		{"truncated listing", func(f *walkFixture) {
			f.hub.json("/repos/acme/widget/commits/"+walkSHA(1)+"/check-runs", 200, queueJSON(map[string]any{"total_count": 2, "check_runs": []any{}}))
		}},
		{"history that does not start at the head", func(f *walkFixture) {
			f.history = func(nodes []any) []any { return nodes[1:] }
		}},
		{"history without parent counts", func(f *walkFixture) {
			f.history = func(nodes []any) []any {
				delete(nodes[0].(map[string]any)["parents"].(map[string]any), "totalCount")
				return nodes
			}
		}},
		{"history repeating a commit", func(f *walkFixture) {
			f.history = func(nodes []any) []any { return append(nodes, nodes[1]) }
		}},
		{"no required checks", func(f *walkFixture) { f.rules = []any{} }},
		{"required check bound to no App", func(f *walkFixture) { f.rules = queueRules("ci-required", 0) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWalkFixture(t)
			f.linear(3)
			f.run(walkSHA(2), 20, "ci-required", 42, "completed", "success")
			tc.alter(f)
			if got, err := f.walk(20); err == nil {
				t.Fatalf("the walk trusted what it could not read: %+v", got)
			}
		})
	}
}

func TestReleaseSourceWalkRefusesAnUnboundRequest(t *testing.T) {
	f := newWalkFixture(t)
	f.linear(2)
	for _, lookBack := range []int{0, -1, releaseSourceHistoryWindow + 1} {
		if _, err := f.walk(lookBack); err == nil {
			t.Fatalf("look back %d accepted", lookBack)
		}
	}
	if _, err := f.client.WalkReleaseSource(context.Background(), installToken, "acme", "widget/x", "main", 20); err == nil {
		t.Fatal("a nested repository accepted")
	}
	if _, err := f.client.WalkReleaseSource(context.Background(), "", "acme", "widget", "main", 20); err == nil {
		t.Fatal("an unauthenticated walk accepted")
	}
	if len(f.hub.seen()) != 0 {
		t.Fatal("a refused walk called GitHub")
	}
}

// The walk's green and the proof's acceptance are one verdict over the same
// bound runs, so the walk never offers a commit the proof would refuse for its
// checks, nor skips one the proof would accept.
func TestReleaseCheckStateAgreesWithTheProofsVerdict(t *testing.T) {
	ci, security := queueRequirement{Context: "ci-required", AppID: 42}, queueRequirement{Context: "security", AppID: 43}
	required := []queueRequirement{ci, security}
	run := func(status, conclusion string) queueCheckRun {
		return queueCheckRun{ID: 1, Name: "x", HeadSHA: headSHA, Status: status, Conclusion: conclusion, App: &struct{ ID int64 }{42}}
	}
	for _, tc := range []struct {
		name  string
		runs  map[queueRequirement]queueCheckRun
		state string
	}{
		{"both passed", map[queueRequirement]queueCheckRun{ci: run("completed", "success"), security: run("completed", "success")}, ""},
		{"none reported", map[queueRequirement]queueCheckRun{}, ReleaseSourceMissing},
		{"one reported", map[queueRequirement]queueCheckRun{ci: run("completed", "success")}, ReleaseSourceMissing},
		{"one running", map[queueRequirement]queueCheckRun{ci: run("completed", "success"), security: run("in_progress", "")}, ReleaseSourcePending},
		{"one cancelled", map[queueRequirement]queueCheckRun{ci: run("completed", "cancelled"), security: run("in_progress", "")}, ReleaseSourceCancelled},
		{"one failed", map[queueRequirement]queueCheckRun{ci: run("completed", "cancelled"), security: run("completed", "timed_out")}, ReleaseSourceFailed},
		{"skipped is not a pass", map[queueRequirement]queueCheckRun{ci: run("completed", "skipped"), security: run("completed", "success")}, ReleaseSourceFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, absent, err := releaseCheckVerdict(headSHA, required, tc.runs)
			passed := err == nil && !absent
			state, check, _ := releaseCheckState(required, tc.runs)
			if passed != (state == "") || state != tc.state {
				t.Fatalf("verdict passed=%v absent=%v err=%v, state %q (want %q)", passed, absent, err, state, tc.state)
			}
			if state != "" && check == "" {
				t.Fatal("a skipped commit names no check")
			}
		})
	}
}
