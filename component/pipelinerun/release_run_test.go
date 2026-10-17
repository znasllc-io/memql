package pipelinerun

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/znasllc-io/memql/component/pipelines"
)

func TestRequestedReleaseBuildPinsTheTagAndDeduplicatesWithItsWebhook(t *testing.T) {
	h := newHarness(t)
	p := testPipeline(DeliveryWebhook)
	h.store.addPipeline(p)
	h.github.commits[repoName+"@refs/tags/v1.4.0"] = headAnswer{SHA: shaC}
	run, err := h.integ.RunRelease(personCtx(ownerID), p.ID, "v1.4.0")
	if err != nil {
		t.Fatal(err)
	}
	if run.SHA != shaC || run.Version != "v1.4.0" || run.Event != pipelines.EventRelease || run.Mode != pipelines.ModeFull || run.Trigger != TriggerManual {
		t.Fatalf("release request lost its source/version/event: %+v", run)
	}
	again, err := h.integ.RunRelease(personCtx(ownerID), p.ID, "v1.4.0")
	if err != nil || again.ID != run.ID {
		t.Fatalf("retry: %+v %v", again, err)
	}
	res := trigger(t, h, "release", "published-later", releaseDelivery(t, "v1.4.0", testInstallation))
	if len(res.Opened) != 0 || len(res.Existing) != 1 || res.Existing[0].ID != run.ID || len(h.github.created) != 1 {
		t.Fatalf("later publication created duplicate release work: %+v; checks %d", res, len(h.github.created))
	}
	h.github.commits[repoName+"@refs/tags/v1.5.0"] = headAnswer{SHA: shaC}
	if _, err := h.integ.RunRelease(personCtx(ownerID), p.ID, "v1.5.0"); err == nil || !strings.Contains(err.Error(), "recorded version") {
		t.Fatalf("another tag must not silently reuse a differently stamped image: %v", err)
	}
}

func TestReleaseRequestRefusesBeforeWritingACheck(t *testing.T) {
	for _, name := range []string{"stranger", "missing connection", "changed installation", "missing tag", "malformed SHA", "branch syntax", "empty tag"} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			p := testPipeline(DeliveryWebhook)
			owner, tag, sha := ownerID, "v1.4.0", shaC
			switch name {
			case "stranger":
				owner = otherID
			case "missing connection":
				p.CredentialID = ""
			case "changed installation":
				h.github.installationID = 8
			case "malformed SHA":
				sha = "not-a-commit"
			case "branch syntax":
				tag = "refs/heads/main"
			case "empty tag":
				tag = ""
			}
			h.store.addPipeline(p)
			if name != "missing tag" {
				h.github.commits[repoName+"@refs/tags/v1.4.0"] = headAnswer{SHA: sha}
			}
			if _, err := h.integ.RunRelease(personCtx(owner), p.ID, tag); err == nil {
				t.Fatal("unsafe release request accepted")
			}
			if len(h.github.created) != 0 {
				t.Fatal("refused release request wrote a check")
			}
			if name == "stranger" || name == "missing connection" || name == "branch syntax" || name == "empty tag" {
				if len(h.github.tokenMints) != 0 {
					t.Fatal("refused owner/pipeline/tag reached GitHub")
				}
			}
		})
	}
}

