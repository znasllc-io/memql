package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/pipelinerun"
	"github.com/znasllc-io/memql/component/pipelines"
)

func projectionPromptSnapshot() map[string]any {
	source := githubapp.ReviewProjectionPromptMarker + "\nRepository {{.repository}}\nPR {{.pullRequest}}\nChanges {{.changes}}\nRefuse if omitted unchanged context is needed."
	return map[string]any{"version": 1, "name": "independentPrompt", "description": "review", "level": "reasoning", "defaultProvider": "", "source": source, "templates": map[string]any{"independentPrompt": source}, "inputSchema": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"repository", "pullRequest", "changes"}, "properties": map[string]any{"repository": map[string]any{"type": "string"}, "pullRequest": map[string]any{"type": "string"}, "changes": map[string]any{"type": "string"}}}}
}
func TestPipelineReviewCapturesProjectionBeforeModelWithExactSource(t *testing.T) {
	f := newIndependentCaptureFixture(t)
	reads := f.reads()
	reads.prompt = func(string) (map[string]any, error) { return projectionPromptSnapshot(), nil }
	input, err := capturePipelineIndependentReview(t.Context(), f.p, f.run(2), "independentPrompt", "policy-version", func(ctx context.Context) error { return ctx.Err() }, reads)
	require.NoError(t, err)
	require.NotNil(t, input.Projection)
	require.Equal(t, input.Diff.Digest(), input.Projection.SourceDigest)
	require.Equal(t, input.Projection.Digest(), input.ProjectionDigest)
	data, err := pipelineReviewModelData(t.Context(), input)
	require.NoError(t, err)
	var wire githubapp.ReviewProjection
	require.NoError(t, json.Unmarshal([]byte(data["changes"].(string)), &wire))
	require.Equal(t, *input.Projection, wire)
	require.Zero(t, f.writes)
	// Source authority remains complete, including bytes omitted from model context.
	require.Equal(t, "old ordinary", input.Diff.Files[0].Before.Content)
	require.Equal(t, "new ordinary", input.Diff.Files[0].After.Content)
}
func TestPipelineReviewProjectionRefusesIncompleteFormatAndCaptureOverflow(t *testing.T) {
	for _, kind := range []string{"missing-projection", "digest", "hunk", "legacy-prompt", "compiled-prompt", "projection-overflow", "render-overflow"} {
		t.Run(kind, func(t *testing.T) {
			f := newIndependentCaptureFixture(t)
			reads := f.reads()
			snapshot := projectionPromptSnapshot()
			if kind == "projection-overflow" {
				f.files[reviewSHA(4)]["ordinary.txt"] = strings.Repeat("x", githubapp.ReviewProjectionMaxBytes)
			}
			if kind == "render-overflow" {
				snapshot["templates"].(map[string]any)["independentPrompt"] = githubapp.ReviewProjectionPromptMarker + "\n" + strings.Repeat("{{.changes}}", 4000)
			}
			if kind == "compiled-prompt" {
				snapshot["templates"].(map[string]any)["independentPrompt"] = "Legacy full file text {{.changes}}"
			}
			reads.prompt = func(string) (map[string]any, error) { return snapshot, nil }
			input, err := capturePipelineIndependentReview(t.Context(), f.p, f.run(2), "independentPrompt", "policy-version", func(ctx context.Context) error { return ctx.Err() }, reads)
			if kind == "projection-overflow" || kind == "render-overflow" || kind == "compiled-prompt" {
				require.Error(t, err)
				require.Zero(t, f.writes)
				return
			}
			require.NoError(t, err)
			switch kind {
			case "missing-projection":
				input.Projection = nil
				input.ProjectionDigest = ""
			case "digest":
				input.ProjectionDigest = strings.Repeat("0", 64)
			case "hunk":
				input.Projection.Files[0].Hunks = nil
				input.ProjectionDigest = input.Projection.Digest()
			case "legacy-prompt":
				input.Prompt = map[string]any{"source": "Review full before/after bytes."}
			}
			_, err = pipelineReviewModelData(t.Context(), input)
			require.Error(t, err)
			require.Zero(t, f.writes)
		})
	}
}
func TestPipelineReviewLegacyPromptRetainsFullFileRepresentation(t *testing.T) {
	f := newIndependentCaptureFixture(t)
	input, err := f.capture(f.run(2))
	require.NoError(t, err)
	require.Nil(t, input.Projection)
	require.Empty(t, input.ProjectionDigest)
	data, err := pipelineReviewModelData(t.Context(), input)
	require.NoError(t, err)
	expected, err := json.Marshal(input.Diff)
	require.NoError(t, err)
	require.Equal(t, string(expected), data["changes"])
}
func TestPipelineReviewProjectedMemberRetainsFullSyntheticEquality(t *testing.T) {
	f := newIndependentCaptureFixture(t)
	reads := f.reads()
	reads.prompt = func(string) (map[string]any, error) { return projectionPromptSnapshot(), nil }
	fence := func(ctx context.Context) error { return ctx.Err() }
	input, err := capturePipelineIndependentReview(t.Context(), f.p, f.run(2), "independentPrompt", "policy-version", fence, reads)
	require.NoError(t, err)
	f.saved[2] = pipelinerun.IndependentReviewRecord{Version: 1, RunID: "run-2", Input: input, InputDigest: pipelineReviewDigest(input), ModelAttempted: true, Verdict: &pipelinerun.IndependentReviewVerdict{Approved: true, Reason: "Accepted exact evidence."}, ReportAttempted: true, Receipt: &pipelinerun.IndependentReviewReceipt{CheckName: pipelines.IndependentReviewCheckName, HeadSHA: reviewSHA(4), ExternalID: "memql-review:" + strings.Repeat("b", 64), CheckRunID: 72, AppID: 42}}
	group, err := capturePipelineIndependentReview(t.Context(), f.p, f.groupRun(), "independentPrompt", "policy-version", fence, reads)
	require.NoError(t, err)
	require.Equal(t, "group", group.Kind)
	require.Len(t, group.Members, 2)
	require.Nil(t, group.Projection)
	f.files[reviewSHA(6)]["ordinary.txt"] = "unreviewed overlapping edit"
	_, err = capturePipelineIndependentReview(t.Context(), f.p, f.groupRun(), "independentPrompt", "policy-version", fence, reads)
	require.ErrorContains(t, err, "synthetic member changes differ")
	require.Zero(t, f.writes)
}
