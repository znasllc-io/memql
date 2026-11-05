package pipelinerun

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"testing"

	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/pipelines"
)

func allowReleaseBuild(ctx context.Context) error { return ctx.Err() }

type releaseBuildRemote struct {
	*fakeGitHub
	proof    githubapp.ReleaseSourceEvidence
	proofErr error
	onProof  func()
	// proved lists the commit each proof was asked for.
	proved []string
	walk   githubapp.ReleaseSourceWalk
	// walkErr is what the walk answers instead of walk.
	walkErr error
	walks   int
}

func (g *releaseBuildRemote) ObserveReleaseSource(ctx context.Context, token, repository, branch, selected string) (githubapp.ReleaseSourceEvidence, error) {
	watchGitHubCall(g.t, ctx, "ObserveReleaseSource")
	g.proved = append(g.proved, selected)
	if token == "" || repository != repoName || branch != "main" || selected != "" && selected != g.proof.SHA {
		return githubapp.ReleaseSourceEvidence{}, errors.New("unbound proof input")
	}
	if g.onProof != nil {
		g.onProof()
	}
	return g.proof, g.proofErr
}

func (g *releaseBuildRemote) WalkReleaseSource(ctx context.Context, token, repository, branch string, lookBack int) (githubapp.ReleaseSourceWalk, error) {
	watchGitHubCall(g.t, ctx, "WalkReleaseSource")
	g.walks++
	if token == "" || repository != repoName || branch != "main" || lookBack != releaseSourceLookBack {
		return githubapp.ReleaseSourceWalk{}, errors.New("unbound walk input")
	}
	return g.walk, g.walkErr
}

func configureReleaseBuild(t *testing.T, h *harness) (pipelines.ReleaseBuildRequest, *releaseBuildRemote) {
	t.Helper()
	p := testPipeline(DeliveryWebhook)
	h.store.addPipeline(p)
	h.store.addPackage(PackageSource{ID: packageID, OwnerUserID: p.OwnerUserID, AccountID: p.AccountID, SourceKind: "repo", RepoURL: "https://github.com/" + repoName, CredentialID: credID, Status: "active"})
	h.github.repos[repoName] = githubapp.RepositoryInfo{ID: 123, FullName: repoName, DefaultBranch: "main"}
	h.github.commits[repoName+"@refs/tags/v1.4.0"] = headAnswer{SHA: shaA}
	g := &releaseBuildRemote{fakeGitHub: h.github, proof: githubapp.ReleaseSourceEvidence{RepositoryID: 123, Repository: repoName, Branch: "main", HeadSHA: shaA, SHA: shaA, TreeSHA: shaB, ProofSHA: shaA, Checks: []githubapp.ReleaseSourceCheck{{Context: "ci-required", AppID: 42, SHA: shaA, Status: "completed", Conclusion: "success"}}},
		walk: githubapp.ReleaseSourceWalk{RepositoryID: 123, Repository: repoName, Branch: "main", HeadSHA: shaA, LookBack: releaseSourceLookBack, Pick: shaA}}
	h.integ.Configure(func(d *Deps) { d.GitHub = g })
	return pipelines.ReleaseBuildRequest{PipelineID: p.ID, SourceID: packageID, CredentialID: credID, Repository: repoName, RepositoryID: 123, InstallationID: p.InstallationID, Branch: "main", ExpectedSHA: shaA, Tag: "v1.4.0"}, g
}

func TestReleaseBuildInspectionMakesNoRunAndRetainsConflicts(t *testing.T) {
	h := newHarness(t)
	r, _ := configureReleaseBuild(t, h)
	initial := pipelines.ReleaseBuildRequest{PipelineID: r.PipelineID, Repository: r.Repository, Branch: r.Branch}
	source, err := h.integ.InspectReleaseBuild(personCtx(ownerID), initial)
	if err != nil || source.SHA != shaA || source.SourceID != packageID || source.CredentialID != credID || source.RepositoryID != 123 || source.OwnerUserID != ownerID {
		t.Fatalf("initial proof: %+v %v", source, err)
	}
	h.github.commits[repoName+"@refs/tags/v1.4.0"] = headAnswer{SHA: shaC}
	source, err = h.integ.InspectReleaseBuild(personCtx(ownerID), r)
	if err != nil || !source.TagExists || source.TagSHA != shaC {
		t.Fatalf("tag collision was not exposed: %+v %v", source, err)
	}
	if _, err = h.integ.StartReleaseBuild(personCtx(ownerID), r, allowReleaseBuild); err == nil {
		t.Fatal("different tag source accepted")
	}
	if len(h.github.created) != 0 || len(h.store.allRuns()) != 0 {
		t.Fatal("inspection or refused start created effects")
	}
}

