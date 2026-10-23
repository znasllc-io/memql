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

type groupFixture struct {
	client     *Client
	hub        *fakeHub
	repository map[string]any
	entries    []map[string]any
	commits    map[string]map[string]any
	reads      int
	beforeRead func(int)
}

func groupSHA(c string) string { return strings.Repeat(c, 40) }
func groupRef() string         { return "gh-readonly-queue/main/opaque-provider-ref" }
func newGroupFixture(t *testing.T) *groupFixture {
	t.Helper()
	f := &groupFixture{hub: newHub(), commits: map[string]map[string]any{}}
	r := map[string]any{"id": "R_widget", "databaseId": 123, "nameWithOwner": "acme/widget"}
	f.repository = map[string]any{"id": "R_widget", "databaseId": 123, "nameWithOwner": "acme/widget", "defaultBranchRef": map[string]any{"name": "main", "target": map[string]string{"oid": groupSHA("a")}}, "ref": map[string]any{"target": map[string]string{"oid": groupSHA("e")}}}
	for n, pair := range [][3]string{{"b", "c", "a"}, {"d", "e", "c"}} {
		pr := map[string]any{"number": n + 7, "headRefOid": groupSHA(pair[0]), "baseRefName": "main", "state": "OPEN", "isDraft": false, "isInMergeQueue": true, "repository": r, "headRepository": r, "baseRepository": r}
		f.entries = append(f.entries, map[string]any{"id": "entry-" + pair[0], "headCommit": map[string]string{"oid": groupSHA(pair[1])}, "baseCommit": map[string]string{"oid": groupSHA(pair[2])}, "pullRequest": pr})
		f.commits[groupSHA(pair[1])] = map[string]any{"sha": groupSHA(pair[1]), "parents": []map[string]string{{"sha": groupSHA(pair[2])}, {"sha": groupSHA(pair[0])}}}
	}
	f.hub.on("/graphql", func(req *http.Request) (int, string) {
		if req.Method != http.MethodPost {
			t.Fatal("unexpected mutation")
		}
		f.reads++
		if f.beforeRead != nil {
			f.beforeRead(f.reads)
		}
		if _, ok := f.repository["mergeQueue"]; !ok {
			f.repository["mergeQueue"] = map[string]any{"configuration": map[string]string{"mergeMethod": "MERGE"}}
		}
		queue := f.repository["mergeQueue"].(map[string]any)
		if _, ok := queue["entries"]; !ok {
			queue["entries"] = map[string]any{"totalCount": len(f.entries), "nodes": f.entries, "pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil}}
		}
		return 200, queueJSON(map[string]any{"data": map[string]any{"repository": f.repository}})
	})
	for sha := range f.commits {
		f.hub.on("/repos/acme/widget/git/commits/"+sha, func(req *http.Request) (int, string) {
			if req.Method != http.MethodGet {
				t.Fatal("commit observation mutated provider")
			}
			return 200, queueJSON(f.commits[sha])
		})
	}
	now := time.Now()
	f.client = testClient(t, f.hub, &now)
	return f
}
func (f *groupFixture) observe() (MergeGroupEvidence, error) {
	return f.client.ObserveMergeGroup(context.Background(), "read-token", "acme", "widget", groupSHA("e"), groupSHA("a"), groupRef())
}

func TestMergeGroupProvesEveryImmutableParentWithActualQueueMembership(t *testing.T) {
	f := newGroupFixture(t)
	evidence, err := f.observe()
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence.Members) != 2 || evidence.RepositoryID != 123 || evidence.HeadSHA != groupSHA("e") || evidence.BaseSHA != groupSHA("a") || evidence.Members[0].Number != 7 || evidence.Members[1].Number != 8 || evidence.Members[0].HeadSHA != groupSHA("b") || evidence.Members[1].HeadSHA != groupSHA("d") || f.reads != 2 {
		t.Fatalf("incomplete group evidence: %+v reads=%d", evidence, f.reads)
	}
	// Ref deliberately carries no PR number: only native graph and queue facts
	// may determine which changes would be covered by the group check.
}

