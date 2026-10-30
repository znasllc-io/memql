package pipelinerun

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/pipelines"
)

func TestComputePlacementUsesCapturedPolicyAcrossReplicas(t *testing.T) {
	a := workflowSource(t, `@template
automation pipelineComputePlacementPolicy {
 args { repository string! pipelineId string! ownerUserId string! event string! }
 return {fleetWaitSeconds: 60, cloudFallback: true}
}`)
	d := Deps{}
	bindTestWorkflow(&d, a)
	p := testPipeline(DeliveryWebhook)
	snapshot, err := capturePipelinePolicy(context.Background(), d, p, prOpening())
	if err != nil {
		t.Fatal(err)
	}
	// The second replica cannot load the first replica's installed definitions.
	// Its only source of placement policy is the run's durable snapshot.
	d.WorkflowSource = func(string, string) (string, error) { return "", errors.New("bundle changed") }
	dr := &runDriver{d: d, run: Run{PolicySnapshot: snapshot.Map(), Repository: p.Repository,
		PipelineID: p.ID, OwnerUserID: p.OwnerUserID, Event: pipelines.EventPullRequest}}
	if err := dr.prepareWorkflow(""); err != nil {
		t.Fatal(err)
	}
	policy, err := dr.selectComputePlacement(context.Background())
	if err != nil || policy == nil || policy.FleetWaitSeconds != 60 || !policy.CloudFallback {
		t.Fatalf("captured placement lost: %+v %v", policy, err)
	}
	// The captured policy names no retries, so the portable limits apply.
	if !reflect.DeepEqual(policy.Retries, pipelines.DefaultRetryPolicy()) {
		t.Fatalf("a policy without retries must read as the portable defaults: %+v", policy.Retries)
	}
	dr.computePlacement = policy
	track := &stepTrack{step: pipelines.Step{Kind: pipelines.StepCommand, Placement: pipelines.PlacementPool}}
	request := dr.request(track, nil)
	request.ComputePlacement.CloudFallback = false
	request.ComputePlacement.Retries.DelaysSeconds[0] = 1
	if got := dr.request(track, nil).ComputePlacement; !got.CloudFallback || got.Retries.DelaysSeconds[0] != 30 {
		t.Fatal("one executor changed another step's policy")
	}
	// Retries apply to every step, so a dedicated command step carries the
	// installed policy too. What it does not get is any use of the capacity
	// preference in it: the executor reads FleetWaitSeconds and CloudFallback
	// for a pool step only (TestAPolicyOnADedicatedStepChangesNoRoute).
	track.step.Placement = pipelines.PlacementCluster
	if got := dr.request(track, nil).ComputePlacement; got == nil || !reflect.DeepEqual(*got, *policy) {
		t.Fatalf("a dedicated command step lost the installed policy: %+v", got)
	}
	// The driver delivers a notification, a report and a repository change
	// itself; none reaches an executor, so none is handed limits to apply.
	for _, kind := range []pipelines.StepKind{pipelines.StepNotify, pipelines.StepSARIF, pipelines.StepRepositoryChange} {
		track.step.Kind = kind
		if dr.request(track, nil).ComputePlacement != nil {
			t.Fatalf("policy leaked into a %s step", kind)
		}
	}
	old := &runDriver{}
	if policy, err := old.selectComputePlacement(context.Background()); err != nil || policy.CloudFallback {
		t.Fatalf("legacy snapshot gained cloud permission: %+v %v", policy, err)
	}
}

func TestComputePlacementRejectsUnboundedOrAmbiguousPolicy(t *testing.T) {
	for _, value := range []any{
		nil, true, map[string]any{},
		map[string]any{"fleetWaitSeconds": 60},
		map[string]any{"fleetWaitSeconds": -1, "cloudFallback": true},
		map[string]any{"fleetWaitSeconds": 3601, "cloudFallback": true},
		map[string]any{"fleetWaitSeconds": 1.5, "cloudFallback": true},
		map[string]any{"fleetWaitSeconds": 1e99, "cloudFallback": true},
		map[string]any{"fleetWaitSeconds": 60, "cloudFallback": "true"},
		map[string]any{"fleetWaitSeconds": 60, "cloudFallback": nil},
		map[string]any{"fleetWaitSeconds": 60, "cloudFallback": true, "machineId": "untrusted"},
	} {
		if got, err := parseComputePlacement(value); err == nil || got != nil {
			t.Fatalf("accepted %+v", value)
		}
	}
	for _, seconds := range []int{0, 3600} {
		if got, err := parseComputePlacement(map[string]any{"fleetWaitSeconds": seconds, "cloudFallback": false}); err != nil || got.FleetWaitSeconds != seconds {
			t.Fatalf("valid boundary refused: %+v %v", got, err)
		}
	}
}