// Preparing a new version reads a tag that does not exist yet, and GitHub
// answers that read with 422, not 404. Reading only 404 as absence refused
// every release whose tag had not been made (0.31.4's first source check).
func TestReleaseBuildInspectionReadsAMissingTagAsAbsent(t *testing.T) {
	h := newHarness(t)
	r, g := configureReleaseBuild(t, h)
	delete(g.commits, repoName+"@refs/tags/v1.4.0")
	source, err := h.integ.InspectReleaseBuild(personCtx(ownerID), r)
	if err != nil || source.TagExists || source.TagSHA != "" || source.SHA != shaA {
		t.Fatalf("missing tag was not read as absent: %+v %v", source, err)
	}
}

// An unbound inspection -- release preparation's first read, which names no
// commit -- walks main and proves what the walk chose. A bound one re-proves
// the commit it names and walks nothing, so every later gate checks the commit
// the saved release chose rather than choosing again (design record
// 2026-11-05-owner-pushes-main-design.md, D6).
func TestReleaseBuildInspectionWalksOnlyWhenUnbound(t *testing.T) {
	h := newHarness(t)
	r, g := configureReleaseBuild(t, h)
	g.walk.HeadSHA, g.proof.HeadSHA = shaC, shaC
	g.walk.Newer = []githubapp.ReleaseSourceSkip{{SHA: shaC, State: githubapp.ReleaseSourcePending, Check: "ci-required"}}
	initial := pipelines.ReleaseBuildRequest{PipelineID: r.PipelineID, Repository: r.Repository, Branch: r.Branch}
	source, err := h.integ.InspectReleaseBuild(personCtx(ownerID), initial)
	if err != nil || source.SHA != shaA || source.HeadSHA != shaC || g.walks != 1 || !reflect.DeepEqual(g.proved, []string{shaA}) {
		t.Fatalf("unbound inspection: %+v %v (walks %d, proved %v)", source, err, g.walks, g.proved)
	}
	want := &pipelines.ReleaseSourceWalk{HeadSHA: shaC, LookBack: releaseSourceLookBack, Newer: []pipelines.ReleaseSourceSkip{{SHA: shaC, State: pipelines.ReleaseSourcePending, Check: "ci-required"}}}
	if !reflect.DeepEqual(source.Walk, want) {
		t.Fatalf("recorded walk:\n got %+v\nwant %+v", source.Walk, want)
	}
	bound, err := h.integ.InspectReleaseBuild(personCtx(ownerID), r)
	if err != nil || bound.SHA != shaA || bound.Walk != nil || g.walks != 1 {
		t.Fatalf("a bound inspection walked: %+v %v (walks %d)", bound, err, g.walks)
	}
	if _, err := h.integ.StartReleaseBuild(personCtx(ownerID), r, allowReleaseBuild); err != nil || g.walks != 1 {
		t.Fatalf("a start walked: %v (walks %d)", err, g.walks)
	}
}

// A walk that passed nothing over chose the head, and records nothing: its
// source encodes exactly as a release of the head did before walks existed.
func TestReleaseBuildInspectionOfAGreenHeadRecordsNoWalk(t *testing.T) {
	h := newHarness(t)
	r, g := configureReleaseBuild(t, h)
	source, err := h.integ.InspectReleaseBuild(personCtx(ownerID), pipelines.ReleaseBuildRequest{PipelineID: r.PipelineID, Repository: r.Repository, Branch: r.Branch})
	if err != nil || source.SHA != shaA || source.Walk != nil || g.walks != 1 {
		t.Fatalf("green head: %+v %v", source, err)
	}
}

