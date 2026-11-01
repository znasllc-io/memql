package pipelines

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// repeatable_test.go -- the manifest's `repeatable` key and the installation's
// retry limits as the contract types carry them (spec D5, D6 and section 6 of
// the 2026-10-28 fleet-first retries design).
//
// The key matters because it is the only thing that lets the engine run a
// command a second time after an attempt whose outcome it cannot know (a
// dropped connection, a node that went away): a step that does not say that
// running it twice is harmless is never run twice. So the resolution order has
// to be exact, and the contradiction has to be refused where the author reads it.

// repeatableSpec has one step of every kind the key resolves differently: one
// that says nothing, one that opts in, one that opts out, and one verification
// step (digest-pinned image, no effects) that must always be repeatable.
func repeatableSpec(pipelineDefault bool) *Spec {
	yes, no := true, false
	return &Spec{
		Image:      "registry.example/toolchain@sha256:" + strings.Repeat("a", 64),
		Repeatable: pipelineDefault,
		Stages: []StageSpec{{Name: "checks", Steps: []StepSpec{
			{Name: "plain", Run: "make check"},
			{Name: "optin", Run: "make flaky", Repeatable: &yes},
			{Name: "optout", Run: "make release", Repeatable: &no},
			{Name: "verify", Run: "go test ./unit", Verification: true},
		}}},
	}
}

func compiledRepeatable(t *testing.T, spec *Spec) map[string]bool {
	t.Helper()
	plan, refusal := Compile(spec, CompileInput{Mode: ModeFull, Event: EventPush, Compute: ComputeCluster})
	if refusal != nil {
		t.Fatalf("the spec did not compile: %v", refusal)
	}
	out := map[string]bool{}
	for _, step := range plan.Steps() {
		out[step.Name] = step.Repeatable
	}
	return out
}

