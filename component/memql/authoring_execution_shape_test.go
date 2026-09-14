package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

func TestRunScopedAuthoredQueryResolvesSiblingShape(t *testing.T) {
	e, _, base := sharedReadMergeEngine(t)
	owner := uniqueSuffix(t.Name())
	ctx := auth.ContextWithUserActor(base, owner)
	todoID := runMutation(t, ctx, e, "createTodo", map[string]any{"todoId": owner, "title": "Private shaped result"})
	shapeCount := e.Shapes().Count()
	for _, tc := range []struct{ name, body, reference string }{
		{"explicit", "row.id\n title", "executionTodoCard"},
		{"qualified", "row.id\n title", "todo.executionTodoCard"},
		{"default", "", "executionTodoCard"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := NewAuthoredRuntimeRegistry()
			// `todo` is a CORE concept and this bundle is authored in its own
			// private domain, so the signature concept has to be imported --
			// the ambient same-domain rule cannot reach it (memql#5375 made
			// the origin the only source of a bundle's domain).
			source := fmt.Sprintf(`use todos.concepts.{ todo }

@row
shape todo executionTodoCard { %s }
@actor
query todo executionTodos {
  args { todoId string! }
  filter row => row.id == args.todoId && row.ownerUserId == actor.userId
  paginate 10
  shape %s
}`, tc.body, tc.reference)
			defined, err := AuthorSessionBundle(reg, owner, source, "shapecheck/concepts.memql")
			require.NoError(t, err, "bundle validation: %+v", defined.Diagnostics)
			runCtx := ContextWithAuthoredExecution(ctx, owner, reg)
			result, err := e.Execute(runCtx, fmt.Sprintf("query executionTodos(todoId: %s)", langparser.QuoteString(todoID)))
			require.NoError(t, err, "agent execution must resolve the shape in the same private bundle")
			row := singleShapeRow(t, result.OutputPayload())
			require.Equal(t, "Private shaped result", row["title"])
			if tc.body != "" {
				require.Equal(t, todoID, row["id"])
				require.Len(t, row, 2)
			}

			_, err = e.resolveNamedShapeForContext(auth.ContextWithUserActor(runCtx, "stranger"), tc.reference, "")
			require.ErrorContains(t, err, "not found", "carrying the run context does not grant its owner's private shapes")
			_, err = e.resolveNamedShapeForContext(auth.ContextWithUserActor(context.Background(), owner), tc.reference, "")
			require.ErrorContains(t, err, "not found", "a later request without the run cannot resolve its private shape")
			require.Equal(t, shapeCount, e.Shapes().Count())
			_, leaked := e.Shapes().Get("executionTodoCard")
			require.False(t, leaked)
		})
	}
}

func TestRunScopedAuthoredShapeKeepsInternalFieldsPrivate(t *testing.T) {
	concept := &memorynodes.Concept{Name: "v1:shapecheck:privateItem", Schemas: map[string]json.RawMessage{
		"definition": json.RawMessage(`{"type":"object","properties":{"title":{"type":"string"},"secret":{"type":"string","x-internal":true}}}`),
	}}
	e := &MemQLEngine{shapes: newShapeRegistry(), concepts: memorynodes.NewRegistry(map[string]*memorynodes.Concept{concept.Name: concept})}
	for _, tc := range []struct{ name, body, wantError string }{
		{"default", "", ""},
		{"explicit internal", "secret", "internal field"},
		{"unknown field", "missingField", "does not declare"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := NewAuthoredRuntimeRegistry()
			require.NoError(t, reg.Register(&AuthoredConstruct{OwnerUserId: "alice", Kind: "shape", Name: "privateItemCard", Version: 1,
				Source: fmt.Sprintf("@row\nshape privateItem privateItemCard { %s }", tc.body)}))
			ctx := ContextWithAuthoredExecution(auth.ContextWithUserActor(context.Background(), "alice"), "alice", reg)
			got, err := e.resolveNamedShapeForContext(ctx, "privateItemCard", "")
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
				return
			}
			require.NoError(t, err)
			obj, ok := got.(*shapeObject)
			require.True(t, ok)
			require.Contains(t, obj.Fields, "title")
			require.NotContains(t, obj.Fields, "secret")
			require.Equal(t, 0, e.Shapes().Count())
		})
	}
}

func TestRunScopedAuthoredUnboundShapeCannotBypassFieldChecks(t *testing.T) {
	e := &MemQLEngine{shapes: newShapeRegistry()}
	reg := NewAuthoredRuntimeRegistry()
	require.NoError(t, reg.Register(&AuthoredConstruct{OwnerUserId: "alice", Kind: "shape", Name: "unboundCard", Version: 1,
		Source: "@row\nshape unboundCard { secret }"}))
	ctx := ContextWithAuthoredExecution(auth.ContextWithUserActor(context.Background(), "alice"), "alice", reg)
	_, err := e.resolveNamedShapeForContext(ctx, "unboundCard", "")
	require.ErrorContains(t, err, "payload projection requires a bound concept")
}

func TestRunScopedAuthoredShapeCannotShadowCore(t *testing.T) {
	e, _, base := sharedReadMergeEngine(t)
	owner := uniqueSuffix(t.Name())
	ctx := auth.ContextWithUserActor(base, owner)
	todoID := runMutation(t, ctx, e, "createTodo", map[string]any{"todoId": owner, "title": "Core shape wins"})
	for _, tc := range []struct{ name, reference, imports string }{
		{"bare", "todoFull", ""},
		{"concept qualified", "todo.todoFull", ""},
		{"namespace qualified", "todos.todoFull", ""},
		{"import alias", "canonicalCard", "use todos.shapes.{ todoFull as canonicalCard }\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := NewAuthoredRuntimeRegistry()
			defined, err := AuthorSessionBundle(reg, owner, tc.imports+fmt.Sprintf(`@row
shape todo todoFull { row.id }
@actor
query todo executionCoreShape {
  args { todoId string! }
  filter row => row.id == args.todoId && row.ownerUserId == actor.userId
  paginate 10
  shape %s
}`, tc.reference), "")
			require.NoError(t, err, "bundle validation: %+v", defined.Diagnostics)
			result, err := e.Execute(ContextWithAuthoredExecution(ctx, owner, reg), fmt.Sprintf("query executionCoreShape(todoId: %s)", langparser.QuoteString(todoID)))
			require.NoError(t, err)
			row := singleShapeRow(t, result.OutputPayload())
			require.Equal(t, "Core shape wins", row["title"], "the private id-only shape must not replace the qualified core shape")
		})
	}
}
