# DSL v1 Freeze Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make MemQL DSL version one a property of the tree rather than a date: the
differential lane red on disagreement, a committed fuzz seed corpus, a generated BNF and
vocabulary served over the introspection builtins, `memqlbreaking` classifying every
corpus-to-corpus change, and the version manifest flipped to edition 2026 / language 1.0
with the extension released in the same PR.

**Architecture:** Five independent deliverables over one branch and one PR. Each rests on
machinery the previous five epics already landed -- `dslspec.Build()` over the parser and
annotation registry, the 529-file conformance corpus with its `expect.json` verdicts, the
`differential_db_test.go` lane, and `cmd/attributematrix` as the generated-docs precedent.
Nothing here invents a new source of truth; every artifact is DERIVED from the registry,
the tier manifest and the function catalog, and every one carries a `-check` gate so a
hand edit is refused.

**Tech Stack:** Go 1.27.1, `testing.F` fuzzing, TimescaleDB (mcp-conformance service
container), GitHub Actions, the VS Code extension under `editors/vscode`.

**Spec:** `docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md`
(section 4, Epic 6; decisions D4, D5, D21, D22, D23, D24, D25).

**Issues:** memql#5385 (epic) closing #5386, #5387, #5388, #5389, #5390.

## Global Constraints

- **Branch `epic/dsl-v1-freeze`, worktree `/home/znas/memql-projects/epic-dsl-v1-freeze`.**
  Never commit to `main`. One PR closes all five task issues.
- **Verify with `make test`, never `go test ./...`** -- a bare `./...` resolves inside one
  workspace module and misses `component/memql`, `component/database` and
  `component/language`, printing `ok` over 64 packages having never touched the engine.
- **db-gated cases self-skip without Postgres.** Point `MEMQL_DATABASE_DSN` at the real
  throwaway Postgres on **port 15434** (not 5432 -- an open 5432 is the k3d load balancer,
  which accepts the connection and then EOFs, so every db case skips silently).
  `MEMQL_REQUIRE_DB=1` turns a skip into a failure.
- **No emojis** in any documentation, generated file, test output or commit message. Use
  `SUCCESS:` / `ERROR:` / `WARNING:` / `INFO:` and `[ ]` / `[x]`.
- **Stage files by explicit path** (`git add <file>`); never `git add -A` or `git add .` --
  the owner runs several sessions against sibling checkouts.
- **This plan file is deleted in the epic's merge** (the epic bodies say so).
- **Every generated artifact carries a `-check` gate.** The precedent is
  `cmd/attributematrix` + `dslspec.AttributeMatrixPath` + `make docs-matrix` /
  `make docs-matrix-check` + `TestAttributeMatrixIsGenerated` in the root package. Follow
  it exactly; a generated page with no gate rots.
- **`component/language` owns the language.** The parser, the registry, the tiers, the
  functions and the generated grammar live there; `component/memql` lowers and evaluates.
  No vocabulary lives outside the first.
- **Commit after every task**, with the issue number in the subject.

---

### Task 1: The differential lane becomes required (#5386)

The lane already exists and already runs against a real TimescaleDB inside the
`mcp-conformance` job, which is already one of `ci-required`'s `needs`. Three things are
missing: a disagreement currently logs and passes; nothing proves the lane can go red; and
the generated half is a hand-written matrix rather than a grammar-driven generator.

**Decision taken with the owner (2026-09-20):** `test/conformance` stays OUT of
`DB_GATED_TREES`. It is deliberately exempt (`scripts/cidb/dsnliteral_test.go`: "runs in
the separate mcp-conformance lane"), and adding it would run the whole 10-minute suite
twice per PR for no property gain. The acceptance's property -- the lane is in the required
set and a disagreement is red -- is satisfied by the mcp-conformance job plus a new gate
that asserts both facts executably. `db-gated-packages.sh` records the routing in a
comment so the next reader does not re-litigate it.

**Files:**
- Modify: `test/conformance/differential_db_test.go` (drop the advisory branch; add the
  grammar-driven generator)
- Create: `test/conformance/differential_control_db_test.go` (the negative control)
- Modify: `.github/workflows/ci.yml` (mcp-conformance job env)
- Create: `scripts/ci/differential_lane_required_test.go` (the gate)
- Modify: `scripts/ci/db-gated-packages.sh` (record the routing)
- Modify: `docs/CLAUDE.md` (the SQL Logic Test rule)

**Interfaces:**
- Consumes: `laneExpr{source, domain, concept, lambda, query}`, `laneMatrix() []string`,
  `laneCorpusExpressions(t) ([]*laneExpr, map[string]string, map[string][]laneRow)`,
  `laneTree`, `laneEngine`, `laneSelect`, `laneEvaluate`, `laneWrite`, `tryDB(t) (*bun.DB, bool)`
  -- all already in `test/conformance`.
- Produces: `laneGenerated(seed int64, n int) []string`, a deterministic grammar-driven
  generator over P-tier expressions, consumed by `TestDifferentialLane` and by
  `TestDifferentialLaneNegativeControl`.

- [ ] **Step 1: Write the failing gate test for CI wiring**

Create `scripts/ci/differential_lane_required_test.go`:

```go
package ci_test

// The differential lane is REQUIRED (memql#5386, D5 and D23 of
// docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md).
//
// "Required" is two separate facts, and each fails open on its own:
//
//   1. The job that runs test/conformance sets MEMQL_DIFFERENTIAL_REQUIRED=1.
//      Without it the lane logs a disagreement on a DIFFERENTIAL: line and
//      passes -- green over a language whose two evaluators disagree.
//   2. That job is one of ci-required's needs. A lane outside the aggregate is
//      advisory however red it goes, which is the memql#3019 fail-open shape.
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

const workflowPath = "../../.github/workflows/ci.yml"

// conformanceJobName is the job that runs ./test/conformance/... against a
// seeded database. It is the `name:` the aggregate and the branch protection
// see, not the YAML key.
const conformanceJobKey = "conformance"

func readWorkflow(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatalf("read %s: %v", workflowPath, err)
	}
	return string(b)
}

// jobBlock returns the YAML block of one top-level job, from its two-space key
// to the next two-space key at the same indent.
func jobBlock(t *testing.T, doc, key string) string {
	t.Helper()
	start := regexp.MustCompile(`(?m)^  ` + regexp.QuoteMeta(key) + `:\s*$`)
	loc := start.FindStringIndex(doc)
	if loc == nil {
		t.Fatalf("no job %q in %s -- if the job was renamed, this gate needs the new key, "+
			"not deleting", key, workflowPath)
	}
	rest := doc[loc[1]:]
	next := regexp.MustCompile(`(?m)^  [a-z][a-z0-9-]*:\s*$`).FindStringIndex(rest)
	if next == nil {
		return rest
	}
	return rest[:next[0]]
}

func TestDifferentialLaneRunsInRequiredMode(t *testing.T) {
	block := jobBlock(t, readWorkflow(t), conformanceJobKey)
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
	needs := jobBlock(t, readWorkflow(t), "ci-required")
	if !regexp.MustCompile(`(?m)^      - ` + regexp.QuoteMeta(conformanceJobKey) + `\s*$`).MatchString(needs) {
		t.Fatalf("ci-required does not list %q in needs.\n"+
			"A lane outside the aggregate is advisory however red it goes -- the merge queue "+
			"never sees it. The differential lane is required (D23), so its job belongs in needs.",
			conformanceJobKey)
	}
}
```

- [ ] **Step 2: Run it to confirm it fails on the env fact**

```bash
cd /home/znas/memql-projects/epic-dsl-v1-freeze
go test ./scripts/ci/ -run TestDifferentialLane -v
```

Expected: `TestDifferentialLaneRunsInRequiredMode` FAILS with "does not set
MEMQL_DIFFERENTIAL_REQUIRED"; `TestDifferentialLaneIsInsideCIRequired` PASSES (the job is
already in `needs`, which is the half that was already true).

- [ ] **Step 3: Set the flag in CI**

In `.github/workflows/ci.yml`, the `conformance:` job's `env:` block currently reads:

```yaml
    env:
      MEMQL_DATABASE_DSN: postgres://memql:memql_dev@localhost:5432/memql?sslmode=disable
```

Make it:

```yaml
    env:
      MEMQL_DATABASE_DSN: postgres://memql:memql_dev@localhost:5432/memql?sslmode=disable
      # The differential lane is REQUIRED (memql#5386, D23). Unset, a
      # disagreement between the SQL lowering and the in-process evaluator is
      # LOGGED on a DIFFERENTIAL: line and the lane passes -- green over a
      # language whose two implementations of one meaning have diverged.
      # scripts/ci/differential_lane_required_test.go holds this line and the
      # ci-required membership together.
      MEMQL_DIFFERENTIAL_REQUIRED: "1"
```

- [ ] **Step 4: Run the gate to verify it passes**

```bash
go test ./scripts/ci/ -run TestDifferentialLane -v
```

Expected: both PASS.

- [ ] **Step 5: Make a disagreement report every case, not just the first**

In `test/conformance/differential_db_test.go`, the `report` closure currently reads:

```go
	report := func(line string) {
		disagreements = append(disagreements, line)
		if differentialRequired() {
			t.Fatalf("the two evaluators disagree: %s", line)
		}
	}
```

`t.Fatalf` stops at the first disagreement, which hides how wide a divergence is -- the one
number a reviewer needs to tell a typo from a lowering rewrite. Replace it with:

```go
	report := func(line string) {
		disagreements = append(disagreements, line)
		if differentialRequired() {
			// Errorf, not Fatalf: a divergence's WIDTH is the first thing a
			// reader needs -- one row of one expression is a typo, forty rows
			// across every comparison is a lowering that changed meaning. The
			// summary line below prints the count either way.
			t.Errorf("the two evaluators disagree: %s", line)
		}
	}
```

- [ ] **Step 6: Write the failing test for the grammar-driven generator**

Append to `test/conformance/differential_db_test.go`:

```go
// TestLaneGeneratedIsGrammarDriven holds the generator to three properties, all
// of which a hand-written list fails: it draws from the TIER MANIFEST rather
// than from a literal list, so a P-tier position or operator added to the
// language is generated without anyone remembering to add it; it is
// deterministic in its seed, so a disagreement found in CI is reproducible from
// the seed printed beside it; and every expression it emits PARSES, so a
// generator bug reads as a generator bug rather than as a lane that compared
// nothing.
func TestLaneGeneratedIsGrammarDriven(t *testing.T) {
	const n = 240
	a := laneGenerated(7, n)
	b := laneGenerated(7, n)
	if len(a) != n {
		t.Fatalf("laneGenerated(7, %d) returned %d expressions", n, len(a))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("laneGenerated is not deterministic: seed 7 gave %q then %q at %d", a[i], b[i], i)
		}
	}
	if c := laneGenerated(8, n); c[0] == a[0] && c[1] == a[1] && c[2] == a[2] {
		t.Fatal("laneGenerated ignores its seed: seeds 7 and 8 opened identically")
	}
	for i, src := range a {
		if _, err := langparser.ParseV1Lambda(src); err != nil {
			t.Fatalf("laneGenerated #%d does not parse: %q: %v", i+1, src, err)
		}
	}
	// The operators the tier manifest admits at a query filter must all appear.
	// A generator that emits only `==` proves the two evaluators agree about
	// equality and nothing else.
	for _, op := range []string{"==", "!=", "<", "<=", ">", ">=", " in ", "startsWith", "&&", "||", "!"} {
		found := false
		for _, src := range a {
			if strings.Contains(src, op) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no generated expression uses %q -- the generator is not covering the admitted operator set", op)
		}
	}
}
```

