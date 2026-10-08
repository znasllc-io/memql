package pipelinerun

import (
	"testing"
	"testing/fstest"
	"time"

	"github.com/znasllc-io/memql/component/pipelines"
)

const dailyScanManifest = `formatVersion: 1
name: shop
pipeline:
  schedule: daily
  stages:
    - name: security
      on: [schedule]
      steps:
        - name: audit
          run: make security
`

func setScheduleManifest(h *harness, sha, manifest string) {
	h.github.mu.Lock()
	defer h.github.mu.Unlock()
	h.github.trees[repoName+"@"+sha] = fstest.MapFS{
		pipelines.ManifestPath: &fstest.MapFile{Data: []byte(manifest)},
	}
}

func schedule(t *testing.T, h *harness) PollResult {
	t.Helper()
	result, err := h.integ.Schedule(automationCtx())
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	return result
}

func TestDailyScheduleDeduplicatesWithinUTCDateAndRunsAgainOnUnchangedHeadNextDay(t *testing.T) {
	h := newHarness(t)
	p := testPipeline(DeliveryWebhook)
	h.store.addPipeline(p)
	h.github.heads[repoName+"@main"] = headAnswer{SHA: shaA, Message: "Security updates"}
	setScheduleManifest(h, shaA, dailyScanManifest)

	first := schedule(t, h)
	if len(first.Opened) != 1 || len(first.Existing) != 0 {
		t.Fatalf("first daily scan: %+v", first)
	}
	run := first.Opened[0]
	day := testNow.UTC().Format("2006-01-02")
	if run.Event != pipelines.EventSchedule || run.Mode != pipelines.ModeFull || run.Trigger != TriggerSchedule ||
		run.SHA != shaA || run.HeadBranch != "main" || run.Title != "Security updates" ||
		run.RunKey != mustScheduledRunKey(t, repoName, shaA, pipelines.ModeFull, day) {
		t.Errorf("scheduled run = %+v", run)
	}
	if rerun := rerunOf(run, "", false); rerun.ScheduleSlot != day {
		t.Errorf("manual re-run lost its UTC schedule slot: %+v", rerun)
	}

	duplicate := schedule(t, h)
	if len(duplicate.Opened) != 0 || len(duplicate.Existing) != 1 || duplicate.Existing[0].ID != run.ID {
		t.Fatalf("duplicate same-day scan should resolve to the existing run: %+v", duplicate)
	}
	if got := len(h.github.createdRuns()); got != 1 {
		t.Fatalf("same-day invocations created %d GitHub checks, want one", got)
	}

	nextDay := testNow.Add(24 * time.Hour)
	h.integ.Configure(func(d *Deps) { d.Now = func() time.Time { return nextDay } })
	next := schedule(t, h)
	if len(next.Opened) != 1 || next.Opened[0].SHA != shaA || next.Opened[0].RunKey == run.RunKey {
		t.Fatalf("unchanged head is eligible on the next UTC day: %+v", next)
	}
	if got := len(h.github.createdRuns()); got != 2 {
		t.Fatalf("next-day scan created %d total checks, want two", got)
	}
}

func TestDailyScheduleReadsTheCurrentManifestAtTheCurrentDefaultHead(t *testing.T) {
	h := newHarness(t)
	p := testPipeline(DeliveryPoll)
	h.store.addPipeline(p)
	h.github.heads[repoName+"@main"] = headAnswer{SHA: shaA}
	setScheduleManifest(h, shaA, "formatVersion: 1\nname: shop\npipeline:\n  stages:\n    - name: checks\n      steps:\n        - name: check\n          run: make check\n")
	if got := schedule(t, h); len(got.Opened) != 0 || len(got.Existing) != 0 || len(h.store.allRuns()) != 0 {
		t.Fatalf("pipeline without schedule opened a run: %+v", got)
	}

	h.github.heads[repoName+"@main"] = headAnswer{SHA: shaB}
	setScheduleManifest(h, shaB, dailyScanManifest)
	got := schedule(t, h)
	if len(got.Opened) != 1 || got.Opened[0].SHA != shaB {
		t.Fatalf("updated default-branch manifest should opt in without reconnecting: %+v", got)
	}
}

func mustScheduledRunKey(t *testing.T, repository, sha string, mode pipelines.Mode, day string) string {
	t.Helper()
	key, err := pipelines.ScheduledRunKey(repository, sha, mode, day)
	if err != nil {
		t.Fatal(err)
	}
	return key
}
