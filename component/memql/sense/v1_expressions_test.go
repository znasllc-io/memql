package sense

import (
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/functions"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/language/tiers"
)

// v1_expressions_test.go covers Sense over the v1 expression language
// (memql#5365): which tier-manifest position the cursor is in, completion that
// offers exactly what that position admits, hover cards built from the function
// catalog and the operator table, and signature help from the catalog.
//
// The positions are detected from source TEXT: an editor's buffer is mid-edit
// and unparseable most of the time. So every case below is a real buffer
// shape, cursor at the end unless a marker or a column says otherwise.

// endOf returns the 1-based line and column just past the end of src.
func endOf(src string) (int, int) {
	lines := strings.Split(src, "\n")
	return len(lines), len(lines[len(lines)-1]) + 1
}

// v1Registry models the vocabulary the cases read: one concept with typed
// fields (including a list field for method completion), a second concept a
// predicate can be bound to, and one predicate of each kind -- a trait, a row
// spec bound to the query's concept, a row spec bound to ANOTHER concept, and
// a context spec over the actor envelope.
func v1Registry() *stubRegistry {
	return &stubRegistry{
		concepts: map[string]*ConceptInfo{
			"v1:todos:todo": {
				Name:        "v1:todos:todo",
				Description: "A todo item.",
				Fields: []FieldInfo{
					{Name: "title", Type: "string", Description: "What to do."},
					{Name: "status", Type: "string"},
					{Name: "tags", Type: "array"},
					{Name: "email", Type: "string"},
					{Name: "done", Type: "boolean"},
				},
			},
			"v1:crm:lead": {Name: "v1:crm:lead", Fields: []FieldInfo{{Name: "score", Type: "number"}}},
		},
		specs: map[string]*SpecInfo{
			"isActiveRecord": {Name: "isActiveRecord", Kind: "row", Trait: true},
			"isOverdue":      {Name: "isOverdue", Kind: "row", Bound: "todo"},
			"isHotLead":      {Name: "isHotLead", Kind: "row", Bound: "lead"},
			"requiresOwner":  {Name: "requiresOwner", Kind: "context", Bound: "actorEnvelope"},
		},
	}
}

const v1Query = "use todos.concepts.{ todo }\n\nquery todo openTodos {\n  args {\n    owner string\n    tag   string\n  }\n"

func TestFilterClauseDetectedOnOneLine(t *testing.T) {
	cases := []struct {
		name   string
		src    string
		pos    tiers.Position
		param  string
		lambda bool
	}{
		{"the v1 lambda form", v1Query + "  filter row => row.status == args.owner && ", tiers.PositionQueryFilter, "row", true},
		{"a parenthesised parameter", v1Query + "  filter (row) => row.status == ", tiers.PositionQueryFilter, "row", true},
		{"any parameter name", v1Query + "  filter t => t.status == ", tiers.PositionQueryFilter, "t", true},
		// 0 of the tree's 522 filters is a `filter {` block: the braceless clause
		// is the one every real query uses, and it was invisible to Sense.
		{"the pre-v1 braceless clause", v1Query + "  filter status == args.owner && ", tiers.PositionQueryFilter, "", false},
		{"a continuation line", v1Query + "  filter row => row.status == args.owner\n    && row.done == ", tiers.PositionQueryFilter, "row", true},
		{"the legacy block", v1Query + "  filter {\n    ", tiers.PositionQueryFilter, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			line, col := endOf(c.src)
			ctx := analyzeCursorContext(c.src, line, col)
			if ctx.Position != c.pos {
				t.Fatalf("Position = %q, want %q", ctx.Position, c.pos)
			}
			if ctx.Param != c.param {
				t.Errorf("Param = %q, want %q", ctx.Param, c.param)
			}
			if ctx.Lambda != c.lambda {
				t.Errorf("Lambda = %v, want %v", ctx.Lambda, c.lambda)
			}
			if ctx.Bound != "todo" {
				t.Errorf("Bound = %q, want the query's concept todo", ctx.Bound)
			}
		})
	}

	// A clause that is not the filter is not the filter's position.
	for src, want := range map[string]tiers.Position{
		v1Query + "  filter row => row.done == true\n  sort \"row.cre": tiers.PositionSort,
		v1Query + "  filter row => row.done == true\n  shape todoFull": "",
		v1Query + "  filter row => row.done == true\n\n  paginate 5":   "",
		v1Query + "    tag2 ": "",
	} {
		line, col := endOf(src)
		if got := analyzeCursorContext(src, line, col).Position; got != want {
			t.Errorf("Position at the end of %q = %q, want %q", src[len(v1Query):], got, want)
		}
	}
}

