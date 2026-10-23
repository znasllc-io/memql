package pipelinerun

import (
	"context"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/component/workjournal"
)

func TestVerificationFingerprintPreservesTheLiveDriver(t *testing.T) {
	dr := &runDriver{
		d:        Deps{EngineRevision: func() string { return "engine-revision" }, Domain: func() string { return "example.test" }},
		p:        Pipeline{ID: "pipeline", OwnerUserID: "owner", Repository: "example/project"},
		run:      Run{SHA: shaA, Event: pipelines.EventMergeGroup, Mode: pipelines.ModeFull, Version: "1.2.3", InboundRequestID: "inbound"},
		workflow: &workflowProgram{fingerprint: "captured-workflow"},
	}
	step := pipelines.Step{Key: "tests.unit", Kind: pipelines.StepCommand, Run: "go test ./unit", Packages: []string{"example/unit"}, Verification: true}
	original := dr.run
	full := dr.definitionOf(step)
	verification := dr.verificationOf(step)
	if verification == "" || verification == full {
		t.Fatal("verification must normalize the run-only facts while retaining an execution definition")
	}
	want := &runDriver{d: dr.d, p: dr.p, run: Run{SHA: shaA}, workflow: dr.workflow}
	if verification != want.definitionOf(step) {
		t.Fatal("verification changed more than the event, mode, version and inbound request")
	}
	want.run.SHA = shaB
	if verification == want.verificationOf(step) {
		t.Fatal("verification lost the exact source revision")
	}
	// The journal handle can be published by another goroutine. Under -race,
	// this exercises the old whole-driver copy against atomic stores, rather
	// than merely asserting the spelling of the implementation.
	first, latest := &workjournal.Run{}, &workjournal.Run{}
	dr.workRun.Store(first)
	dr.journalFailed.Store(true)
	dr.cleanupFailed.Store(true)
	ready, stop, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		close(ready)
		for {
			select {
			case <-stop:
				dr.workRun.Store(latest)
				return
			default:
				dr.workRun.Store(first)
				dr.workRun.Store(latest)
				runtime.Gosched()
			}
		}
	}()
	<-ready
	stable := true
	for range 256 {
		stable = stable && dr.verificationOf(step) == verification
	}
	close(stop)
	<-done
	if !stable || !reflect.DeepEqual(dr.run, original) || dr.work() != latest || !dr.journalFailed.Load() || !dr.cleanupFailed.Load() {
		t.Fatal("fingerprinting changed the live run, journal ownership or failure state")
	}
	step.Verification = false
	if dr.verificationOf(step) != "" {
		t.Fatal("ordinary work must not acquire reusable verification evidence")
	}
}

func TestRecoveryRequiresTheSameExecutionDefinition(t *testing.T) {
	for _, change := range []string{"command", "image", "memory reservation", "CPU reservation", "checkout history", "registry credential", "failure policy", "engine", "domain", "unknown engine", "dirty engine"} {
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
			case "checkout history":
				dh.setTree(shaA, strings.Replace(driveManifest, "run: go test ./...", "run: go test ./...\n          checkoutHistory: full", 1))
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
