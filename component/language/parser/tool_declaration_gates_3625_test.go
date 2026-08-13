package parser

import (
	"strings"
	"testing"
)

// tool_declaration_gates_3625_test.go -- memql#3625, the parse-time half.
//
// A tool declaration is read by NAME out of an annotation's argument map, so
// an argument nobody looks up is simply never read. That made a one-letter
// typo indistinguishable from omitting the argument -- and a tool whose
// handler had silently evaporated still registered and was still advertised to
// the model. The same silence swallowed an unknown FIELD annotation, so a
// constraint the author wrote (`@enums`) was never enforced and nothing said
// so.
//
// These are the two shapes memql#3625 measured, plus the direction that keeps
// the change honest.

// The archetype: a typo in the VALUE-carrying kwarg left the function name
// empty, so the tool registered with a handler that names nothing.
func TestToolHandlerRejectsTypodValueKwarg(t *testing.T) {
	src := `@description("create a todo")
@handler(type="function", nmae="createTodo")
tool todosCreate {
  title string @required
}
`
	_, err := ParseToolDecl(src)
	if err == nil {
		t.Fatal("`nmae=` was accepted. An unrecognised argument name is never read, so the " +
			"function name stayed empty and the tool registered with a handler that names " +
			"nothing -- then failed only when a model called it (memql#3625).")
	}
	if !strings.Contains(err.Error(), "nmae") {
		t.Errorf("the error must NAME the unrecognised argument, or the author cannot find it "+
			"in a long annotation; got: %v", err)
	}
	if !strings.Contains(err.Error(), "name") {
		t.Errorf("the error must list the supported argument names so the fix is obvious; got: %v", err)
	}
}

// The sharper half of the same typo family: mistyping `type` dropped the
// ENTIRE handler, because the converter only builds a ToolHandler when
// HandlerType is non-empty.
func TestToolHandlerRejectsTypodTypeKwarg(t *testing.T) {
	src := `@description("create a todo")
@handler(tipe="function", name="createTodo")
tool todosCreate {
  title string @required
}
`
	_, err := ParseToolDecl(src)
	if err == nil {
		t.Fatal("`tipe=` was accepted. With no `type` the converter builds no handler at all, " +
			"so the tool registered with NOTHING to execute and was advertised to the model " +
			"anyway (memql#3625).")
	}
	if !strings.Contains(err.Error(), "tipe") {
		t.Errorf("the error must name the unrecognised argument; got: %v", err)
	}
}

// A @handler present but carrying no type is the same outcome by a different
// route, so it is refused at the same place rather than left to a later layer
// that never sees a handler to check.
func TestToolHandlerRequiresAType(t *testing.T) {
	src := `@description("create a todo")
@handler(name="createTodo")
tool todosCreate {
  title string @required
}
`
	_, err := ParseToolDecl(src)
	if err == nil {
		t.Fatal("@handler with no type= was accepted, which produces a tool with no handler")
	}
	if !strings.Contains(err.Error(), "type") {
		t.Errorf("the error must say the type argument is what is missing; got: %v", err)
	}
}

// @rateLimit is the second kwarg-bearing annotation on a tool and had the
// identical defect twice over: an unrecognised name was dropped, and so was a
// value that did not parse as an integer. Either way the author declared a
// ceiling and got none.
func TestToolRateLimitRejectsTypoAndNonInteger(t *testing.T) {
	for _, tc := range []struct{ name, annotation, want string }{
		{"typo'd kwarg", `@rateLimit(maxCals=10, periodSeconds=60)`, "maxCals"},
		{"non-integer maxCalls", `@rateLimit(maxCalls="ten", periodSeconds=60)`, "maxCalls"},
		{"non-integer period", `@rateLimit(maxCalls=10, periodSeconds="a minute")`, "periodSeconds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := `@description("create a todo")
@handler(type="function", name="createTodo")
` + tc.annotation + `
tool todosCreate {
  title string @required
}
`
			_, err := ParseToolDecl(src)
			if err == nil {
				t.Fatalf("%s was accepted -- the declared rate limit is silently discarded and "+
					"the tool runs unthrottled", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the error must name %q; got: %v", tc.want, err)
			}
		})
	}
}

// An unknown FIELD annotation was pure declaration theatre: `@enums` never
// reached the JSON schema, so the model was told the field was an
// unconstrained string, and nothing complained.
func TestToolFieldRejectsUnknownAnnotation(t *testing.T) {
	src := `@description("create a todo")
@handler(type="function", name="createTodo")
tool todosCreate {
  status string @enums("open", "done")
}
`
	_, err := ParseToolDecl(src)
	if err == nil {
		t.Fatal("@enums was accepted and discarded. The constraint the author wrote never " +
			"reached the schema, so the model saw an unconstrained string (memql#3625).")
	}
	if !strings.Contains(err.Error(), "enums") || !strings.Contains(err.Error(), "@enum") {
		t.Errorf("the error must name the unknown annotation AND the spelling that works; got: %v", err)
	}
}

// The direction that keeps the change honest: every spelling the corpus
// actually uses must still parse, and must still carry its value through.
func TestValidToolDeclarationsStillParse(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
	}{
		{"function handler", `@description("d")
@handler(type="function", name="createTodo")
@executionTime("fast")
tool todosCreate {
  title  string  @required @description("the title")
  done   boolean @default("false")
}
`},
		{"query handler", `@description("d")
@handler(type="query", query="query todos(done: $args.done)")
tool todosList {
  done boolean @description("filter")
}
`},
		{"webhook handler with method", `@description("d")
@handler(type="webhook", url="https://example.test/hook", method="post")
tool hookIt {
  body object
}
`},
		{"rate limit", `@description("d")
@handler(type="function", name="createTodo")
@rateLimit(maxCalls=10, periodSeconds=60)
tool todosCreate {
  title string @required
}
`},
		{"enum type + enum annotation", `@description("d")
@handler(type="function", name="createTodo")
tool todosCreate {
  priority enum("low", "high")
  status   string @enum("open", "done")
  who      string @autoInjected
}
`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decl, err := ParseToolDecl(tc.src)
			if err != nil {
				t.Fatalf("a valid tool declaration must still parse: %v", err)
			}
			if decl.HandlerType == "" {
				t.Fatalf("handler type lost: %#v", decl)
			}
		})
	}
}

// Distinct @handler arguments must survive with the values written -- a fix
// that refused every multi-argument @handler would pass every test above.
func TestToolHandlerArgumentsSurviveWithTheirValues(t *testing.T) {
	decl, err := ParseToolDecl(`@description("d")
@handler(type="webhook", url="https://example.test/hook", method="patch")
tool hookIt {
  body object
}
`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if decl.HandlerType != "webhook" {
		t.Errorf("HandlerType = %q, want webhook", decl.HandlerType)
	}
	if decl.HandlerURL != "https://example.test/hook" {
		t.Errorf("HandlerURL = %q", decl.HandlerURL)
	}
	if decl.HandlerMethod != "PATCH" {
		t.Errorf("HandlerMethod = %q, want PATCH (upper-cased)", decl.HandlerMethod)
	}
}
