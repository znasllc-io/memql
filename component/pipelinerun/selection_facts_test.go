package pipelinerun

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/identity/githubapp"
)

const selectionFactsPolicy = `@template
    automation pipelineStageIncluded {
      args { pullRequest object! stage string! }
      if args.stage == "tests" {
        return {included: args.pullRequest.present && args.pullRequest.author == "package-bot", reason: "Dependency maintenance applies only to package-bot changes."}
      }
      return true
    }`

func selectionFactsHead() githubapp.PullRequestHead {
	return githubapp.PullRequestHead{Number: 42, State: "open", Author: "human", HeadSHA: shaA, HeadRef: "cart", HeadRepository: repoName, BaseSHA: shaBase}
}

func selectionFactsHarness(t *testing.T) *driveHarness {
	t.Helper()
	dh := newDriveHarness(t, driveManifest)
	a := workflowSource(t, selectionFactsPolicy)
	dh.integ.Configure(func(d *Deps) { bindTestWorkflow(d, a) })
	dh.github.pulls[repoName] = []githubapp.PullRequestHead{selectionFactsHead()}
	return dh
}

func TestStageSelectionFactsExcludeBeforeExecutorAndKeepReason(t *testing.T) {
	for _, author := range []string{"human", "package-bot"} {
		t.Run(author, func(t *testing.T) {
			dh := selectionFactsHarness(t)
			dh.github.pulls[repoName][0].Author = author
			run := dh.openRun(t, prOpening())
			deliver(t, dh.integ, run)
			got := mustRun(t, dh.store, run.ID)
			want := []string{"checks.vet"}
			if author == "package-bot" {
				want = append(want, "tests.lint", "tests.unit")
			}
			keys := dh.exec.sentKeys()
			slices.Sort(keys)
			if got.Conclusion != ConclusionSuccess || !slices.Equal(keys, want) || got.SelectionFacts == nil || len(dh.github.headReads()) != 1 {
				t.Fatalf("selection outcome: conclusion=%s keys=%v facts=%v reads=%v", got.Conclusion, keys, got.SelectionFacts != nil, dh.github.headReads())
			}
			if !slices.ContainsFunc(got.Notes, func(n Note) bool {
				return n.Code == "pipeline_stage_selection:tests" && strings.Contains(n.Message, "Dependency maintenance")
			}) {
				t.Fatalf("missing readable selection note: %+v", got.Notes)
			}
		})
	}
}

func TestStageSelectionFactsFailClosedBeforeExecutor(t *testing.T) {
	for _, name := range []string{"provider unavailable", "moved commit", "fork", "branch", "closed", "missing author", "source owner", "source installation"} {
		t.Run(name, func(t *testing.T) {
			dh := selectionFactsHarness(t)
			run := dh.openRun(t, prOpening())
			head := &dh.github.pulls[repoName][0]
			switch name {
			case "provider unavailable":
				dh.github.pullHeadErr = errors.New("unavailable")
			case "moved commit":
				head.HeadSHA = shaB
			case "fork":
				head.HeadRepository = "someone/else"
			case "branch":
				head.HeadRef = "another"
			case "closed":
				head.State = "closed"
			case "missing author":
				head.Author = ""
			case "source owner":
				p := dh.p
				p.OwnerUserID = "another-owner"
				dh.store.addPipeline(p)
			case "source installation":
				p := dh.p
				p.InstallationID++
				dh.store.addPipeline(p)
			}
			deliver(t, dh.integ, run)
			got := mustRun(t, dh.store, run.ID)
			if got.Conclusion != ConclusionFailure || len(dh.exec.sentKeys()) != 0 || got.WorkRunID != "" || got.SelectionFacts != nil {
				t.Fatalf("unverified selection executed: conclusion=%s work=%s keys=%v facts=%v", got.Conclusion, got.WorkRunID, dh.exec.sentKeys(), got.SelectionFacts != nil)
			}
		})
	}
}

func TestStageSelectionLegacyAndNonPRDoNotReadProvider(t *testing.T) {
	for _, legacy := range []bool{true, false} {
		dh := newDriveHarness(t, driveManifest)
		opening := prOpening()
		if !legacy {
			a := workflowSource(t, selectionFactsPolicy)
			dh.integ.Configure(func(d *Deps) { bindTestWorkflow(d, a) })
			opening = pushOpening()
		}
		dh.github.pullHeadErr = errors.New("must not be read")
		run := dh.openRun(t, opening)
		deliver(t, dh.integ, run)
		got := mustRun(t, dh.store, run.ID)
		if got.Conclusion != ConclusionSuccess || len(dh.github.headReads()) != 0 {
			t.Fatalf("legacy=%v conclusion=%s reads=%v", legacy, got.Conclusion, dh.github.headReads())
		}
		if legacy && got.SelectionFacts != nil {
			t.Fatal("legacy resolver opted into provider facts")
		}
	}
}

func preparedSelectionDriver(t *testing.T, dh *driveHarness, run Run) *runDriver {
	t.Helper()
	dr := claimedTestDriver(t, dh.integ, run.ID)
	dr.p, dr.installation, dr.token = dh.p, dh.p.InstallationID, "read-token"
	if err := dr.prepareWorkflow(""); err != nil {
		t.Fatal(err)
	}
	return dr
}

type selectionHookGitHub struct {
	GitHub
	after func()
}

func (g selectionHookGitHub) PullRequestHead(ctx context.Context, token, repo string, number int) (githubapp.PullRequestHead, error) {
	head, err := g.GitHub.PullRequestHead(ctx, token, repo, number)
	g.after()
	return head, err
}

