package memoryNodes

import (
	"strings"
	"testing"
)

func TestConceptServerWrittenDeclaration(t *testing.T) {
	for _, annotated := range []bool{false, true} {
		source := "concept receipt {\n value string\n}\n"
		if annotated {
			source = "@serverWritten\n" + source
		}
		c, err := buildConcept(t, source)
		if err != nil || c.ServerWritten != annotated {
			t.Fatalf("annotated=%v: concept=%+v, error=%v", annotated, c, err)
		}
	}
	for _, annotation := range []string{"@serverWritten(\"yes\")", "@serverWritten\n@serverWritten"} {
		_, err := buildConcept(t, annotation+"\nconcept receipt {\n value string\n}\n")
		if err == nil || !strings.Contains(err.Error(), "serverWritten") {
			t.Fatalf("malformed declaration %q accepted: %v", annotation, err)
		}
	}
}
