package memql

// memql#2614 loader-altitude pins: absent @namespace derives the mounted
// domain directory; an explicit mismatch is a load ERROR (the moved-file
// guard) unless the domain carries a namespace.pin; the pin travels through
// the tree like any other domain file.

import (
	"strings"
	"testing"
	"testing/fstest"

	memqldsl "github.com/znasllc-io/memql/dsl"
)

func TestLoadUnifiedConcepts_NamespaceDefault(t *testing.T) {
	t.Run("absent-derives-domain", func(t *testing.T) {
		memqldsl.RegisterTree("nsprobe", fstest.MapFS{
			"concepts.memql": {Data: []byte("concept probeThing {\n  ownerUserId string!\n}\n")},
		})
		defer memqldsl.UnregisterTree("nsprobe")
		if _, err := LoadUnifiedConcepts(nil); err != nil {
			t.Fatalf("derived-namespace load must succeed: %v", err)
		}
	})
	t.Run("mismatch-is-a-load-error", func(t *testing.T) {
		memqldsl.RegisterTree("nsprobe", fstest.MapFS{
			"concepts.memql": {Data: []byte("@namespace(\"identity\")\nconcept probeThing {\n  ownerUserId string!\n}\n")},
		})
		defer memqldsl.UnregisterTree("nsprobe")
		_, err := LoadUnifiedConcepts(nil)
		if err == nil || !strings.Contains(err.Error(), "does not match its domain directory") {
			t.Fatalf("moved-file guard must be a load error, got %v", err)
		}
	})
	t.Run("pin-allows-divergence", func(t *testing.T) {
		memqldsl.RegisterTree("nsprobe", fstest.MapFS{
			"namespace.pin":  {Data: []byte("identity\n")},
			"concepts.memql": {Data: []byte("@namespace(\"identity\")\nconcept probeThing {\n  ownerUserId string!\n}\n")},
		})
		defer memqldsl.UnregisterTree("nsprobe")
		if _, err := LoadUnifiedConcepts(nil); err != nil {
			t.Fatalf("pinned divergence must load: %v", err)
		}
	})
	t.Run("colon-extension-allowed", func(t *testing.T) {
		memqldsl.RegisterTree("nsprobe", fstest.MapFS{
			"concepts.memql": {Data: []byte("@namespace(\"nsprobe:sub\")\nconcept probeThing {\n  ownerUserId string!\n}\n")},
		})
		defer memqldsl.UnregisterTree("nsprobe")
		if _, err := LoadUnifiedConcepts(nil); err != nil {
			t.Fatalf("colon-scoped extension must load: %v", err)
		}
	})
}
