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

// TestAutomationCondition_EditionTwentySix: the same rule in the forms the
// edition-2026 codemod writes (epic memql#5363). coalesce and concat become
// the `??` and `+` operators, and a trigger filter becomes a lambda -- which
// opens a group at once, so a condition read to the first `)` stops inside it.
func TestAutomationCondition_EditionTwentySix(t *testing.T) {
	catches := []struct {
		name string
		src  string
		rule string
	}{
		{"`??` default in a lambda @filter", `@filter(row => (row.kind ?? "regular") == "daily")
automation probe {
  step run { logic f ( event ) }
}`, "automation-condition-builtin"},
		{"date math with `+` and `??` in an if", `automation probe {
  step apply {
    forEach item in decide.result {
      if addDuration(item.createdAt, "P" + (window.first().payload.value ?? "30") + "D") < now {
        mutation expire ( id: item.id )
      }
    }
  }
}`, "automation-condition-builtin"},
		{"`+` concat in an if", `automation probe {
  step apply {
    if "role:" + event.node.payload.role == "role:admin" {
      mutation m ( id: id )
    }
  }
}`, "automation-condition-builtin"},
		{"vocabulary in a lambda @filter", `@filter(row => row.status == "running" || row.status == "compiling")
automation probe {
  step run { logic f ( event ) }
}`, "automation-condition-vocabulary"},
		// The legacy form, past the first `)`: the old capture read
		// `exists(event.node.id` and nothing after it.
		{"a builtin after a call in a legacy @filter", `@filter(exists(event.node.id) && concat(event.node.payload.a, "x") == "yx")
automation probe {
  step run { logic f ( event ) }
}`, "automation-condition-builtin"},
	}
	for _, tc := range catches {
		t.Run(tc.name, func(t *testing.T) {
			for _, f := range automationFindings(t, tc.src) {
				if f.Rule == tc.rule {
					return
				}
			}
			t.Fatalf("expected a %s finding, got %v", tc.rule, automationFindings(t, tc.src))
		})
	}

	passes := []struct {
		name string
		src  string
	}{
		{"relevance lambda @filter", `@filter(row => row.preferences.computerUseEnabled == false)
automation probe {
  step run { logic f ( event ) }
}`},
		{"nil presence and a decided gate", `automation probe {
  step decide { logic decideThing ( event ) }
  step teardown {
    if steps.decide.result == true && event.node.id != nil {
      builtin teardown ( planId: event.node.id )
    }
  }
}`},
		{"a group inside a lambda @filter", `@filter(row => (row.kind == "file" && row.archived == true))
automation probe {
  step run { logic f ( event ) }
}`},
	}
	for _, tc := range passes {
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