// Every position the tier manifest names is detected from its real spelling.
func TestExpressionPositionsAreDetected(t *testing.T) {
	cases := []struct {
		name  string
		src   string
		pos   tiers.Position
		param string
		bound string
	}{
		{"spec over a concept", "use todos.concepts.{ todo }\n\nspec todo isOverdue = row => row.", tiers.PositionSpecBody, "row", "todo"},
		{"spec over an actor shape", "spec actorEnvelope requiresOwner = actor => actor.", tiers.PositionSpecBody, "actor", "actorEnvelope"},
		{"trait", "trait isOpen = row => row.status == ", tiers.PositionSpecBody, "row", ""},
		{"pre-v1 spec body", "spec todo isOverdue {\n  return ", tiers.PositionSpecBody, "", "todo"},
		{"trigger filter", "@trigger(event=\"graph.node.updated.v1:todos:todo\")\n@filter(row => row.", tiers.PositionTriggerFilter, "row", "todo"},
		{"trigger filter over a concept kwarg", "@trigger(event=\"node.created\", concept=\"v1:todos:todo\")\n@filter(row => row.done == ", tiers.PositionTriggerFilter, "row", "todo"},
		{"row-authz argument", "@rowAuthz(owner=\"", tiers.PositionRowAuthzArgument, "", ""},
		{"automation if", "automation sweep {\n  step decide {\n    if args.windowDays > ", tiers.PositionAutomationCondition, "", ""},
		{"automation forEach where", "automation sweep {\n  forEach item in rows where item.done == ", tiers.PositionAutomationCondition, "", ""},
		{"automation switch", "automation sweep {\n  switch ", tiers.PositionAutomationCondition, "", ""},
		{"automation precondition", "automation sweep {\n  precondition ok {\n    check: ", tiers.PositionAutomationCondition, "", ""},
		{"step argument", "automation sweep {\n  step record {\n    mutation createTodo(title: ", tiers.PositionStepArgument, "", ""},
		{"logic body", "logic compute {\n  body {\n    x := ", tiers.PositionLogicBody, "", ""},
		{"logic return", "logic compute {\n  body {\n    return ", tiers.PositionLogicBody, "", ""},
		{"mutation value", "mutate todo createTodo {\n  insert {\n    title: ", tiers.PositionMutationValue, "", ""},
		{"stamp value", "mutate todo createTodo {\n  insert {\n    accept { title }\n    stamp {\n      createdBy: ", tiers.PositionMutationValue, "", ""},
		{"tool default", "tool searchTodos {\n  limit integer @default(\"", tiers.PositionToolDefault, "", ""},
		{"tool handler query", "@handler(type=\"query\", query=\"concept==v1:todos:todo && ", tiers.PositionQueryFilter, "", ""},
		{"prompt default", "prompt summarize {\n  style string @default(\"", tiers.PositionPromptInput, "", ""},
		{"refine clause", v1Query + "  filter row => row.done == false\n  paginate 20\n  refine row => row.", tiers.PositionQueryRefine, "row", "todo"},
		// Not an expression position at all.
		{"a mutation field name", "mutate todo createTodo {\n  insert {\n    tit", "", "", ""},
		{"an args block", "logic compute {\n  args {\n    x ", "", "", ""},
		{"a concept body", "concept todo {\n  title ", "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			line, col := endOf(c.src)
			ctx := analyzeCursorContext(c.src, line, col)
			if ctx.Position != c.pos {
				t.Fatalf("Position = %q, want %q", ctx.Position, c.pos)
			}
			if ctx.Param != c.param {
				t.Errorf("Param = %q, want %q", ctx.Param, c.param)
			}
			if ctx.Bound != c.bound {
				t.Errorf("Bound = %q, want %q", ctx.Bound, c.bound)
			}
		})
	}
}

// The refine clause is a query's one in-process position: a lambda over each
// row of the page `paginate` read, whose parameter reads the query's concept.
func TestRefineClauseIsDetected(t *testing.T) {
	head := v1Query + "  filter row => row.done == false\n  paginate 20\n"
	cases := []struct {
		name   string
		src    string
		param  string
		lambda bool
		params string
	}{
		{"the lambda form", head + "  refine row => row.title.includes(args.tag) && ", "row", true, "row<>"},
		{"any parameter name", head + "  refine r => r.", "r", true, "r<>"},
		{"a parenthesised parameter", head + "  refine (row) => row.", "row", true, "row<>"},
		{"a nested lambda", head + "  refine row => row.tags.any(g => g.", "row", true, "row<>,g<row.tags.any>"},
		{"a continuation line", head + "  refine row => row.title != nil\n    && row.", "row", true, "row<>"},
		{"no header yet", head + "  refine ", "", false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			line, col := endOf(c.src)
			ctx := analyzeCursorContext(c.src, line, col)
			if ctx.Position != tiers.PositionQueryRefine {
				t.Fatalf("Position = %q, want %q", ctx.Position, tiers.PositionQueryRefine)
			}
			if ctx.Param != c.param || ctx.Lambda != c.lambda {
				t.Errorf("Param, Lambda = %q, %v; want %q, %v", ctx.Param, ctx.Lambda, c.param, c.lambda)
			}
			if ctx.Bound != "todo" {
				t.Errorf("Bound = %q, want the query's concept todo", ctx.Bound)
			}
			var names []string
			for _, p := range ctx.Params {
				names = append(names, p.Name+"<"+p.Callee+">")
			}
			if got := strings.Join(names, ","); got != c.params {
				t.Errorf("Params = %s, want %s", got, c.params)
			}
		})
	}

	// The clause after refine is not the refine clause, and neither is the
	// filter above it.
	for src, want := range map[string]tiers.Position{
		head + "  refine row => row.done == true\n  shape todoFull": "",
		head + "  refine row => row.done == true\n  sort \"row.cre": tiers.PositionSort,
		v1Query + "  filter row => row.":                            tiers.PositionQueryFilter,
	} {
		line, col := endOf(src)
		if got := analyzeCursorContext(src, line, col).Position; got != want {
			t.Errorf("Position at the end of %q = %q, want %q", src[len(v1Query):], got, want)
		}
	}
}

// A nested lambda puts its own parameter in scope for exactly as long as the
// cursor sits inside it.
func TestNestedLambdaParametersAreScoped(t *testing.T) {
	src := v1Query + "  filter row => row.tags.any(t => t == args.tag) && childOf(p => p."
	line, col := endOf(src)
	ctx := analyzeCursorContext(src, line, col)
	var names []string
	for _, p := range ctx.Params {
		names = append(names, p.Name+"<"+p.Callee+">")
	}
	if got := strings.Join(names, ","); got != "row<>,p<childOf>" {
		t.Errorf("Params = %s, want row<>,p<childOf> (t went out of scope when any( closed)", got)
	}

	src = "logic compute {\n  body {\n    x := args.members.where(m => m."
	line, col = endOf(src)
	ctx = analyzeCursorContext(src, line, col)
	if len(ctx.Params) != 1 || ctx.Params[0].Name != "m" || ctx.Params[0].Callee != "args.members.where" {
		t.Errorf("Params = %+v, want the one parameter m of args.members.where", ctx.Params)
	}
}

