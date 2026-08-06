package baseregistry

import (
	"errors"
	"strings"
	"testing"
)

type item struct {
	Name  string
	Value int
}

func (i *item) clone() *item {
	if i == nil {
		return nil
	}
	cp := *i
	return &cp
}

func newTestRegistry() *Registry[item] {
	return New[item]("test",
		func(i *item) *item { return i.clone() },
		func(name string) error {
			if strings.TrimSpace(name) == "" {
				return errors.New("name required")
			}
			return nil
		})
}

func TestAddAndGet(t *testing.T) {
	r := newTestRegistry()
	if err := r.Add("foo", &item{Name: "foo", Value: 1}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	got, err := r.Get("foo")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Value != 1 {
		t.Fatalf("got value %d, want 1", got.Value)
	}
}

func TestAddDuplicateErrors(t *testing.T) {
	r := newTestRegistry()
	_ = r.Add("foo", &item{Name: "foo"})
	err := r.Add("foo", &item{Name: "foo"})
	if err == nil {
		t.Fatal("expected duplicate error")
	}
}

func TestUpsertOverwrites(t *testing.T) {
	r := newTestRegistry()
	_ = r.Add("foo", &item{Name: "foo", Value: 1})
	if err := r.Upsert("foo", &item{Name: "foo", Value: 2}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, _ := r.Get("foo")
	if got.Value != 2 {
		t.Fatalf("expected 2 after Upsert, got %d", got.Value)
	}
}

func TestCloneIsolatesEgress(t *testing.T) {
	r := newTestRegistry()
	_ = r.Add("foo", &item{Name: "foo", Value: 1})
	got, _ := r.Get("foo")
	got.Value = 999
	again, _ := r.Get("foo")
	if again.Value != 1 {
		t.Fatalf("expected egress clone to isolate; got %d", again.Value)
	}
}

func TestLookupCommaOk(t *testing.T) {
	r := newTestRegistry()
	if _, ok := r.Lookup("missing"); ok {
		t.Fatal("Lookup of missing key returned ok=true")
	}
	_ = r.Add("present", &item{Name: "present"})
	if _, ok := r.Lookup("present"); !ok {
		t.Fatal("Lookup of present key returned ok=false")
	}
}

func TestGetMissingErrors(t *testing.T) {
	r := newTestRegistry()
	_, err := r.Get("missing")
	if err == nil {
		t.Fatal("expected error for missing key")
	}
	if !strings.Contains(err.Error(), "test") {
		t.Fatalf("error should mention kind label 'test': %v", err)
	}
}

func TestSnapshotAndList(t *testing.T) {
	r := newTestRegistry()
	if r.Snapshot() != nil {
		t.Fatal("empty registry Snapshot should be nil")
	}
	if r.List() != nil {
		t.Fatal("empty registry List should be nil")
	}
	_ = r.Add("b", &item{Name: "b", Value: 2})
	_ = r.Add("a", &item{Name: "a", Value: 1})

	snap := r.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("Snapshot size = %d, want 2", len(snap))
	}
	list := r.List()
	if len(list) != 2 {
		t.Fatalf("List size = %d, want 2", len(list))
	}
	if list[0].Name != "a" {
		t.Fatalf("List not sorted: %s first", list[0].Name)
	}
}

func TestNamesSorted(t *testing.T) {
	r := newTestRegistry()
	_ = r.Add("zebra", &item{Name: "zebra"})
	_ = r.Add("alpha", &item{Name: "alpha"})
	names := r.Names()
	if len(names) != 2 || names[0] != "alpha" || names[1] != "zebra" {
		t.Fatalf("Names not sorted: %v", names)
	}
}

func TestNilRegistrySafe(t *testing.T) {
	var r *Registry[item]
	if r.Count() != 0 {
		t.Fatal("nil Count != 0")
	}
	if r.Has("x") {
		t.Fatal("nil Has true")
	}
	if r.Names() != nil {
		t.Fatal("nil Names != nil")
	}
	if r.Snapshot() != nil {
		t.Fatal("nil Snapshot != nil")
	}
	if _, ok := r.Lookup("x"); ok {
		t.Fatal("nil Lookup ok")
	}
	if _, err := r.Get("x"); err == nil {
		t.Fatal("nil Get nil error")
	}
}

func TestValidateBlocksBadName(t *testing.T) {
	r := newTestRegistry()
	err := r.Add(" ", &item{Name: " "})
	if err == nil {
		t.Fatal("expected validate error")
	}
}

func TestNoCloneStrategy(t *testing.T) {
	r := New[item]("raw", nil, nil)
	orig := &item{Name: "x", Value: 7}
	_ = r.Upsert("x", orig)
	got, _ := r.Lookup("x")
	if got != orig {
		t.Fatal("expected pointer identity when clone is nil")
	}
}
