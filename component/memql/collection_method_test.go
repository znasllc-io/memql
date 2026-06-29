package memql

import (
	"testing"

	parser "github.com/znasllc-io/memql/component/language/parser"
)

// evalChain parses, converts (logic context), and evaluates a collection
// chain expression over the given caller args. It mirrors the engine's
// single-statement logic body path: parse -> ConvertExpression(WithCollectionMethods)
// -> evaluate.
func evalChain(t *testing.T, src string, args map[string]any) any {
	t.Helper()
	pexpr, err := parser.ParseExpression(src)
	if err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	engineExpr, err := NewASTConverter(WithCollectionMethods()).ConvertExpression(pexpr)
	if err != nil {
		t.Fatalf("convert %q: %v", src, err)
	}
	coll, ok := engineExpr.(*CollectionMethodExpression)
	if !ok {
		t.Fatalf("%q converted to %T, want *CollectionMethodExpression", src, engineExpr)
	}
	val, err := evaluateCollectionMethodExpression(coll, args)
	if err != nil {
		t.Fatalf("evaluate %q: %v", src, err)
	}
	return val
}

func sampleMembers() map[string]any {
	return map[string]any{
		"members": []any{
			map[string]any{"name": "alice", "role": "admin", "active": true, "age": 30, "email": "a@x"},
			map[string]any{"name": "bob", "role": "admin", "active": false, "age": 25, "email": "b@x"},
			map[string]any{"name": "carol", "role": "user", "active": true, "age": 40, "email": "c@x"},
			map[string]any{"name": "dave", "role": "user", "active": true, "age": 20, "email": "d@x"},
		},
	}
}

func TestCollectionEval(t *testing.T) {
	args := sampleMembers()

	if got := evalChain(t, `args.members.where(m => m.role == "admin" && m.active).count()`, args); got != 1 {
		t.Errorf("where+count = %v, want 1", got)
	}
	if got := evalChain(t, `args.members.count()`, args); got != 4 {
		t.Errorf("count = %v, want 4", got)
	}
	if got := evalChain(t, `args.members.count(m => m.active)`, args); got != 3 {
		t.Errorf("count(lambda) = %v, want 3", got)
	}
	if got := evalChain(t, `args.members.any(m => m.role == "user")`, args); got != true {
		t.Errorf("any = %v, want true", got)
	}
	if got := evalChain(t, `args.members.all(m => m.active)`, args); got != false {
		t.Errorf("all = %v, want false", got)
	}
	if got := evalChain(t, `args.members.empty()`, args); got != false {
		t.Errorf("empty = %v, want false", got)
	}
	if got := evalChain(t, `args.members.sum(m => m.age)`, args); got != float64(115) {
		t.Errorf("sum = %v, want 115", got)
	}
	if got := evalChain(t, `args.members.avg(m => m.age)`, args); got != float64(115)/4 {
		t.Errorf("avg = %v, want 28.75", got)
	}
	if got := evalChain(t, `args.members.min(m => m.age)`, args); got != float64(20) {
		t.Errorf("min = %v, want 20", got)
	}
	if got := evalChain(t, `args.members.max(m => m.age)`, args); got != float64(40) {
		t.Errorf("max = %v, want 40", got)
	}

	// where -> select -> collection of emails (active members).
	sel := evalChain(t, `args.members.where(m => m.active).select(m => m.email)`, args)
	emails, ok := sel.([]any)
	if !ok || len(emails) != 3 {
		t.Fatalf("select = %v, want 3 emails", sel)
	}

	// first with predicate.
	first := evalChain(t, `args.members.first(m => m.role == "user")`, args)
	fm, ok := first.(map[string]any)
	if !ok || fm["name"] != "carol" {
		t.Errorf("first(user) = %v, want carol", first)
	}

	// orderBy age ascending then first.
	youngest := evalChain(t, `args.members.orderBy(m => m.age).first()`, args)
	ym, ok := youngest.(map[string]any)
	if !ok || ym["name"] != "dave" {
		t.Errorf("orderBy.first = %v, want dave", youngest)
	}
	// orderByDesc age then first.
	oldest := evalChain(t, `args.members.orderByDesc(m => m.age).first()`, args)
	om, ok := oldest.(map[string]any)
	if !ok || om["name"] != "carol" {
		t.Errorf("orderByDesc.first = %v, want carol", oldest)
	}

	// take / skip.
	taken := evalChain(t, `args.members.take(2)`, args).([]any)
	if len(taken) != 2 {
		t.Errorf("take(2) len = %d, want 2", len(taken))
	}
	skipped := evalChain(t, `args.members.skip(3)`, args).([]any)
	if len(skipped) != 1 {
		t.Errorf("skip(3) len = %d, want 1", len(skipped))
	}

	// distinct by role.
	roles := evalChain(t, `args.members.distinct(m => m.role)`, args).([]any)
	if len(roles) != 2 {
		t.Errorf("distinct(role) len = %d, want 2", len(roles))
	}

	// groupBy role -> 2 groups.
	groups := evalChain(t, `args.members.groupBy(m => m.role)`, args).([]any)
	if len(groups) != 2 {
		t.Fatalf("groupBy len = %d, want 2", len(groups))
	}
	g0 := groups[0].(map[string]any)
	if g0["key"] != "admin" {
		t.Errorf("group0 key = %v, want admin", g0["key"])
	}
	if items, ok := g0["items"].([]any); !ok || len(items) != 2 {
		t.Errorf("group0 items = %v, want 2", g0["items"])
	}
}

func TestCollectionReduceAndContains(t *testing.T) {
	args := map[string]any{"nums": []any{1, 2, 3, 4}}
	// reduce sum.
	sum := evalChain(t, `args.nums.reduce(0, (acc, n) => acc)`, args)
	// Identity body returns acc each step, so the seed (0) threads through.
	if f, ok := runtimeToFloat64(sum); !ok || f != 0 {
		t.Errorf("reduce identity acc = %v (%T), want numeric 0", sum, sum)
	}

	// contains value.
	if got := evalChain(t, `args.nums.contains(3)`, args); got != true {
		t.Errorf("contains(3) = %v, want true", got)
	}
	if got := evalChain(t, `args.nums.contains(9)`, args); got != false {
		t.Errorf("contains(9) = %v, want false", got)
	}
}