func TestStageSelectionProviderReturnCannotPersistAfterAuthorityEnds(t *testing.T) {
	for _, stop := range []string{"context", "cancel request", "lease", "terminal", "source changed"} {
		t.Run(stop, func(t *testing.T) {
			dh := selectionFactsHarness(t)
			run := dh.openRun(t, prOpening())
			dr := preparedSelectionDriver(t, dh, run)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			dr.d.GitHub = selectionHookGitHub{GitHub: dh.github, after: func() {
				switch stop {
				case "context":
					cancel()
				case "cancel request":
					if err := dh.store.UpdateRun(context.Background(), run.OwnerUserID, run.ID, RunPatch{CancelRequested: ptr(true)}); err != nil {
						t.Fatal(err)
					}
				case "lease":
					_ = claimedTestDriver(t, dh.integ, run.ID)
				case "terminal":
					if err := dh.store.UpdateRun(context.Background(), run.OwnerUserID, run.ID, RunPatch{Status: ptr(StatusCompleted)}); err != nil {
						t.Fatal(err)
					}
				case "source changed":
					p := dh.p
					p.CredentialID = "replacement-credential"
					dh.store.addPipeline(p)
				}
			}}
			if _, err := dr.stagePullRequest(ctx); err == nil {
				t.Fatal("stale provider answer accepted")
			}
			if got := mustRun(t, dh.store, run.ID); got.SelectionFacts != nil {
				t.Fatal("stale facts persisted")
			}
		})
	}
}

func TestStageSelectionDecisionRejectsMalformedResults(t *testing.T) {
	for _, value := range []any{nil, "false", map[string]any{"included": false}, map[string]any{"included": false, "reason": ""}, map[string]any{"included": "false", "reason": "reason"}, map[string]any{"included": false, "reason": "reason", "grant": true}, map[string]any{"included": false, "reason": "two\nlines"}, map[string]any{"included": false, "reason": strings.Repeat("x", 513)}} {
		if _, _, err := stageSelectionDecision(value); err == nil {
			t.Fatalf("accepted malformed decision: %#v", value)
		}
	}
}

func TestStageSelectionSavedFactsAndNotesAreBoundAndIdempotent(t *testing.T) {
	dh := selectionFactsHarness(t)
	run := dh.openRun(t, prOpening())
	dr := preparedSelectionDriver(t, dh, run)
	ctx := context.Background()
	original, err := dr.stagePullRequest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := dr.writeRun(ctx, RunPatch{Notes: ptr([]Note{{Code: "unrelated", Message: "Keep this note"}})}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := dr.recordStageDecision(ctx, "tests", false, "No maintenance applies."); err != nil {
			t.Fatal(err)
		}
	}
	if err := dr.recordStageDecision(ctx, "tests", false, "Changed explanation."); err == nil {
		t.Fatal("changed saved decision")
	}
	if err := dr.recordStageDecision(ctx, "tests", true, "No maintenance applies."); err == nil {
		t.Fatal("changed saved inclusion with the same explanation")
	}
	if notes := mustRun(t, dh.store, run.ID).Notes; len(notes) != 2 || notes[0].Code != "unrelated" {
		t.Fatalf("notes: %+v", notes)
	}
	// A stale/missing provider cannot change the original observation on recovery.
	dh.github.pullHeadErr = errors.New("unavailable on recovering replica")
	fresh := preparedSelectionDriver(t, dh, run)
	recovered, err := fresh.stagePullRequest(ctx)
	if err != nil || !reflect.DeepEqual(selectionJSON(t, original), selectionJSON(t, recovered)) || len(dh.github.headReads()) != 1 {
		t.Fatalf("facts changed: %v reads=%v", err, dh.github.headReads())
	}
	p := dh.p
	p.CredentialID = "different"
	dh.store.addPipeline(p)
	fresh.p = p
	if _, err := fresh.stagePullRequest(ctx); err == nil {
		t.Fatal("reused facts under another credential")
	}
}

func selectionJSON(t *testing.T, value map[string]any) map[string]any {
	t.Helper()
	var normalized map[string]any
	if err := decodeSelectionObject(value, &normalized); err != nil {
		t.Fatal(err)
	}
	return normalized
}

// The new eligibility observation deliberately does not alter or substitute
// for repositoryChangeOrigin's current provider and actor checks. Existing
// TestRepositoryChangeRefusesForgedEvidenceAndRevokedAuthority is run with this
// suite as the write-side negative control.

func TestStageSelectionSavedFactBindingsRejectTampering(t *testing.T) {
	dh := selectionFactsHarness(t)
	run := dh.openRun(t, prOpening())
	dr := preparedSelectionDriver(t, dh, run)
	if _, err := dr.stagePullRequest(context.Background()); err != nil {
		t.Fatal(err)
	}
	binding, err := dr.selectionBinding(dr.run, dh.p)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"runId", "ownerUserId", "pipelineId", "repository", "sha", "event", "headBranch", "installationId", "credentialId", "policyVersion"} {
		t.Run(field, func(t *testing.T) {
			forged := selectionJSON(t, dr.run.SelectionFacts)
			forged["binding"].(map[string]any)[field] = "another-value"
			if _, err := readSelectionObservation(forged, binding); err == nil {
				t.Fatal("accepted mismatched stored observation")
			}
		})
	}
	for _, field := range []string{"version", "observedAt", "pullRequest"} {
		forged := selectionJSON(t, dr.run.SelectionFacts)
		delete(forged, field)
		if _, err := readSelectionObservation(forged, binding); err == nil {
			t.Fatalf("accepted missing %s", field)
		}
	}
	forged := selectionJSON(t, dr.run.SelectionFacts)
	forged["pullRequest"].(map[string]any)["canWrite"] = true
	if _, err := readSelectionObservation(forged, binding); err == nil {
		t.Fatal("accepted unknown provider field")
	}
}
