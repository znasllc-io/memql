package sense

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/dslspec"
)

func blockLabels(t *testing.T, rp RegistryProvider, src string) map[string]bool {
	t.Helper()
	s := New(rp)
	lines := strings.Split(src, "\n")
	return labelsOfItems(s.Complete(src, len(lines), len(lines[len(lines)-1])+1, "probe.memql"))
}

// #2628: each block gets its OWN set; no set leaks into another block.
func TestBlockSpecificCompletion(t *testing.T) {
	rp := &fakeRegistry{concepts: []string{"v1:todos:todo"}}

	args := blockLabels(t, rp, "query todo todos {\n  args {\n    ")
	for _, want := range []string{"string", "bool", "enum"} {
		if !args[want] {
			t.Errorf("args block must offer field type %q, got %v", want, args)
		}
	}
	for _, never := range []string{"coalesce", "cond", "filter", "accept", "payload"} {
		if args[never] {
			t.Errorf("args block must not offer %q", never)
		}
	}

	filter := blockLabels(t, rp, "@actor\nquery todo todos {\n  filter {\n    ")
	for _, want := range []string{"payload", "actor", "args", "now", "title"} {
		if !filter[want] {
			t.Errorf("filter clause must offer %q, got %v", want, filter)
		}
	}
	for _, never := range []string{"string", "bool", "accept", "stamp"} {
		if filter[never] {
			t.Errorf("filter clause must not offer %q", never)
		}
	}

	write := blockLabels(t, rp, "mutate todo createTodo {\n  insert {\n    ")
	for _, want := range []string{"accept", "stamp", "title"} {
		if !write[want] {
			t.Errorf("write block must offer %q, got %v", want, write)
		}
	}
	for _, never := range []string{"string", "payload", "filter"} {
		if write[never] {
			t.Errorf("write block must not offer %q", never)
		}
	}
}

// The write block teaches the POST-epoch surface: the retired long
// forms must never creep back through the editor.
func TestWriteBlockOffersOnlyShortForms(t *testing.T) {
	rp := &fakeRegistry{concepts: []string{"v1:todos:todo"}}
	s := New(rp)
	src := "mutate todo createTodo {\n  insert {\n    "
	lines := strings.Split(src, "\n")
	items := s.Complete(src, len(lines), len(lines[len(lines)-1])+1, "probe.memql")

	var sawAccept, sawStamp bool
	for _, it := range items {
		switch it.Label {
		case "accept":
			sawAccept = true
		case "stamp":
			sawStamp = true
		}
		// No item may teach a retired spelling.
		for _, retired := range []string{"@required", "@enum(", "@cache(ttl=", "args.X"} {
			if strings.Contains(it.InsertText, retired) {
				t.Errorf("write-block completion teaches the retired form %q via %+v", retired, it)
			}
		}
	}
	if !sawAccept || !sawStamp {
		t.Error("write block must teach accept/stamp (#2616)")
	}

	// The args block teaches the enum TYPE form, not the annotation pair.
	src = "query todo todos {\n  args {\n    "
	lines = strings.Split(src, "\n")
	for _, it := range s.Complete(src, len(lines), len(lines[len(lines)-1])+1, "probe.memql") {
		if it.Label == "enum" && !strings.HasPrefix(it.InsertText, "enum(") {
			t.Errorf("args enum must insert the type form, got %q", it.InsertText)
		}
		if strings.Contains(it.Documentation, "string @enum") {
			t.Errorf("args completion must not teach the retired pair: %+v", it)
		}
	}
}

// Every declared NextRule label must be either computed by the
// classifier or explicitly listed as not-yet-detected -- no silently
// dead labels going forward.
func TestEveryNextRuleLabelIsClassifiedOrDeclaredUndetected(t *testing.T) {
	for _, r := range dslspec.Build().NextRules {
		if specDetectedContextLabels[r.Context] {
			continue
		}
		if specUndetectedContextLabels[r.Context] {
			continue
		}
		t.Errorf("NextRule label %q is neither detected by the classifier nor listed as undetected -- it is silently dead", r.Context)
	}
	// The undetected list may not name a label that does not exist.
	declared := map[string]bool{}
	for _, r := range dslspec.Build().NextRules {
		declared[r.Context] = true
	}
	for label := range specUndetectedContextLabels {
		if !declared[label] {
			t.Errorf("undetected list names %q, which no NextRule declares", label)
		}
	}
}

// The filter heads mirror the engine plan parser's reserved set.
func TestReservedFilterHeadsMirrorEngine(t *testing.T) {
	want := map[string]bool{
		"payload": true, "actor": true, "args": true, "now": true, "config": true,
		"trace": true, "meta": true, "schema": true, "partition": true, "provenance": true,
	}
	got := map[string]bool{}
	for _, h := range reservedFilterHeads {
		got[h] = true
	}
	for h := range want {
		if !got[h] {
			t.Errorf("filter completion is missing the engine's reserved head %q", h)
		}
	}
	for h := range got {
		if !want[h] {
			t.Errorf("filter completion offers %q, which the engine does not reserve", h)
		}
	}
}
