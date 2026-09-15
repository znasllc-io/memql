package sense

import (
	"strings"
	"testing"
)

// #2624: member completion after a dot. A dot context offers ONLY the
// root's members -- never keywords, builtins, functions, or concepts.

func completeAt(t *testing.T, rp RegistryProvider, source string) []CompletionItem {
	t.Helper()
	s := New(rp)
	lines := strings.Split(source, "\n")
	line := len(lines)
	col := len(lines[line-1]) + 1
	return s.Complete(source, line, col, "probe.memql")
}

func labels(items []CompletionItem) map[string]bool {
	out := map[string]bool{}
	for _, it := range items {
		out[it.Label] = true
	}
	return out
}

func TestFieldAccess_ActorMembers(t *testing.T) {
	// Trailing-dot shape.
	got := labels(completeAt(t, &fakeRegistry{}, "@actor\nquery todo todos {\n  filter row => row.ownerUserId == actor."))
	for _, want := range []string{"userId", "role", "identityId", "isClusterOwner", "primaryEmail", "now", "isOwner"} {
		if !got[want] {
			t.Errorf("actor. must offer %q, got %v", want, got)
		}
	}
	for _, never := range []string{"query", "lower", "hash", "concept", "filter"} {
		if got[never] {
			t.Errorf("dot context must not offer %q", never)
		}
	}

	// Mid-member shape: prefix after the last dot filters.
	items := completeAt(t, &fakeRegistry{}, "@actor\nquery todo todos {\n  filter row => row.ownerUserId == actor.us")
	if len(items) != 1 || items[0].Label != "userId" {
		t.Errorf("actor.us must offer exactly userId, got %v", labels(items))
	}

	// The alias sorts after canonical members.
	for _, it := range completeAt(t, &fakeRegistry{}, "@actor\nquery todo todos {\n  filter row => row.done == actor.") {
		if it.Label == "isOwner" && it.SortPriority <= 1 {
			t.Errorf("alias must sort after canonical members: %+v", it)
		}
		if it.SortPriority == 0 {
			t.Errorf("every member item must set SortPriority: %+v", it)
		}
	}
}

func TestFieldAccess_EventMembers(t *testing.T) {
	got := labels(completeAt(t, &fakeRegistry{}, "@trigger(event=\"x.y\")\nautomation onThing {\n  note := event."))
	for _, want := range []string{"topic", "kind", "payload", "actor", "timestamp"} {
		if !got[want] {
			t.Errorf("event. must offer %q, got %v", want, got)
		}
	}

	// event.actor is the EVENT stamp: {id} only.
	items := completeAt(t, &fakeRegistry{}, "@trigger(event=\"x.y\")\nautomation onThing {\n  who := event.actor.")
	if len(items) != 1 || items[0].Label != "id" {
		t.Errorf("event.actor. must offer only id, got %v", labels(items))
	}
}

func TestFieldAccess_ArgsMembers(t *testing.T) {
	auto := "automation sweep {\n  args {\n    windowDays int\n    dryRun bool\n  }\n  d := args."
	got := labels(completeAt(t, &fakeRegistry{}, auto))
	if !got["windowDays"] || !got["dryRun"] || len(got) != 2 {
		t.Errorf("args. must offer exactly the declared fields, got %v", got)
	}

	// Non-automation constructs get the args. member path too.
	q := "query todo todos {\n  args {\n    done bool\n  }\n  filter row => row.done == args."
	got = labels(completeAt(t, &fakeRegistry{}, q))
	if !got["done"] || len(got) != 1 {
		t.Errorf("query args. must offer declared fields, got %v", got)
	}
}

// A filter reads the bound concept's fields through its lambda parameter.
// `payload.` was the pre-v1 spelling, which edition 2026 refuses, so it offers
// nothing -- with a header or without one.
func TestFieldAccess_PayloadMembers(t *testing.T) {
	rp := &fakeRegistry{concepts: []string{"v1:todos:todo"}}
	got := labels(completeAt(t, rp, "@actor\nquery todo todoById {\n  filter row => row."))
	if !got["title"] {
		t.Errorf("row. must offer the bound concept's fields, got %v", got)
	}
	for _, src := range []string{
		"@actor\nquery todo todoById {\n  filter row => payload.",
		"@actor\nquery todo todoById {\n  filter payload.",
	} {
		if got := labels(completeAt(t, rp, src)); len(got) != 0 {
			t.Errorf("%q: payload. is the pre-v1 spelling and must offer nothing, got %v", src, got)
		}
	}
}

func TestFieldAccess_UnknownRootOffersNothing(t *testing.T) {
	items := completeAt(t, &fakeRegistry{}, "logic l {\n  x := steps.")
	if len(items) != 0 {
		t.Errorf("unknown root must offer nothing, got %v", labels(items))
	}
	// Float literals never fire the dot context as members of "3".
	items = completeAt(t, &fakeRegistry{}, "logic l {\n  x := 3.")
	for _, it := range items {
		if it.Kind == "field" {
			t.Errorf("numeric dot must not be member completion: %+v", it)
		}
	}
}
