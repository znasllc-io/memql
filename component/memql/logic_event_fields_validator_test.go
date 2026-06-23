package memql

import (
	"strings"
	"testing"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// parseLogicForTest runs the same NormaliseAll + parse path the loader uses and
// returns the FunctionDef for the named logic plus the raw (struct-form) source.
func parseLogicForTest(t *testing.T, src, name string) (*languageParser.FunctionDef, string) {
	t.Helper()
	content := src
	if rw, err := languageParser.NormaliseAll(src); err == nil {
		content = rw
	}
	lexer := languageParser.NewLexer(content)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("lex: %v", err)
	}
	node, err := languageParser.NewParser(tokens).Parse()
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	file := node.(*languageParser.File)
	for _, d := range file.Definitions {
		if fd, ok := d.(*languageParser.FunctionDef); ok && fd.Name == name {
			// The loader validates against the NORMALISED source (the
			// `func (Logic) ... { ... }` body form extractFunctionBody reads),
			// so the test must pass `content`, not the struct-form `src`.
			return fd, content
		}
	}
	t.Fatalf("logic %q not found", name)
	return nil, ""
}

func TestValidateLogicEventFields_AcceptsDeclared(t *testing.T) {
	src := `@enabled
@eventField("partitionId", "siParticipantId")
logic logicOk {
  args { event object @required }
  body {
    return noop({ a: args.event.payload.partitionId, b: args.event.payload.siParticipantId })
  }
}`
	fd, raw := parseLogicForTest(t, src, "logicOk")
	if err := validateLogicEventFields(raw, fd); err != nil {
		t.Fatalf("declared fields should pass, got %v", err)
	}
}

func TestValidateLogicEventFields_RejectsUnknown(t *testing.T) {
	src := `@enabled
@eventField("partitionId")
logic logicBad {
  args { event object @required }
  body {
    return noop({ a: args.event.payload.partitionId, b: args.event.payload.typooo })
  }
}`
	fd, raw := parseLogicForTest(t, src, "logicBad")
	err := validateLogicEventFields(raw, fd)
	if err == nil {
		t.Fatal("an undeclared event payload field must be rejected")
	}
	if !strings.Contains(err.Error(), "typooo") {
		t.Fatalf("error should name the offending field, got %v", err)
	}
}

func TestValidateLogicEventFields_OptOutWhenNoAnnotation(t *testing.T) {
	// No @eventField -> not field-validated, even with novel payload refs.
	src := `@enabled
logic logicUnannotated {
  args { event object @required }
  body {
    return noop({ a: args.event.payload.anythingGoes })
  }
}`
	fd, raw := parseLogicForTest(t, src, "logicUnannotated")
	if err := validateLogicEventFields(raw, fd); err != nil {
		t.Fatalf("unannotated logic must not be field-validated, got %v", err)
	}
}

func TestValidateLogicEventFields_NestedAndComments(t *testing.T) {
	// `event.payload.node.type` validates the head `node`; a commented-out ref
	// to an undeclared field must NOT trip the check.
	src := `@enabled
@eventField("node")
logic logicNested {
  args { event object @required }
  body {
    // historical: args.event.payload.removedField was dropped
    return noop({ t: args.event.payload.node.type })
  }
}`
	fd, raw := parseLogicForTest(t, src, "logicNested")
	if err := validateLogicEventFields(raw, fd); err != nil {
		t.Fatalf("nested head + commented ref should pass, got %v", err)
	}
}

func TestValidateLogicEventFields_PayloadPrefixedDeclaration(t *testing.T) {
	// `@eventField("payload.partitionId")` normalizes to head `partitionId`.
	src := `@enabled
@eventField("payload.partitionId")
logic logicPrefixed {
  args { event object @required }
  body { return noop({ a: args.event.payload.partitionId }) }
}`
	fd, raw := parseLogicForTest(t, src, "logicPrefixed")
	if err := validateLogicEventFields(raw, fd); err != nil {
		t.Fatalf("payload.-prefixed declaration should accept the bare ref, got %v", err)
	}
}

func TestNormalizeEventFieldHead(t *testing.T) {
	cases := map[string]string{
		"partitionId":          "partitionId",
		"payload.partitionId":  "partitionId",
		"payload.node.type": "node",
		"  partitionId  ":      "partitionId",
	}
	for in, want := range cases {
		if got := normalizeEventFieldHead(in); got != want {
			t.Errorf("normalizeEventFieldHead(%q)=%q want %q", in, got, want)
		}
	}
}
