package sense

import (
	"strings"
	"testing"
)

func ctxAtEnd(source string) CursorContext {
	lines := strings.Split(source, "\n")
	return analyzeCursorContext(source, len(lines), len(lines[len(lines)-1])+1)
}

// #2626: the ONE enclosure answer, populated on every context.
func TestResolveEnclosingConstruct(t *testing.T) {
	cases := []struct {
		name, src, wantKeyword, wantReceiver string
		wantBlocks                           []string
	}{
		{
			name: "query body", wantKeyword: "query", wantReceiver: "Query",
			src: "@actor\nquery todo todos {\n  filter todo.done == false\n  ",
		},
		{
			name: "query args block", wantKeyword: "query", wantReceiver: "Query",
			wantBlocks: []string{"args"},
			src:        "query todo todos {\n  args {\n    ",
		},
		{
			name: "mutate insert stamp chain", wantKeyword: "mutate", wantReceiver: "Mutation",
			wantBlocks: []string{"insert", "stamp"},
			src:        "mutate todo createTodo {\n  insert {\n    stamp {\n      ",
		},
		{
			name: "automation step", wantKeyword: "automation", wantReceiver: "Automation",
			wantBlocks: []string{"step"},
			src:        "@trigger(event=\"x.y\")\nautomation onThing {\n  step run {\n    ",
		},
		{
			name: "logic body", wantKeyword: "logic", wantReceiver: "Logic",
			wantBlocks: []string{"body"},
			src:        "logic compute {\n  body {\n    ",
		},
		{
			name: "concept body", wantKeyword: "concept",
			src: "@namespace(\"probe\")\nconcept widget {\n  ",
		},
		{
			name: "shape body", wantKeyword: "shape",
			src: "@actor\nshape actorEnvelope {\n  ",
		},
		{
			name: "closed construct leaves no enclosure",
			src:  "query todo todos {\n  filter todo.done == false\n}\n",
		},
	}
	for _, tc := range cases {
		ctx := ctxAtEnd(tc.src)
		if ctx.Enclosing.Keyword != tc.wantKeyword {
			t.Errorf("%s: keyword = %q, want %q", tc.name, ctx.Enclosing.Keyword, tc.wantKeyword)
		}
		if tc.wantReceiver != "" && ctx.ReceiverType != tc.wantReceiver {
			t.Errorf("%s: ReceiverType = %q, want %q", tc.name, ctx.ReceiverType, tc.wantReceiver)
		}
		if strings.Join(ctx.Enclosing.Blocks, "/") != strings.Join(tc.wantBlocks, "/") {
			t.Errorf("%s: blocks = %v, want %v", tc.name, ctx.Enclosing.Blocks, tc.wantBlocks)
		}
	}
}

// A top-level @ PRECEDES its construct: the receiver is the next header
// below the cursor; at EOF there is none (union fallback preserved).
func TestPreambleAnnotationReceiver(t *testing.T) {
	src := "@\nquery todo todos {\n  filter todo.done == false\n}\n"
	ctx := analyzeCursorContext(src, 1, 2)
	if ctx.ReceiverType != "Query" {
		t.Errorf("preamble @ above a query: ReceiverType = %q, want Query", ctx.ReceiverType)
	}
	if !ctx.Enclosing.Preamble {
		t.Error("preamble resolution must be marked")
	}

	// Second construct below: the NEAREST header wins.
	src2 := "query todo a {\n}\n\n@\nmutate todo b {\n}\n"
	ctx = analyzeCursorContext(src2, 4, 2)
	if ctx.ReceiverType != "Mutation" {
		t.Errorf("nearest header below must win, got %q", ctx.ReceiverType)
	}

	// EOF: no header follows -> no receiver -> union fallback.
	ctx = analyzeCursorContext("query todo a {\n}\n\n@", 4, 2)
	if ctx.ReceiverType != "" {
		t.Errorf("@ at EOF must yield no receiver, got %q", ctx.ReceiverType)
	}
}

// The story's engaged-filter smoke assertion: with ReceiverType finally
// populated, @ above a query stops offering mutation-only annotations.
func TestAnnotationFilterEngaged(t *testing.T) {
	s := New(&fakeRegistry{})
	src := "@\nquery todo todos {\n  filter todo.done == false\n}\n"
	items := s.Complete(src, 1, 2, "probe.memql")
	got := map[string]bool{}
	for _, it := range items {
		got[strings.TrimPrefix(it.Label, "@")] = true
	}
	if got["mergeFields"] || got["createOnly"] || got["scrubPii"] {
		t.Errorf("query preamble must not offer mutation-only annotations: %v", got)
	}
	for _, want := range []string{"description", "cache", "actor"} {
		if !got[want] {
			t.Errorf("query preamble must offer %q, got %v", want, got)
		}
	}

	// The mutation side still gets its own set.
	src = "@\nmutate todo createTodo {\n  insert {\n    accept { title }\n  }\n}\n"
	items = s.Complete(src, 1, 2, "probe.memql")
	got = map[string]bool{}
	for _, it := range items {
		got[strings.TrimPrefix(it.Label, "@")] = true
	}
	if !got["mergeFields"] {
		t.Errorf("mutation preamble must offer mergeFields, got %v", got)
	}
}
