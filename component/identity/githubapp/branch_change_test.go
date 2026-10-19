package githubapp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func branchFixture(t *testing.T) (*Client, *fakeHub, BranchChange, *string) {
	t.Helper()
	now := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	oldTree, newTree, newCommit, blob := strings.Repeat("b", 40), strings.Repeat("c", 40), strings.Repeat("d", 40), strings.Repeat("e", 40)
	digest := sha256.Sum256([]byte("old\n"))
	current := headSHA
	edit := BranchChange{Branch: "dependabot/go", ExpectedHead: headSHA, OperationID: strings.Repeat("a", 64), Title: "Tidy module", Timestamp: now, Files: []BranchReplacement{{"go.mod", hex.EncodeToString(digest[:]), "new\n"}}, Authorize: func(context.Context, string) (string, error) { return installToken, nil }}
	hub := newHub().json("/repos/acme/widget/git/commits/"+headSHA, 200, `{"tree":{"sha":"`+oldTree+`"}}`).json("/repos/acme/widget/git/trees/"+oldTree, 200, `{"tree":[{"path":"go.mod","mode":"100644","type":"blob","sha":"`+blob+`"}]}`).json("/repos/acme/widget/git/blobs/"+blob, 200, `{"encoding":"base64","size":4,"content":"`+base64.StdEncoding.EncodeToString([]byte("old\n"))+`"}`).json("/repos/acme/widget/git/trees", 201, `{"sha":"`+newTree+`"}`).json("/repos/acme/widget/git/commits", 201, `{"sha":"`+newCommit+`"}`)
	prepared, _ := json.Marshal(map[string]any{"tree": map[string]string{"sha": newTree}, "parents": []map[string]string{{"sha": headSHA}}, "message": edit.Title + "\n\nMemQL-Operation: " + edit.OperationID})
	hub.json("/repos/acme/widget/git/commits/"+newCommit, 200, string(prepared))
	hub.on("/repos/acme/widget/branches/"+edit.Branch, func(*http.Request) (int, string) {
		if current == "" {
			return 404, `{}`
		}
		return 200, fmt.Sprintf(`{"commit":{"sha":%q}}`, current)
	})
	hub.on("/repos/acme/widget/git/refs/heads/"+edit.Branch, func(*http.Request) (int, string) { current = newCommit; return 200, `{}` })
	return testClient(t, hub, &now), hub, edit, &current
}

func TestBranchChangeRecoveryUsesSameCommitAndNeverRepeatsMutableWrite(t *testing.T) {
	c, h, e, current := branchFixture(t)
	for range 2 {
		got, err := c.ApplyBranchChange(context.Background(), installToken, "acme", "widget", e)
		if err != nil || got != *current {
			t.Fatalf("%q %v", got, err)
		}
	}
	if h.hits("/repos/acme/widget/git/refs/heads/"+e.Branch) != 1 {
		t.Fatal("recovery repeated branch update")
	}
	var commits []string
	for n, r := range h.seen() {
		b := h.sentBodies()[n]
		if r.URL.Path == "/repos/acme/widget/git/commits" {
			commits = append(commits, b)
		}
		if r.Method == http.MethodPatch && !strings.Contains(b, `"force":false`) {
			t.Fatal("force push")
		}
	}
	if len(commits) != 2 || commits[0] != commits[1] {
		t.Fatal("commit construction is not deterministic")
	}
}

func TestBranchChangeUncertainResponseRequiresActualReadBack(t *testing.T) {
	for _, landed := range []bool{false, true} {
		t.Run(fmt.Sprint(landed), func(t *testing.T) {
			c, h, e, current := branchFixture(t)
			h.on("/repos/acme/widget/git/refs/heads/"+e.Branch, func(*http.Request) (int, string) {
				if landed {
					*current = strings.Repeat("d", 40)
				}
				return 502, `{}`
			})
			_, err := c.ApplyBranchChange(context.Background(), installToken, "acme", "widget", e)
			if (err == nil) != landed {
				t.Fatalf("landed=%v err=%v", landed, err)
			}
		})
	}
}

func TestBranchChangeRejectsMovedSourceRevocationAndUnsafeTrees(t *testing.T) {
	for _, kind := range []string{"moved", "revoked", "bad hash", "symlink", "truncated", "wrong prepared parent", "invalid branch", "invalid utf8"} {
		t.Run(kind, func(t *testing.T) {
			c, h, e, current := branchFixture(t)
			switch kind {
			case "moved":
				*current = strings.Repeat("f", 40)
			case "revoked":
				e.Authorize = func(context.Context, string) (string, error) { return "", errors.New("revoked") }
			case "bad hash":
				e.Files[0].BeforeSHA256 = strings.Repeat("0", 64)
			case "symlink":
				h.json("/repos/acme/widget/git/trees/"+strings.Repeat("b", 40), 200, `{"tree":[{"path":"go.mod","mode":"120000","type":"blob","sha":"`+strings.Repeat("e", 40)+`"}]}`)
			case "truncated":
				h.json("/repos/acme/widget/git/trees/"+strings.Repeat("b", 40), 200, `{"truncated":true}`)
			case "wrong prepared parent":
				h.json("/repos/acme/widget/git/commits/"+strings.Repeat("d", 40), 200, `{"tree":{"sha":"`+strings.Repeat("c", 40)+`"},"parents":[]}`)
			case "invalid branch":
				e.Branch = "branch@{oops"
			case "invalid utf8":
				e.Files[0].Content = string([]byte{255})
			}
			if _, err := c.ApplyBranchChange(context.Background(), installToken, "acme", "widget", e); err == nil {
				t.Fatal("unsafe change admitted")
			}
			for _, r := range h.seen() {
				if strings.Contains(r.URL.Path, "/git/refs") {
					t.Fatal("unsafe mutable write")
				}
			}
		})
	}
}

func TestBranchChangeNewProposalNeverReplacesExistingBranch(t *testing.T) {
	c, h, e, current := branchFixture(t)
	e.Create = true
	if _, err := c.ApplyBranchChange(context.Background(), installToken, "acme", "widget", e); err == nil {
		t.Fatal("overwrote existing proposal")
	}
	*current = ""
	h.on("/repos/acme/widget/git/refs", func(*http.Request) (int, string) { *current = strings.Repeat("d", 40); return 201, `{}` })
	if _, err := c.ApplyBranchChange(context.Background(), installToken, "acme", "widget", e); err != nil {
		t.Fatal(err)
	}
}

func TestChangePullRequestRecoversLostReplyAndDoesNotReopenClosedProposal(t *testing.T) {
	for _, closed := range []bool{false, true} {
		t.Run(fmt.Sprint(closed), func(t *testing.T) {
			c, h, _, _ := branchFixture(t)
			exists := closed
			posts := 0
			h.on("/repos/acme/widget/pulls", func(r *http.Request) (int, string) {
				if r.Method == http.MethodPost {
					posts++
					exists = true
					return 502, `{}`
				}
				if !exists {
					return 200, `[]`
				}
				state := "open"
				if closed {
					state = "closed"
				}
				return 200, fmt.Sprintf(`[{"state":%q,"html_url":"https://github.com/acme/widget/pull/7"}]`, state)
			})
			for range 2 {
				_, err := c.EnsureChangePullRequest(context.Background(), installToken, "acme", "widget", "maintenance/update", "main", "Update", "Body")
				if (err != nil) != closed {
					t.Fatal(err)
				}
			}
			if closed && posts != 0 || !closed && posts != 1 {
				t.Fatal("repeated or reopened proposal", posts)
			}
		})
	}
}
