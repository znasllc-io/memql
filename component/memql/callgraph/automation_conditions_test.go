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
  if submitterRole == "admin" || submitterRole == "writer" {
    apply := mutation advanceRequest(requestId: id)
  }
}`, "automation-condition-vocabulary"},
		{"retention date-math if", `automation probe {
  for item in decide.result {
    if addDuration(item.createdAt, "P" + (window.first().value ?? "30") + "D") < now {
      mutation expire(id: item.id)
    }
  }
}`, "automation-condition-builtin"},
		{"workbench terminal ||-chain", `automation probe {
  if event.node.payload.status == "succeeded" || event.node.payload.status == "failed" || event.node.payload.status == "cancelled" {
    teardown := builtin teardown(planId: event.node.id)
  }
}`, "automation-condition-vocabulary"},
		{"`??` default in @filter", `@filter(row => (row.kind ?? "regular") == "daily")
automation probe {
  run := logic f(event: event)
}`, "automation-condition-builtin"},
		{"`+` concat in an if", `automation probe {
  if "role:" + event.node.payload.role == "role:admin" {
    apply := mutation m(id: id)
  }
}`, "automation-condition-builtin"},
		{"vocabulary in a lambda @filter", `@filter(row => row.status == "running" || row.status == "compiling")
automation probe {
  run := logic f(event: event)
}`, "automation-condition-vocabulary"},
		{"vocabulary in forEach where", `automation probe {
  for nt in engineNodeTypes if nt == "voice" || nt == "bff" {
    logic f(nt: nt)
  }
}`, "automation-condition-vocabulary"},
		// Two shapes of a statement body that open no line with `if`.
		{"vocabulary in an else-if", `automation probe {
  if args.a == nil {
    logic f()
  } else if args.status == "failed" || args.status == "cancelled" {
    logic g()
  }
}`, "automation-condition-vocabulary"},
		{"date math in a one-line if", `automation probe {
  if addDuration(args.at, "P7D") < now { logic f() }
}`, "automation-condition-builtin"},
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
    if steps.terminal.result == true && event.node.id != nil {
      builtin teardown ( planId: event.node.id )
    }
  }
}`},
		{"relevance @filter equality", `@filter(row => event.node.payload.preferences.computerUseEnabled == false)
automation probe {
  run := logic f(event: event)
}`},
		{"switch fan-out on decided value", `automation probe {
  decide := logic decideThing(event: event)
  switch decide {
    case "queued" {
      mutation m(id: id)
    }
    default {
      mutation n(id: id)
    }
  }
}`},
		{"where single equality fan-out", `automation probe {
  for nt in engineNodeTypes if environment == "development" {
    logic f(nt: nt)
  }
}`},
		{"relevance lambda @filter on the row", `@filter(row => row.preferences.computerUseEnabled == false)
automation probe {
  run := logic f(event: event)
}`},
		{"a group inside a lambda @filter", `@filter(row => (row.kind == "file" && row.archived == true))
automation probe {
  run := logic f(event: event)
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

// TestAnnotationArgsReadsToTheBalancedParen pins the extraction itself.
func TestAnnotationArgsReadsToTheBalancedParen(t *testing.T) {
	got := annotationArgs(`@filter(row => (row.a == ")") && f(row.b))
automation x {}`, automationFilterRE)
	if len(got) != 1 || got[0] != `row => (row.a == ")") && f(row.b)` {
		t.Fatalf("annotationArgs = %q", got)
	}
}
