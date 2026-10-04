package pipelinerun

import (
	"encoding/json"
	"testing"
)

// deliveries_test.go -- GitHub delivery bodies, trimmed to what
// pipelines.ClassifyDelivery reads (its own fixtures are the realistic ones,
// component/pipelines/testdata/github). Built here so each test names exactly
// the repository, installation and commits it is about.

const testInstallation = 7

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal delivery: %v", err)
	}
	return string(b)
}

func envelopeFor(repo string, installation int64) map[string]any {
	return map[string]any{
		"repository":   map[string]any{"full_name": repo, "default_branch": "main"},
		"installation": map[string]any{"id": installation},
	}
}

// prDelivery is a pull_request delivery whose head lives in headRepo ("" is
// a deleted fork: GitHub reports the head repository as null).
func prDelivery(t *testing.T, action string, number int, sha, headRepo string, installation int64) string {
	t.Helper()
	body := envelopeFor(repoName, installation)
	var repo any
	if headRepo != "" {
		repo = map[string]any{"full_name": headRepo}
	}
	body["action"] = action
	body["number"] = number
	body["pull_request"] = map[string]any{
		"number": number,
		"title":  "Show the cart count",
		"head":   map[string]any{"ref": "cart-badge", "sha": sha, "repo": repo},
		"base":   map[string]any{"ref": "main", "sha": shaBase, "repo": map[string]any{"full_name": repoName}},
	}
	return mustJSON(t, body)
}

func pushDelivery(t *testing.T, ref, before, after string, installation int64) string {
	t.Helper()
	body := envelopeFor(repoName, installation)
	body["ref"] = ref
	body["before"] = before
	body["after"] = after
	body["head_commit"] = map[string]any{"message": "Land the cart badge\n\nLonger body."}
	return mustJSON(t, body)
}

func mergeGroupDelivery(t *testing.T, sha string, installation int64) string {
	t.Helper()
	body := envelopeFor(repoName, installation)
	body["action"] = "checks_requested"
	body["merge_group"] = map[string]any{
		"head_sha": sha, "head_ref": "refs/heads/gh-readonly-queue/main/pr-42", "base_sha": shaBase,
		"head_commit": map[string]any{"message": "Merge pull request #42"},
	}
	return mustJSON(t, body)
}

func checkRunDelivery(t *testing.T, checkRunID int64, sha string, installation int64) string {
	t.Helper()
	body := envelopeFor(repoName, installation)
	body["action"] = "rerequested"
	body["check_run"] = map[string]any{"id": checkRunID, "head_sha": sha}
	return mustJSON(t, body)
}

func checkSuiteDelivery(t *testing.T, sha string, installation int64) string {
	t.Helper()
	body := envelopeFor(repoName, installation)
	body["action"] = "rerequested"
	body["check_suite"] = map[string]any{"head_sha": sha}
	return mustJSON(t, body)
}

func releaseDelivery(t *testing.T, tag string, installation int64) string {
	t.Helper()
	body := envelopeFor(repoName, installation)
	body["action"] = "published"
	body["release"] = map[string]any{"tag_name": tag, "name": "Shop " + tag}
	return mustJSON(t, body)
}

// deliveryHeadersJSON is the headers the inbound seam stages, lower-cased.
func deliveryHeadersJSON(t *testing.T, event, deliveryID string) string {
	t.Helper()
	return mustJSON(t, map[string]string{"x-github-event": event, "x-github-delivery": deliveryID, "x-github-hook-id": "1"})
}