// Nothing green within the look back is a typed answer carrying what the walk
// found, reached before any proof is attempted. A pick the proof refuses is
// that refusal: the inspection never walks on past it, and a retry walks again.
func TestReleaseBuildInspectionAnswersAnUntestedSourceAndAProofsRefusal(t *testing.T) {
	h := newHarness(t)
	r, g := configureReleaseBuild(t, h)
	initial := pipelines.ReleaseBuildRequest{PipelineID: r.PipelineID, Repository: r.Repository, Branch: r.Branch}
	g.walk.Pick = ""
	g.walk.Newer = []githubapp.ReleaseSourceSkip{{SHA: shaA, State: githubapp.ReleaseSourcePending, Check: "ci-required"}, {SHA: shaB, State: githubapp.ReleaseSourceMissing, Check: "ci-required"}}
	_, err := h.integ.InspectReleaseBuild(personCtx(ownerID), initial)
	var untested *pipelines.ReleaseSourceUntestedError
	if !errors.As(err, &untested) || untested.Branch != "main" || untested.Walk.HeadSHA != shaA || len(untested.Walk.Newer) != 2 || untested.Walk.Newer[1].State != pipelines.ReleaseSourceMissing || len(g.proved) != 0 {
		t.Fatalf("untested source: %v (proved %v)", err, g.proved)
	}

	h = newHarness(t)
	r, g = configureReleaseBuild(t, h)
	g.walk.HeadSHA = shaC
	g.walk.Newer = []githubapp.ReleaseSourceSkip{{SHA: shaC, State: githubapp.ReleaseSourceFailed, Check: "ci-required", Conclusion: "failure"}}
	g.proofErr = errors.New("required release check \"ci-required\" has not succeeded")
	_, err = h.integ.InspectReleaseBuild(personCtx(ownerID), initial)
	if err == nil || !errors.Is(err, g.proofErr) && err.Error() != g.proofErr.Error() || g.walks != 1 || !reflect.DeepEqual(g.proved, []string{shaA}) {
		t.Fatalf("refused pick: %v (walks %d, proved %v)", err, g.walks, g.proved)
	}
	if _, err = h.integ.InspectReleaseBuild(personCtx(ownerID), initial); err == nil || g.walks != 2 {
		t.Fatalf("a retry did not walk again: %v (walks %d)", err, g.walks)
	}
}

