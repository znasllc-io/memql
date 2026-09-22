package conformance

// differential_db_test.go -- the differential lane (memql#5369; D5, D7 and D23
// of docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md).
//
// A pushdown expression has two implementations of one meaning: the SQL
// memql.Lower and the executor push down, and memql.EvalExpr, which answers the
// same expression in process wherever nothing pushes down. D7 holds them equal,
// and this lane is where they are compared on the rows the language finds
// awkward: an absent key, JSON null, "", " ", 0, 1, 1.5, false, true, "e",
// "E", "é", a stored "1", a nested object with an absent intermediate, the
// arrays [], ["a"] and [1, 2, 3], a scalar and an object where an array is
// expected, and a string where a boolean is.
//
// For every expression -- each `lower` and pushdown `evaluate` case of the
// corpus, and a generated matrix over the absence table and the typed
// comparisons -- it declares a query whose filter is that expression, boots an
// engine over a real database, writes the rows through the engine's own insert
// form, and reads them back two ways: through the executor's real read path
// (the query, called with its arguments), and through EvalExpr over each stored
// row. A row one side selects and the other does not is a disagreement, and so
// is a row EvalExpr refuses while the SQL answers -- a filter would skip it and
// a refine clause over the same rows would fail the read. Each is named with
// the case file (or the generated expression), the row and both answers.
//
// There is no allowance: every row of every expression must agree, the
// mistyped ones included. A stored value of the wrong type is data on both
// sides (component/memql/expr_stored.go): not equal, not ordered, not a
// member, not true, and counted by what it holds.
//
// REQUIRED since memql#5386: the `mcp-conformance` job sets
// MEMQL_DIFFERENTIAL_REQUIRED=1, and under it EVERY disagreement is a test
// error -- not just the first, so a divergence's width is readable from one
// run. Unset (a local run, a developer machine), a disagreement is logged on a
// line starting `DIFFERENTIAL:` and the test passes. Zero disagreements is the
// lane's expected state either way. An expression the lowering refuses at load
// is not a disagreement -- the refusal IS its answer, and the corpus pins
// refusals -- but the lane counts them, and fails when it compared nothing at
// all.
//
// WHERE IT RUNS. The `mcp-conformance` job of .github/workflows/ci.yml runs
// `go test -count=1 -timeout=600s -v ./test/conformance/...` against a
// TimescaleDB service container with MEMQL_DATABASE_DSN pointing at it, so
// this lane runs there on every change to the Go tree, the DSL tree or the
// corpus. That job is one of ci-required's needs, and
// scripts/ci/differential_lane_required_test.go holds both halves -- the env
// flag and the membership -- because each fails open on its own. Locally it
// reaches Postgres the way the MCP dimensions beside it do (tryDB,
// harness_test.go); without one it skips, and MEMQL_REQUIRE_DB=1 turns the
// skip into a failure.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/ast"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/language/tiers"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/component"
	"github.com/znasllc-io/memql/core/dslfs"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// differentialRequired reports whether a disagreement fails the test.
func differentialRequired() bool { return os.Getenv("MEMQL_DIFFERENTIAL_REQUIRED") == "1" }

// laneDomain is the overlay domain the lane's scratch concept lives in.
const laneDomain = "differential"

// laneConcept is the scratch concept: every field but the two labels is typed
// any, so a row stores whatever JSON the matrix needs.
const laneConcept = `/// The differential lane's scratch row. Every field but the two labels is
/// typed any, so one row can store whatever JSON the absence table and the
/// typed comparisons need.
concept probe {
  lane   string
  label  string
  value  any
  tags   any
  obj    any
  flag   any
}
`

// laneExpr is one expression the lane compares.
type laneExpr struct {
	source  string         // where it came from: a case file, or "generated"
	domain  string         // the overlay domain whose concept it is over
	concept string         // the concept's bare name
	lambda  string         // the lambda, as the case or the matrix writes it
	args    map[string]any // the arguments the call binds
	actor   map[string]any // the caller, when the case names one
	preds   func(string) (string, ast.ExpressionNode, bool)
	query   string // the generated query's name, set when the tree is built
}

// laneRow is one row the lane writes.
type laneRow struct {
	label   string
	payload map[string]any
}