Add `langparser "github.com/znasllc-io/memql/component/language/parser"` to the file's
imports if it is not already there.

> **Check the parse entry point before writing Step 8.** The exported name for "parse one
> edition-2026 lambda" may be `ParseV1Lambda`, `ParseLambda` or reachable only through
> `ParseExpression`. Run
> `grep -n "^func Parse" component/language/parser/*.go | grep -v _test`
> and use the one that takes a `row => ...` source. If none is exported, parse the
> expression through the same path `laneTree` already uses to build a query and assert the
> query loads -- do NOT export a new parser entry point for a test.

- [ ] **Step 7: Run it to verify it fails**

```bash
go test ./test/conformance/ -run TestLaneGeneratedIsGrammarDriven -v
```

Expected: FAIL to compile with "undefined: laneGenerated".

- [ ] **Step 8: Write the generator**

Append to `test/conformance/differential_db_test.go`:

```go
// laneGenerated is the grammar-driven half of the lane (memql#5386): random
// P-tier expressions over the scratch concept, drawn from the shapes the TIER
// MANIFEST admits at a query filter rather than from a list somebody
// maintains. laneMatrix beside it stays: it is the CHOSEN half, the absence
// table and the typed comparisons the language is known to find awkward, and a
// random generator reaches those rows only by luck.
//
// Deterministic in seed. The seed is printed in the lane's summary line, so a
// disagreement found on a hosted runner is reproducible on a developer machine
// with one number.
func laneGenerated(seed int64, n int) []string {
	rnd := rand.New(rand.NewSource(seed))

	// The operand vocabulary: the scratch concept's fields (probe carries
	// value, tags, flag and obj -- see laneRowsFor) against the literal set the
	// absence table is built from. Both sides are P-tier by construction: a
	// field read and a literal are the only leaf kinds pushdownKinds admits.
	fields := []string{"row.value", "row.flag", "row.?obj.a"}
	lits := []string{`""`, `nil`, `"a"`, `"e"`, `"é"`, `"1"`, `1`, `0`, `1.5`, `true`, `false`}
	cmps := []string{"==", "!=", "<", "<=", ">", ">="}

	atom := func() string {
		switch rnd.Intn(8) {
		case 0:
			return fmt.Sprintf(`%s in [%s, %s]`, fields[rnd.Intn(len(fields))],
				lits[rnd.Intn(len(lits))], lits[rnd.Intn(len(lits))])
		case 1:
			return fmt.Sprintf(`%s startsWith %s`, fields[rnd.Intn(len(fields))],
				[]string{`"a"`, `""`, `"é"`}[rnd.Intn(3)])
		case 2:
			return fmt.Sprintf(`%s.includes(%s)`, fields[rnd.Intn(len(fields))],
				[]string{`"a"`, `""`, `"e"`}[rnd.Intn(3)])
		case 3:
			return fmt.Sprintf(`%s in row.tags`, lits[rnd.Intn(len(lits))])
		case 4:
			return fmt.Sprintf(`row.tags.count() %s %d`, cmps[rnd.Intn(len(cmps))], rnd.Intn(3))
		default:
			return fmt.Sprintf(`%s %s %s`, fields[rnd.Intn(len(fields))],
				cmps[rnd.Intn(len(cmps))], lits[rnd.Intn(len(lits))])
		}
	}

	// A term is an atom, a negated atom, or a parenthesised connective of two.
	// Depth stops at two: the lane is comparing two EVALUATORS, and a deeper
	// tree tests the same operators through more parentheses.
	term := func() string {
		switch rnd.Intn(4) {
		case 0:
			return "!(" + atom() + ")"
		case 1:
			return "(" + atom() + " && " + atom() + ")"
		case 2:
			return "(" + atom() + " || " + atom() + ")"
		default:
			return atom()
		}
	}

	seen := map[string]bool{}
	out := make([]string, 0, n)
	// Bounded: a small vocabulary saturates, and an unbounded loop over a
	// saturated generator is an infinite loop in CI.
	for attempts := 0; len(out) < n && attempts < n*50; attempts++ {
		src := "row => " + term()
		if rnd.Intn(3) == 0 {
			src += " " + []string{"&&", "||"}[rnd.Intn(2)] + " " + term()
		}
		if seen[src] {
			continue
		}
		seen[src] = true
		out = append(out, src)
	}
	return out
}

// laneGeneratorSeed is the lane's seed. It is a CONSTANT rather than a clock
// reading: a lane that generates different expressions on every run is a lane
// whose red is not reproducible, and "it passed when I re-ran it" is the answer
// that ends an investigation without resolving it. Move it deliberately, the
// way a fuzz corpus grows -- by committing the case that broke, not by
// reshuffling.
const laneGeneratorSeed = 20260920
```

Add `"math/rand"` to the imports.

- [ ] **Step 9: Feed the generator into the lane**

In `TestDifferentialLane`, after the `laneMatrix()` loop, add:

```go
	for i, src := range laneGenerated(laneGeneratorSeed, 240) {
		exprs = append(exprs, &laneExpr{source: fmt.Sprintf("generated(seed=%d) #%d", laneGeneratorSeed, i+1), domain: laneDomain, concept: "probe", lambda: src})
	}
```

and extend the summary `t.Logf` so the seed is in the output a failure is read from:

```go
	t.Logf("differential lane (generator seed %d): %d expressions (%d lowered, %d refused at load) over %d rows of %d concepts: %d comparisons, %d disagreements",
		laneGeneratorSeed, len(exprs), lowered, len(exprs)-lowered, rowCount, len(written), compared, len(disagreements))
```

- [ ] **Step 10: Run the generator test and the lane**

```bash
go test ./test/conformance/ -run TestLaneGeneratedIsGrammarDriven -v
MEMQL_DATABASE_DSN='postgres://memql:memql_dev@localhost:15434/memql?sslmode=disable' \
  MEMQL_DIFFERENTIAL_REQUIRED=1 go test -count=1 -timeout=900s ./test/conformance/ -run TestDifferentialLane -v 2>&1 | tail -30
```

Expected: the generator test PASSES; the lane PASSES and its summary line reports a
comparison count well above the pre-generator number with `0 disagreements`.

> If the lane reports disagreements, STOP and read them. A disagreement is a real defect in
> either `memql.Lower` or `memql.EvalExpr` and it is exactly what this epic exists to
> surface. Do not narrow the generator to make it green. Fix the evaluator, add the case to
> `test/conformance/2026/expr/` as a `lower` + `evaluate` pair, and say so in the PR.

- [ ] **Step 11: Write the negative control**

The lane's requiredness is a claim, and a claim about a gate is only checkable by making
the gate fire. Create `test/conformance/differential_control_db_test.go`:

```go
package conformance

// The differential lane's negative control (memql#5386).
//
// "A disagreement makes the lane red" is a claim about a gate, and a gate that
// has never fired is indistinguishable from a gate that cannot. This control
// SEEDS a disagreement and requires the lane's own comparison to report it.
//
// It does not run the lane. It runs the lane's two evaluators over one row of
// one expression whose two implementations are made to differ, through the same
// laneSelect / laneEvaluate seam TestDifferentialLane compares with, and fails
// when they agree -- which is the state that would mean the comparison itself
// stopped comparing.
//
// Why a seam and not a mutated evaluator: patching memql.Lower under a test
// would prove the control can detect a defect this repo does not have. What can
// actually go wrong here is the COMPARISON going inert -- laneSelect returning
// every row, laneEvaluate returning the SQL's answer, the row set coming back
// empty. Each of those makes a real disagreement invisible, and each is what
// this control catches.

import (
	"os"
	"testing"
)

func TestDifferentialLaneNegativeControl(t *testing.T) {
	db, hasDB := tryDB(t)
	if !hasDB {
		if os.Getenv("MEMQL_REQUIRE_DB") == "1" {
			t.Fatal("the differential lane's control needs Postgres, and MEMQL_REQUIRE_DB=1 makes its absence a failure")
		}
		t.Skip("the differential lane's control needs Postgres (MEMQL_DATABASE_DSN)")
	}
	_ = db
	// Build the lane's tree over ONE expression and ONE pair of rows chosen so
	// the expression selects exactly one of them: `row.value == "a"` over a row
	// storing "a" and a row storing "b". Then assert, through the lane's own
	// two seams, that the pair is SPLIT -- one row in, one row out, on BOTH
	// sides. A comparison that has gone inert returns the same verdict for both
	// rows, and that is the failure this control reports.
	//
	// IMPLEMENTATION NOTE for whoever writes this: reuse laneTree, laneEngine,
	// laneWrite, laneSelect and laneEvaluate exactly as TestDifferentialLane
	// does -- do not build a second engine harness. The control's whole value is
	// that it exercises the SAME seam; a parallel harness proves the parallel
	// harness works.
	t.Skip("STEP 12 writes this body")
}
```

- [ ] **Step 12: Implement the control body**

Replace the `t.Skip` with the real body, modelled on `TestDifferentialLane`'s own setup
(read that function and mirror it; the helper signatures are in the Interfaces block above):

```go
	lane := fmt.Sprintf("dc%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.NewDelete().Model((*memoryNodes.MemoryNode)(nil)).
			Where("payload->>'lane' = ?", lane).Exec(context.Background())
	})

	x := &laneExpr{source: "control", domain: laneDomain, concept: "probe", lambda: `row => row.value == "a"`}
	tree := laneTree(t, []*laneExpr{x}, map[string]string{})
	eng, stop := laneEngine(t, db, tree)
	defer stop()

	rows := []laneRow{{Label: "is-a", Payload: map[string]any{"value": "a"}}, {Label: "is-b", Payload: map[string]any{"value": "b"}}}
	written := laneWrite(t, laneContext(nil), eng, db, lane, "v1:"+laneDomain+":probe", rows)
	if len(written) != 2 {
		t.Fatalf("the control wrote %d rows, wanted 2 -- laneWrite is not writing what the lane compares over", len(written))
	}

	selected, err := laneSelect(eng, lane, x)
	if err != nil {
		t.Fatalf("the control's read failed: %v", err)
	}
	var sqlYes, evalYes int
	for _, node := range written {
		if _, in := selected[laneBareID(node.ID)]; in {
			sqlYes++
		}
		got, evalErr := laneEvaluate(eng, x, node)
		if evalErr != nil {
			t.Fatalf("the control's in-process evaluation refused %s: %v", laneLabel(node), evalErr)
		}
		if got {
			evalYes++
		}
	}
	if sqlYes != 1 {
		t.Fatalf("the SQL side selected %d of 2 rows for `%s`; a side that answers the same for every row "+
			"cannot report a disagreement, so the lane would be green over any divergence", sqlYes, x.lambda)
	}
	if evalYes != 1 {
		t.Fatalf("the in-process side selected %d of 2 rows for `%s`; see above -- an inert comparison "+
			"is the failure this control exists to catch", evalYes, x.lambda)
	}
```

