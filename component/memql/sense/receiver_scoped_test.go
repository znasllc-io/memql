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
			name: "mutation preamble", src: "@\nmutation todo createTodo {\n}\n", line: 1, col: 2,
			want: []string{"mergeFields", "createOnly", "actor"}, absent: []string{"cache", "trigger"},
		},
		{
			name: "automation preamble", src: "@\nautomation onThing {\n}\n", line: 1, col: 2,
			want: []string{"trigger", "filter"}, absent: []string{"mergeFields", "cache"},
		},
		{
			name: "concept preamble uses the Concept receiver", src: "@\nconcept widget {\n}\n", line: 1, col: 2,
			// @cache is RETIRED on a concept (the concept loader always
			// refused it; the old "" receiver offered it anyway), and
			// @relationship is written inside the body, not before the
			// declaration (memql#5359).
			want: []string{"namespace", "version", "rowAuthz", "displayCard"}, absent: []string{"mergeFields", "trigger", "handler", "cache", "relationship"},
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
			name: "query body offers its clauses only",
			src:  "query todo todos {\n  ",
			want: []string{"args", "filter", "shape", "sort", "paginate", "asOf", "count"}, absent: []string{"insert", "update", "body"},
		},
		{
			name: "mutation body offers write blocks",
			src:  "mutation todo createTodo {\n  ",
			want: []string{"args", "insert", "update", "accept", "stamp"}, absent: []string{"filter", "shape", "body"},
		},
		// A logic's and an automation's statements are the body (epic
		// memql#5370): the retired `body { }` wrapper is never offered.
		{
			name: "logic body offers statements, never filter or a body block",
			src:  "logic compute {\n  ",
			want: []string{"args", "return", "if"}, absent: []string{"filter", "insert", "shape", "body", "publish"},
		},
		{
			// An automation's statements are its body (epic memql#5370): neither
			// a `body { }` wrapper nor a `step` block is offered; its blocks are
			// args and precondition.
			name: "automation body offers statements, never a body or step block or insert",
			src:  "@trigger(event=\"x.y\")\nautomation onThing {\n  ",
			want: []string{"args", "precondition", "return", "publish"}, absent: []string{"insert", "filter", "shape", "body", "step"},
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

// annotationTakesArgs reads the registry's argument forms: completion inserts
// `@name(` exactly when the placement cannot be written bare. Pinned per
// placement, plus the fallback that has no receiver.
func TestAnnotationTakesArgsInSync(t *testing.T) {
	for _, p := range annotations.Placements() {
		want := p.Forms&annotations.FormFlag == 0
		if got := annotationTakesArgs(string(p.Receiver), p.Name); got != want {
			t.Errorf("%s @%s: annotationTakesArgs = %v, want %v (forms: %s)", p.Receiver, p.Name, got, want, p.Forms)
		}
	}
	if !annotationTakesArgs("", "trigger") {
		t.Error("with no receiver, @trigger must still insert its paren")
	}
	if annotationTakesArgs("", "serverOnly") {
		t.Error("with no receiver, the bare flag @serverOnly must not insert a paren")
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
