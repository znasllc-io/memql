package main

import (
	"testing"

	protocol "github.com/tliron/glsp/protocol_3_16"
)

func rangeChange(sl, sc, el, ec int, text string) protocol.TextDocumentContentChangeEvent {
	return protocol.TextDocumentContentChangeEvent{
		Range: &protocol.Range{
			Start: protocol.Position{Line: protocol.UInteger(sl), Character: protocol.UInteger(sc)},
			End:   protocol.Position{Line: protocol.UInteger(el), Character: protocol.UInteger(ec)},
		},
		Text: text,
	}
}

func TestDocumentStore_OpenGetClose(t *testing.T) {
	s := newDocumentStore()
	const uri = "file:///a.memql"
	if _, ok := s.get(uri); ok {
		t.Fatal("expected no document before open")
	}
	s.open(uri, "concept x {}")
	got, ok := s.get(uri)
	if !ok || got != "concept x {}" {
		t.Fatalf("get after open = (%q,%v); want (%q,true)", got, ok, "concept x {}")
	}
	s.closeDoc(uri)
	if _, ok := s.get(uri); ok {
		t.Fatal("expected no document after close")
	}
}

func TestDocumentStore_WholeDocumentChange(t *testing.T) {
	s := newDocumentStore()
	const uri = "file:///a.memql"
	s.open(uri, "old text")
	got, ok := s.applyChanges(uri, []any{
		protocol.TextDocumentContentChangeEventWhole{Text: "brand new text"},
	})
	if !ok || got != "brand new text" {
		t.Fatalf("whole-document change = (%q,%v); want (%q,true)", got, ok, "brand new text")
	}
	if stored, _ := s.get(uri); stored != "brand new text" {
		t.Errorf("stored = %q; want %q", stored, "brand new text")
	}
}

func TestDocumentStore_IncrementalRangeChange(t *testing.T) {
	s := newDocumentStore()
	const uri = "file:///a.memql"
	s.open(uri, "hello world")
	// Replace "world" (characters 6..11 on line 0) with "there".
	got, ok := s.applyChanges(uri, []any{rangeChange(0, 6, 0, 11, "there")})
	if !ok || got != "hello there" {
		t.Fatalf("range change = (%q,%v); want (%q,true)", got, ok, "hello there")
	}
}

func TestDocumentStore_IncrementalRangeChangeAcrossAstral(t *testing.T) {
	s := newDocumentStore()
	const uri = "file:///a.memql"
	// 'b' sits after the emoji, which spans UTF-16 code units 1..3, so 'b' is
	// at character 3. This exercises glsp's UTF-16-correct byte resolution.
	s.open(uri, "a😀b")
	got, ok := s.applyChanges(uri, []any{rangeChange(0, 3, 0, 4, "Z")})
	if !ok || got != "a😀Z" {
		t.Fatalf("astral range change = (%q,%v); want (%q,true)", got, ok, "a😀Z")
	}
}

func TestDocumentStore_MultipleChangesInOrder(t *testing.T) {
	s := newDocumentStore()
	const uri = "file:///a.memql"
	s.open(uri, "abcdef")
	// First delete "ab" (0..2 -> ""), then, against the resulting "cdef",
	// replace "cd" (0..2) with "XY".
	got, ok := s.applyChanges(uri, []any{
		rangeChange(0, 0, 0, 2, ""),
		rangeChange(0, 0, 0, 2, "XY"),
	})
	if !ok || got != "XYef" {
		t.Fatalf("ordered changes = (%q,%v); want (%q,true)", got, ok, "XYef")
	}
}

func TestDocumentStore_OutOfRangeRangeClampsInsteadOfCorrupting(t *testing.T) {
	// Regression: an out-of-range ranged edit (character past a no-trailing-
	// newline line, or a line past EOF) must clamp to the line/document end, not
	// silently corrupt the buffer. glsp's Range.IndexesIn returns a 0 sentinel
	// for these, which -- spliced through -- would wipe or relocate content.
	cases := []struct {
		name  string
		start string
		ch    protocol.TextDocumentContentChangeEvent
		want  string
	}{
		{"char past final line -> append at end", "abc", rangeChange(0, 3, 0, 10, "X"), "abcX"},
		{"char past end mid-doc", "hello", rangeChange(0, 2, 0, 99, "WORLD"), "heWORLD"},
		{"end char past last line", "ab\ncd", rangeChange(1, 0, 1, 9, "X"), "ab\nX"},
		{"end line past EOF selects to doc end", "abc\ndef", rangeChange(0, 0, 5, 0, "Z"), "Z"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newDocumentStore()
			const uri = "file:///r.memql"
			s.open(uri, c.start)
			got, ok := s.applyChanges(uri, []any{c.ch})
			if !ok || got != c.want {
				t.Fatalf("applyChanges(%q) = (%q,%v); want (%q,true)", c.start, got, ok, c.want)
			}
		})
	}
}

func TestDocumentStore_ChangeOnUnopenedDocument(t *testing.T) {
	s := newDocumentStore()
	if _, ok := s.applyChanges("file:///missing.memql", []any{
		protocol.TextDocumentContentChangeEventWhole{Text: "x"},
	}); ok {
		t.Error("expected applyChanges on an unopened document to report ok=false")
	}
}

func TestDocumentStore_MultilineRangeChange(t *testing.T) {
	s := newDocumentStore()
	const uri = "file:///a.memql"
	s.open(uri, "line one\nline two\nline three")
	// Replace from line 1 char 5 (start of "two") through line 2 char 4 (the
	// space before "three") -- a cross-line range -- with "TWO/THREE". The tail
	// " three" (line 2 from char 4) is preserved.
	got, ok := s.applyChanges(uri, []any{rangeChange(1, 5, 2, 4, "TWO/THREE")})
	if !ok || got != "line one\nline TWO/THREE three" {
		t.Fatalf("multiline range change = %q; want %q", got, "line one\nline TWO/THREE three")
	}
}
