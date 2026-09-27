package dslgate

import (
	"strings"
	"testing"
)

// misplacedAutomation is the shape the ten fleet automations had: a whole,
// well-formed automation, in a domain file that is not automations.memql.
const misplacedAutomation = `use platform.mutations.{ updateInboundRequestStatus }

/// A verified inbound delivery arrived.
@trigger(event="node.created", concept="v1:platform:inboundRequest")
automation claimStripeDelivery {
  args {
    id     any
    source any
  }

  switch args.source {
    case "stripe" {
      mutation updateInboundRequestStatus(requestId: args.id, status: "failed", lastError: "no handler")
    }
    default { }
  }
}
`

// TestAnAutomationOutsideItsFileIsRefused is the gate's catch: the declaration
// is refused at its line, by name, naming the file it belongs in, with the
// stable rule id last.
func TestAnAutomationOutsideItsFileIsRefused(t *testing.T) {
	got := gateHits(ScanSource("fleet/billing.memql", misplacedAutomation, Options{}), GateConstructMisplaced)
	if len(got) != 1 {
		t.Fatalf("want one construct-misplaced violation, got %d: %v", len(got), got)
	}
	v := got[0]
	if v.File != "fleet/billing.memql" || v.Line != 5 || v.Kind != "automation" || v.Construct != "claimStripeDelivery" {
		t.Errorf("violation attributed to %s:%d %s %s, want fleet/billing.memql:5 automation claimStripeDelivery", v.File, v.Line, v.Kind, v.Construct)
	}
	const want = "automation claimStripeDelivery is declared in fleet/billing.memql, which the automation loader never reads: " +
		"a domain's automations load from its automations.memql and from no other file, so it would load as nothing, " +
		"with no skip and no warning. Move it to fleet/automations.memql [construct_misplaced]"
	if v.Detail != want {
		t.Errorf("detail:\n got %q\nwant %q", v.Detail, want)
	}
}

// TestTheSameAutomationInItsFileLoads is the reachable positive: the identical
// source, in the file the loader reads, is not a finding -- at any namespace
// depth.
func TestTheSameAutomationInItsFileLoads(t *testing.T) {
	for _, p := range []string{"fleet/automations.memql", "beta/sub/automations.memql"} {
		if got := gateHits(ScanSource(p, misplacedAutomation, Options{}), GateConstructMisplaced); len(got) != 0 {
			t.Errorf("%s: an automation in the file its loader reads was refused: %v", p, got)
		}
	}
}

// TestOnlyARestrictedKindIsPlaced: every other construct is read from any file
// of its domain, so a query, a logic or a concept outside its per-kind file is
// house style, not a finding. And a construct keyword that is not a top-level
// declaration -- in a doc comment, in a string, a call inside a body -- is not
// one either.
func TestOnlyARestrictedKindIsPlaced(t *testing.T) {
	src := `/// automation notADeclaration { in a doc comment.
concept brief {
  note string @description("automation inAString {")
}

query brief briefs {
  filter row => row.note != nil
  paginate 20
}

logic decide {
  args {
    x string
  }
  return args.x
}
`
	if got := gateHits(ScanSource("research/brief.memql", src, Options{}), GateConstructMisplaced); len(got) != 0 {
		t.Errorf("a file declaring no automation was refused: %v", got)
	}
}

// TestAHiddenOrRootFileIsNamedForWhatItIs: the two other ways a file is never
// read by the automation loader each get their own reason, and a file at the
// root of a tree, which no domain claims, names no home it cannot have.
func TestAHiddenOrRootFileIsNamedForWhatItIs(t *testing.T) {
	hidden := gateHits(ScanSource("fleet/.wip/automations.memql", misplacedAutomation, Options{}), GateConstructMisplaced)
	if len(hidden) != 1 || !strings.Contains(hidden[0].Detail, "the loader skips a directory whose name begins with _ or .") ||
		!strings.Contains(hidden[0].Detail, "Move it to fleet/automations.memql") {
		t.Errorf("an automation under a hidden directory: %v", hidden)
	}
	root := gateHits(ScanSource("billing.memql", misplacedAutomation, Options{}), GateConstructMisplaced)
	if len(root) != 1 || !strings.Contains(root[0].Detail, "at the root of the tree, in no domain directory") ||
		!strings.Contains(root[0].Detail, "Move it to the automations.memql of its domain") {
		t.Errorf("an automation at the root of a tree: %v", root)
	}
}

// TestTheGateRunsOverTheCorpusBootScans: boot calls ScanFiles, not ScanSource,
// so the refusal must survive that entry point -- one violation per misplaced
// declaration, and nothing for the file that is in its place.
func TestTheGateRunsOverTheCorpusBootScans(t *testing.T) {
	files := []SourceFile{
		{Path: "fleet/automations.memql", Content: strings.Replace(misplacedAutomation, "claimStripeDelivery", "welcomeOnInstanceRunning", 1)},
		{Path: "fleet/trial.memql", Content: "@trigger(schedule=\"0 0 9 * * *\")\nautomation trialNudgeDay7 {\n  return\n}\n\n@trigger(schedule=\"0 0 2 * * *\")\nautomation trialExpirySuspend {\n  return\n}\n"},
	}
	got := gateHits(ScanFiles(files, Options{}), GateConstructMisplaced)
	if len(got) != 2 || got[0].Construct != "trialNudgeDay7" || got[1].Construct != "trialExpirySuspend" ||
		got[0].File != "fleet/trial.memql" || got[0].Line != 2 || got[1].Line != 7 {
		t.Fatalf("ScanFiles: want trialNudgeDay7 at fleet/trial.memql:2 and trialExpirySuspend at :7, got %v", got)
	}
}
