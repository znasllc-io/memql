package pipelinerun

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/pipelines"
)

func TestRequestedReleaseBuildPinsTheTagAndDeduplicatesWithItsWebhook(t *testing.T) {
	h := newHarness(t)
	p := testPipeline(DeliveryWebhook)
	h.store.addPipeline(p)
	h.github.commits[repoName+"@tags/v1.4.0"] = headAnswer{SHA: shaC}
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
	h.github.commits[repoName+"@tags/v1.5.0"] = headAnswer{SHA: shaC}
	if _, err := h.integ.RunRelease(personCtx(ownerID), p.ID, "v1.5.0"); err == nil || !strings.Contains(err.Error(), "recorded version") {
		t.Fatalf("another tag must not silently reuse a differently stamped image: %v", err)
	}
}

func TestReleaseRequestRefusesBeforeWritingACheck(t *testing.T) {
	for _, name := range []string{"stranger", "disconnected", "missing connection", "changed installation", "missing tag", "malformed SHA", "branch syntax", "empty tag"} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			p := testPipeline(DeliveryWebhook)
			owner, tag, sha := ownerID, "v1.4.0", shaC
			switch name {
			case "stranger":
				owner = otherID
			case "disconnected":
				p.Status = PipelineDisconnected
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
				h.github.commits[repoName+"@tags/v1.4.0"] = headAnswer{SHA: sha}
			}
			if _, err := h.integ.RunRelease(personCtx(owner), p.ID, tag); err == nil {
				t.Fatal("unsafe release request accepted")
			}
			if len(h.github.created) != 0 {
				t.Fatal("refused release request wrote a check")
			}
			if name == "stranger" || name == "disconnected" || name == "missing connection" || name == "branch syntax" || name == "empty tag" {
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
	dh.github.commits[repoName+"@tags/v1.4.0"] = headAnswer{SHA: shaA}
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
	sent := dh.exec.sent()
	if len(sent) != 1 || sent[0].Event != pipelines.EventRelease || sent[0].Version != "v1.4.0" || sent[0].SHA != shaA || sent[0].Step.Run != "build-release" {
		t.Fatalf("release stages did not retain their source/version across the hop: %+v", sent)
	}
}
