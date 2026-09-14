package memql

// expr_eval_fuzz_test.go -- FuzzEvalExpr, the in-process evaluator held to one
// property over any input the edition-2026 parser accepts (epic memql#5363,
// memql#5369; D5 and D23: fuzz targets for the lexer, the parser and the
// lowering, and the evaluator that answers where nothing lowers).
//
//	go test -run='^$' -fuzz=FuzzEvalExpr -fuzztime=60s github.com/znasllc-io/memql/component/memql/
//
// The property: evaluating a parsed expression against a fixed scope returns a
// value or an *ExprError -- never a panic, never an error of another type (the
// scope below installs no hook whose own errors would pass through), and never
// a value JSON cannot carry, because every value EvalExpr returns is handed on
// as a payload, a step argument or a condition. The step budget is small, so
// an input that nests collection methods ends in expression_budget_exceeded
// rather than running on. A failure the fuzzer finds is kept as a seed under
// testdata/fuzz/FuzzEvalExpr/, so `go test` replays it on every run.

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/language/ast"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// fuzzEvalSeeds reach every node kind, every catalog function and method, the
// absence table's awkward values and the arithmetic edges, so the fuzzer
// mutates from the shapes the evaluator has rules about.
var fuzzEvalSeeds = []string{
	`args.status == "open" && actor.role == "admin"`,
	`args.?ticket.status ?? "none"`,
	`row.value == "" || row.value == nil || row.value != "e"`,
	`row.value < "f" && row.value <= 1 && row.value > 0 && row.value >= -1.5`,
	`row.value in ["", "a", nil, 1, true]`,
	`"a" in row.tags && row.tags.count() > 0`,
	`row.title startsWith ["INC-", ""] || row.title.includes(" ")`,
	`!(row.status in ["closed"]) ? -row.priority * 2 : row.priority % 3`,
	`(args.a + args.b) / (args.c - 1)`,
	`"n=" + toString(args.n) + [1, 2] + nil`,
	`{title: args.title ?? "untitled", tags: [args.tag, args.missing], n: -args.n}`,
	`args.items.where(i => i.qty > 0).select(i => i.name).distinct().take(2).skip(1)`,
	`args.items.orderBy(i => i.rank).first().name ?? args.items.orderByDesc(i => i.rank).last().name`,
	`args.items.groupBy(i => i.kind).count() + args.one.single() + args.none.empty()`,
	`args.scores.sum(s => s) + args.scores.min(s => s) + args.scores.max(s => s) + args.scores.avg(s => s)`,
	`args.scores.reduce(0, (acc, s) => acc + s) + args.result.nodes().count()`,
	`args.items.any(i => args.items.all(j => j.rank <= i.rank))`,
	`lower(args.s) + upper(args.s) + trim(args.s) + hash(args.s) + shortId("v1:work:goal:g1")`,
	`canonicalId(args.id, "v1:work:goal") + addDuration(now, "P1D") + toString(daysBetween("2026-01-01", now))`,
	`args.x != nil ? args.x : error("x is required")`,
	`var("region") + systemVar("r") + secret("s") + systemSecret("t")`,
	`query tickets(status: "open").count()`,
	`isOpen(row) && isOpen(args.ticket)`,
	`9223372036854775807 + 1`,
	`-9223372036854775807 - 2`,
	`1e308 * 10`,
	`args.big.sum(x => x)`,
	`args.big.avg(x => x)`,
	`1 / 0`,
	`7.5 % 2`,
	`"é".count() == 1`,
	`row.?lineage.planId == nil`,
	`row.id + row.concept + row.createdAt + row.createdBy + row.type`,
	`args.items.0.name`,
	`args.items.-1.name`,
	`p ? a : q ? b : c`,
}

// fuzzEvalScope is the fixed scope every input is evaluated against: args
// holding each kind of value the absence table names, a caller, and a row.
func fuzzEvalScope() MapScope {
	return MapScope{
		"args": map[string]any{
			"status": "open", "s": " Mixed Case ", "id": "g1", "n": 3.0, "a": 1.0, "b": 2.0, "c": 1.0,
			"x": "given", "title": "", "tag": "ops",
			"ticket": map[string]any{"status": "open", "priority": 5.0},
			"items": []any{
				map[string]any{"name": "disk", "qty": 1.0, "rank": 2.0, "kind": "hw"},
				map[string]any{"name": "fan", "qty": 0.0, "rank": 1.0, "kind": "hw"},
				map[string]any{"name": "doc", "qty": 3.0, "rank": 3.0, "kind": "sw"},
			},
			"scores": []any{1.0, 2.0, 3.0},
			"big":    []any{math.MaxFloat64, math.MaxFloat64},
			"one":    []any{"only"},
			"result": map[string]any{"nodes": []any{map[string]any{"id": "a"}}},
			"null":   nil,
		},
		"actor": map[string]any{"userId": "u1", "role": "admin"},
		"row": ExprRow{
			ID: "v1:x:ticket:t1", Concept: "v1:x:ticket", Type: "object", CreatedBy: "u1",
			CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			Payload: map[string]any{
				"title": "INC-1 disk", "status": "open", "priority": 2.0, "value": "", "tags": []any{"a"},
				"lineage": nil,
			},
		},
	}
}

// TestEvalExprAggregateStaysFinite pins the first thing FuzzEvalExpr found: a
// sum or mean that leaves the finite range returned +Inf, a value JSON cannot
// carry, where `+` on the same numbers is arithmetic_overflow.
func TestEvalExprAggregateStaysFinite(t *testing.T) {
	for _, src := range []string{`args.big.sum(x => x)`, `args.big.avg(x => x)`, `args.big.min(x => x * 10)`} {
		n, err := languageParser.ParseV1Expression(src)
		if err != nil {
			t.Fatalf("parse %q: %v", src, err)
		}
		v, err := EvalExpr(context.Background(), n, fuzzEvalScope(), EvalOptions{})
		var ee *ExprError
		if !errors.As(err, &ee) || ee.Code != "arithmetic_overflow" {
			t.Errorf("EvalExpr(%q) = %v, %v; want the refusal arithmetic_overflow", src, v, err)
		}
	}
	n, _ := languageParser.ParseV1Expression(`args.scores.sum(s => s)`)
	if v, err := EvalExpr(context.Background(), n, fuzzEvalScope(), EvalOptions{}); err != nil || v != 6.0 {
		t.Errorf("a finite sum = %v, %v; want 6", v, err)
	}
}

func FuzzEvalExpr(f *testing.F) {
	for _, s := range fuzzEvalSeeds {
		f.Add(s)
	}
	opts := EvalOptions{
		Now:    time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Budget: 20_000,
		Predicates: func(name string) (string, ast.ExpressionNode, bool) {
			if name != "isOpen" {
				return "", nil, false
			}
			body, err := languageParser.ParseV1Expression(`t.status == "open"`)
			if err != nil {
				return "", nil, false
			}
			return "t", body, true
		},
	}
	f.Fuzz(func(t *testing.T, src string) {
		n, err := languageParser.ParseV1Expression(src)
		if err != nil {
			return
		}
		v, err := EvalExpr(context.Background(), n, fuzzEvalScope(), opts)
		if err != nil {
			var ee *ExprError
			if !errors.As(err, &ee) {
				t.Fatalf("EvalExpr(%q) failed with %T, not an *ExprError: %v", src, err, err)
			}
			return
		}
		if _, err := json.Marshal(v); err != nil {
			t.Fatalf("EvalExpr(%q) = %#v, which JSON cannot carry: %v", src, v, err)
		}
	})
}
