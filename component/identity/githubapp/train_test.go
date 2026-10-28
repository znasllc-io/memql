package githubapp

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func trainTestClient(t *testing.T, hub *fakeHub) *Client {
	t.Helper()
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	return testClient(t, hub, &now)
}

func TestTrainPullRequestsDecodesDraftLabelsAndRefs(t *testing.T) {
	hub := newHub().on("/repos/acme/widget/pulls", func(r *http.Request) (int, string) {
		q := r.URL.Query()
		if q.Get("state") != "open" || q.Get("base") != "main" || q.Get("per_page") != "100" {
			return http.StatusBadRequest, `{"message":"unexpected query ` + r.URL.RawQuery + `"}`
		}
		return http.StatusOK, `[{"number":7,"title":"Fix the badge","draft":true,"html_url":"https://github.com/acme/widget/pull/7",
			"user":{"login":"znas-io"},"head":{"sha":"aaa111","ref":"claude/badge","repo":{"full_name":"acme/widget"}},"base":{"ref":"main"},
			"labels":[{"name":"train:hold"},{"name":"bug"}]},
			{"number":8,"title":"From a deleted fork","html_url":"https://github.com/acme/widget/pull/8",
			"user":{"login":"mallory"},"head":{"sha":"bbb222","ref":"patch-1","repo":null},"base":{"ref":"main"},"labels":[]}]`
	})
	got, err := trainTestClient(t, hub).TrainPullRequests(context.Background(), "tok", "acme", "widget", "main")
	if err != nil {
		t.Fatalf("TrainPullRequests: %v", err)
	}
	want := TrainPullRequest{Number: 7, Title: "Fix the badge", HeadSHA: "aaa111", HeadRef: "claude/badge", BaseRef: "main",
		Author: "znas-io", URL: "https://github.com/acme/widget/pull/7", Draft: true, Labels: []string{"train:hold", "bug"}}
	if len(got) != 2 || got[0].Number != want.Number || got[0].HeadSHA != want.HeadSHA || got[0].HeadRef != want.HeadRef ||
		got[0].BaseRef != want.BaseRef || got[0].Author != want.Author || got[0].URL != want.URL || !got[0].Draft ||
		strings.Join(got[0].Labels, ",") != "train:hold,bug" {
		t.Errorf("TrainPullRequests = %+v, want [%+v ...]", got, want)
	}
	// The head's repository is what tells a fork apart; GitHub reports a
	// deleted fork's as null, which must read as no repository, never as ours.
	if len(got) == 2 && (got[0].HeadRepository != "acme/widget" || got[1].HeadRepository != "") {
		t.Errorf("head repositories = %q, %q; want acme/widget and empty", got[0].HeadRepository, got[1].HeadRepository)
	}
}

func TestMergeIntoBranchOutcomes(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   MergeOutcome
		err    bool
	}{
		{"merged", http.StatusCreated, `{"sha":"merge123"}`, MergeOutcome{SHA: "merge123"}, false},
		{"already in the branch", http.StatusNoContent, ``, MergeOutcome{NothingToMerge: true}, false},
		{"a conflict", http.StatusConflict, `{"message":"Merge conflict"}`, MergeOutcome{Conflict: true}, false},
		{"a refusal", http.StatusUnprocessableEntity, `{"message":"Validation Failed"}`, MergeOutcome{}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hub := newHub().json("/repos/acme/widget/merges", c.status, c.body)
			got, err := trainTestClient(t, hub).MergeIntoBranch(context.Background(), "tok", "acme", "widget", "train/2026-10-28-1", "aaa111", "Board #7")
			if (err != nil) != c.err {
				t.Fatalf("MergeIntoBranch error = %v, want error %v", err, c.err)
			}
			if got != c.want {
				t.Errorf("MergeIntoBranch = %+v, want %+v", got, c.want)
			}
			if c.status == http.StatusCreated {
				var sent map[string]string
				_ = json.Unmarshal([]byte(hub.bodies[hub.seen()[0]]), &sent)
				if sent["base"] != "train/2026-10-28-1" || sent["head"] != "aaa111" || sent["commit_message"] != "Board #7" {
					t.Errorf("sent %v, want base, head SHA and message", sent)
				}
			}
		})
	}
}

