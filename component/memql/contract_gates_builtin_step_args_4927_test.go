package memql

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql/baseloader"
)

// The builtin-step gate, both directions (memql#4927).
//
// A gate that never fires passes the control; a gate that always fires passes
// the rule. Only both together say anything -- the standing form of these
// pairs, from contract_gates_subautomation_boot_4471_test.go.
//
// The shape under test is the one that shipped: a builtin declared
// `@args(profile="object")` with an EMPTY body, and a scheduled automation
// calling it `builtin NAME ()`. Every file is well-formed on its own, every
// suite in the repo is green, and the step dies at parse on the cron leader
// every two minutes for as long as it is deployed.

// The probe drives the REAL registry, so the builtins named below are ones the
// embedded tree actually declares. A hand-written fixture builtin would have to
// be loaded into a registry first, and a gate answered by a registry built for
// the test is a gate answered by the test.
const gate4927RealCall = `
@trigger(schedule="0 */2 * * * *")
automation probeScheduled {
  step sweep {
    builtin %s ()
  }
}
`

func scan4927Real(t *testing.T, builtin string) []string {
	t.Helper()
	report := newLoadReport()
	corpus := []baseloader.RawFile{{
		Path:    "probe/automations.memql",
		Content: strings.Replace(gate4927RealCall, "%s", builtin, 1),
	}}
	engine := newParserTestEngine(t)
	recordContractGateProblems(report, corpus, engine.functions)

	var hits []string
	for _, s := range report.Skipped {
		if strings.HasPrefix(s.Phase, "contract-gate:builtin-step-args") {
			hits = append(hits, s.Err)
		}
	}
	return hits
}

// THE RULE. A builtin whose profile refuses an empty call, called with one.
func TestBuiltinStepGateRefusesAnEmptyCallTheProfileRejects(t *testing.T) {
	// `packageNoteUpstreamFromWebhook` is declared @args(profile="object")
	// with three fields, so an empty call is exactly the memql#4927 shape.
	hits := scan4927Real(t, "packageNoteUpstreamFromWebhook")
	if len(hits) != 1 {
		t.Fatalf("want 1 violation, got %d: %v", len(hits), hits)
	}
	for _, want := range []string{
		"requires a JSON object argument",
		"optionalObject",
		"fails every time the step RUNS",
	} {
		if !strings.Contains(hits[0], want) {
			t.Errorf("violation does not mention %q:\n%s", want, hits[0])
		}
	}
}

// THE CONTROL. The same call shape against a builtin whose profile admits it
// must pass, or the gate is refusing every scheduled automation in the tree.
func TestBuiltinStepGateAdmitsAnEmptyCallTheProfileAccepts(t *testing.T) {
	for _, name := range []string{
		"customDomainReconcile",
		"packageSweepAbandoned",
		"packagePollUpstream",
		"logsSweep",
	} {
		if hits := scan4927Real(t, name); len(hits) != 0 {
			t.Errorf("%s: want no violation, got %v", name, hits)
		}
	}
}

// A name this build resolves to no builtin is passed over rather than
// reported: a product bundle at MEMQL_DSL_PATH may call one this tree cannot
// see, and refusing a boot over it would make the gate narrower than the
// resolver it stands in front of.
func TestBuiltinStepGateIsSilentOnABuiltinItCannotSee(t *testing.T) {
	if hits := scan4927Real(t, "somethingNoBuildDeclares"); len(hits) != 0 {
		t.Errorf("want no violation for an unknown name, got %v", hits)
	}
}

// The whole embedded tree must be clean, or this gate cannot ship.
func TestBuiltinStepGatePassesOnTheEmbeddedTree(t *testing.T) {
	engine := newParserTestEngine(t)
	report := newLoadReport()
	recordContractGateProblems(report, baseloader.ReadAll(nil), engine.functions)

	for _, s := range report.Skipped {
		if strings.HasPrefix(s.Phase, "contract-gate:builtin-step-args") {
			t.Errorf("%s: %s", s.File, s.Err)
		}
	}
}