// laneValues are the stored values the matrix puts in an `any` field.
var laneValues = []laneRow{
	{"null", map[string]any{"value": nil}},
	{"empty", map[string]any{"value": ""}},
	{"space", map[string]any{"value": " "}},
	{"zero", map[string]any{"value": 0}},
	{"one", map[string]any{"value": 1}},
	{"one-and-a-half", map[string]any{"value": 1.5}},
	{"false", map[string]any{"value": false}},
	{"true", map[string]any{"value": true}},
	{"e-acute", map[string]any{"value": "é"}},
	{"upper-e", map[string]any{"value": "E"}},
	{"lower-e", map[string]any{"value": "e"}},
	{"text-one", map[string]any{"value": "1"}},
	{"a", map[string]any{"value": "a"}},
	{"abc", map[string]any{"value": "abc"}},
	{"tags-empty", map[string]any{"tags": []any{}}},
	{"tags-a", map[string]any{"tags": []any{"a"}}},
	{"tags-numbers", map[string]any{"tags": []any{1, 2, 3}}},
	{"tags-scalar", map[string]any{"tags": "a"}},
	{"tags-null", map[string]any{"tags": nil}},
	{"tags-object", map[string]any{"tags": map[string]any{"k": "a"}}},
	{"obj-empty", map[string]any{"obj": map[string]any{}}},
	{"obj-a", map[string]any{"obj": map[string]any{"a": "x"}}},
	{"obj-a-null", map[string]any{"obj": map[string]any{"a": nil}}},
	{"obj-null", map[string]any{"obj": nil}},
	{"obj-scalar", map[string]any{"obj": "x"}},
	{"flag-true", map[string]any{"flag": true}},
	{"flag-false", map[string]any{"flag": false}},
	{"flag-null", map[string]any{"flag": nil}},
	{"flag-string-true", map[string]any{"flag": "true"}},
	{"flag-malformed", map[string]any{"flag": "maybe"}},
}

// laneMatrix is the generated half: the absence table and the typed
// comparisons, over the scratch concept.
func laneMatrix() []string {
	var out []string
	for _, v := range []string{`""`, `nil`, `"e"`, `"é"`, `"1"`, `1`, `0`, `1.5`, `true`, `false`} {
		out = append(out, "row => row.value == "+v, "row => row.value != "+v)
	}
	for _, v := range []string{`"e"`, `"f"`, `1`, `0`} {
		for _, op := range []string{"<", "<=", ">", ">="} {
			out = append(out, "row => row.value "+op+" "+v)
		}
	}
	out = append(out,
		`row => row.value in ["", "a"]`,
		`row => row.value in ["a"]`,
		`row => row.value in [nil]`,
		`row => row.value in [1, 2]`,
		`row => row.value in ["1", "a"]`,
		`row => row.value startsWith "a"`,
		`row => row.value startsWith ""`,
		`row => row.value startsWith ["x", "a"]`,
		`row => row.value.includes("b")`,
		`row => row.value.includes("")`,
		`row => !(row.value == "e")`,
		`row => !(row.value in ["a"])`,
		`row => "a" in row.tags`,
		`row => 1 in row.tags`,
		`row => row.tags.count() == 0`,
		`row => row.tags.count() > 1`,
		`row => row.tags.any(t => t == "a")`,
		`row => row.tags.all(t => t == "a")`,
		`row => row.?obj.a == "x"`,
		`row => row.?obj.a == nil`,
		`row => row.?obj.a != nil`,
		`row => row.flag == true`,
		`row => !(row.flag == true)`,
		`row => row.flag != false`,
		`row => row.flag`,
		`row => !row.flag`,
		// A bare field as an operand and as a ternary's branch is a condition
		// too, and count() counts what the field stores.
		`row => row.flag || row.value == "e"`,
		`row => row.flag == true ? true : row.flag`,
		`row => row.value.count() == 1`,
		`row => row.value.count() == 0`,
	)
	return out
}

