package docsmd

import "testing"

func TestParseTrimsQuotesTheWayTheGateDoes(t *testing.T) {
	// The bug a shared parser removes: the gate trimmed these quotes and
	// passed the page; the old bundler compared the quoted string and dropped
	// it without a word.
	content := "---\ntitle: \"Quoted: with a colon\"\naudience: 'public'\nstatus: stable\nno colon here\n---\n\n# Body\n"
	fm, body, ok := Parse(content)
	if !ok {
		t.Fatal("a closed block was not parsed")
	}
	if fm["title"] != "Quoted: with a colon" {
		t.Errorf("title = %q; the value is split on the FIRST colon and its quotes trimmed", fm["title"])
	}
	if fm["audience"] != "public" {
		t.Errorf("audience = %q, want public", fm["audience"])
	}
	if _, present := fm["no colon here"]; present {
		t.Error("a line with no colon became a key")
	}
	if body != "\n# Body\n" {
		t.Errorf("body = %q", body)
	}
}

func TestParseReadsCRLF(t *testing.T) {
	fm, body, ok := Parse("---\r\ntitle: X\r\narea: ai\r\n---\r\nText\r\n")
	if !ok || fm["title"] != "X" || fm["area"] != "ai" {
		t.Fatalf("CRLF block: ok=%v fm=%v", ok, fm)
	}
	if body != "Text\r\n" {
		t.Errorf("body = %q", body)
	}
}

func TestParseRefusesAnOpenOrMissingBlock(t *testing.T) {
	for _, content := range []string{
		"",
		"---",
		"---\ntitle: X\n",
		"# No front matter\n---\n",
		" ---\ntitle: X\n---\n",
	} {
		if fm, body, ok := Parse(content); ok || fm != nil || body != content {
			t.Errorf("Parse(%q) = %v, %q, %v; want nil, the content, false", content, fm, body, ok)
		}
	}
}

func TestSplitKeepsTheBlockAsWritten(t *testing.T) {
	content := "---\ntitle:  spaced   \n---\nbody\n"
	front, body, ok := Split(content)
	if !ok || front != "---\ntitle:  spaced   \n---\n" || body != "body\n" {
		t.Fatalf("Split = %q, %q, %v", front, body, ok)
	}
	front, body, ok = Split("---\n---")
	if !ok || front != "---\n---" || body != "" {
		t.Fatalf("an empty block at EOF: %q, %q, %v", front, body, ok)
	}
}