func TestRepeatableManifestKey(t *testing.T) {
	t.Run("resolution", func(t *testing.T) {
		for _, c := range []struct {
			name            string
			pipelineDefault bool
			want            map[string]bool
		}{
			{
				// Absent means false: a step is never run twice on a guess.
				name: "absent is false, and only a step's own word or verification changes it", pipelineDefault: false,
				want: map[string]bool{"plain": false, "optin": true, "optout": false, "verify": true},
			},
			{
				// The pipeline's value is the default of its steps; a step that
				// says otherwise wins.
				name: "pipeline repeatable: true is the default of its steps, and a step's false overrides it", pipelineDefault: true,
				want: map[string]bool{"plain": true, "optin": true, "optout": false, "verify": true},
			},
		} {
			t.Run(c.name, func(t *testing.T) {
				spec := repeatableSpec(c.pipelineDefault)
				if refusal := Validate(spec); refusal != nil {
					t.Fatalf("the fixture must be a valid pipeline: %v", refusal)
				}
				got := compiledRepeatable(t, spec)
				if !reflect.DeepEqual(got, c.want) {
					t.Fatalf("repeatable by step = %v, want %v", got, c.want)
				}
			})
		}
	})

	// A verification command is deterministic, reads only its inputs and
	// produces no artifacts (validateVerification), so running it again can only
	// repeat an answer. Declaring that it must not be repeated says two opposite
	// things about one command; the manifest is refused instead of one of them
	// being chosen.
	t.Run("a verification step cannot opt out of repeating", func(t *testing.T) {
		for _, pipelineDefault := range []bool{false, true} {
			spec := repeatableSpec(pipelineDefault)
			no := false
			spec.Stages[0].Steps[3].Repeatable = &no

			const want = "A verification step is repeatable; remove repeatable: false."
			for name, refusal := range map[string]*Refusal{
				"Validate": Validate(spec),
				"Compile": func() *Refusal {
					_, r := Compile(spec, CompileInput{Mode: ModeFull, Event: EventPush, Compute: ComputeCluster})
					return r
				}(),
			} {
				if refusal == nil || refusal.Code != CodeStepInvalid || refusal.Detail != want || refusal.Scope != "checks/verify" {
					t.Errorf("%s (pipeline repeatable: %v) = %+v, want pipeline_step_invalid at checks/verify saying %q", name, pipelineDefault, refusal, want)
				}
			}
		}

		// Saying the same thing twice is not a contradiction.
		spec := repeatableSpec(true)
		yes := true
		spec.Stages[0].Steps[3].Repeatable = &yes
		if refusal := Validate(spec); refusal != nil {
			t.Errorf("verification with repeatable: true was refused: %v", refusal)
		}

		// The contradiction is about the declaration, so it is reported even when
		// the step is invalid for another reason too: a mutable image is a second
		// problem the author fixes next.
		spec = repeatableSpec(false)
		no := false
		spec.Stages[0].Steps[3].Repeatable = &no
		spec.Image = "registry.example/toolchain:latest"
		if refusal := Validate(spec); refusal == nil || refusal.Detail != "A verification step is repeatable; remove repeatable: false." {
			t.Errorf("a step that is both contradictory and mutable reported %+v, want the contradiction first", refusal)
		}
	})

	// The driver delivers a notification, a SARIF report and a repository change
	// itself, and never hands one to the executor, so nothing can run them twice.
	// Their steps are not repeatable whatever the pipeline's default says, and a
	// manifest cannot pretend otherwise: those steps accept only the keys their
	// refusal message lists.
	t.Run("driver-owned steps are never repeatable", func(t *testing.T) {
		spec := reportSpec()
		spec.Repeatable = true
		spec.Stages = append(spec.Stages, StageSpec{Name: "tell", Channel: "ops"})
		plan, refusal := Compile(spec, CompileInput{Mode: ModeFull, Event: EventPush, Compute: ComputeCluster})
		if refusal != nil {
			t.Fatal(refusal)
		}
		kinds := map[StepKind]int{}
		for _, step := range plan.Steps() {
			kinds[step.Kind]++
			switch step.Kind {
			case StepCommand:
				if !step.Repeatable {
					t.Errorf("command step %s did not take the pipeline's repeatable: true", step.Key)
				}
			default:
				if step.Repeatable {
					t.Errorf("%s step %s is repeatable; the driver delivers it and the executor never sees it", step.Kind, step.Key)
				}
			}
		}
		if kinds[StepCommand] != 1 || kinds[StepSARIF] != 1 || kinds[StepNotify] != 1 {
			t.Fatalf("the fixture must plan one command, one SARIF and one notify step, got %v", kinds)
		}

		yes := true
		for name, spec := range map[string]*Spec{
			"sarif": func() *Spec {
				s := reportSpec()
				s.Stages[1].Steps[0].Repeatable = &yes
				return s
			}(),
			"repositoryChange": func() *Spec {
				s := &Spec{Image: "toolchain", Caches: []string{"go"}, Stages: []StageSpec{
					{Name: "proposal", Steps: []StepSpec{{Name: "generate", Run: "prepare", Artifacts: []string{"proposal.json"}}}},
					{Name: "apply", Steps: []StepSpec{{Name: "change", RepositoryChange: &RepositoryChange{Rule: "tidy", Step: "proposal.generate", Path: "proposal.json"}}}},
				}}
				s.Stages[1].Steps[0].Repeatable = &yes
				return s
			}(),
		} {
			refusal := Validate(spec)
			if refusal == nil || refusal.Code != CodeStepInvalid {
				t.Errorf("a %s step declaring repeatable = %+v, want pipeline_step_invalid: the driver owns its execution, so the key would be ignored in silence", name, refusal)
			}
		}
	})
}