func TestMergeGroupReadsMembersBeyondTheFirstQueuePage(t *testing.T) {
	f := newGroupFixture(t)
	first := []map[string]any{f.entries[0]}
	for n := range 99 {
		pr := map[string]any{}
		for key, value := range f.entries[0]["pullRequest"].(map[string]any) {
			pr[key] = value
		}
		pr["number"], pr["headRefOid"] = 1000+n, fmt.Sprintf("%040x", 1000+n)
		first = append(first, map[string]any{"id": fmt.Sprintf("waiting-%d", n), "headCommit": nil, "baseCommit": nil, "pullRequest": pr})
	}
	last := []map[string]any{f.entries[1]}
	f.beforeRead = func(n int) {
		bodies := f.hub.sentBodies()
		var body struct{ Variables map[string]any }
		if err := json.Unmarshal([]byte(bodies[len(bodies)-1]), &body); err != nil {
			t.Fatal(err)
		}
		nodes, more := first, true
		var cursor any = "after-first-page"
		if n%2 == 0 {
			if body.Variables["cursor"] != "after-first-page" {
				t.Fatal("queue continuation lost its cursor")
			}
			nodes, more, cursor = last, false, nil
		} else if body.Variables["cursor"] != nil {
			t.Fatal("fresh observation reused an old cursor")
		}
		f.repository["mergeQueue"] = map[string]any{"configuration": map[string]string{"mergeMethod": "MERGE"}, "entries": map[string]any{"totalCount": 101, "nodes": nodes, "pageInfo": map[string]any{"hasNextPage": more, "endCursor": cursor}}}
	}
	evidence, err := f.observe()
	if err != nil || len(evidence.Members) != 2 || f.reads != 4 {
		t.Fatalf("paginated proof: members=%d reads=%d error=%v", len(evidence.Members), f.reads, err)
	}
}

func TestMergeGroupRefusesIncompleteStaleOrAmbiguousProof(t *testing.T) {
	cases := map[string]func(*groupFixture){
		"unreviewed extra parent": func(f *groupFixture) {
			f.commits[groupSHA("e")]["parents"] = []map[string]string{{"sha": groupSHA("c")}, {"sha": groupSHA("d")}, {"sha": groupSHA("f")}}
		},
		"different change commit":    func(f *groupFixture) { f.entries[1]["pullRequest"].(map[string]any)["headRefOid"] = groupSHA("f") },
		"missing intermediate entry": func(f *groupFixture) { f.entries = f.entries[1:] },
		"group ref moved": func(f *groupFixture) {
			f.repository["ref"] = map[string]any{"target": map[string]string{"oid": groupSHA("f")}}
		},
		"base moved": func(f *groupFixture) {
			f.repository["defaultBranchRef"] = map[string]any{"name": "main", "target": map[string]string{"oid": groupSHA("f")}}
		},
		"removed from queue": func(f *groupFixture) { f.entries[1]["pullRequest"].(map[string]any)["isInMergeQueue"] = false },
		"fork repository": func(f *groupFixture) {
			f.entries[1]["pullRequest"].(map[string]any)["headRepository"] = map[string]any{"id": "R_other", "databaseId": 999, "nameWithOwner": "other/widget"}
		},
		"duplicate synthetic commit": func(f *groupFixture) { f.entries[1]["headCommit"] = f.entries[0]["headCommit"] },
		"unprepared group head":      func(f *groupFixture) { f.entries[1]["headCommit"] = nil },
		"unmatched first parent":     func(f *groupFixture) { f.entries[1]["baseCommit"] = map[string]string{"oid": groupSHA("a")} },
		"unsupported squash queue": func(f *groupFixture) {
			f.repository["mergeQueue"] = map[string]any{"configuration": map[string]string{"mergeMethod": "SQUASH"}}
		},
		"truncated queue page": func(f *groupFixture) {
			f.repository["mergeQueue"] = map[string]any{"configuration": map[string]string{"mergeMethod": "MERGE"}, "entries": map[string]any{"totalCount": 3, "nodes": f.entries, "pageInfo": map[string]any{"hasNextPage": false}}}
		},
		"queue changed during read": func(f *groupFixture) {
			f.beforeRead = func(n int) {
				if n == 2 {
					f.entries[1]["id"] = "replacement-entry"
				}
			}
		},
		"commit parent differs from requested SHA": func(f *groupFixture) { f.commits[groupSHA("e")]["sha"] = groupSHA("f") },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f := newGroupFixture(t)
			change(f)
			out, err := f.observe()
			if err == nil || len(out.Members) != 0 {
				t.Fatalf("incomplete proof was accepted: %+v error=%v", out, err)
			}
		})
	}
}

