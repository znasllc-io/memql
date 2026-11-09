package pipelinerun

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/pipelines"
)

const weeklySecurityManifest = `formatVersion: 1
name: shop
pipeline:
  schedule: weekly
  stages:
    - name: security
      security: true
      on: [schedule, manual]
      steps:
        - name: audit
          run: make security
`

func securityVariable(d *Deps, raw string, err error) {
	prior := d.SystemVariable
	d.SystemVariable = func(ctx context.Context, name string) (string, error) {
		if name == pipelines.SecuritySettingsVariable {
			return raw, err
		}
		if prior != nil {
			return prior(ctx, name)
		}
		return "", nil
	}
}

func TestSecurityScheduleRequiresConsentAndStopsAfterDisable(t *testing.T) {
	h := newHarness(t)
	h.store.addPipeline(testPipeline(DeliveryWebhook))
	h.github.heads[repoName+"@main"] = headAnswer{SHA: shaA}
	setScheduleManifest(h, shaA, weeklySecurityManifest)
	for _, raw := range []string{"", `{"enabled":false}`, `{"enabled":"true"}`, `{"enabled":true,"extra":true}`} {
		h.integ.Configure(func(d *Deps) { securityVariable(d, raw, nil) })
		if got := schedule(t, h); len(got.Opened) != 0 || len(got.Existing) != 0 {
			t.Fatalf("disabled or invalid setting %q opened security work: %+v", raw, got)
		}
	}
	h.integ.Configure(func(d *Deps) { securityVariable(d, `{"enabled":true}`, fmt.Errorf("settings unreadable")) })
	if got := schedule(t, h); len(got.Opened) != 0 {
		t.Fatal("a failed settings read enabled scanning")
	}
	h.integ.Configure(func(d *Deps) { securityVariable(d, `{"enabled":true}`, nil) })
	first := schedule(t, h)
	if len(first.Opened) != 1 {
		t.Fatalf("enabled scan did not open: %+v", first)
	}
	if again := schedule(t, h); len(again.Opened) != 0 || len(again.Existing) != 1 {
		t.Fatalf("same-week scan duplicated: %+v", again)
	}
	h.integ.Configure(func(d *Deps) {
		securityVariable(d, `{"enabled":false}`, nil)
		d.Now = func() time.Time { return testNow.AddDate(0, 0, 7) }
	})
	if got := schedule(t, h); len(got.Opened) != 0 || len(got.Existing) != 0 {
		t.Fatalf("next-week scan survived disabling: %+v", got)
	}
}

func TestDisabledSecurityPreservesUnrelatedWeeklyWork(t *testing.T) {
	h := newHarness(t)
	h.store.addPipeline(testPipeline(DeliveryWebhook))
	h.github.heads[repoName+"@main"] = headAnswer{SHA: shaA}
	setScheduleManifest(h, shaA, weeklySecurityManifest+`    - name: inventory
      on: [schedule]
      steps:
        - name: inventory
          run: make inventory
`)
	if got := schedule(t, h); len(got.Opened) != 1 {
		t.Fatalf("security switch disabled unrelated scheduled work: %+v", got)
	}
}

func TestSecuritySelectionCannotBeOverriddenByInstalledPolicy(t *testing.T) {
	spec := &pipelines.Spec{Schedule: "weekly", Stages: []pipelines.StageSpec{
		{Name: "security", Security: true, On: []string{"schedule", "manual"}},
		{Name: "inventory", On: []string{"schedule", "manual"}},
	}}
	for _, event := range []pipelines.Event{pipelines.EventManual, pipelines.EventSchedule} {
		for _, enabled := range []bool{true, false} {
			dh := newDriveHarness(t, driveManifest)
			run := dh.openRun(t, prOpening())
			dr := preparedSelectionDriver(t, dh, run)
			securityVariable(&dr.d, fmt.Sprintf(`{"enabled":%t}`, enabled), nil)
			got, err := dr.selectStages(context.Background(), spec, event, pipelines.ModeFull, mustScheduledRunKey(t, repoName, shaA, pipelines.ModeFull, "week:2026-10-12"))
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"inventory"}
			if enabled {
				want = []string{"security", "inventory"}
			}
			if !slices.Equal(got.Included, want) {
				t.Fatalf("%s enabled=%t selected %v, want %v", event, enabled, got.Included, want)
			}
		}
	}
}

func TestSecuritySelectionRejectsDailySlotsEvenWhenPolicySelectsEveryStage(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	policy := workflowSource(t, `@template
automation pipelineStageIncluded {
  args { event string! mode string! on []string! runSchedule string! stageSchedule string! }
  return true
}`)
	dh.integ.Configure(func(d *Deps) { bindTestWorkflow(d, policy) })
	run := dh.openRun(t, prOpening())
	dr := preparedSelectionDriver(t, dh, run)
	securityVariable(&dr.d, `{"enabled":true}`, nil)
	spec := &pipelines.Spec{Schedule: "daily", Stages: []pipelines.StageSpec{
		{Name: "security", Security: true, Schedule: "weekly", On: []string{"schedule", "manual"}},
		{Name: "inventory", On: []string{"schedule"}},
	}}
	got, err := dr.selectStages(context.Background(), spec, pipelines.EventSchedule, pipelines.ModeFull,
		mustScheduledRunKey(t, repoName, shaA, pipelines.ModeFull, "2026-10-14"))
	if err != nil || !slices.Equal(got.Included, []string{"inventory"}) {
		t.Fatalf("a custom policy bypassed weekly scan cadence: %v, %v", got.Included, err)
	}
}