func TestRequestedReleaseBuildDrivesItsReleaseStagesOnAnotherReplica(t *testing.T) {
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
    - name: rehearsal
      on: [manual]
      steps:
        - name: check
          run: rehearse
`
	dh := newDriveHarness(t, manifest)
	// The request is explicit; automatic delivery stays disconnected so a
	// bootstrap neither subscribes to other PRs nor races a main push.
	p, _ := dh.store.pipeline(dh.p.ID)
	p.Status = PipelineDisconnected
	dh.store.addPipeline(p)
	dh.github.commits[repoName+"@refs/tags/v1.4.0"] = headAnswer{SHA: shaA}
	run, err := dh.integ.RunRelease(personCtx(ownerID), dh.p.ID, "v1.4.0")
	if err != nil {
		t.Fatal(err)
	}
	peer := dh.peer("agent-b")
	again, err := peer.RunRelease(personCtx(ownerID), dh.p.ID, "v1.4.0")
	if err != nil || again.ID != run.ID || len(dh.github.created) != 1 {
		t.Fatalf("another replica did not recover the release: %+v %v", again, err)
	}
	deliver(t, peer, run)
	got, _ := dh.store.run(run.ID)
	if got.Conclusion != ConclusionSuccess {
		t.Fatalf("release did not succeed on another replica: %+v", got)
	}
	for _, write := range append(dh.github.createdRuns(), dh.github.updatedRuns()...) {
		if write.Run.Name != "MemQL / shop / release" {
			t.Fatalf("a release check lost its separate context across the replica hop: %+v", write.Run)
		}
	}
	sent := dh.exec.sent()
	if len(sent) != 1 || sent[0].Event != pipelines.EventRelease || sent[0].Version != "v1.4.0" || sent[0].SHA != shaA || sent[0].Step.Run != "build-release" {
		t.Fatalf("release stages did not retain their source/version across the hop: %+v", sent)
	}
	p, _ = dh.store.pipeline(dh.p.ID)
	if p.Active() {
		t.Fatal("an explicit release request enabled automatic delivery")
	}
}

func TestAutomaticReleaseCannotStartAfterDeliveryIsDisconnected(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	run := dh.openRun(t, Opening{Event: pipelines.EventRelease, Mode: pipelines.ModeFull, SHA: shaA, ReleaseTag: "v1.4.0", Trigger: TriggerWebhook})
	p, _ := dh.store.pipeline(dh.p.ID)
	p.Status = PipelineDisconnected
	dh.store.addPipeline(p)
	deliver(t, dh.peer("agent-b"), run)
	got, _ := dh.store.run(run.ID)
	if got.Conclusion != ConclusionFailure || got.RefusalCode != pipelines.CodeDisconnected || len(dh.exec.sent()) != 0 {
		t.Fatalf("automatic release ran after disconnect: %+v; steps %v", got, dh.exec.sentKeys())
	}
}

func TestOwnerCanRerunADisconnectedReleaseOnAnotherReplica(t *testing.T) {
	for _, tc := range []struct {
		name, trigger string
		failedOnly    bool
	}{
		{"manual-full", TriggerManual, false},
		{"manual-failed", TriggerManual, true},
		{"webhook-full", TriggerWebhook, false},
		{"webhook-failed", TriggerWebhook, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dh := newDriveHarness(t, driveManifest)
			var fail atomic.Bool
			fail.Store(true)
			dh.exec.answer = func(_ context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
				result := passed(req)
				if fail.Load() {
					result.Status, result.ExitCode = pipelines.OutcomeFailed, 1
				}
				return result, nil
			}
			p, _ := dh.store.pipeline(dh.p.ID)
			if tc.trigger == TriggerManual {
				p.Status = PipelineDisconnected
				dh.store.addPipeline(p)
			}
			first := dh.openRun(t, Opening{Event: pipelines.EventRelease, Mode: pipelines.ModeFull, SHA: shaA, ReleaseTag: "v1.4.0", Trigger: tc.trigger})
			deliver(t, dh.peer("agent-first"), first)
			original, _ := dh.store.run(first.ID)
			if original.Conclusion != ConclusionFailure {
				t.Fatalf("first release must fail: %+v", original)
			}
			p.Status = PipelineDisconnected
			dh.store.addPipeline(p)
			if _, err := dh.integ.Rerun(personCtx(otherID), first.ID, false); err == nil {
				t.Fatal("another person reran the release")
			}
			fail.Store(false)
			next, err := dh.integ.Rerun(personCtx(ownerID), first.ID, tc.failedOnly)
			if err != nil {
				t.Fatalf("explicit release rerun: %v", err)
			}
			if next.Attempt != 2 || next.RerunOf != first.ID || next.SHA != shaA || next.Version != "v1.4.0" || next.Event != pipelines.EventRelease || next.Mode != pipelines.ModeFull || next.RerunFailedOnly != tc.failedOnly {
				t.Fatalf("rerun lost the pinned release identity: %+v", next)
			}
			if _, err := dh.integ.Rerun(personCtx(ownerID), first.ID, false); !errors.Is(err, ErrRunInProgress) {
				t.Fatalf("an active attempt must not be duplicated: %v", err)
			}
			sentBefore := len(dh.exec.sent())
			deliver(t, dh.peer("agent-retry"), next)
			got, _ := dh.store.run(next.ID)
			if got.Conclusion != ConclusionSuccess || len(dh.exec.sent()) <= sentBefore {
				t.Fatalf("another replica did not execute the retry: %+v", got)
			}
			for _, req := range dh.exec.sent()[sentBefore:] {
				if req.SHA != shaA || req.Version != "v1.4.0" || req.Event != pipelines.EventRelease {
					t.Fatalf("runner received a changed release identity: %+v", req)
				}
			}
			p, _ = dh.store.pipeline(dh.p.ID)
			original, _ = dh.store.run(first.ID)
			if p.Active() || original.Conclusion != ConclusionFailure {
				t.Fatal("retry changed automatic delivery or the original outcome")
			}
		})
	}
}

func TestWebhookReleaseRerequestCannotStartAfterDisconnect(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	first := dh.openRun(t, Opening{Event: pipelines.EventRelease, Mode: pipelines.ModeFull, SHA: shaA, ReleaseTag: "v1.4.0", Trigger: TriggerManual})
	deliver(t, dh.integ, first)
	completed, _ := dh.store.run(first.ID)
	next := dh.openRun(t, rerunOf(completed, "verified-check-rerequest", false))
	p, _ := dh.store.pipeline(dh.p.ID)
	p.Status = PipelineDisconnected
	dh.store.addPipeline(p)
	sentBefore := len(dh.exec.sent())
	deliver(t, dh.peer("agent-retry"), next)
	got, _ := dh.store.run(next.ID)
	if got.Conclusion != ConclusionFailure || got.RefusalCode != pipelines.CodeDisconnected || len(dh.exec.sent()) != sentBefore {
		t.Fatalf("a webhook rerun inherited the original manual consent: %+v", got)
	}
}