// completionItems runs completion at the end of src and indexes the items by
// label.
func completionItems(t *testing.T, s *Service, src string) map[string]CompletionItem {
	t.Helper()
	line, col := endOf(src)
	out := map[string]CompletionItem{}
	for _, it := range s.Complete(src, line, col, "dsl/todos/queries.memql") {
		out[it.Label] = it
	}
	return out
}

func TestCompletionOffersTheTierOfThePosition(t *testing.T) {
	s := New(v1Registry())

	t.Run("the parameter's members are the concept's fields and the row intrinsics", func(t *testing.T) {
		got := completionItems(t, s, v1Query+"  filter row => row.")
		for field, typ := range map[string]string{"title": "string", "status": "string", "tags": "array"} {
			it, ok := got[field]
			if !ok {
				t.Errorf("row. must offer the field %q, got %v", field, keys(got))
				continue
			}
			if it.Detail != typ {
				t.Errorf("field %s detail = %q, want its type %q", field, it.Detail, typ)
			}
		}
		for _, intrinsic := range []string{"id", "createdAt", "createdBy"} {
			if it, ok := got[intrinsic]; !ok || it.Detail != "row intrinsic" {
				t.Errorf("row. must offer the intrinsic %q with detail \"row intrinsic\", got %+v", intrinsic, it)
			}
		}
		for _, never := range []string{"lower", "childOf", "isActiveRecord(row)", "args"} {
			if _, ok := got[never]; ok {
				t.Errorf("a member position must offer only members, got %q", never)
			}
		}
	})

	t.Run("a pushdown position offers what it admits and nothing it refuses", func(t *testing.T) {
		got := completionItems(t, s, v1Query+"  filter row => ")
		for _, want := range []string{"row", "args", "actor", "now", "childOf", "ids", "isActiveRecord(row)", "isOverdue(row)", "requiresOwner(actor)"} {
			if _, ok := got[want]; !ok {
				t.Errorf("a query filter must offer %q, got %v", want, keys(got))
			}
		}
		// An in-process function is offered, and says it cannot read the row.
		for _, m := range []string{"lower", "addDuration"} {
			it, ok := got[m]
			if !ok {
				t.Errorf("a query filter must offer %s for values that do not read the row", m)
				continue
			}
			if !strings.Contains(it.Detail, "before the query") || !strings.Contains(it.Detail, "cannot read the row") {
				t.Errorf("%s detail = %q; it must say it computes before the query and cannot read the row", m, it.Detail)
			}
		}
		if it := got["childOf"]; strings.Contains(it.Detail, "cannot read the row") {
			t.Errorf("childOf pushes down; its detail must not carry the plan-constant warning: %q", it.Detail)
		}
		for _, never := range []string{
			"query", "mutation", "logic", // construct calls are refused in a filter
			"cond", "concat", "coalesce", "first", "last", // retired spellings
			"return", "if", "for", // statements
			"isHotLead(row)", // bound to a different concept
		} {
			if _, ok := got[never]; ok {
				t.Errorf("a query filter must not offer %q", never)
			}
		}
	})

	t.Run("methods on a row array are the ones that push down", func(t *testing.T) {
		got := completionItems(t, s, v1Query+"  filter row => row.tags.")
		for _, want := range []string{"any", "all", "count"} {
			if _, ok := got[want]; !ok {
				t.Errorf("row.tags. must offer %q, got %v", want, keys(got))
			}
		}
		// where() cannot read the row, and row.tags IS the row: offering it
		// here would offer a call the load must refuse.
		for _, never := range []string{"where", "select", "first"} {
			if _, ok := got[never]; ok {
				t.Errorf("row.tags. must not offer %q in a query filter", never)
			}
		}
	})

	t.Run("args members carry their declared type", func(t *testing.T) {
		got := completionItems(t, s, v1Query+"  filter row => row.status == args.")
		if it, ok := got["owner"]; !ok || it.Detail != "string" {
			t.Errorf("args. must offer owner with detail string, got %+v", it)
		}
	})

	t.Run("a list arg offers every method, since it does not read the row", func(t *testing.T) {
		src := "use todos.concepts.{ todo }\n\nquery todo tagged {\n  args {\n    tags []string\n  }\n  filter row => args.tags."
		got := completionItems(t, s, src)
		for _, want := range []string{"any", "count", "where", "first"} {
			it, ok := got[want]
			if !ok {
				t.Errorf("args.tags. must offer %q, got %v", want, keys(got))
				continue
			}
			// A plan constant: the in-process methods are legal on it, so the
			// detail is the signature, not the can't-read-the-row warning.
			if strings.Contains(it.Detail, "cannot read the row") {
				t.Errorf("%s on an arg is a plan constant; detail = %q", want, it.Detail)
			}
		}
	})

	t.Run("an actor spec's parameter is the actor envelope", func(t *testing.T) {
		got := completionItems(t, s, "spec actorEnvelope requiresOwner = actor => actor.")
		for _, want := range []string{"userId", "role", "isClusterOwner"} {
			if _, ok := got[want]; !ok {
				t.Errorf("actor. must offer %q, got %v", want, keys(got))
			}
		}
	})

	t.Run("a logic body is in-process and may call constructs", func(t *testing.T) {
		got := completionItems(t, s, "logic compute {\n  body {\n    return ")
		for _, want := range []string{"lower", "addDuration", "hash", "query", "args", "now"} {
			if _, ok := got[want]; !ok {
				t.Errorf("a logic body must offer %q, got %v", want, keys(got))
			}
		}
		if it := got["lower"]; strings.Contains(it.Detail, "cannot read the row") {
			t.Errorf("in process, lower carries no plan-constant warning: %q", it.Detail)
		}
	})

	t.Run("a logic body offers the names bound above the cursor", func(t *testing.T) {
		body := "logic compute {\n  body {\n    rows := query activeUsers()\n    total := rows.count()\n    return "
		got := completionItems(t, s, body)
		for _, want := range []string{"rows", "total"} {
			if it, ok := got[want]; !ok || it.Detail != "local" {
				t.Errorf("a logic body must offer the local %q, got %+v", want, it)
			}
		}
		// A name bound BELOW the cursor is not in scope yet.
		got = completionItems(t, s, "logic compute {\n  body {\n    return ")
		if _, ok := got["rows"]; ok {
			t.Error("no local is bound above the cursor here")
		}
		// A query's result is a list of rows: its members are the list methods.
		got = completionItems(t, s, "logic compute {\n  body {\n    rows := query activeUsers()\n    return rows.")
		for _, want := range []string{"count", "first", "nodes", "where"} {
			if _, ok := got[want]; !ok {
				t.Errorf("a query result must offer the list method %q, got %v", want, keys(got))
			}
		}
	})

	t.Run("an automation condition may not call a construct", func(t *testing.T) {
		got := completionItems(t, s, "automation sweep {\n  step decide {\n    if args.windowDays > 1 && lo")
		if _, ok := got["lower"]; !ok {
			t.Errorf("a condition must offer lower, got %v", keys(got))
		}
		if _, ok := got["logic"]; ok {
			t.Error("a condition must not offer the construct call `logic`: the manifest refuses constructCall there")
		}
	})

	t.Run("a trigger filter reads the triggering row", func(t *testing.T) {
		got := completionItems(t, s, "@trigger(event=\"graph.node.updated.v1:todos:todo\")\n@filter(row => ")
		for _, want := range []string{"row", "isActiveRecord(row)", "lower"} {
			if _, ok := got[want]; !ok {
				t.Errorf("a trigger filter must offer %q, got %v", want, keys(got))
			}
		}
		got = completionItems(t, s, "@trigger(event=\"graph.node.updated.v1:todos:todo\")\n@filter(row => row.")
		if _, ok := got["status"]; !ok {
			t.Errorf("row. in a trigger filter must offer the triggering concept's fields, got %v", keys(got))
		}
	})

	t.Run("a trigger filter reads the args its automation binds from the payload", func(t *testing.T) {
		got := completionItems(t, s, "@trigger(event=\"graph.node.updated.v1:todos:todo\")\n@filter(row => row.status == ")
		it, ok := got["args"]
		if !ok {
			t.Fatalf("a trigger filter must offer args, got %v", keys(got))
		}
		if !strings.Contains(it.Documentation, "payload") {
			t.Errorf("args in a trigger filter is bound from the triggering payload, not passed by a caller: %q", it.Documentation)
		}
		// The members are the args of the automation the annotation decorates,
		// which is declared BELOW the cursor.
		automation := "\nautomation onTodoChanged {\n  args {\n    status string\n    tags   []string\n  }\n  step s {\n    logic record(status: args.status)\n  }\n}\n"
		got = completionAt(t, s, "@trigger(event=\"graph.node.updated.v1:todos:todo\")\n@filter(row => row.status == args.<|>)"+automation)
		for field, typ := range map[string]string{"status": "string", "tags": "[]string"} {
			if it, ok := got[field]; !ok || it.Detail != typ {
				t.Errorf("args. in a trigger filter must offer the automation's %s with detail %q, got %+v (all: %v)", field, typ, it, keys(got))
			}
		}
		got = completionAt(t, s, "@trigger(event=\"graph.node.updated.v1:todos:todo\")\n@filter(row => args.tags.<|>)"+automation)
		if _, ok := got["any"]; !ok {
			t.Errorf("a list arg in a trigger filter offers the list methods, got %v", keys(got))
		}
	})

	t.Run("a refine clause runs in process over the page", func(t *testing.T) {
		head := v1Query + "  filter row => row.done == false\n  paginate 20\n"
		got := completionItems(t, s, head+"  refine row => ")
		for _, want := range []string{"row", "args", "now", "lower", "addDuration", "isActiveRecord(row)", "isOverdue(row)"} {
			if _, ok := got[want]; !ok {
				t.Errorf("a refine clause must offer %q, got %v", want, keys(got))
			}
		}
		// In process, an in-process function reads the row directly.
		if it := got["lower"]; strings.Contains(it.Detail, "cannot read the row") {
			t.Errorf("in a refine clause lower may read the row; detail = %q", it.Detail)
		}
		for _, never := range []string{"query", "mutation", "logic", "isHotLead(row)", "return"} {
			if _, ok := got[never]; ok {
				t.Errorf("a refine clause must not offer %q", never)
			}
		}
		got = completionItems(t, s, head+"  refine row => row.")
		if _, ok := got["title"]; !ok {
			t.Errorf("row. in a refine clause must offer the query's concept's fields, got %v", keys(got))
		}
		// A row array offers every list method: where() runs in process here.
		got = completionItems(t, s, head+"  refine row => row.tags.")
		for _, want := range []string{"any", "where", "first"} {
			if _, ok := got[want]; !ok {
				t.Errorf("row.tags. in a refine clause must offer %q, got %v", want, keys(got))
			}
		}
		// Before its lambda header, the header is what the clause needs.
		got = completionItems(t, s, head+"  refine ")
		if it, ok := got["row => ..."]; !ok || !it.IsSnippet {
			t.Errorf("a refine clause with no header must offer the lambda header snippet, got %v", keys(got))
		}
		if _, ok := got["lower"]; ok {
			t.Error("before its header a refine clause offers the header, not the vocabulary of its body")
		}
	})

	t.Run("a predicate inserts its application", func(t *testing.T) {
		got := completionItems(t, s, v1Query+"  filter row => ")
		if it := got["isActiveRecord(row)"]; it.InsertText != "isActiveRecord(row)" {
			t.Errorf("a trait applies to the row: insert = %q", it.InsertText)
		}
		if it := got["requiresOwner(actor)"]; it.InsertText != "requiresOwner(actor)" {
			t.Errorf("an actor spec applies to actor: insert = %q", it.InsertText)
		}
	})
}

