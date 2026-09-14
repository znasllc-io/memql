package compiler

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/ast"
)

// The bodies here use body_scope_test.go's builders.

// compiled lowers a body that must compile and returns its steps as JSON.
func compiled(t *testing.T, kind string, b *ast.Body) string {
	t.Helper()
	steps, ps := CompileBody(kind, "probe", nil, b)
	if len(ps) > 0 {
		t.Fatalf("the body did not compile: %v", ps)
	}
	out, err := json.Marshal(steps)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// sameJSON compares two JSON texts structurally, so the expectations can be
// written by hand in any key order and with whitespace.
func sameJSON(t *testing.T, got, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Fatalf("got is not JSON: %v\n%s", err, got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("want is not JSON: %v\n%s", err, want)
	}
	gb, _ := json.Marshal(g)
	wb, _ := json.Marshal(w)
	if string(gb) != string(wb) {
		t.Fatalf("steps differ\n got: %s\nwant: %s", gb, wb)
	}
}

func TestCompileBodyStatements(t *testing.T) {
	cases := []struct {
		name string
		kind string
		body *ast.Body
		want string
	}{
		{
			name: "a call bound to a name is a function step carrying its kind and binds",
			body: body(setCall(2, "rows", "query", "activeUsers", named("status", lit("active")), named("limit", lit(int64(5))))),
			want: `[{"id":"rows","type":"function","binds":"rows",
			         "function":{"name":"activeUsers","kind":"query","args":{"status":"active","limit":5}}}]`,
		},
		{
			name: "an expression bound to a name is an expression step in canonical source",
			body: body(set(2, "role", bin("??", fld(rd("args", 2), "role"), lit("")))),
			want: `[{"id":"role","type":"expression","binds":"role","expression":"args.role ?? \"\""}]`,
		},
		{
			name: "a bare call takes its callee's name as its id and binds nothing",
			body: body(do(2, "mutation", "recordEvent", named("kind", lit("routed")))),
			want: `[{"id":"recordEvent","type":"function","function":{"name":"recordEvent","kind":"mutation","args":{"kind":"routed"}}}]`,
		},
		{
			name: "automation and action calls are their own step types",
			body: body(
				setCall(2, "sub", "automation", "sweep", named("limit", lit(int64(10)))),
				&ast.CallStatement{Call: &ast.ConstructCall{Kind: "action", Name: "applyRelease", Surface: "deploy",
					Args: []ast.NamedArg{named("version", fld(rd("sub", 3), "version"))}}, Span: sp(3, 3)},
			),
			want: `[{"id":"sub","type":"automation","binds":"sub","automation":{"name":"sweep","args":{"limit":10}}},
			        {"id":"applyRelease","type":"action","action":{"ref":"applyRelease","surface":"deploy","args":{"version":{"$expr":"sub.version"}}}}]`,
		},
		{
			name: "argument values: literals stay literals, everything else is an $expr leaf, maps and lists keep their shape",
			body: body(do(2, "builtin", "notify",
				named("to", fld(rd("actor", 2), "userId")),
				named("text", lit("hello")),
				named("when", rd("now", 2)),
				named("meta", &ast.MapExpr{Entries: []ast.MapEntry{{Key: "n", Value: lit(int64(1))}, {Key: "who", Value: fld(rd("args", 2), "id")}}}),
				named("tags", &ast.ListExpr{Elems: []ast.ExpressionNode{lit("a"), fld(rd("args", 2), "tag")}}),
				named("none", &ast.NilExpr{}),
			)),
			want: `[{"id":"notify","type":"function","function":{"name":"notify","kind":"builtin","args":{
			         "to":{"$expr":"actor.userId"},"text":"hello","when":{"$expr":"now"},
			         "meta":{"n":1,"who":{"$expr":"args.id"}},"tags":["a",{"$expr":"args.tag"}],"none":null}}}]`,
		},
		{
			name: "publish is an event step; its payload uses the same leaves",
			body: body(pub(2, "request.routed", ast.MapEntry{Key: "id", Value: fld(rd("args", 2), "id")}, ast.MapEntry{Key: "status", Value: lit("queued")})),
			want: `[{"id":"publish","type":"event","event":{"topic":"request.routed","payload":{"id":{"$expr":"args.id"},"status":"queued"}}}]`,
		},
		{
			name: "return of an expression is a return step",
			kind: "logic",
			body: body(ret(2, bin("==", fld(rd("args", 2), "role"), lit("owner")))),
			want: `[{"id":"return","type":"return","return":{"value":"args.role == \"owner\""}}]`,
		},
		{
			name: "a bare return has no value",
			body: body(ret(2, nil)),
			want: `[{"id":"return","type":"return","return":{}}]`,
		},
		{
			name: "return of a call is the call's step, marked returns",
			kind: "logic",
			body: body(retCall(2, "builtin", "ensureDailySpace", named("userId", fld(rd("args", 2), "id")))),
			want: `[{"id":"ensureDailySpace","type":"function","returns":true,
			         "function":{"name":"ensureDailySpace","kind":"builtin","args":{"userId":{"$expr":"args.id"}}}}]`,
		},
		{
			name: "retry and on error continue on a call",
			body: body(&ast.CallStatement{Call: cc(2, "builtin", "flaky"), Mods: ast.StatementMods{Retry: 2, OnError: "continue"}, Span: sp(2, 3)}),
			want: `[{"id":"flaky","type":"function","retryCount":2,"onError":"continue","function":{"name":"flaky","kind":"builtin"}}]`,
		},
		{
			name: "a for loop is a forEach step with its body in do, its variable in as",
			body: body(
				setCall(2, "rows", "query", "q"),
				&ast.ForStatement{Var: "item", VarSpan: sp(3, 7), Source: rd("rows", 3), Filter: fld(rd("item", 3), "active"),
					Mods: ast.StatementMods{OnError: "continue"}, Span: sp(3, 3),
					Body: []ast.BodyStatement{do(4, "mutation", "touch", named("id", fld(rd("item", 4), "id")))}},
			),
			want: `[{"id":"rows","type":"function","binds":"rows","function":{"name":"q","kind":"query"}},
			        {"id":"for_item","type":"forEach","onError":"continue","forEach":{"source":"rows","filter":"item.active","as":"item",
			          "do":[{"id":"touch","type":"function","function":{"name":"touch","kind":"mutation","args":{"id":{"$expr":"item.id"}}}}]}}]`,
		},
		{
			name: "a parallel is one step, each branch a block with its own list",
			body: body(&ast.ParallelStatement{Wait: "any", Span: sp(2, 3), Branches: []ast.ParallelBranch{
				pbr(3, "fast", setCall(4, "a", "builtin", "one")),
				pbr(6, "slow", setCall(7, "a", "builtin", "two")),
			}}),
			want: `[{"id":"parallel","type":"parallel","parallel":{"wait":"any","failFast":true,"branches":[
			         {"id":"fast","type":"block","block":{"steps":[{"id":"a","type":"function","binds":"a","function":{"name":"one","kind":"builtin"}}]}},
			         {"id":"slow","type":"block","block":{"steps":[{"id":"a","type":"function","binds":"a","function":{"name":"two","kind":"builtin"}}]}}]}}]`,
		},
		{
			name: "wait all is the default and is written",
			body: body(par(2, pbr(3, "only", do(4, "builtin", "one")))),
			want: `[{"id":"parallel","type":"parallel","parallel":{"wait":"all","failFast":true,"branches":[
			         {"id":"only","type":"block","block":{"steps":[{"id":"one","type":"function","function":{"name":"one","kind":"builtin"}}]}}]}}]`,
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			kind := c.kind
			if kind == "" {
				kind = "automation"
			}
			sameJSON(t, compiled(t, kind, c.body), c.want)
		})
	}
}

