package release

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
)

func TestCutRequiresTheEntireReviewedCandidate(t *testing.T) {
	for _, missing := range []string{"all", "repository", "sha", "version"} {
		t.Run(missing, func(t *testing.T) {
			f := newFakeGitHub(t, []tagRef{{Name: "v1.0.0", Sha: "old"}}, "reviewed-head").withVersionFile("1.0.1\n")
			i, engine := ownerIntegration(t, f)
			req := approvedCutRequest(f, CutRequest{Bump: "patch"})
			switch missing {
			case "all":
				req = CutRequest{Bump: "patch"}
			case "repository":
				req.ExpectedRepository = ""
			case "sha":
				req.ExpectedSha = ""
			case "version":
				req.ExpectedVersion = ""
			}
			_, err := i.Cut(ownerCtx(), req)
			if RefusalCode(err) != CodeCandidateRequired {
				t.Fatalf("missing %s: %v", missing, err)
			}
			assertNoCandidateWrites(t, f, engine)
		})
	}
}

func TestScopedWorkflowCannotReuseAnotherCandidatesVersionCheck(t *testing.T) {
	for _, change := range []string{
		`builtin releaseSelectCandidate(previousTag: "v0.9.0")`,
		`builtin releaseReadSnapshot()`,
		"builtin releaseReadSnapshot()\n builtin releaseCheckVersion()",
	} {
		t.Run(change, func(t *testing.T) {
			f := newFakeGitHub(t, []tagRef{{Name: "v1.0.0", Sha: "old"}, {Name: "v0.9.0", Sha: "older"}}, "reviewed-head").withVersionFile("1.0.1\n")
			i, engine := ownerIntegration(t, f)
			req := approvedCutRequest(f, CutRequest{Bump: "patch"})
			if strings.Contains(change, "SelectCandidate") {
				req.ExpectedVersion = "v0.9.1"
			}
			actor, err := requireOwner(ownerCtx())
			if err != nil {
				t.Fatal(err)
			}
			scope := &cutScope{i: i, actor: actor, req: req}
			a, err := automations.NewLoader(automations.LoaderOptions{}).CompileSource(`@template
automation changedReleaseCandidate {
 builtin releaseReadSnapshot()
 builtin releaseSelectCandidate(previousTag: "v1.0.0")
 builtin releaseCheckVersion()
 `+change+`
 builtin releaseCreateTag()
}`, "candidate-test.memql")
			if err != nil {
				t.Fatal(err)
			}
			_, err = workflowhost.Run(ownerCtx(), a.Name, nil, workflowhost.Options{
				Load: func(string) (*automations.Automation, error) { return a, nil }, Operations: scope.operations(),
			})
			if RefusalCode(err) != CodeCandidateRequired {
				t.Fatalf("changed candidate reused validation: %v", err)
			}
			assertNoCandidateWrites(t, f, engine)
		})
	}
}

func TestTaggedScopeCannotReplaceItsCandidate(t *testing.T) {
	for _, operation := range []string{"snapshot", "selection"} {
		t.Run(operation, func(t *testing.T) {
			scope := &cutScope{tagged: true, out: Outcome{Version: "v1.0.1", BaseSha: "tagged-sha"}}
			var err error
			if operation == "snapshot" {
				_, err = scope.readSnapshot(ownerCtx(), nil)
			} else {
				_, err = scope.selectCandidate(ownerCtx(), map[string]any{"previousTag": "v9.0.0"})
			}
			if RefusalCode(err) != CodeCandidateRequired || scope.out.Version != "v1.0.1" || scope.out.BaseSha != "tagged-sha" {
				t.Fatalf("tagged candidate changed: %+v, %v", scope.out, err)
			}
		})
	}
}

func TestReviewedCandidateSurvivesAReplicaHopAndRefusesDrift(t *testing.T) {
	for _, change := range []string{"unchanged", "main", "version", "repository"} {
		t.Run(change, func(t *testing.T) {
			f := newFakeGitHub(t, []tagRef{{Name: "v1.0.0", Sha: "old"}}, "reviewed-head").withVersionFile("1.0.1\n")
			planner, planEngine := ownerIntegration(t, f)
			plan, err := planner.Cut(ownerCtx(), CutRequest{Bump: "patch", DryRun: true})
			if err != nil {
				t.Fatal(err)
			}
			assertNoCandidateWrites(t, f, planEngine)
			// A separate integration has no planner/session state. All approval
			// values cross the same argument boundary as a call to another node.
			publisher, publishEngine := ownerIntegration(t, f)
			switch change {
			case "main":
				f.headSha = "new-unreviewed-head"
			case "version":
				f.tags = append(f.tags, tagRef{Name: "v1.0.1", Sha: "other-commit"})
				f.withVersionFile("1.0.2\n")
			case "repository":
				publisher.resolver.env = func(name string) string {
					if name == RepoVariableName {
						return "acme/other"
					}
					if name == SecretName {
						return "token"
					}
					return ""
				}
			}
			rows, err := publisher.handleCut(ownerCtx(), map[string]any{
				"bump": plan.Bump, "expectedRepository": plan.Repository,
				"expectedSha": plan.BaseSha, "expectedVersion": plan.Version,
			}, 0)
			if change != "unchanged" {
				if RefusalCode(err) != CodeCandidateChanged {
					t.Fatalf("changed %s: %v", change, err)
				}
				assertNoCandidateWrites(t, f, publishEngine)
				return
			}
			if err != nil || len(rows) != 1 {
				t.Fatalf("approved publication: rows=%d, error=%v", len(rows), err)
			}
			if len(f.createdRefs) != 1 || len(f.createdReleases) != 1 {
				t.Fatalf("publication count: tags=%v releases=%v", f.createdRefs, f.createdReleases)
			}
			tag := f.tags[len(f.tags)-1]
			if tag.Name != plan.Version || tag.Sha != plan.BaseSha {
				t.Fatalf("published %+v, reviewed %+v", tag, plan)
			}
			records := publishEngine.callsNamed("createReleaseCut")
			if len(records) != 1 || !strings.Contains(records[0], plan.BaseSha) {
				t.Fatalf("reviewed commit absent from release history: %v", records)
			}
		})
	}
}

func assertNoCandidateWrites(t *testing.T, f *fakeGitHub, engine *recordingEngine) {
	t.Helper()
	if len(f.createdRefs) != 0 || len(f.createdReleases) != 0 || len(engine.calls) != 0 || f.pinPROpened {
		t.Fatalf("refused/dry-run candidate wrote state: tags=%v releases=%v graph=%v pinPR=%v", f.createdRefs, f.createdReleases, engine.calls, f.pinPROpened)
	}
}
