package parser

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/ast"
)

// v1BodyCases are the sources the statement parser must accept. Each is a
// whole file, parsed the way the loaders parse one (NormaliseAll, then
// ParseFile); want is the body rendered by renderBody, one statement per line,
// blocks indented two spaces. TestTransitionalDispatchIsExact reads this table
// too: every source here must take the native path.
var v1BodyCases = []struct {
	name string
	src  string
	want string
}{
	{
		name: "a logic with args and three statements",
		src: `logic routeStatus {
  args {
    submitterRole any
  }
  role := args.submitterRole ?? ""
  admin := role == "admin" || role == "writer"
  return role == "owner" ? "queued" : admin ? "needs_approval" : "needs_validation"
}`,
		want: `role := args.submitterRole ?? ""
admin := role == "admin" || role == "writer"
return role == "owner" ? "queued" : admin ? "needs_approval" : "needs_validation"`,
	},
	{
		name: "an automation with a trigger, args, a precondition and statements",
		src: `@trigger(event="node.created", concept="v1:forge:request")
automation routeRequest {
  args {
    id            any
    submitterRole any
  }
  precondition hasId {
    check: args.id != nil
    literal: id
  }
  decide := logic routeStatus(submitterRole: args.submitterRole)
  mutation advanceRequest(requestId: args.id, status: decide)
}`,
		want: `decide := logic routeStatus(submitterRole: args.submitterRole)
mutation advanceRequest(requestId: args.id, status: decide)`,
	},
	{
		name: "every call kind",
		src: `automation kinds {
  rows := query activeUsers(status: "active")
  written := mutation touch(id: rows.first().id)
  decided := logic decide(n: rows.count())
  sized := builtin measure(rows: rows)
  sub := automation sweep(limit: 10)
  released := action applyRelease(version: "1.2.3")
}`,
		want: `rows := query activeUsers(status: "active")
written := mutation touch(id: rows.first().id)
decided := logic decide(n: rows.count())
sized := builtin measure(rows: rows)
sub := automation sweep(limit: 10)
released := action applyRelease(version: "1.2.3")`,
	},
	{
		name: "if, else if and else",
		src: `automation branches {
  if args.kind == "a" {
    mutation one()
  } else if args.kind == "b" {
    x := builtin two()
    mutation three(x: x)
  } else {
    return
  }
}`,
		want: `if args.kind == "a"
  mutation one()
else if args.kind == "b"
  x := builtin two()
  mutation three(x: x)
else
  return`,
	},
	{
		name: "for with and without if",
		src: `automation loops {
  rows := query q()
  for r in rows if r.active {
    mutation touch(id: r.id)
  }
  for r in rows {
    builtin log(id: r.id)
  } on error continue
}`,
		want: `rows := query q()
for r in rows if r.active
  mutation touch(id: r.id)
for r in rows on error continue
  builtin log(id: r.id)`,
	},
	{
		name: "switch with a two-label case and a default",
		src: `automation cases {
  switch args.status {
    case "a", "b" {
      mutation early()
    }
    case 3 {
      mutation three()
    }
    default {
      mutation other()
    }
  }
}`,
		want: `switch args.status
  case "a", "b"
    mutation early()
  case 3
    mutation three()
  default
    mutation other()`,
	},
	{
		name: "parallel with two branches, wait any and on error continue",
		src: `automation fanOut {
  parallel {
    branch fast {
      a := builtin one()
    }
    branch slow {
      b := builtin two()
      mutation record(b: b)
    }
  } wait any on error continue
}`,
		want: `parallel wait any on error continue
  branch fast
    a := builtin one()
  branch slow
    b := builtin two()
    mutation record(b: b)`,
	},
	{
		name: "publish with a payload map",
		src: `automation announce {
  publish "request.routed" { a: 1, b: args.x }
}`,
		want: `publish "request.routed" {a: 1, b: args.x}`,
	},
	{
		name: "return of a call, and a bare return before the brace",
		src: `logic ensure {
  args {
    id any
  }
  if args.id == nil {
    return
  }
  return builtin ensureDailySpace(userId: args.id) retry(2)
}`,
		want: `if args.id == nil
  return
return builtin ensureDailySpace(userId: args.id) retry(2)`,
	},
	{
		name: "trailing clauses in the canonical order",
		src: `automation clauses {
  a := action deploy(v: 1) on surface("ops") retry(3) on error continue
  action deploy(v: 2) on surface("ops")
  b := query q() retry(1)
  mutation m() on error continue
  c := builtin b() retry(2) on error continue
}`,
		want: `a := action deploy(v: 1) on surface("ops") retry(3) on error continue
action deploy(v: 2) on surface("ops")
b := query q() retry(1)
mutation m() on error continue
c := builtin b() retry(2) on error continue`,
	},
	{
		name: "a multi-line call whose arguments carry comments",
		src: `automation commented {
  out := mutation record(
    // the request
    requestId: args.id,
    status:    "queued", // routed
  )
  return out
}`,
		want: `out := mutation record(requestId: args.id, status: "queued")
return out`,
	},
	{
		name: "an expression continued by a trailing && and by a leading ||",
		src: `automation continued {
  ready := args.a != nil &&
    args.b != nil
  either := args.c == 1
    || args.d == 2
  return ready || either
}`,
		want: `ready := args.a != nil && args.b != nil
either := args.c == 1 || args.d == 2
return ready || either`,
	},
	{
		name: "a doc comment and an annotation on a logic",
		src: `use common.builtins.{ measure }

/// Measure the thing.
@actor
logic measured {
  return builtin measure(who: actor.userId)
}`,
		want: `return builtin measure(who: actor.userId)`,
	},
	{
		name: "a one-line block holds one statement",
		src: `automation oneLine {
  if args.x { return 1 }
  mutation m()
}`,
		want: `if args.x
  return 1
mutation m()`,
	},
}