func TestCompileBodyFlattensOnceBlocks(t *testing.T) {
	t.Run("if, else if and else", func(t *testing.T) {
		b := body(
			set(2, "role", fld(rd("args", 2), "role")),
			ifs(3,
				br(bin("==", rd("role", 3), lit("owner")), do(4, "mutation", "queue")),
				br(bin("==", rd("role", 5), lit("admin")), do(6, "mutation", "review"), do(7, "builtin", "notify")),
				br(nil, do(9, "mutation", "validate")),
			),
		)
		sameJSON(t, compiled(t, "automation", b), `[
			{"id":"role","type":"expression","binds":"role","expression":"args.role"},
			{"id":"queue","type":"function","condition":"role == \"owner\"","function":{"name":"queue","kind":"mutation"}},
			{"id":"review","type":"function","condition":"!(role == \"owner\") && role == \"admin\"","function":{"name":"review","kind":"mutation"}},
			{"id":"notify","type":"function","condition":"!(role == \"owner\") && role == \"admin\"","function":{"name":"notify","kind":"builtin"}},
			{"id":"validate","type":"function","condition":"!(role == \"owner\") && !(role == \"admin\")","function":{"name":"validate","kind":"mutation"}}]`)
	})
	t.Run("a switch with a two-label case and a default written first", func(t *testing.T) {
		b := body(
			set(2, "s", fld(rd("args", 2), "status")),
			&ast.SwitchStatement{Subject: rd("s", 3), Span: sp(3, 3), Cases: []ast.CaseArm{
				{Default: true, Body: []ast.BodyStatement{do(4, "builtin", "other")}},
				{Labels: []ast.ExpressionNode{lit("a"), lit("b")}, Body: []ast.BodyStatement{do(6, "builtin", "early")}},
				{Labels: []ast.ExpressionNode{lit("c")}, Body: []ast.BodyStatement{do(8, "builtin", "late")}},
			}},
		)
		sameJSON(t, compiled(t, "automation", b), `[
			{"id":"s","type":"expression","binds":"s","expression":"args.status"},
			{"id":"other","type":"function","condition":"!(s == \"a\" || s == \"b\") && !(s == \"c\")","function":{"name":"other","kind":"builtin"}},
			{"id":"early","type":"function","condition":"s == \"a\" || s == \"b\"","function":{"name":"early","kind":"builtin"}},
			{"id":"late","type":"function","condition":"s == \"c\"","function":{"name":"late","kind":"builtin"}}]`)
	})
	t.Run("a switch subject that binds looser than == is parenthesised", func(t *testing.T) {
		b := body(sw(2, &ast.TernaryExpr{Condition: fld(rd("args", 2), "x"), Then: lit("a"), Else: lit("b")},
			arm("a", do(3, "builtin", "one"))))
		sameJSON(t, compiled(t, "automation", b), `[
			{"id":"one","type":"function","condition":"(args.x ? \"a\" : \"b\") == \"a\"","function":{"name":"one","kind":"builtin"}}]`)
	})
	t.Run("an if inside a for: the loop's do carries the inner condition, the loop only its own", func(t *testing.T) {
		b := body(
			setCall(2, "rows", "query", "q"),
			ifs(3, br(fld(rd("args", 3), "enabled"),
				loop(4, "r", rd("rows", 4), nil,
					ifs(5, br(fld(rd("r", 5), "stale"), do(6, "mutation", "refresh", named("id", fld(rd("r", 6), "id"))))),
				),
			)),
		)
		sameJSON(t, compiled(t, "automation", b), `[
			{"id":"rows","type":"function","binds":"rows","function":{"name":"q","kind":"query"}},
			{"id":"for_r","type":"forEach","condition":"args.enabled","forEach":{"source":"rows","as":"r","do":[
				{"id":"refresh","type":"function","condition":"r.stale","function":{"name":"refresh","kind":"mutation","args":{"id":{"$expr":"r.id"}}}}]}}]`)
	})
	t.Run("nested ifs conjoin their conditions", func(t *testing.T) {
		b := body(ifs(2, br(fld(rd("args", 2), "a"),
			ifs(3, br(fld(rd("args", 3), "b"), do(4, "builtin", "both")), br(nil, do(6, "builtin", "onlyA"))),
		)))
		sameJSON(t, compiled(t, "automation", b), `[
			{"id":"both","type":"function","condition":"args.a && args.b","function":{"name":"both","kind":"builtin"}},
			{"id":"onlyA","type":"function","condition":"args.a && !args.b","function":{"name":"onlyA","kind":"builtin"}}]`)
	})
	t.Run("a condition joined by || keeps its meaning under &&", func(t *testing.T) {
		b := body(ifs(2, br(bin("||", fld(rd("args", 2), "a"), fld(rd("args", 2), "b")),
			ifs(3, br(fld(rd("args", 3), "c"), do(4, "builtin", "x"))))))
		sameJSON(t, compiled(t, "automation", b), `[
			{"id":"x","type":"function","condition":"(args.a || args.b) && args.c","function":{"name":"x","kind":"builtin"}}]`)
	})
}

