package docsmd

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDescriptionSkipsBookkeepingAndJoinsWrappedLines(t *testing.T) {
	body := strings.Join([]string{
		"",
		"# Environment Variables",
		"",
		"**Audience:** engineers running MemQL.",
		"**Last updated:** 2026-04-25",
		"",
		"> **Last Updated:** September 13, 2026",
		"",
		"---",
		"",
		"The registry is the **authoritative** list of every",
		"variable MemQL reads, in `manifest.yaml`; see [the guide](../x.md).",
		"",
		"Second paragraph.",
	}, "\n")
	got := Description(body, 160)
	want := "The registry is the authoritative list of every variable MemQL reads, in manifest.yaml; see the guide."
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestDescriptionSkipsCodeListsTablesAndHTML(t *testing.T) {
	body := "# T\n\n```\nnot this\n```\n\n- a list\n\n| a | b |\n\n<div>x</div>\n\n![img](a.png)\n\nThis one.\n"
	if got := Description(body, 160); got != "This one." {
		t.Errorf("got %q", got)
	}
}

func TestDescriptionEndsAtTheFirstNonProseLine(t *testing.T) {
	body := "Opening line that\ncontinues here.\n- then a list\n"
	if got := Description(body, 160); got != "Opening line that continues here." {
		t.Errorf("got %q", got)
	}
}

func TestDescriptionIsEmptyWithNoProse(t *testing.T) {
	if got := Description("# Only a heading\n\n## And another\n\n- list\n", 160); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestDescriptionCutsOnASentenceOrAWord(t *testing.T) {
	sentence := strings.Repeat("word ", 20) + "ends here. " + strings.Repeat("more ", 30)
	got := Description(sentence, 160)
	if !strings.HasSuffix(got, "ends here.") {
		t.Errorf("a sentence end in the second half of the budget should be the cut: %q", got)
	}

	words := strings.Repeat("abcdefg ", 40)
	got = Description(words, 50)
	if utf8.RuneCountInString(got) > 50 {
		t.Errorf("longer than the budget: %d runes", utf8.RuneCountInString(got))
	}
	if !strings.HasSuffix(got, "abcdefg…") {
		t.Errorf("a word cut should end on a whole word and an ellipsis: %q", got)
	}
}
