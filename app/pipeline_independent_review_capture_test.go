package app

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/pipelinerun"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/component/repositoryauthority"
)

// This adapter fixture uses the actual capture/member-selection code and the
// actual provider tree/blob/queue/check protocols. Only current native source
// and persisted journal reads are injected; there is no model or write port.
type independentCaptureFixture struct {
	t                                                                                         *testing.T
	client                                                                                    *githubapp.Client
	p                                                                                         pipelinerun.Pipeline
	source                                                                                    pipelinerun.ReviewSource
	base, mergeBase, compareStatus                                                            string
	files                                                                                     map[string]map[string]string
	saved                                                                                     map[int]pipelinerun.IndependentReviewRecord
	nativeError                                                                               error
	missingOrdinary, changedSource, changedPrompt, movedHead, movedGroup, wrongApp, truncated bool
	ordinaryReads, writes                                                                     int
}

func reviewSHA(n int) string { return strings.Repeat(fmt.Sprintf("%x", n), 40) }
func captureBlob(text string) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d%c", len(text), 0)
	h.Write([]byte(text))
	return hex.EncodeToString(h.Sum(nil))
}
func newIndependentCaptureFixture(t *testing.T) *independentCaptureFixture {
	f := &independentCaptureFixture{t: t, base: reviewSHA(1), mergeBase: reviewSHA(1), compareStatus: "ahead", saved: map[int]pipelinerun.IndependentReviewRecord{}}
	f.p = pipelinerun.Pipeline{ID: "pipeline", OwnerUserID: "owner", PackageID: "source", CredentialID: "credential", Repository: "acme/repo", DefaultBranch: "main", InstallationID: 123}
	f.source = pipelinerun.ReviewSource{PipelineID: f.p.ID, OwnerUserID: f.p.OwnerUserID, SourceID: f.p.PackageID, CredentialID: f.p.CredentialID, Repository: f.p.Repository, Branch: "main", InstallationID: 123, RepositoryID: 21, AppID: 42}
	f.files = map[string]map[string]string{
		reviewSHA(1): {"native.txt": "old native", "ordinary.txt": "old ordinary"},
		reviewSHA(2): {"native.txt": "old native", "ordinary.txt": "old ordinary", "unrelated.txt": "another merged PR"},
		reviewSHA(3): {"native.txt": "new native", "ordinary.txt": "old ordinary"},
		reviewSHA(4): {"native.txt": "old native", "ordinary.txt": "new ordinary"},
		reviewSHA(5): {"native.txt": "new native", "ordinary.txt": "old ordinary", "unrelated.txt": "another merged PR"},
		reviewSHA(6): {"native.txt": "new native", "ordinary.txt": "new ordinary", "unrelated.txt": "another merged PR"},
	}
	f.client = githubapp.New(githubapp.Config{}, githubapp.WithHTTPClient(&http.Client{Transport: f}))
	return f
}
func (f *independentCaptureFixture) run(number int) pipelinerun.Run {
	return pipelinerun.Run{ID: fmt.Sprintf("run-%d", number), OwnerUserID: "owner", PipelineID: "pipeline", Repository: "acme/repo", Event: pipelines.EventPullRequest, Mode: pipelines.ModeFull, PullRequest: number, SHA: reviewSHA(number + 2), BaseSHA: reviewSHA(1), HeadBranch: fmt.Sprintf("feature-%d", number)}
}
func (f *independentCaptureFixture) reads() pipelineReviewReader {
	return pipelineReviewReader{
		source: func(ctx context.Context, p pipelinerun.Pipeline, fence func(context.Context) error) (pipelinerun.ReviewSource, *githubapp.Client, string, error) {
			if err := fence(ctx); err != nil {
				return pipelinerun.ReviewSource{}, nil, "", err
			}
			require.Equal(f.t, f.p, p)
			source := f.source
			if f.changedSource {
				source.CredentialID = "revoked"
			}
			return source, f.client, "read-token", nil
		},
		prompt: func(name string) (map[string]any, error) {
			body := "Review the complete immutable change."
			if f.changedPrompt {
				body = "changed prompt"
			}
			return map[string]any{"name": name, "body": body}, nil
		},
		native: func(_ context.Context, source pipelinerun.ReviewSource, m githubapp.MergeGroupMember) (repositoryauthority.NativeReviewMember, bool, error) {
			if m.Number != 1 {
				return repositoryauthority.NativeReviewMember{}, false, nil
			}
			if f.nativeError != nil {
				return repositoryauthority.NativeReviewMember{}, true, f.nativeError
			}
			return repositoryauthority.NativeReviewMember{BaseSHA: reviewSHA(1), Review: repositoryauthority.MergeGroupReviewMember{RequestID: "native-request", ReviewDigest: strings.Repeat("a", 64), ExternalID: "memql-review:" + strings.Repeat("a", 64), CheckRunID: 71, AppID: 42}}, true, nil
		},
		ordinary: func(_ context.Context, p pipelinerun.Pipeline, number int, head string) (pipelinerun.Run, pipelinerun.IndependentReviewRecord, error) {
			f.ordinaryReads++
			require.Equal(f.t, 2, number)
			require.Equal(f.t, reviewSHA(4), head)
			if f.missingOrdinary {
				return pipelinerun.Run{}, pipelinerun.IndependentReviewRecord{}, errors.New("latest review missing")
			}
			record, ok := f.saved[number]
			if !ok {
				return pipelinerun.Run{}, record, errors.New("not reviewed")
			}
			return f.run(number), record, nil
		},
	}
}
func (f *independentCaptureFixture) capture(r pipelinerun.Run) (pipelinerun.IndependentReviewInput, error) {
	return capturePipelineIndependentReview(f.t.Context(), f.p, r, "independentPrompt", "policy-version", func(c context.Context) error { return c.Err() }, f.reads())
}
func (f *independentCaptureFixture) saveOrdinary() {
	input, err := f.capture(f.run(2))
	require.NoError(f.t, err)
	require.Equal(f.t, "ordinary", input.Kind)
	f.saved[2] = pipelinerun.IndependentReviewRecord{Version: 1, RunID: "run-2", Input: input, InputDigest: pipelineReviewDigest(input), ModelAttempted: true, Verdict: &pipelinerun.IndependentReviewVerdict{Approved: true, Reason: "Full diff accepted."}, ReportAttempted: true, Receipt: &pipelinerun.IndependentReviewReceipt{CheckName: pipelines.IndependentReviewCheckName, HeadSHA: reviewSHA(4), ExternalID: "memql-review:" + strings.Repeat("b", 64), CheckRunID: 72, AppID: 42}}
}
func (f *independentCaptureFixture) groupRun() pipelinerun.Run {
	return pipelinerun.Run{ID: "group-run", OwnerUserID: "owner", PipelineID: "pipeline", Repository: "acme/repo", Event: pipelines.EventMergeGroup, Mode: pipelines.ModeFull, SHA: reviewSHA(6), BaseSHA: reviewSHA(2), HeadBranch: "gh-readonly-queue/main/group"}
}
func (f *independentCaptureFixture) RoundTrip(r *http.Request) (*http.Response, error) {
	require.Equal(f.t, "Bearer read-token", r.Header.Get("Authorization"))
	var out any
	status := 200
	repo := map[string]any{"id": "repo-node", "databaseId": 21, "nameWithOwner": "acme/repo"}
	switch {
	case r.URL.Path == "/graphql":
		var in struct{ Query string }
		require.NoError(f.t, json.NewDecoder(r.Body).Decode(&in))
		require.Contains(f.t, in.Query, "MemQLMergeGroup")
		entry := func(number int, base, head string) any {
			return map[string]any{"id": fmt.Sprintf("entry-%d", number), "headCommit": map[string]string{"oid": head}, "baseCommit": map[string]string{"oid": base}, "pullRequest": map[string]any{"number": number, "headRefOid": reviewSHA(number + 2), "baseRefName": "main", "state": "OPEN", "isDraft": false, "isInMergeQueue": true, "repository": repo, "headRepository": repo, "baseRepository": repo}}
		}
		groupHead := reviewSHA(6)
		if f.movedGroup {
			groupHead = reviewSHA(7)
		}
		out = map[string]any{"data": map[string]any{"repository": map[string]any{"id": "repo-node", "databaseId": 21, "nameWithOwner": "acme/repo", "defaultBranchRef": map[string]any{"name": "main", "target": map[string]string{"oid": reviewSHA(2)}}, "ref": map[string]any{"target": map[string]string{"oid": groupHead}}, "mergeQueue": map[string]any{"configuration": map[string]string{"mergeMethod": "MERGE"}, "entries": map[string]any{"totalCount": 2, "pageInfo": map[string]any{"hasNextPage": false}, "nodes": []any{entry(1, reviewSHA(2), reviewSHA(5)), entry(2, reviewSHA(5), reviewSHA(6))}}}}}}
	case strings.HasPrefix(r.URL.Path, "/repos/acme/repo/pulls/"):
		number := 2
		head := reviewSHA(4)
		if f.movedHead {
			head = reviewSHA(7)
		}
		ref := func(sha, name string) any {
			return map[string]any{"sha": sha, "ref": name, "repo": map[string]string{"full_name": "acme/repo"}}
		}
		out = map[string]any{"number": number, "state": "open", "merged": false, "head": ref(head, "feature-2"), "base": ref(f.base, "main"), "html_url": "https://github.com/acme/repo/pull/2"}
	case strings.HasPrefix(r.URL.Path, "/repos/acme/repo/compare/"):
		if strings.HasSuffix(r.URL.Path, reviewSHA(4)) {
			out = map[string]any{"base_commit": map[string]string{"sha": f.base}, "merge_base_commit": map[string]string{"sha": f.mergeBase}}
		} else {
			out = map[string]any{"status": f.compareStatus, "behind_by": 0, "base_commit": map[string]string{"sha": reviewSHA(1)}, "merge_base_commit": map[string]string{"sha": reviewSHA(1)}}
		}
	case strings.HasPrefix(r.URL.Path, "/repos/acme/repo/git/commits/"):
		sha := strings.TrimPrefix(r.URL.Path, "/repos/acme/repo/git/commits/")
		require.Contains(f.t, f.files, sha)
		body := map[string]any{"sha": sha, "tree": map[string]string{"sha": sha}}
		if sha == reviewSHA(5) {
			body["parents"] = []any{map[string]string{"sha": reviewSHA(2)}, map[string]string{"sha": reviewSHA(3)}}
		}
		if sha == reviewSHA(6) {
			body["parents"] = []any{map[string]string{"sha": reviewSHA(5)}, map[string]string{"sha": reviewSHA(4)}}
		}
		out = body
	case strings.HasPrefix(r.URL.Path, "/repos/acme/repo/git/trees/"):
		sha := strings.TrimPrefix(r.URL.Path, "/repos/acme/repo/git/trees/")
		entries := []any{}
		for path, text := range f.files[sha] {
			entries = append(entries, map[string]string{"path": path, "mode": "100644", "type": "blob", "sha": captureBlob(text)})
		}
		out = map[string]any{"sha": sha, "truncated": f.truncated, "tree": entries}
	case strings.HasPrefix(r.URL.Path, "/repos/acme/repo/git/blobs/"):
		sha := strings.TrimPrefix(r.URL.Path, "/repos/acme/repo/git/blobs/")
		found := false
		for _, files := range f.files {
			for _, text := range files {
				if captureBlob(text) == sha {
					out = map[string]any{"sha": sha, "size": len(text), "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(text))}
					found = true
				}
			}
		}
		require.True(f.t, found)
	case strings.HasPrefix(r.URL.Path, "/repos/acme/repo/check-runs/"):
		id := int64(71)
		head := reviewSHA(3)
		external := "memql-review:" + strings.Repeat("a", 64)
		if strings.HasSuffix(r.URL.Path, "72") {
			id = 72
			head = reviewSHA(4)
			external = "memql-review:" + strings.Repeat("b", 64)
		}
		app := 42
		if f.wrongApp {
			app = 91
		}
		out = map[string]any{"id": id, "name": pipelines.IndependentReviewCheckName, "head_sha": head, "external_id": external, "status": "completed", "conclusion": "success", "app": map[string]int{"id": app}}
	default:
		f.t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
	}
	if r.Method != "GET" && r.URL.Path != "/graphql" {
		f.writes++
		f.t.Fatal("capture performed an effect")
	}
	data, err := json.Marshal(out)
	require.NoError(f.t, err)
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(data))), Request: r}, nil
}
func TestIndependentReviewCaptureAllowsOnlyProvenDisjointMainAdvance(t *testing.T) {
	for _, kind := range []string{"disjoint", "empty target advance", "overlap", "directory overlap", "rewritten base", "changed merge base", "head moved", "source changed", "prompt changed", "incomplete tree"} {
		t.Run(kind, func(t *testing.T) {
			f := newIndependentCaptureFixture(t)
			f.saveOrdinary()
			saved := f.saved[2].Input
			f.base = reviewSHA(2)
			switch kind {
			case "empty target advance":
				f.files[reviewSHA(2)] = f.files[reviewSHA(1)]
			case "overlap":
				f.files[reviewSHA(2)]["ordinary.txt"] = "unreviewed target edit"
			case "directory overlap":
				delete(f.files[reviewSHA(2)], "ordinary.txt")
				f.files[reviewSHA(2)]["ordinary.txt/child"] = "new directory"
			case "rewritten base":
				f.compareStatus = "diverged"
			case "changed merge base":
				f.mergeBase = reviewSHA(2)
			case "head moved":
				f.movedHead = true
			case "source changed":
				f.changedSource = true
			case "prompt changed":
				f.changedPrompt = true
			case "incomplete tree":
				f.truncated = true
			}
			fresh, err := f.capture(f.run(2))
			if err == nil {
				err = samePipelineIndependentReview(t.Context(), saved, fresh, f.client, "read-token")
			}
			if kind == "disjoint" || kind == "empty target advance" {
				require.NoError(t, err)
				require.Equal(t, reviewSHA(1), saved.PullRequest.BaseSHA)
				require.Equal(t, reviewSHA(2), fresh.PullRequest.BaseSHA)
				require.Equal(t, saved.Diff, fresh.Diff)
			} else {
				require.Error(t, err)
			}
			require.Zero(t, f.writes)
		})
	}
}
func TestMixedGroupCaptureReusesOnlyCurrentNativeAndOrdinaryProofs(t *testing.T) {
	for _, kind := range []string{"disjoint main advance", "overlapping main", "synthetic conflict", "synthetic extra", "native rejected", "ordinary missing", "wrong App", "source changed", "policy changed", "prompt changed", "membership changed"} {
		t.Run(kind, func(t *testing.T) {
			f := newIndependentCaptureFixture(t)
			f.saveOrdinary()
			f.base = reviewSHA(2)
			switch kind {
			case "overlapping main":
				f.files[reviewSHA(2)]["ordinary.txt"] = "changed before merge"
			case "synthetic conflict":
				f.files[reviewSHA(6)]["ordinary.txt"] = "conflict resolved differently"
			case "synthetic extra":
				f.files[reviewSHA(6)]["extra"] = "not reviewed"
			case "native rejected":
				f.nativeError = errors.New("native verdict rejected")
			case "ordinary missing":
				f.missingOrdinary = true
			case "wrong App":
				f.wrongApp = true
			case "source changed":
				f.changedSource = true
			case "policy changed":
				r := f.saved[2]
				r.Input.PolicyVersion = "old policy"
				f.saved[2] = r
			case "prompt changed":
				f.changedPrompt = true
			case "membership changed":
				f.movedGroup = true
			}
			input, err := f.capture(f.groupRun())
			if kind == "disjoint main advance" {
				require.NoError(t, err)
				require.Equal(t, "group", input.Kind)
				require.Len(t, input.Members, 2)
				require.Equal(t, []string{"native", "ordinary"}, []string{input.Members[0].Kind, input.Members[1].Kind})
				require.Equal(t, "native-request", input.Members[0].RequestID)
				require.Equal(t, "run-2", input.Members[1].RunID)
				require.Empty(t, input.Members[1].RequestID)
				require.Equal(t, reviewSHA(6), input.Group.HeadSHA)
				require.Equal(t, f.saved[2].InputDigest, input.Members[1].ReviewDigest)
			} else {
				require.Error(t, err)
			}
			if kind == "native rejected" {
				require.Zero(t, f.ordinaryReads, "invalid native evidence must stop before ordinary fallback")
			}
			require.Zero(t, f.writes)
		})
	}
}

func TestIndependentReviewCaptureUsesCanonicalOwnerIdentity(t *testing.T) {
	f := newIndependentCaptureFixture(t)
	f.p.OwnerUserID = "v1:identity:user:owner"
	input, err := f.capture(f.run(2))
	require.NoError(t, err)
	require.Equal(t, "owner", input.Source.OwnerUserID)
	run := f.run(2)
	run.OwnerUserID = "another-owner"
	_, err = f.capture(run)
	require.ErrorContains(t, err, "foreign")
	require.Zero(t, f.writes)
}