// parseV1BodyFile parses src as the loaders do and returns its one statement
// definition.
func parseV1BodyFile(t *testing.T, src string) *FunctionDef {
	t.Helper()
	rewritten, err := NormaliseAll(src)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	file, err := ParseFile(rewritten)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, d := range file.Definitions {
		if fn, ok := d.(*FunctionDef); ok {
			return fn
		}
	}
	t.Fatalf("no definition in %s", src)
	return nil
}

func v1Body(t *testing.T, fn *FunctionDef) *ast.Body {
	t.Helper()
	auto, ok := fn.Body.(*AutomationDef)
	if !ok || auto.Body == nil {
		t.Fatalf("%s did not parse to a statement body (it took the legacy path): %#v", fn.Name, fn.Body)
	}
	return auto.Body
}

func TestV1BodyParses(t *testing.T) {
	for _, c := range v1BodyCases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			fn := parseV1BodyFile(t, c.src)
			if got := renderBody(v1Body(t, fn).Statements, ""); got != c.want {
				t.Fatalf("body:\n%s\nwant:\n%s", got, c.want)
			}
		})
	}
}

func TestV1BodyDefinition(t *testing.T) {
	fn := parseV1BodyFile(t, v1BodyCases[1].src)
	if fn.Type != FunctionTypeAutomation || fn.Name != "routeRequest" || !fn.Enabled || !fn.ExpressionsV1 {
		t.Fatalf("definition = %+v", fn)
	}
	auto := fn.Body.(*AutomationDef)
	if auto.Trigger == nil || auto.Trigger.Event != "node.created" || !auto.ExpressionsV1 || !auto.Enabled {
		t.Fatalf("automation = %+v, want the trigger attached, enabled, v1", auto)
	}
	if fn.ArgsSchema == nil || len(fn.ArgsSchema.Fields) != 2 {
		t.Fatalf("args = %+v, want the two declared fields", fn.ArgsSchema)
	}
	if len(auto.Steps) != 0 {
		t.Fatalf("a statement body has no legacy steps, got %d", len(auto.Steps))
	}

	logic := parseV1BodyFile(t, v1BodyCases[12].src)
	if logic.Type != FunctionTypeLogic || !strings.Contains(logic.DocComment, "Measure the thing") {
		t.Fatalf("logic = %+v, want its doc comment", logic)
	}
	if len(logic.Attributes) != 1 || logic.Attributes[0].Name != "actor" {
		t.Fatalf("logic attributes = %+v, want @actor", logic.Attributes)
	}
}

func TestV1BodySpans(t *testing.T) {
	fn := parseV1BodyFile(t, `automation spans {
  rows := query q(a: 1)
  if rows.count() > 0 {
    mutation touch(n: rows.count())
  }
}`)
	stmts := v1Body(t, fn).Statements
	assign := stmts[0].(*ast.AssignStatement)
	if assign.NameSpan.Line != 2 || assign.NameSpan.Col != 3 || assign.Span.Line != 2 || assign.Span.EndLine != 2 {
		t.Errorf("assign spans = %+v / %+v", assign.NameSpan, assign.Span)
	}
	if assign.Call.Span.Line != 2 || assign.Call.Span.Col != 11 {
		t.Errorf("call span = %+v, want 2:11", assign.Call.Span)
	}
	ifs := stmts[1].(*ast.IfStatement)
	if ifs.Span.Line != 3 || ifs.Span.EndLine != 5 {
		t.Errorf("if span = %+v, want lines 3-5", ifs.Span)
	}
	inner := ifs.Branches[0].Body[0].(*ast.CallStatement)
	if inner.Span.Line != 4 || inner.Span.Col != 5 {
		t.Errorf("inner call span = %+v, want 4:5", inner.Span)
	}
}