// policyWithRetries is a valid policy whose retries the caller edits.
func policyWithRetries(edit func(retries map[string]any)) map[string]any {
	retries := map[string]any{"sameMachine": 2, "delaysSeconds": []any{30, 120}, "otherMachines": true}
	if edit != nil {
		edit(retries)
	}
	return map[string]any{"fleetWaitSeconds": 60, "cloudFallback": true, "retries": retries}
}

// The installation sets the limits (spec D6) and the engine takes them
// literally, so a limit that cannot be read exactly is refused, naming the
// field, instead of being rounded to something the author never wrote: a run
// that fails with "retries.sameMachine must be an integer 0..3" costs a
// sentence to fix, and a policy that silently retried 30 times would not have
// been noticed until the bill.
func TestRetriesParseStrictly(t *testing.T) {
	t.Run("what is accepted", func(t *testing.T) {
		for _, c := range []struct {
			name  string
			value any
			want  pipelines.RetryPolicy
		}{
			{
				// retries is optional: the older installed policies return only
				// {fleetWaitSeconds, cloudFallback} and must keep working.
				"absent retries is the portable default",
				map[string]any{"fleetWaitSeconds": 60, "cloudFallback": true},
				pipelines.DefaultRetryPolicy(),
			},
			{"the portable values, written out", policyWithRetries(nil), pipelines.DefaultRetryPolicy()},
			{
				"no retries at all",
				policyWithRetries(func(r map[string]any) {
					r["sameMachine"], r["delaysSeconds"], r["otherMachines"] = 0, []any{0}, false
				}),
				pipelines.RetryPolicy{SameMachine: 0, DelaysSeconds: []int{0}, OtherMachines: false},
			},
			{
				"the largest limits",
				policyWithRetries(func(r map[string]any) { r["sameMachine"], r["delaysSeconds"] = 3, []any{900, 900, 900} }),
				pipelines.RetryPolicy{SameMachine: 3, DelaysSeconds: []int{900, 900, 900}, OtherMachines: true},
			},
			{
				"values as Go code hands them over",
				policyWithRetries(func(r map[string]any) { r["sameMachine"], r["delaysSeconds"] = int64(1), []int{5, 10} }),
				pipelines.RetryPolicy{SameMachine: 1, DelaysSeconds: []int{5, 10}, OtherMachines: true},
			},
		} {
			t.Run(c.name, func(t *testing.T) {
				got, err := parseComputePlacement(c.value)
				if err != nil || got == nil {
					t.Fatalf("refused: %v", err)
				}
				if !reflect.DeepEqual(got.Retries, c.want) {
					t.Fatalf("retries = %+v, want %+v", got.Retries, c.want)
				}
				if got.FleetWaitSeconds != 60 || !got.CloudFallback {
					t.Fatalf("the capacity half of the policy changed with its retries: %+v", got)
				}
			})
		}
	})

	t.Run("what is refused, and what the refusal names", func(t *testing.T) {
		for _, c := range []struct {
			name, field string
			value       any
		}{
			{"same-machine retries above the bound", "sameMachine", policyWithRetries(func(r map[string]any) { r["sameMachine"] = 4 })},
			{"same-machine retries below zero", "sameMachine", policyWithRetries(func(r map[string]any) { r["sameMachine"] = -1 })},
			{"fractional same-machine retries", "sameMachine", policyWithRetries(func(r map[string]any) { r["sameMachine"] = 1.5 })},
			{"same-machine retries as text", "sameMachine", policyWithRetries(func(r map[string]any) { r["sameMachine"] = "2" })},
			{"null same-machine retries", "sameMachine", policyWithRetries(func(r map[string]any) { r["sameMachine"] = nil })},
			{"absent same-machine retries", "sameMachine", policyWithRetries(func(r map[string]any) { delete(r, "sameMachine") })},

			{"no delays", "delaysSeconds", policyWithRetries(func(r map[string]any) { r["delaysSeconds"] = []any{} })},
			{"four delays", "delaysSeconds", policyWithRetries(func(r map[string]any) { r["delaysSeconds"] = []any{1, 2, 3, 4} })},
			{"a delay past a quarter hour", "delaysSeconds", policyWithRetries(func(r map[string]any) { r["delaysSeconds"] = []any{901} })},
			{"a negative delay", "delaysSeconds", policyWithRetries(func(r map[string]any) { r["delaysSeconds"] = []any{30, -1} })},
			{"a fractional delay", "delaysSeconds", policyWithRetries(func(r map[string]any) { r["delaysSeconds"] = []any{1.5} })},
			{"a null delay", "delaysSeconds", policyWithRetries(func(r map[string]any) { r["delaysSeconds"] = []any{30, nil} })},
			{"a text delay", "delaysSeconds", policyWithRetries(func(r map[string]any) { r["delaysSeconds"] = []any{"30"} })},
			{"delays that are not a list", "delaysSeconds", policyWithRetries(func(r map[string]any) { r["delaysSeconds"] = 30 })},
			{"null delays", "delaysSeconds", policyWithRetries(func(r map[string]any) { r["delaysSeconds"] = nil })},
			{"absent delays", "delaysSeconds", policyWithRetries(func(r map[string]any) { delete(r, "delaysSeconds") })},

			{"other machines as text", "otherMachines", policyWithRetries(func(r map[string]any) { r["otherMachines"] = "yes" })},
			{"other machines as a number", "otherMachines", policyWithRetries(func(r map[string]any) { r["otherMachines"] = 1 })},
			{"null other machines", "otherMachines", policyWithRetries(func(r map[string]any) { r["otherMachines"] = nil })},
			{"absent other machines", "otherMachines", policyWithRetries(func(r map[string]any) { delete(r, "otherMachines") })},

			// No other field: a limit the engine does not have is a policy that
			// believes it set something. The refusal names the key it found.
			{"an extra retry field", "jitter", policyWithRetries(func(r map[string]any) { r["jitter"] = 5 })},
			{"a machine named in the policy", "machineId", policyWithRetries(func(r map[string]any) { r["machineId"] = "untrusted" })},

			// The same goes for the policy itself: a key it does not define is named,
			// so a misspelled retries (retry) is found at once and not read as absent.
			{"a misspelled retries key", "retry", map[string]any{"fleetWaitSeconds": 60, "cloudFallback": true, "retry": map[string]any{"sameMachine": 1}}},

			// retries is optional, but when it is there it is an object. null is
			// not "absent": it would be a policy author's mistake read as a choice.
			{"null retries", "retries", map[string]any{"fleetWaitSeconds": 60, "cloudFallback": true, "retries": nil}},
			{"retries as text", "retries", map[string]any{"fleetWaitSeconds": 60, "cloudFallback": true, "retries": "none"}},
			{"retries as a list", "retries", map[string]any{"fleetWaitSeconds": 60, "cloudFallback": true, "retries": []any{2}}},
			{"empty retries", "sameMachine", map[string]any{"fleetWaitSeconds": 60, "cloudFallback": true, "retries": map[string]any{}}},
		} {
			t.Run(c.name, func(t *testing.T) {
				got, err := parseComputePlacement(c.value)
				if err == nil || got != nil {
					t.Fatalf("accepted %+v as %+v", c.value, got)
				}
				if !strings.Contains(err.Error(), c.field) {
					t.Fatalf("the refusal %q does not name %q", err, c.field)
				}
			})
		}
	})

	// A policy that returns only the capacity half is what every installed policy
	// before retries existed returns, and the portable template is what an
	// installation that wrote none gets. Both halves of "the portable default"
	// must say the same numbers, or an installation that deletes its policy
	// changes how hard its steps are retried without anyone editing a limit. The
	// template also has to WRITE the retries out: an absent block parses to the
	// same defaults, so checking the parsed policy alone would pass for a template
	// that says nothing and leave an installation copying it with no limits to read.
	t.Run("the shipped template states the portable defaults", func(t *testing.T) {
		value, err := workflowhost.Run(context.Background(), "pipelineComputePlacementPolicy", map[string]any{
			"repository": "acme/shop", "pipelineId": "pipeline", "ownerUserId": "owner", "event": "pull_request",
		}, workflowhost.Options{})
		if err != nil {
			t.Fatal(err)
		}
		returned, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("the shipped template returned %T, want an object", value)
		}
		if _, written := returned["retries"]; !written {
			t.Fatalf("the shipped template returns %v: it must state the portable retry limits, so an installation that copies it sees them", returned)
		}
		got, err := parseComputePlacement(value)
		if err != nil {
			t.Fatalf("the shipped template returns something its own parser refuses: %v", err)
		}
		want := &pipelines.ComputePlacementPolicy{Retries: pipelines.DefaultRetryPolicy()}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("dsl/pipelines/automations.memql pipelineComputePlacementPolicy returns %+v, want %+v: no Fleet wait, no cloud, the default retries", got, want)
		}
	})
}