// TestDifferentialLane compares the pushdown and the in-process answer of
// every expression over every row, and logs (or, required, fails on) each
// disagreement.
func TestDifferentialLane(t *testing.T) {
	db, hasDB := tryDB(t)
	if !hasDB {
		if os.Getenv("MEMQL_REQUIRE_DB") == "1" {
			t.Fatal("the differential lane needs Postgres, and MEMQL_REQUIRE_DB=1 makes its absence a failure")
		}
		t.Skip("the differential lane needs Postgres (MEMQL_DATABASE_DSN); it runs in the CI mcp-conformance job")
	}
	lane := fmt.Sprintf("dl%d", time.Now().UnixNano())
	t.Cleanup(func() {
		// The database is shared with every other suite and every other run;
		// the lane's rows are its own and go when it does.
		_, _ = db.NewDelete().Model((*memoryNodes.MemoryNode)(nil)).
			Where("payload->>'lane' = ?", lane).Exec(context.Background())
	})

	exprs, fixtures, caseRows := laneCorpusExpressions(t)
	for i, src := range laneMatrix() {
		exprs = append(exprs, &laneExpr{source: fmt.Sprintf("generated #%d", i+1), domain: laneDomain, concept: "probe", lambda: src})
	}
	for i, src := range laneGenerated(laneGeneratorSeed, 240) {
		exprs = append(exprs, &laneExpr{source: fmt.Sprintf("generated(seed=%d) #%d", laneGeneratorSeed, i+1), domain: laneDomain, concept: "probe", lambda: src})
	}
	tree := laneTree(t, exprs, fixtures)
	eng, stop := laneEngine(t, db, tree)
	defer stop()

	// The refusal carries its CODE, not only its text. The code is what the
	// accept-parity arm below scopes on: a TYPE RULE says the expression is
	// wrong and must be refused on both sides, while every other refusal says
	// the node has no SQL form -- which is exactly what a refine clause
	// exists to evaluate in process.
	type laneRefusal struct {
		why  string
		code string
	}
	refused := map[string]laneRefusal{}
	if rep := eng.LoadReport(); rep != nil {
		for _, s := range rep.Skipped {
			refused[s.Name] = laneRefusal{why: fmt.Sprintf("%s (%s)", s.Err, s.Phase), code: s.Code}
		}
	}

	ctx := laneContext(nil)
	written := map[string][]memoryNodes.MemoryNode{}
	for domain := range laneDomains(exprs) {
		for _, concept := range laneConceptsIn(exprs, domain) {
			id := "v1:" + domain + ":" + concept
			rows := laneRowsFor(domain, concept, fixtures, caseRows)
			written[id] = laneWrite(t, ctx, eng, db, lane, id, rows)
		}
	}

	var compared, lowered int
	var disagreements []string
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
	// PER CODE, not a total. A single counter cannot say whether both type
	// rules are actually reached, and an arm that only ever exercises one of
	// them is half-untested while reading as fully green -- the summary below
	// prints the breakdown so a reader can see which rules this run held.
	typeRulesPaired := map[string]int{}
	var formRefusals int
	for _, x := range exprs {
		if r, isRefused := refused[x.query]; isRefused {
			// ACCEPT PARITY (memql#5522). D7 holds the two evaluators equal on
			// what they ANSWER; this holds them equal on what they ACCEPT, for
			// the refusals where that is the right question.
			//
			// A TYPE RULE is a fact about the language -- a boolean has no
			// order, one `in` tests one type -- so it is true wherever the
			// expression is written, and an in-process path that answers one
			// is applying a different language from the one the author was
			// refused by. Measured before it was fixed: `row.value in [1, "1"]`
			// was refused in a filter and answered TRUE for both the stored
			// number and the stored string in a refine.
			//
			// Every OTHER refusal stays a skip, and deliberately. The generic
			// code covers "no form at this position" -- an in-process function,
			// arithmetic over the row -- and demanding a matching in-process
			// refusal for those would refuse the expressions refine exists for.
			if memql.IsTypeRuleCode(r.code) {
				if err := laneCheckTypeRules(x); err == nil {
					report(fmt.Sprintf("%s: `%s`: the SQL lowering refuses it as a type rule (%s) and the "+
						"in-process check ACCEPTS it -- one source text, two languages: %s",
						x.source, x.lambda, r.code, r.why))
					continue
				}
				typeRulesPaired[r.code]++
				continue
			}
			formRefusals++
			t.Logf("differential: %s: `%s` does not lower, so there is no SQL to compare: %s", x.source, x.lambda, r.why)
			continue
		}
		conceptID := "v1:" + x.domain + ":" + x.concept
		stored := written[conceptID]
		selected, err := laneSelect(eng, lane, x)
		if err != nil {
			report(fmt.Sprintf("%s: `%s`: the read failed: %v", x.source, x.lambda, err))
			continue
		}
		lowered++
		for _, node := range stored {
			inProcess, evalErr := laneEvaluate(eng, x, node)
			_, inSQL := selected[laneBareID(node.ID)]
			compared++
			switch {
			case evalErr != nil:
				// A refusal against an answer is a disagreement whichever way
				// the SQL answered: a query filter would skip the row, a
				// refine clause over the same rows would fail the read.
				report(fmt.Sprintf("%s: `%s` over row %s %s: SQL says %v, EvalExpr refuses: %v", x.source, x.lambda, laneLabel(node), node.Payload, inSQL, evalErr))
			case inProcess != inSQL:
				report(fmt.Sprintf("%s: `%s` over row %s %s: SQL says %v, EvalExpr says %v", x.source, x.lambda, laneLabel(node), node.Payload, inSQL, inProcess))
			}
		}
	}

	rowCount := 0
	for _, nodes := range written {
		rowCount += len(nodes)
	}
	paired, pairedTotal := 0, 0
	pairedBreakdown := make([]string, 0, len(typeRulesPaired))
	for _, code := range memql.TypeRuleCodes() {
		n := typeRulesPaired[code]
		pairedBreakdown = append(pairedBreakdown, fmt.Sprintf("%s=%d", code, n))
		pairedTotal += n
		if n > 0 {
			paired++
		}
	}
	t.Logf("differential lane (generator seed %d): %d expressions (%d lowered, %d refused at load: "+
		"%d type rules PAIRED against the in-process check [%s], %d with no SQL form) over %d rows "+
		"of %d concepts: %d comparisons, %d disagreements",
		laneGeneratorSeed, len(exprs), lowered, len(exprs)-lowered, pairedTotal,
		strings.Join(pairedBreakdown, " "), formRefusals, rowCount, len(written), compared,
		len(disagreements))
	// EVERY type rule must be reached by something, or the arm that pairs it
	// is asserting nothing. The corpus carries a case per rule for exactly
	// this reason (test/conformance/2026/expr/typeRules/), so a zero here is
	// either a missing case or a rule whose refusal stopped carrying its code
	// -- both of which make this lane quietly stop checking a language rule.
	if paired != len(memql.TypeRuleCodes()) {
		t.Errorf("the accept-parity arm reached %d of %d type rules [%s] -- a rule nothing "+
			"refuses is a rule this lane is not holding either evaluator to",
			paired, len(memql.TypeRuleCodes()), strings.Join(pairedBreakdown, " "))
	}
	for _, d := range disagreements {
		t.Log("DIFFERENTIAL: " + d)
	}
	if lowered == 0 {
		t.Fatal("the lane compared nothing: every expression was refused at load, so neither evaluator was held to anything -- is memql.Lower (memql#5366) on this branch?")
	}
}

