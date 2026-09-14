package automations

import (
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// TestAutomationSliceRefusalNamesTheFileLine: the unified loader compiles each
// automation from its slice of the file; a refusal inside the SECOND
// automation's @filter lambda names the file's line and column, not the
// slice's (memql#5364).
func TestAutomationSliceRefusalNamesTheFileLine(t *testing.T) {
	file := `/// The first automation.
@trigger(event="node.created", concept="v1:probe:thing")
automation first {
  step run {
    logic noteThing(x: 1)
  }
}

/// The second, whose filter holds the retired null.
@filter(row => row.b == null)
@trigger(event="node.created", concept="v1:probe:thing")
automation second {
  step run {
    logic noteThing(x: 2)
  }
}
`
	lowered, err := languageParser.NormaliseTerseAutomationSource(file)
	if err != nil {
		t.Fatal(err)
	}
	slices, _ := extractAutomationSlicesReporting(lowered)
	for _, s := range slices {
		if s.Name != "second" {
			continue
		}
		_, err := NewLoader(LoaderOptions{}).compileMemQL(anchoredAutomationSlice(file, s.Source), "unified:probe/automations.memql:second")
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