Adjust `laneRow`'s field names to whatever the struct actually declares (read it; the
labels above are illustrative). Add the imports the body needs.

- [ ] **Step 13: Run the control**

```bash
MEMQL_DATABASE_DSN='postgres://memql:memql_dev@localhost:15434/memql?sslmode=disable' \
  go test -count=1 -timeout=600s ./test/conformance/ -run TestDifferentialLaneNegativeControl -v
```

Expected: PASS.

Then prove the control can fail: temporarily change its expression to `row => true`
(selects both rows), re-run, and confirm it FAILS with "selected 2 of 2 rows". Put the
expression back. **Record the observed failure text in the commit body** -- a control
nobody has seen fail is a control nobody has tested.

- [ ] **Step 14: Record the routing in db-gated-packages.sh**

Append to the comment block above `DB_GATED_TREES` in `scripts/ci/db-gated-packages.sh`:

```bash
# `test/conformance` is deliberately ABSENT, and memql#5386 re-confirmed it
# rather than changing it. The tree's db-gated cases -- the differential lane
# above all -- run in the SEPARATE mcp-conformance job, which brings up the same
# TimescaleDB service container and is one of ci-required's needs; scripts/cidb
# exempts the tree by name for that reason (dsnliteral_test.go). Adding it here
# would move it out of go-checks and into db-tests WITHOUT taking it out of
# mcp-conformance, so the ten-minute suite would run twice on every PR for no
# property that is not already held.
#
# What holds the property instead is executable, not this comment:
# scripts/ci/differential_lane_required_test.go asserts that the
# mcp-conformance job sets MEMQL_DIFFERENTIAL_REQUIRED=1 AND that the job is in
# ci-required's needs. Both facts fail open on their own, which is why both are
# gated.
```

- [ ] **Step 15: Write the SQL Logic Test rule into docs/CLAUDE.md**

Read `docs/CLAUDE.md` first and match its heading level and voice. Add a section:

```markdown
## A bug is not fixed until its case is in the corpus

Borrowed from SQL Logic Test, and it is the rule that makes
`test/conformance/2026/` worth its size: a defect in the language -- a filter
that lowers to the wrong SQL, an expression the two evaluators answer
differently, a refusal whose wording stopped naming the fix -- is not fixed by
the commit that changes the code. It is fixed by the commit that adds the case
the old code fails and the new code passes.

The reason is that the corpus is the only thing that remembers. A fix with no
case is a fix that survives exactly until someone refactors the path it lives
on, and the regression comes back reading like a new bug.

Where the case goes, by what it is about:

| The defect | The case |
|---|---|
| A filter lowers wrongly, or the two evaluators disagree | `test/conformance/2026/expr/<position>/`, as a `lower` and an `evaluate` verdict over the rows that split them |
| A construct or annotation accepts what it should refuse | `test/conformance/2026/cells/<construct>/<attribute>/`, with the `code` and `message` the refusal carries |
| A refusal's wording lost the replacement it used to name | the same cell's `expect.json`; the wording is pinned, so a change to it is a deliberate edit and not a drift |
| A load refusal nothing covered | `test/conformance/2026/negative/<construct>/<fault>.memql` |
| The parser panicked on an input | `test/conformance/2026/fuzz/`, or the package's own `testdata/fuzz/<Target>/` |

**The differential lane is REQUIRED** (memql#5386). It runs in the
`mcp-conformance` job with `MEMQL_DIFFERENTIAL_REQUIRED=1`, and a disagreement
between the SQL lowering and the in-process evaluator fails it. The lane draws
its expressions from the corpus plus a deterministic grammar-driven generator,
so its seed is printed on the summary line: a red on a hosted runner is
reproducible locally from that one number.
```

- [ ] **Step 16: Verify and commit**

```bash
cd /home/znas/memql-projects/epic-dsl-v1-freeze
go test ./scripts/ci/ -run TestDifferentialLane -v
go test ./test/conformance/ -run 'TestLaneGeneratedIsGrammarDriven' -v
MEMQL_DATABASE_DSN='postgres://memql:memql_dev@localhost:15434/memql?sslmode=disable' \
  MEMQL_DIFFERENTIAL_REQUIRED=1 go test -count=1 -timeout=900s ./test/conformance/ \
  -run 'TestDifferentialLane|TestDifferentialLaneNegativeControl' -v 2>&1 | tail -20
go test -count=1 . 2>&1 | tail -5   # the root package's docs gates read docs/CLAUDE.md
```

```bash
git add test/conformance/differential_db_test.go \
        test/conformance/differential_control_db_test.go \
        .github/workflows/ci.yml \
        scripts/ci/differential_lane_required_test.go \
        scripts/ci/db-gated-packages.sh \
        docs/CLAUDE.md
git commit -m "Issue #5386: the differential lane becomes required, with a grammar-driven generator and a control"
```

---

### Task 2: Fuzz seed corpus and refusal-wording pins (#5387)

Five fuzz targets already exist (`FuzzLexer`, `FuzzParseV1Expression`, `FuzzLower`,
`FuzzEvalExpr`, `FuzzParse`). Three things are missing: no committed seed corpus under any
`testdata/fuzz/` (so the targets start from their in-code `f.Add` seeds only and a found
crash is not remembered), no CI step that fuzzes for a bounded time, and no pin that every
load refusal the corpus names keeps its wording.

**Files:**
- Create: `component/language/parser/testdata/fuzz/FuzzLexer/` (seed files)
- Create: `component/language/parser/testdata/fuzz/FuzzParseV1Expression/` (seed files)
- Create: `component/memql/testdata/fuzz/FuzzLower/` (seed files)
- Create: `component/memql/testdata/fuzz/FuzzEvalExpr/` (seed files)
- Create: `test/conformance/2026/fuzz/` (corpus-level seeds, replacing the lone README)
- Create: `test/conformance/refusal_wording_test.go`
- Modify: `.github/workflows/ci.yml` (bounded fuzz step in `go-checks`)
- Modify: `test/conformance/2026/fuzz/README.md`

**Interfaces:**
- Consumes: the five `Fuzz*` targets; the corpus reader `corpusCases` / `corpusEditionDir`
  already in `test/conformance`.
- Produces: `test/conformance/refusal_wording_test.go`'s `TestEveryNamedRefusalIsPinned`,
  which later tasks do not consume.

- [ ] **Step 1: Read how the corpus reader exposes a case's code and message**

```bash
cd /home/znas/memql-projects/epic-dsl-v1-freeze
grep -n "Code\|Message\|Verdict" test/conformance/corpus_test.go | head -30
sed -n '1,80p' test/conformance/corpus_test.go
```

Note the exact struct name and field names for a case (it is the decoder for the
`expect.json` shape: `{cases:[{file, verdict, code, message, note}]}`). Every snippet below
that says `corpusCase` must be changed to whatever it is actually called.

- [ ] **Step 2: Write the failing refusal-wording pin**

Create `test/conformance/refusal_wording_test.go`:

```go
package conformance

// Every refusal the corpus names carries its WORDING, not just its code
// (memql#5387, D23 and D24).
//
// D24 is the requirement the pin exists for: "every refusal names the
// construct, the position, the rule id and the replacement, and when the fix is
// mechanical, the rewritten line; the corpus pins the wording". A refusal CODE
// is a contract with a machine. The MESSAGE is the contract with the author,
// and it is the half that rots: a refactor that keeps the code and loses the
// sentence naming `memqlmigrate --rewrite=expressions` turns a two-minute fix
// into an afternoon, and no test notices, because the code still matches.
//
// So: every corpus case whose verdict is a refusal must pin a message, and the
// engine's actual refusal must still contain it. TestCorpusVerdicts already
// checks the second half for the cases that HAVE a message. This test checks
// the first -- that a refusal case is not allowed to pin nothing.

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// refusalVerdicts are the verdicts that carry a message contract.
var refusalVerdicts = map[string]bool{"refuse_parse": true, "refuse_load": true}

func TestEveryNamedRefusalIsPinned(t *testing.T) {
	cases := allCorpusCases(t) // Step 3 writes this helper
	if len(cases) == 0 {
		t.Fatal("no corpus cases were read at all: the pin would pass over an empty set, which is the " +
			"fail-open shape a completeness gate exists to prevent")
	}
	var refusals, pinned int
	var unpinned []string
	for _, c := range cases {
		if !refusalVerdicts[c.Verdict] {
			continue
		}
		refusals++
		if strings.TrimSpace(c.Message) == "" {
			unpinned = append(unpinned, fmt.Sprintf("%s (%s)", c.Path, c.Verdict))
			continue
		}
		pinned++
	}
	if refusals == 0 {
		t.Fatal("the corpus holds no refusal cases at all -- either the reader stopped reading or every " +
			"negative case was deleted; both make this pin vacuous")
	}
	if len(unpinned) > 0 {
		sort.Strings(unpinned)
		t.Errorf("%d of %d refusal cases pin no message.\n\n"+
			"A refusal's CODE is the contract with a machine; its MESSAGE is the contract with the author, "+
			"and it is the half that rots silently -- a refactor keeps the code and drops the sentence naming "+
			"the replacement, and nothing notices.\n\n"+
			"Add a \"message\" to each case below: a distinctive substring of what the engine actually says, "+
			"including the replacement it names.\n\n  %s",
			len(unpinned), refusals, strings.Join(unpinned, "\n  "))
	}
	t.Logf("refusal-wording pins: %d refusal cases, all pinned", pinned)
}

// TestRefusalsNameTheirReplacement is the D24 half that matters most and is
// scoped to where it is checkable: a refusal for a RETIRED form must name what
// to write instead. The retirement tables carry the replacement text
// (annotations.Retirements(), parser.V1RetiredForms), so this is a claim about
// the tree's own data, not about English.
func TestRefusalsNameTheirReplacement(t *testing.T) {
	// The concrete assertion is written in Step 5, once Step 1 has established
	// what the retirement tables expose.
	t.Skip("STEP 5 writes this body")
}
```