// completionAt runs completion at the <|> marker in src -- for a cursor with
// source below it -- and indexes the items by label.
func completionAt(t *testing.T, s *Service, src string) map[string]CompletionItem {
	t.Helper()
	before, after, ok := strings.Cut(src, "<|>")
	if !ok {
		t.Fatalf("fixture %q has no <|> cursor marker", src)
	}
	line, col := endOf(before)
	out := map[string]CompletionItem{}
	for _, it := range s.Complete(before+after, line, col, "dsl/todos/automations.memql") {
		out[it.Label] = it
	}
	return out
}

func keys(m map[string]CompletionItem) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// hoverOn returns the hover contents with the cursor on the first occurrence of
// needle in the LAST line of src (one column into it).
func hoverOn(t *testing.T, s *Service, src, needle string) string {
	t.Helper()
	lines := strings.Split(src, "\n")
	last := lines[len(lines)-1]
	idx := strings.Index(last, needle)
	if idx < 0 {
		t.Fatalf("fixture line %q does not contain %q", last, needle)
	}
	res := s.Hover(src, len(lines), idx+2, "dsl/todos/queries.memql")
	if res == nil {
		return ""
	}
	return res.Contents
}

// hoverFirst returns the hover contents with the cursor on the first
// occurrence of needle anywhere in src (one column into it), for a construct
// that has to be complete to be read.
func hoverFirst(t *testing.T, s *Service, src, needle string) string {
	t.Helper()
	at := strings.Index(src, needle)
	if at < 0 {
		t.Fatalf("fixture %q does not contain %q", src, needle)
	}
	line := strings.Count(src[:at], "\n") + 1
	col := at - (strings.LastIndex(src[:at], "\n") + 1) + 2
	res := s.Hover(src, line, col, "dsl/todos/specs.memql")
	if res == nil {
		return ""
	}
	return res.Contents
}

