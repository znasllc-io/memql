package memql

import (
	"errors"
	"strings"
	"testing"
)

// writeShapingConceptSrc is the concept every case below writes: one field of
// each kind a write-shaping annotation cares about.
const writeShapingConceptSrc = `
concept Ticket {
  title        string
  status       string
  attempts     int
  closedAt     datetime
  labels       []string
  preferences  object
  settings {
    theme  string
  }
}
`

// TestWriteShapingAnnotationsNameFieldsTheWriteCanShape (memql#5426): each of
// the six write-shaping annotations is refused, at load, when it names a field
// the bound concept does not declare as a top-level field, a field of a type
// it cannot shape, or a field the mutation's block does not write -- each of
// which loaded before and did nothing, because the executor keys on the name.
// Every refusal names the annotation and the field and carries its rule id.
func TestWriteShapingAnnotationsNameFieldsTheWriteCanShape(t *testing.T) {
	reg := conceptRegistry(mustConcept(t, writeShapingConceptSrc, "v1/test/ticket"))
	insertBlock := "  insert {\n    id: args.ticketId\n    title: args.title\n    status: \"open\"\n  }\n"
	updateBlock := func(field, value string) string {
		return "  update {\n    id: args.ticketId\n    " + field + ": " + value + "\n  }\n"
	}
	cases := []struct {
		name, annotation, args, block, code, want string
	}{
		{"createOnly, an undeclared field", `@createOnly("priority")`, "ticketId string!\n    title string!", insertBlock,
			CodeWriteAnnotationUndeclaredField, `@createOnly("priority") names a field concept "v1:test:ticket" does not declare as a top-level field`},
		{"createOnly, a field the insert does not write", `@createOnly("status", "attempts")`, "ticketId string!\n    title string!", insertBlock,
			CodeWriteAnnotationUnwrittenField, `@createOnly("attempts") names a field this mutation's insert block does not write`},
		{"noUnset, a field the update does not write", `@noUnset("closedAt")`, "ticketId string!", updateBlock("status", `"closed"`),
			CodeWriteAnnotationUnwrittenField, `@noUnset("closedAt") names a field this mutation's update block does not write`},
		{"noUnset, a nested path", `@noUnset("settings.theme")`, "ticketId string!\n    theme string!", updateBlock("settings", "{ theme: args.theme }"),
			CodeWriteAnnotationUndeclaredField, `@noUnset("settings.theme") names a field concept "v1:test:ticket" does not declare as a top-level field`},
		{"mergeFields, a string", `@mergeFields("title")`, "ticketId string!\n    title string!", updateBlock("title", "args.title"),
			CodeWriteAnnotationFieldType, `@mergeFields("title") names a field declared string: @mergeFields deep-merges a stored object`},
		{"appendFields, a number", `@appendFields("attempts")`, "ticketId string!\n    attempts int!", updateBlock("attempts", "args.attempts"),
			CodeWriteAnnotationFieldType, `@appendFields("attempts") names a field declared int: @appendFields changes the members of a stored list`},
		{"addToSet, an undeclared field", `@addToSet("tags")`, "ticketId string!\n    labels []string!", updateBlock("labels", "args.labels"),
			CodeWriteAnnotationUndeclaredField, `@addToSet("tags") names a field concept "v1:test:ticket" does not declare`},
		{"removeFromSet, a field the update does not write", `@removeFromSet("labels")`, "ticketId string!\n    title string!", updateBlock("title", "args.title"),
			CodeWriteAnnotationUnwrittenField, `@removeFromSet("labels") names a field this mutation's update block does not write`},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			name := "writeShapingProbe" + string(rune('A'+i))
			src := c.annotation + "\nmutation ticket " + name + " {\n  args {\n    " + c.args + "\n  }\n" + c.block + "}\n"
			_, err := tryParseNewFunctionSyntax(name, "mutation", src, "test/mutations.memql", reg)
			if err == nil {
				t.Fatalf("loaded:\n%s", src)
			}
			var we *WriteAnnotationError
			if !errors.As(err, &we) || we.RuleCode() != c.code {
				t.Fatalf("want a %s refusal, got: %v", c.code, err)
			}
			if !strings.Contains(err.Error(), c.want) || !strings.HasSuffix(err.Error(), "["+c.code+"]") {
				t.Errorf("the refusal does not say %q with its code last: %v", c.want, err)
			}
		})
	}
}

// TestWriteShapingAnnotationsControls: the forms the annotations exist for
// load -- a list verb on a list, a merge on an object and on a nested block,
// createOnly and noUnset on fields the block writes -- and so does a block
// whose payload is one caller-supplied object, whose written fields are the
// caller's to decide.
func TestWriteShapingAnnotationsControls(t *testing.T) {
	reg := conceptRegistry(mustConcept(t, writeShapingConceptSrc, "v1/test/ticket"))
	cases := map[string]string{
		"createOnly": `@createOnly("status")
mutation ticket writeShapingOkCreateOnly {
  args {
    ticketId  string!
    title     string!
  }
  insert {
    id: args.ticketId
    title: args.title
    status: "open"
  }
}`,
		"lists": `@appendFields("labels")
mutation ticket writeShapingOkAppend {
  args {
    ticketId  string!
    labels    []string!
  }
  update {
    id: args.ticketId
    labels: args.labels
  }
}`,
		"a caller-supplied payload": `@createOnly("status", "attempts")
mutation ticket writeShapingOkSplat {
  args {
    ticketId  string!
    ticket    object!
  }
  insert {
    id: args.ticketId
    payload: args.ticket
    status: "open"
  }
}`,
		"merge": `@mergeFields("preferences", "settings")
@noUnset("preferences")
mutation ticket writeShapingOkMerge {
  args {
    ticketId     string!
    preferences  object!
    theme        string!
  }
  update {
    id: args.ticketId
    preferences: args.preferences
    settings: {
      theme: args.theme
    }
  }
}`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			fnName := strings.Fields(strings.SplitN(src, "mutation ticket ", 2)[1])[0]
			if _, err := tryParseNewFunctionSyntax(fnName, "mutation", src, "test/mutations.memql", reg); err != nil {
				t.Fatalf("refused: %v", err)
			}
		})
	}
}