func TestCompileBodyStepIDs(t *testing.T) {
	t.Run("two unnamed calls to one callee", func(t *testing.T) {
		b := body(do(2, "mutation", "createArtifact"), do(3, "mutation", "createArtifact"))
		got := compiled(t, "automation", b)
		if !strings.Contains(got, `"id":"createArtifact"`) || !strings.Contains(got, `"id":"createArtifact#2"`) {
			t.Fatalf("ids: %s", got)
		}
	})
	t.Run("a named statement claims its name before an earlier unnamed call to that callee", func(t *testing.T) {
		b := body(do(2, "builtin", "quote"), setCall(3, "quote", "builtin", "quote"))
		sameJSON(t, compiled(t, "automation", b), `[
			{"id":"quote#2","type":"function","function":{"name":"quote","kind":"builtin"}},
			{"id":"quote","type":"function","binds":"quote","function":{"name":"quote","kind":"builtin"}}]`)
	})
	t.Run("a rebinding in else is numbered and binds the same name", func(t *testing.T) {
		b := body(
			ifs(2,
				br(fld(rd("args", 2), "fast"), setCall(3, "quote", "builtin", "quickQuote")),
				br(nil, setCall(5, "quote", "builtin", "fullQuote")),
			),
			ret(7, rd("quote", 7)),
		)
		sameJSON(t, compiled(t, "automation", b), `[
			{"id":"quote","type":"function","binds":"quote","condition":"args.fast","function":{"name":"quickQuote","kind":"builtin"}},
			{"id":"quote#2","type":"function","binds":"quote","condition":"!args.fast","function":{"name":"fullQuote","kind":"builtin"}},
			{"id":"return","type":"return","return":{"value":"quote"}}]`)
	})
	t.Run("ids are unique within a list, not across lists", func(t *testing.T) {
		b := body(
			setCall(2, "rows", "query", "q"),
			loop(3, "r", rd("rows", 3), nil, do(4, "builtin", "touch")),
			do(6, "builtin", "touch"),
		)
		got := compiled(t, "automation", b)
		if strings.Count(got, `"id":"touch"`) != 2 || strings.Contains(got, "touch#2") {
			t.Fatalf("a loop's list numbers its own ids: %s", got)
		}
	})
}

