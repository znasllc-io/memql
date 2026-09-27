package memql

import (
	"strings"
	"testing"
	"testing/fstest"
)

// TestBootRefusesAUseLineBelowAConstruct (memql#5426): a `use` line is
// file-top. The parser refuses one below a construct, so memqllint's import
// pass (dslimports.Load parses each file whole) refused it -- but no boot
// loader parses a file whole, and boot loaded it clean. The contract gate
// refuses it at load now, naming the file, the line, the module and the rule
// id; the same file with the line moved up loads.
func TestBootRefusesAUseLineBelowAConstruct(t *testing.T) {
	queries := func(late bool) string {
		head, tail := "use probeuse.concepts.{ ticket }\n\n", ""
		if late {
			head, tail = "", "\nuse probeuse.concepts.{ ticket }\n"
		}
		return head + `/// Open tickets.
@unbounded("a probe")
query ticket probeUseOpenTickets {
  filter row => row.status == "open"
}
` + tail
	}
	tree := func(late bool) fstest.MapFS {
		return withLanguageLines(fstest.MapFS{
			"probeuse/concepts.memql": {Data: []byte("/// A ticket.\nconcept ticket {\n  status  string\n}\n")},
			"probeuse/queries.memql":  {Data: []byte(queries(late))},
		})
	}

	diags, _, err := LintUnifiedTree(nil, tree(true))
	if err != nil {
		t.Fatalf("LintUnifiedTree: %v", err)
	}
	var found string
	for _, d := range diags {
		if strings.Contains(d.Message, "[use_not_file_top]") {
			found = d.File + ": " + d.Message
		}
	}
	for _, want := range []string{
		"probeuse/queries.memql: ",
		`use "probeuse.concepts" (contract-gate:use-not-file-top): line 7: a use line must come before the file's first construct`,
	} {
		if !strings.Contains(found, want) {
			t.Fatalf("boot did not refuse the late use line as %q; diagnostics: %v", want, diags)
		}
	}

	clean, _, err := LintUnifiedTree(nil, tree(false))
	if err != nil {
		t.Fatalf("LintUnifiedTree: %v", err)
	}
	if len(clean) != 0 {
		t.Fatalf("the file-top use line drew diagnostics: %v", clean)
	}
}