func TestMergePullRequestSendsTheHeadGuard(t *testing.T) {
	hub := newHub().on("/repos/acme/widget/pulls/41/merge", func(r *http.Request) (int, string) {
		if r.Method != http.MethodPut {
			return http.StatusMethodNotAllowed, `{}`
		}
		return http.StatusOK, `{"sha":"landed9","merged":true}`
	})
	sha, err := trainTestClient(t, hub).MergePullRequest(context.Background(), "tok", "acme", "widget", 41, "head42")
	if err != nil || sha != "landed9" {
		t.Fatalf("MergePullRequest = %q, %v; want landed9", sha, err)
	}
	var sent map[string]string
	_ = json.Unmarshal([]byte(hub.bodies[hub.seen()[0]]), &sent)
	if sent["sha"] != "head42" || sent["merge_method"] != "merge" {
		t.Errorf("sent %v, want sha head42 and merge_method merge", sent)
	}

	moved := newHub().json("/repos/acme/widget/pulls/41/merge", http.StatusConflict, `{"message":"Head branch was modified"}`)
	if _, err := trainTestClient(t, moved).MergePullRequest(context.Background(), "tok", "acme", "widget", 41, "head42"); StatusOf(err) != http.StatusConflict {
		t.Errorf("a moved head answered %v, want the 409 as a StatusError", err)
	}
}

func TestPullRequestFilesStopsAtThreePages(t *testing.T) {
	page := func(n, start int) string {
		var files []string
		for i := 0; i < n; i++ {
			files = append(files, `{"filename":"f`+strconv.Itoa(start+i)+`.go"}`)
		}
		return "[" + strings.Join(files, ",") + "]"
	}
	many := newHub().on("/repos/acme/widget/pulls/7/files", func(r *http.Request) (int, string) {
		p, _ := strconv.Atoi(r.URL.Query().Get("page"))
		return http.StatusOK, page(100, (p-1)*100)
	})
	files, complete, err := trainTestClient(t, many).PullRequestFiles(context.Background(), "tok", "acme", "widget", 7)
	if err != nil || len(files) != 300 || complete {
		t.Errorf("a long pull request: %d files, complete %v, err %v; want 300, false", len(files), complete, err)
	}
	if n := len(many.seen()); n != 3 {
		t.Errorf("asked for %d pages, want 3", n)
	}

	few := newHub().on("/repos/acme/widget/pulls/7/files", func(r *http.Request) (int, string) {
		if r.URL.Query().Get("page") == "1" {
			return http.StatusOK, page(2, 0)
		}
		return http.StatusOK, `[]`
	})
	files, complete, err = trainTestClient(t, few).PullRequestFiles(context.Background(), "tok", "acme", "widget", 7)
	if err != nil || len(files) != 2 || !complete || files[0] != "f0.go" {
		t.Errorf("a short pull request: %v, complete %v, err %v; want [f0.go f1.go], true", files, complete, err)
	}
}

func TestCheckRunConclusionPicksTheNewest(t *testing.T) {
	hub := newHub().on("/repos/acme/widget/commits/aaa111/check-runs", func(r *http.Request) (int, string) {
		if r.URL.Query().Get("check_name") != "MemQL / memql" {
			return http.StatusBadRequest, `{}`
		}
		return http.StatusOK, `{"check_runs":[
			{"name":"MemQL / memql","status":"completed","conclusion":"failure","started_at":"2026-10-28T10:00:00Z","app":{"id":4822538}},
			{"name":"MemQL / memql","status":"completed","conclusion":"success","started_at":"2026-10-28T11:00:00Z","app":{"id":4822538}},
			{"name":"MemQL / memql","status":"completed","conclusion":"success","started_at":"2026-10-28T12:00:00Z","app":{"id":999}}]}`
	})
	status, conclusion, err := trainTestClient(t, hub).CheckRunConclusion(context.Background(), "tok", "acme", "widget", "aaa111", "MemQL / memql", 4822538)
	if err != nil || status != "completed" || conclusion != "success" {
		t.Errorf("CheckRunConclusion = %q, %q, %v; want the app's newest: completed, success", status, conclusion, err)
	}
	none := newHub().json("/repos/acme/widget/commits/aaa111/check-runs", http.StatusOK, `{"check_runs":[]}`)
	status, conclusion, err = trainTestClient(t, none).CheckRunConclusion(context.Background(), "tok", "acme", "widget", "aaa111", "MemQL / memql", 4822538)
	if err != nil || status != "" || conclusion != "" {
		t.Errorf("no check run: %q, %q, %v; want empty and no error", status, conclusion, err)
	}
}

func TestCreatePullRequestIsNeverADraft(t *testing.T) {
	hub := newHub().json("/repos/acme/widget/pulls", http.StatusCreated, `{"number":42,"html_url":"https://github.com/acme/widget/pull/42"}`)
	n, url, err := trainTestClient(t, hub).CreatePullRequest(context.Background(), "tok", "acme", "widget", "train/2026-10-28-1", "main", "Train 2026-10-28-1", "body")
	if err != nil || n != 42 || url != "https://github.com/acme/widget/pull/42" {
		t.Fatalf("CreatePullRequest = %d, %q, %v", n, url, err)
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte(hub.bodies[hub.seen()[0]]), &sent)
	if sent["draft"] != false || sent["head"] != "train/2026-10-28-1" || sent["base"] != "main" {
		t.Errorf("sent %v, want draft false with head and base", sent)
	}
}