// legacyPolicySnapshot is a delivery policy as an engine captured it before the
// compute placement role existed: every required phase and none of the
// optional ones.
func legacyPolicySnapshot(t *testing.T) map[string]any {
	t.Helper()
	phases := make(map[string]string, len(policyRoles))
	for _, name := range policyRoles {
		phases[name] = name
	}
	snapshot, err := workflowhost.CapturePhases(policyRoles["workflow"], pipelinePolicyContract, phases,
		[]string{"pipelineSARIFDelivery"}, nil, pipelinePolicyOperations())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Phases["pipelineComputePlacementPolicy"] != "" {
		t.Fatal("the fixture was meant to lack the compute placement phase")
	}
	return snapshot.Map()
}

// Retries apply to every step, so every run that has a command step gets the
// installed policy, whether or not it has a pool step to prefer capacity for.
// Before this the policy was computed only for a pool step, and a run of
// dedicated steps never read it.
func TestEveryRunGetsAPolicyEvenWithoutPoolSteps(t *testing.T) {
	want := pipelines.ComputePlacementPolicy{Retries: pipelines.DefaultRetryPolicy()}

	if got, err := (&runDriver{}).selectComputePlacement(context.Background()); err != nil || !reflect.DeepEqual(got, &want) {
		t.Fatalf("a driver with no policy phase selected %+v, %v; want no Fleet wait, no cloud and the default retries", got, err)
	}

	for _, c := range []struct {
		name   string
		legacy bool
	}{
		{"a snapshot that lacks the phase", true},
		{"a snapshot that carries the portable template", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			dh := newDriveHarness(t, driveManifest) // cluster steps only
			run := dh.openRun(t, prOpening())
			if c.legacy {
				snapshot := legacyPolicySnapshot(t)
				dh.store.mu.Lock()
				stored := dh.store.runs[run.ID]
				stored.PolicySnapshot = snapshot
				dh.store.runs[run.ID] = stored
				dh.store.mu.Unlock()
			}
			deliver(t, dh.integ, run)
			got, _ := dh.store.run(run.ID)
			if got.Conclusion != ConclusionSuccess {
				t.Fatalf("the run did not pass: %s (%s %s)", got.Conclusion, got.RefusalCode, got.RefusalMessage)
			}
			sent := dh.exec.sent()
			if len(sent) != 3 {
				t.Fatalf("the executor was handed %d steps, want the manifest's 3", len(sent))
			}
			for _, req := range sent {
				if req.Step.Placement != "" && req.Step.Placement != pipelines.PlacementCluster {
					t.Fatalf("the fixture must have no pool or fleet step: %+v", req.Step)
				}
				if req.ComputePlacement == nil || !reflect.DeepEqual(*req.ComputePlacement, want) {
					t.Fatalf("step %s was handed policy %+v, want %+v", req.StepKey, req.ComputePlacement, want)
				}
			}
		})
	}

	// The other side of the same change, stated so nobody mistakes it for an
	// oversight: a policy that cannot be read now stops every run that has a
	// command step, not only a run with a pool step, because the limits in it
	// apply to all of them. The run is refused before a work run or an effect
	// exists, and the sentence names the field to fix.
	installUnreadablePolicy := func(t *testing.T, dh *driveHarness) {
		t.Helper()
		a := workflowSource(t, "@template\nautomation pipelineComputePlacementPolicy { args { repository string! pipelineId string! ownerUserId string! event string! } "+
			"return {fleetWaitSeconds: 0, cloudFallback: false, retries: {sameMachine: 4, delaysSeconds: [30, 120], otherMachines: true}} }")
		dh.integ.Configure(func(d *Deps) { bindTestWorkflow(d, a) })
	}
	t.Run("a policy that cannot be read refuses the run before any effect", func(t *testing.T) {
		dh := newDriveHarness(t, driveManifest) // no pool step anywhere
		installUnreadablePolicy(t, dh)
		run := dh.openRun(t, prOpening())
		deliver(t, dh.integ, run)
		got, _ := dh.store.run(run.ID)
		if got.Conclusion == ConclusionSuccess || len(dh.exec.sent()) != 0 || got.WorkRunID != "" {
			t.Fatalf("an unreadable policy reached effects: %+v", got)
		}
		if got.RefusalCode != pipelines.CodeStageInvalid || !strings.Contains(got.RefusalMessage, "sameMachine") {
			t.Fatalf("refusal = %s %q, want pipeline_stage_invalid naming sameMachine", got.RefusalCode, got.RefusalMessage)
		}
	})

	// A skipped step asks for the policy too. A replica that takes a run over
	// compiles the plan again, and what it skips can differ from what the first
	// replica skipped (a changed-files compare that fails the second time selects
	// everything). The policy is bound into every command step's recovery
	// definition, so whether it is read must follow the plan's step kinds, which
	// do not move, and not its skips, which can. The observable half: a run whose
	// only command step is skipped is refused for an unreadable policy all the
	// same, and passes, running nothing, for a readable one.
	t.Run("a run whose command steps are all skipped reads it as well", func(t *testing.T) {
		const skippedOnly = `formatVersion: 1
name: shop
pipeline:
  image: ghcr.io/acme/toolchain@sha256:abc
  select:
    buckets:
      os: ["web/**"]
  stages:
    - name: tests
      steps:
        - name: os
          run: make os
          when: { bucket: os }
`
		for _, readable := range []bool{true, false} {
			dh := newDriveHarness(t, skippedOnly)
			dh.github.mu.Lock()
			dh.github.compare[repoName+"@"+shaBase+"..."+shaA] = compareAnswer{Files: []string{"docs/guide.md"}, Complete: true}
			dh.github.mu.Unlock()
			if !readable {
				installUnreadablePolicy(t, dh)
			}
			run := dh.openRun(t, prOpening())
			deliver(t, dh.integ, run)
			got, _ := dh.store.run(run.ID)
			if len(dh.exec.sent()) != 0 {
				t.Fatalf("readable=%v: a skipped step reached the executor", readable)
			}
			if readable && got.Conclusion != ConclusionSuccess {
				t.Fatalf("a run of skipped steps with a readable policy ended %s (%s %s)", got.Conclusion, got.RefusalCode, got.RefusalMessage)
			}
			if !readable && (got.Conclusion == ConclusionSuccess || got.RefusalCode != pipelines.CodeStageInvalid || !strings.Contains(got.RefusalMessage, "sameMachine")) {
				t.Fatalf("an unreadable policy was not read for a run whose command step was skipped: %s %s %q", got.Conclusion, got.RefusalCode, got.RefusalMessage)
			}
		}
	})
}

