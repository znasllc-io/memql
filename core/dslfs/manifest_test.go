package dslfs

import (
	"errors"
	"strings"
	"testing"
)

func TestParseManifestReadsTheTwoLines(t *testing.T) {
	cases := map[string]string{
		"canonical":             "memql = \"1.0\"\nedition = \"2026\"\n",
		"comments and blanks":   "# The language this domain is written in.\n\nmemql = \"1.0\"   # the line\n\nedition = \"2026\"\n",
		"reordered":             "edition = \"2026\"\nmemql = \"1.0\"",
		"crlf line endings":     "memql = \"1.0\"\r\nedition = \"2026\"\r\n",
		"byte-order mark":       "\xef\xbb\xbfmemql = \"1.0\"\nedition = \"2026\"\n",
		"aligned equals signs":  "memql   = \"1.0\"\nedition = \"2026\"\n",
		"tabs around the equal": "memql\t=\t\"1.0\"\nedition = \"2026\"\n",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			m, err := ParseManifest([]byte(src))
			if err != nil {
				t.Fatalf("ParseManifest: %v", err)
			}
			if m.Language != "1.0" || m.Edition != "2026" {
				t.Errorf("got %+v, want language 1.0 and edition 2026", m)
			}
		})
	}
}

// A missing key is not a parse error: the loader, which knows the engine's own
// line, is the one that says which line to add.
func TestParseManifestLeavesAMissingKeyEmpty(t *testing.T) {
	m, err := ParseManifest([]byte("edition = \"2026\"\n"))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if m.Language != "" || m.Edition != "2026" {
		t.Errorf("got %+v", m)
	}
	empty, err := ParseManifest(nil)
	if err != nil || empty != (Manifest{}) {
		t.Errorf("empty file: got %+v, %v", empty, err)
	}
}

// Every refusal names its line and says what to write instead.
func TestParseManifestRefusesAnythingElse(t *testing.T) {
	cases := []struct {
		name, src string
		line      int
		want      string
	}{
		{"third key", "memql = \"1.0\"\nedition = \"2026\"\nname = \"x\"\n", 3, `"name" is not a key`},
		{"table", "[package]\nmemql = \"1.0\"\n", 1, "table header"},
		{"unquoted value", "memql = 1.0\n", 1, "double quotes"},
		{"single quotes", "memql = '1.0'\n", 1, "double quotes"},
		{"escape in value", "memql = \"1\\.0\"\n", 1, "double quotes"},
		{"key twice", "memql = \"1.0\"\n\nmemql = \"1.1\"\n", 3, "declared twice (lines 1 and 3)"},
		{"no equals", "memql \"1.0\"\n", 1, "key = \"value\""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseManifest([]byte(c.src))
			var me *ManifestError
			if !errors.As(err, &me) {
				t.Fatalf("ParseManifest(%q) = %v, want a *ManifestError", c.src, err)
			}
			if me.Line != c.line {
				t.Errorf("refusal on line %d, want %d: %v", me.Line, c.line, err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("refusal %q does not say %q", err, c.want)
			}
			if !strings.HasPrefix(err.Error(), ManifestFile+" line ") {
				t.Errorf("refusal %q does not name the file and line", err)
			}
		})
	}
}

func TestRenderRoundTrips(t *testing.T) {
	want := Manifest{Language: "1.0", Edition: "2026"}
	text := want.Render()
	if text != "memql = \"1.0\"\nedition = \"2026\"\n" {
		t.Errorf("Render = %q", text)
	}
	got, err := ParseManifest([]byte(text))
	if err != nil || got != want {
		t.Errorf("round trip: got %+v, %v", got, err)
	}
}
