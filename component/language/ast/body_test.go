package ast

import (
	"reflect"
	"sort"
	"testing"
)

// ccall builds a construct call; id is v1_format_test.go's helper.
func ccall(kind, name string, args ...NamedArg) *ConstructCall {
	return &ConstructCall{Kind: kind, Name: name, Args: args}
}

// sampleBody is `x := a` / `if c { y := query q(); for i in y { mutation m(v: i) } } else { return x }`.
func sampleBody() []BodyStatement {
	return []BodyStatement{
		&AssignStatement{Name: "x", Value: id("a")},
		&IfStatement{Branches: []IfBranch{
			{Cond: id("c"), Body: []BodyStatement{
				&AssignStatement{Name: "y", Call: ccall("query", "q")},
				&ForStatement{Var: "i", Source: id("y"), Body: []BodyStatement{
					&CallStatement{Call: ccall("mutation", "m", NamedArg{Name: "v", Value: id("i")})},
				}},
			}},
			{Body: []BodyStatement{
				&ReturnStatement{Value: id("x")},
			}},
		}},
	}
}

func visitLabel(s BodyStatement) string {
	switch t := s.(type) {
	case *AssignStatement:
		return "assign " + t.Name
	case *CallStatement:
		return "call " + t.Call.Name
	case *IfStatement:
		return "if"
	case *ForStatement:
		return "for " + t.Var
	case *ReturnStatement:
		return "return"
	default:
		return StatementKind(s)
	}
}

func TestWalkBodyVisitsInSourceOrder(t *testing.T) {
	var got []string
	WalkBody(sampleBody(), func(s BodyStatement) bool {
		got = append(got, visitLabel(s))
		return true
	})
	want := []string{"assign x", "if", "assign y", "for i", "call m", "return"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("visit order = %v, want %v", got, want)
	}
}

func TestWalkBodySkipsChildrenOnFalse(t *testing.T) {
	var got []string
	WalkBody(sampleBody(), func(s BodyStatement) bool {
		got = append(got, visitLabel(s))
		_, isIf := s.(*IfStatement)
		return !isIf
	})
	want := []string{"assign x", "if"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("visit order = %v, want %v (the if's children must be skipped)", got, want)
	}
}

// statementSamples holds one statement of every kind, each carrying marker
// identifiers in every expression field the kind has. The marker names say
// which field they sit in, so a helper that forgets a field fails by name.
func statementSamples() map[string]struct {
	stmt BodyStatement
	want []string
} {
	type sample = struct {
		stmt BodyStatement
		want []string
	}
	return map[string]sample{
		"assign": {&AssignStatement{Name: "a", Value: id("assignValue")}, []string{"assignValue"}},
		"assignCall": {&AssignStatement{Name: "a", Call: ccall("query", "q", NamedArg{Name: "k", Value: id("assignCallArg")})},
			[]string{"assignCallArg"}},
		"call": {&CallStatement{Call: ccall("mutation", "m", NamedArg{Name: "k", Value: id("callArg")})}, []string{"callArg"}},
		"if": {&IfStatement{Branches: []IfBranch{
			{Cond: id("ifCond"), Body: []BodyStatement{&AssignStatement{Name: "n", Value: id("nested")}}},
			{Cond: id("elseIfCond")},
			{},
		}}, []string{"ifCond", "elseIfCond"}},
		"for": {&ForStatement{Var: "i", Source: id("forSource"), Filter: id("forFilter")}, []string{"forSource", "forFilter"}},
		"switch": {&SwitchStatement{Subject: id("switchSubject"), Cases: []CaseArm{
			{Labels: []ExpressionNode{id("caseLabel")}},
			{Default: true},
		}}, []string{"switchSubject", "caseLabel"}},
		"parallel": {&ParallelStatement{Branches: []ParallelBranch{{Label: "b", Body: []BodyStatement{
			&AssignStatement{Name: "n", Value: id("nested")},
		}}}}, nil},
		"publish": {&PublishStatement{Topic: "t", Payload: &MapExpr{Entries: []MapEntry{{Key: "k", Value: id("publishValue")}}}},
			[]string{"publishValue"}},
		"return": {&ReturnStatement{Value: id("returnValue")}, []string{"returnValue"}},
	}
}

func identNames(exprs []ExpressionNode) []string {
	var out []string
	for _, e := range exprs {
		WalkV1(e, func(n ExpressionNode) bool {
			if i, ok := n.(*IdentExpr); ok {
				out = append(out, i.Name)
			}
			return true
		})
	}
	sort.Strings(out)
	return out
}

func TestStatementExpressionsCoversEveryField(t *testing.T) {
	samples := statementSamples()
	for name, s := range samples {
		got := identNames(StatementExpressions(s.stmt))
		want := append([]string(nil), s.want...)
		sort.Strings(want)
		if len(got) == 0 && len(want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: StatementExpressions = %v, want %v (a statement's own expressions, not its nested statements')", name, got, want)
		}
	}
	// Every statement kind the package declares has a sample above, so a kind
	// added without teaching StatementExpressions about it fails here.
	covered := map[string]bool{}
	for _, s := range samples {
		covered[StatementKind(s.stmt)] = true
	}
	for _, k := range BodyStatementKinds() {
		if !covered[k] {
			t.Errorf("statement kind %q has no sample in statementSamples: add one carrying a marker in every expression field it has", k)
		}
	}
}

func TestStatementKindNamesEveryType(t *testing.T) {
	kinds := map[string]bool{}
	for _, k := range BodyStatementKinds() {
		if kinds[k] {
			t.Fatalf("BodyStatementKinds lists %q twice", k)
		}
		kinds[k] = true
	}
	for name, s := range statementSamples() {
		if k := StatementKind(s.stmt); !kinds[k] {
			t.Errorf("%s: StatementKind = %q, which BodyStatementKinds does not list", name, k)
		}
	}
}