// What a replica that takes a run over applies is the limits the run began
// with. The policy is captured from the snapshot, so it only moves when its
// answer depends on something that moves (a clock); then the recovery
// definition must change with it and recovery must refuse, rather than apply
// new limits to a step the first replica had already begun (spec D6).
func TestRetryPolicyIsPartOfTheRecoveryDefinition(t *testing.T) {
	driver := func(edit func(*pipelines.ComputePlacementPolicy)) *runDriver {
		policy := pipelines.ComputePlacementPolicy{FleetWaitSeconds: 60, CloudFallback: true, Retries: pipelines.DefaultRetryPolicy()}
		edit(&policy)
		return &runDriver{d: Deps{EngineRevision: func() string { return "immutable-engine" }},
			p: testPipeline(DeliveryWebhook), computePlacement: &policy}
	}
	same := func(*pipelines.ComputePlacementPolicy) {}
	// A cluster step: the case the carried pool-only binding did not cover. It
	// is also a verification step, so its evidence fingerprint is in play.
	cluster := pipelines.Step{Key: "checks.vet", Kind: pipelines.StepCommand, Placement: pipelines.PlacementCluster, Run: "go vet ./...", Verification: true}

	base := driver(same)
	if base.definitionOf(cluster) == "" || base.definitionOf(cluster) != driver(same).definitionOf(cluster) {
		t.Fatal("equal policies must give one recovery definition")
	}
	for name, edit := range map[string]func(*pipelines.ComputePlacementPolicy){
		"same-machine retries": func(p *pipelines.ComputePlacementPolicy) { p.Retries.SameMachine = 1 },
		"a delay":              func(p *pipelines.ComputePlacementPolicy) { p.Retries.DelaysSeconds[1] = 60 },
		"another delay": func(p *pipelines.ComputePlacementPolicy) {
			p.Retries.DelaysSeconds = append(p.Retries.DelaysSeconds, 300)
		},
		"other machines": func(p *pipelines.ComputePlacementPolicy) { p.Retries.OtherMachines = false },
		"cloud fallback": func(p *pipelines.ComputePlacementPolicy) { p.CloudFallback = false },
		"the Fleet wait": func(p *pipelines.ComputePlacementPolicy) { p.FleetWaitSeconds = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			other := driver(edit)
			if base.definitionOf(cluster) == other.definitionOf(cluster) {
				t.Fatal("a changed policy kept its recovery definition: a replica taking over would apply the new limits")
			}
			// Evidence is about what the command computes, not where it ran or
			// how hard it was retried.
			if base.verificationOf(cluster) == "" || base.verificationOf(cluster) != other.verificationOf(cluster) {
				t.Fatal("the policy changed what a verification step proves")
			}
		})
	}

	// The checks below report with Error, not Fatal, so that one failure does not
	// hide the resume check that follows them.
	//
	// The limits are bound for a command step, which an executor runs; a step
	// the driver delivers itself never reads them.
	notify := pipelines.Step{Key: "tell.notify", Kind: pipelines.StepNotify, Channel: "ops"}
	if base.definitionOf(notify) == "" || base.definitionOf(notify) != driver(func(p *pipelines.ComputePlacementPolicy) { p.Retries.SameMachine = 0 }).definitionOf(notify) {
		t.Error("the retry limits changed the definition of a step that is never retried")
	}

	// The pool binding the carried work put in the verification fingerprint is
	// out of it too: a pool step is not a verification step today (verification
	// requires a cluster container), and if one ever were, its evidence would
	// still bind the command and not the placement.
	pool := cluster
	pool.Placement = pipelines.PlacementPool
	if base.verificationOf(pool) == "" || base.verificationOf(pool) != driver(func(p *pipelines.ComputePlacementPolicy) { p.CloudFallback = false }).verificationOf(pool) {
		t.Error("a pool step's verification evidence is bound to the installed placement policy")
	}
	if base.definitionOf(pool) == driver(func(p *pipelines.ComputePlacementPolicy) { p.CloudFallback = false }).definitionOf(pool) {
		t.Error("a pool step's recovery definition lost its placement policy")
	}

	// At the seam recovery uses: the rows a first replica journaled carry the
	// definition it computed, and a second replica whose policy reads differently
	// is refused rather than resumed.
	t.Run("a replica that reads other limits is refused at resume", func(t *testing.T) {
		withSameMachine := func(sameMachine int) *runDriver {
			return driver(func(p *pipelines.ComputePlacementPolicy) { p.Retries.SameMachine = sameMachine })
		}
		step := pipelines.Step{Key: "checks.vet", Kind: pipelines.StepCommand, Run: "go vet ./...", Placement: pipelines.PlacementCluster}
		plan := pipelines.Plan{Stages: []pipelines.PlanStage{{Name: "checks", Steps: []pipelines.Step{step}}}}

		first := withSameMachine(2)
		first.buildTracks(plan)
		rows := []WorkStep{{Key: step.Key, Status: WorkStepRunning, Attempt: 1, DefinitionFingerprint: first.definitionOf(step)}}

		same := withSameMachine(2)
		same.buildTracks(plan)
		if why := same.resume(rows); why != "" {
			t.Fatalf("a replica that reads the same limits was refused: %s", why)
		}

		changed := withSameMachine(1)
		changed.buildTracks(plan)
		if why := changed.resume(rows); why == "" || !strings.Contains(why, "definition") {
			t.Fatalf("a replica that reads different limits resumed the run (why = %q)", why)
		}
	})
}

