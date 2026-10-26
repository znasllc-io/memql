package pipelinerun

import (
	"context"
	"crypto/sha1" // Git fixture blob identity.
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/identity/githubapp"
)

func TestIndependentReviewLegacyDigestRemainsByteIdentical(t *testing.T) {
	// Deliberately the exact pre-projection type/order, not an alias of the new
	// type: an omitted field must not rewrite saved legacy InputDigest values.
	type legacyInput struct {
		Kind                      string
		Source                    ReviewSource
		PolicyVersion, PromptName string
		Prompt                    map[string]any
		PullRequest               *githubapp.PullRequestObservation
		Diff                      *githubapp.ReviewDiff
		Group                     *githubapp.MergeGroupEvidence
		Members                   []ReviewMember
	}
	old := legacyInput{Kind: "ordinary", Source: ReviewSource{OwnerUserID: "owner"}, PolicyVersion: "policy", PromptName: "review", Prompt: map[string]any{"source": "full bytes"}, PullRequest: &githubapp.PullRequestObservation{HeadSHA: strings.Repeat("1", 40)}, Diff: &githubapp.ReviewDiff{Files: []githubapp.ReviewFileChange{{Path: "source", After: githubapp.ReviewFileVersion{Content: "complete"}}}}}
	b, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	var next IndependentReviewInput
	if err = json.Unmarshal(b, &next); err != nil {
		t.Fatal(err)
	}
	actual, err := json.Marshal(next)
	if err != nil || string(actual) != string(b) || reviewDigest(old) != reviewDigest(next) {
		t.Fatal("legacy evidence digest rewritten", err)
	}
	record := IndependentReviewRecord{Version: 1, RunID: "run", Input: next, InputDigest: reviewDigest(old)}
	raw, _ := json.Marshal(record)
	var object map[string]any
	if err = json.Unmarshal(raw, &object); err != nil {
		t.Fatal(err)
	}
	if _, err = readIndependentReview(object); err != nil {
		t.Fatal("legacy persisted record refused", err)
	}
}
func TestIndependentReviewProjectionPreflightFailureNeverMarksAttempt(t *testing.T) {
	dh, dr, f := independentReviewDriver(t)
	dr.d.IndependentReview.Capture = func(ctx context.Context, _ Pipeline, _ Run, _, _ string, _ func(context.Context) error) (IndependentReviewInput, error) {
		content := strings.Repeat("x", githubapp.ReviewProjectionMaxBytes)
		blob := fmt.Sprintf("%x", sha1.Sum([]byte(fmt.Sprintf("blob %d%c%s", len(content), 0, content))))
		_, err := githubapp.ProjectReviewDiff(ctx, githubapp.ReviewDiff{BeforeSHA: strings.Repeat("1", 40), AfterSHA: strings.Repeat("2", 40), BeforeTree: strings.Repeat("3", 40), AfterTree: strings.Repeat("4", 40), Files: []githubapp.ReviewFileChange{{Path: "large", After: githubapp.ReviewFileVersion{Present: true, Mode: "100644", BlobSHA: blob, Content: content}}}})
		return IndependentReviewInput{}, err
	}
	if _, err := dr.independentReview(context.Background()); err == nil {
		t.Fatal("failed capture accepted")
	}
	stored := mustRun(t, dh.store, dr.runID)
	if stored.ReviewEvidence != nil || f.model != 0 || f.reports != 0 {
		t.Fatal("capture failure recorded attempt or invoked effect")
	}
}
