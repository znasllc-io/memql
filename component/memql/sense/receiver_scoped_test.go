package sense

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/annotations"
)

// #2627: @ offers only the enclosing/following construct's annotations.
func TestReceiverFilteredAnnotations(t *testing.T) {
	s := New(&fakeRegistry{})
	cases := []struct {
		name, src    string
		line, col    int
		want, absent []string
	}{
		{
			name: "query preamble", src: "@\nquery todo todos {\n}\n", line: 1, col: 2,
			want: []string{"cache", "unbounded", "actor"}, absent: []string{"mergeFields", "trigger", "handler"},
		},
		{
			name: "mutation preamble", src: "@\nmutate todo createTodo {\n}\n", line: 1, col: 2,
			want: []string{"mergeFields", "createOnly", "actor"}, absent: []string{"cache", "trigger"},
		},
		{
			name: "automation preamble", src: "@\nautomation onThing {\n}\n", line: 1, col: 2,
			want: []string{"trigger", "filter"}, absent: []string{"mergeFields", "cache"},
		},
		{
			name: "concept preamble uses the \"\" receiver", src: "@\nconcept widget {\n}\n", line: 1, col: 2,
			// NOTE: @cache IS legal on a concept (the registry's ""
			// receiver carries it); the absent list must not invent
			// restrictions the engine does not have.
			want: []string{"namespace", "version", "relationship", "cache"}, absent: []string{"mergeFields", "trigger", "handler"},
		},
		{
			name: "tool preamble", src: "@\ntool probeTool {\n}\n", line: 1, col: 2,
			want: []string{"handler", "allowedRoles"}, absent: []string{"mergeFields", "cache"},
		},
	}
	for _, tc := range cases {
		got := labelsOfItems(s.Complete(tc.src, tc.line, tc.col, "probe.memql"))
		for _, w := range tc.want {
			if !got[w] {
				t.Errorf("%s: missing %q; got %v", tc.name, w, got)
			}
		}
		for _, a := range tc.absent {
			if got[a] {
				t.Errorf("%s: must not offer %q", tc.name, a)
			}
		}
	}

	// No detection (EOF) keeps the union fallback.
	got := labelsOfItems(s.Complete("query todo a {\n}\n\n@", 4, 2, "probe.memql"))
	if !got["mergeFields"] || !got["cache"] || !got["trigger"] {
		t.Errorf("EOF must fall back to the union, got %v", got)
	}
}

// The unbacked construct (`use`, per the drift test's knownUnbacked pin)
// falls back to the union rather than offering nothing.
func TestUnbackedConstructFallsBackToUnion(t *testing.T) {
	got := annotationsForConstruct(EnclosingConstruct{Keyword: "use"})
	if len(got) != len(allAnnotationNames()) {
		t.Errorf("unbacked construct must fall back to the union, got %d names", len(got))
	}
	// The concept receiver is "" -- a REAL key, not "unresolved".
	conceptNames := annotationsForConstruct(EnclosingConstruct{Keyword: "concept", Receiver: ""})
	if len(conceptNames) == len(allAnnotationNames()) {
		t.Error("concept must use the \"\" receiver's own list, not the union")
	}
}

// #2627: body completion is construct-scoped.
func TestConstructScopedBodyCompletion(t *testing.T) {
	s := New(&fakeRegistry{})
	cases := []struct {
		name, src    string
		want, absent []string
	}{
		{
			name: "query body offers its blocks only",
			src:  "query todo todos {\n  ",
			want: []string{"args", "filter", "shape"}, absent: []string{"insert", "update", "body"},
		},
		{
			name: "mutate body offers write blocks",
			src:  "mutate todo createTodo {\n  ",
			want: []string{"args", "insert", "update"}, absent: []string{"filter", "shape", "body"},
		},
		{
			name: "logic body offers body, never filter",
			src:  "logic compute {\n  ",
			want: []string{"args", "body"}, absent: []string{"filter", "insert", "shape"},
		},
		{
			name: "automation body offers body, never insert",
			src:  "@trigger(event=\"x.y\")\nautomation onThing {\n  ",
			want: []string{"args", "body"}, absent: []string{"insert", "filter", "shape"},
		},
	}
	for _, tc := range cases {
		lines := strings.Split(tc.src, "\n")
		got := labelsOfItems(s.Complete(tc.src, len(lines), len(lines[len(lines)-1])+1, "probe.memql"))
		for _, w := range tc.want {
			if !got[w] {
				t.Errorf("%s: missing %q; got %v", tc.name, w, got)
			}
		}
		for _, a := range tc.absent {
			if got[a] {
				t.Errorf("%s: must not offer another construct's block %q", tc.name, a)
			}
		}
	}
}

// The never-offer-what-the-engine-rejects contract (sense.md:106-121),
// as a registry-driven gate that survives future registry edits: for
// EVERY spec construct, every annotation the completer offers must be
// legal for that construct's receiver.
func TestOfferedAnnotationsAreAlwaysLegal(t *testing.T) {
	for _, c := range dslSpec.Constructs {
		if !c.RegistryBacked {
			continue
		}
		legal := map[string]bool{}
		for _, n := range annotations.ByReceiver[c.AnnotationReceiver] {
			legal[n] = true
		}
		for _, offered := range annotationsForConstruct(EnclosingConstruct{Keyword: c.Keyword}) {
			if !legal[offered] {
				t.Errorf("construct %q (receiver %q): completion offers @%s, which the engine rejects",
					c.Keyword, c.AnnotationReceiver, offered)
			}
		}
	}
}

// annotationTakesArgs is hand-maintained; every offerable annotation
// must be classified, and the classification must match the registry's
// own argument model.
func TestAnnotationTakesArgsInSync(t *testing.T) {
	for _, name := range allAnnotationNames() {
		hasArgs := len(annotations.KeywordArgs[name]) > 0
		if hasArgs && !annotationTakesArgs(name) {
			t.Errorf("@%s has registry keyword args but annotationTakesArgs says no -- completion inserts it without '('", name)
		}
	}
}

// labelsOfItems is the #2627 helper: labels with the annotation '@'
// prefix stripped, so a test can assert on bare annotation names.
func labelsOfItems(items []CompletionItem) map[string]bool {
	out := map[string]bool{}
	for _, it := range items {
		out[strings.TrimPrefix(it.Label, "@")] = true
	}
	return out
}
