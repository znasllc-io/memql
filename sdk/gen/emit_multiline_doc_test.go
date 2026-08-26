package gen

import (
	"bytes"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// A `///` doc comment on an args field may span more than one paragraph, and
// it arrives here with the newlines intact. Prefixing only the first line
// emitted bare prose into a struct body -- Go that does not compile.
//
// The failure was invisible in exactly the way that matters: `make
// sdk-gen-check` regenerates and DIFFS, so it reported no drift while the file
// it had just written was unbuildable, and the only signal was a warning line
// from a DIFFERENT tool (memql-arch) that happened to parse the SDK.
//
// So this test PARSES what the emitter produced rather than reading it.
func TestMultiLineArgDocsEmitParseableGo(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString("package client\n\ntype Args struct {\n")
	writeArgField(&buf, ArgField{
		Name: "tokenSpentLocal",
		Type: "int",
		Description: "First paragraph explaining the field.\n" +
			"\n" +
			"Second paragraph, which used to land in the struct body as bare prose\n" +
			"and produce \"unexpected name in struct type\".",
	})
	buf.WriteString("}\n")

	if _, err := parser.ParseFile(token.NewFileSet(), "generated.go", buf.String(), parser.AllErrors); err != nil {
		t.Fatalf("the emitted struct does not parse: %v\n---\n%s", err, buf.String())
	}
	for i, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "Second paragraph") {
			t.Fatalf("line %d is uncommented prose inside a struct body: %q", i+1, line)
		}
	}
}

// The TypeScript side has the same shape and the same fix: one /** */ per
// line, because a raw newline inside a one-line JSDoc block reads as
// unterminated to a human even where the parser tolerates it.
func TestMultiLineArgDocsEmitOneJSDocPerLine(t *testing.T) {
	var buf bytes.Buffer
	emitTSArgField(&buf, ArgField{
		Name:        "tokenSpentLocal",
		Type:        "int",
		Description: "First paragraph.\n\nSecond paragraph.",
	})
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "/**") && !strings.HasPrefix(trimmed, "tokenSpentLocal") {
			continue
		}
		if strings.HasPrefix(trimmed, "/**") && !strings.HasSuffix(trimmed, "*/") {
			t.Fatalf("an unterminated JSDoc line was emitted: %q", line)
		}
	}
}