func TestComputePlacementDecisionIsPartOfPoolRecoveryDefinition(t *testing.T) {
	dr := &runDriver{d: Deps{EngineRevision: func() string { return "immutable-engine" }},
		p: testPipeline(DeliveryWebhook), computePlacement: &pipelines.ComputePlacementPolicy{FleetWaitSeconds: 60, CloudFallback: true, Retries: pipelines.DefaultRetryPolicy()}}
	pool := pipelines.Step{Key: "checks.vet", Kind: pipelines.StepCommand, Placement: pipelines.PlacementPool}
	driverOwned := pipelines.Step{Key: "tell.notify", Kind: pipelines.StepNotify, Channel: "ops"}
	poolBefore, driverOwnedBefore := dr.definitionOf(pool), dr.definitionOf(driverOwned)
	dr.computePlacement.CloudFallback = false
	if poolBefore == "" || poolBefore == dr.definitionOf(pool) {
		t.Fatal("changed placement retained recovery permission")
	}
	if driverOwnedBefore == "" || driverOwnedBefore != dr.definitionOf(driverOwned) {
		t.Fatal("pool policy changed the definition of a step the executor never sees")
	}
}

func TestComputePlacementDriverBindsPolicyBeforePoolEffects(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(map[bool]string{false: "captured policy", true: "invalid policy"}[invalid], func(t *testing.T) {
			manifest := strings.Replace(driveManifest, "          run: go vet ./...", "          placement: pool\n          platform: linux/amd64\n          run: go vet ./...", 1)
			dh := newDriveHarness(t, manifest)
			dh.p.Compute = pipelines.ComputeClusterAndFleet
			dh.store.addPipeline(dh.p)
			result := "{fleetWaitSeconds: 60, cloudFallback: true}"
			if invalid {
				result = "{fleetWaitSeconds: 3601, cloudFallback: true}"
			}
			a := workflowSource(t, "@template\nautomation pipelineComputePlacementPolicy { args { repository string! pipelineId string! ownerUserId string! event string! } return "+result+" }")
			dh.integ.Configure(func(d *Deps) { bindTestWorkflow(d, a) })
			run := dh.openRun(t, prOpening())
			deliver(t, dh.integ, run)
			got, _ := dh.store.run(run.ID)
			if invalid {
				if got.Conclusion == ConclusionSuccess || len(dh.exec.sent()) != 0 || got.WorkRunID != "" {
					t.Fatalf("invalid policy reached effects: %+v", got)
				}
				return
			}
			if got.Conclusion != ConclusionSuccess {
				t.Fatalf("pool run failed: %+v", got)
			}
			found := false
			for _, req := range dh.exec.sent() {
				// Every command step carries the one installed policy: its retries
				// apply to all of them. Only the pool step uses the rest of it.
				want := pipelines.ComputePlacementPolicy{FleetWaitSeconds: 60, CloudFallback: true, Retries: pipelines.DefaultRetryPolicy()}
				if req.ComputePlacement == nil || !reflect.DeepEqual(*req.ComputePlacement, want) {
					t.Fatalf("step %s lost the installed policy: %+v", req.StepKey, req.ComputePlacement)
				}
				if req.Step.Placement == pipelines.PlacementPool {
					found = true
				}
			}
			if !found {
				t.Fatal("pool step did not reach executor")
			}
		})
	}
}