// hoverStyle holds a card to the design direction: plain text, the signature
// first, sentence case -- no bold labels, no decorative separators, no
// middle-dot meta strings.
func hoverStyle(t *testing.T, card string) {
	t.Helper()
	if !strings.HasPrefix(card, "```memql\n") {
		t.Errorf("a card opens with the memql code block, got:\n%s", card)
	}
	for _, banned := range []string{"**", "---", "·", "Tier:", "Signature:", "Note:"} {
		if strings.Contains(card, banned) {
			t.Errorf("card contains %q, which the design direction rules out:\n%s", banned, card)
		}
	}
}

func TestHoverShowsSignatureTierAndLegality(t *testing.T) {
	s := New(v1Registry())
	lower, _ := functions.Lookup("lower")

	card := hoverOn(t, s, v1Query+"  filter row => lower(row.email) == \"x\"", "lower")
	hoverStyle(t, card)
	for _, want := range []string{
		"```memql\n" + lower.Signature() + "\n```",
		"Returns value with its letters lowercased.",
		"Runs in process before the query, on values that do not read the row.",
		"Not allowed on the row in a query filter: `lower` runs in process.",
		"`row.email == lower(args.email)`",
	} {
		if !strings.Contains(card, want) {
			t.Errorf("hover on lower in a filter should contain %q, got:\n%s", want, card)
		}
	}

	// The same function on a value that does not read the row is a plan
	// constant: legal, so the card says where it runs and raises no alarm.
	card = hoverOn(t, s, v1Query+"  filter row => row.email == lower(args.owner)", "lower")
	if !strings.Contains(card, "Runs in process before the query, on values that do not read the row.") {
		t.Errorf("a plan-constant lower still runs before the query, got:\n%s", card)
	}
	if strings.Contains(card, "Not allowed") {
		t.Errorf("lower(args.owner) does not read the row, so there is nothing to refuse, got:\n%s", card)
	}
	// And an operator likewise: `??` over args is folded before the query.
	if card := hoverOn(t, s, v1Query+"  filter row => row.status == (args.owner ?? \"x\")", "??"); strings.Contains(card, "Not allowed") {
		t.Errorf("args.owner ?? \"x\" does not read the row, got:\n%s", card)
	}

	card = hoverOn(t, s, "logic compute {\n  body {\n    return lower(args.x)", "lower")
	hoverStyle(t, card)
	if !strings.Contains(card, "Runs in process.") {
		t.Errorf("in a logic body lower runs in process, got:\n%s", card)
	}
	if strings.Contains(card, "Not allowed") {
		t.Errorf("lower is admitted in a logic body, so there is no legality line, got:\n%s", card)
	}

	card = hoverOn(t, s, v1Query+"  filter row => childOf(p => p.id == args.owner)", "childOf")
	hoverStyle(t, card)
	if !strings.Contains(card, "Pushed down to SQL.") || strings.Contains(card, "Not allowed") {
		t.Errorf("childOf pushes down in a filter, with no legality line, got:\n%s", card)
	}

	card = hoverOn(t, s, v1Query+"  filter row => row.title.includes(args.tag)", "includes")
	hoverStyle(t, card)
	for _, want := range []string{"string.includes(sub string) bool", "Pushed down to SQL.", "Replaces `contains(s, sub)`."} {
		if !strings.Contains(card, want) {
			t.Errorf("hover on includes should contain %q, got:\n%s", want, card)
		}
	}

	card = hoverOn(t, s, v1Query+"  filter row => row.tags.count() > 2", "count")
	if !strings.Contains(card, "Replaces `len(x)` and `count(x)`.") {
		t.Errorf("hover on list.count should name both spellings it replaces, got:\n%s", card)
	}

	// A method that cannot read the row names the pushdown methods as the fix.
	card = hoverOn(t, s, v1Query+"  filter row => row.tags.where(t => t == \"x\").count() > 0", "where")
	if !strings.Contains(card, "Not allowed on the row in a query filter") || !strings.Contains(card, "`row.tags.any(") {
		t.Errorf("hover on where over a row array should point at any/all/count, got:\n%s", card)
	}

	// Every position has a phrase for the legality line to name it by.
	for _, p := range tiers.Positions() {
		if positionPhrase(p) == "" {
			t.Errorf("position %s has no phrase for the legality line", p)
		}
	}
}

