package memql

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// memql#1074: the per-construct loader treated a `func (Query)` /
// `func (Mutation)` token that only appears inside a `//` line comment
// or a `/* */` block comment as a real procedural header, then
// mis-parsed the FOLLOWING struct construct -- a struct query whose
// leading comment mentioned `func (Query)` failed with `query body
// must start with 'return'`. The fix blanks comment bytes (preserving
// offsets) before any header detection runs. These tests pin that a
// comment-embedded receiver token no longer breaks the load.

func bug1074Registry() memoryNodes.Registry {
	return newMemoryRegistry(map[string]*memoryNodes.Concept{
		"v1:cognition:space": {Name: "v1:cognition:space"},
	})
}

func TestLoader_QueryWithFuncTokenInLineComment(t *testing.T) {
	registry := bug1074Registry()
	src := "use cognition.concepts.{ space }\n\n" +
		"// Migrated from the legacy `func (Query)` procedural form -- see memql#1074.\n" +
		"@description(\"recent active spaces\")\n" +
		"query space queryRecentSpacesLine {\n" +
		"  filter  payload.status == \"active\"\n" +
		"  shape   space\n" +
		"}"

	fn, err := tryParseNewFunctionSyntax("queryRecentSpacesLine", "query", src, "cognition.queries.memql", registry)
	require.NoError(t, err, "struct query with `func (Query)` in a // comment must still load")
	require.NotNil(t, fn)
	require.Equal(t, "queryRecentSpacesLine", fn.Name)
	require.Equal(t, "query", fn.FunctionKind)
}

func TestLoader_QueryWithFuncTokenInBlockComment(t *testing.T) {
	registry := bug1074Registry()
	src := "use cognition.concepts.{ space }\n\n" +
		"/*\n" +
		" * Was once `func (Query) queryRecentSpaces(ctx any) (any, error)`.\n" +
		" * Now struct form. memql#1074.\n" +
		" */\n" +
		"query space queryRecentSpacesBlock {\n" +
		"  filter  payload.status == \"active\"\n" +
		"  shape   space\n" +
		"}"

	fn, err := tryParseNewFunctionSyntax("queryRecentSpacesBlock", "query", src, "cognition.queries.memql", registry)
	require.NoError(t, err, "struct query with `func (Query)` in a /* */ comment must still load")
	require.NotNil(t, fn)
	require.Equal(t, "queryRecentSpacesBlock", fn.Name)
}

func TestLoader_MutationWithFuncTokenInLineComment(t *testing.T) {
	registry := bug1074Registry()
	src := "use cognition.concepts.{ space }\n\n" +
		"// Replaces the retired `func (Mutation)` author form (memql#1074).\n" +
		"@description(\"create a space\")\n" +
		"mutate space mutationCreateSpaceLineComment {\n" +
		"  args {\n" +
		"    partitionId  string  @required\n" +
		"    name     string  @required\n" +
		"  }\n" +
		"  insert {\n" +
		"    id:        args.partitionId\n" +
		"    name:      args.name\n" +
		"    status:    \"active\"\n" +
		"    createdAt: now\n" +
		"    createdBy: actor.userId\n" +
		"  }\n" +
		"}"

	fn, err := tryParseNewFunctionSyntax("mutationCreateSpaceLineComment", "mutation", src, "cognition.mutations.memql", registry)
	require.NoError(t, err, "struct mutation with `func (Mutation)` in a // comment must still load")
	require.NotNil(t, fn)
	require.Equal(t, "mutation", fn.FunctionKind)
}

func TestLoader_MutationWithFuncTokenInBlockComment(t *testing.T) {
	registry := bug1074Registry()
	src := "use cognition.concepts.{ space }\n\n" +
		"/* legacy: func (Mutation) mutationCreateSpace(ctx any) error { ... } */\n" +
		"mutate space mutationCreateSpaceBlockComment {\n" +
		"  args {\n" +
		"    partitionId  string  @required\n" +
		"    name     string  @required\n" +
		"  }\n" +
		"  insert {\n" +
		"    id:        args.partitionId\n" +
		"    name:      args.name\n" +
		"    status:    \"active\"\n" +
		"    createdAt: now\n" +
		"    createdBy: actor.userId\n" +
		"  }\n" +
		"}"

	fn, err := tryParseNewFunctionSyntax("mutationCreateSpaceBlockComment", "mutation", src, "cognition.mutations.memql", registry)
	require.NoError(t, err, "struct mutation with `func (Mutation)` in a /* */ comment must still load")
	require.NotNil(t, fn)
	require.Equal(t, "mutation", fn.FunctionKind)
}

// The slicer must extract exactly the one real struct construct (0
// skips) even when a `func (Query)` token sits in a preceding comment.
func TestExtractFunctionSlices_IgnoresFuncTokenInComment(t *testing.T) {
	src := "use cognition.concepts.{ space }\n\n" +
		"// once a `func (Query)` procedural form -- memql#1074\n" +
		"query space queryRecentSpacesSlice {\n" +
		"  filter  payload.status == \"active\"\n" +
		"  shape   space\n" +
		"}\n\n" +
		"/* func (Mutation) legacyWrite(ctx any) error */\n" +
		"mutate space mutationWriteSpaceSlice {\n" +
		"  args { partitionId string @required }\n" +
		"  insert { id: args.partitionId status: \"active\" createdAt: now createdBy: actor.userId }\n" +
		"}"

	slices := ExtractFunctionSlices(src)
	require.Len(t, slices, 2, "exactly the two real constructs must be sliced, not the comment tokens")

	names := []string{slices[0].Name, slices[1].Name}
	require.Contains(t, names, "queryRecentSpacesSlice")
	require.Contains(t, names, "mutationWriteSpaceSlice")
}

// A genuine (non-comment) procedural function must still be detected
// exactly as before: BlankComments only touches comment bytes, so the
// real `func (Builtin) ...` header is untouched and extracted.
func TestExtractFunctionSlices_RealProceduralStillParses(t *testing.T) {
	src := "func (Builtin) realBuiltinFn(ctx any) (any, error) {\n" +
		"  return concept==v1:cognition:space, nil\n" +
		"}"

	slices := ExtractFunctionSlices(src)
	require.Len(t, slices, 1, "a genuine procedural func must still be sliced")
	require.Equal(t, "realBuiltinFn", slices[0].Name)
	require.True(t, strings.HasPrefix(strings.TrimSpace(slices[0].Source), "func (Builtin)"))
}

// BlankComments unit coverage: comment markers inside string literals
// survive; comment content is blanked to spaces; byte length + line
// count are preserved; a real header outside any comment is untouched.
func TestBlankComments_Behaviour(t *testing.T) {
	in := "func (Query) foo() {\n" +
		"  // func (Mutation) not real\n" +
		"  x := \"// not a comment\"\n" +
		"  /* func (Logic) also not real */\n" +
		"}"
	out := languageParser.BlankComments(in)

	require.Equal(t, len(in), len(out), "byte length must be preserved")
	require.Equal(t, strings.Count(in, "\n"), strings.Count(out, "\n"), "newlines must be preserved")

	// The real header is outside any comment -> untouched.
	require.Contains(t, out, "func (Query) foo()")
	// The string literal's `// not a comment` survives.
	require.Contains(t, out, "\"// not a comment\"")
	// The commented-out receiver tokens are gone.
	require.NotContains(t, out, "func (Mutation)")
	require.NotContains(t, out, "func (Logic)")
}
