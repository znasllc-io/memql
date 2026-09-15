package memql

// memql#2614 loader-altitude pins: absent @namespace derives the mounted
// domain directory; an explicit mismatch is a load ERROR (the moved-file
// guard) unless the domain carries a namespace.pin; the pin travels through
// the tree like any other domain file.

import (
	"strings"
	"testing"
	"testing/fstest"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

func TestLoadUnifiedConcepts_NamespaceDefault(t *testing.T) {
	t.Run("absent-derives-domain", func(t *testing.T) {
		memqldsl.RegisterTree("nsprobe", withLanguageLine(fstest.MapFS{
			"concepts.memql": {Data: []byte("concept probeThing {\n  ownerUserId string!\n}\n")},
		}))
		defer memqldsl.UnregisterTree("nsprobe")
		if _, err := LoadUnifiedConcepts(nil); err != nil {
			t.Fatalf("derived-namespace load must succeed: %v", err)
		}
	})
	// THE MOVED-FILE GUARD IS NOW UNREACHABLE, and that is worth stating
	// rather than deleting quietly.
	//
	// The guard (#2614) reconciled TWO sources of truth for a concept's
	// namespace -- an explicit @namespace and the file's directory -- and
	// refused when they disagreed, so a file moved between domains could not
	// silently change every id it declares. Epic memql#5375 retired the
	// annotation, leaving ONE source, so there is nothing left to disagree: a
	// moved file's ids follow the move by construction. What protects a domain
	// that must keep its ids is the namespace.pin, covered by
	// "pin-allows-divergence" below.
	//
	// What this case still asserts is that the retired annotation is a LOAD
	// error rather than a silently-ignored one -- the same position in the
	// same walk.
	t.Run("retired-annotation-is-a-load-error", func(t *testing.T) {
		memqldsl.RegisterTree("nsprobe", withLanguageLine(fstest.MapFS{
			"concepts.memql": {Data: []byte("@namespace(\"identity\")\nconcept probeThing {\n  ownerUserId string!\n}\n")},
		}))
		defer memqldsl.UnregisterTree("nsprobe")
		_, err := LoadUnifiedConcepts(nil)
		if err == nil || !strings.Contains(err.Error(), "is retired") {
			t.Fatalf("a retired @namespace must be a load error, got %v", err)
		}
	})
	t.Run("pin-allows-divergence", func(t *testing.T) {
		memqldsl.RegisterTree("nsprobe", withLanguageLine(fstest.MapFS{
			"namespace.pin":  {Data: []byte("identity\n")},
			"concepts.memql": {Data: []byte("concept probeThing {\n  ownerUserId string!\n}\n")},
		}))
		defer memqldsl.UnregisterTree("nsprobe")
		if _, err := LoadUnifiedConcepts(nil); err != nil {
			t.Fatalf("pinned divergence must load: %v", err)
		}
	})
	// A colon-scoped sub-namespace was a PER-CONCEPT
	// @namespace("nsprobe:sub"); epic memql#5375 retired the annotation, so
	// the surviving expression is a PIN, which is per-DIRECTORY. The
	// capability is not gone -- it moved grain, and this case measures the
	// grain it moved to. Without the rewrite the fixture would be a copy of
	// "absent-derives-domain" above and would measure nothing.
	t.Run("colon-scoped-pin-allowed", func(t *testing.T) {
		memqldsl.RegisterTree("nsprobe", withLanguageLine(fstest.MapFS{
			"namespace.pin":  {Data: []byte("nsprobe:sub\n")},
			"concepts.memql": {Data: []byte("concept probeThing {\n  ownerUserId string!\n}\n")},
		}))
		defer memqldsl.UnregisterTree("nsprobe")
		if _, err := LoadUnifiedConcepts(nil); err != nil {
			t.Fatalf("a colon-scoped namespace.pin must load: %v", err)
		}
		if _, err := memoryNodes.Get("v1:nsprobe:sub:probeThing"); err != nil {
			t.Errorf("the pin's colon-scoped namespace did not reach the id: %v", err)
		}
	})
}
