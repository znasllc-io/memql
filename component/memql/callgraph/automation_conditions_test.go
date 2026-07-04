package callgraph

// P4 (memql#2371): the automation condition-vocabulary rule. Re-introducing
// any migrated pattern (forge role-if, retention date-math-if, workbench
// status ||-chain) is a finding; the sanctioned shapes are not.

import "testing"

func automationFindings(t *testing.T, src string) []Finding {
	t.Helper()
	return ConstructFindings("automation", "probe", src, map[string]string{}, func(string) bool { return false })
}

func TestAutomationCondition_MigratedPatternsAreFindings(t *testing.T) {
	cases := []struct {
		name string
		src  string
		rule string
	}{
		{"forge role-if vocabulary", `automation probe {
  step apply {
    if submitterRole == "admin" || submitterRole == "writer" {
      mutation advanceRequest ( requestId: id )
    }
  }
}`, "automation-condition-vocabulary"},
		{"retention date-math if", `automation probe {
  step apply {
    forEach item in decide.result {
      if addDuration(item.createdAt, concat("P", coalesce(window.first().payload.value, "30"), "D")) < now {
        mutation expire ( id: item.id )
      }
    }
  }
}`, "automation-condition-builtin"},
		{"workbench terminal ||-chain", `automation probe {
  step teardown {
    if event.node.payload.status == "succeeded" || event.node.payload.status == "failed" || event.node.payload.status == "cancelled" {
      builtin teardown ( planId: event.node.id )
    }
  }
}`, "automation-condition-vocabulary"},
		{"coalesce default in @filter", `@filter(coalesce(payload.kind, "regular") == "daily")
automation probe {
  step run { logic f ( event ) }
}`, "automation-condition-builtin"},
		{"vocabulary in forEach where", `automation probe {
  step fan {
    forEach nt in engineNodeTypes where nt == "voice" || nt == "bff" {
      logic f ( nt )
    }
  }
}`, "automation-condition-vocabulary"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := automationFindings(t, tc.src)
			for _, f := range fs {
				if f.Rule == tc.rule {
					return
				}
			}
			t.Fatalf("expected a %s finding, got %v", tc.rule, fs)
		})
	}
}

func TestAutomationCondition_SanctionedShapesPass(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{"decide-gate + presence + single fan-out equality", `automation probe {
  step decide { logic decideThing ( event ) }
  step apply {
    forEach item in decide.nodes() {
      if steps.decide.result == true && item.payload.status == "provisioned" {
        mutation release ( id: item.id )
      }
    }
  }
  step teardown {
    if steps.terminal.result == true && exists(event.node.id) {
      builtin teardown ( planId: event.node.id )
    }
  }
}`},
		{"relevance @filter equality", `@filter(event.node.payload.preferences.computerUseEnabled == false)
automation probe {
  step run { logic f ( event ) }
}`},
		{"switch fan-out on decided value", `automation probe {
  step decide { logic decideThing ( event ) }
  step advance {
    switch steps.decide.result {
      case "queued" { mutation m ( id: id ) }
      default { mutation n ( id: id ) }
    }
  }
}`},
		{"where single equality fan-out", `automation probe {
  step fan {
    forEach nt in engineNodeTypes where environment == "development" {
      logic f ( nt )
    }
  }
}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if fs := automationFindings(t, tc.src); len(fs) != 0 {
				t.Fatalf("sanctioned shape must produce zero findings, got %v", fs)
			}
		})
	}
}