func TestHoverOnOperators(t *testing.T) {
	s := New(v1Registry())
	filter := v1Query + "  filter row => row.?lineage.planId == args.owner && !(row.status in [\"a\"]) || row.status != \"\" || row.title startsWith args.tag"
	logicLine := "logic compute {\n  body {\n    x := args.a ?? (args.flag ? \"y\" : \"n\")"

	cases := []struct {
		src, needle string
		want        []string
	}{
		{filter, "==", []string{"```memql\na == b\n```", "equal", "`\"\"`"}},
		{filter, "!=", []string{"```memql\na != b\n```", "`row.f != \"\"` are false"}},
		{filter, "&&", []string{"```memql\na && b\n```", "both sides are true"}},
		{filter, "||", []string{"```memql\na || b\n```", "either side is true"}},
		{filter, "!(", []string{"```memql\n!(x in list)\n```", "`!(x == v)` is exactly `x != v`"}},
		{filter, " in ", []string{"```memql\nv in list\n```", "only when the list holds `\"\"` or `nil`"}},
		{filter, "startsWith", []string{"```memql\ns startsWith p\n```", "prefix"}},
		{filter, ".?", []string{"```memql\nrow.?lineage.planId\n```", "absent"}},
		{filter, "=>", []string{"```memql\nrow => row.status == \"open\"\n```", "parameter"}},
		{logicLine, "??", []string{"```memql\na ?? b\n```", "blank"}},
		{logicLine, "? \"", []string{"```memql\np ? a : b\n```", "boolean"}},
		{logicLine, ": \"", []string{"```memql\np ? a : b\n```"}},
	}
	for _, c := range cases {
		card := hoverOn(t, s, c.src, c.needle)
		if card == "" {
			t.Errorf("no hover on %q", c.needle)
			continue
		}
		hoverStyle(t, card)
		for _, want := range c.want {
			if !strings.Contains(card, want) {
				t.Errorf("hover on %q should contain %q, got:\n%s", c.needle, want, card)
			}
		}
	}

	// `??` in a query filter cannot read the row: the card says so.
	card := hoverOn(t, s, v1Query+"  filter row => row.stage == (row.alias ?? \"x\")", "??")
	if !strings.Contains(card, "Not allowed on the row in a query filter") {
		t.Errorf("?? in a filter is plan-constant only; its card should say so, got:\n%s", card)
	}

	// The same spellings that are NOT these operators keep their own meaning.
	if card := hoverOn(t, s, "automation sweep {\n  forEach item in rows {", " in "); strings.Contains(card, "v in list") {
		t.Errorf("forEach's `in` is the loop keyword, not membership, got:\n%s", card)
	}
	if card := hoverOn(t, s, "@trigger(event=\"x.y\")\nautomation sweep @trigger(event=\"x.y\") => logic handle", "=>"); strings.Contains(card, "parameter") {
		t.Errorf("the terse automation arrow is not a lambda, got:\n%s", card)
	}
	if card := hoverOn(t, s, "logic compute {\n  body {\n    x := {a: 1}", ": 1"); strings.Contains(card, "p ? a : b") {
		t.Errorf("a map key colon is not the ternary, got:\n%s", card)
	}
}

func TestHoverOnARetiredSpelling(t *testing.T) {
	s := New(v1Registry())
	logic := func(expr string) string { return "logic compute {\n  body {\n    return " + expr }

	for _, c := range []struct {
		src, needle, replacement string
	}{
		{logic("cond(args.a, 1, 2)"), "cond", "p ? a : b"},
		{logic("concat(\"a\", args.b)"), "concat", "a + b"},
		{logic("coalesce(args.a, 1)"), "coalesce", "a ?? b"},
		{logic("exists(args.a)"), "exists", "x != nil"},
		{logic("len(args.a)"), "len", "x.count()"},
		{logic("mean(args.a)"), "mean", "x.avg()"},
		{logic("contains(args.s, \"x\")"), "contains", "s.includes(sub)"},
		{logic("args.xs.contains(args.v)"), "contains", "v in <list>"},
		{v1Query + "  filter when(args.owner) { status == args.owner }", "when", "args.x == nil"},
	} {
		card := hoverOn(t, s, c.src, c.needle)
		if card == "" {
			t.Errorf("no hover on the retired %s", c.needle)
			continue
		}
		hoverStyle(t, card)
		for _, want := range []string{c.replacement, "retired", "memqlmigrate --rewrite=expressions"} {
			if !strings.Contains(card, want) {
				t.Errorf("hover on retired %s should contain %q, got:\n%s", c.needle, want, card)
			}
		}
	}

	// contains with ONE argument is the live traversal, not the retired form.
	card := hoverOn(t, s, v1Query+"  filter row => contains(p => p.id == args.owner)", "contains")
	if strings.Contains(card, "retired") || !strings.Contains(card, "contains(label? string, match lambda) rows") {
		t.Errorf("one-argument contains is the traversal, got:\n%s", card)
	}
	// A bare `count` line is the query's count clause, not the retired count().
	if card := hoverOn(t, s, v1Query+"  count", "count"); strings.Contains(card, "retired") {
		t.Errorf("the count clause is not the retired count(x), got:\n%s", card)
	}
	// A declaration's own keyword is not the retired `spec <name>` reference.
	for src, needle := range map[string]string{
		"spec todo isOverdue = row => row.done == false": "spec",
		"trait isOpen = row => row.done == false":        "trait",
	} {
		if card := hoverOn(t, s, src, needle); strings.Contains(card, "retired") {
			t.Errorf("the %s keyword of a declaration is not a retired reference, got:\n%s", needle, card)
		}
	}
}

