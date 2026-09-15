package memql

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestBeforeWriteRunsInNameOrderOnMergedRow(t *testing.T) {
	engine := &MemQLEngine{}
	var names []string
	engine.SetBeforeWriteHooks(map[string][]BeforeWriteHook{"ticket": {
		{Name: "z", On: "write", Apply: func(_ context.Context, row map[string]any) error {
			names = append(names, "z")
			if row["status"] != "queued" {
				t.Fatal(row)
			}
			row["label"] = row["status"]
			return nil
		}},
		{Name: "a", On: "create", Apply: func(_ context.Context, row map[string]any) error {
			names = append(names, "a")
			row["status"] = "queued"
			return nil
		}},
	}})
	row := map[string]any{"status": "submitted", "retained": "prior"}
	if err := engine.applyBeforeWrite(context.Background(), "ticket", "id", false, row); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(names, []string{"a", "z"}) || row["retained"] != "prior" || row["label"] != "queued" {
		t.Fatal(names, row)
	}
	names = nil
	if err := engine.applyBeforeWrite(context.Background(), "ticket", "id", true, row); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(names, []string{"z"}) {
		t.Fatal(names)
	}
	engine.SetBeforeWriteHooks(map[string][]BeforeWriteHook{"ticket": {{Name: "refuse", On: "write", Apply: func(context.Context, map[string]any) error { return errors.New("refused") }}}})
	if err := engine.applyBeforeWrite(context.Background(), "ticket", "id", true, row); err == nil {
		t.Fatal("hook error ignored")
	}
}

func TestBeforeWriteSourcesReplaceIndependently(t *testing.T) {
	engine := &MemQLEngine{}
	var seen []string
	hook := func(name string) BeforeWriteHook {
		return BeforeWriteHook{Name: name, On: "write", Apply: func(context.Context, map[string]any) error { seen = append(seen, name); return nil }}
	}
	engine.SetBeforeWriteHooks(map[string][]BeforeWriteHook{"ticket": {hook("core")}})
	engine.SetBeforeWriteHookSource("owner-a", map[string][]BeforeWriteHook{"ticket": {hook("authored")}})
	if err := engine.applyBeforeWrite(context.Background(), "ticket", "id", false, map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(seen, []string{"authored", "core"}) {
		t.Fatal(seen)
	}
	seen = nil
	engine.SetBeforeWriteHookSource("owner-a", nil)
	if err := engine.applyBeforeWrite(context.Background(), "ticket", "id", false, map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(seen, []string{"core"}) {
		t.Fatal(seen)
	}
}
