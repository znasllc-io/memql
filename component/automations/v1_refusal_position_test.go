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

// TestTerseAutomationRefusalNamesTheFileLine: a terse automation reaches the
// compiler as the lowering's longhand, found nowhere in the file; its
// refusals are placed against the one-line header the author wrote -- a
// retired form inside its @filter lambda at that token, a missing arg at the
// header -- where they used to be relative to the lowered slice.
func TestTerseAutomationRefusalNamesTheFileLine(t *testing.T) {
	file := `/// The first automation.
@trigger(event="node.created", concept="v1:probe:thing")
automation first {
  step run {
    logic noteThing(x: 1)
  }
}

/// Note a thing whose b is missing, written with the retired null.
automation onThing @trigger(event="node.created", concept="v1:probe:thing") @filter(row => row.b == null) => logic noteThing
`
	lowered, err := languageParser.NormaliseTerseAutomationSource(file)
	if err != nil {
		t.Fatal(err)
	}
	slices, _ := extractAutomationSlicesReporting(lowered)
	for _, s := range slices {
		if s.Name != "onThing" {
			continue
		}
		if strings.Contains(file, s.Source) {
			t.Fatal("the terse automation's slice is text of the file: this test would not reach the terse placement")
		}
		_, err := NewLoader(LoaderOptions{}).compileUnifiedSlice(file, s, "unified:probe/automations.memql:onThing")
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
	t.Fatal("no slice for the terse automation")
}

// TestTerseLoweringRefusalNamesTheFileLine: the unified loader's own terse
// lowering refuses an args block ahead of a terse automation at that block.
func TestTerseLoweringRefusalNamesTheFileLine(t *testing.T) {
	file := "/// A terse automation takes no args.\nargs {\n  x string\n}\nautomation onThing @trigger(event=\"node.created\", concept=\"v1:probe:thing\") => logic noteThing\n"
	_, err := languageParser.NormaliseTerseAutomationSource(file)
	if err == nil {
		t.Fatal("an args block ahead of a terse automation was lowered")
	}
	if got := languageParser.PositionRewriteError(file, err).Error(); !strings.HasPrefix(got, "rewrite error at line 2, column 1: ") {
		t.Fatalf("got %q, want the refusal placed at the args block, line 2 column 1", got)
	}
}