// TestCompileBodyKeepsSourceOrder is the reason this compiler exists. b reads
// a; c reads nothing. The legacy compile's Kahn queue released c with a, so it
// ran a, c, b. A statement body runs as written.
func TestCompileBodyKeepsSourceOrder(t *testing.T) {
	steps, ps := CompileBody("automation", "probe", nil, body(
		setCall(2, "a", "builtin", "one"),
		setCall(3, "b", "builtin", "two", named("v", meth(rd("a", 3), "first"))),
		setCall(4, "c", "builtin", "three"),
	))
	if len(ps) > 0 {
		t.Fatal(ps)
	}
	var ids []string
	for _, s := range steps {
		ids = append(ids, s["id"].(string))
	}
	if strings.Join(ids, ",") != "a,b,c" {
		t.Fatalf("order = %v, want the source order a,b,c", ids)
	}
}

func TestCompileBodyRefusesABodyWithProblems(t *testing.T) {
	steps, ps := CompileBody("automation", "probe", nil, body(set(2, "a", rd("b", 2)), set(3, "b", lit(1))))
	if steps != nil {
		t.Fatalf("a refused body compiled to %v", steps)
	}
	if len(ps) != 1 || ps[0].Code != codeBodyForwardReference {
		t.Fatalf("problems = %v, want the forward reference", ps)
	}
}

