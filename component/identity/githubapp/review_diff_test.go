package githubapp

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func reviewBlob(text string) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d%c", len(text), 0)
	h.Write([]byte(text))
	return hex.EncodeToString(h.Sum(nil))
}
func reviewEntry(p, mode, text string) map[string]any {
	return map[string]any{"path": p, "mode": mode, "type": "blob", "sha": reviewBlob(text)}
}
func reviewTreeFixture(h *fakeHub, commit, tree string, entries []any) {
	h.json("/repos/acme/widget/git/commits/"+commit, 200, queueJSON(map[string]any{"sha": commit, "tree": map[string]string{"sha": tree}}))
	h.json("/repos/acme/widget/git/trees/"+tree, 200, queueJSON(map[string]any{"sha": tree, "truncated": false, "tree": entries}))
}
func reviewBlobFixture(h *fakeHub, text string) {
	h.json("/repos/acme/widget/git/blobs/"+reviewBlob(text), 200, queueJSON(map[string]any{"sha": reviewBlob(text), "size": len(text), "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(text))}))
}
func TestImmutableReviewDiffRetainsCompleteChangesAndEmptyFiles(t *testing.T) {
	h := newHub()
	now := time.Now()
	before, after := strings.Repeat("1", 40), strings.Repeat("2", 40)
	reviewTreeFixture(h, before, strings.Repeat("3", 40), []any{reviewEntry("old.txt", "100644", "old"), reviewEntry("run.sh", "100644", "run"), reviewEntry("same", "100644", "untouched")})
	reviewTreeFixture(h, after, strings.Repeat("4", 40), []any{reviewEntry("new.txt", "100644", "old"), reviewEntry("run.sh", "100755", "run"), reviewEntry("empty", "100644", ""), reviewEntry("same", "100644", "untouched")})
	for _, text := range []string{"old", "run", ""} {
		reviewBlobFixture(h, text)
	}
	got, err := testClient(t, h, &now).ImmutableReviewDiff(context.Background(), installToken, "acme", "widget", before, after)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Files) != 4 || got.Files[0].Path != "empty" || got.Files[0].Before.Present || !got.Files[0].After.Present || got.Files[0].After.Content != "" || got.Files[1].Path != "new.txt" || got.Files[2].Path != "old.txt" || got.Files[2].After.Present || got.Files[3].Before.Mode != "100644" || got.Files[3].After.Mode != "100755" {
		t.Fatalf("incomplete review delta: %+v", got)
	}
	for _, r := range h.seen() {
		if strings.Contains(r.URL.Path, reviewBlob("untouched")) {
			t.Fatal("unchanged content fetched")
		}
	}
}
func TestImmutableReviewDiffRefusesIncompleteOrUnsupportedEvidence(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alter func(*fakeHub, string, string)
	}{
		{"truncated", func(h *fakeHub, tree, blob string) {
			h.json("/repos/acme/widget/git/trees/"+tree, 200, queueJSON(map[string]any{"sha": tree, "truncated": true, "tree": []any{}}))
		}},
		{"missing completeness", func(h *fakeHub, tree, blob string) {
			h.json("/repos/acme/widget/git/trees/"+tree, 200, queueJSON(map[string]any{"sha": tree, "tree": []any{}}))
		}},
		{"wrong tree", func(h *fakeHub, tree, blob string) {
			h.json("/repos/acme/widget/git/trees/"+tree, 200, queueJSON(map[string]any{"sha": headSHA, "truncated": false, "tree": []any{}}))
		}},
		{"link", func(h *fakeHub, tree, blob string) {
			reviewTreeFixture(h, strings.Repeat("2", 40), tree, []any{reviewEntry("x", "120000", "new")})
		}},
		{"duplicate", func(h *fakeHub, tree, blob string) {
			e := reviewEntry("x", "100644", "new")
			reviewTreeFixture(h, strings.Repeat("2", 40), tree, []any{e, e})
		}},
		{"path traversal", func(h *fakeHub, tree, blob string) {
			reviewTreeFixture(h, strings.Repeat("2", 40), tree, []any{reviewEntry("../x", "100644", "new")})
		}},
		{"binary", func(h *fakeHub, tree, blob string) {
			h.json("/repos/acme/widget/git/blobs/"+blob, 200, queueJSON(map[string]any{"sha": blob, "size": 3, "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte{'n', 0, 'w'})}))
		}},
		{"wrong bytes", func(h *fakeHub, tree, blob string) {
			h.json("/repos/acme/widget/git/blobs/"+blob, 200, queueJSON(map[string]any{"sha": blob, "size": 3, "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte("lie"))}))
		}},
		{"wrong blob", func(h *fakeHub, tree, blob string) {
			h.json("/repos/acme/widget/git/blobs/"+blob, 200, queueJSON(map[string]any{"sha": headSHA, "size": 3, "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte("new"))}))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHub()
			now := time.Now()
			before, after, tree := strings.Repeat("1", 40), strings.Repeat("2", 40), strings.Repeat("4", 40)
			reviewTreeFixture(h, before, strings.Repeat("3", 40), []any{})
			reviewTreeFixture(h, after, tree, []any{reviewEntry("x", "100644", "new")})
			reviewBlobFixture(h, "new")
			tc.alter(h, tree, reviewBlob("new"))
			if _, err := testClient(t, h, &now).ImmutableReviewDiff(context.Background(), installToken, "acme", "widget", before, after); err == nil {
				t.Fatal("unsafe evidence accepted")
			}
		})
	}
}
func TestReviewChangesAllowDisjointSyntheticParentsButRefuseResolution(t *testing.T) {
	file := ReviewFileChange{Path: "a", Before: ReviewFileVersion{Present: true, Mode: "100644", BlobSHA: reviewBlob("old"), Content: "old"}, After: ReviewFileVersion{Present: true, Mode: "100644", BlobSHA: reviewBlob("new"), Content: "new"}}
	original := ReviewDiff{BeforeSHA: strings.Repeat("1", 40), AfterSHA: strings.Repeat("2", 40), Files: []ReviewFileChange{file}}
	merged := ReviewDiff{BeforeSHA: strings.Repeat("3", 40), AfterSHA: strings.Repeat("4", 40), Files: []ReviewFileChange{file}}
	if !SameReviewChanges(original, merged) {
		t.Fatal("disjoint parent changes rejected")
	}
	merged.Files[0].Before.Content = "changed by sibling"
	if SameReviewChanges(original, merged) {
		t.Fatal("conflicting earlier member accepted")
	}
	merged.Files = []ReviewFileChange{file, file}
	if SameReviewChanges(original, merged) {
		t.Fatal("extra synthetic change accepted")
	}
}
func TestObserveReviewCheckNeverWritesAbsentOrUncertainIntent(t *testing.T) {
	f := newReviewCheckFixture(t)
	id, err := f.client.ObserveReviewCheck(context.Background(), installToken, "acme", "widget", headSHA, "MemQL / review", f.external(), 42)
	if err != nil || id != 0 || f.writes != 0 {
		t.Fatal(id, err, f.writes)
	}
	f.listFail = true
	if _, err = f.client.ObserveReviewCheck(context.Background(), installToken, "acme", "widget", headSHA, "MemQL / review", f.external(), 42); err == nil || f.writes != 0 {
		t.Fatal("uncertain observation wrote")
	}
	for _, req := range f.hub.seen() {
		if req.Method != http.MethodGet {
			t.Fatal("observation mutated")
		}
	}
}