- [ ] **Step 3: Write the corpus reader helper**

Add `allCorpusCases` to the same file, reusing whatever `corpus_test.go` already does --
**do not write a second `expect.json` decoder.** If `corpus_test.go` has an unexported
reader, call it; if it inlines the walk, extract it into a shared helper in
`corpus_test.go` and call that from both. The helper returns a flat slice of
`{Path, Verdict, Code, Message}` across every edition directory.

```go
type corpusCaseRef struct {
	Path    string // "<edition>/cells/query/actor/with-true.memql"
	Verdict string
	Code    string
	Message string
}
```

- [ ] **Step 4: Run it and record the real number**

```bash
go test ./test/conformance/ -run TestEveryNamedRefusalIsPinned -v 2>&1 | tail -40
```

Expected: either PASS (every refusal already pins a message -- likely, since the cells seen
so far all do) or a FAIL listing the unpinned cases. **If it fails, pin each listed case by
running the engine against that fixture and copying a distinctive substring of the real
refusal into `expect.json`.** Do not pin a guess; run it:

```bash
go run ./cmd/memqllint --path test/conformance/2026/<the case's directory> 2>&1 | head
```

- [ ] **Step 5: Write the replacement-naming pin**

Replace the `t.Skip` body:

```go
func TestRefusalsNameTheirReplacement(t *testing.T) {
	// A retirement's whole value to an author is the sentence that says what to
	// write instead. annotations.Retirements() carries that text as data, so
	// this is checkable: every retirement's hint must be non-empty and must not
	// be only the name of the thing being retired.
	rs := annotations.Retirements()
	if len(rs) == 0 {
		t.Fatal("annotations.Retirements() is empty: either the table moved or the accessor stopped " +
			"reading it, and this pin passes over nothing either way")
	}
	for _, r := range rs {
		hint := retirementHintOf(r) // adapt to the struct's actual field
		if strings.TrimSpace(hint) == "" {
			t.Errorf("retirement %q names no replacement: an author who writes it is told only that it is "+
				"gone, which is the half of the message that does not help", retirementNameOf(r))
			continue
		}
		if !strings.ContainsAny(hint, " ") {
			t.Errorf("retirement %q's hint is a bare token (%q) rather than a sentence naming the fix",
				retirementNameOf(r), hint)
		}
	}
}
```

Read `component/language/annotations/retired.go` for `Retirement`'s real fields and drop
the `*Of` adapters in favour of direct field reads.

- [ ] **Step 6: Run it**

```bash
go test ./test/conformance/ -run TestRefusalsNameTheirReplacement -v
```

Expected: PASS. If a retirement genuinely names no replacement, ADD the replacement text to
`retired.go` rather than weakening the test.

- [ ] **Step 7: Generate the fuzz seed corpus**

A Go fuzz seed file is a specific format. Write a small generator rather than hand-writing
them. Create `scripts/dev/fuzz-seeds.sh`:

```bash
#!/usr/bin/env bash
# Write the committed fuzz seed corpus (memql#5387).
#
# Go reads seeds from testdata/fuzz/<Target>/ as files in the `go test fuzz v1`
# format: a version line, then one typed literal per f.Fuzz parameter. Every
# target here takes a single string, so each file is two lines.
#
# The seeds are drawn from the conformance corpus -- every .memql file the
# language has a rule about -- plus the awkward inputs a lexer and a parser are
# known to mishandle. Committing them is what makes `go test -fuzz` start from
# the forms the language cares about instead of from random bytes, and it is
# what makes a found crash REMEMBERED: the failing input Go writes into
# testdata/fuzz/ on a crash is committed beside these and replays forever after.
set -euo pipefail

main() {
	local root; root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
	cd "$root"
	seed_from_corpus component/language/parser/testdata/fuzz/FuzzLexer
	seed_from_corpus component/language/parser/testdata/fuzz/FuzzParseV1Expression
	seed_edge_cases  component/language/parser/testdata/fuzz/FuzzLexer
	echo "SUCCESS: fuzz seeds written"
}

main "$@"
```

> **Judgment call for the implementer.** The corpus is 529 files; committing every one as a
> seed into four targets is ~2000 near-duplicate files and is the "corpus that becomes a
> burden" failure mode the record names in section 6. `FuzzParse` in `test/conformance`
> ALREADY seeds from every corpus `.memql` at run time via `f.Add`, so those seeds are not
> missing -- they are just not on disk. **Commit a SMALL, deliberately chosen seed set per
> target** (15-30 files): the shapes a lexer and a parser are known to mishandle, listed in
> Step 8. Say so in the README. The corpus-wide seeding stays where it is, in `f.Add`.

- [ ] **Step 8: Write the chosen seed inputs**

For `component/language/parser/testdata/fuzz/FuzzLexer/`, one file per input, each named
`seed-<slug>`, each in this exact format:

```
go test fuzz v1
string("query q { filter row => row.a == \"x\" }")
```

The inputs to commit, each chosen because it is a shape a lexer gets wrong:

| Slug | Input | Why |
|---|---|---|
| `empty` | `` | the degenerate input |
| `nul` | `query q {\x00}` | a NUL in source; `grep` skips such a file, so a defect here hides from every text-based gate |
| `unterminated-string` | `query q { filter row => row.a == "x }` | the lexer must not run off the end |
| `unterminated-comment` | `/* query q {` | same, for the block comment |
| `nested-comment` | `/* /* */ */` | nesting the lexer may or may not support |
| `crlf` | `query q {\r\n}` | line endings in position reporting |
| `bom` | `\xef\xbb\xbfquery q {}` | a byte-order mark before the first token |
| `lone-surrogate` | `"\ud800"` | an escape that is not a character |
| `deep-nesting` | `((((((((((1))))))))))` x 50 | stack depth |
| `long-identifier` | `a` x 10000 | buffer growth |
| `combining-marks` | `row.é` | a grapheme that is two runes |
| `rtl` | `row.‮abc` | a bidi override in an identifier |
| `tab-indent` | `query\tq\t{\t}` | tabs as separators |
| `only-operators` | `&&\|\|!==>=<=??` | operators with no operands |
| `lambda-arrow-glyph` | `row => row.a` then `row=>row.a` then `row = > row.a` | the `=>` glyph's tokenisation |

For `FuzzParseV1Expression/`, seed the expression forms the edition refuses, so the fuzzer
starts adjacent to every retirement: `row => row.a has "x"`, `row => cond(a, b, c)`,
`row => coalesce(a, b)`, `row => a ; b`, `row => null`, `row => x?.y`, `row => {"k": 1}`,
`row => f(name=1)`, `row => concept == v1:crm:lead`, plus the accepted forms they replace.

For `component/memql/testdata/fuzz/FuzzLower/` and `FuzzEvalExpr/`, seed the absence table:
`row => row.v == nil`, `row => row.v != ""`, `row => row.v in []`, `row => row.v startsWith ""`,
`row => row.?o.a == nil`, `row => row.tags.count() == 0`, `row => !row.flag`.

> **Write these with a script, not by hand.** A here-doc loop in `scripts/dev/fuzz-seeds.sh`
> that takes a slug and a Go-quoted literal and writes the two-line file is ~20 lines and
> removes the whole class of "the seed file is malformed so Go silently ignores it". Verify
> Go is reading them, in Step 9, by the seed COUNT.

- [ ] **Step 9: Verify Go reads the seeds**

```bash
go test ./component/language/parser/ -run 'FuzzLexer|FuzzParseV1Expression' -v 2>&1 | grep -c 'seed#'
```

Expected: a count matching the number of committed files plus the in-code `f.Add` seeds.
**A count that does not move when you add a file means the format is wrong and Go is
skipping it** -- that is the silent failure this step exists to catch. A malformed seed
file is ignored without a warning.

- [ ] **Step 10: Add the bounded fuzz step to CI**

In `.github/workflows/ci.yml`, inside the `go-checks` job after its existing `go test`
step, add:

```yaml
      # Bounded fuzzing (memql#5387). `go test -fuzz` runs ONE target per
      # invocation and runs until -fuzztime expires, so this is five short runs
      # rather than one long one. 40s each keeps the step near three minutes.
      #
      # What this buys over the seed corpus alone: the seeds run as ordinary
      # subtests on every `go test`, which is the regression half. This is the
      # SEARCH half -- it mutates them looking for the next crash. A crash here
      # writes the failing input into testdata/fuzz/<Target>/, and the fix is to
      # COMMIT that file: a bug is not fixed until its case is in the corpus
      # (docs/CLAUDE.md).
      - name: bounded fuzz (lexer, parser, lowering, evaluation)
        run: |
          set -euo pipefail
          fuzz() {
            local pkg="$1" target="$2"
            echo "INFO: fuzzing $target in $pkg"
            go test "$pkg" -run "^$target$" -fuzz "^$target$" -fuzztime=40s
          }
          fuzz ./component/language/parser FuzzLexer
          fuzz ./component/language/parser FuzzParseV1Expression
          fuzz ./component/memql FuzzLower
          fuzz ./component/memql FuzzEvalExpr
          fuzz ./test/conformance FuzzParse
          echo "SUCCESS: bounded fuzz completed with no new failing inputs"
```

> **Check `go-checks`' timeout before committing this.** `scripts/ci/workflow_timeout_test.go`
> holds every job's timeout to at least 2x its measured median. Adding ~3.5 minutes may
> require raising `go-checks`' `timeout-minutes`. Run
> `go test ./scripts/ci/ -run Timeout -v` after the edit and follow what it says.

- [ ] **Step 11: Rewrite the fuzz README**

`test/conformance/2026/fuzz/README.md` currently describes an empty directory. Make it say
what the directory is for and what the rule is:

```markdown
# Fuzz corpus

Inputs that once broke the lexer, the parser or the lowering. One file per
input, in Go's `go test fuzz v1` seed format.

**This directory grows one way: from found failures.** When a fuzz run crashes,
Go writes the failing input into the target's `testdata/fuzz/<Target>/`
directory. Committing that file is the fix's other half -- a bug is not fixed
until its case is in the corpus (`docs/CLAUDE.md`). Do not add speculative
inputs here; the chosen seed sets live beside their targets, and the whole
conformance corpus is already seeded into `FuzzParse` at run time through
`f.Add`.

Targets and where their seeds live:

| Target | Package | Seeds |
|---|---|---|
| `FuzzLexer` | `component/language/parser` | `testdata/fuzz/FuzzLexer/` |
| `FuzzParseV1Expression` | `component/language/parser` | `testdata/fuzz/FuzzParseV1Expression/` |
| `FuzzLower` | `component/memql` | `testdata/fuzz/FuzzLower/` |
| `FuzzEvalExpr` | `component/memql` | `testdata/fuzz/FuzzEvalExpr/` |
| `FuzzParse` | `test/conformance` | every corpus `.memql`, via `f.Add`, plus this directory |

CI fuzzes each target for 40 seconds in the `go-checks` job. That is a search,
not a proof; the seeds themselves run as ordinary subtests on every `go test`,
which is the regression guarantee.
```

