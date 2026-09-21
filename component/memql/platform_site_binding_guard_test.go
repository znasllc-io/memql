package memql

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// The storefront binding names a store row, and the guard is the answer to the
// question issue memql#5538 asked: who may bind a storefront they own to a store
// they may not read. Nobody.
//
// The capability (`app:deployables/store`, owner-seeded) says who may reach the
// mutation. This says which stores they may name once they have. They are
// different questions: a grant is a grant on an APP PART, and it cannot know
// which rows a cluster holds.

func TestABindingNamingAnUnreadableStoreIsRefused(t *testing.T) {
	e := &MemQLEngine{}
	payload := map[string]any{"binding": map[string]any{"storeId": "nope"}}
	err := e.validateSiteStoreBinding(context.Background(), payload, "u1", func(context.Context, string) (bool, error) {
		return false, nil
	})
	if err == nil {
		t.Fatal("a binding naming a store the caller cannot read was accepted")
	}
	for _, want := range []string{"nope", "v1:shopify:store"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
}

func TestABindingNamingAReadableStoreIsAccepted(t *testing.T) {
	e := &MemQLEngine{}
	payload := map[string]any{"binding": map[string]any{"storeId": "acme"}}
	if err := e.validateSiteStoreBinding(context.Background(), payload, "u1", func(context.Context, string) (bool, error) {
		return true, nil
	}); err != nil {
		t.Fatalf("a binding naming a readable store was refused: %v", err)
	}
}

// CLEARING MUST BE EXPRESSIBLE. An empty object and an empty storeId are the
// unbound state, and neither reads a store.
func TestAnEmptyBindingReadsNoStore(t *testing.T) {
	e := &MemQLEngine{}
	reads := 0
	probe := func(context.Context, string) (bool, error) { reads++; return false, nil }
	for _, payload := range []map[string]any{
		{"binding": map[string]any{}},
		{"binding": map[string]any{"storeId": ""}},
		{"binding": nil},
		{},
	} {
		if err := e.validateSiteStoreBinding(context.Background(), payload, "u1", probe); err != nil {
			t.Errorf("payload %v was refused: %v", payload, err)
		}
	}
	if reads != 0 {
		t.Errorf("an unbound binding read a store %d times; it must read none", reads)
	}
}

// THE LEGACY SHAPE IS REFUSED, NOT IGNORED. Pre-release means no shim: a write
// still carrying the copied domain is a caller that was not migrated, and
// accepting it would leave two records of one store on that row forever.
func TestTheLegacyCopiedBindingIsRefused(t *testing.T) {
	e := &MemQLEngine{}
	payload := map[string]any{"binding": map[string]any{
		"storeDomain":        "acme.myshopify.com",
		"storefrontTokenRef": "acme-storefront-token",
	}}
	err := e.validateSiteStoreBinding(context.Background(), payload, "u1", func(context.Context, string) (bool, error) {
		return true, nil
	})
	if err == nil {
		t.Fatal("the retired {storeDomain, storefrontTokenRef} binding was accepted")
	}
	if !strings.Contains(err.Error(), "storeId") {
		t.Errorf("the refusal does not say what to write instead: %v", err)
	}
}

// A BINDING THAT IS NOT AN OBJECT IS REFUSED RATHER THAN IGNORED, because the
// guard's every other answer is read off a map and a non-map would sail past
// all of them -- including the retired-key refusal above.
func TestABindingThatIsNotAnObjectIsRefused(t *testing.T) {
	e := &MemQLEngine{}
	payload := map[string]any{"binding": "acme.myshopify.com"}
	err := e.validateSiteStoreBinding(context.Background(), payload, "u1", func(context.Context, string) (bool, error) {
		return true, nil
	})
	if err == nil {
		t.Fatal("a scalar binding was accepted")
	}
	if !strings.Contains(err.Error(), "storeId") {
		t.Errorf("the refusal does not say what the object holds: %v", err)
	}
}

// THE MUTATION MUST RENDER, AND NOTHING ELSE IN THIS FILE WOULD NOTICE IF IT
// DID NOT (memory `nested-object-in-a-mutation-template-needs-commas`).
//
// A mutation template that parses but does not RENDER is invisible to both
// loaders and to any assertion that the construct is registered: it registers
// cleanly, is advertised, and then fails on every single call. `binding: {
// storeId: ... }` is the exact shape that fails that way -- a multi-key object
// literal with no commas lexes into one identifier and both loaders pass -- so
// this renders the construct the tree actually ships and reads the payload
// back. It loads the real unified tree rather than a fixture for the same
// reason: a fixture would prove that a string in this file renders.
func TestUpdateSiteStoreBindingRendersTheStoreReference(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	if _, err := LoadUnifiedConcepts(logger); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	registry := newFunctionRegistry()
	if _, _, err := LoadUnifiedFunctions(logger, registry, memoryNodes.DefaultRegistry()); err != nil {
		t.Fatalf("LoadUnifiedFunctions: %v", err)
	}

	fn, ok := registry.Lookup("updateSiteStoreBinding")
	if !ok {
		t.Fatal("updateSiteStoreBinding is not registered")
	}
	if fn.MutationTemplate == nil {
		t.Fatal("updateSiteStoreBinding carries no mutation template")
	}

	engine := &MemQLEngine{}
	for _, tc := range []struct {
		name    string
		args    map[string]any
		wantID  string
		wantRef string
	}{
		{"bound", map[string]any{"siteId": "site-1", "storeId": "acme"}, "site-1", "acme"},
		// The unbound state: an omitted storeId writes an empty object, which
		// is what makes detaching a store expressible at all.
		{"unbound", map[string]any{"siteId": "site-1"}, "site-1", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node, err := engine.renderMutationTemplate(context.Background(), fn.MutationTemplate, tc.args)
			if err != nil {
				t.Fatalf("updateSiteStoreBinding does not render: %v", err)
			}
			if node.Concept != conceptPlatformSite {
				t.Errorf("rendered concept = %q, want %q", node.Concept, conceptPlatformSite)
			}
			if node.ID != tc.wantID {
				t.Errorf("rendered id = %q, want %q", node.ID, tc.wantID)
			}
			t.Logf("rendered payload: %s", node.PayloadRaw)
			var payload map[string]any
			if err := json.Unmarshal([]byte(node.PayloadRaw), &payload); err != nil {
				t.Fatalf("rendered payload is not JSON (%q): %v", node.PayloadRaw, err)
			}
			binding, isObject := payload["binding"].(map[string]any)
			if !isObject {
				t.Fatalf("binding rendered as %T (%q), not an object -- the object literal did not render", payload["binding"], node.PayloadRaw)
			}
			if got := binding["storeId"]; got != tc.wantRef {
				t.Errorf("binding.storeId = %#v, want %q (payload %q)", got, tc.wantRef, node.PayloadRaw)
			}
			if len(binding) != 1 {
				t.Errorf("binding carries %d keys (%v); the reference is exactly {storeId}", len(binding), binding)
			}
		})
	}
}