// Sense's card for a retired spelling IS the parser's refusal table: the
// spelling and its replacement are read from parser.V1RetiredForms, never
// restated, so the hover and the load refusal cannot say different things.
// Every form the parser refuses is either hovered here or named in unhoverable
// with the reason a token scan cannot find it.
func TestRetiredHoverIsTheParsersTable(t *testing.T) {
	s := New(v1Registry())
	logic := func(expr string) string { return "logic compute {\n  body {\n    return " + expr }
	filter := func(expr string) string { return v1Query + "  filter " + expr }
	type sample struct {
		src, needle string
		// example is the author's own construct as the rewrite writes it, for
		// the predicate positions, whose card shows it in place of the table's
		// placeholder form. Their needle is found anywhere in src.
		example string
	}

	samples := map[string]sample{
		"retired_when_guard":           {src: filter("when(args.owner) { status == args.owner }"), needle: "when"},
		"retired_conditional_prefix":   {src: filter("?.status == args.owner"), needle: "?."},
		"retired_semicolon_connective": {src: filter("status == args.owner; done == false"), needle: ";"},
		"retired_has":                  {src: filter("tags has args.tag"), needle: "has"},
		"retired_not_in":               {src: filter("status not in [\"a\", \"b\"]"), needle: "not"},
		"retired_null":                 {src: logic("args.a == null"), needle: "null"},
		"retired_dollar_args":          {src: filter("status == $args.owner"), needle: "$"},
		"retired_spec_reference":       {src: filter("done == false && spec isOverdue"), needle: "spec"},
		"retired_trait_reference":      {src: filter("trait isActiveRecord"), needle: "trait"},
		"retired_contains_method":      {src: logic("args.xs.contains(args.v)"), needle: "contains"},
		"retired_filter_without_lambda": {
			src: filter("status == args.owner && isActiveRecord\n}"), needle: "filter",
			example: "filter row => row.status == args.owner && isActiveRecord(row)",
		},
		"retired_spec_return_body": {
			src: "use todos.concepts.{ todo }\n\nspec todo isOverdue {\n  return done == false\n}", needle: "return",
			example: "spec todo isOverdue = row => row.done == false",
		},
		"retired_trait_return_body": {
			src: "trait isOpen {\n  return status == \"open\"\n}", needle: "return",
			example: "trait isOpen = row => row.status == \"open\"",
		},
		"retired_filter_annotation": {
			src:    "@trigger(event=\"graph.node.updated.v1:todos:todo\")\n@filter(payload.status == \"archived\")\nautomation onTodo {\n}",
			needle: "@filter", example: "@filter(row => row.status == \"archived\")",
		},
	}
	unhoverable := map[string]string{
		// A comma also separates arguments and list elements. The parser tells
		// a connective by where the expression ends, which a token scan cannot.
		"retired_comma_connective": "a comma is also a separator",
	}

	forms := parser.V1RetiredForms()
	checked := 0
	for _, form := range forms {
		if _, skip := unhoverable[form.Rule]; skip {
			continue
		}
		smp, ok := samples[form.Rule]
		if name, isCall := strings.CutSuffix(strings.TrimPrefix(form.Rule, "retired_"), "_call"); isCall && !ok {
			// Every retired call has one shape: its name, applied.
			smp, ok = sample{src: logic(name + "(args.a, args.b)"), needle: name}, true
		}
		if !ok {
			t.Errorf("the parser retires %s (%s) and Sense has no sample for it: hover it, or name it in unhoverable with the reason", form.Rule, form.Spelling)
			continue
		}
		card := ""
		if smp.example != "" {
			card = hoverFirst(t, s, smp.src, smp.needle)
		} else {
			card = hoverOn(t, s, smp.src, smp.needle)
		}
		if card == "" {
			t.Errorf("%s: no hover on %q in %q", form.Rule, smp.needle, smp.src)
			continue
		}
		checked++
		hoverStyle(t, card)
		spelling := "`" + form.Spelling + "`"
		if glyph, ok := strings.CutSuffix(form.Spelling, " as a connective"); ok {
			spelling = "`" + glyph + "` as a connective"
		}
		want := []string{spelling + " is retired in edition 2026.", "`" + migrator + "` rewrites it."}
		switch {
		case form.Rule == "retired_contains_method":
			// The one replacement the table writes as a choice.
			want = append(want, "```memql\nv in <list>\ns.includes(sub)\n```",
				"Write `v in <list>` for membership or `s.includes(sub)` for a substring;")
		case smp.example != "":
			want = append(want, "```memql\n"+smp.example+"\n```", "Write `"+form.Replacement+"` instead;")
		default:
			want = append(want, "```memql\n"+form.Replacement+"\n```", "Write `"+form.Replacement+"` instead;")
		}
		for _, w := range want {
			if !strings.Contains(card, w) {
				t.Errorf("%s: the card should contain %q, got:\n%s", form.Rule, w, card)
			}
		}
	}
	if want := len(forms) - len(unhoverable); checked != want {
		t.Errorf("hovered %d retired forms, want %d: the parser's table is %d forms", checked, want, len(forms))
	}
}