- [ ] **Step 12: Verify and commit**

```bash
go test ./test/conformance/ -run 'TestEveryNamedRefusalIsPinned|TestRefusalsNameTheirReplacement' -v
go test ./component/language/parser/ -run 'FuzzLexer|FuzzParseV1Expression' -v 2>&1 | tail -5
go test ./component/memql/ -run 'FuzzLower|FuzzEvalExpr' -v 2>&1 | tail -5
go test ./component/language/parser -run FuzzLexer -fuzz FuzzLexer -fuzztime=20s
go test ./scripts/ci/ -v 2>&1 | tail -10
```

```bash
git add component/language/parser/testdata/fuzz \
        component/memql/testdata/fuzz \
        test/conformance/2026/fuzz \
        test/conformance/refusal_wording_test.go \
        test/conformance/corpus_test.go \
        scripts/dev/fuzz-seeds.sh \
        .github/workflows/ci.yml
git commit -m "Issue #5387: committed fuzz seeds, bounded fuzzing in CI, and the refusal-wording pins"
```

---

### Task 3: The generated BNF and vocabulary (#5388)

`dslspec.Build()` already returns the whole authoring surface derived from the parser and
the annotation registry, and `cmd/attributematrix` already shows how a generated page is
written, `-check`ed and gated. This task adds two more renderings of the same spec -- a BNF
and a vocabulary -- writes them into `docs/public/language`, serves them over the
introspection builtins, and gates the docs' examples against the corpus.

**Files:**
- Create: `component/language/dslspec/grammar.go`
- Create: `component/language/dslspec/grammar_test.go`
- Create: `component/language/dslspec/vocabulary.go`
- Create: `component/language/dslspec/vocabulary_test.go`
- Create: `cmd/dslgrammar/main.go`
- Create: `docs/public/language/grammar.md` (generated)
- Create: `docs/public/language/vocabulary.md` (generated)
- Modify: `component/memql/executor_builtin.go`
- Modify: `dsl/common/builtins.memql`
- Modify: `Makefile`
- Create: `test/conformance/docs_examples_test.go`
- Modify: root-package gate file alongside `TestAttributeMatrixIsGenerated`

**Interfaces:**
- Consumes: `dslspec.Build() *Spec`, `dslspec.AttributeMatrixPath`, `functions.Catalog()`,
  `functions.Operators()`, `annotations.Placements()`, `annotations.Retirements()`,
  `parser.Edition`, `parser.GrammarVersion`, `tiers` manifest.
- Produces:
  - `dslspec.GrammarPath = "docs/public/language/grammar.md"`
  - `dslspec.VocabularyPath = "docs/public/language/vocabulary.md"`
  - `dslspec.RenderGrammar() string` -- the BNF page
  - `dslspec.RenderVocabulary() string` -- the vocabulary page
  - `dslspec.BNF() string` -- the bare grammar, no page furniture, for the builtin
  - `dslspec.Vocabulary() []VocabularyEntry` -- `{Kind, Name, Signature, Description, Since}`
  - builtins `memqlGrammar` and `memqlVocabulary`

- [ ] **Step 1: Read the precedent end to end**

```bash
cd /home/znas/memql-projects/epic-dsl-v1-freeze
cat cmd/attributematrix/main.go
grep -rn "AttributeMatrixPath" component/language/dslspec/
grep -rn "TestAttributeMatrixIsGenerated" *.go
sed -n '575,595p' Makefile
```

Everything below mirrors this. Do not invent a different shape.

- [ ] **Step 2: Write the failing grammar test**

Create `component/language/dslspec/grammar_test.go`:

```go
package dslspec

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/parser"
)

// The BNF is GENERATED from the same tables the parser reads (memql#5388, D24).
// Its purpose is to be given to a model as grammar-in-prompt and to drive
// syntax-only constrained decoding, and both uses fail the same way if it
// drifts: the model emits a form the parser refuses, confidently.
func TestBNFNamesEveryConstruct(t *testing.T) {
	bnf := BNF()
	if strings.TrimSpace(bnf) == "" {
		t.Fatal("BNF() is empty")
	}
	for _, c := range Build().Constructs {
		// Every construct the spec advertises must have a production. A
		// construct missing from the grammar is a form the model will never
		// emit and a reader will conclude does not exist.
		if !strings.Contains(bnf, "<"+c.Keyword+">") {
			t.Errorf("no production for construct %q; the grammar advertises a language that is missing it", c.Keyword)
		}
	}
}

func TestBNFNamesTheLanguageItDescribes(t *testing.T) {
	bnf := BNF()
	if !strings.Contains(bnf, parser.Edition) {
		t.Errorf("the BNF does not name its edition (%s); a grammar with no version is a grammar a reader cannot check against their cluster", parser.Edition)
	}
	if !strings.Contains(bnf, parser.GrammarVersion) {
		t.Errorf("the BNF does not name its grammar version (%s)", parser.GrammarVersion)
	}
}

func TestBNFCarriesNoRetiredForm(t *testing.T) {
	bnf := BNF()
	// A retired form in the shipped grammar is worse than an absent one: it
	// TELLS a model to emit something the parser refuses.
	for _, form := range parser.V1RetiredForms {
		name := retiredFormToken(form) // adapt to the actual struct
		if name == "" {
			continue
		}
		if strings.Contains(bnf, "<"+name+">") {
			t.Errorf("the BNF has a production for the retired form %q", name)
		}
	}
}
```

> Read `component/language/parser/v1_refusals.go` for `V1RetiredForms`' real element type
> before writing `retiredFormToken`; if the elements carry no token name, assert over
> `annotations.Retirements()` instead and say so in a comment.

- [ ] **Step 3: Run it to verify it fails**

```bash
go test ./component/language/dslspec/ -run TestBNF -v
```

Expected: FAIL to compile, "undefined: BNF".

- [ ] **Step 4: Write the BNF renderer**

Create `component/language/dslspec/grammar.go`. The renderer walks `Build()` and emits
EBNF-flavoured productions. It must be DERIVED -- no literal production list.

```go
package dslspec

// The generated grammar (memql#5388, D24 of the DSL v1 freeze record).
//
// # Why a BNF ships with the engine
//
// Two consumers, and neither is a human reading it top to bottom. A model given
// the grammar in its prompt emits fewer forms the parser refuses; a decoder
// constrained to the grammar emits none. Both break the same way if the
// grammar is hand-maintained: it drifts, and a drifted grammar does not fail
// loudly -- it teaches a model a form that does not exist, and the failure
// surfaces as "the model is bad at MemQL".
//
// So it is GENERATED, from the same three tables the parser itself reads: the
// construct set and its body clauses (parser.StructFormKeywords,
// parser.TopLevelDeclKeywords, parser.BodyClauses), the annotation registry
// (component/language/annotations), and the function catalog and operator table
// (component/language/functions). Nothing here is authored twice. The page it
// writes is gated the way the attribute matrix is: `make docs-grammar-check`
// fails when the committed page is not what the tables render.
//
// # What it deliberately does NOT describe
//
// The internal query form -- the string an SDK sends to Execute -- keeps its
// own grammar and is not here. A reader given both would have no way to tell
// which one their file is written in.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/language/annotations"
	"github.com/znasllc-io/memql/component/language/functions"
	"github.com/znasllc-io/memql/component/language/parser"
)

// GrammarPath is where the generated grammar page is committed.
const GrammarPath = "docs/public/language/grammar.md"

// BNF renders the grammar with no page furniture: the productions alone, which
// is the form a prompt or a constrained decoder takes.
func BNF() string {
	var b strings.Builder
	spec := Build()

	fmt.Fprintf(&b, "(* MemQL edition %s, grammar %s. GENERATED -- do not edit. *)\n\n",
		parser.Edition, parser.GrammarVersion)

	// A file is a language line, then imports, then declarations.
	b.WriteString("<file>        ::= <use>* <declaration>*\n")
	b.WriteString("<use>         ::= \"use\" <dotted-path> \".\" \"{\" <name> (\",\" <name>)* \"}\"\n\n")

	// One production per construct, derived from the spec.
	kws := make([]string, 0, len(spec.Constructs))
	for _, c := range spec.Constructs {
		kws = append(kws, c.Keyword)
	}
	sort.Strings(kws)
	b.WriteString("<declaration> ::= " + joinAlternatives(kws) + "\n\n")

	for _, c := range sortedConstructs(spec) {
		b.WriteString(constructProduction(c))
	}

	b.WriteString(annotationProduction())
	b.WriteString(expressionProduction())
	b.WriteString(operatorProduction(functions.Operators()))
	b.WriteString(functionProduction(functions.Catalog()))
	_ = annotations.Placements
	return b.String()
}

// RenderGrammar wraps BNF in the committed page, with the front matter
// docs/ uses and the regeneration instruction.
func RenderGrammar() string {
	var b strings.Builder
	b.WriteString("---\ntitle: MemQL Grammar\naudience: public\nstatus: stable\narea: language\nowner: znas\n---\n\n")
	b.WriteString("# MemQL Grammar\n\n")
	b.WriteString("> GENERATED from the parser, the annotation registry and the function\n")
	b.WriteString("> catalog. Run `make docs-grammar` to refresh it; a hand edit is refused\n")
	b.WriteString("> by `make docs-grammar-check`.\n\n")
	b.WriteString("This is the authoring grammar for `.memql` files. The internal query\n")
	b.WriteString("form -- the string an SDK sends to `Execute` -- has its own grammar and\n")
	b.WriteString("is deliberately not described here.\n\n```ebnf\n")
	b.WriteString(BNF())
	b.WriteString("```\n")
	return b.String()
}
```

Write `sortedConstructs`, `constructProduction`, `annotationProduction`,
`expressionProduction`, `operatorProduction`, `functionProduction` and `joinAlternatives`
as small helpers in the same file. Each reads the spec or a catalog; none holds a literal
list of language forms.

- [ ] **Step 5: Run the grammar tests**

```bash
go test ./component/language/dslspec/ -run TestBNF -v
```

Expected: PASS. If `TestBNFNamesEveryConstruct` fails naming a construct, add its
production shape to `constructProduction`'s derivation -- do not special-case it.

- [ ] **Step 6: Write the failing vocabulary test**

Create `component/language/dslspec/vocabulary_test.go`:

```go
package dslspec

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/annotations"
	"github.com/znasllc-io/memql/component/language/functions"
)