// renderBody prints statements one per line, blocks indented, expressions in
// canonical source: what each case above expects.
func renderBody(stmts []ast.BodyStatement, indent string) string {
	var lines []string
	for _, s := range stmts {
		lines = append(lines, renderStatement(s, indent)...)
	}
	return strings.Join(lines, "\n")
}

func renderStatement(s ast.BodyStatement, indent string) []string {
	child := func(body []ast.BodyStatement, in string) []string {
		if len(body) == 0 {
			return nil
		}
		return strings.Split(renderBody(body, in), "\n")
	}
	var out []string
	switch t := s.(type) {
	case *ast.AssignStatement:
		v := ""
		if t.Call != nil {
			v = renderCall(t.Call)
		} else {
			v = ast.FormatExpr(t.Value)
		}
		out = append(out, indent+t.Name+" := "+v+renderMods(t.Mods))
	case *ast.CallStatement:
		out = append(out, indent+renderCall(t.Call)+renderMods(t.Mods))
	case *ast.IfStatement:
		for i, b := range t.Branches {
			head := "if " + formatOrEmpty(b.Cond)
			switch {
			case i > 0 && b.Cond != nil:
				head = "else if " + ast.FormatExpr(b.Cond)
			case b.Cond == nil:
				head = "else"
			}
			out = append(out, indent+head)
			out = append(out, child(b.Body, indent+"  ")...)
		}
	case *ast.ForStatement:
		head := "for " + t.Var + " in " + ast.FormatExpr(t.Source)
		if t.Filter != nil {
			head += " if " + ast.FormatExpr(t.Filter)
		}
		out = append(out, indent+head+renderMods(t.Mods))
		out = append(out, child(t.Body, indent+"  ")...)
	case *ast.SwitchStatement:
		out = append(out, indent+"switch "+ast.FormatExpr(t.Subject))
		for _, c := range t.Cases {
			if c.Default {
				out = append(out, indent+"  default")
			} else {
				var ls []string
				for _, l := range c.Labels {
					ls = append(ls, ast.FormatExpr(l))
				}
				out = append(out, indent+"  case "+strings.Join(ls, ", "))
			}
			out = append(out, child(c.Body, indent+"    ")...)
		}
	case *ast.ParallelStatement:
		head := "parallel"
		if t.Wait == "any" {
			head += " wait any"
		}
		out = append(out, indent+head+renderMods(t.Mods))
		for _, b := range t.Branches {
			out = append(out, indent+"  branch "+b.Label)
			out = append(out, child(b.Body, indent+"    ")...)
		}
	case *ast.PublishStatement:
		out = append(out, indent+"publish "+strconv.Quote(t.Topic)+" "+ast.FormatExpr(t.Payload))
	case *ast.ReturnStatement:
		switch {
		case t.Call != nil:
			out = append(out, indent+"return "+renderCall(t.Call)+renderMods(t.Mods))
		case t.Value != nil:
			out = append(out, indent+"return "+ast.FormatExpr(t.Value))
		default:
			out = append(out, indent+"return")
		}
	default:
		out = append(out, indent+fmt.Sprintf("<%T>", s))
	}
	return out
}

func formatOrEmpty(e ast.ExpressionNode) string {
	if e == nil {
		return ""
	}
	return ast.FormatExpr(e)
}

func renderCall(c *ast.ConstructCall) string {
	var args []string
	for _, a := range c.Args {
		args = append(args, a.Name+": "+ast.FormatExpr(a.Value))
	}
	s := c.Kind + " " + c.Name + "(" + strings.Join(args, ", ") + ")"
	if c.Surface != "" {
		s += " on surface(" + strconv.Quote(c.Surface) + ")"
	}
	return s
}

func renderMods(m ast.StatementMods) string {
	s := ""
	if m.Retry > 0 {
		s += " retry(" + strconv.Itoa(m.Retry) + ")"
	}
	if m.OnError != "" {
		s += " on error " + m.OnError
	}
	return s
}