// laneCorpusExpressions reads every pushdown expression the corpus holds: each
// `lower` and pushdown `evaluate` case, once per file, with the rows its
// directory's evaluate cases name.
func laneCorpusExpressions(t *testing.T) ([]*laneExpr, map[string]string, map[string][]laneRow) {
	t.Helper()
	fixtures := map[string]string{}
	caseRows := map[string][]laneRow{}
	seen := map[string]bool{}
	var out []*laneExpr
	for _, r := range discoverCorpus(t) {
		if r.c.Verdict != verdictLower && r.c.Verdict != verdictEvaluate {
			continue
		}
		pos := tiers.Position(r.c.Position)
		if tiers.TierOf(pos) != tiers.TierP || adapterLiteralPosition(pos) {
			continue
		}
		domain := corpusDomainName(strings.TrimPrefix(r.dir, r.edition+"/")) + "_lane"
		fixtures[domain] = r.fixture
		if r.c.Row != nil {
			caseRows[domain] = append(caseRows[domain], laneRow{label: fmt.Sprintf("%s#%d", r.c.File, r.index+1), payload: r.c.Row})
		}
		key := r.rel + fmt.Sprint(r.c.Args) + fmt.Sprint(r.c.Actor)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, &laneExpr{
			source: r.rel, domain: domain, concept: r.c.Concept, lambda: strings.TrimSpace(r.src),
			args: r.c.Args, actor: r.c.Actor, preds: adapterPredicates(r.line.Edition, r.fixture),
		})
	}
	return out, fixtures, caseRows
}

// laneTree declares the scratch concept, each corpus directory's fixture, and
// one query per expression: its filter is the expression, narrowed to this
// run's rows, and its args block declares every argument the expression reads.
func laneTree(t *testing.T, exprs []*laneExpr, fixtures map[string]string) fstest.MapFS {
	t.Helper()
	line := dslfs.Manifest{Language: langparser.LanguageVersion, Edition: langparser.Edition}
	files := map[string]*strings.Builder{}
	file := func(domain string) *strings.Builder {
		b, ok := files[domain]
		if !ok {
			b = &strings.Builder{}
			files[domain] = b
		}
		return b
	}
	file(laneDomain).WriteString(laneConcept)
	for i, x := range exprs {
		lam, err := langparser.ParseV1Lambda(x.lambda)
		if err != nil || len(lam.Params) != 1 {
			t.Fatalf("%s: `%s` is not a lambda of one parameter: %v", x.source, x.lambda, err)
		}
		param := lam.Params[0]
		x.query = fmt.Sprintf("laneQuery%d", i+1)
		b := file(x.domain)
		// A query that reads the caller declares it (#2621).
		actorLine := ""
		if laneReadsRoot(lam.Body, "actor") {
			actorLine = "@actor\n"
		}
		fmt.Fprintf(b, "\n/// The differential lane's read of %s.\n%squery %s %s {\n  args {\n    lane  string  @required\n", laneComment(x.source), actorLine, x.concept, x.query)
		for _, name := range laneArgNames(lam.Body) {
			fmt.Fprintf(b, "    %s  %s\n", name, laneArgType(x.args[name]))
		}
		fmt.Fprintf(b, "  }\n  filter %s => %s.lane == args.lane && (%s)\n  paginate 500\n}\n", param, param, ast.FormatExpr(lam.Body))
	}
	tree := fstest.MapFS{}
	for domain, b := range files {
		tree[domain+"/"+dslfs.ManifestFile] = &fstest.MapFile{Data: []byte(line.Render())}
		src := b.String()
		if f := fixtures[domain]; f != "" {
			tree[domain+"/fixture.memql"] = &fstest.MapFile{Data: []byte(laneFixtureWithLane(f))}
		}
		tree[domain+"/queries.memql"] = &fstest.MapFile{Data: []byte(src)}
	}
	return tree
}

// laneConceptHeader finds each concept declaration's opening line.
var laneConceptHeader = regexp.MustCompile(`(?m)^concept[ \t]+[A-Za-z_][A-Za-z0-9_]*[ \t]*\{[ \t]*$`)

