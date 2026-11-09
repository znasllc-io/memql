package pipelines

import "testing"

func securityReportSpec() *Spec {
	s := reportSpec()
	s.Schedule = "weekly"
	for n := range s.Stages {
		s.Stages[n].Security = true
		s.Stages[n].On = []string{"schedule", "manual"}
	}
	s.Stages[1].Steps[0].SARIF.Destination = "goals"
	return s
}

func TestSecurityStagesCannotDisableRequiredChecks(t *testing.T) {
	for _, event := range []string{"push", "release", "pull_request", "merge_group", "full", "affected"} {
		t.Run(event, func(t *testing.T) {
			s := securityReportSpec()
			s.Stages[0].On = append(s.Stages[0].On, event)
			if Validate(s) == nil {
				t.Fatal("required check accepted an optional security switch")
			}
		})
	}
	for _, change := range []func(*Spec){
		func(s *Spec) { s.Schedule = "daily" },
		func(s *Spec) { s.Stages[0].On = nil },
		func(s *Spec) { s.Stages[0].On = []string{"manual"} },
		func(s *Spec) { s.Stages[1].Security = false },
		func(s *Spec) { s.Stages[1].Steps[0].SARIF.Destination = "unknown" },
	} {
		s := securityReportSpec()
		change(s)
		if Validate(s) == nil {
			t.Fatal("invalid optional security declaration admitted")
		}
	}
}

func TestSecurityReportCompilesLocallyAndRejectsPolicyEventBypass(t *testing.T) {
	s := securityReportSpec()
	input := CompileInput{Mode: ModeFull, Event: EventManual, Compute: ComputeCluster,
		StageSelection: &StageSelection{Included: []string{"analysis", "reports"}}}
	plan, refusal := Compile(s, input)
	if refusal != nil {
		t.Fatal(refusal)
	}
	if got := plan.Steps()[1]; got.SARIF.Destination != "goals" || got.Kind != StepSARIF || got.Run != "" {
		t.Fatalf("local report acquired a worker or lost its destination: %+v", got)
	}
	input.Event = EventPush
	if _, refusal := Compile(s, input); refusal == nil {
		t.Fatal("a custom policy admitted optional security work on a required push")
	}
}
