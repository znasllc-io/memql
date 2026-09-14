package memql

// expr_lower_fuzz_test.go -- FuzzLower, the one lowering held to one property
// over any input the edition-2026 parser accepts (epic memql#5363, memql#5369;
// D5 and D23: fuzz targets for the lexer, the parser and the lowering).
//
//	go test -run='^$' -fuzz=FuzzLower -fuzztime=60s github.com/znasllc-io/memql/component/memql/
//
// The property: lowering a parsed expression -- at a query filter, over a
// fixed concept that declares a field of every type the lowering types
// against (lowerTestConceptSource), with a fixed set of declared arguments,
// and again at a spec body, which takes none -- either produces the
// executor's IR or refuses; it never panics. When it answers, the IR is also
// printed canonically (what the result cache keys on) and run through the
// in-process twin over a fixed row, which walk every node it holds. A lambda
// of one parameter lowers its body with that parameter as the row; anything
// else lowers as the body of `row => ...`.
//
// The seeds are testdata/fuzz/FuzzLower/: every pushdown expression the
// conformance corpus holds, the differential lane's generated matrix and the
// three-way agreement matrix, plus the inputs a fuzz run once broke. `go test`
// replays them on every run.

import (
	"encoding/json"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/ast"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/language/tiers"
)

func FuzzLower(f *testing.F) {
	f.Add(`row => row.status == args.status && row.priority > args.n`)
	f.Add(`row => args.status == nil || row.status == args.status`)

	file, err := languageParser.ParseFile(lowerTestConceptSource)
	if err != nil {
		f.Fatalf("parse the fuzz concept: %v", err)
	}
	var decl *languageParser.ConceptDecl
	for _, d := range file.Definitions {
		if cd, ok := d.(*languageParser.ConceptDecl); ok {
			decl = cd
		}
	}
	concept, err := memoryNodes.BuildConceptFromDecl(decl, "v1:lowerfuzz:ticket")
	if err != nil {
		f.Fatalf("build the fuzz concept: %v", err)
	}
	args := map[string]ArgType{"status": "string", "n": "int", "xs": "array", "flag": "bool", "any": ""}
	payload, _ := json.Marshal(map[string]any{
		"title": "INC-1 disk", "status": "open", "priority": 2, "tags": []any{"a", 1}, "blob": "x",
		"lineage": map[string]any{"planId": "p"}, "urgent": "maybe",
	})
	row := memoryNodes.MemoryNode{ID: "v1:lowerfuzz:ticket:t1", Concept: "v1:lowerfuzz:ticket", Payload: payload}

	f.Fuzz(func(t *testing.T, src string) {
		n, err := languageParser.ParseV1Expression(src)
		if err != nil {
			return
		}
		param, body := "row", n
		if lam, ok := n.(*ast.LambdaExpr); ok && lam != nil && len(lam.Params) == 1 {
			param, body = lam.Params[0], lam.Body
		}
		for _, env := range []LowerEnv{
			{Position: tiers.PositionQueryFilter, Param: param, Concept: concept, Args: args, Predicate: lowerTestSpecs()},
			{Position: tiers.PositionSpecBody, Param: param, Concept: concept, Predicate: lowerTestSpecs()},
		} {
			ir, err := Lower(body, env)
			if err != nil {
				continue
			}
			_ = canonicalExpression(ir)
			_, _ = nodeMatchesExpression(row, ir, map[string]map[string]any{})
		}
	})
}