// laneFixtureWithLane adds the `lane` field to every concept a corpus fixture
// declares, so the lane can write rows of it that only its own run reads back
// -- the database is shared, and every run writes fresh rows.
func laneFixtureWithLane(fixture string) string {
	return laneConceptHeader.ReplaceAllStringFunc(fixture, func(header string) string {
		return header + "\n  lane  string"
	})
}

// laneEngine boots an engine over the database with the lane's tree mounted.
// The boot tolerates skipped constructs (MEMQL_DSL_ALLOW_SKIPS): a query whose
// filter the lowering refuses is one expression with no SQL to compare, not a
// reason to compare none.
func laneEngine(t *testing.T, db *bun.DB, tree fstest.MapFS) (*memql.MemQLEngine, func()) {
	t.Helper()
	t.Setenv(memql.AllowSkipsEnvVar, "1")
	_, _, unmount := memqldsl.MountOverlayDomains(corpusQuiet, tree)
	stop := func() {
		unmount()
		memoryNodes.ReplaceAll(nil)
		_, _ = memql.LoadUnifiedConcepts(corpusQuiet)
	}
	if _, err := memql.LoadUnifiedConcepts(corpusQuiet); err != nil {
		stop()
		t.Fatalf("the lane's concepts do not load: %v", err)
	}
	eng, err := memql.New(db, (&component.Component{}).WithLoggerWriter(io.Discard))
	if err != nil {
		stop()
		t.Fatalf("construct the lane's engine: %v", err)
	}
	eng.Logger = corpusQuiet
	if err := eng.Init(memoryNodes.DefaultRegistry()); err != nil {
		stop()
		t.Fatalf("the lane's engine does not boot: %v", err)
	}
	return eng, stop
}

// laneContext is the caller the lane reads and writes as: a cluster owner,
// internal origin, or the case's own actor when it names one.
func laneContext(actor map[string]any) context.Context {
	user, _ := actor["userId"].(string)
	if user == "" {
		user = "user-differential-lane"
	}
	role := auth.RoleOwner
	if r, ok := actor["role"].(string); ok && r != "" {
		role = auth.Role(r)
	}
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: user, Role: role})
	ctx = auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: user})
	return auth.ContextWithInternalOrigin(ctx)
}