// The vocabulary is the second artifact a model is given (memql#5388, D24):
// every construct, annotation, builtin and function with a description of three
// or four sentences. The grammar says what may be WRITTEN; the vocabulary says
// what each thing MEANS, which is the half a grammar cannot carry.
func TestVocabularyCoversEveryNamedThing(t *testing.T) {
	v := Vocabulary()
	byName := map[string]VocabularyEntry{}
	for _, e := range v {
		byName[e.Kind+":"+e.Name] = e
	}
	for _, c := range Build().Constructs {
		if _, ok := byName["construct:"+c.Keyword]; !ok {
			t.Errorf("the vocabulary omits the construct %q", c.Keyword)
		}
	}
	for _, p := range annotations.Placements() {
		if _, ok := byName["annotation:"+annotationNameOf(p)]; !ok {
			t.Errorf("the vocabulary omits the annotation %q", annotationNameOf(p))
		}
	}
	for _, f := range functions.Catalog() {
		if _, ok := byName["function:"+f.Name]; !ok {
			t.Errorf("the vocabulary omits the function %q", f.Name)
		}
	}
}

// D24 says "a description of three or four sentences". A one-word description
// is the shape that makes a vocabulary useless to a model: it repeats the name.
func TestVocabularyDescriptionsAreSentences(t *testing.T) {
	for _, e := range Vocabulary() {
		d := strings.TrimSpace(e.Description)
		if d == "" {
			t.Errorf("%s %q has no description", e.Kind, e.Name)
			continue
		}
		if len(strings.Fields(d)) < 6 {
			t.Errorf("%s %q's description is %d words (%q); D24 asks for three or four sentences, and a "+
				"description shorter than the name it explains teaches a model nothing",
				e.Kind, e.Name, len(strings.Fields(d)), d)
		}
	}
}

func TestVocabularyNamesNoRetiredThing(t *testing.T) {
	retired := map[string]bool{}
	for _, r := range annotations.Retirements() {
		retired[retirementNameOf(r)] = true
	}
	for _, e := range Vocabulary() {
		if e.Kind == "annotation" && retired[e.Name] {
			t.Errorf("the vocabulary advertises the retired annotation %q", e.Name)
		}
	}
}
```

- [ ] **Step 7: Write the vocabulary renderer**

Create `component/language/dslspec/vocabulary.go` with `VocabularyEntry`, `Vocabulary()`,
`VocabularyPath` and `RenderVocabulary()`, sourced from the same registries. Descriptions
come from the tables that already carry them (`Construct.Doc`, the annotation registry's
docs, `functions.Function`'s doc field). **Where a table carries no description, add it to
the TABLE, not to the renderer** -- the renderer holding prose is the hand-maintained thing
being removed.

- [ ] **Step 8: Run the vocabulary tests**

```bash
go test ./component/language/dslspec/ -run TestVocabulary -v
```

Expected: PASS after descriptions are filled in. `TestVocabularyDescriptionsAreSentences`
will name each short one; fix them at the source table.

- [ ] **Step 9: Write the generator command**

Create `cmd/dslgrammar/main.go`, modelled exactly on `cmd/attributematrix/main.go`, writing
BOTH pages and supporting `-check`:

```go
// Command dslgrammar writes the generated grammar and vocabulary pages
// (docs/public/language/grammar.md and vocabulary.md) from the parser, the
// annotation registry and the function catalog, and with -check fails when a
// committed page is stale (memql#5388).
//
//	make docs-grammar          write both pages
//	make docs-grammar-check    CI gate: each page must equal what the tables render
package main
```

- [ ] **Step 10: Wire the Makefile**

Beside the `docs-matrix` targets:

```makefile
## Regenerate the grammar and vocabulary pages (docs/public/language/)
docs-grammar:
	$(GO) run ./cmd/dslgrammar

## CI gate: the grammar and vocabulary pages must equal what the tables render
docs-grammar-check:
	$(GO) run ./cmd/dslgrammar -check
```

Add both to `.PHONY`.

- [ ] **Step 11: Generate the pages and add the staleness gate**

```bash
make docs-grammar
make docs-grammar-check
```

Then add `TestGrammarPageIsGenerated` and `TestVocabularyPageIsGenerated` to the root
package beside `TestAttributeMatrixIsGenerated`, in the same file, in the same shape.

- [ ] **Step 12: Serve them over the introspection builtins**

`memqlDocs()` and `help()` already exist. Add two siblings rather than overloading
`memqlDocs`: a caller asking for the grammar and a caller asking for the docs want
different things, and a mode argument on `memqlDocs` is a second thing to discover.

In `dsl/common/builtins.memql`:

```memql
// ---- builtinMemqlGrammar ----
//
// The generated BNF for the authoring grammar, for grammar-in-prompt and for
// syntax-only constrained decoding. Generated from the parser, the annotation
// registry and the function catalog, so it cannot disagree with what the
// cluster accepts.

@executor("memqlGrammar")
@description("Returns the generated BNF for this cluster's MemQL authoring grammar, with the edition and grammar version it was built from.")
builtin memqlGrammar {
}

// ---- builtinMemqlVocabulary ----
//
// Every construct, annotation, builtin and function this cluster knows, each
// with a description. The grammar says what may be written; this says what it
// means.

@executor("memqlVocabulary")
@description("Returns every construct, annotation, builtin and function with its description, as the vocabulary a model is given alongside the grammar.")
builtin memqlVocabulary {
  kind  string  @description("Narrow to one kind: construct, annotation, builtin or function. Omitted, every kind is returned.")
}
```

In `component/memql/engine_types.go`, add the executor name constants beside
`BuiltinExecutorMemqlDocs`. In `component/memql/executor_builtin.go`, add the two cases,
returning the rows in the id-keyed map shape every builtin reply takes (read a neighbouring
case; a builtin reply is ONE id-keyed map, not a bare slice).

- [ ] **Step 13: Test the builtins**

Add cases to `component/memql/meta_commands_test.go` (or the nearest builtin test file)
asserting each builtin resolves, returns non-empty content, and that `memqlGrammar`'s
output contains `parser.GrammarVersion`.

```bash
go test ./component/memql/ -run 'Grammar|Vocabulary' -v
```

- [ ] **Step 14: Gate the docs' examples against the corpus**

Create `test/conformance/docs_examples_test.go`:

```go
package conformance

// The docs cannot show a form the parser refuses (memql#5388, D24).
//
// Every fenced ```memql block in docs/public/language is extracted and handed
// to the parser. A block that does not parse fails this test, naming the file
// and the line the fence opens on.
//
// Why this and not "the examples are drawn from the corpus": drawing every
// example from a corpus file would mean either rewriting every page as a
// transclusion or copying files around, and both make the pages worse to read.
// The PROPERTY the record asks for is that an example which stops loading fails
// the build, and parsing every block delivers exactly that, over every page,
// including the ones nobody remembered to wire up.
//
// Blocks may opt out with the info string ```memql-invalid -- that is how a
// page SHOWS a refused form while saying it is refused, which the retirement
// tables' documentation has to do. An opted-out block is required to FAIL to
// parse, so the marker cannot be used to hide a broken example.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestDocsExamplesParse(t *testing.T) {
	const dir = "../../docs/public/language"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	fence := regexp.MustCompile("(?s)```memql(-invalid)?\\n(.*?)```")
	total := 0
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".md" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for _, m := range fence.FindAllStringSubmatch(string(b), -1) {
			invalid, src := m[1] == "-invalid", m[2]
			if strings.TrimSpace(src) == "" {
				continue
			}
			total++
			err := parseDocExample(t, src) // Step 15 writes this
			switch {
			case invalid && err == nil:
				t.Errorf("%s: a ```memql-invalid block PARSES; either the form came back or the marker is wrong:\n%s", e.Name(), src)
			case !invalid && err != nil:
				t.Errorf("%s: a ```memql example does not parse: %v\n%s", e.Name(), err, src)
			}
		}
	}
	if total == 0 {
		t.Fatal("no ```memql blocks were found in docs/public/language at all: this gate would pass over " +
			"a docs tree with every example broken, which is the fail-open shape it exists to prevent")
	}
	t.Logf("docs examples: %d ```memql blocks parsed", total)
}
```

- [ ] **Step 15: Write `parseDocExample` and fix what it finds**

`parseDocExample` parses one source through the same entry point the loader uses. A doc
example is usually a FRAGMENT (a single `filter` line, an `args` block), so the helper must
either wrap a fragment in a minimal enclosing construct or skip blocks that are plainly
fragments. **Prefer wrapping**, and make the wrapping rule explicit and small:

- a block starting with a construct keyword parses as a file;
- a block starting with `row =>` parses as a lambda;
- anything else is a fragment and is skipped with a `t.Logf` naming it, so the skip count
  is visible and a page of nothing but fragments is not silently uncovered.

Run it, and **fix every example it names.** Expect real findings: `memql.md`,
`authoring-rules.md` and `first-program.md` predate three grammar epics.

```bash
go test ./test/conformance/ -run TestDocsExamplesParse -v 2>&1 | tail -60
```

- [ ] **Step 16: Verify and commit**

```bash
make docs-grammar-check
go test ./component/language/dslspec/ -v 2>&1 | tail -5
go test ./component/memql/ -run 'Grammar|Vocabulary' -v 2>&1 | tail -5
go test ./test/conformance/ -run TestDocsExamplesParse -v 2>&1 | tail -10
go test -count=1 . 2>&1 | tail -5
```

```bash
git add component/language/dslspec/grammar.go component/language/dslspec/grammar_test.go \
        component/language/dslspec/vocabulary.go component/language/dslspec/vocabulary_test.go \
        cmd/dslgrammar/main.go \
        docs/public/language/grammar.md docs/public/language/vocabulary.md \
        component/memql/executor_builtin.go component/memql/engine_types.go \
        dsl/common/builtins.memql Makefile \
        test/conformance/docs_examples_test.go
git commit -m "Issue #5388: the generated BNF and vocabulary, served over the introspection builtins"
```

---

### Task 4: memqlbreaking (#5389)

A new command that diffs two corpora and classifies every difference as `parse`, `meaning`
or `wire`, refusing a deletion that carries no reserved entry.

**Files:**
- Create: `cmd/memqlbreaking/main.go`
- Create: `cmd/memqlbreaking/surface.go`
- Create: `cmd/memqlbreaking/diff.go`
- Create: `cmd/memqlbreaking/reserved.go`
- Create: `cmd/memqlbreaking/diff_test.go`
- Create: `cmd/memqlbreaking/testdata/` (the deliberately-breaking fixtures)
- Create: `component/language/reserved.json` (the reservation ledger)
- Modify: `.github/workflows/ci.yml`
- Modify: `Makefile`

**Interfaces:**
- Consumes: `dslspec.Build()`, `annotations.Placements()`, `annotations.Retirements()`,
  `functions.Catalog()`, `parser.Edition`, `parser.GrammarVersion`.
- Produces:
  - `Surface` -- the machine-readable authoring surface of one tree:
    `{Edition, GrammarVersion string, Constructs, Annotations, Functions map[string]Item, Shapes map[string][]string}`
  - `Finding{Category, Name, Was, Now, Detail string}` with `Category` one of
    `parse` / `meaning` / `wire`
  - `Diff(old, new Surface, res Reservations) []Finding`
  - `Reservations` read from `component/language/reserved.json`

- [ ] **Step 1: Write the failing diff test**

Create `cmd/memqlbreaking/diff_test.go`:

```go
package main