// What the walk says is checked before it is believed or recorded, and a walk
// that misdescribes its repository, branch, bound or commits proves nothing.
func TestReleaseBuildInspectionRefusesAWalkItCannotBelieve(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alter func(*releaseBuildRemote)
	}{
		{"walk failed", func(g *releaseBuildRemote) { g.walkErr = errors.New("GitHub truncated release check evidence") }},
		{"another repository", func(g *releaseBuildRemote) { g.walk.Repository = "acme/other" }},
		{"another branch", func(g *releaseBuildRemote) { g.walk.Branch = "develop" }},
		{"another look back", func(g *releaseBuildRemote) { g.walk.LookBack = 5 }},
		{"pick past the look back", func(g *releaseBuildRemote) {
			g.walk.HeadSHA = shaC
			for n := 0; n < releaseSourceLookBack; n++ {
				g.walk.Newer = append(g.walk.Newer, githubapp.ReleaseSourceSkip{SHA: fmt.Sprintf("%040x", n+1), State: githubapp.ReleaseSourceMissing, Check: "ci-required"})
			}
			g.walk.Newer[0].SHA = shaC
		}},
		{"no repository identity", func(g *releaseBuildRemote) { g.walk.RepositoryID = 0 }},
		{"another repository identity than the proof", func(g *releaseBuildRemote) { g.walk.RepositoryID = 999 }},
		{"pick is not a commit", func(g *releaseBuildRemote) { g.walk.Pick = "main" }},
		{"head is not a commit", func(g *releaseBuildRemote) { g.walk.HeadSHA = "HEAD" }},
		{"head passed over without being recorded", func(g *releaseBuildRemote) { g.walk.HeadSHA = shaC }},
		{"pick among the commits passed over", func(g *releaseBuildRemote) {
			g.walk.HeadSHA = shaA
			g.walk.Newer = []githubapp.ReleaseSourceSkip{{SHA: shaA, State: githubapp.ReleaseSourcePending, Check: "ci-required"}}
		}},
		{"unknown state", func(g *releaseBuildRemote) {
			g.walk.HeadSHA = shaC
			g.walk.Newer = []githubapp.ReleaseSourceSkip{{SHA: shaC, State: "green", Check: "ci-required"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			r, g := configureReleaseBuild(t, h)
			tc.alter(g)
			source, err := h.integ.InspectReleaseBuild(personCtx(ownerID), pipelines.ReleaseBuildRequest{PipelineID: r.PipelineID, Repository: r.Repository, Branch: r.Branch})
			if err == nil {
				t.Fatalf("an unbelievable walk was used: %+v", source)
			}
			if tc.name != "another repository identity than the proof" && len(g.proved) != 0 {
				t.Fatalf("a refused walk was proven: %v", g.proved)
			}
		})
	}
}

// osReleaseJourneyPath is the MemQL OS file whose Cut release screen tells a
// person how far checking the source looks back.
const osReleaseJourneyPath = "../../clients/os/src/apps/cluster/releases/journey.ts"

var osSourceLookBack = regexp.MustCompile(`export const SOURCE_LOOK_BACK = (\d+);`)

// The OS says the number in words before anyone checks a source, so its copy
// must be the engine's bound, not a guess at it.
func TestReleaseSourceLookBackMatchesTheOS(t *testing.T) {
	raw, err := os.ReadFile(osReleaseJourneyPath)
	if err != nil {
		t.Fatalf("the OS release journey is unreadable at %s: %v\nIf it moved, update osReleaseJourneyPath.", osReleaseJourneyPath, err)
	}
	m := osSourceLookBack.FindSubmatch(raw)
	if m == nil {
		t.Fatalf("%s does not export SOURCE_LOOK_BACK as a numeric literal, which this gate compares", osReleaseJourneyPath)
	}
	if n, err := strconv.Atoi(string(m[1])); err != nil || n != releaseSourceLookBack {
		t.Fatalf("the OS says checking the source looks back %s commits; the engine looks back %d", m[1], releaseSourceLookBack)
	}
}

func TestReleaseBuildRefusesChangedAuthorityBeforeOpening(t *testing.T) {
	for _, name := range []string{"foreign owner", "archived source", "changed source owner", "changed connection", "changed repository", "changed installation", "changed provider identity", "failed check", "missing check", "incomplete binding", "missing tag", "cancelled", "provider unavailable", "source moves during proof", "grant revoked during proof"} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			r, g := configureReleaseBuild(t, h)
			ctx := personCtx(ownerID)
			source := h.store.packages[packageID]
			switch name {
			case "foreign owner":
				ctx = clusterOwnerCtx(otherID)
			case "archived source":
				source.Status = "archived"
				h.store.addPackage(source)
			case "changed source owner":
				source.OwnerUserID = otherID
				h.store.addPackage(source)
			case "changed connection":
				source.CredentialID = "another-connection"
				h.store.addPackage(source)
			case "changed repository":
				source.RepoURL = "https://github.com/acme/other"
				h.store.addPackage(source)
			case "changed installation":
				g.installationID = 8
			case "changed provider identity":
				g.proof.RepositoryID = 999
			case "failed check":
				g.proof.Checks[0].Conclusion = "failure"
			case "missing check":
				g.proof.Checks = nil
			case "incomplete binding":
				r.CredentialID = ""
			case "missing tag":
				delete(g.commits, repoName+"@refs/tags/v1.4.0")
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "provider unavailable":
				g.proofErr = errors.New("provider unavailable")
			case "source moves during proof":
				g.onProof = func() { source.CredentialID = "changed"; h.store.addPackage(source) }
			case "grant revoked during proof":
				g.onProof = func() { g.tokenErr = errors.New("revoked") }
			}
			if _, err := h.integ.StartReleaseBuild(ctx, r, allowReleaseBuild); err == nil {
				t.Fatal("unsafe source opened")
			}
			if len(h.github.created) != 0 || len(h.store.allRuns()) != 0 {
				t.Fatal("refusal created check or run")
			}
		})
	}
}

func TestReleaseBuildRechecksSourceInsideTheOpeningGate(t *testing.T) {
	h := newHarness(t)
	r, _ := configureReleaseBuild(t, h)
	oldGate := h.integ.snapshot().Gate
	h.integ.Configure(func(d *Deps) {
		d.Gate = func(ctx context.Context, key string, fn func(context.Context) error) error {
			p, _ := h.store.pipeline(r.PipelineID)
			p.CredentialID = "changed"
			h.store.addPipeline(p)
			return oldGate(ctx, key, fn)
		}
	})
	if _, err := h.integ.StartReleaseBuild(personCtx(ownerID), r, allowReleaseBuild); err == nil {
		t.Fatal("source change at admission accepted")
	}
	if len(h.github.created) != 0 || len(h.store.allRuns()) != 0 {
		t.Fatal("source fence ran after the effect")
	}
}

