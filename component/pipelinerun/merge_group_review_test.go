package pipelinerun

import (
	"context"
	"fmt"
	"testing"

	"github.com/znasllc-io/memql/component/pipelines"
)

const groupReviewWorkflow = `use pipelines.automations.{ runPipelineStages }
@template
automation groupReviewWorkflow {
 args {stages []any!}
 automation runPipelineStages(stages: args.stages)
 facts := builtin pipelineWorkflowFacts()
 if facts.event == "merge_group" && !facts.stopped {
  builtin pipelineReportMergeGroupReview()
 }
}`

func TestCapturedPipelineReportsOnlyItsActualMergeGroup(t *testing.T) {
	for _, event := range []pipelines.Event{pipelines.EventPullRequest, pipelines.EventPush, pipelines.EventMergeGroup} {
		t.Run(string(event), func(t *testing.T) {
			a := workflowSource(t, groupReviewWorkflow)
			dh := newDriveHarness(t, manifestWorkflow(driveManifest, a.Name))
			reports := 0
			dh.integ.Configure(func(d *Deps) {
				bindTestWorkflow(d, a)
				d.ReportMergeGroupReview = func(ctx context.Context, p Pipeline, r Run, fence func(context.Context) error) (map[string]any, error) {
					reports++
					if err := fence(ctx); err != nil {
						return nil, err
					}
					if p.ID != dh.p.ID || r.Event != pipelines.EventMergeGroup || r.SHA != shaA || r.BaseSHA != shaBase || r.HeadBranch != "gh-readonly-queue/main/opaque" {
						t.Fatal("report escaped captured native group")
					}
					return map[string]any{"state": "complete", "headSha": r.SHA, "checkName": pipelines.IndependentReviewCheckName}, nil
				}
			})
			opening := prOpening()
			opening.Event = event
			if event == pipelines.EventPush {
				opening = pushOpening()
			}
			if event == pipelines.EventMergeGroup {
				opening.Branch = "gh-readonly-queue/main/opaque"
				opening.PullRequest = 0
			}
			r := dh.openRun(t, opening)
			deliver(t, dh.integ, r)
			got, _ := dh.store.run(r.ID)
			if got.Conclusion != ConclusionSuccess {
				t.Fatalf("workflow failed: %+v", got)
			}
			want := 0
			if event == pipelines.EventMergeGroup {
				want = 1
			}
			if reports != want {
				t.Fatalf("reports=%d want%d", reports, want)
			}
		})
	}
}

func TestMergeGroupProviderFenceRejectsAnotherReplicaStateChange(t *testing.T) {
	for _, change := range []string{"cancel", "lease", "head", "base", "ref", "source", "finished", "policy"} {
		t.Run(change, func(t *testing.T) {
			a := workflowSource(t, groupReviewWorkflow)
			dh := newDriveHarness(t, manifestWorkflow(driveManifest, a.Name))
			entered, refused := false, false
			dh.integ.Configure(func(d *Deps) {
				bindTestWorkflow(d, a)
				d.ReportMergeGroupReview = func(ctx context.Context, p Pipeline, r Run, fence func(context.Context) error) (map[string]any, error) {
					entered = true
					if err := fence(ctx); err != nil {
						return nil, err
					}
					dh.store.mu.Lock()
					current := dh.store.runs[r.ID]
					switch change {
					case "cancel":
						current.CancelRequested = true
					case "lease":
						current.DriverLeaseID = "other-replica-claim"
					case "head":
						current.SHA = shaB
					case "base":
						current.BaseSHA = shaB
					case "ref":
						current.HeadBranch = "other-ref"
					case "policy":
						current.PolicySnapshot = map[string]any{"version": "forged"}
					case "finished":
						current.Status = StatusCompleted
					case "source":
						source := dh.store.pipelines[p.ID]
						source.CredentialID = "other-connection"
						dh.store.pipelines[p.ID] = source
					}
					dh.store.runs[r.ID] = current
					dh.store.mu.Unlock()
					err := fence(ctx)
					refused = err != nil
					if err == nil {
						return nil, fmt.Errorf("fence accepted changed authority")
					}
					return nil, err
				}
			})
			r := dh.openRun(t, Opening{Event: pipelines.EventMergeGroup, SHA: shaA, BaseSHA: shaBase, Branch: "gh-readonly-queue/main/opaque", Trigger: TriggerWebhook})
			deliver(t, dh.integ, r)
			if !entered || !refused {
				t.Fatalf("provider fence not exercised: entered=%v refused=%v", entered, refused)
			}
		})
	}
}

func TestMergeGroupReviewCapabilityHasNoAmbientAuthority(t *testing.T) {
	_, err := workflowCapability("reportMergeGroupReview")(context.Background(), nil, 1)
	if err == nil {
		t.Fatal("ambient caller obtained merge-group report authority")
	}
	a := workflowSource(t, `@template
automation forgedGroupReport { args {stages []any!} builtin pipelineReportMergeGroupReview(headSha: "forged") }`)
	dh := newDriveHarness(t, manifestWorkflow(driveManifest, a.Name))
	calls := 0
	dh.integ.Configure(func(d *Deps) {
		bindTestWorkflow(d, a)
		d.ReportMergeGroupReview = func(context.Context, Pipeline, Run, func(context.Context) error) (map[string]any, error) {
			calls++
			return nil, nil
		}
	})
	r := dh.openRun(t, Opening{Event: pipelines.EventMergeGroup, SHA: shaA, BaseSHA: shaBase, Branch: "gh-readonly-queue/main/opaque", Trigger: TriggerWebhook})
	deliver(t, dh.integ, r)
	if calls != 0 {
		t.Fatal("workflow selected its own group head")
	}
	got, _ := dh.store.run(r.ID)
	if got.Conclusion != ConclusionFailure {
		t.Fatalf("forged report did not fail run: %+v", got)
	}
}
