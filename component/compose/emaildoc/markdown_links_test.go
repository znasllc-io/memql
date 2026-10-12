package emaildoc

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// markdownLinkCase is one text of the Markdown links corpus and whether
// Parse refuses it for its links (a link with no label, or an address the
// link rule does not allow).
type markdownLinkCase struct {
	Text    string `json:"text"`
	Refused bool   `json:"refused"`
}

// TestMarkdownLinkCorpus holds testdata/markdown-links.json to the parser's
// verdicts. The VS Code editor refuses the same links in place before an edit
// is sent, and its test (editors/productivity/test/email-markdown.test.ts)
// reads this file too, so the two stay in step. -update rewrites the verdicts
// from the parser; review the diff.
func TestMarkdownLinkCorpus(t *testing.T) {
	path := filepath.Join("testdata", "markdown-links.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cases []markdownLinkCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 200 {
		t.Fatalf("the corpus has %d texts; it needs at least 200", len(cases))
	}
	if *updateGolden {
		for i := range cases {
			cases[i].Refused = markdownProblem(cases[i].Text) != ""
		}
		if err := os.WriteFile(path, encodeMarkdownLinkCases(t, cases), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	refused := 0
	for _, c := range cases {
		got := markdownProblem(c.Text) != ""
		if got != c.Refused {
			t.Errorf("%q: the parser refuses it: %v, the corpus says %v (run with -update and review the diff)", c.Text, got, c.Refused)
		}
		if got {
			refused++
		}
	}
	if refused == 0 || refused == len(cases) {
		t.Errorf("the corpus needs both verdicts; %d of %d are refused", refused, len(cases))
	}
}

// encodeMarkdownLinkCases writes the corpus one case to a line, so a change
// to a verdict is a one-line diff.
func encodeMarkdownLinkCases(t *testing.T, cases []markdownLinkCase) []byte {
	t.Helper()
	var out bytes.Buffer
	out.WriteString("[\n")
	for i, c := range cases {
		var line bytes.Buffer
		encoder := json.NewEncoder(&line)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(c); err != nil {
			t.Fatal(err)
		}
		out.WriteString("  ")
		out.Write(bytes.TrimRight(line.Bytes(), "\n"))
		if i < len(cases)-1 {
			out.WriteString(",")
		}
		out.WriteString("\n")
	}
	out.WriteString("]\n")
	return out.Bytes()
}
