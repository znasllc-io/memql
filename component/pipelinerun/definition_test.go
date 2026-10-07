package pipelinerun

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/pipelines"
)

func TestRecoveryRequiresTheSameExecutionDefinition(t *testing.T) {
	for _, change := range []string{"command", "image", "memory reservation", "CPU reservation", "registry credential", "failure policy", "engine", "domain", "unknown engine", "dirty engine"} {
		t.Run(change, func(t *testing.T) {
			dh := newDriveHarness(t, driveManifest)
			release := make(chan struct{})
			blockTests(dh, release)
			run := dh.openRun(t, prOpening())
			dh.integ.HandleRunEvent(graphEvent(events.TopicGraphNodeCreated, run))
			dh.awaitEntered(t, "checks.vet", "tests.unit", "tests.lint")
			before := dh.exec.sentKeys()
			other := dh.peer("agent-b")
			switch change {
			case "command":
				dh.setTree(shaA, strings.Replace(driveManifest, "run: go test ./...", "run: echo changed", 1))
			case "image":
				dh.setTree(shaA, strings.Replace(driveManifest, "sha256:abc", "sha256:def", 1))
			case "memory reservation":
				dh.setTree(shaA, strings.Replace(driveManifest, "run: go test ./...", "run: go test ./...\n          memoryMiB: 6144", 1))
			case "CPU reservation":
				dh.setTree(shaA, strings.Replace(driveManifest, "run: go test ./...", "run: go test ./...\n          cpuMilli: 4000", 1))
			case "registry credential":
				dh.setTree(shaA, strings.Replace(driveManifest, "secrets: [SHOP_TOKEN]", "imagePullSecret: SHOP_TOKEN", 1))
			case "failure policy":
				dh.setTree(shaA, strings.Replace(driveManifest, "- name: tests", "- name: tests\n      runAfterFailure: true", 1))
			case "domain":
				other.Configure(func(d *Deps) { d.Domain = func() string { return "different.example" } })
			default:
				revision := map[string]string{"engine": "different-engine", "unknown engine": "", "dirty engine": "revision-dirty"}[change]
				other.Configure(func(d *Deps) { d.EngineRevision = func() string { return revision } })
			}
			dh.store.mu.Lock()
			stored := dh.store.runs[run.ID]
			stored.DriverHeartbeatAt = testNow.Add(-3 * time.Minute)
			dh.store.runs[run.ID] = stored
			dh.store.mu.Unlock()
			if err := other.RecoverRuns(context.Background()); err != nil {
				t.Fatal(err)
			}
			waitDrives(t, other)
			close(release)
			waitDrives(t, dh.integ)
			if got := dh.exec.sentKeys(); !slices.Equal(got, before) {
				t.Fatalf("replacement executed changed work: %v", got)
			}
			got, _ := dh.store.run(run.ID)
			if got.Conclusion != ConclusionFailure {
				t.Fatalf("changed definition concluded %s", got.Conclusion)
			}
			if receipts := dh.work.receiptsOf("checks.vet"); len(receipts) != 1 || argString(receipts[0].Args, "status") != WorkStepDone {
				t.Fatal("a confirmed receipt was changed while refusing recovery")
			}
		})
	}
}

func TestResumeRefusesMissingDuplicateAndReorderedDefinitions(t *testing.T) {
	for _, change := range []string{"missing identity", "missing row", "duplicate", "reordered"} {
		t.Run(change, func(t *testing.T) {
			dr := &runDriver{d: Deps{EngineRevision: func() string { return "engine" }}}
			step := pipelines.Step{Key: "build.one", Kind: pipelines.StepCommand, Run: "build", Packages: []string{"a"}}
			dr.buildTracks(pipelines.Plan{Stages: []pipelines.PlanStage{{Name: "build", Steps: []pipelines.Step{step}}}})
			rows := []WorkStep{{Key: step.Key, DefinitionFingerprint: dr.definitionOf(step), Packages: step.Packages}}
			switch change {
			case "missing identity":
				rows[0].DefinitionFingerprint = ""
			case "missing row":
				rows = nil
			case "duplicate":
				rows = append(rows, rows[0])
			case "reordered":
				rows[0].Seq = 1
			}
			if why := dr.resume(rows); why == "" {
				t.Fatal("unproven plan accepted")
			}
		})
	}
}

func TestFailedOnlyRerunDoesNotCarryAChangedOrUnprovenDefinition(t *testing.T) {
	for _, recorded := range []string{"", "different"} {
		track := &stepTrack{step: pipelines.Step{Key: "build.one"}, definition: "current"}
		carryPassed([]*stepTrack{track}, []WorkStep{{Key: track.step.Key, Status: WorkStepDone, DefinitionFingerprint: recorded}}, 1)
		if track.step.Skip != nil {
			t.Fatal("previous success authorized a changed or unknown command")
		}
	}
}