import "testing"

// memqlbreaking classifies a change on three axes (memql#5389, D21): parse (a
// form stops parsing), meaning (the same text does something different) and
// wire (a generated SDK method, a shape key or an event payload changes name).
//
// The command exists because a breaking change reaches a bundle author as a
// load failure in their tree, weeks later, with no way to tell a deliberate
// retirement from an accident. A classified, named finding at the commit that
// causes it is the whole product.

func TestDeletedAnnotationWithoutReservationIsParse(t *testing.T) {
	old := Surface{Annotations: map[string]Item{"serverOnly": {Name: "serverOnly"}}}
	now := Surface{Annotations: map[string]Item{}}
	fs := Diff(old, now, Reservations{})
	if len(fs) != 1 {
		t.Fatalf("want 1 finding, got %d: %+v", len(fs), fs)
	}
	if fs[0].Category != CategoryParse {
		t.Errorf("a deleted annotation is a parse break, got %q", fs[0].Category)
	}
	if fs[0].Name != "serverOnly" {
		t.Errorf("the finding does not name what was deleted: %+v", fs[0])
	}
}

// The reservation is what makes a deletion SAFE rather than merely recorded: a
// reserved name can never come back with another meaning, which is the failure
// a bare deletion leaves open -- a bundle written against the old meaning loads
// under the new one and does something else.
func TestDeletedAnnotationWithReservationIsAccepted(t *testing.T) {
	old := Surface{Annotations: map[string]Item{"rateLimit": {Name: "rateLimit"}}}
	now := Surface{Annotations: map[string]Item{}}
	res := Reservations{Annotations: map[string]string{"rateLimit": "memql#5375: read by nothing; the tool's limit was never enforced"}}
	if fs := Diff(old, now, res); len(fs) != 0 {
		t.Errorf("a reserved deletion is not a finding, got %+v", fs)
	}
}

func TestShapeKeyRenameIsWire(t *testing.T) {
	old := Surface{Shapes: map[string][]string{"folderCard": {"id", "name", "createdAt"}}}
	now := Surface{Shapes: map[string][]string{"folderCard": {"id", "title", "createdAt"}}}
	fs := Diff(old, now, Reservations{})
	if len(fs) != 1 || fs[0].Category != CategoryWire {
		t.Fatalf("a shape key rename is a wire break, got %+v", fs)
	}
	if fs[0].Was != "name" || fs[0].Now != "title" {
		t.Errorf("the finding does not name both sides of the rename: %+v", fs[0])
	}
}

func TestAnnotationArgFormChangeIsMeaning(t *testing.T) {
	old := Surface{Annotations: map[string]Item{"cache": {Name: "cache", Form: "ttl=<number>"}}}
	now := Surface{Annotations: map[string]Item{"cache": {Name: "cache", Form: "<number>"}}}
	fs := Diff(old, now, Reservations{})
	if len(fs) != 1 || fs[0].Category != CategoryMeaning {
		t.Fatalf("an argument-form change is a meaning break, got %+v", fs)
	}
}

// An ADDITION is never a finding. memqlbreaking reports BREAKS; a command that
// reports every change is a command whose output nobody reads, and the one
// signal that matters drowns.
func TestAdditionIsNotAFinding(t *testing.T) {
	old := Surface{Annotations: map[string]Item{}}
	now := Surface{Annotations: map[string]Item{"loop": {Name: "loop"}}}
	if fs := Diff(old, now, Reservations{}); len(fs) != 0 {
		t.Errorf("an added annotation is not a break, got %+v", fs)
	}
}

