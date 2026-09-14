package memql

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// logic_statement_validators_test.go -- the load-time validators that read a
// logic's body as text reach a statement body (epic memql#5370).
//
// They find the body with extractFunctionBody, which knew one header: the
// receiver form the rewriter emits for every construct it rewrites. The
// rewriter leaves a statement-body logic as written, so each of them read "no
// body" and skipped its check, and a logic that read the actor without
// `@actor`, an undeclared argument or an event field outside its
// `@eventField` loaded.

func TestStatementLogicValidatorsReachTheBody(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"the actor read without @actor",
			"logic probe {\n  return actor.userId\n}",
			"reads the auth envelope (actor.*) but does not declare `@actor`"},
		{"an argument the args block does not declare",
			"logic probe {\n  args {\n    a  string\n  }\n  return args.b\n}",
			"body reads args.b, which is not declared in the args block (declared: a)"},
		{"the event read without declaring it",
			"logic probe {\n  return args.event.payload.id\n}",
			"does not declare an `event` input in its args block"},
		{"an event field outside @eventField",
			"@eventField(\"id\")\nlogic probe {\n  args {\n    event  object!\n  }\n  return args.event.payload.priority\n}",
			"references event payload field(s) priority not declared in its @eventField schema (declared: id)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadLogicV1(t, "probe", tc.src)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}

	// A body that declares what it reads loads -- a comment in its args block
	// naming a field the @eventField set leaves out included, since the args
	// block declares and does not read.
	fn, err := loadLogicV1(t, "probe", "@actor\n@eventField(\"id\")\nlogic probe {\n  args {\n"+
		"    /// Carries event.payload.priority too; the body does not read it.\n    event  object!\n  }\n"+
		"  who := actor.userId\n  return args.event.payload.id ?? who\n}")
	require.NoError(t, err)
	require.NotNil(t, fn.LogicBody, "the probe must load as a statement body, or this measures the legacy path")
}

// TestExtractFunctionBodyFindsAStatementBody: the statements after the args
// block, with braces in a comment and in a string left out of the matching.
func TestExtractFunctionBodyFindsAStatementBody(t *testing.T) {
	src := "/// A doc comment with a brace: {\n@actor\nlogic probe {\n  args {\n    a  string\n  }\n" +
		"  x := query q(v: \"}\")\n  return x\n}\n"
	require.Equal(t, "\n  x := query q(v: \"}\")\n  return x\n", extractFunctionBody(src))

	// No args block: the whole body.
	require.Equal(t, "\n  return 1\n", extractFunctionBody("logic probe {\n  return 1\n}\n"))

	// The receiver form still wins where it is present.
	require.Equal(t, " return 1 ", extractFunctionBody("func (Logic) probe(_ any) any { return 1 }"))
}
