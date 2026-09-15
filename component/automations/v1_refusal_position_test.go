package automations

import (
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestAutomationSliceRefusalNamesTheFileLine: the unified loader compiles each
// automation from its slice of the file; a refusal inside the SECOND
// automation's @filter lambda names the file's line and column, not the
// slice's (memql#5364).
func TestAutomationSliceRefusalNamesTheFileLine(t *testing.T) {
	file := `/// The first automation.
@trigger(event="node.created", concept="v1:probe:thing")
automation first {
  run := logic noteThing(x: 1)
}

/// The second, whose filter holds the retired null.
@filter(row => row.b == null)
@trigger(event="node.created", concept="v1:probe:thing")
automation second {
  run := logic noteThing(x: 2)
}
`
	slices, _ := extractAutomationSlicesReporting(file)
	for _, s := range slices {
		if s.Name != "second" {
			continue
		}
		_, err := NewLoader(LoaderOptions{}).compileUnifiedSlice(file, s, "unified:probe/automations.memql:second")
		if err == nil {
			t.Fatal("an @filter holding the retired null compiled")
		}
		off := strings.Index(file, "== null") + len("== ")
		lineStart := strings.LastIndex(file[:off], "\n") + 1
		where := "at line " + strconv.Itoa(1+strings.Count(file[:off], "\n")) + ", column " + strconv.Itoa(1+utf8.RuneCountInString(file[lineStart:off])) + ":"
		if !strings.Contains(err.Error(), where) {
			t.Fatalf("got %v, want the refusal %s", err, where)
		}
		return
	}
	t.Fatal("no slice for the second automation")
}

// TestTerseHeaderRefusalNamesTheFileLine: the retired terse header is sliced
// as its line of the file, and the parser's refusal of it names that line.
func TestTerseHeaderRefusalNamesTheFileLine(t *testing.T) {
	// memqlmigrate:keep -- the retired terse header is the case.
	file := `/// The first automation.
@trigger(event="node.created", concept="v1:probe:thing")
automation first {
  run := logic noteThing(x: 1)
}

/// Note a thing, in the retired terse form.
automation onThing @trigger(event="node.created", concept="v1:probe:thing") => logic noteThing
`
	slices, _ := extractAutomationSlicesReporting(file)
	for _, s := range slices {
		if s.Name != "onThing" {
			continue
		}
		_, err := NewLoader(LoaderOptions{}).compileUnifiedSlice(file, s, "unified:probe/automations.memql:onThing")
		if err == nil || !strings.Contains(err.Error(), "body_terse_retired") {
			t.Fatalf("got %v, want the body_terse_retired refusal", err)
		}
		if !strings.Contains(err.Error(), "at line 8, column 1:") {
			t.Fatalf("got %v, want the refusal at the header's line, 8, column 1", err)
		}
		return
	}
	t.Fatal("no slice for the terse automation")
}
