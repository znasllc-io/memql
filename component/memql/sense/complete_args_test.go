package sense

import "testing"

// G2 (memql#2364): inside an automation body that declares args { }, Complete
// offers the declared field names.
const argsAutomationSrc = `@trigger(event="deploy.requested", concept="v1:cluster:deployment")
automation deployEngineCluster {
  args {
    environment     string   @required
    engineNodeTypes []string @required
  }
  step gate {
    logic deployGateGreen { env: en }
  }
}`

func TestComplete_OffersAutomationArgsFields(t *testing.T) {
	s := New(nil)
	// Cursor on the step line (line 8), typing "en".
	items := s.Complete(argsAutomationSrc, 8, 30, "x/automations.memql")
	var found bool
	for _, it := range items {
		if it.Label == "environment" && it.Kind == "variable" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected 'environment' args field in completions; got %d items", len(items))
	}
}

func TestComplete_NoArgsFieldsOutsideAutomation(t *testing.T) {
	s := New(nil)
	src := "logic plain {\n  body {\n    return 1\n  }\n}"
	for _, it := range s.Complete(src, 3, 5, "x/logic.memql") {
		if it.Detail == "args field" {
			t.Fatalf("args-field completion leaked outside an automation: %+v", it)
		}
	}
}
