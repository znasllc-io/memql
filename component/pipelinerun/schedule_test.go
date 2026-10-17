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

func TestWeeklyScheduleCatchesUpAndDeduplicatesAcrossReplicasAndNewHeads(t *testing.T) {
	h := newHarness(t)
	h.store.addPipeline(testPipeline(DeliveryWebhook))
	h.github.heads[repoName+"@main"] = headAnswer{SHA: shaA}
	manifest := dailyScanManifest + `    - name: deep
      on: [schedule, manual]
      schedule: weekly
      steps:
        - name: history
          run: make deep
`
	setScheduleManifest(h, shaA, manifest)
	wednesday := time.Date(2026, 10, 14, 3, 30, 0, 0, time.UTC)
	h.integ.Configure(func(d *Deps) { d.Now = func() time.Time { return wednesday } })
	// Two scheduler replicas with no shared integration-local state.
	d := h.integ.snapshot()
	d.NodeID = "agent-b"
	other := New(d)
	resultCh := make(chan PollResult, 2)
	errCh := make(chan error, 2)
	for _, i := range []*Integration{h.integ, other} {
		go func(i *Integration) { r, e := i.Schedule(automationCtx()); resultCh <- r; errCh <- e }(i)
	}
	opened := []Run{}
	for range 2 {
		r := <-resultCh
		if e := <-errCh; e != nil {
			t.Fatal(e)
		}
		if len(r.Failed) > 0 {
			t.Fatalf("schedule: %+v", r)
		}
		opened = append(opened, r.Opened...)
	}
	if len(opened) != 2 || len(h.github.createdRuns()) != 2 {
		t.Fatalf("replicas opened %+v", opened)
	}
	var weekly Run
	for _, r := range opened {
		if slot, _ := pipelines.ScheduledSlotFromRunKey(r.RunKey); slot == "week:2026-10-12" {
			weekly = r
		}
	}
	if weekly.ID == "" {
		t.Fatal("missed Monday did not catch up on Wednesday")
	}
	for _, run := range opened {
		check := ComposeCheckRun("", testPipeline(DeliveryWebhook), run, ReportFor(testPipeline(DeliveryWebhook), run, nil))
		want := "MemQL / shop / daily"
		if run.ID == weekly.ID {
			want = "MemQL / shop / weekly"
		}
		if check.Name != want {
			t.Errorf("periodic check can collide with required CI: %q, want %q", check.Name, want)
		}
	}
	// A failure remains visible and is not automatically rerun on a changed SHA.
	h.store.mu.Lock()
	for key, r := range h.store.runs {
		if r.ID == weekly.ID {
			r.Status = StatusCompleted
			r.Conclusion = ConclusionFailure
			h.store.runs[key] = r
		}
	}
	h.store.mu.Unlock()
	h.github.mu.Lock()
	h.github.heads[repoName+"@main"] = headAnswer{SHA: shaB}
	h.github.mu.Unlock()
	setScheduleManifest(h, shaB, manifest)
	thursday := wednesday.Add(24 * time.Hour)
	other.Configure(func(d *Deps) { d.Now = func() time.Time { return thursday } })
	r, err := other.Schedule(automationCtx())
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Opened) != 1 || len(r.Existing) != 1 || r.Existing[0].ID != weekly.ID || r.Existing[0].SHA != shaA || (r.Existing[0].Status != StatusCompleted || r.Existing[0].Conclusion != ConclusionFailure) {
		t.Fatalf("new head repeated or hid weekly result: %+v", r)
	}
	retry := rerunOf(r.Existing[0], "", false)
	if retry.ScheduleSlot != "week:2026-10-12" || retry.SHA != shaA {
		t.Fatalf("retry lost pinned period/source: %+v", retry)
	}
	next := wednesday.AddDate(0, 0, 7)
	other.Configure(func(d *Deps) { d.Now = func() time.Time { return next } })
	r, err = other.Schedule(automationCtx())
	if err != nil || len(r.Opened) != 2 {
		t.Fatalf("next week: %+v %v", r, err)
	}
}
