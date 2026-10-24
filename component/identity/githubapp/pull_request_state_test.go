package githubapp

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestPullRequestStateRequiresActualMergeEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		merged bool
		state  string
		want   string
	}{
		{"open prospective commit", false, "open", ""},
		{"closed without merge", false, "closed", ""},
		{"merged", true, "closed", headSHA},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := pullRequestObservationFixture()
			p["merged"] = tc.merged
			p["state"] = tc.state
			body, _ := json.Marshal(p)
			now := time.Now().UTC()
			hub := newHub().json("/repos/acme/widget/pulls/7", http.StatusOK, string(body))
			got, err := testClient(t, hub, &now).PullRequestState(context.Background(), installToken, "acme", "widget", 7)
			if err != nil {
				t.Fatal(err)
			}
			if got.Number != 7 || got.Merged != tc.merged || got.MergeCommitSHA != tc.want || got.BaseRepository != "acme/widget" || got.HeadRepository != "acme/widget" {
				t.Fatalf("unexpected observation: %+v", got)
			}
			if hub.seen()[0].Header.Get("Authorization") != "Bearer "+installToken {
				t.Fatal("lost caller credential")
			}
		})
	}
}

func TestPullRequestStateRejectsIncompleteOrContradictoryEvidence(t *testing.T) {
	for name, change := range map[string]func(map[string]any){
		"wrong number":         func(p map[string]any) { p["number"] = 8 },
		"missing merged":       func(p map[string]any) { delete(p, "merged") },
		"unknown state":        func(p map[string]any) { p["state"] = "pending" },
		"open claimed merge":   func(p map[string]any) { p["state"] = "open" },
		"missing merge commit": func(p map[string]any) { p["merge_commit_sha"] = "" },
		"invalid merge commit": func(p map[string]any) { p["merge_commit_sha"] = "not-a-commit" },
		"invalid head":         func(p map[string]any) { p["head"].(map[string]any)["sha"] = "head" },
		"wrong base repository": func(p map[string]any) {
			p["base"].(map[string]any)["repo"] = map[string]any{"full_name": "other/widget"}
		},
		"missing base repository": func(p map[string]any) { p["base"].(map[string]any)["repo"] = nil },
	} {
		t.Run(name, func(t *testing.T) {
			p := pullRequestObservationFixture()
			change(p)
			body, _ := json.Marshal(p)
			now := time.Now().UTC()
			hub := newHub().json("/repos/acme/widget/pulls/7", http.StatusOK, string(body))
			if _, err := testClient(t, hub, &now).PullRequestState(context.Background(), installToken, "acme", "widget", 7); err == nil {
				t.Fatal("accepted incomplete merge evidence")
			}
		})
	}
}

func TestPullRequestStateKeepsDeletedHeadExplicit(t *testing.T) {
	p := pullRequestObservationFixture()
	p["head"].(map[string]any)["repo"] = nil
	body, _ := json.Marshal(p)
	now := time.Now().UTC()
	hub := newHub().json("/repos/acme/widget/pulls/7", http.StatusOK, string(body))
	c := testClient(t, hub, &now)
	got, err := c.PullRequestState(context.Background(), installToken, "acme", "widget", 7)
	if err != nil || got.HeadRepository != "" {
		t.Fatalf("deleted head: %+v %v", got, err)
	}
	if _, err = c.PullRequestState(context.Background(), installToken, "acme", "widget", 0); err == nil {
		t.Fatal("accepted invalid number")
	}
	if len(hub.seen()) != 1 {
		t.Fatal("invalid number sent a provider request")
	}
}

func pullRequestObservationFixture() map[string]any {
	return map[string]any{"number": 7, "state": "closed", "merged": true, "merge_commit_sha": headSHA, "html_url": "https://github.com/acme/widget/pull/7",
		"head": map[string]any{"sha": headSHA, "ref": "release/pin", "repo": map[string]any{"full_name": "acme/widget"}},
		"base": map[string]any{"sha": headSHA, "ref": "main", "repo": map[string]any{"full_name": "acme/widget"}}}
}