// The predicate positions' legacy spellings are hovered on every token an
// author meets them by, and the card shows the author's own construct as the
// rewrite writes it -- or the table's form, when the rewrite refuses it.
func TestRetiredPredicateHover(t *testing.T) {
	s := New(v1Registry())
	spec := "use todos.concepts.{ todo }\n\nspec todo isOverdue {\n  return done == false\n}"
	for _, c := range []struct {
		name, src, needle, code string
	}{
		{"the header of a brace-bodied spec", spec, "spec", "spec todo isOverdue = row => row.done == false"},
		{"the header of a brace-bodied trait", "trait isOpen { return status == \"open\" }", "trait", "trait isOpen = row => row.status == \"open\""},
		// requiresOwner is a loaded context spec: its parameter is the actor.
		{"a spec the registry knows reads the actor", "use common.shapes.{ actorEnvelope }\n\nspec actorEnvelope requiresOwner {\n  return role == \"owner\"\n}",
			"return", "spec actorEnvelope requiresOwner = actor => actor.role == \"owner\""},
		// isAdmin is not loaded, but a loaded context spec binds actorEnvelope.
		{"a new spec over an @actor shape reads the actor", "spec actorEnvelope isAdmin {\n  return role == \"admin\"\n}",
			"return", "spec actorEnvelope isAdmin = actor => actor.role == \"admin\""},
		{"the @ of a lambda-less @filter", "@filter(payload.done == true)\nautomation onTodo {\n}", "@", "@filter(row => row.done == true)"},
		// The rewrite keeps the author's line breaks; the card keeps its lines.
		{"a filter continued over several lines", v1Query + "  filter status == args.owner\n    && isActiveRecord\n}", "filter",
			"filter row => row.status == args.owner\n           && isActiveRecord(row)"},
		// notAPredicate is neither a spec nor a trait, so the rewrite refuses
		// the clause and the card falls back to the table's form.
		{"a filter the rewrite refuses", v1Query + "  filter status == args.owner && notAPredicate\n}", "filter", "filter row => <predicate>"},
	} {
		t.Run(c.name, func(t *testing.T) {
			card := hoverFirst(t, s, c.src, c.needle)
			hoverStyle(t, card)
			if !strings.Contains(card, "```memql\n"+c.code+"\n```") || !strings.Contains(card, "is retired in edition 2026") {
				t.Errorf("the card should show %q and the retirement, got:\n%s", c.code, card)
			}
		})
	}

	// The v1 forms, and the same words anywhere else, are not retired.
	for name, c := range map[string]struct{ src, needle string }{
		"a lambda filter":          {v1Query + "  filter row => row.status == args.owner\n}", "filter"},
		"a lambda spec":            {"spec todo isOverdue = row => row.done == false", "spec"},
		"a lambda @filter":         {"@filter(row => row.done == true)\nautomation onTodo {\n}", "@filter"},
		"a return in a logic body": {"logic compute {\n  body {\n    return args.a\n  }\n}", "return"},
		"a filter not yet written": {v1Query + "  filter \n}", "filter"},
	} {
		if card := hoverFirst(t, s, c.src, c.needle); strings.Contains(card, "retired") {
			t.Errorf("%s is not a retired form, got:\n%s", name, card)
		}
	}
}

func TestTokenizePipePipe(t *testing.T) {
	svc := &Service{}
	for src, want := range map[string]map[string]string{
		"a || b":        {"||": "operator"},
		"a && b":        {"&&": "operator"},
		"row.?lineage":  {".?": "operator"},
		"a has b":       {"has": "operator"},
		"import x":      {"import": "keyword"},
		"x => x == nil": {"=>": "operator"},
	} {
		tokens := svc.Tokenize(src)
		for literal, typ := range want {
			if got := firstTokenType(tokens, literal); got != typ {
				t.Errorf("Tokenize(%q): %q is %q, want %q", src, literal, got, typ)
			}
		}
	}
}

// retiredCallRE matches a retired function spelling used as a CALL -- the name
// followed by `(`, not as a method (`.count(` is the live list method).
func retiredCallRE() *regexp.Regexp {
	var names []string
	for name := range functions.RetiredFunctions() {
		names = append(names, regexp.QuoteMeta(name))
	}
	return regexp.MustCompile(`(^|[^.\w])(` + strings.Join(names, "|") + `)\(`)
}

// snippetTexts collects every snippet the service can insert: the construct
// skeletons, every construct's block snippets, and the annotation snippets.
func snippetTexts(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, sk := range constructSkeletons {
		out["skeleton "+sk.keyword] = sk.body
	}
	for _, c := range dslSpec.Constructs {
		for _, blk := range c.BodyBlocks {
			it := blockSnippet(blk, c.Keyword)
			out["block "+c.Keyword+"."+blk] = it.InsertText
		}
	}
	for _, it := range annotationSnippets(EnclosingConstruct{Keyword: "automation", Receiver: "Automation"}) {
		out["annotation "+it.Label] = it.InsertText
	}
	return out
}

func TestSnippetsUseTheV1Forms(t *testing.T) {
	texts := snippetTexts(t)
	retired := retiredCallRE()
	for name, text := range texts {
		if m := retired.FindStringSubmatch(text); m != nil {
			t.Errorf("%s emits the retired call %s(: %q", name, m[2], text)
		}
		for _, needle := range []string{"when(", "filter {", "{ return", "null", " has ", "?.", ";", "${1:Concept}."} {
			if strings.Contains(text, needle) {
				t.Errorf("%s emits the retired form %q: %q", name, needle, text)
			}
		}
	}
	// A spec or trait body is a lambda now; `return` there is the retired form.
	for _, name := range []string{"skeleton spec", "skeleton trait"} {
		if strings.Contains(texts[name], "return") {
			t.Errorf("%s still writes a return body: %q", name, texts[name])
		}
	}

	for name, want := range map[string]string{
		"skeleton query":          "filter row => row.${3:field} == args.${4:value}",
		"skeleton spec":           "= row => row.",
		"skeleton trait":          "= row => row.",
		"block query.filter":      "filter row => row.",
		"annotation @filter(...)": "filter(row => row.",
	} {
		text, ok := texts[name]
		if !ok {
			t.Errorf("no snippet %q; have %v", name, snippetNames(texts))
			continue
		}
		if !strings.Contains(text, want) {
			t.Errorf("%s = %q, want it to contain the v1 form %q", name, text, want)
		}
	}
}

func snippetNames(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
