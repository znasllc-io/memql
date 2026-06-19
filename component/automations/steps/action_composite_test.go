package steps

import (
	"reflect"
	"testing"

	"github.com/znasllc-io/memql/component/harness/actionpin"
)

func TestBindChildArgs(t *testing.T) {
	input := map[string]any{"dir": "/work/proj", "module": "memql"}
	// argTemplate: child key -> composite param name (string) or literal.
	tmpl := map[string]any{
		"path":  "dir",        // binds to input["dir"]
		"name":  "module",     // binds to input["module"]
		"mode":  "0644",       // not a key in input -> literal
		"depth": float64(2),   // non-string -> literal
	}
	got := bindChildArgs(tmpl, input)
	want := map[string]any{
		"path":  "/work/proj",
		"name":  "memql",
		"mode":  "0644",
		"depth": float64(2),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("bindChildArgs:\n got  %#v\n want %#v", got, want)
	}
}

func TestBindChildArgs_NilTemplate(t *testing.T) {
	if got := bindChildArgs(nil, map[string]any{"a": 1}); len(got) != 0 {
		t.Fatalf("nil template should yield empty args, got %#v", got)
	}
}

func TestCompositeChildRef(t *testing.T) {
	// pinned
	r, err := compositeChildRef(map[string]any{"actionId": "act_mkdir", "version": float64(2)})
	if err != nil || r.ID != "act_mkdir" || r.Version != 2 || r.Floating {
		t.Fatalf("pinned child ref wrong: %+v (%v)", r, err)
	}
	// floating (no version)
	f, err := compositeChildRef(map[string]any{"actionId": "act_writeFile"})
	if err != nil || f.ID != "act_writeFile" || !f.Floating {
		t.Fatalf("floating child ref wrong: %+v (%v)", f, err)
	}
	// version 0 -> floating (invalid pin floors to latest)
	z, _ := compositeChildRef(map[string]any{"actionId": "act_x", "version": float64(0)})
	if !z.Floating {
		t.Fatalf("version 0 should float, got %+v", z)
	}
	// missing id -> error
	if _, err := compositeChildRef(map[string]any{}); err == nil {
		t.Fatalf("missing actionId must error")
	}
}

// Sanity: Format round-trips the refs compositeChildRef produces.
func TestCompositeChildRef_FormatRoundTrip(t *testing.T) {
	r, _ := compositeChildRef(map[string]any{"actionId": "act_x", "version": float64(3)})
	if actionpin.Format(r) != "act_x@3" {
		t.Fatalf("format: want act_x@3, got %s", actionpin.Format(r))
	}
}
