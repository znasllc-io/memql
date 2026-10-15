package campaigns

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// result_test.go -- the payload of a builtin's answer is plain JSON.
//
// encoding/json writes each "<", ">" and "&" of a string as six bytes
// (<), a safeguard for JSON that is placed inside HTML. An answer is not,
// and the answer to the preview of an email is HTML, full of them: a text part
// made of "<" would be six times the size it carries, written inside the render
// slot and read again by the engine and the wire outside it.

func TestBuiltinResultsAreWrittenAsPlainJSON(t *testing.T) {
	t.Run("a result", func(t *testing.T) {
		nodes, err := resultNode("probe", map[string]any{"html": `<p class="a">A & B > C</p>`, "n": 1})
		if err != nil {
			t.Fatal(err)
		}
		// The bytes themselves: what JSON needs escaped is, and nothing more, and
		// there is no newline after them.
		if want := `{"html":"<p class=\"a\">A & B > C</p>","n":1}`; string(nodes[0].Payload) != want {
			t.Errorf("payload = %s, want %s", nodes[0].Payload, want)
		}
	})

	t.Run("a list of rows", func(t *testing.T) {
		nodes, err := resultRows("probe", []map[string]any{
			{"id": "a", "document": map[string]any{"html": "<table><tr><td>A & B</td></tr></table>"}},
			{"id": "b", "name": "<b>"},
		})
		if err != nil {
			t.Fatal(err)
		}
		want := []string{
			`{"document":{"html":"<table><tr><td>A & B</td></tr></table>"},"id":"a"}`,
			`{"id":"b","name":"<b>"}`,
		}
		for i, node := range nodes {
			if string(node.Payload) != want[i] {
				t.Errorf("row %d = %s, want %s", i, node.Payload, want[i])
			}
		}
	})

	t.Run("what JSON needs escaped still is", func(t *testing.T) {
		value := "quote \" backslash \\ newline \n tab \t nul \x00 separator  "
		nodes, err := resultNode("probe", map[string]any{"v": value})
		if err != nil {
			t.Fatal(err)
		}
		if bytes.ContainsAny(nodes[0].Payload, "\n\t\x00") || bytes.Contains(nodes[0].Payload, []byte(" ")) {
			t.Errorf("a character JSON cannot hold raw is raw in %q", nodes[0].Payload)
		}
		var back map[string]any
		if err := json.Unmarshal(nodes[0].Payload, &back); err != nil || back["v"] != value {
			t.Errorf("the payload does not read back as what was written: %v, %q", err, back["v"])
		}
	})

	t.Run("a megabyte of markup is a megabyte", func(t *testing.T) {
		markup := strings.Repeat("<&>", 1<<20/3)
		nodes, err := resultNode("probe", map[string]any{"html": markup})
		if err != nil {
			t.Fatal(err)
		}
		if got, limit := len(nodes[0].Payload), len(markup)+64; got > limit {
			t.Errorf("a payload of %d bytes for %d bytes of markup (HTML escaping makes it %d)", got, len(markup), 6*len(markup))
		}
	})

	t.Run("an answer that cannot be written is an error", func(t *testing.T) {
		if _, err := resultNode("probe", map[string]any{"c": make(chan int)}); err == nil {
			t.Error("a channel was written as JSON")
		}
		if _, err := resultRows("probe", []map[string]any{{"id": "a", "c": make(chan int)}}); err == nil {
			t.Error("a channel was written as JSON in a row")
		}
	})
}