func TestReleaseBuildObservesAndDrivesSameRunOnAnotherReplica(t *testing.T) {
	const manifest = `formatVersion: 1
name: shop
pipeline:
  image: ghcr.io/acme/toolchain@sha256:abc
  stages:
    - name: images
      on: [release]
      steps:
        - name: build
          run: build-release
`
	dh := newDriveHarness(t, manifest)
	r, _ := configureReleaseBuild(t, dh.harness)
	p, _ := dh.store.pipeline(r.PipelineID)
	p.Status = PipelineDisconnected
	dh.store.addPipeline(p)
	first, err := dh.integ.StartReleaseBuild(personCtx(ownerID), r, allowReleaseBuild)
	if err != nil {
		t.Fatal(err)
	}
	peer := New(dh.integ.snapshot())
	dh.configureDriver(peer, "agent-b")
	observed, found, err := peer.ObserveReleaseBuild(personCtx(ownerID), r)
	if err != nil || !found || observed.ID != first.ID || len(dh.github.created) != 1 {
		t.Fatalf("peer observation: %+v %v %v", observed, found, err)
	}
	again, err := peer.StartReleaseBuild(personCtx(ownerID), r, allowReleaseBuild)
	if err != nil || again.ID != first.ID || len(dh.github.created) != 1 {
		t.Fatalf("peer dedup: %+v %v", again, err)
	}
	native, ok := dh.store.run(first.ID)
	if !ok {
		t.Fatal("native run missing")
	}
	deliver(t, peer, native)
	observed, found, err = peer.ObserveReleaseBuild(personCtx(ownerID), r)
	if err != nil || !found || observed.Conclusion != ConclusionSuccess || observed.WorkRunID == "" || observed.Tag != r.Tag {
		t.Fatalf("peer did not complete exact release: %+v %v", observed, err)
	}
	sent := dh.exec.sent()
	if len(sent) != 1 || sent[0].Event != pipelines.EventRelease || sent[0].Version != r.Tag || sent[0].SHA != r.ExpectedSHA {
		t.Fatalf("wrong execution identity: %+v", sent)
	}
	p, _ = dh.store.pipeline(r.PipelineID)
	if p.Status != PipelineDisconnected {
		t.Fatal("release start enabled automatic intake")
	}
}

func TestReleaseBuildSameSourceDifferentTagCannotCreateAnotherRun(t *testing.T) {
	h := newHarness(t)
	r, _ := configureReleaseBuild(t, h)
	first, err := h.integ.StartReleaseBuild(personCtx(ownerID), r, allowReleaseBuild)
	if err != nil {
		t.Fatal(err)
	}
	r.Tag = "v1.5.0"
	h.github.commits[repoName+"@refs/tags/"+r.Tag] = headAnswer{SHA: r.ExpectedSHA}
	proof, err := h.integ.InspectReleaseBuild(personCtx(ownerID), r)
	if err != nil || proof.ExistingRun == nil || proof.ExistingRun.ID != first.ID || proof.ExistingRun.Tag != "v1.4.0" {
		t.Fatalf("missing recorded collision: %+v %v", proof, err)
	}
	if _, err := h.integ.StartReleaseBuild(personCtx(ownerID), r, allowReleaseBuild); err == nil {
		t.Fatal("different tag reopened same release source")
	}
	if _, _, err := h.integ.ObserveReleaseBuild(personCtx(ownerID), r); err == nil {
		t.Fatal("different tag pretended to observe selected release")
	}
	if len(h.github.created) != 1 || len(h.store.allRuns()) != 1 {
		t.Fatal("different tag created another effect")
	}
}

func TestReleaseBuildNativeAuthorityFenceRunsAfterReadsAndUnderGate(t *testing.T) {
	for _, refuseAt := range []int{0, 1, 2} {
		t.Run(fmt.Sprint(refuseAt), func(t *testing.T) {
			h := newHarness(t)
			r, g := configureReleaseBuild(t, h)
			observed := false
			calls := 0
			g.onProof = func() { observed = true }
			var fence func(context.Context) error
			if refuseAt > 0 {
				fence = func(ctx context.Context) error {
					calls++
					if !observed {
						t.Fatal("authority fence did not follow provider observation")
					}
					_, held := gateHeldOn(ctx)
					if held != (calls == 2) {
						t.Fatal("final authority fence is not at the ordinary opening gate")
					}
					if calls == refuseAt {
						return errors.New("native release lease or profile revoked")
					}
					return nil
				}
			}
			if _, err := h.integ.StartReleaseBuild(personCtx(ownerID), r, fence); err == nil {
				t.Fatal("missing or revoked native authority accepted")
			}
			if calls != refuseAt || len(h.github.created) != 0 || len(h.store.allRuns()) != 0 {
				t.Fatalf("late authority refusal: calls %d checks %d runs %d", calls, len(h.github.created), len(h.store.allRuns()))
			}
		})
	}
}

