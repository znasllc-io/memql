package pipelinerun

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelines"
)

func TestDeliveryPolicyAbsentOptionalResolverUsesPortableSnapshot(t *testing.T) {
	p := testPipeline(DeliveryWebhook)
	explicit, err := capturePipelinePolicy(context.Background(), Deps{
		SystemVariable: func(context.Context, string) (string, error) { return "", nil },
	}, p, prOpening())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		value string
		err   error
	}{
		{"absent", "", memql.ErrVariableNotFound},
		{"wrapped absent with unusable value", "mustNotSelectThisResolver", fmt.Errorf("lookup: %w", memql.ErrVariableNotFound)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads := 0
			d := Deps{SystemVariable: func(ctx context.Context, name string) (string, error) {
				reads++
				if name != PolicyResolverVariable || !memql.FreshReadFromContext(ctx) {
					t.Fatal("resolver read lost its exact name or freshness")
				}
				return tc.value, tc.err
			}}
			snapshot, err := capturePipelinePolicy(context.Background(), d, p, prOpening())
			if err != nil {
				t.Fatal(err)
			}
			if reads != 1 || snapshot.Version != explicit.Version {
				t.Fatalf("optional absence changed the portable snapshot: reads=%d version=%s", reads, snapshot.Version)
			}
			// Recover only the durable snapshot on another replica; neither the
			// optional setting nor the originating replica's source is needed.
			recovered, err := readPipelinePolicy(snapshot.Map())
			if err != nil {
				t.Fatal(err)
			}
			mode, err := policyModeForEvent(context.Background(), recovered, p, Opening{Event: pipelines.EventMergeGroup})
			if err != nil || mode != pipelines.ModeFull {
				t.Fatalf("portable queue coverage changed: %q, %v", mode, err)
			}
		})
	}
}

func TestDeliveryPolicyAbsentOptionalResolverOpensOnce(t *testing.T) {
	h := newHarness(t)
	p := testPipeline(DeliveryWebhook)
	h.store.addPipeline(p)
	h.integ.Configure(func(d *Deps) {
		d.SystemVariable = func(context.Context, string) (string, error) {
			return "", fmt.Errorf("global lookup: %w", memql.ErrVariableNotFound)
		}
	})
	first, err := h.integ.open(context.Background(), h.integ.snapshot(), p, prOpening())
	if err != nil || !first.Opened || first.Run.Mode != pipelines.ModeAffected || first.Run.PolicySnapshot == nil {
		t.Fatalf("missing optional resolver blocked the ordinary PR opening: %+v, %v", first, err)
	}
	second, err := h.integ.open(context.Background(), h.integ.snapshot(), p, prOpening())
	if err != nil || second.Opened || second.Run.ID != first.Run.ID || len(h.store.allRuns()) != 1 || len(h.github.createdRuns()) != 1 {
		t.Fatalf("redelivery did not reuse the exact run and check: %+v, %v", second, err)
	}
}

func TestDeliveryPolicyResolverReadFailuresRefuseBeforeCaptureAndEffects(t *testing.T) {
	for _, failure := range []error{
		errors.New("storage unavailable"),
		errors.New("access refused"),
		errors.New(`variable "MEMQL_PIPELINES_POLICY_RESOLVER" not found`),
		context.Canceled,
		context.DeadlineExceeded,
	} {
		t.Run(failure.Error(), func(t *testing.T) {
			h := newHarness(t)
			p := testPipeline(DeliveryWebhook)
			h.store.addPipeline(p)
			sourceReads := 0
			h.integ.Configure(func(d *Deps) {
				d.SystemVariable = func(context.Context, string) (string, error) { return "", failure }
				d.WorkflowSource = func(string, string) (string, error) {
					sourceReads++
					return "", errors.New("must not capture after a failed setting read")
				}
			})
			_, err := h.integ.open(context.Background(), h.integ.snapshot(), p, prOpening())
			if !errors.Is(err, failure) || sourceReads != 0 || len(h.store.allRuns()) != 0 || len(h.github.createdRuns()) != 0 || len(h.github.tokenMints) != 0 {
				t.Fatalf("setting read failure crossed capture or effect boundary: %v (source reads %d)", err, sourceReads)
			}
		})
	}
}

