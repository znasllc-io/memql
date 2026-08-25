package automations

import "testing"

// A sub-automation step is authored `automation <name>( ... )` -- the same
// kind-prefixed invocation shape as `logic <name>( ... )`, and `automation` has
// been a legal kind prefix in the rewriter all along.
//
// THE DEFECT THIS GUARDS (epic memql#4463). automationLooseHeader existed to
// find a real declaration whose `{` sits on the next line, so that an
// automation could never silently vanish (#2830). It matched
// `^[ \t]*automation[ \t]+NAME` with no trailer, which is also exactly what a
// CALL SITE looks like once indented inside a step body. So authoring a
// composition reported the CALLEE as a header whose opening brace was missing,
// and -- because an unextractable header refuses boot -- a correctly-authored
// automation took the node down.
//
// The failure was badly misleading in a way worth recording: the callee's real
// declaration elsewhere in the same file extracted fine, so the diagnostic
// named a block whose braces balance perfectly and blamed its braces. Nothing
// pointed at the call site, and the corpus contained no sub-automation calls to
// compare against.
func TestSubAutomationCallIsNotMistakenForAHeader(t *testing.T) {
	const source = `
@trigger(event="child.requested")
automation childVerb {
  args {
    note string!
  }
  step act {
    action notifyDeploy(status: "succeeded", deploymentId: note, dryRun: false)
  }
}

@trigger(event="parent.requested")
automation parentVerb {
  args {
    note string!
  }
  step delegate {
    automation childVerb( note: note )
  }
  step delegateMultiline {
    automation childVerb(
      note
    )
  }
}
`

	slices, unextracted := extractAutomationSlicesReporting(source)

	if len(unextracted) != 0 {
		t.Errorf("a sub-automation CALL was reported as an unextractable header: %v", unextracted)
	}

	got := make(map[string]bool, len(slices))
	for _, s := range slices {
		got[s.Name] = true
	}
	for _, want := range []string{"childVerb", "parentVerb"} {
		if !got[want] {
			t.Errorf("declaration %q was not extracted; got %v", want, got)
		}
	}
	if len(slices) != 2 {
		t.Errorf("expected exactly 2 declarations, got %d (%v) -- a call site was extracted as one", len(slices), got)
	}
}

// The loose header must still catch the case it was written for: a real
// declaration whose opening brace is on the NEXT line. Fixing the call-site
// false positive must not blind it to the silent-loss case (#2830).
func TestLooseHeaderStillReportsBraceOnNextLine(t *testing.T) {
	const source = `
@trigger(event="x.y")
automation braceOnNextLine
{
  step s {
    action notifyDeploy(status: "succeeded", deploymentId: "d", dryRun: false)
  }
}
`

	_, unextracted := extractAutomationSlicesReporting(source)

	if len(unextracted) != 1 || unextracted[0] != "braceOnNextLine" {
		t.Errorf("a declaration with its brace on the next line must still be reported as unextractable, got %v", unextracted)
	}
}
