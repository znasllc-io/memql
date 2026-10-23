package pipelinerun

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/pipelines"
)

func TestVerificationReusesOnlyFreshExactDefinitionReceipts(t *testing.T) {
	manifest := `formatVersion: 1
name: verify
pipeline:
  platform: linux/amd64
  image: registry/toolchain@sha256:` + strings.Repeat("a", 64) + `
  stages:
    - name: tests
      steps:
        - name: unit
          verification: true
          placement: cluster
          run: go test ./unit
`
	for _, change := range []string{"same", "engine", "expired", "different-commit"} {
		t.Run(change, func(t *testing.T) {
			dh := newDriveHarness(t, manifest)
			dh.exec.answer = func(_ context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
				result := passed(req)
				result.ArtifactFileIDs = nil
				return result, nil
			}
			a := workflowSource(t, `@template
automation pipelineEvidenceReuse {
 args { event string! sourceEvent string! sourceMode string! ageSeconds int! }
 return true
}`)
			dh.integ.Configure(func(d *Deps) { bindTestWorkflow(d, a) })
			source := dh.openRun(t, Opening{Event: pipelines.EventMergeGroup, SHA: shaA, Branch: "gh-readonly-queue/main/test", Trigger: TriggerWebhook})
			deliver(t, dh.integ, source)
			first, _ := dh.store.run(source.ID)
			if first.Conclusion != ConclusionSuccess {
				t.Fatalf("producer failed: %+v", first)
			}
			o := pushOpening()
			switch change {
			case "engine":
				dh.integ.Configure(func(d *Deps) { d.EngineRevision = func() string { return "changed-engine" } })
			case "expired":
				dh.integ.Configure(func(d *Deps) { d.Now = func() time.Time { return first.FinishedAt.Add(25 * time.Hour) } })
			case "different-commit":
				o.SHA = shaB
				dh.setTree(shaB, manifest)
			}
			target := dh.openRun(t, o)
			deliver(t, dh.integ, target)
			got, _ := dh.store.run(target.ID)
			if got.Conclusion != ConclusionSuccess {
				t.Fatalf("consumer failed: %+v", got)
			}
			want := 2
			if change == "same" {
				want = 1
			}
			if len(dh.exec.sentKeys()) != want {
				t.Fatalf("%s executed %d commands, want %d", change, len(dh.exec.sentKeys()), want)
			}
			if change == "same" {
				rows, err := dh.store.WorkSteps(context.Background(), got.WorkRunID)
				if err != nil || len(rows) != 1 || rows[0].Status != WorkStepSkipped || rows[0].Skip.Code != pipelines.CodePassedEarlier {
					t.Fatalf("missing reuse proof: %+v %v", rows, err)
				}
			}
		})
	}
}