func TestPutBranchFileSendsTheBlobItReplaces(t *testing.T) {
	hub := newHub().on("/repos/acme/widget/contents/VERSION", func(r *http.Request) (int, string) {
		switch r.Method {
		case http.MethodGet:
			if r.URL.Query().Get("ref") != "train/2026-10-28-1" {
				return http.StatusNotFound, `{}`
			}
			return http.StatusOK, `{"sha":"blob1"}`
		case http.MethodPut:
			return http.StatusOK, `{"commit":{"sha":"commit2"}}`
		}
		return http.StatusMethodNotAllowed, `{}`
	})
	sha, err := trainTestClient(t, hub).PutBranchFile(context.Background(), "tok", "acme", "widget", "train/2026-10-28-1", "VERSION", "Prepare 0.31.5", []byte("0.31.5\n"))
	if err != nil || sha != "commit2" {
		t.Fatalf("PutBranchFile = %q, %v; want commit2", sha, err)
	}
	reqs := hub.seen()
	var sent map[string]string
	_ = json.Unmarshal([]byte(hub.bodies[reqs[len(reqs)-1]]), &sent)
	if sent["sha"] != "blob1" || sent["branch"] != "train/2026-10-28-1" || sent["content"] != "MC4zMS41Cg==" {
		t.Errorf("sent %v, want the replaced blob, the branch and base64 content", sent)
	}
}

func TestRefsReviewsCommentsAndLabels(t *testing.T) {
	hub := newHub().
		json("/repos/acme/widget/git/refs", http.StatusCreated, `{"ref":"refs/heads/train/2026-10-28-1"}`).
		json("/repos/acme/widget/git/refs/heads/train/2026-10-28-1", http.StatusOK, `{"ref":"refs/heads/train/2026-10-28-1"}`).
		json("/repos/acme/widget/pulls/41/reviews", http.StatusOK, `[{"user":{"login":"znas-io"},"state":"APPROVED","commit_id":"head42","submitted_at":"2026-10-28T12:00:00Z"}]`).
		json("/repos/acme/widget/issues/7/comments", http.StatusCreated, `{"id":1}`).
		json("/repos/acme/widget/issues/7/labels", http.StatusOK, `[{"name":"train:dropped"}]`).
		json("/repos/acme/widget/pulls/41", http.StatusOK, `{"state":"closed","merged":true}`)
	c := trainTestClient(t, hub)
	ctx := context.Background()
	if err := c.CreateBranchRef(ctx, "tok", "acme", "widget", "train/2026-10-28-1", "base1"); err != nil {
		t.Errorf("CreateBranchRef: %v", err)
	}
	if err := c.ResetBranchRef(ctx, "tok", "acme", "widget", "train/2026-10-28-1", "base2"); err != nil {
		t.Errorf("ResetBranchRef: %v", err)
	}
	reviews, err := c.PullRequestReviews(ctx, "tok", "acme", "widget", 41)
	if err != nil || len(reviews) != 1 || reviews[0].Login != "znas-io" || reviews[0].State != "APPROVED" || reviews[0].CommitID != "head42" || reviews[0].SubmittedAt.IsZero() {
		t.Errorf("PullRequestReviews = %+v, %v", reviews, err)
	}
	if err := c.CommentOnPullRequest(ctx, "tok", "acme", "widget", 7, "Dropped"); err != nil {
		t.Errorf("CommentOnPullRequest: %v", err)
	}
	if err := c.AddPullRequestLabels(ctx, "tok", "acme", "widget", 7, []string{"train:dropped"}); err != nil {
		t.Errorf("AddPullRequestLabels: %v", err)
	}
	open, merged, err := c.PullRequestOpen(ctx, "tok", "acme", "widget", 41)
	if err != nil || open || !merged {
		t.Errorf("PullRequestOpen = %v, %v, %v; want closed and merged", open, merged, err)
	}
	var reset map[string]any
	for _, r := range hub.seen() {
		if r.Method == http.MethodPatch {
			_ = json.Unmarshal([]byte(hub.bodies[r]), &reset)
		}
	}
	if reset["sha"] != "base2" || reset["force"] != true {
		t.Errorf("ResetBranchRef sent %v, want the new SHA with force", reset)
	}
	if err := c.CreateBranchRef(ctx, "tok", "acme", "widget", "train/../main", "x"); err == nil {
		t.Errorf("CreateBranchRef accepted a branch name with ..")
	}
}
