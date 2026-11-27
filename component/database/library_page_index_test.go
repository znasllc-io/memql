package database

import "testing"

func TestLibraryPageIndexRequiresTheCursorOrder(t *testing.T) {
	base := indexFacts{Method: "btree", Partial: true, Expression: true,
		Predicate: "(concept = 'v1:library:artifact'::text)", Expressions: "(payload ->> 'ownerUserId'::text)",
		KeyColumns: []string{"", "concept", "createdAt", "id"}, KeyDescending: []bool{false, false, true, true},
		KeyCollations: []string{"default", "default", "", "C"}, KeyNullsFirst: []bool{false, false, true, true}}
	if !libraryPageIndexMatches(base) {
		t.Fatal("the intended page index was refused")
	}
	for _, change := range []func(*indexFacts){
		func(f *indexFacts) { f.KeyCollations = []string{"default", "default", "", "default"} },
		func(f *indexFacts) { f.KeyDescending = []bool{false, false, true, false} },
		func(f *indexFacts) { f.KeyNullsFirst = []bool{false, false, true, false} },
		func(f *indexFacts) { f.KeyNullsFirst = []bool{false, false, false, true} },
		func(f *indexFacts) { f.Predicate = "" },
		func(f *indexFacts) { f.KeyColumns = []string{"", "concept", "id", "createdAt"} },
	} {
		f := base
		change(&f)
		if libraryPageIndexMatches(f) {
			t.Fatalf("accepted a mismatched page index: %+v", f)
		}
	}
}
