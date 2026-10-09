package pipelinerun

import (
	"testing"

	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/pipelines"
)

func TestRunLatestPinsTheDisconnectedPipelinesCurrentDefaultHead(t *testing.T) {
	h := newHarness(t)
	p := testPipeline(DeliveryWebhook)
	p.Status = PipelineDisconnected
	h.store.addPipeline(p)
	h.github.repos[repoName] = githubapp.RepositoryInfo{FullName: "acme/shop", DefaultBranch: "trunk"}
	h.github.heads[repoName+"@trunk"] = headAnswer{SHA: shaA, Message: "Rehearse this commit\nbody"}

	got, err := h.integ.RunLatest(personCtx(ownerID), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PipelineID != p.ID || got.Event != pipelines.EventManual || got.Mode != pipelines.ModeFull ||
		got.Trigger != TriggerManual || got.SHA != shaA || got.HeadBranch != "trunk" || got.Title != "Rehearse this commit" ||
		got.Status != StatusQueued || got.Attempt != 1 {
		t.Fatalf("manual run is not pinned to the current default branch: %+v", got)
	}
	if len(h.github.created) != 1 || h.github.created[0].Repository != repoName || h.github.created[0].Token != "ghs_"+credID {
		t.Fatalf("check run was not written through the owner's grant: %+v", h.github.created)
	}

	// A double click or a lost UI response resolves to the same attempt and
	// cannot leave two check runs on one commit.
	again, err := h.integ.RunLatest(personCtx(ownerID), p.ID)
	if err != nil || again.ID != got.ID || len(h.github.created) != 1 {
		t.Fatalf("repeated manual request = %+v, checks %d, error %v", again, len(h.github.created), err)
	}

	got.Status, got.Conclusion = StatusCompleted, ConclusionFailure
	h.store.addRun(got)
	next, err := h.integ.Rerun(personCtx(ownerID), got.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if next.ID == got.ID || next.Attempt != 2 || next.Event != pipelines.EventManual || next.Trigger != TriggerRerun {
		t.Fatalf("manual rehearsal re-run = %+v", next)
	}
}

func TestRunLatestRequiresOwnerAndDisconnectedPipeline(t *testing.T) {
	t.Run("another owner", func(t *testing.T) {
		h := newHarness(t)
		p := testPipeline(DeliveryWebhook)
		p.Status = PipelineDisconnected
		h.store.addPipeline(p)
		if _, err := h.integ.RunLatest(personCtx(otherID), p.ID); err == nil {
			t.Fatal("another owner's pipeline was not refused")
		}
		if len(h.github.tokenMints) != 0 || len(h.github.created) != 0 {
			t.Fatalf("stranger reached GitHub: tokens %d checks %d", len(h.github.tokenMints), len(h.github.created))
		}
	})

	t.Run("automatic delivery active", func(t *testing.T) {
		h := newHarness(t)
		p := testPipeline(DeliveryWebhook)
		h.store.addPipeline(p)
		if _, err := h.integ.RunLatest(personCtx(ownerID), p.ID); refusalCode(err) != pipelines.CodeManualRequiresDisconnected {
			t.Fatalf("active pipeline refusal = %v", err)
		}
		if len(h.github.tokenMints) != 0 || len(h.github.created) != 0 {
			t.Fatalf("active pipeline reached GitHub: tokens %d checks %d", len(h.github.tokenMints), len(h.github.created))
		}
	})
}

func TestRunLatestRefusesUnpinnedOrChangedInstallation(t *testing.T) {
	for _, tc := range []struct {
		name         string
		sha          string
		installation int64
	}{
		{name: "invalid SHA", sha: "not-a-commit", installation: 7},
		{name: "changed installation", sha: shaA, installation: 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			p := testPipeline(DeliveryWebhook)
			p.Status = PipelineDisconnected
			h.store.addPipeline(p)
			h.github.installationID = tc.installation
			h.github.repos[repoName] = githubapp.RepositoryInfo{DefaultBranch: "main"}
			h.github.heads[repoName+"@main"] = headAnswer{SHA: tc.sha, Message: "commit"}
			if _, err := h.integ.RunLatest(personCtx(ownerID), p.ID); err == nil {
				t.Fatal("manual run was opened without a stable repository pin")
			}
			if len(h.github.created) != 0 {
				t.Fatalf("invalid request created check runs: %+v", h.github.created)
			}
		})
	}
}