func TestEditionChangeIsReported(t *testing.T) {
	old := Surface{Edition: "2025"}
	now := Surface{Edition: "2026"}
	fs := Diff(old, now, Reservations{})
	if len(fs) == 0 {
		t.Fatal("an edition change is reported: it is the one change that reclassifies every other finding")
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

```bash
go test ./cmd/memqlbreaking/ -v
```

Expected: FAIL to compile.

- [ ] **Step 3: Write the surface capture**

Create `cmd/memqlbreaking/surface.go`:

```go
package main

// The authoring SURFACE of one tree: the thing a bundle author writes against,
// captured as data so two of them can be diffed.
//
// It is deliberately NOT the whole spec. A surface holds the names and shapes a
// change to which breaks somebody else's file; it excludes documentation,
// ordering and anything generated FROM the surface, because a diff that reports
// a reworded doc comment is a diff whose real findings are buried.

type Item struct {
	Name string   `json:"name"`
	Form string   `json:"form,omitempty"` // the argument form, where one exists
	On   []string `json:"on,omitempty"`   // the receivers that accept it
}

type Surface struct {
	Edition        string              `json:"edition"`
	GrammarVersion string              `json:"grammarVersion"`
	Constructs     map[string]Item     `json:"constructs"`
	Annotations    map[string]Item     `json:"annotations"`
	Functions      map[string]Item     `json:"functions"`
	Shapes         map[string][]string `json:"shapes"`
}

// Capture builds the surface of the tree this binary was compiled from.
func Capture() Surface { /* read dslspec.Build(), annotations, functions */ }
```

Shapes come from loading the DSL tree; read how `cmd/memqllint` boots a loader offline and
reuse that path rather than writing a second one.

- [ ] **Step 4: Write the diff and the reservation ledger**

Create `cmd/memqlbreaking/diff.go` implementing `Diff` to satisfy Step 1's tests, and
`cmd/memqlbreaking/reserved.go` reading `component/language/reserved.json`:

```json
{
  "readme": [
    "APPEND-ONLY. A name here was removed from the authoring surface and may never return with another meaning.",
    "memqlbreaking refuses a deletion that has no entry here; that refusal is the gate (memql#5389, D21).",
    "The concept-field ledger (component/conceptfields/concept-fields.snapshot.json) is the same rule for concept fields, and predates this file."
  ],
  "annotations": {},
  "constructs": {},
  "functions": {}
}
```

Seed it from `annotations.Retirements()` -- every already-retired name belongs here, or the
first run reports the whole of epic 4 as unreserved breaks.

- [ ] **Step 5: Run the tests**

```bash
go test ./cmd/memqlbreaking/ -v
```

Expected: PASS.

- [ ] **Step 6: Write the command**

`cmd/memqlbreaking/main.go`:

```go
// Command memqlbreaking classifies the authoring-surface changes between two
// MemQL trees as parse, meaning or wire breaks, and refuses a deletion that
// carries no reserved entry (memql#5389, D21).
//
//	memqlbreaking -capture -out surface.json     write this tree's surface
//	memqlbreaking -baseline surface.json         diff this tree against it
//	memqlbreaking -baseline surface.json -json   the same, machine-readable
//
// Exit codes: 0 no breaks; 1 breaks found; 2 bad usage.
package main
```

- [ ] **Step 7: Write the self-test over a deliberately breaking fixture**

The record names this specifically: "a `memqlbreaking` self-test over a deliberately
breaking fixture". Add to `diff_test.go`:

```go
// TestSelfTestOverABreakingFixture is the negative control: the committed
// baseline mutated in four ways, one per category plus one reserved deletion,
// run through the real Diff. A gate nobody has seen fire is a gate nobody has
// tested.
func TestSelfTestOverABreakingFixture(t *testing.T) {
	base := readSurface(t, "testdata/baseline.json")
	broken := readSurface(t, "testdata/breaking.json")
	fs := Diff(base, broken, readReservations(t, "testdata/reserved.json"))
	got := map[Category]int{}
	for _, f := range fs {
		got[f.Category]++
	}
	for _, want := range []Category{CategoryParse, CategoryMeaning, CategoryWire} {
		if got[want] == 0 {
			t.Errorf("the breaking fixture produced no %s finding; the fixture or the classifier has stopped covering that axis", want)
		}
	}
}
```

Commit `testdata/baseline.json`, `testdata/breaking.json` and `testdata/reserved.json`.

- [ ] **Step 8: Capture the committed baseline**

```bash
mkdir -p component/language/surface
go run ./cmd/memqlbreaking -capture -out component/language/surface/2026.json
```

This is the previous edition's corpus the CI check diffs against. Commit it.

- [ ] **Step 9: Wire CI and the Makefile**

Makefile:

```makefile
## Classify authoring-surface breaks against the committed baseline
memqlbreaking:
	$(GO) run ./cmd/memqlbreaking -baseline component/language/surface/2026.json

## Refresh the committed surface baseline (do this in the PR that breaks it, with the reservation)
memqlbreaking-capture:
	$(GO) run ./cmd/memqlbreaking -capture -out component/language/surface/2026.json
```

In `ci.yml`'s `go-checks` job:

```yaml
      - name: memqlbreaking (authoring-surface break classification)
        run: make memqlbreaking
```

- [ ] **Step 10: Run it against the real tree**

```bash
make memqlbreaking
```

Expected: `SUCCESS: no breaks against component/language/surface/2026.json` (the baseline
was just captured from this tree, so the first run is necessarily clean). **Then prove it
can fail:** delete one annotation from the registry, re-run, confirm it reports a `parse`
break naming it, and restore. Record the observed output in the commit body.

- [ ] **Step 11: Verify and commit**

```bash
go test ./cmd/memqlbreaking/ -v
make memqlbreaking
go test ./scripts/ci/ -v 2>&1 | tail -5
```

```bash
git add cmd/memqlbreaking component/language/reserved.json \
        component/language/surface/2026.json Makefile .github/workflows/ci.yml
git commit -m "Issue #5389: memqlbreaking classifies parse, meaning and wire breaks against a reserved ledger"
```

---

### Task 5: The release -- deprecation window, the manifest flip, the extension (#5390)

**Files:**
- Create: `component/language/deprecation/window.go`
- Create: `component/language/deprecation/window_test.go`
- Modify: `component/language/tiers/limits.go` (the window as a manifest value)
- Modify: `test/conformance/2026/manifest.json`
- Create: `test/conformance/2026/files-memql-1.0/` (the language-line cases)
- Modify: `component/language/parser/grammar_version.go`
- Modify: `editors/vscode/package.json`
- Modify: `editors/vscode/CHANGELOG.md`
- Modify: `docs/public/language/memql.md`

**Interfaces:**
- Consumes: `parser.Edition`, `parser.GrammarVersion`, `tiers` limits, the extension
  parity gate that already exists (Step 1 finds it by name).
- Produces, in package `component/language/deprecation`:
  - `type Window struct { MinorReleases int; DeprecatedAt, Current string }` -- the
    manifest values, not constants
  - `func New(Window) *Tracker`
  - `func (*Tracker) Message(form, replacement string) string` -- the load-time warning
  - `func (*Tracker) Record(form string)` -- the deprecated-use counter
  - `func (*Tracker) Counts() map[string]int`
  - `func (*Tracker) Refuses(form string) bool` -- false inside the window, true after it

- [ ] **Step 1: Find the existing extension parity gate**

```bash
cd /home/znas/memql-projects/epic-dsl-v1-freeze
grep -rn "grammarVersion\|memql.edition" --include=*.go . | grep -i test | head
```

D25 says a gate already compares `editors/vscode/package.json`'s pins with
`parser.GrammarVersion`. **Read it before touching either side**; Step 8 extends it rather
than adding a second one.

- [ ] **Step 2: Write the failing deprecation-window test**

Create `component/language/deprecation/window_test.go`:

```go
package deprecation

import "testing"

// The deprecation window (memql#5390, D22): a public language form warns,
// naming its replacement, for at least two minor releases before it refuses,
// and deprecated use is COUNTED so the removal rests on evidence rather than on
// a guess about who is still writing it.
//
// The counter is the half that is easy to leave out and is the whole point. A
// warning tells one author at one load. A count tells the person deciding
// whether to remove the form whether anyone is using it.

func TestWarnNamesTheReplacement(t *testing.T) {
	w := New(Window{MinorReleases: 2})
	got := w.Message("cond(", "the ternary `p ? a : b`")
	if got == "" {
		t.Fatal("Message returned nothing")
	}
	for _, want := range []string{"cond(", "ternary", "2"} {
		if !contains(got, want) {
			t.Errorf("the warning does not name %q: %q", want, got)
		}
	}
}

func TestUseIsCounted(t *testing.T) {
	w := New(Window{MinorReleases: 2})
	w.Record("cond(")
	w.Record("cond(")
	w.Record("has")
	if got := w.Counts()["cond("]; got != 2 {
		t.Errorf("cond( counted %d times, want 2", got)
	}
	if got := w.Counts()["has"]; got != 1 {
		t.Errorf("has counted %d times, want 1", got)
	}
}

// "It refuses only after the window" is the acceptance's second half, and a
// window that is a constant cannot be tested at both ends. The record's
// cross-cutting rule is explicit: values, not constants.
func TestRefusesOnlyAfterTheWindow(t *testing.T) {
	w := New(Window{MinorReleases: 2, DeprecatedAt: "0.22", Current: "0.23"})
	if w.Refuses("cond(") {
		t.Error("one minor release after deprecation is inside the window: it warns, it does not refuse")
	}
	w = New(Window{MinorReleases: 2, DeprecatedAt: "0.22", Current: "0.24"})
	if !w.Refuses("cond(") {
		t.Error("two minor releases after deprecation is the end of the window: it refuses")
	}
}
```

- [ ] **Step 3: Run it to verify it fails, then implement**

```bash
go test ./component/language/deprecation/ -v
```

Then write `window.go`. Keep it a leaf package (standard library only) so
`component/language` can consume it without a cycle. The window's default lives in
`component/language/tiers/limits.go` beside the depth cap and the budgets, per the record's
"values, not constants" rule.

- [ ] **Step 4: Flip the corpus manifest**

`test/conformance/2026/manifest.json` reads `"status": "draft"`. Make it:

```json
{
  "edition": "2026",
  "language": "1.0",
  "status": "frozen",
  "frozenAt": "2026-09-20",
  "note": "Edition 2026 / language 1.0. A form in this corpus is the language; changing one is a memqlbreaking finding (cmd/memqlbreaking) and needs a reserved entry to delete."
}
```

Find what reads `status` first (`grep -rn '"status"' test/conformance/`) and make the
reader assert `frozen` rather than ignoring it -- a status field nothing reads is a comment.

- [ ] **Step 5: Add the language-line corpus cases**

`#5390` names `test/conformance/2026/files-memql-1.0`. Create it with the cases that pin
the language line's contract, each with its `expect.json`:

| File | Verdict | Pins |
|---|---|---|
| `declares-1.0.memql` + `memql.toml` | `load_ok` | the line the engine writes |
| `declares-nothing/` | `refuse_load` | a mounted domain with no `memql.toml` |
| `declares-2.0/` | `refuse_load` | a language newer than the engine, naming both |
| `declares-edition-2025/` | `refuse_load` | an edition this engine does not read |

Read `test/conformance/2026/negative/`'s directory shape and match it exactly.

- [ ] **Step 6: Bump the grammar version**

Epic 6 changes no authored form, so the digest suffix should NOT move. **Run the drift test
first and let it tell you:**

```bash
go test ./component/language/parser/ -run TestGrammarVersionCarriesTheSurfaceDigest -v
go test ./component/language/parser/ -run Drift -v
```

If the digest is unchanged, leave `GrammarVersion` alone and add only the documentation
block recording the freeze. If it moved, something in this epic changed the authored
surface -- **find out what before editing the constant**; the constant is the last edit, not
the first.

Add to the doc comment above the constant:

```go
// # 2026.09 -- the freeze (memql#5390)
//
// Edition 2026 and language 1.0 are FROZEN. The corpus manifest
// (test/conformance/2026/manifest.json) says so, cmd/memqlbreaking classifies
// every later change to the authoring surface against the committed baseline in
// component/language/surface/2026.json, and a deletion needs an entry in
// component/language/reserved.json.
//
// What the freeze is, precisely: a form the corpus holds is the language. It
// may be ADDED to without ceremony -- an addition breaks nobody -- and it is
// removed only through the deprecation window (component/language/deprecation):
// a load-time warning naming the replacement, for at least two minor releases,
// with the use counted so the removal rests on evidence.
```

- [ ] **Step 7: Bump the extension**

`editors/vscode/package.json`: `"version": "0.5.1"` becomes `"0.6.0"` (a minor: the
extension now carries the grammar and vocabulary of a frozen edition). Its `memql` block's
`edition` and `grammarVersion` must equal `parser.Edition` and `parser.GrammarVersion` --
if Step 6 left the version alone, this block is already correct and only the version and
changelog move.

`editors/vscode/CHANGELOG.md`, a new top entry (no emojis):

```markdown
## 0.6.0 -- 2026-09-20

MemQL DSL edition 2026 / language 1.0 is frozen.

- The language server, TextMate grammar, language configuration and snippets are
  regenerated for edition 2026.
- The generated grammar and vocabulary the engine serves over `memqlGrammar()`
  and `memqlVocabulary()` are the same tables this extension is generated from,
  so a completion the editor offers is a form the cluster accepts.
- Connect-time comparison: a cluster newer than this extension shows a notice
  naming both versions and the release to install, and the language server keeps
  working on the forms it knows; a cluster older than this extension warns that
  the editor may offer forms that cluster refuses.
```

- [ ] **Step 8: Regenerate the extension assets and run the parity gate**

```bash
grep -n "vscode" Makefile | head -20   # find the regeneration target
make <the regeneration target>
go test ./... -run 'Extension|Vscode|Parity' 2>&1 | tail -20
```

Run the `vscode-extension` lane's own checks:

```bash
cd editors/vscode && npm ci && npm run compile && npm test 2>&1 | tail -20
```

> Extension tests flake under parallel load. If one fails, re-run it alone before treating
> it as a finding.

- [ ] **Step 9: Document the freeze**

Add a short section to `docs/public/language/memql.md` linking the three new artifacts
(`grammar.md`, `vocabulary.md`, `reserved.md`) and stating the freeze rule in one paragraph.
Keep the examples parseable -- Task 3's `TestDocsExamplesParse` now gates this file.

- [ ] **Step 10: Verify and commit**

```bash
go test ./component/language/deprecation/ -v
go test ./component/language/parser/ -v 2>&1 | tail -5
go test ./test/conformance/ -run 'Corpus|Manifest|DocsExamples' -v 2>&1 | tail -10
go test -count=1 . 2>&1 | tail -5
```

```bash
git add component/language/deprecation component/language/tiers/limits.go \
        test/conformance/2026/manifest.json test/conformance/2026/files-memql-1.0 \
        component/language/parser/grammar_version.go \
        editors/vscode/package.json editors/vscode/CHANGELOG.md \
        docs/public/language/memql.md
git commit -m "Issue #5390: the freeze -- deprecation window, edition 2026 / language 1.0, the extension release"
```

---

### Task 6: Whole-tree verification and the PR

- [ ] **Step 1: Run the full suite**

```bash
cd /home/znas/memql-projects/epic-dsl-v1-freeze
make test 2>&1 | tail -30
```

- [ ] **Step 2: Run the db-gated trees the way CI does**

```bash
MEMQL_REQUIRE_DB=1 MEMQL_DIFFERENTIAL_REQUIRED=1 \
  MEMQL_DATABASE_DSN='postgres://memql:memql_dev@localhost:15434/memql?sslmode=disable' \
  go test -count=1 -timeout=900s ./test/conformance/... 2>&1 | tail -30
```

- [ ] **Step 3: Run every gate this epic touched or could have broken**

```bash
go test -count=1 . 2>&1 | tail -10                  # root: docs gates, generated-asset gates
go test ./scripts/ci/... -v 2>&1 | tail -10         # CI-shape gates
make docs-grammar-check
make docs-matrix-check
make memqlbreaking
make frontdoor-hosts-check && make frontdoor-paths-check
```

- [ ] **Step 4: Open the PR**

```bash
git push -u origin epic/dsl-v1-freeze
gh pr create --repo znasllc-io/memql --base main --title "Epic #5385: DSL v1 freeze" --body "$(cat <<'EOF'
<the body: what each issue delivered, the evidence for each acceptance criterion,
and the negative-control observations recorded in Tasks 1, 4>

Closes #5386
Closes #5387
Closes #5388
Closes #5389
Closes #5390
EOF
)"
```

> `Closes #a, #b, #c` on one line links only the FIRST issue. One `Closes #n` per line.

- [ ] **Step 5: Delete this plan in the merge**

The epic bodies require it: `git rm docs/superpowers/plans/2026-09-20-dsl-v1-freeze.md`
in the final commit on the branch.

- [ ] **Step 6: Watch CI, then enqueue**

```bash
gh pr checks <n> --repo znasllc-io/memql --watch
scripts/dev/merge-as-owner.sh --pr=<n> --check
scripts/dev/merge-as-owner.sh --pr=<n>
```

`ci-required` is the only required check. `Analyze (go)` and `install-cluster-e2e` are red
on pristine main and not required -- but `install-cluster-e2e` DOES test the branch, so a
red there is a log to read.

- [ ] **Step 7: Close the epic issue and clean up**

```bash
gh issue close 5385 --repo znasllc-io/memql --reason completed
git worktree remove /home/znas/memql-projects/epic-dsl-v1-freeze
git branch -d epic/dsl-v1-freeze
```
