package parser

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/ast"
)

// memql#3336 -- an args-field `@description` is REJECTED at load.
//
// It used to be accepted and then thrown away (there is no AST slot for it),
// so the same annotation meant two different things depending on which
// construct it sat in, and one of those meanings was "silently nothing".
// The live channel for an arg description is the `///` doc comment above the
// field; the annotation now fails loud, exactly as `@default` on an args
// field does (#991).
//
// These tests pin BOTH halves: the rejection on every construct that takes an
// `args { ... }` block, and the untouched `@description` on tool / prompt /
// builtin bodies, where the annotation is load-bearing and retained.

// The bare args-block parser rejects the annotation, with a message that
// names the annotation, the construct surface, and the `///` alternative.
func TestParseArgsBlockField_RejectsDescription(t *testing.T) {
	_, err := parseArgsSafe(`args { kind string @description("the kind") }`)
	if err == nil {
		t.Fatal("expected @description on an args field to be rejected, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"@description", "args field", "///"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error should mention %q; got %q", want, msg)
		}
	}
}

// The rejection survives the annotation carrying no parens / a bad payload --
// the annotation NAME is what is refused, so the author gets the migration
// message rather than a generic "expected `(`" complaint.
func TestParseArgsBlockField_RejectsDescriptionWithoutParens(t *testing.T) {
	_, err := parseArgsSafe(`args { kind string @description }`)
	if err == nil {
		t.Fatal("expected a bare @description on an args field to be rejected, got nil")
	}
	if !strings.Contains(err.Error(), "///") {
		t.Errorf("error should point at the /// doc comment; got %q", err.Error())
	}
}

// Every construct that takes an `args { ... }` block rejects it, through the
// real loader pipeline (struct-form rewriter -> parser).
func TestArgsFieldDescription_RejectedOnEveryConstruct(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{
			name: "query",
			src: `use cognition.concepts.{ participant }
use cognition.shapes.{ participantFull }
@description("participants in a space")
query participant spaceParticipants {
  args {
    spaceId string @required @description("the space")
  }
  filter spaceId==args.spaceId
  shape  participantFull
}`,
		},
		{
			name: "mutate",
			src: `use cognition.concepts.{ space }
@description("create a space")
mutate space createSpace {
  args {
    spaceId string @required @description("the space id")
  }
  insert {
    id:        args.spaceId
    createdAt: now
  }
}`,
		},
		{
			name: "logic",
			src: `@description("decide a thing")
logic decideThing {
  args {
    x string @required @description("the input")
  }
  body {
    return x
  }
}`,
		},
		{
			// `action` and `capability` reach parseArgsBlockField through
			// their OWN call sites in parseActionDecl / parseCapabilityDecl,
			// not through the file-top/parseDefinition pair the other kinds
			// use -- they are the two constructs the first pass of memql#3336
			// missed. They discard the annotation identically: ArgsField has
			// no Description slot, and both loaders build their param structs
			// from Name/Type/Required alone.
			name: "action",
			src: `use capabilities.shell.{ script }
@description("Check out a repo at a ref.")
action cloneRepoAtVersion {
  args {
    workdir string @required @description("repo working tree")
  }
  capability script(script: "deploy.cloneRepo", workdir: args.workdir)
}`,
		},
		{
			name: "capability",
			src: `@sideEffect("exec")
@description("Run a shell script.")
capability shell.script {
  args {
    script string @required @description("the script id")
  }
}`,
		},
		{
			name: "automation",
			src: `@trigger(event="node.created", concept="v1:cognition:participant", partition="*")
@description("bootstrap a session")
automation bootstrapSession {
  args {
    id any @description("the participant id")
  }

  step decide {
    logic bootstrapSession ( event )
  }
}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := rewriteAndParse(t, tc.src)
			if err == nil {
				t.Fatalf("expected an args-field @description on a %s to be rejected, got nil", tc.name)
			}
			if !strings.Contains(err.Error(), "///") {
				t.Errorf("error should point at the /// doc comment; got %q", err.Error())
			}
		})
	}
}

// (The channel that DOES carry an arg description -- the `///` doc comment
// landing on ArgsField.DocComment -- is pinned by TestDocComment_ArgsFieldSlot
// in doc_comment_test.go.)

// NEGATIVE CASE: a `tool` field @description is untouched -- accepted AND
// retained. Same for prompt + builtin bodies, where the body IS the schema
// and the annotation has a real AST slot.
func TestFieldDescriptionStillRetainedOnDeclarativeBodies(t *testing.T) {
	t.Run("tool", func(t *testing.T) {
		decl, err := ParseToolDecl(`@description("Search for users")
@handler(type="query", query="concept==v1:memql:backend:user")
tool searchUsers {
  active boolean @description("Filter by active status")
}`)
		if err != nil {
			t.Fatalf("ParseToolDecl: %v", err)
		}
		if len(decl.Fields) != 1 || decl.Fields[0].Description != "Filter by active status" {
			t.Fatalf("tool field @description must be retained; got %+v", decl.Fields)
		}
	})

	t.Run("prompt", func(t *testing.T) {
		decl, err := ParsePromptDecl(`@description("Generate an agent reply")
@templateFile("agentReply.tmpl")
prompt agentReply {
  space object @required @description("The space envelope")
}`)
		if err != nil {
			t.Fatalf("ParsePromptDecl: %v", err)
		}
		if len(decl.Fields) != 1 {
			t.Fatalf("Fields = %d, want 1", len(decl.Fields))
		}
		if got := attrValue(decl.Fields[0].Attributes, "description"); got != "The space envelope" {
			t.Fatalf("prompt field @description must be retained; got %q", got)
		}
	})

	t.Run("builtin", func(t *testing.T) {
		decl, err := ParseBuiltinDecl(`@description("Score an utterance")
@executor("integration.cognition.scoreUtterance")
builtin cognitionScore {
  spaceId string @required @description("The space id")
}`)
		if err != nil {
			t.Fatalf("ParseBuiltinDecl: %v", err)
		}
		if len(decl.Fields) != 1 {
			t.Fatalf("Fields = %d, want 1", len(decl.Fields))
		}
		if got := attrValue(decl.Fields[0].Attributes, "description"); got != "The space id" {
			t.Fatalf("builtin field @description must be retained; got %q", got)
		}
	})
}

// attrValue returns the string value of the named field-level annotation,
// or "" when it is absent. Prompt + builtin fields keep their annotations
// in the generic Attributes slice rather than a dedicated slot.
func attrValue(attrs []*ast.Attribute, name string) string {
	for _, a := range attrs {
		if a.Name != name {
			continue
		}
		if s, ok := a.Value.(string); ok {
			return s
		}
	}
	return ""
}
