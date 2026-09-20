package ci

// The differential lane is REQUIRED (memql#5386, D5 and D23 of
// docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md).
//
// "Required" is two separate facts, and each fails open on its own:
//
//  1. The job that runs test/conformance sets MEMQL_DIFFERENTIAL_REQUIRED=1.
//     Without it the lane logs a disagreement on a DIFFERENTIAL: line and
//     passes -- green over a language whose two evaluators disagree.
//  2. That job is one of ci-required's needs. A lane outside the aggregate is
//     advisory however red it goes, which is the memql#3019 fail-open shape.
//
// test/conformance is deliberately NOT in DB_GATED_TREES: it runs in its own
// seeded lane (scripts/cidb/dsnliteral_test.go says so by name), and adding it
// would run the suite twice per PR. This test is what makes the routing
// falsifiable instead of a comment.

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

const differentialWorkflowPath = "../../.github/workflows/ci.yml"

// conformanceJobKey is the YAML key of the job that runs
// ./test/conformance/... against a seeded database. Its `name:` is
// `mcp-conformance`; the key is what ci-required's `needs` list names, and
// `needs` is the membership this file gates.
const conformanceJobKey = "conformance"

func readDifferentialWorkflow(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(differentialWorkflowPath)
	if err != nil {
		t.Fatalf("read %s: %v", differentialWorkflowPath, err)
	}
	return string(b)
}

// differentialJobBlock returns the YAML block of one top-level job, from its
// two-space key to the next two-space key at the same indent.
func differentialJobBlock(t *testing.T, doc, key string) string {
	t.Helper()
	start := regexp.MustCompile(`(?m)^  ` + regexp.QuoteMeta(key) + `:\s*$`)
	loc := start.FindStringIndex(doc)
	if loc == nil {
		t.Fatalf("no job %q in %s -- if the job was renamed, this gate needs the new key, "+
			"not deleting", key, differentialWorkflowPath)
	}
	rest := doc[loc[1]:]
	next := regexp.MustCompile(`(?m)^  [a-z][a-z0-9-]*:\s*$`).FindStringIndex(rest)
	if next == nil {
		return rest
	}
	return rest[:next[0]]
}

func TestDifferentialLaneRunsInRequiredMode(t *testing.T) {
	block := differentialJobBlock(t, readDifferentialWorkflow(t), conformanceJobKey)
	if !strings.Contains(block, "MEMQL_DIFFERENTIAL_REQUIRED") {
		t.Fatalf("the %s job does not set MEMQL_DIFFERENTIAL_REQUIRED.\n"+
			"Without it a disagreement between memql.Lower and memql.EvalExpr is LOGGED and the "+
			"lane passes: the one place the language has two implementations of one meaning goes "+
			"green while they disagree. Add `MEMQL_DIFFERENTIAL_REQUIRED: \"1\"` to the job's env.",
			conformanceJobKey)
	}
	if !regexp.MustCompile(`MEMQL_DIFFERENTIAL_REQUIRED:\s*"?1"?`).MatchString(block) {
		t.Fatalf("the %s job names MEMQL_DIFFERENTIAL_REQUIRED but not with the value 1; "+
			"differentialRequired() compares against exactly \"1\", so any other value is off",
			conformanceJobKey)
	}
	if !strings.Contains(block, "./test/conformance/") {
		t.Fatalf("the %s job no longer runs ./test/conformance/..., so setting "+
			"MEMQL_DIFFERENTIAL_REQUIRED there gates nothing", conformanceJobKey)
	}
}

func TestDifferentialLaneIsInsideCIRequired(t *testing.T) {
	needs := differentialJobBlock(t, readDifferentialWorkflow(t), "ci-required")
	if !regexp.MustCompile(`(?m)^      - `+regexp.QuoteMeta(conformanceJobKey)+`\s*$`).MatchString(needs) {
		t.Fatalf("ci-required does not list %q in needs.\n"+
			"A lane outside the aggregate is advisory however red it goes -- the merge queue "+
			"never sees it. The differential lane is required (D23), so its job belongs in needs.",
			conformanceJobKey)
	}
}
