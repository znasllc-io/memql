package sense

// Tests for segment-aware completion on the `use` line (#2732): typing a
// namespace then a dot offers the module kinds, and the brace list offers that
// module's importable ids -- instead of dumping the whole symbol table.

import (
	"strings"
	"testing"
)

func completeAtEnd(s *Service, src string) map[string]bool {
	lines := strings.Split(src, "\n")
	return labelsOfItems(s.Complete(src, len(lines), len(lines[len(lines)-1])+1, "probe.memql"))
}

func useGraph() fakeGraph {
	return fakeGraph{
		nsList: []string{"fylo", "platform"},
		kinds:  map[string][]string{"fylo": {"concepts", "queries", "mutations"}},
		moduleSyms: map[string][]string{
			"fylo.concepts": {"order", "scanEvent", "customer"},
		},
	}
}

func TestUseCompletion_Namespaces(t *testing.T) {
	got := completeAtEnd(NewWithWorkspace(nil, useGraph()), "use ")
	for _, want := range []string{"fylo", "platform"} {
		if !got[want] {
			t.Errorf("`use ` should offer namespace %q, got %v", want, got)
		}
	}
	// Not a dump: no construct keywords / concepts leak in.
	for _, never := range []string{"order", "concepts", "mutate", "query"} {
		if got[never] {
			t.Errorf("`use ` must not offer %q (dumped)", never)
		}
	}
}

func TestUseCompletion_NamespacePrefixFilter(t *testing.T) {
	got := completeAtEnd(NewWithWorkspace(nil, useGraph()), "use fy")
	if !got["fylo"] || got["platform"] {
		t.Errorf("`use fy` should offer only fylo, got %v", got)
	}
}

func TestUseCompletion_KindsAfterDot(t *testing.T) {
	// The user's symptom 3: typing `fylo.` must offer the kinds, not everything.
	got := completeAtEnd(NewWithWorkspace(nil, useGraph()), "use fylo.")
	for _, want := range []string{"concepts", "queries", "mutations"} {
		if !got[want] {
			t.Errorf("`use fylo.` should offer kind %q, got %v", want, got)
		}
	}
	for _, never := range []string{"fylo", "platform", "order"} {
		if got[never] {
			t.Errorf("`use fylo.` must not offer %q", never)
		}
	}
}

func TestUseCompletion_KindPrefixFilter(t *testing.T) {
	got := completeAtEnd(NewWithWorkspace(nil, useGraph()), "use fylo.con")
	if !got["concepts"] || got["queries"] {
		t.Errorf("`use fylo.con` should offer only concepts, got %v", got)
	}
}

func TestUseCompletion_ImportListIds(t *testing.T) {
	got := completeAtEnd(NewWithWorkspace(nil, useGraph()), "use fylo.concepts.{ ")
	for _, want := range []string{"order", "scanEvent", "customer"} {
		if !got[want] {
			t.Errorf("brace list should offer id %q, got %v", want, got)
		}
	}
}

func TestUseCompletion_ImportListExcludesListed(t *testing.T) {
	got := completeAtEnd(NewWithWorkspace(nil, useGraph()), "use fylo.concepts.{ order, ")
	if got["order"] {
		t.Errorf("already-listed id `order` must not be re-offered, got %v", got)
	}
	if !got["scanEvent"] || !got["customer"] {
		t.Errorf("remaining ids should still be offered, got %v", got)
	}
}

// The regression that IS the bug: the open brace must no longer classify as a
// function body and dump the whole symbol table.
func TestUseCompletion_BraceIsNotFuncBodyDump(t *testing.T) {
	ctx := analyzeCursorContext("use fylo.concepts.{ ", 1, len("use fylo.concepts.{ ")+1)
	if ctx.Kind != ContextUseImportList {
		t.Fatalf("`use fylo.concepts.{ ` classified as %v, want ContextUseImportList (not the func-body dump)", ctx.Kind)
	}
	if ctx.UseNamespace != "fylo" || ctx.UseKind != "concepts" {
		t.Errorf("classified module = %q.%q, want fylo.concepts", ctx.UseNamespace, ctx.UseKind)
	}
}

func TestUseCompletion_ClassificationSegments(t *testing.T) {
	cases := []struct {
		src      string
		wantKind ContextKind
		ns, kind string
	}{
		{"use ", ContextUseNamespace, "", ""},
		{"use fy", ContextUseNamespace, "", ""},
		{"use fylo.", ContextUseKind, "fylo", ""},
		{"use fylo.con", ContextUseKind, "fylo", ""},
		{"use fylo.concepts.{ ", ContextUseImportList, "fylo", "concepts"},
	}
	for _, c := range cases {
		ctx := analyzeCursorContext(c.src, 1, len(c.src)+1)
		if ctx.Kind != c.wantKind {
			t.Errorf("%q -> kind %v, want %v", c.src, ctx.Kind, c.wantKind)
			continue
		}
		if c.ns != "" && ctx.UseNamespace != c.ns {
			t.Errorf("%q -> ns %q, want %q", c.src, ctx.UseNamespace, c.ns)
		}
		if c.kind != "" && ctx.UseKind != c.kind {
			t.Errorf("%q -> kind %q, want %q", c.src, ctx.UseKind, c.kind)
		}
	}
}