// laneWrite inserts each row through the engine's raw insert form and reads
// the stored nodes back. A row the concept's schema refuses is not written,
// and says so.
func laneWrite(t *testing.T, ctx context.Context, eng *memql.MemQLEngine, db *bun.DB, lane, conceptID string, rows []laneRow) []memoryNodes.MemoryNode {
	t.Helper()
	for i, row := range rows {
		payload := map[string]any{}
		for k, v := range row.payload {
			payload[k] = v
		}
		payload["lane"] = lane
		if conceptID == "v1:"+laneDomain+":probe" {
			// Only the scratch concept declares label; a corpus concept's row
			// is named by its payload.
			payload["label"] = row.label
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("row %s is not JSON: %v", row.label, err)
		}
		id := fmt.Sprintf("%s-%d", lane, i+1)
		if _, err := eng.Execute(ctx, fmt.Sprintf(`insert(%s, id=%s, payload=%s)`, ast.QuoteString(conceptID), ast.QuoteString(id), raw)); err != nil {
			t.Logf("differential: %s row %s %s is not written: %v", conceptID, row.label, raw, err)
		}
	}
	var nodes []memoryNodes.MemoryNode
	if err := db.NewSelect().Model(&nodes).Where("concept = ?", conceptID).Where("payload->>'lane' = ?", lane).Scan(context.Background()); err != nil {
		t.Fatalf("read back the lane's %s rows: %v", conceptID, err)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	return nodes
}

// laneSelect runs the expression's query through the executor's real read
// path and returns the bare ids it selected.
func laneSelect(eng *memql.MemQLEngine, lane string, x *laneExpr) (map[string]bool, error) {
	var args []string
	args = append(args, "lane: "+ast.QuoteString(lane))
	names := make([]string, 0, len(x.args))
	for name := range x.args {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if x.args[name] == nil {
			continue
		}
		args = append(args, name+": "+laneLiteral(x.args[name]))
	}
	res, err := eng.Execute(laneContext(x.actor), fmt.Sprintf("query %s(%s)", x.query, strings.Join(args, ", ")))
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	if res != nil && res.Bundle != nil {
		for _, n := range res.Bundle.GetNodes() {
			out[laneBareID(n.GetId())] = true
		}
	}
	return out, nil
}

// laneCheckTypeRules asks the IN-PROCESS load gate about the same expression
// the SQL lowering refused.
//
// It calls what a refine clause's load calls (memql.CheckTypeRules, via the
// bare-expression form), against the same bound concept, so a disagreement
// here is a disagreement the loader would have -- not an artefact of the lane
// asking a question nothing else asks.
//
// The concept is resolved from the registry rather than passed: a nil concept
// makes every field-typed side undecidable, which would let a real divergence
// (`row.flag > 1` over a declared boolean) read as agreement.
func laneCheckTypeRules(x *laneExpr) error {
	lam, err := langparser.ParseV1Lambda(x.lambda)
	if err != nil {
		return err
	}
	concept, _ := memoryNodes.Get("v1:" + x.domain + ":" + x.concept)
	return memql.CheckTypeRules(lam, concept, tiers.PositionQueryRefine)
}

// laneEvaluate answers the expression over one stored row in process.
func laneEvaluate(eng *memql.MemQLEngine, x *laneExpr, node memoryNodes.MemoryNode) (bool, error) {
	lam, err := langparser.ParseV1Lambda(x.lambda)
	if err != nil {
		return false, err
	}
	row, err := memql.NewExprRow(node)
	if err != nil {
		return false, err
	}
	actor := adapterMap(x.actor)
	if _, ok := actor["userId"]; !ok {
		actor = map[string]any{"userId": "user-differential-lane", "role": string(auth.RoleOwner)}
	}
	scope := memql.MapScope{"args": adapterMap(x.args), "actor": actor, lam.Params[0]: row}
	return memql.EvalCondition(context.Background(), lam.Body, scope, memql.EvalOptions{
		Now:        time.Now(),
		Predicates: x.preds,
		CanonicalID: func(ctx context.Context, value any, concept string) (string, error) {
			return eng.CanonicalizeIdValue(ctx, fmt.Sprint(value), concept)
		},
	})
}

// laneRowsFor is the rows a concept gets: the matrix for the scratch concept;
// for a corpus concept, a value per declared field of its type, and the rows
// the directory's evaluate cases name.
func laneRowsFor(domain, concept string, fixtures map[string]string, caseRows map[string][]laneRow) []laneRow {
	if domain == laneDomain {
		return append([]laneRow{{"absent", map[string]any{}}}, laneValues...)
	}
	rows := []laneRow{{"absent", map[string]any{}}}
	file, err := corpusParseFile(langparser.Edition, fixtures[domain])
	if err == nil && file != nil {
		for _, def := range file.Definitions {
			c, ok := def.(*ast.ConceptDecl)
			if !ok || c.Name != concept {
				continue
			}
			for _, p := range c.Properties {
				for i, v := range laneFieldValues(p) {
					rows = append(rows, laneRow{label: fmt.Sprintf("%s-%d", p.Name, i+1), payload: map[string]any{p.Name: v}})
				}
			}
		}
	}
	return append(rows, caseRows[domain]...)
}

// laneFieldValues are the values a declared field is written with: the
// awkward ones for its type.
func laneFieldValues(p *ast.PropertyDecl) []any {
	if p == nil || p.Type == nil {
		return nil
	}
	if len(p.Nested) > 0 {
		first := p.Nested[0].Name
		return []any{map[string]any{}, map[string]any{first: ""}, map[string]any{first: "p1"}}
	}
	switch p.Type.Kind {
	case "string":
		return []any{"", " ", "open", "closed", "held", "é", "E", "INC-1", "a disk is full"}
	case "enum":
		out := make([]any, 0, len(p.Type.EnumValues))
		for _, v := range p.Type.EnumValues {
			out = append(out, v)
		}
		return out
	case "datetime":
		return []any{"2026-01-01T00:00:00Z", "2099-01-01T00:00:00Z"}
	case "int":
		return []any{0, 1, 2, 4, -2}
	case "float":
		return []any{0, 1.5, -2.5}
	case "bool":
		return []any{true, false}
	case "array":
		return []any{[]any{}, []any{"urgent"}, []any{"team-a", "team-b"}}
	}
	return []any{nil, "", 0, false, "x"}
}

// laneReadsRoot reports whether an expression reads the reserved root name.
func laneReadsRoot(body ast.ExpressionNode, root string) bool {
	found := false
	ast.WalkV1(body, func(n ast.ExpressionNode) bool {
		if id, ok := n.(*ast.IdentExpr); ok && id != nil && id.Name == root {
			found = true
		}
		return !found
	})
	return found
}

// laneArgNames lists the args an expression reads, sorted.
func laneArgNames(body ast.ExpressionNode) []string {
	seen := map[string]bool{}
	ast.WalkV1(body, func(n ast.ExpressionNode) bool {
		if m, ok := n.(*ast.MemberExpr); ok && m != nil {
			if id, ok := m.Object.(*ast.IdentExpr); ok && id != nil && id.Name == "args" {
				seen[m.Field] = true
			}
		}
		return true
	})
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// laneArgType declares an argument by the value the case binds it to.
func laneArgType(v any) string {
	switch x := v.(type) {
	case bool:
		return "bool"
	case float64:
		if !math.IsInf(x, 0) && math.Trunc(x) == x {
			return "int"
		}
		return "float"
	case []any:
		return "[]string"
	case map[string]any:
		return "object"
	}
	return "string"
}

// laneLiteral renders a value as a MemQL literal for the query call.
func laneLiteral(v any) string {
	switch x := v.(type) {
	case string:
		return ast.QuoteString(x)
	case bool:
		return strconv.FormatBool(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = laneLiteral(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = k + ": " + laneLiteral(x[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return "nil"
}

func laneDomains(exprs []*laneExpr) map[string]bool {
	out := map[string]bool{}
	for _, x := range exprs {
		out[x.domain] = true
	}
	return out
}

func laneConceptsIn(exprs []*laneExpr, domain string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range exprs {
		if x.domain == domain && !seen[x.concept] {
			seen[x.concept] = true
			out = append(out, x.concept)
		}
	}
	sort.Strings(out)
	return out
}

func laneBareID(id string) string {
	if i := strings.LastIndexByte(id, ':'); i >= 0 {
		return id[i+1:]
	}
	return id
}

func laneLabel(node memoryNodes.MemoryNode) string {
	var p map[string]any
	_ = json.Unmarshal(node.Payload, &p)
	if l, ok := p["label"].(string); ok && l != "" {
		return l
	}
	return laneBareID(node.ID)
}

func laneComment(source string) string {
	return strings.ReplaceAll(source, "\n", " ")
}

// TestLaneGeneratedIsGrammarDriven holds the generator to four properties, all
// of which a hand-written list fails: it draws from the TIER MANIFEST rather
// than from a literal list, so a node kind the query-filter position stops
// admitting stops being generated and one it starts admitting is a shape to
// add rather than a silent gap; it is deterministic in its seed, so a
// disagreement found in CI is reproducible from the seed printed beside it;
// every expression it emits PARSES, so a generator bug reads as a generator
// bug rather than as a lane that compared nothing; and the operators the
// position admits all appear, because a generator that emits only `==` proves
// the two evaluators agree about equality and nothing else.
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
	// The operators the tier manifest admits at a query filter must all
	// appear.
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
	// The manifest, not a literal list, is what the shapes are selected by:
	// every shape laneShapes() offers names the node kind it produces, and a
	// kind the query-filter position does not admit must not be generated.
	// This is the half that makes "grammar-driven" falsifiable -- without it
	// the generator could be a list with a comment claiming otherwise.
	shapes := laneShapes()
	if len(shapes) == 0 {
		t.Fatal("laneShapes() is empty, so the generator draws from nothing and the properties above are vacuous")
	}
	for _, s := range shapes {
		if !tiers.Allows(tiers.PositionQueryFilter, s.kind) {
			t.Errorf("laneShapes offers a shape producing %q, which tiers.PositionQueryFilter does not admit: "+
				"the lane would compare an expression that cannot lower", s.kind)
		}
	}
	for _, kind := range []ast.NodeKind{ast.KindComparison, ast.KindIn, ast.KindStartsWith, ast.KindNot, ast.KindAnd, ast.KindOr} {
		covered := false
		for _, s := range shapes {
			if s.kind == kind {
				covered = true
				break
			}
		}
		if !covered {
			t.Errorf("no shape produces %q, a kind the query-filter position admits: the lane holds the two "+
				"evaluators to nothing about it", kind)
		}
	}
}

// laneShape is one expression shape the generator can emit.
//
// `kind` is the node kind the shape EXERCISES -- the one whose admission at a
// query filter decides whether the shape may be generated at all. For
// `row.tags.count() > 1` that is the method call, not the comparison at the
// top: a position that stopped admitting method calls is a position this shape
// cannot reach, whatever else it contains.
//
// A `composite` shape takes atoms; a leaf shape ignores the atom function.
// The split is what bounds the recursion: atoms are drawn from leaves only, so
// a term is at most two deep.
type laneShape struct {
	kind      ast.NodeKind
	composite bool
	emit      func(rnd *rand.Rand, atom func() string) string
}

// laneFields are the operands the scratch concept offers: a plain payload
// path, a second one holding booleans and strings, and the absent-safe path
// through a nested object. laneRowsFor writes value, tags, obj and flag.
var laneFields = []string{"row.value", "row.flag", "row.?obj.a"}

// laneLits is the literal set the absence table is built from: the values that
// sit either side of every boundary the two evaluators have to agree about.
var laneLits = []string{`""`, `nil`, `"a"`, `"e"`, `"é"`, `"1"`, `1`, `0`, `1.5`, `true`, `false`}

// laneCmps are the comparison operators.
var laneCmps = []string{"==", "!=", "<", "<=", ">", ">="}

func lanePick(rnd *rand.Rand, from []string) string { return from[rnd.Intn(len(from))] }

// laneShapeTable is every shape the generator knows how to write, in a stable
// order -- a map would make the generator's output depend on iteration order
// rather than on its seed. laneShapes filters it through the tier manifest.
func laneShapeTable() []laneShape {
	return []laneShape{
		{kind: ast.KindComparison, emit: func(rnd *rand.Rand, _ func() string) string {
			return fmt.Sprintf(`%s %s %s`, lanePick(rnd, laneFields), lanePick(rnd, laneCmps), lanePick(rnd, laneLits))
		}},
		{kind: ast.KindIn, emit: func(rnd *rand.Rand, _ func() string) string {
			return fmt.Sprintf(`%s in [%s, %s]`, lanePick(rnd, laneFields), lanePick(rnd, laneLits), lanePick(rnd, laneLits))
		}},
		{kind: ast.KindIn, emit: func(rnd *rand.Rand, _ func() string) string {
			// Membership the other way round: the operand is the literal and
			// the collection is the stored array, which is where a scalar, an
			// object and an absent key stored where an array was expected are
			// answered.
			return fmt.Sprintf(`%s in row.tags`, lanePick(rnd, laneLits))
		}},
		{kind: ast.KindStartsWith, emit: func(rnd *rand.Rand, _ func() string) string {
			if rnd.Intn(4) == 0 {
				return fmt.Sprintf(`%s startsWith [%s, %s]`, lanePick(rnd, laneFields),
					lanePick(rnd, laneStrLits), lanePick(rnd, laneStrLits))
			}
			return fmt.Sprintf(`%s startsWith %s`, lanePick(rnd, laneFields), lanePick(rnd, laneStrLits))
		}},
		{kind: ast.KindMethodCall, emit: func(rnd *rand.Rand, _ func() string) string {
			return fmt.Sprintf(`%s.includes(%s)`, lanePick(rnd, laneFields), lanePick(rnd, laneStrLits))
		}},
		{kind: ast.KindMethodCall, emit: func(rnd *rand.Rand, _ func() string) string {
			return fmt.Sprintf(`row.tags.count() %s %d`, lanePick(rnd, laneCmps), rnd.Intn(3))
		}},
		{kind: ast.KindLambda, emit: func(rnd *rand.Rand, _ func() string) string {
			return fmt.Sprintf(`row.tags.%s(t => t %s %s)`,
				[]string{"any", "all"}[rnd.Intn(2)], lanePick(rnd, laneCmps), lanePick(rnd, laneLits))
		}},
		{kind: ast.KindOptionalMember, emit: func(rnd *rand.Rand, _ func() string) string {
			return fmt.Sprintf(`row.?obj.a %s %s`, lanePick(rnd, laneCmps), lanePick(rnd, laneLits))
		}},
		{kind: ast.KindMember, emit: func(rnd *rand.Rand, _ func() string) string {
			// A bare field read IS a condition, and what a non-boolean stored
			// value means there is one of the questions the lane exists to
			// ask.
			return lanePick(rnd, laneFields)
		}},
		{kind: ast.KindNot, composite: true, emit: func(_ *rand.Rand, atom func() string) string {
			return "!(" + atom() + ")"
		}},
		{kind: ast.KindAnd, composite: true, emit: func(_ *rand.Rand, atom func() string) string {
			return "(" + atom() + " && " + atom() + ")"
		}},
		{kind: ast.KindOr, composite: true, emit: func(_ *rand.Rand, atom func() string) string {
			return "(" + atom() + " || " + atom() + ")"
		}},
		{kind: ast.KindTernary, composite: true, emit: func(_ *rand.Rand, atom func() string) string {
			// A BOOLEAN ternary over the row lowers to (c && p) || (!c && q);
			// a value ternary over the row is refused, so all three branches
			// are conditions.
			return "(" + atom() + " ? " + atom() + " : " + atom() + ")"
		}},
	}
}

// laneStrLits are the string prefixes and substrings the string shapes use.
var laneStrLits = []string{`"a"`, `""`, `"e"`, `"é"`, `"1"`}

// laneShapes is laneShapeTable narrowed to what the TIER MANIFEST admits at a
// query filter. This is what makes the generator grammar-driven rather than a
// list: a kind the position stops admitting stops being generated, and the
// lane stops asserting agreement about an expression that can no longer lower.
func laneShapes() []laneShape {
	var out []laneShape
	for _, s := range laneShapeTable() {
		if tiers.Allows(tiers.PositionQueryFilter, s.kind) {
			out = append(out, s)
		}
	}
	return out
}

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
	shapes := laneShapes()
	var leaves []laneShape
	for _, s := range shapes {
		if !s.composite {
			leaves = append(leaves, s)
		}
	}
	if len(shapes) == 0 || len(leaves) == 0 {
		return nil
	}

	atom := func() string { return leaves[rnd.Intn(len(leaves))].emit(rnd, nil) }
	// A term is any admitted shape: a leaf, or a composite over two or three
	// atoms. Depth stops at two -- the lane is comparing two EVALUATORS, and a
	// deeper tree tests the same operators through more parentheses.
	term := func() string { return shapes[rnd.Intn(len(shapes))].emit(rnd, atom) }

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