func TestMergeGroupReviewCheckRecoversOnlyExactAppOwnedEvidence(t *testing.T) {
	f := newGroupFixture(t)
	evidence, err := f.observe()
	if err != nil {
		t.Fatal(err)
	}
	external := "memql-review:" + strings.Repeat("a", 64)
	var recorded map[string]any
	writes, grants := 0, 0
	f.hub.on("/repos/acme/widget/commits/"+evidence.HeadSHA+"/check-runs", func(r *http.Request) (int, string) {
		if r.Method != http.MethodGet || r.URL.Query().Get("filter") != "all" {
			t.Fatal("unbound history read")
		}
		runs := []any{}
		if recorded != nil {
			runs = append(runs, recorded)
		}
		return 200, queueJSON(map[string]any{"total_count": len(runs), "check_runs": runs})
	})
	f.hub.on("/repos/acme/widget/check-runs", func(r *http.Request) (int, string) {
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer fresh-check-writer" {
			t.Fatal("wrong effect or authority")
		}
		bodies := f.hub.sentBodies()
		var body map[string]any
		if err := json.Unmarshal([]byte(bodies[len(bodies)-1]), &body); err != nil {
			t.Fatal(err)
		}
		if body["head_sha"] != evidence.HeadSHA || body["external_id"] != external || body["name"] != "MemQL / review" {
			t.Fatal("review check targeted different evidence")
		}
		writes++
		recorded = map[string]any{"id": 91, "name": "MemQL / review", "head_sha": evidence.HeadSHA, "external_id": external, "status": "completed", "conclusion": "success", "app": map[string]any{"id": 1234567}}
		return 503, `{"message":"lost reply"}`
	})
	f.hub.on("/repos/acme/widget/check-runs/91", func(*http.Request) (int, string) { return 200, queueJSON(recorded) })
	authorize := func(_ context.Context, got MergeGroupEvidence) (string, error) {
		grants++
		if got.Members[0].HeadSHA != groupSHA("b") || got.Members[1].HeadSHA != groupSHA("d") {
			t.Fatal("native authorization was not given every exact member")
		}
		return "fresh-check-writer", nil
	}
	for range 2 {
		id, err := f.client.ReportMergeGroupReviewCheck(context.Background(), "reader", "acme", "widget", evidence, "MemQL / review", external, 1234567, authorize)
		if err != nil || id != 91 {
			t.Fatalf("report: id=%d error=%v", id, err)
		}
	}
	if writes != 1 || grants != 3 {
		t.Fatalf("lost reply retried effect or recovery skipped authority: writes=%d grants=%d", writes, grants)
	}
	_, err = f.client.ReportMergeGroupReviewCheck(context.Background(), "reader", "acme", "widget", evidence, "MemQL / review", external, 1234567, func(context.Context, MergeGroupEvidence) (string, error) {
		return "", errors.New("member approval revoked")
	})
	if err == nil || writes != 1 {
		t.Fatal("existing provider success bypassed revoked native authority")
	}
	recorded["app"] = map[string]any{"id": 999}
	if err := f.client.VerifyReviewCheck(context.Background(), "reader", "acme", "widget", 91, evidence.HeadSHA, "MemQL / review", external, 1234567); err == nil {
		t.Fatal("another app satisfied native review evidence")
	}
}