func TestDeliveryPolicySnapshotSurvivesAnotherReplicasBundleChange(t *testing.T) {
	a := workflowSource(t, `@template automation pipelineVersionForEvent { args { eventName string! sha string! tag string! } return "frozen-version" }`)
	d := Deps{}
	bindTestWorkflow(&d, a)
	p := testPipeline(DeliveryWebhook)
	s, err := capturePipelinePolicy(context.Background(), d, p, prOpening())
	if err != nil {
		t.Fatal(err)
	}
	// Serialize as the row and consume on a second replica whose installed
	// sources cannot be reached. No pointer from the originating node survives.
	saved := s.Map()
	d.WorkflowSource = func(string, string) (string, error) { return "", fmt.Errorf("new replica has a changed bundle") }
	other, err := readPipelinePolicy(saved)
	if err != nil {
		t.Fatal(err)
	}
	version, err := policyVersionForEvent(context.Background(), other, prOpening(), shaA)
	if err != nil || version != "frozen-version" {
		t.Fatalf("snapshot version = %q, %v", version, err)
	}
	dr := &runDriver{d: d, run: Run{PolicySnapshot: saved}}
	if err := dr.prepareWorkflow("untrustedManifestWorkflow"); err == nil {
		t.Fatal("source replaced operator binding")
	}
	if err := dr.prepareWorkflow(""); err != nil || dr.workflowIdentity() != s.Version {
		t.Fatalf("recovered identity: %v", err)
	}
	row := runFromRow(map[string]any{"policySnapshot": saved})
	if _, err := readPipelinePolicy(row.PolicySnapshot); err != nil {
		t.Fatalf("stored snapshot: %v", err)
	}
}

func TestDeliveryPolicyCanDeclineAnEventWithoutCreatingChecksOrRuns(t *testing.T) {
	h := newHarness(t)
	p := testPipeline(DeliveryWebhook)
	h.store.addPipeline(p)
	a := workflowSource(t, `@template automation pipelineModeForEvent { args { event string! } return "" }`)
	h.integ.Configure(func(d *Deps) { bindTestWorkflow(d, a) })
	got, err := h.integ.open(context.Background(), h.integ.snapshot(), p, pushOpening())
	if err != nil || got.Ignored == "" || got.Opened || len(h.store.allRuns()) != 0 || len(h.github.createdRuns()) != 0 || len(h.github.tokenMints) != 0 {
		t.Fatalf("declined event produced work: %+v %v", got, err)
	}
}

func TestDeliveryPolicyCannotDowngradeQueueCoverageOrPerformSelectorEffects(t *testing.T) {
	for _, body := range []string{
		`return "affected"`,
		`builtin pipelineSkipStep(stepKey: "forged", code: "pipeline_stage_blocked", reason: "forged")
return "full"`,
	} {
		h := newHarness(t)
		p := testPipeline(DeliveryWebhook)
		a := workflowSource(t, `@template automation pipelineModeForEvent { args { event string! } `+body+` }`)
		h.integ.Configure(func(d *Deps) { bindTestWorkflow(d, a) })
		_, err := h.integ.open(context.Background(), h.integ.snapshot(), p, Opening{Event: pipelines.EventMergeGroup, SHA: shaA, Trigger: TriggerWebhook})
		if err == nil || len(h.store.allRuns()) != 0 || len(h.github.createdRuns()) != 0 {
			t.Fatalf("unsafe selector admitted: %v", err)
		}
	}
}

func TestDeliveryPolicyResolverFailsClosedOnUnknownOrPartialBindings(t *testing.T) {
	for _, answer := range []string{`true`, `{workflow: "runPipelineStages"}`, `{workflow: ""}`} {
		d := Deps{SystemVariable: func(context.Context, string) (string, error) { return "badProfile", nil }}
		d.WorkflowSource = func(kind, name string) (string, error) {
			if kind == "automation" && name == "badProfile" {
				return `@template
automation badProfile { args { repository string! ownerUserId string! pipelineId string! } return ` + answer + ` }`, nil
			}
			return workflowhost.InstalledSource(kind, name)
		}
		if _, err := capturePipelinePolicy(context.Background(), d, testPipeline(DeliveryWebhook), prOpening()); err == nil {
			t.Fatalf("accepted %s", answer)
		}
	}
}

func TestDeliveryPolicyRetryKeepsTheOriginalPolicyAndRejectsAnotherOwner(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	run := dh.openRun(t, prOpening())
	d := dh.integ.snapshot()
	d.WorkflowSource = func(string, string) (string, error) {
		return "", fmt.Errorf("replacement source must not be consulted")
	}
	s, err := capturePipelinePolicy(context.Background(), d, dh.p, Opening{RerunOf: run.ID})
	if err != nil || s.Version != run.PolicySnapshot["version"] {
		t.Fatalf("retry rebound policy: %v", err)
	}
	wrong := dh.p
	wrong.OwnerUserID = "another-owner"
	if _, err := capturePipelinePolicy(context.Background(), d, wrong, Opening{RerunOf: run.ID}); err == nil || !strings.Contains(err.Error(), "matching") {
		t.Fatalf("cross-owner policy lineage accepted: %v", err)
	}
}

func TestDeliveryPolicyRedeliveryUsesSavedPolicyWhenReplicaCannotLoadIt(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	o := prOpening()
	run := dh.openRun(t, o)
	dh.integ.Configure(func(d *Deps) {
		d.WorkflowSource = func(string, string) (string, error) { return "", fmt.Errorf("new bundle missing old source") }
	})
	result, err := dh.integ.open(context.Background(), dh.integ.snapshot(), dh.p, o)
	if err != nil || result.Opened || result.Run.ID != run.ID || len(dh.github.createdRuns()) != 1 {
		t.Fatalf("redelivery rebound its policy or duplicated a check: %s %v", result.Run.ID, err)
	}
}