func TestDefaultRetryPolicyIsThePortableOne(t *testing.T) {
	got := DefaultRetryPolicy()
	if got.SameMachine != 2 || !reflect.DeepEqual(got.DelaysSeconds, []int{30, 120}) || !got.OtherMachines {
		t.Fatalf("DefaultRetryPolicy() = %+v, want sameMachine 2, delaysSeconds [30 120], otherMachines true", got)
	}
	// Each call is its own value: a caller that edits its delays must not edit
	// the default another run reads.
	got.DelaysSeconds[0] = 999
	if again := DefaultRetryPolicy(); again.DelaysSeconds[0] != 30 {
		t.Fatalf("an edit to one default reached the next: %+v", again)
	}
}

func TestAPolicyCloneSharesNoStorage(t *testing.T) {
	original := ComputePlacementPolicy{FleetWaitSeconds: 60, CloudFallback: true, Retries: DefaultRetryPolicy()}
	clone := original.Clone()
	clone.CloudFallback = false
	clone.Retries.SameMachine = 0
	clone.Retries.DelaysSeconds[1] = 1
	if !original.CloudFallback || original.Retries.SameMachine != 2 || original.Retries.DelaysSeconds[1] != 120 {
		t.Fatalf("editing a clone edited the policy it came from: %+v", original)
	}
	if !reflect.DeepEqual(ComputePlacementPolicy{}.Clone(), ComputePlacementPolicy{}) {
		t.Error("the zero policy does not clone to itself")
	}
}

// The retry limits cross NodeService inside the step request, and the key
// crosses the analysis report inside the spec, so their names are a wire
// contract: a rename fails here instead of as a limit that silently goes
// missing on another node.
func TestRetryAndRepeatableWireNamesArePinned(t *testing.T) {
	raw, err := json.Marshal(StepRequest{
		ComputePlacement: &ComputePlacementPolicy{FleetWaitSeconds: 60, CloudFallback: true, Retries: DefaultRetryPolicy()},
		Step:             Step{Repeatable: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(raw, &request); err != nil {
		t.Fatal(err)
	}
	policy, _ := request["computePlacement"].(map[string]any)
	if keys := sortedKeys(policy); !reflect.DeepEqual(keys, []string{"cloudFallback", "fleetWaitSeconds", "retries"}) {
		t.Errorf("ComputePlacementPolicy JSON keys = %v, want [cloudFallback fleetWaitSeconds retries]", keys)
	}
	retries, _ := policy["retries"].(map[string]any)
	if keys := sortedKeys(retries); !reflect.DeepEqual(keys, []string{"delaysSeconds", "otherMachines", "sameMachine"}) {
		t.Errorf("RetryPolicy JSON keys = %v, want [delaysSeconds otherMachines sameMachine]", keys)
	}
	if retries["sameMachine"] != float64(2) || retries["otherMachines"] != true || !reflect.DeepEqual(retries["delaysSeconds"], []any{float64(30), float64(120)}) {
		t.Errorf("retries on the wire = %v, want the portable default", retries)
	}
	if step, _ := request["step"].(map[string]any); step["repeatable"] != true {
		t.Errorf("a repeatable step on the wire = %v, want repeatable true", request["step"])
	}

	// A step that is not repeatable says nothing at all, so a peer that has
	// never heard of the key reads what it always read.
	raw, err = json.Marshal(Step{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "repeatable") {
		t.Errorf("a step that is not repeatable carries the key anyway: %s", raw)
	}

	// The manifest side: false is a statement a step can make over a pipeline's
	// true, so it must survive the analysis report; the pipeline's own false is
	// just the default, so it is not written.
	no := false
	raw, err = json.Marshal(StepSpec{Name: "release", Run: "publish", Repeatable: &no})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"repeatable":false`) {
		t.Errorf("an explicit repeatable: false was dropped from the spec: %s", raw)
	}
	raw, err = json.Marshal(Spec{Repeatable: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"repeatable":true`) {
		t.Errorf("the pipeline's repeatable: true was dropped from the spec: %s", raw)
	}
	raw, err = json.Marshal(Spec{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "repeatable") {
		t.Errorf("the pipeline default is written into every spec: %s", raw)
	}
}