// TestCompileSourceTakesAStatementBody runs the whole pipeline a loader runs --
// the rewriter, the parser, the compiler -- over an automation in statement
// form: its steps come from CompileBody, in source order, and it is marked as
// edition-2026 expressions.
func TestCompileSourceTakesAStatementBody(t *testing.T) {
	res, err := CompileSource(`@trigger(event="node.created")
automation routeRequest {
  args {
    id any
  }
  decide := logic routeStatus(id: args.id)
  if decide == "queued" {
    mutation advance(id: args.id)
  }
}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Automations) != 1 {
		t.Fatalf("automations = %d", len(res.Automations))
	}
	out := res.Automations[0].JSON
	if out["body"] != "statements" {
		t.Errorf("body = %v, want statements", out["body"])
	}
	if _, ok := out["_return"]; ok {
		t.Errorf("a statement body writes no _return: its return is a step")
	}
	b, _ := json.Marshal(out["steps"])
	sameJSON(t, string(b), `[
		{"id":"decide","type":"function","binds":"decide","function":{"name":"routeStatus","kind":"logic","args":{"id":{"$expr":"args.id"}}}},
		{"id":"advance","type":"function","condition":"decide == \"queued\"","function":{"name":"advance","kind":"mutation","args":{"id":{"$expr":"args.id"}}}}]`)
	trig, _ := out["trigger"].(map[string]any)
	if trig["event"] != "node.created" {
		t.Errorf("trigger = %v", out["trigger"])
	}
}

func TestCompileSourceRefusesAStatementBodyWithProblems(t *testing.T) {
	_, err := CompileSource(`automation early {
  x := y
  y := 1
}`)
	if err == nil {
		t.Fatal("a body reading a later name compiled")
	}
	for _, want := range []string{"automation early, line 2:8", "move line 3 above line 2", "[body_forward_reference]"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

// TestCompileBodyCoversEveryStatementKind lowers one statement of every kind
// and requires a step type other than the kind-name fallback, so a statement
// kind added without teaching the compiler fails here.
func TestCompileBodyCoversEveryStatementKind(t *testing.T) {
	seen := map[string]bool{}
	for name, s := range map[string]ast.BodyStatement{
		"assign":   set(2, "a", lit(1)),
		"call":     do(2, "builtin", "b"),
		"for":      loop(2, "i", fld(rd("args", 2), "list"), nil, do(3, "builtin", "b")),
		"parallel": par(2, pbr(3, "x", do(4, "builtin", "b"))),
		"publish":  pub(2, "t"),
		"return":   ret(2, nil),
		"if":       ifs(2, br(fld(rd("args", 2), "c"), do(3, "builtin", "b"))),
		"switch":   sw(2, fld(rd("args", 2), "s"), arm("a", do(3, "builtin", "b"))),
	} {
		steps, ps := CompileBody("automation", "probe", nil, body(s))
		if len(ps) > 0 || len(steps) != 1 {
			t.Fatalf("%s: steps %v, problems %v", name, steps, ps)
		}
		switch typ := steps[0]["type"]; typ {
		case "function", "expression", "forEach", "parallel", "event", "return", "automation", "action", "block":
		default:
			t.Errorf("%s lowered to step type %v: the compiler does not know this statement kind", name, typ)
		}
		seen[name] = true
	}
	for _, k := range ast.BodyStatementKinds() {
		if !seen[k] {
			t.Errorf("statement kind %q has no case here", k)
		}
	}
}