// releaseAttempt is one attempt of the release run key these chain tests share.
func releaseAttempt(id string, attempt int, rerunOf string) Run {
	return Run{ID: id, RunKey: "release-key", Attempt: attempt, RerunOf: rerunOf, Event: pipelines.EventRelease, Mode: pipelines.ModeFull,
		Status: StatusCompleted, Conclusion: ConclusionFailure, Trigger: TriggerManual}
}

// TestReleaseAttemptChainFollowsRerunOf: a release run's chain is every earlier
// attempt of its run key, newest first, read through rerunOf alone. A GitHub
// "Re-run" (trigger rerun) belongs to the chain like an owner's re-run.
func TestReleaseAttemptChainFollowsRerunOf(t *testing.T) {
	a1 := releaseAttempt("a1", 1, "")
	a2 := releaseAttempt("a2", 2, "a1")
	a2.Trigger, a2.Conclusion, a2.CancelRequested = TriggerRerun, ConclusionCancelled, true
	a3 := releaseAttempt("a3", 3, "v1:pipelines:run:a2")
	a3.Status, a3.Conclusion = StatusInProgress, ConclusionNone
	byID := map[string]Run{"a1": a1, "a2": a2, "a3": a3}
	want := []pipelines.ReleaseBuildAttempt{
		{ID: "a2", Attempt: 2, Status: StatusCompleted, Conclusion: ConclusionCancelled, CancelRequested: true},
		{ID: "a1", Attempt: 1, Status: StatusCompleted, Conclusion: ConclusionFailure},
	}
	if got := releaseAttemptChain(a3, byID); !reflect.DeepEqual(got, want) {
		t.Fatalf("chain of attempt 3:\n got %+v\nwant %+v", got, want)
	}
	if got := releaseAttemptChain(a1, byID); got != nil {
		t.Fatalf("a first attempt has no earlier attempts: %+v", got)
	}
}

// TestReleaseAttemptChainIsEmptyWhenBroken: a chain that cannot be followed to
// attempt 1 inside its own run key is no chain at all, so nothing adopts it.
func TestReleaseAttemptChainIsEmptyWhenBroken(t *testing.T) {
	long := func() (Run, map[string]Run) {
		byID := map[string]Run{}
		for n := 1; n <= 51; n++ {
			rerunOf := ""
			if n > 1 {
				rerunOf = fmt.Sprintf("a%d", n-1)
			}
			byID[fmt.Sprintf("a%d", n)] = releaseAttempt(fmt.Sprintf("a%d", n), n, rerunOf)
		}
		return byID["a51"], byID
	}
	for name, build := range map[string]func() (Run, map[string]Run){
		"missing link": func() (Run, map[string]Run) {
			a2 := releaseAttempt("a2", 2, "gone")
			return a2, map[string]Run{"a2": a2}
		},
		"cycle": func() (Run, map[string]Run) {
			a2, a3 := releaseAttempt("a2", 2, "a3"), releaseAttempt("a3", 3, "a2")
			return a3, map[string]Run{"a2": a2, "a3": a3}
		},
		"other run key": func() (Run, map[string]Run) {
			a1, a2 := releaseAttempt("a1", 1, ""), releaseAttempt("a2", 2, "a1")
			a1.RunKey = "push-key"
			return a2, map[string]Run{"a1": a1, "a2": a2}
		},
		"attempt does not decrease": func() (Run, map[string]Run) {
			a1, a2 := releaseAttempt("a1", 2, ""), releaseAttempt("a2", 2, "a1")
			return a2, map[string]Run{"a1": a1, "a2": a2}
		},
		"ends above attempt 1": func() (Run, map[string]Run) {
			a2, a3 := releaseAttempt("a2", 2, ""), releaseAttempt("a3", 3, "a2")
			return a3, map[string]Run{"a2": a2, "a3": a3}
		},
		"more than 50 attempts": long,
	} {
		t.Run(name, func(t *testing.T) {
			latest, byID := build()
			if got := releaseAttemptChain(latest, byID); got != nil {
				t.Fatalf("a broken chain was followed: %+v", got)
			}
		})
	}
}
