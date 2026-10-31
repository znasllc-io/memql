package pipelinerun

import (
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/pipelines"
)

var (
	treeOne = strings.Repeat("7", 40)
	treeTwo = strings.Repeat("8", 40)
)

// Every run keeps the git tree of its commit, on its row and in its work input,
// before any step runs. The row is how a release finds the run; the work input
// enters every receipt's digest, so a receipt commits to the tree it tested.
func TestARunRecordsTheTreeOfItsCommit(t *testing.T) {
	for name, opening := range map[string]Opening{"pull request": prOpening(), "push": pushOpening()} {
		t.Run(name, func(t *testing.T) {
			dh := newDriveHarness(t, driveManifest)
			dh.github.commitTrees[repoName+"@"+shaA] = treeOne
			run := dh.openRun(t, opening)
			deliver(t, dh.integ, run)
			got, _ := dh.store.run(run.ID)
			if got.Conclusion != ConclusionSuccess || got.TreeHash != treeOne {
				t.Fatalf("run = %s, tree %q; want success on %s", got.Conclusion, got.TreeHash, treeOne)
			}
			input, _ := dh.work.run(got.WorkRunID)["input"].(map[string]any)
			if input["treeHash"] != treeOne {
				t.Fatalf("the work input does not name the tree: %v", input)
			}
		})
	}
}

// A tree that cannot be read costs a run its reuse and nothing else: refusing a
// pull request's checks because an optional read failed would be the worse
// failure. The run has no tree, so a later release cannot stand on it.
func TestATreeThatCannotBeReadDoesNotFailTheRun(t *testing.T) {
	for name, fail := range map[string]func(*fakeGitHub){
		"the read fails":     func(g *fakeGitHub) { g.commitTreeErr = errors.New("GitHub answered 500") },
		"the hash is broken": func(g *fakeGitHub) { g.commitTrees[repoName+"@"+shaA] = "tree" },
	} {
		t.Run(name, func(t *testing.T) {
			dh := newDriveHarness(t, driveManifest)
			fail(dh.github)
			run := dh.openRun(t, pushOpening())
			deliver(t, dh.integ, run)
			got, _ := dh.store.run(run.ID)
			if got.Conclusion != ConclusionSuccess || got.TreeHash != "" {
				t.Fatalf("run = %s, tree %q; want success with no tree", got.Conclusion, got.TreeHash)
			}
			input, _ := dh.work.run(got.WorkRunID)["input"].(map[string]any)
			if _, named := input["treeHash"]; named {
				t.Fatalf("the work input names a tree nobody read: %v", input)
			}
			for _, row := range dh.work.stepRows(bareID(got.WorkRunID)) {
				if _, named := argObject(row, "call")["treeFingerprint"]; named {
					t.Fatalf("a step offers reuse on an unknown tree: %v", row)
				}
			}
		})
	}
}

func treeDriver(run Run) *runDriver {
	return &runDriver{
		d:   Deps{EngineRevision: func() string { return "engine" }},
		p:   Pipeline{ID: "pipeline", OwnerUserID: "owner", Repository: repoName},
		run: run,
	}
}

func treeStep() pipelines.Step {
	return pipelines.Step{Key: "tests.unit", Stage: "tests", Name: "unit", Kind: pipelines.StepCommand, Verification: true,
		Run: "go test ./unit", Image: "registry/toolchain@sha256:" + strings.Repeat("a", 64), Packages: []string{"a"}}
}

// The fingerprint says "the same check on the same tree": equal across commits,
// events, modes and versions, and across what orders the plan.
func TestTreeFingerprintIsTheSameCheckOnTheSameTree(t *testing.T) {
	want := treeDriver(Run{SHA: shaA, TreeHash: treeOne, Event: pipelines.EventPullRequest, Mode: pipelines.ModeAffected}).treeFingerprintOf(treeStep())
	if want == "" {
		t.Fatal("a verification step on a known tree has no fingerprint")
	}
	release := Run{SHA: shaB, TreeHash: treeOne, Event: pipelines.EventRelease, Mode: pipelines.ModeFull, Version: "v1.4.0", InboundRequestID: "request"}
	if got := treeDriver(release).treeFingerprintOf(treeStep()); got != want {
		t.Fatalf("a release of the same tree differs: %s, want %s", got, want)
	}
	// A pull request plan and a release plan put different stages before a step,
	// so dependsOn differs on one tree. It orders the plan; the command never
	// sees it.
	moved := treeStep()
	moved.DependsOn = []string{"secrets.gitleaks"}
	if got := treeDriver(release).treeFingerprintOf(moved); got != want {
		t.Fatal("what orders the plan changed the fingerprint")
	}
	skipped := treeStep()
	skipped.Skip = &pipelines.Skip{Code: pipelines.CodePassedEarlier, Reason: "Passed."}
	if got := treeDriver(release).treeFingerprintOf(skipped); got != want {
		t.Fatal("a planned skip changed the fingerprint")
	}

	for name, change := range map[string]func(*runDriver, *pipelines.Step){
		"tree":     func(dr *runDriver, _ *pipelines.Step) { dr.run.TreeHash = treeTwo },
		"engine":   func(dr *runDriver, _ *pipelines.Step) { dr.d.EngineRevision = func() string { return "other" } },
		"pipeline": func(dr *runDriver, _ *pipelines.Step) { dr.p.ID = "other" },
		"owner":    func(dr *runDriver, _ *pipelines.Step) { dr.p.OwnerUserID = "other" },
		"source":   func(dr *runDriver, _ *pipelines.Step) { dr.p.Repository = "acme/other" },
		"compute":  func(dr *runDriver, _ *pipelines.Step) { dr.p.Compute = pipelines.Compute("other") },
		"domain":   func(dr *runDriver, _ *pipelines.Step) { dr.d.Domain = func() string { return "other.test" } },
		"workflow": func(dr *runDriver, _ *pipelines.Step) { dr.workflow = &workflowProgram{fingerprint: "other"} },
		"command":  func(_ *runDriver, s *pipelines.Step) { s.Run = "go test ./other" },
		"image":    func(_ *runDriver, s *pipelines.Step) { s.Image = "registry/other@sha256:" + strings.Repeat("b", 64) },
		"packages": func(_ *runDriver, s *pipelines.Step) { s.Packages = []string{"b"} },
		"shard":    func(_ *runDriver, s *pipelines.Step) { s.Shard = pipelines.ShardRef{Index: 1, Count: 2} },
	} {
		t.Run("binds "+name, func(t *testing.T) {
			dr, step := treeDriver(release), treeStep()
			change(dr, &step)
			if got := dr.treeFingerprintOf(step); got == "" || got == want {
				t.Fatalf("a different %s kept the fingerprint: %q", name, got)
			}
		})
	}
	for name, change := range map[string]func(*Run, *pipelines.Step){
		"no tree":       func(r *Run, _ *pipelines.Step) { r.TreeHash = "" },
		"not declared":  func(_ *Run, s *pipelines.Step) { s.Verification = false },
		"not a command": func(_ *Run, s *pipelines.Step) { s.Kind = pipelines.StepNotify },
		"needs history": func(_ *Run, s *pipelines.Step) { s.CheckoutHistory = "full" },
	} {
		t.Run("none when "+name, func(t *testing.T) {
			run, step := release, treeStep()
			change(&run, &step)
			if got := treeDriver(run).treeFingerprintOf(step); got != "" {
				t.Fatalf("got fingerprint %q", got)
			}
		})
	}
}
